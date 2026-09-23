package classify

import (
	"math"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"
)

// TestPerShapeCostStabilityAtTheP90 is GOV-B14's go/no-go, and it is deliberately the narrowest
// question the story turns on.
//
// WHY A SECOND STABILITY TEST. TestClassCostVariance already reports stability across the
// corpus's two halves, and it was GOV-B03's go/no-go. But it reports the per-SHAPE figure on the
// MEAN, and per-CLASS on both. GOV-B14 is not free to size by the mean: clause 2 of the story
// requires the p90, because the mean is precisely the statistic that killed GOV-B03 — eleven
// concurrent Medium builds at a p90 of 1.09 cores is ~12 cores of demand on 8. So the number
// GOV-B14 actually depends on — whether a SHAPE's p90 holds still between one period and the
// next — was never measured. This measures it.
//
// WHAT WOULD MAKE THE STORY WORTH BUILDING. A per-shape table is worth shipping only if some
// shape's p90 is stable enough that correcting toward it beats the shipped constant by more than
// the correction's own uncertainty. The prize on offer is small and known: Medium's p90 is within
// ~9% of `budget.SlotCores`. The prize per shape can be larger — pytest's p90 is 0.861 against
// the constant's 1.20, which is a ~39% correction — but only if that 0.861 is a property of
// pytest rather than of the fortnight it was measured in.
//
// THE STORY'S OWN INSTRUCTION, which is why a red result here is a finished answer rather than a
// failure: "IF THE PER-HOST CORRECTION COMES OUT SMALLER THAN THE MEASUREMENT'S OWN NOISE, SAY SO
// AND CLOSE THIS RATHER THAN SHIPPING A TABLE THAT MOVES NOTHING."
//
// A P90 NEEDS A CONFIDENCE INTERVAL, NOT A SIGMA. The half-to-half comparisons in
// TestClassCostVariance use the standard error of a mean, which has a closed form. A p90 does
// not, so each half's p90 gets a bootstrap 95% interval instead and the verdict is whether the
// two intervals OVERLAP. Overlapping means the corpus cannot distinguish the two halves' p90s —
// which is the honest reading of "the correction is inside the noise". The bootstrap is seeded
// so the number is reproducible; a measurement test that moves between runs is not evidence.
//
// Skipped unless GOVERNOR_CORPUS points at the output of tools/bashcorpus, like every other
// corpus test here.
//
//	go build -o /tmp/bashcorpus ./tools/bashcorpus && /tmp/bashcorpus -out /tmp/corpus.jsonl
//	GOVERNOR_CORPUS=/tmp/corpus.jsonl go test ./internal/classify/ -run PerShapeCost -v
func TestPerShapeCostStabilityAtTheP90(t *testing.T) {
	path := os.Getenv("GOVERNOR_CORPUS")
	if path == "" {
		t.Skip("set GOVERNOR_CORPUS to the output of tools/bashcorpus")
	}

	type sample struct {
		peak  float64
		start time.Time
		shape string
	}
	var all []sample
	for _, r := range readCorpus(t, path) {
		if r.synthetic() || !r.Labelled || r.AttrSamplesN < minAttrSamplesForCost {
			continue
		}
		res := Classify(r.Cmd)
		if !res.Gated || len(res.Shapes) == 0 {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, r.Start)
		all = append(all, sample{peak: r.AttrChildPeak, start: ts, shape: res.Shapes[0]})
	}
	if len(all) == 0 {
		t.Fatal("no fittable gated rows in the corpus: nothing can be concluded")
	}

	sort.Slice(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
	mid := len(all) / 2
	byShape := func(half []sample) map[string][]float64 {
		m := map[string][]float64{}
		for _, s := range half {
			m[s.shape] = append(m[s.shape], s.peak)
		}
		for k := range m {
			sort.Float64s(m[k])
		}
		return m
	}
	first, second := byShape(all[:mid]), byShape(all[mid:])

	// minForP90 is higher than TestClassCostVariance's n>=5 on purpose. The p90 of five points
	// IS the maximum, so a five-point p90 is not a percentile, it is an extreme. Twenty puts
	// two observations above the mark.
	const minForP90 = 20
	var names []string
	for k := range first {
		if len(first[k]) >= minForP90 && len(second[k]) >= minForP90 {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no shape has %d observations in both halves; the p90 question cannot be "+
			"asked of this corpus at all, which is itself an answer for GOV-B14", minForP90)
	}

	const cores = 8.0
	shippedSlotCores := 1.2 // budget.SlotCores; hard-coded to avoid an import cycle in tests

	t.Logf("per-shape p90 stability across the corpus's two chronological halves (n >= %d in both)",
		minForP90)
	// OVERLAPPING INTERVALS ARE NOT THE SAME AS AGREEING INTERVALS, and conflating them is the
	// easiest way to read this table wrongly. Two p90s whose intervals are each a factor of two
	// wide will overlap whatever the truth is: that is a corpus too thin to disagree, not a
	// stable statistic. A shape is only usable for sizing if its interval is also NARROW
	// relative to the number being sized, so the width is reported as a fraction of the p90 and
	// has to clear maxCIWidth before the shape counts.
	const maxCIWidth = 0.25
	t.Logf("%-16s %-22s %-22s %7s %7s  %-9s %s", "shape", "first half p90 [95% CI]",
		"second half p90 [95% CI]", "ratio", "CIwidth", "verdict", "B implied (8 cores)")

	usable := 0
	for _, k := range names {
		a, b := first[k], second[k]
		pa, loA, hiA := p90CI(a, k+"-a")
		pb, loB, hiB := p90CI(b, k+"-b")
		overlap := loA <= hiB && loB <= hiA
		// The wider of the two intervals, relative to its own p90.
		widthA, widthB := (hiA-loA)/math.Max(pa, 0.001), (hiB-loB)/math.Max(pb, 0.001)
		width := math.Max(widthA, widthB)
		precise := width <= maxCIWidth

		verdict := "MOVED"
		switch {
		case overlap && precise:
			verdict = "stable"
		case overlap && !precise:
			verdict = "TOO THIN" // cannot disagree, which is not the same as agreeing
		}
		ratio := safeRatio(math.Max(pa, pb), math.Min(pa, pb))
		t.Logf("%-16s %6.3f [%5.3f,%5.3f]    %6.3f [%5.3f,%5.3f]  %6.2fx %6.0f%%  %-9s %d -> %d",
			k, pa, loA, hiA, pb, loB, hiB, ratio, 100*width, verdict,
			int(cores/math.Max(pa, 0.001)), int(cores/math.Max(pb, 0.001)))
		// A shape is usable by GOV-B14 only if its two halves agree AND correcting to it
		// actually changes the budget. Agreeing about a number that yields the same B as the
		// constant is a table that moves nothing, which the story says to close rather than ship.
		bShape := int(cores / math.Max((pa+pb)/2, 0.001))
		bConst := int(cores / shippedSlotCores)
		if overlap && precise && bShape != bConst {
			usable++
		}
	}

	t.Logf("")
	t.Logf("shapes whose p90 agrees across halves, is precise enough to size with, AND changes "+
		"B away from the shipped constant (B=%d): %d of %d",
		int(cores/shippedSlotCores), usable, len(names))
	t.Logf("a shape failing any of the three cannot carry a per-shape correction: an unstable " +
		"p90 is a number about a fortnight, an imprecise one cannot be distinguished from the " +
		"constant, and a stable precise one that reproduces B is a table that moves nothing")
}

// p90CI returns the p90 of sorted v and a bootstrap 95% confidence interval for it. The seed is
// derived from the label so each shape is reproducible and independent.
func p90CI(sorted []float64, label string) (p90, lo, hi float64) {
	p90 = durQuantile(sorted, 0.90)
	const reps = 2000
	var seed int64
	for _, c := range label {
		seed = seed*31 + int64(c)
	}
	rng := rand.New(rand.NewSource(seed))
	boot := make([]float64, 0, reps)
	buf := make([]float64, len(sorted))
	for i := 0; i < reps; i++ {
		for j := range buf {
			buf[j] = sorted[rng.Intn(len(sorted))]
		}
		sort.Float64s(buf)
		boot = append(boot, durQuantile(buf, 0.90))
	}
	sort.Float64s(boot)
	return p90, durQuantile(boot, 0.025), durQuantile(boot, 0.975)
}
