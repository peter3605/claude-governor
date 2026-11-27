package budget

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

// clockQuiet is a reading with every other cause at rest, so only the clock can move the budget.
func clockQuiet(known bool, active, frac float64) Reading {
	return Reading{RunQueue: 0.2, InUse: true, PClockKnown: known, PClockActive: active, PClockFrac: frac}
}

// A sustained throttle cuts, by the clock's own cause, once most of the window is slow.
func TestASustainedThrottleCutsByClock(t *testing.T) {
	c := &Controller{Max: 3}
	cutAt := 0
	for i := 1; i <= 120 && cutAt == 0; i++ {
		c.Step(clockQuiet(true, 1, 0.60))
		if c.Moved() == MovedClock {
			cutAt = i
		}
	}
	if cutAt < 30 || cutAt > 32 {
		t.Fatalf("cut on tick %d; want the tick a majority of the 60-tick window turned slow (~31)", cutAt)
	}
	for i := 0; i < 3; i++ {
		c.Step(clockQuiet(true, 1, 0.60))
	}
	if c.Budget() != Floor || c.Cut() != "clock" {
		t.Errorf("budget %d cut %q, want the floor held by the clock", c.Budget(), c.Cut())
	}
}

// An idle P-cluster clocks down by design; at any clock it must never cut.
func TestAnIdleClusterNeverCuts(t *testing.T) {
	c := &Controller{Max: 3}
	for i := 0; i < 600; i++ {
		if c.Step(clockQuiet(true, 0.20, 0.35)) != 3 {
			t.Fatalf("tick %d: an idle cluster at 35%% clock cut the budget to %d (%s)", i, c.Budget(), c.Moved())
		}
	}
}

// Unknown is nominal: a machine that cannot read its clock is not a throttled one.
func TestAnUnknownClockNeverCuts(t *testing.T) {
	c := &Controller{Max: 3}
	for i := 0; i < 600; i++ {
		if c.Step(clockQuiet(false, 1, 0.10)) != 3 {
			t.Fatalf("tick %d: an unknown clock cut the budget", i)
		}
	}
}

// Between the cut and clear levels the budget holds, demand or not; above clear it climbs.
func TestTheClockHoldsUntilClear(t *testing.T) {
	c := &Controller{Max: 3}
	for i := 0; i < 40; i++ {
		c.Step(clockQuiet(true, 1, 0.60))
	}
	hover := clockQuiet(true, 1, 0.81) // under ClockClearFrac, over ClockCutFrac
	hover.DemandWaiting = true
	for i := 0; i < 120; i++ {
		if c.Step(hover) != Floor {
			t.Fatalf("tick %d: climbed to %d at 0.81, inside the hysteresis band", i, c.Budget())
		}
	}
	if c.Cut() != "clock" {
		t.Errorf("hold names %q, want clock", c.Cut())
	}
	clear := clockQuiet(true, 1, 0.92)
	clear.DemandWaiting = true
	for i := 0; i < 60 && c.Budget() < 3; i++ {
		c.Step(clear)
	}
	if c.Budget() != 3 || c.Cut() != "" {
		t.Errorf("budget %d cut %q after the clock cleared, want the ceiling and no cut", c.Budget(), c.Cut())
	}
}

// A wake discards the window: a cut from before the sleep does not survive it (GOV-B48).
func TestAWakeForgetsTheClock(t *testing.T) {
	c := &Controller{Max: 3}
	for i := 0; i < 29; i++ { // one short of cutting
		c.Step(clockQuiet(true, 1, 0.60))
	}
	w := clockQuiet(true, 1, 0.60)
	w.Woke = true
	c.Step(w)
	for i := 0; i < 20; i++ {
		c.Step(clockQuiet(true, 1, 0.60))
	}
	if c.Budget() != 3 {
		t.Errorf("cut %d after a wake on pre-sleep evidence (%s)", c.Budget(), c.Moved())
	}
}

// replayRun steps a controller through one GOV-B54/B60 run, a powermetrics second per tick, and
// returns the tick of the first clock cut (0 if none) and the busy seconds seen by then.
//
// powermetrics stands in for IOReport because below the 2988 MHz all-core cap the two agree
// within 2-3 MHz (design.md section 5.3); above it powermetrics reads 2988 where IOReport reads
// up to 3204, and both are far above the cut level.
func replayRun(t *testing.T, name string) (cutTick, ticks int) {
	t.Helper()
	f, err := os.Open("../../docs/data/thermal-throttle/" + name + ".tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const top = 3204.0 // the M1 table top these runs were taken on
	c := &Controller{Max: 3}
	sc := bufio.NewScanner(f)
	sc.Scan() // header: at p_mhz p_res ...
	for sc.Scan() {
		col := strings.Split(sc.Text(), "\t")
		if len(col) < 3 {
			continue
		}
		mhz, err1 := strconv.ParseFloat(col[1], 64)
		res, err2 := strconv.ParseFloat(col[2], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		ticks++
		c.Step(clockQuiet(true, res/100, mhz/top))
		if c.Moved() == MovedClock && cutTick == 0 {
			cutTick = ticks
		}
	}
	return cutTick, ticks
}

// The constants against the runs they were derived from: every saturating run cuts within
// 90 s (plus the few non-busy seconds at its start), and the real build fan-out never does.
func TestTheClockCutAgainstTheMeasuredRuns(t *testing.T) {
	for _, run := range []string{"run-A-saturate", "run-A2-saturate", "run-A3-saturate"} {
		cut, n := replayRun(t, run)
		if cut == 0 || cut > 95 {
			t.Errorf("%s: clock cut at tick %d of %d, want within ~90 s", run, cut, n)
		}
		t.Logf("%s: cut at %d s", run, cut)
	}
	if cut, n := replayRun(t, "run-B-fanout"); cut != 0 {
		t.Errorf("run-B-fanout: the real build fan-out was clock-cut at tick %d of %d", cut, n)
	}
}
