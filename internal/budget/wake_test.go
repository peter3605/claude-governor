package budget

import (
	"math"
	"testing"
	"time"
)

// decay is one kernel load-average update: the average moves toward n by 1-e^(-5/60).
func decay(la, n float64) float64 {
	m := math.Exp(-LoadavgPeriod.Seconds() / LoadavgTau.Seconds())
	return la*m + n*(1-m)
}

// wakeTrace is the raw run queue a 1 Hz sampler reads after a wake: the frozen value until
// the first kernel update, then a new value every fifth tick, each one update further toward
// the runnable count now. firstUpdate is the tick the first update lands on (1-5).
func wakeTrace(frozen, now float64, firstUpdate, ticks int) []float64 {
	out := make([]float64, ticks)
	la := frozen
	for i := range out {
		if i+1 >= firstUpdate && (i+1-firstUpdate)%5 == 0 {
			la = decay(la, now)
		}
		out[i] = la
	}
	return out
}

// TestAwayIsTheTimeTheProcessDidNotExperience: the monotonic clock stops during sleep on both
// platforms, so the difference between the clocks is the sleep, and an ordinary late tick is
// not one.
func TestAwayIsTheTimeTheProcessDidNotExperience(t *testing.T) {
	cases := []struct {
		wall, mono, want time.Duration
	}{
		{12 * time.Hour, time.Second, 12*time.Hour - time.Second}, // hibernate, 2026-11-21
		{909 * time.Second, time.Second, 908 * time.Second},       // a DarkWake gap, 2026-11-22
		{2 * time.Second, time.Second, 0},                         // a late tick
		{20 * time.Second, 20 * time.Second, 0},                   // a stalled process, not a sleep
		{SleepGap + time.Second, time.Second, 0},                  // exactly at the threshold
	}
	for _, c := range cases {
		if got := Away(c.wall, c.mono); got != c.want {
			t.Errorf("Away(wall %v, mono %v) = %v, want %v", c.wall, c.mono, got, c.want)
		}
	}
}

// TestWakeFilterRemovesExactlyThePreSleepMemory checks the filter against the measured decay.
// On 2026-11-22 the trace read loadavg1 118.99 before a 319 s gap and 109.46 on the first tick
// after it: one update toward an idle machine. The corrected reading must be that idle
// machine, not the frozen 14.9 per core.
func TestWakeFilterRemovesExactlyThePreSleepMemory(t *testing.T) {
	const cores = 8
	f := WakeFilter{Frozen: true}
	f.Observe(118.99/cores, 0)
	got, ok := f.Observe(109.46/cores, 319*time.Second)
	if !ok {
		t.Fatal("an updated reading was held as stale")
	}
	if got > 0.05 {
		t.Errorf("corrected run queue %.3f, want ~0: the machine was idle and the rest is pre-sleep memory", got)
	}
	if f.Before() != 118.99/cores {
		t.Errorf("Before() = %v, want the frozen %v", f.Before(), 118.99/cores)
	}

	// And a machine that is genuinely busy at wake reads as busy, from the first update on.
	for _, now := range []float64{0.8, 3.0, 7.77} {
		f := WakeFilter{Frozen: true}
		f.Observe(6.73, 0)
		trace := wakeTrace(6.73, now, 3, 60)
		for i, raw := range trace {
			away := time.Duration(0)
			if i == 0 {
				away = 12 * time.Hour
			}
			got, ok := f.Observe(raw, away)
			if i < 2 {
				if ok {
					t.Errorf("now=%.2f tick %d: the frozen value was passed as a reading", now, i)
				}
				continue
			}
			if !ok || math.Abs(got-now) > 1e-9 && f.Waking() {
				t.Fatalf("now=%.2f tick %d: corrected %.4f ok=%v, want %.2f", now, i, got, ok, now)
			}
		}
	}
}

// TestWakeFilterHoldIsBounded: an update that leaves the average unchanged is possible (the
// runnable count equalled it), and the filter must not hold forever waiting for a change.
func TestWakeFilterHoldIsBounded(t *testing.T) {
	f := WakeFilter{Frozen: true}
	f.Observe(3, 0)
	held := 0
	for i := 0; i < 20; i++ {
		away := time.Duration(0)
		if i == 0 {
			away = time.Hour
		}
		if got, ok := f.Observe(3, away); !ok {
			held++
		} else if math.Abs(got-3) > 1e-9 {
			t.Fatalf("tick %d: unchanged average corrected to %.4f, want 3: it was never stale", i, got)
		}
	}
	if held != wakeHoldTicks-1 {
		t.Errorf("held %d ticks, want %d", held, wakeHoldTicks-1)
	}
}

// TestWakeFilterPassesThroughWhereTheFreezeIsUnmeasured is the Linux half of clause 5.
func TestWakeFilterPassesThroughWhereTheFreezeIsUnmeasured(t *testing.T) {
	f := WakeFilter{Frozen: false}
	f.Observe(6.73, 0)
	for i, raw := range wakeTrace(6.73, 0.8, 3, 20) {
		away := time.Duration(0)
		if i == 0 {
			away = time.Hour
		}
		if got, ok := f.Observe(raw, away); !ok || got != raw {
			t.Fatalf("tick %d: got %.4f ok=%v, want the raw %.4f", i, got, ok, raw)
		}
	}
}

