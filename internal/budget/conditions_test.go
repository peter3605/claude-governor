package budget

import (
	"fmt"
	"testing"
)

// GOV-32 CLAUSE 1: THE LOOP'S TWO CONDITIONS, COUNTED OVER REAL LOAD.
//
// GOV-31 finished by measuring the CUT condition firing on 72-99% of a build fan-out's ticks
// and left the INCREASE condition unmeasured. That asymmetry is why "the budget cannot climb
// back while a build runs" stood as a reading of the code rather than as a number: it is
// obvious from `Setpoint - CPU > Headroom` that a pegged machine cannot satisfy it, and
// obvious is not measured.
//
// WHAT THE COUNT FOUND, on the loop as it stood:
//
//	ten-way fan-out, ungoverned       cut 100% of busy ticks, climb  0%
//	ten-way fan-out, governed at 2.0  cut  93%,               climb  4%
//	3,000-tick real trace             cut  67%,               climb 30%
//	  of its 471 ticks at CPU >= 0.99                         climb  0%
//
// Zero was not a small number, it was a structural one: the climb required CPU below
// Setpoint-Headroom = 0.65 and saturation means CPU at 1.00. docs/climb-condition.md carries
// the fan-out measurement and section 3 of it is why that was judged over-control rather than
// control.
//
// THIS TEST NOW CHARACTERISES THE LOOP THAT REPLACED IT. It still prints both shares over the
// same trace, because the point is to make any later change to the loop announce what it did
// to them, and it still fails on a qualitative change rather than pinning percentages a
// different trace would not reproduce.
func TestTheLoopsConditionsOverARealTrace(t *testing.T) {
	trace := loadTrace(t)
	climbAt := (&Controller{}).climbRunQueue()

	var cut, climb, neither, saturated, saturatedClimb, saturatedQuiet int
	for _, r := range trace {
		c, u := Cutting(r, RunQueueMax), Climbing(r, climbAt)
		switch {
		case c:
			cut++
		case u:
			climb++
		default:
			neither++
		}
		// SATURATED IS THE REGIME THIS STORY IS ABOUT. A build fan-out holds host CPU at
		// p50 1.00 (docs/run-queue-sweep.md section 4), so the ticks at or above 0.99 here
		// are the trace's own sample of the state a fan-out puts the machine in.
		if r.CPU >= 0.99 {
			saturated++
			if u {
				saturatedClimb++
			}
			if r.RunQueue <= climbAt {
				saturatedQuiet++
			}
		}
	}
	n := len(trace)
	pct := func(k int, of int) string {
		if of == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.1f%%", 100*float64(k)/float64(of))
	}
	t.Logf("over %d real ticks: cut %d (%s), climb %d (%s), neither %d (%s)",
		n, cut, pct(cut, n), climb, pct(climb, n), neither, pct(neither, n))
	t.Logf("of the %d ticks at CPU >= 0.99 (%s of the trace), %d had a run queue at or under "+
		"the climb threshold of %.2f and %d satisfied the climb condition (%s)",
		saturated, pct(saturated, n), saturatedQuiet, climbAt, saturatedClimb,
		pct(saturatedClimb, saturated))

	if saturated == 0 {
		t.Fatalf("the trace contains no saturated ticks; it cannot characterise the regime "+
			"this test exists for (%d ticks read)", n)
	}

	// THE CLAIM UNDER TEST, and it is the reverse of the one this file was created to record.
	// Under the old loop no saturated tick could satisfy the climb, whatever the queue was
	// doing. Under this one a saturated tick climbs exactly when the queue is shallow, so the
	// two counts above must agree: saturation is no longer, on its own, a reason not to climb.
	if saturatedClimb != saturatedQuiet {
		t.Errorf("%d saturated ticks had a shallow queue but %d satisfied the climb "+
			"condition; on a loop that climbs on the run queue alone those must be the same "+
			"ticks, so something else is gating the increase", saturatedQuiet, saturatedClimb)
	}
	if saturatedQuiet > 0 && saturatedClimb == 0 {
		t.Errorf("%d of %d saturated ticks had a run queue at or under %.2f and none of them "+
			"could climb; that is the GOV-32 defect, returned", saturatedQuiet, saturated, climbAt)
	}
}

