package budget

import (
	"math"
	"math/rand"
	"testing"
)

// The control loop's tests (GOV-15 clause 2).
//
// The clause asks for three properties over a replay: the budget never exceeds B_max, never
// drops below 1, and SETTLES rather than oscillating. The first two are easy and the third is
// the one worth writing carefully, because "settles" is the property a plausible-looking AIMD
// loop fails silently — it keeps working, the budget just flaps between two values forever and
// the machine is alternately starved and over-subscribed.

// tick is one reading in a replay trace.
type tick struct {
	cpu      float64
	runQueue float64
	severe   bool
	demand   bool
}

// replay steps a controller over a trace and returns every budget it passed through.
func replay(c *Controller, trace []tick) []int {
	out := make([]int, 0, len(trace))
	for _, t := range trace {
		out = append(out, c.Step(Reading{
			CPU: t.cpu, RunQueue: t.runQueue, ThermalSevere: t.severe, DemandWaiting: t.demand,
		}))
	}
	return out
}

// idle is the shape of this machine most of the time: the measured median tick, with
// something waiting so the loop is free to climb.
func idle() tick { return tick{cpu: 0.185, runQueue: 0.351, demand: true} }

// busy is over the setpoint band and over the run-queue threshold — a tick the loop must cut
// on. Drawn from the measured p95: CPU 0.728 rounded up past the band, run queue 2.946.
func busy() tick { return tick{cpu: 0.95, runQueue: 2.946, demand: true} }

func TestTheBudgetStaysInsideItsBounds(t *testing.T) {
	// A trace that swings between the two extremes as hard as it can, which is the worst
	// case for the bounds: every tick is either a halving or an increase.
	rng := rand.New(rand.NewSource(1))
	trace := make([]tick, 0, 5000)
	for i := 0; i < 5000; i++ {
		switch rng.Intn(3) {
		case 0:
			trace = append(trace, busy())
		case 1:
			trace = append(trace, idle())
		default:
			trace = append(trace, tick{cpu: rng.Float64(), runQueue: rng.Float64() * 4, demand: rng.Intn(2) == 0})
		}
	}

	c := &Controller{Max: MaxSlots(8)}
	for i, b := range replay(c, trace) {
		if b < Floor {
			t.Fatalf("tick %d: budget %d is below the floor of %d; invariant 4 says a machine "+
				"under duress still makes progress", i, b, Floor)
		}
		if b > c.Max {
			t.Fatalf("tick %d: budget %d exceeds B_max of %d", i, b, c.Max)
		}
	}
}

// A thermal cut goes straight to the floor, whatever else the tick says.
func TestASevereThermalStateFloorsTheBudgetAtOnce(t *testing.T) {
	c := &Controller{Max: MaxSlots(8)}
	replay(c, []tick{idle(), idle(), idle(), idle(), idle(), idle()})
	if c.Budget() <= Floor {
		t.Fatalf("the budget never climbed off the floor on an idle machine: %d", c.Budget())
	}

	got := c.Step(Reading{CPU: 0.0, RunQueue: 0.0, ThermalSevere: true})
	if got != Floor {
		t.Errorf("budget = %d after a severe thermal reading, want the floor of %d", got, Floor)
	}
}

// And it does not climb straight back out: three clean ticks first, so the budget does not
// oscillate against the platform's own thermal governor (design.md section 5.3).
func TestTheClimbBackFromAThermalCutIsHeldOff(t *testing.T) {
	c := &Controller{Max: MaxSlots(8)}
	c.Step(Reading{ThermalSevere: true})

	for i := 0; i < NominalTicksBeforeIncrease; i++ {
		if got := c.Step(Reading{CPU: 0.0, RunQueue: 0.0, DemandWaiting: true}); got != Floor {
			t.Fatalf("budget climbed to %d after only %d clean ticks, want it held at %d for %d",
				got, i+1, Floor, NominalTicksBeforeIncrease)
		}
	}
	if got := c.Step(Reading{CPU: 0.0, RunQueue: 0.0, DemandWaiting: true}); got <= Floor {
		t.Errorf("budget = %d after the hysteresis expired, want it climbing again", got)
	}
}

