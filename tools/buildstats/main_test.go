package main

import "testing"

// TestPercentileIsMonotonicAndExactAtTheEnds guards the reading of clause 2's scored quantity.
//
// A percentile off by one rank reports a number that looks entirely reasonable and is wrong,
// which is how the retired clause-3 grep published 93 test runs where the window held 144. The
// scored statistic now gets the same treatment the counted one has.
func TestPercentileIsMonotonicAndExactAtTheEnds(t *testing.T) {
	a := make([]float64, 100)
	for i := range a {
		a[i] = float64(i + 1)
	}
	cp := func() []float64 { return append([]float64(nil), a...) }

	if got := pct(cp(), 0); got != 1 {
		t.Errorf("p0 = %v, want the minimum 1", got)
	}
	// advice.md's clause 2 elects the maximum, so this end is the one a wrong rank would
	// quietly understate.
	if got := pct(cp(), 1); got != 100 {
		t.Errorf("p100 = %v, want the maximum 100", got)
	}

	prev := -1.0
	for _, p := range []float64{0, .5, .9, .95, .99, 1} {
		got := pct(cp(), p)
		if got < 1 || got > 100 {
			t.Errorf("p%v = %v, outside the input", p, got)
		}
		if got < prev {
			t.Errorf("p%v = %v went backwards from %v", p, got, prev)
		}
		prev = got
	}
}

// TestPercentileOfNothingIsNotACrash keeps a day with no samples from taking the report down.
// The window deliberately spans days the lid was shut, which carry no readings at all.
func TestPercentileOfNothingIsNotACrash(t *testing.T) {
	if got := pct(nil, .99); got != 0 {
		t.Errorf("pct(nil) = %v, want 0", got)
	}
}

// TestAThinDayIsNotHalfOfTheComparison pins the floors that keep a near-dead day out of the
// halves test. 2026-09-22 — day one of the baseline window — carried 6 builds in 0.87 awake
// hours because the lid was shut. With three qualifying days it would have been half the
// baseline, which is the same defect as the 2.21x volume mismatch arriving through the
// instrument instead of the workload.
func TestAThinDayIsNotHalfOfTheComparison(t *testing.T) {
	for _, c := range []struct {
		name    string
		granted int
		awake   int
		want    bool
	}{
		{"a real active day", 88, 4 * 3600, true},
		{"exactly on both floors", minActiveBuilds, minActiveAwakeSec, true},
		{"2026-09-22, the lid was shut", 6, 3132, false},
		{"enough builds, too little awake time", 200, 600, false},
		{"enough awake time, too few builds", 3, 6 * 3600, false},
		{"a day with nothing at all", 0, 0, false},
	} {
		d := &day{granted: c.granted, awakeSec: c.awake}
		if got := d.active(); got != c.want {
			t.Errorf("%s: %d builds in %.2f awake h -> active()=%v, want %v",
				c.name, d.builds(), float64(c.awake)/3600, got, c.want)
		}
	}
}