// TestASaturatedMachineWithAShallowQueueIsNotTrouble is the property the fix turns on, stated
// against the constants so that it cannot be read as an artefact of one trace.
//
// IT REPLACES TestTheClimbConditionIsUnreachableUnderSaturation, which asserted the opposite
// and was correct when it was written. What that test pinned: at CPU 1.00 with an empty run
// queue the loop CUT, because `CPU > Setpoint+SetpointBand` fired on its own, and it could not
// climb at any queue depth, because the increase needed CPU below 0.65. Sixty saturated ticks
// took the budget from the ceiling to the floor and left it there.
//
// That is now inverted, deliberately. A machine at full utilisation with nothing queued behind
// it is a build machine working, not a machine in trouble, and docs/climb-condition.md section
// 3 is the measurement that decided it: at the operating point the old loop drove to, this
// host reads CPU 0.77 and run queue 1.22 per core, which is 23% idle with work waiting.
func TestASaturatedMachineWithAShallowQueueIsNotTrouble(t *testing.T) {
	climbAt := (&Controller{}).climbRunQueue()

	// Fully utilised, nothing queued: the friendliest saturated state there is.
	r := Reading{CPU: 1.0, RunQueue: 0.0, DemandWaiting: true}
	if Cutting(r, RunQueueMax) {
		t.Errorf("the cut condition fires at CPU 1.00 with an empty run queue; utilisation is "+
			"deciding again, and a loop that cuts here has no operating point above the floor "+
			"while any build runs")
	}
	if !Climbing(r, climbAt) {
		t.Errorf("the climb condition is not satisfied at CPU 1.00 with an empty run queue, "+
			"against a climb threshold of %.2f; the increase is unreachable under saturation, "+
			"which is the defect GOV-32 measured", climbAt)
	}

	// And the budget that follows: from the floor to the ceiling while CPU stays pegged
	// throughout. Six ticks is the climb time on this machine, so sixty is ample.
	c := &Controller{Max: MaxSlots(8)}
	for i := 0; i < 20; i++ {
		c.Step(Reading{CPU: 1.0, RunQueue: 30.0, DemandWaiting: true})
	}
	if got := c.Budget(); got != Floor {
		t.Fatalf("a run queue of 30 per core left the budget at %d; this test cannot tell a "+
			"recovery from a budget that never cut", got)
	}
	for i := 0; i < 60; i++ {
		c.Step(r)
	}
	if got := c.Budget(); got != MaxSlots(8) {
		t.Errorf("after 60 ticks at CPU 1.00 with an empty queue the budget is %d, want the "+
			"ceiling %d: B_max must be reachable while builds are running", got, MaxSlots(8))
	}
}

// TestADeepQueueStillCutsWhateverTheUtilisation is the other half, and it is the regression
// that matters most: the fix removed a cut trigger, and the thing it must not do is stop
// governing.
//
// The load that motivated the whole design — ten concurrent builds on an eight-core laptop —
// produces a run queue of 25 per core (docs/run-queue-sweep.md section 4). That must floor the
// budget, and it must do so whether or not CPU is reading anything useful at the time.
func TestADeepQueueStillCutsWhateverTheUtilisation(t *testing.T) {
	for _, cpu := range []float64{0.0, 0.50, 0.77, 1.00} {
		c := &Controller{Max: MaxSlots(8)}
		for i := 0; i < 20; i++ {
			c.Step(Reading{CPU: cpu, RunQueue: 24.98, DemandWaiting: true})
		}
		if got := c.Budget(); got != Floor {
			t.Errorf("at CPU %.2f a run queue of 24.98 per core settled at %d, want the "+
				"floor %d: saturation of the queue is the cut, and it does not depend on "+
				"what utilisation happens to read", cpu, got, Floor)
		}
	}
}