// TestTheBudgetSettlesRatherThanOscillating is clause 2's real assertion.
//
// A loop that merely stayed inside its bounds could flap between 1 and B_max forever and pass
// every other test here. The trace is therefore a STEADY one — a machine sitting at a
// constant load — and the question is whether the budget converges on an answer or keeps
// hunting for one.
//
// "Settles" is defined as: over the last quarter of the trace, the budget takes at most two
// distinct values and never moves by more than one slot per tick. Two values rather than one
// because AIMD by construction cannot sit still — additive increase means it must probe
// upward, and a loop that never probed would never find headroom that had come free. What it
// must NOT do is halve.
func TestTheBudgetSettlesRatherThanOscillating(t *testing.T) {
	cases := []struct {
		name string
		load tick
	}{
		// Comfortably inside the setpoint: the budget should climb to the ceiling and stay.
		{"idle", tick{cpu: 0.185, runQueue: 0.351, demand: true}},
		// Just under the band, so no cut, but with no headroom to climb on either.
		{"at the setpoint", tick{cpu: Setpoint, runQueue: 1.0, demand: true}},
		// Just under both thresholds — the most delicate place for a loop to sit.
		{"on the edge", tick{cpu: Setpoint + SetpointBand - 0.001, runQueue: RunQueueMax - 0.001, demand: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trace := make([]tick, 400)
			for i := range trace {
				trace[i] = tc.load
			}
			c := &Controller{Max: MaxSlots(8)}
			got := replay(c, trace)

			tail := got[len(got)*3/4:]
			seen := map[int]bool{}
			for _, b := range tail {
				seen[b] = true
			}
			if len(seen) > 2 {
				t.Errorf("the budget takes %d distinct values over the settled tail %v; "+
					"a loop that keeps hunting is a machine alternately starved and "+
					"over-subscribed", len(seen), tail[:min(20, len(tail))])
			}
			for i := 1; i < len(tail); i++ {
				if d := tail[i] - tail[i-1]; d < -1 {
					t.Errorf("the budget halved from %d to %d after settling; a steady load "+
						"must not produce a multiplicative decrease", tail[i-1], tail[i])
					break
				}
			}
		})
	}
}

// A sustained overload drives the budget to the floor and holds it there. The multiplicative
// half of AIMD is what makes this fast: from a ceiling of 6 it takes three ticks, not six.
func TestASustainedOverloadReachesTheFloorQuickly(t *testing.T) {
	c := &Controller{Max: MaxSlots(8)}
	replay(c, []tick{idle(), idle(), idle(), idle(), idle(), idle(), idle(), idle()})
	start := c.Budget()
	if start < 4 {
		t.Fatalf("the budget only reached %d on an idle machine; this test needs room to fall from", start)
	}

	ticks := 0
	for c.Budget() > Floor {
		c.Step(Reading{CPU: busy().cpu, RunQueue: busy().runQueue, DemandWaiting: true})
		ticks++
		if ticks > 20 {
			t.Fatalf("the budget was still %d after 20 overloaded ticks", c.Budget())
		}
	}
	// log2(start) halvings, give or take the floor.
	want := int(math.Ceil(math.Log2(float64(start)))) + 1
	if ticks > want {
		t.Errorf("took %d ticks to reach the floor from %d; multiplicative decrease should "+
			"need about %d", ticks, start, want)
	}
}

// The increase is gated on something actually asking. Without this the first fan-out after an
// idle period arrives with an unearned budget.
func TestAnIdleMachineWithNoDemandDoesNotClimb(t *testing.T) {
	c := &Controller{Max: MaxSlots(8)}
	// Drive it to the floor first, so there is somewhere to climb from.
	for i := 0; i < 10; i++ {
		c.Step(Reading{CPU: busy().cpu, RunQueue: busy().runQueue})
	}
	if c.Budget() != Floor {
		t.Fatalf("budget = %d, want the floor before this test starts", c.Budget())
	}

	for i := 0; i < 50; i++ {
		c.Step(Reading{CPU: 0.0, RunQueue: 0.0, DemandWaiting: false})
	}
	if got := c.Budget(); got != Floor {
		t.Errorf("budget climbed to %d with nothing waiting for a slot; the first fan-out "+
			"after an idle period would arrive with an unearned budget", got)
	}
}

// TestTheCeilingComesFromTheMachine: nothing tuned on one laptop ships as anyone else's
// default, so B_max is derived from the cores this process may actually use.
func TestTheCeilingComesFromTheMachine(t *testing.T) {
	cases := []struct {
		capacity float64
		want     int
	}{
		{8, 6},   // this machine: 8 cores / 1.2 per Medium build
		{2, 1},   // a --cpus=2 container
		{0.5, 1}, // half a core is a perfectly ordinary cgroup limit, and the floor holds
		{64, 53}, // a big host
		{0, 1},   // capacity not read yet
		{-1, 1},  // nonsense in, floor out
		{1.2, 1}, // exactly one slot
		{2.4, 2}, // exactly two
	}
	for _, tc := range cases {
		if got := MaxSlots(tc.capacity); got != tc.want {
			t.Errorf("MaxSlots(%v) = %d, want %d", tc.capacity, got, tc.want)
		}
	}
}

