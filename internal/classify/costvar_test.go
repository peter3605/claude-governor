package classify

import (
	"math"
	"os"
	"sort"
	"testing"
	"time"
)

// TestClassCostVariance asks whether a per-class moving estimate of build cost can mean
// anything, and it is GOV-B03's go/no-go rather than one of its deliverables.
//
// THE QUESTION AND WHY IT IS ASKED FIRST. GOV-B03 proposes replacing the additive climb with
// B = headroom / expected cost per build. That rests entirely on "expected cost per build"
// being a quantity that exists. The story's own notes say it may not: Phase 0 measured child
// cores at 0.000 median, 7.822 max, non-zero on 9.1% of ticks, and a single moving mean over
// that distribution is close to meaningless. The notes' instruction is explicit — estimate
// WITHIN a cost class, where the variance is plausibly tractable, and if per-class variance is
// also large then plain AIMD is the better answer and the story should be DROPPED rather than
// forced.
//
// So this test does not assert that the estimator works. It reports the numbers that decide
// whether to build one, and it fails only on the conditions that would make the story's
// premise false. A green run here is a licence to build; a red one is the evidence for
// dropping it.
//
// THREE THINGS ARE MEASURED, because "is the variance tractable" is three questions.
//
//  1. SPREAD WITHIN A CLASS. If p99/p50 inside Medium is as wide as the global distribution,
//     classification has bought nothing and the mean is as meaningless as Phase 0 said.
//
//  2. HOW MUCH OF THE VARIANCE THE CLASS EXPLAINS (eta-squared, the between-class share of
//     total variance). This is the direct form of "does knowing the class tell you the cost".
//     A class that explains almost none of it is a label, not a predictor.
//
//  3. WHETHER THE ESTIMATE IS STABLE OVER TIME. An estimator is a MOVING mean, so what
//     matters operationally is not the spread of the whole corpus but whether the statistic
//     holds still between one period and the next. A per-class mean that swings by a factor
//     of several between halves of the corpus is one a moving estimate will chase rather
//     than converge on, and chasing it is how B oscillates.
//
// THE COST VARIABLE IS attr_child_peak, the same one the classes were derived from and the
// same one GOV-14 defined "expensive" against. Peak rather than mean for the reason design.md
// section 5.5 gives: a loop protecting against saturation sizes work by its peak, and a cold
// `go test ./...` averages 0.06 cores over 44s while bursting far higher.
//
// Skipped unless GOVERNOR_CORPUS points at the output of tools/bashcorpus, for the same reason
// TestClassDurations and TestEvaluateAgainstFullCorpus are: the corpus is a real machine's
// traffic and does not belong in the repository.
//
//	go build -o /tmp/bashcorpus ./tools/bashcorpus && /tmp/bashcorpus -out /tmp/corpus.jsonl
//	GOVERNOR_CORPUS=/tmp/corpus.jsonl go test ./internal/classify/ -run ClassCostVariance -v
func TestClassCostVariance(t *testing.T) {
	path := os.Getenv("GOVERNOR_CORPUS")
	if path == "" {
		t.Skip("set GOVERNOR_CORPUS to the output of tools/bashcorpus")
	}
	rows := readCorpus(t, path)

	// The fittable set, which is the only set any cost claim may be made over: labelled, at
	// least two attributable seconds, and not one of GOV-13's own thermal probes.
	type sample struct {
		peak  float64
		start time.Time
		class Class
		gated bool
		shape string
	}
	var all []sample
	for _, r := range rows {
		if r.synthetic() || !r.Labelled || r.AttrSamplesN < minAttrSamplesForCost {
			continue
		}
		res := Classify(r.Cmd)
		ts, _ := time.Parse(time.RFC3339Nano, r.Start)
		shape := ""
		if len(res.Shapes) > 0 {
			shape = res.Shapes[0]
		}
		all = append(all, sample{peak: r.AttrChildPeak, start: ts, class: res.Class, gated: res.Gated, shape: shape})
	}
	if len(all) == 0 {
		t.Fatal("no fittable rows in the corpus: nothing can be concluded")
	}

	peaksOf := func(pred func(sample) bool) []float64 {
		var v []float64
		for _, s := range all {
			if pred(s) {
				v = append(v, s.peak)
			}
		}
		sort.Float64s(v)
		return v
	}

	global := peaksOf(func(sample) bool { return true })
	gated := peaksOf(func(s sample) bool { return s.gated })
	byClass := map[Class][]float64{}
	for _, c := range []Class{Medium, Heavy} {
		byClass[c] = peaksOf(func(s sample) bool { return s.gated && s.class == c })
	}

	// ---- 1. the distributions, global against per-class -------------------------------

	report := func(name string, v []float64) {
		if len(v) == 0 {
			t.Logf("%-16s n=0", name)
			return
		}
		t.Logf("%-16s n=%5d  p50=%6.3f  p90=%6.3f  p99=%6.3f  max=%6.3f  mean=%6.3f  sd=%6.3f  cv=%5.2f",
			name, len(v), durQuantile(v, 0.50), durQuantile(v, 0.90), durQuantile(v, 0.99),
			v[len(v)-1], meanOf(v), sdOf(v), cvOf(v))
	}

	t.Log("peak child cores (attributable), the quantity an estimator would have to predict")
	report("all fittable", global)
	report("gated", gated)
	for _, c := range []Class{Medium, Heavy} {
		report(c.String(), byClass[c])
	}

	// THE FIRST COMPARISON THE STORY TURNS ON. Phase 0's global figure has a median of zero,
	// which is what makes a global mean meaningless: most seconds cost nothing. If the class
	// predicate has done its job the gated median is well off zero, because the predicate's
	// entire purpose is to select the calls that are not free.
	t.Logf("global p50 %.3f -> gated p50 %.3f: the predicate moves the median off the floor",
		durQuantile(global, 0.50), durQuantile(gated, 0.50))

	// ---- 2. how much of the variance the class explains --------------------------------

	// Eta-squared over the GATED set: the share of variance in peak cores attributable to
	// which class a command is in, rather than to variation within its class. This is the
	// honest form of "does the class predict the cost". It is computed over gated calls only
	// because Light is not a class an estimator would ever be asked about — a Light command
	// takes no lease and the loop never sizes one.
	eta := etaSquared([][]float64{byClass[Medium], byClass[Heavy]})
	t.Logf("eta-squared (share of gated variance explained by the class): %.3f", eta)

	// Between-class separation and within-class spread, the same pair TestClassDurations
	// reports for hold time, so the two can be read against each other.
	med, hvy := byClass[Medium], byClass[Heavy]
	if len(med) > 0 && len(hvy) > 0 {
		t.Logf("between-class separation (heavy p90 / medium p90): %.1fx",
			durQuantile(hvy, 0.90)/durQuantile(med, 0.90))
		t.Logf("within-class spread medium (p99/p50): %.1fx   heavy (p99/p50): %.1fx",
			safeRatio(durQuantile(med, 0.99), durQuantile(med, 0.50)),
			safeRatio(durQuantile(hvy, 0.99), durQuantile(hvy, 0.50)))
	}

	// PER SHAPE WITHIN A CLASS, because the class is a bucket of shapes and bash-corpus.md
	// section 5 already warns that the same tool is not the same cost in two repositories. If
	// the shapes inside one class disagree with each other more than the classes do, then the
	// class is the wrong grain for the estimate and the shape is the right one.
	t.Log("within-class, by shape (n >= 5)")
	shapes := map[string][]float64{}
	for _, s := range all {
		if s.gated && s.shape != "" {
			shapes[s.shape] = append(shapes[s.shape], s.peak)
		}
	}
	names := make([]string, 0, len(shapes))
	for k := range shapes {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := shapes[k]
		if len(v) < 5 {
			continue
		}
		sort.Float64s(v)
		t.Logf("  %-14s n=%5d  p50=%6.3f  p90=%6.3f  max=%6.3f  mean=%6.3f  cv=%5.2f",
			k, len(v), durQuantile(v, 0.50), durQuantile(v, 0.90), v[len(v)-1], meanOf(v), cvOf(v))
	}

	// ---- 3. is the estimate stable between one period and the next? --------------------

	// THE OPERATIONAL QUESTION, and the one the first two cannot answer. An estimator is a
	// moving mean; it does not need the distribution to be narrow, it needs the STATISTIC to
	// hold still. So split the corpus at its chronological midpoint and ask whether each
	// class's mean and p90 agree across the halves. Disagreement by a large factor means a
	// moving estimate would chase the difference, and a budget derived from it would move
	// with it.
	chron := append([]sample(nil), all...)
	sort.Slice(chron, func(i, j int) bool { return chron[i].start.Before(chron[j].start) })
	mid := len(chron) / 2
	halfPeaks := func(half []sample, c Class) []float64 {
		var v []float64
		for _, s := range half {
			if s.gated && s.class == c {
				v = append(v, s.peak)
			}
		}
		sort.Float64s(v)
		return v
	}
	t.Log("stability of the statistic across the corpus's two chronological halves")
	for _, c := range []Class{Medium, Heavy} {
		a, b := halfPeaks(chron[:mid], c), halfPeaks(chron[mid:], c)
		if len(a) < 5 || len(b) < 5 {
			t.Logf("  %-8s too few in one half (n=%d, %d) to compare", c.String(), len(a), len(b))
			continue
		}
		t.Logf("  %-8s first half  n=%4d mean=%6.3f p90=%6.3f   second half n=%4d mean=%6.3f p90=%6.3f   mean ratio %.2fx",
			c.String(), len(a), meanOf(a), durQuantile(a, 0.90), len(b), meanOf(b), durQuantile(b, 0.90),
			safeRatio(math.Max(meanOf(a), meanOf(b)), math.Min(meanOf(a), meanOf(b))))
	}

	// IS THE HALF-TO-HALF SHIFT REAL, OR IS IT SAMPLING NOISE? It decides how the instability
	// above should be read, and the two readings point opposite ways. If the shift is inside
	// the standard error then the statistic is merely noisy and a longer window fixes it. If
	// it is far outside, the per-class cost is genuinely NON-STATIONARY — the workload really
	// did get heavier — and a moving estimate tracking it is doing its job rather than
	// chasing. Non-stationary is the harder case for clause 2, because a predictor that must
	// track a moving target spends ticks doing it.
	for _, c := range []Class{Medium, Heavy} {
		a, b := halfPeaks(chron[:mid], c), halfPeaks(chron[mid:], c)
		if len(a) < 5 || len(b) < 5 {
			continue
		}
		sa, sb := sdOf(a)/math.Sqrt(float64(len(a))), sdOf(b)/math.Sqrt(float64(len(b)))
		diff := math.Abs(meanOf(b) - meanOf(a))
		se := math.Sqrt(sa*sa + sb*sb)
		t.Logf("  %-8s half-to-half mean shift %.3f cores = %.1f standard errors (%s)",
			c.String(), diff, safeRatio(diff, se),
			map[bool]string{true: "REAL, the cost is non-stationary", false: "inside the noise"}[diff > 2*se])
	}

	// IS THE CLASS MEAN MOVING BECAUSE BUILDS GOT DEARER, OR BECAUSE THE MIX CHANGED?
	//
	// The two readings are different facts with different fixes, and they are easy to confuse
	// because they produce the same number. If each SHAPE costs what it always did and the
	// class mean still moves, then what moved is the proportion of expensive shapes inside the
	// class — the class is an average over a basket whose weights drift, and the low
	// eta-squared and the non-stationarity are then ONE fact rather than two. If instead the
	// shapes themselves move, the cost really is non-stationary and no finer grain escapes it.
	//
	// This distinction decides the grain of any follow-up: a mix shift is fixed by estimating
	// per shape, a genuine cost shift is not fixed by estimating per anything.
	t.Log("per-shape stability across the same two halves (n >= 5 in BOTH halves)")
	shapeHalf := func(half []sample) map[string][]float64 {
		m := map[string][]float64{}
		for _, s := range half {
			if s.gated && s.shape != "" {
				m[s.shape] = append(m[s.shape], s.peak)
			}
		}
		return m
	}
	first, second := shapeHalf(chron[:mid]), shapeHalf(chron[mid:])
	var shapeNames []string
	for k := range first {
		if len(first[k]) >= 5 && len(second[k]) >= 5 {
			shapeNames = append(shapeNames, k)
		}
	}
	sort.Strings(shapeNames)
	stable, moved := 0, 0
	for _, k := range shapeNames {
		a, b := first[k], second[k]
		sa, sb := sdOf(a)/math.Sqrt(float64(len(a))), sdOf(b)/math.Sqrt(float64(len(b)))
		diff := math.Abs(meanOf(b) - meanOf(a))
		se := math.Sqrt(sa*sa + sb*sb)
		verdict := "stable"
		if diff > 2*se {
			verdict = "MOVED"
			moved++
		} else {
			stable++
		}
		t.Logf("  %-14s n=%4d->%4d  mean %6.3f -> %6.3f  (%.1f sigma, %s)",
			k, len(a), len(b), meanOf(a), meanOf(b), safeRatio(diff, se), verdict)
	}
	t.Logf("  %d shapes stable, %d moved", stable, moved)

	// AND DID THE MIX ACTUALLY CHANGE? The counterpart measurement: the share each shape takes
	// of the Medium class in each half. A class mean that moves while its shapes hold still
	// must be explained here or not at all.
	t.Log("shape mix within Medium, as a share of the class in each half")
	mixOf := func(half []sample) (map[string]int, int) {
		m, n := map[string]int{}, 0
		for _, s := range half {
			if s.gated && s.class == Medium && s.shape != "" {
				m[s.shape]++
				n++
			}
		}
		return m, n
	}
	mixA, nA := mixOf(chron[:mid])
	mixB, nB := mixOf(chron[mid:])
	var mixNames []string
	for k := range mixA {
		mixNames = append(mixNames, k)
	}
	for k := range mixB {
		if _, seen := mixA[k]; !seen {
			mixNames = append(mixNames, k)
		}
	}
	sort.Strings(mixNames)
	for _, k := range mixNames {
		if mixA[k]+mixB[k] < 10 {
			continue
		}
		t.Logf("  %-14s %5.1f%% -> %5.1f%%  (n %d -> %d)",
			k, 100*float64(mixA[k])/float64(nA), 100*float64(mixB[k])/float64(nB), mixA[k], mixB[k])
	}

	// WHAT BUDGET EACH CHOICE OF STATISTIC WOULD ACTUALLY PRODUCE, on this host, for a pool
	// of Medium builds. This is the number the story exists to change, so it is reported
	// rather than left as an inference from the distributions above.
	//
	// The comparison that matters is against what already ships. budget.SlotCores is 1.2 —
	// the Medium p90 from the original corpus — and MaxSlots is floor(capacity/SlotCores).
	// If a live per-class estimate lands on the same number, the estimator has bought the
	// loop nothing it did not already have as a constant.
	const capacity = 8.0 // this host; the point is the ratio, not the machine
	const shipped = 1.2  // budget.SlotCores, not imported: internal/budget must not depend on classify
	if len(med) > 0 {
		mean90 := durQuantile(med, 0.90)
		t.Logf("budget for a pool of Medium builds on %.0f cores:", capacity)
		t.Logf("  sized by the shipped constant %.2f -> B = %d", shipped, int(math.Floor(capacity/shipped)))
		t.Logf("  sized by the measured mean     %.3f -> B = %d", meanOf(med), int(math.Floor(capacity/meanOf(med))))
		t.Logf("  sized by the measured p90      %.3f -> B = %d", mean90, int(math.Floor(capacity/mean90)))
		t.Logf("  a p90-sized live estimate differs from the shipped constant by %.0f%%",
			100*math.Abs(mean90-shipped)/shipped)
	}

	// ---- the go/no-go ------------------------------------------------------------------

	// WHAT WOULD MAKE THE STORY'S PREMISE FALSE. Only one condition is asserted, and it is
	// the one the notes name: that classifying buys nothing over the global distribution. If
	// the gated median is still on the floor then the predicate has not separated the costly
	// calls from the free ones, there is nothing for a per-class mean to be a mean OF, and
	// GOV-B03 should be dropped in favour of plain AIMD.
	//
	// The other numbers are REPORTED, NOT ASSERTED, deliberately. Where the threshold for
	// "too much spread" sits is a design judgement that belongs in the doc and in the owner's
	// hands, not in a test that would encode it as a pass mark. This test's job is to make
	// the judgement possible, not to make it.
	if durQuantile(gated, 0.50) <= 0 {
		t.Errorf("the gated median peak is %.3f: classification has not separated costly calls "+
			"from free ones, so there is no per-class cost for an estimator to track. "+
			"GOV-B03's premise is false and plain AIMD is the better answer.",
			durQuantile(gated, 0.50))
	}
}