// replayWake drives a controller through clause 4's scenario and reports the budget on each
// post-wake tick. correct says whether the tick carries GOV-B48's wake handling; without it
// the controller sees exactly what it saw before GOV-B48 — raw readings, no Woke, no Stale.
func replayWake(correct bool, frozen, now float64) (budgets []int, c *Controller) {
	c = &Controller{Max: 4, RunQueueMax: 5.52} // this machine's calibration
	f := WakeFilter{Frozen: true}
	// Before the sleep: a build running, the run queue over the cut. 4 -> 2 -> 1.
	for i := 0; i < 3; i++ {
		c.Step(Reading{RunQueue: frozen, InUse: true})
		f.Observe(frozen, 0)
	}
	// Hours asleep, then the machine wakes on the frozen average, which decays toward the
	// machine as it is now. A lease is still held, as it was on 2026-11-21 (1.08 of 1 slot).
	for i, raw := range wakeTrace(frozen, now, 4, 240) {
		away := time.Duration(0)
		if i == 0 {
			away = 12 * time.Hour
		}
		r := Reading{RunQueue: raw, InUse: true}
		if correct {
			rq, ok := f.Observe(raw, away)
			r.RunQueue, r.Woke, r.Stale = rq, away > 0, !ok
		}
		budgets = append(budgets, c.Step(r))
	}
	return budgets, c
}

// TestABuildAskingAtWakeIsNotRefusedOnTheStaleReading is clause 4. The 2026-11-21 shape: cut
// to 1 at run queue 6.73 per core, twelve hours of hibernate, and readings that start at 6.73
// and decay toward an idle 0.8. A build asking on the first tick after wake must find the
// budget at the ceiling, and the decaying pre-sleep average must not cut it again.
func TestABuildAskingAtWakeIsNotRefusedOnTheStaleReading(t *testing.T) {
	budgets, c := replayWake(true, 6.73, 0.8)
	for i, b := range budgets {
		if b != 4 {
			t.Fatalf("tick %d after wake: budget %d, want 4 — refused on a stale reading (budgets %v)", i, b, budgets[:i+1])
		}
	}
	if c.Cut() != "" {
		t.Errorf("Cut() = %q after waking to an idle machine, want empty", c.Cut())
	}

	// The same readings WITHOUT the wake handling are the pre-GOV-B48 controller, and it is
	// the defect: still at 1 on the first tick, and cut back down by the frozen average.
	before, _ := replayWake(false, 6.73, 0.8)
	if before[0] != 1 {
		t.Fatalf("pre-GOV-B48 controller at wake: budget %d, want 1 (the defect this test exists for)", before[0])
	}
}

// TestAMachineBusyAtWakeIsStillCut is the other direction of clause 3: releasing the budget on
// a stale reading must not become oversubscribing a machine that really is busy at wake. The
// measured full wake of 2026-11-22 reached 7.77 per core; the loop must cut on the first
// fresh reading, which is the first kernel update — never later than five seconds.
func TestAMachineBusyAtWakeIsStillCut(t *testing.T) {
	budgets, _ := replayWake(true, 6.73, 7.77)
	// wakeTrace(…, 4, …) puts the first update on the fourth tick (index 3).
	for i := 0; i < 3; i++ {
		if budgets[i] != 4 {
			t.Fatalf("tick %d: budget %d before any fresh reading, want 4 (held at the restored ceiling)", i, budgets[i])
		}
	}
	if budgets[3] != 2 || budgets[4] != 1 {
		t.Fatalf("first fresh ticks: budgets %v, want 4 -> 2 -> 1 from the first update", budgets[:6])
	}
}

// TestADarkWakeAtTheCeilingMovesNothing is the DarkWake trap: the daemon runs for a few
// seconds every few minutes all night, and a wake that finds nothing cut must not touch the
// budget or name a cause.
func TestADarkWakeAtTheCeilingMovesNothing(t *testing.T) {
	c := &Controller{Max: 4, RunQueueMax: 5.52}
	c.Step(Reading{RunQueue: 0.5})
	if b := c.Step(Reading{RunQueue: 0.5, Woke: true, Stale: true}); b != 4 || c.Moved() != "" {
		t.Errorf("DarkWake at the ceiling: budget %d moved %q, want 4 and nothing", b, c.Moved())
	}
}

// TestAStaleTickStillFloorsOnThermal: thermalState is read live at wake, so a stale run queue
// suspends the run-queue branches and nothing else.
func TestAStaleTickStillFloorsOnThermal(t *testing.T) {
	c := &Controller{Max: 4, RunQueueMax: 5.52}
	c.Step(Reading{RunQueue: 0.5})
	if b := c.Step(Reading{RunQueue: 9, Stale: true, ThermalSevere: true}); b != Floor {
		t.Errorf("stale tick with severe thermal: budget %d, want %d", b, Floor)
	}
}
