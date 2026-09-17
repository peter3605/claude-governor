package classify

import (
	"os"
	"sort"
	"testing"
)

// TestClassDurations measures how long a gated command actually holds a slot, by cost class.
//
// This is GOV-18 clause 4's evidence. The classifier ships an expected PEAK COST per class
// (ExpectedPeakCores) and says nothing about DURATION, but a fair-share queue is sensitive to
// hold time rather than to cost: FIFO over items of wildly unequal size is where fair-share
// schemes fail. So the question is whether the cost class predicts the hold time well enough
// to be worth scheduling on.
//
// Skipped unless GOVERNOR_CORPUS points at the output of tools/bashcorpus, for the same
// reason TestEvaluateAgainstFullCorpus is: the corpus is a real machine's traffic and does
// not belong in the repository.
//
//	go build -o /tmp/bashcorpus ./tools/bashcorpus && /tmp/bashcorpus -out /tmp/corpus.jsonl
//	GOVERNOR_CORPUS=/tmp/corpus.jsonl go test ./internal/classify/ -run ClassDurations -v
func TestClassDurations(t *testing.T) {
	path := os.Getenv("GOVERNOR_CORPUS")
	if path == "" {
		t.Skip("set GOVERNOR_CORPUS to the output of tools/bashcorpus")
	}
	rows := readCorpus(t, path)

	byClass := map[Class][]float64{}
	var gatedAll []float64
	var ungated []float64

	for _, r := range rows {
		if r.synthetic() || r.DurS <= 0 {
			continue
		}
		res := Classify(r.Cmd)
		if !res.Gated {
			ungated = append(ungated, r.DurS)
			continue
		}
		gatedAll = append(gatedAll, r.DurS)
		byClass[res.Class] = append(byClass[res.Class], r.DurS)
	}

	report := func(name string, v []float64) {
		if len(v) == 0 {
			t.Logf("%-10s n=0", name)
			return
		}
		sort.Float64s(v)
		t.Logf("%-10s n=%5d  p50=%7.2fs  p90=%8.2fs  p99=%9.2fs  max=%9.2fs  mean=%7.2fs",
			name, len(v), durQuantile(v, 0.50), durQuantile(v, 0.90), durQuantile(v, 0.99), v[len(v)-1], meanOf(v))
	}

	t.Log("hold time of a gated command, by cost class (all foreground calls with a runtime)")
	report("ungated", ungated)
	report("gated", gatedAll)
	for _, c := range []Class{Light, Medium, Heavy} {
		report(c.String(), byClass[c])
	}

	// The ratio that decides clause 4: if the classes' hold times are not separated, the
	// cost class is not a duration predictor and the queue cannot schedule on it.
	for _, c := range []Class{Light, Medium, Heavy} {
		v := byClass[c]
		if len(v) == 0 {
			continue
		}
		sort.Float64s(v)
		t.Logf("%-10s expected peak %.2f cores -> p90 hold %.2fs", c.String(), c.ExpectedPeakCores(), durQuantile(v, 0.90))
	}

	// SEPARATION vs SPREAD. A class is only worth scheduling on if knowing it tells you more
	// about the hold time than the spread WITHIN the class leaves unknown. Between-class
	// separation is the ratio of the two p90s; within-class spread is p99/p50. If the second
	// dwarfs the first, the class is a weak predictor and reordering the queue on it would be
	// scheduling on noise.
	med, hvy := byClass[Medium], byClass[Heavy]
	sort.Float64s(med)
	sort.Float64s(hvy)
	if len(med) > 0 && len(hvy) > 0 {
		t.Logf("between-class separation (heavy p90 / medium p90): %.1fx",
			durQuantile(hvy, 0.90)/durQuantile(med, 0.90))
		t.Logf("within-class spread medium (p99/p50): %.1fx   heavy (p99/p50): %.1fx",
			durQuantile(med, 0.99)/durQuantile(med, 0.50),
			durQuantile(hvy, 0.99)/durQuantile(hvy, 0.50))

		// The overlap: how often a Medium outlasts the MEDIAN Heavy. If the classes were
		// cleanly separated this would be near zero. Every point of it is a case where
		// scheduling by class would have made the wrong call.
		hMed := durQuantile(hvy, 0.50)
		over := 0
		for _, d := range med {
			if d > hMed {
				over++
			}
		}
		t.Logf("overlap: %.1f%% of medium commands outlast the median heavy (%.2fs)",
			100*float64(over)/float64(len(med)), hMed)
	}

	// WHAT A LONG BUILD COSTS A STARVED SESSION, in the only unit that matters: the share of
	// gated commands that outlast a full parked wait. A waiter behind one of these is
	// refused rather than served, however the queue is ordered.
	const parkedWait = 30.0
	outlast := func(name string, v []float64) {
		if len(v) == 0 {
			return
		}
		n := 0
		for _, d := range v {
			if d > parkedWait {
				n++
			}
		}
		t.Logf("%-10s %5.1f%% outlast a %.0fs parked wait", name, 100*float64(n)/float64(len(v)), parkedWait)
	}
	outlast("gated", gatedAll)
	outlast("medium", med)
	outlast("heavy", hvy)
}

func durQuantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func meanOf(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}