// minAttrSamplesForCost mirrors tools/bashcorpus's own minAttrSamples: one 1 Hz reading is
// within the sampler's jitter of a quantity whose median is zero, so two is the smallest
// number that is not a coin flip.
const minAttrSamplesForCost = 2

func sdOf(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := meanOf(v)
	s := 0.0
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(v)-1))
}

// cvOf is the coefficient of variation, sd over mean: spread in units of the mean itself,
// which is what makes it comparable between a class at 0.5 cores and one at 4.5.
func cvOf(v []float64) float64 {
	m := meanOf(v)
	if m == 0 {
		return 0
	}
	return sdOf(v) / m
}

func safeRatio(a, b float64) float64 {
	if b == 0 {
		return math.Inf(1)
	}
	return a / b
}

// etaSquared is the share of total variance that lies BETWEEN the groups rather than within
// them — the standard one-way measure of how much knowing the group tells you about the value.
// Zero means the grouping explains nothing; one means it explains everything.
func etaSquared(groups [][]float64) float64 {
	var all []float64
	for _, g := range groups {
		all = append(all, g...)
	}
	if len(all) < 2 {
		return 0
	}
	grand := meanOf(all)
	var between, total float64
	for _, x := range all {
		total += (x - grand) * (x - grand)
	}
	for _, g := range groups {
		if len(g) == 0 {
			continue
		}
		d := meanOf(g) - grand
		between += float64(len(g)) * d * d
	}
	if total == 0 {
		return 0
	}
	return between / total
}