// The budget starts at the ceiling, not the floor. A daemon is started lazily BY a gate, so
// the moment it comes up is precisely the moment someone is trying to do something, and
// climbing one slot per tick from the floor would throttle an idle machine for no reason.
func TestTheBudgetStartsAtTheCeiling(t *testing.T) {
	c := &Controller{Max: 6}
	if got := c.Budget(); got != 6 {
		t.Errorf("budget = %d before any tick, want the ceiling of 6", got)
	}
	// And a controller that has never been told its ceiling is honest about it rather than
	// optimistic.
	var unset Controller
	if got := unset.Budget(); got != Floor {
		t.Errorf("budget = %d with no ceiling set, want the floor of %d", got, Floor)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// GOV-31: what one build actually does to the loop
// ---------------------------------------------------------------------------

// oneMediumBuild is one `go build -a ./...` running alone on a settled 8-core machine.
//
// MEASURED, and the measurement corrects the figure this story was filed on. GOV-15 reported
// that one build drives the run queue to ~2.9 per core on its own, and GOV-31 was filed to
// raise the threshold on that basis. Measured properly — one build, nothing else running, the
// machine settled below 0.8 first — it is 1.22 (p50, p90 and max all 1.22), with host CPU at
// p50 0.77. See docs/run-queue-sweep.md section 4. The 2.9 was read off a ten-way fan-out
// arm, not off one build.
//
// One build is Medium by internal/classify's table (`"go": Medium`), which is what makes it
// the reading clause 5 asks about.
func oneMediumBuild() tick { return tick{cpu: 0.77, runQueue: 1.22, demand: true} }

// TestOneMediumBuildsRunQueueDoesNotFloorTheBudget is GOV-31 clause 5's regression: the
// specific failure the story was filed for must not come back silently.
//
// It supplies ONLY the run-queue pressure of one Medium build, holding CPU below the cut, so
// that it tests the thing the clause names rather than passing or failing for a reason next
// door. With the threshold where it is, one build's 1.22 is 2% over 1.2 and does cut — which
// is the defect, in the direction GOV-31 claimed and roughly twenty times smaller than it
// estimated.
func TestOneMediumBuildsRunQueueDoesNotFloorTheBudget(t *testing.T) {
	c := &Controller{Max: MaxSlots(8)}
	b := oneMediumBuild()
	// CPU held under the cut so that the run queue is the only thing that can fire. The
	// measured p50 of 0.77 is itself under Setpoint+SetpointBand; see the sibling test for
	// what happens at the measured distribution rather than its median.
	for i := 0; i < 20; i++ {
		c.Step(Reading{CPU: 0.60, RunQueue: b.runQueue, DemandWaiting: true})
	}
	if got := c.Budget(); got <= Floor {
		t.Errorf("one Medium build's run-queue pressure (%.2f per core, measured) drove the "+
			"budget to %d against a ceiling of %d: a single build must not exhaust the "+
			"machine's whole build concurrency (GOV-31 clause 5, docs/run-queue-sweep.md)",
			b.runQueue, got, MaxSlots(8))
	}
}

// TestUnderBuildLoadTheSetpointBindsAndTheRunQueueDoesNot pins the finding that made GOV-31's
// sweep come back empty, so that a later session does not re-run it expecting a different
// answer.
//
// The loop cuts on `CPU > Setpoint+SetpointBand` OR `RunQueue > RunQueueMax`. Measured across
// a ten-way fan-out, host CPU sits at p50 1.00 and is above the cut on 72-99% of ticks
// (docs/run-queue-sweep.md sections 2 and 4), so the CPU disjunct decides and the run-queue
// threshold is shadowed. Five values of RunQueueMax from 1.2 to 6.0, measured twice under the
// same load, produced no difference the experiment could detect.
//
// The test states that as a property: at build-load CPU, the budget floors identically no
// matter what the run-queue threshold is.
func TestUnderBuildLoadTheSetpointBindsAndTheRunQueueDoesNot(t *testing.T) {
	// The measured fan-out reading: CPU pegged, run queue well under even a raised threshold.
	const fanOutCPU, fanOutRunQueue = 1.00, 2.5

	var floors []int
	for _, threshold := range []float64{1.2, 2.0, 3.0, 4.5, 6.0} {
		c := &Controller{Max: MaxSlots(8), RunQueueMax: threshold}
		for i := 0; i < 20; i++ {
			c.Step(Reading{CPU: fanOutCPU, RunQueue: fanOutRunQueue, DemandWaiting: true})
		}
		floors = append(floors, c.Budget())
	}
	for i, got := range floors {
		if got != Floor {
			t.Errorf("threshold %d of the sweep settled at %d, want the floor %d", i, got, Floor)
		}
	}

	// And the other half of the same finding: it cannot climb back either, because the
	// increase needs CPU below Setpoint-Headroom = 0.65, which never happens while a build
	// runs. Without this the reader could conclude the cut was transient.
	c := &Controller{Max: MaxSlots(8), RunQueueMax: 6.0}
	for i := 0; i < 20; i++ {
		c.Step(Reading{CPU: fanOutCPU, RunQueue: fanOutRunQueue, DemandWaiting: true})
	}
	for i := 0; i < 50; i++ {
		// Run queue now far below the threshold, CPU still pegged as it is throughout a build.
		c.Step(Reading{CPU: fanOutCPU, RunQueue: 0.1, DemandWaiting: true})
	}
	if got := c.Budget(); got != Floor {
		t.Errorf("budget recovered to %d with CPU still at %.2f; the increase requires CPU "+
			"below %.2f, so a build fan-out should pin it at the floor", got, fanOutCPU,
			Setpoint-Headroom)
	}
}
