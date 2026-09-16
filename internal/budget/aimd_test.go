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
	// The build's OWN measured CPU is used here, not a value held under the cut. Until GOV-32
	// this test had to pass CPU 0.60 so that the run queue was the only thing that could
	// fire, because the real reading — 0.77 — sat close enough to the cut that the test would
	// have been measuring the CPU disjunct instead of the threshold it names. The loop no
	// longer cuts on utilisation, so the measurement can be used as measured.
	for i := 0; i < 20; i++ {
		c.Step(Reading{CPU: b.cpu, RunQueue: b.runQueue, DemandWaiting: true})
	}
	if got := c.Budget(); got <= Floor {
		t.Errorf("one Medium build's run-queue pressure (%.2f per core, measured) drove the "+
			"budget to %d against a ceiling of %d: a single build must not exhaust the "+
			"machine's whole build concurrency (GOV-31 clause 5, docs/run-queue-sweep.md)",
			b.runQueue, got, MaxSlots(8))
	}
}

// TestUnderBuildLoadTheRunQueueDecidesAndTheBudgetCanLeaveTheFloor replaces
// TestUnderBuildLoadTheSetpointBindsAndTheRunQueueDoesNot, which pinned the behaviour GOV-32
// removed.
//
// WHAT THE OLD TEST SAID, because deleting it silently would erase the finding it recorded:
// the loop cut on `CPU > Setpoint+SetpointBand || RunQueue > RunQueueMax`, host CPU under a
// fan-out sits at p50 1.00, so the CPU disjunct decided every cut and five values of
// RunQueueMax from 1.2 to 6.0 produced no difference any experiment could detect. That was
// true, it was GOV-31's real finding, and it is now false BY CONSTRUCTION rather than by
// retuning: the CPU disjunct is gone. docs/climb-condition.md section 3 is why.
//
// The property this asserts is the mirror image: at build-load CPU, the run-queue threshold is
// the only thing that decides, so a different threshold must give a different answer.
func TestUnderBuildLoadTheRunQueueDecidesAndTheBudgetCanLeaveTheFloor(t *testing.T) {
	// The measured fan-out reading: CPU pegged throughout, which is now a fact about the
	// machine that the loop does not act on.
	const fanOutCPU = 1.00

	// Three builds' measured run queue (2.55, docs/run-queue-sweep.md section 4) is over the
	// shipped threshold and under a raised one. The old loop floored at every threshold here;
	// the new one must not, or the threshold is still shadowed by something.
	const threeBuilds = 2.55
	settled := map[float64]int{}
	for _, threshold := range []float64{2.0, 6.0} {
		c := &Controller{Max: MaxSlots(8), RunQueueMax: threshold}
		for i := 0; i < 40; i++ {
			c.Step(Reading{CPU: fanOutCPU, RunQueue: threeBuilds, DemandWaiting: true})
		}
		settled[threshold] = c.Budget()
	}
	if settled[2.0] != Floor {
		t.Errorf("at a threshold of 2.0 a run queue of %.2f settled at %d, want the floor %d: "+
			"%.2f is over the threshold and must still cut", threeBuilds, settled[2.0], Floor, threeBuilds)
	}
	if settled[6.0] <= Floor {
		t.Errorf("at a threshold of 6.0 a run queue of %.2f settled at %d; it is well under "+
			"that threshold, so the budget must be able to sit above the floor. A floor here "+
			"means something other than the run queue is still deciding", threeBuilds, settled[6.0])
	}

	// And the half the story is named for: with CPU pegged for the whole run, the budget must
	// climb back once the queue drains. Under the old loop this was the assertion that it
	// could NOT.
	c := &Controller{Max: MaxSlots(8)}
	for i := 0; i < 20; i++ {
		c.Step(Reading{CPU: fanOutCPU, RunQueue: 10.0, DemandWaiting: true})
	}
	if got := c.Budget(); got != Floor {
		t.Fatalf("a run queue of 10.0 per core did not reach the floor (got %d); the rest of "+
			"this test cannot tell a recovery from a budget that never cut", got)
	}
	for i := 0; i < 50; i++ {
		// The queue has drained. CPU is still pegged, as it is for as long as any build runs.
		c.Step(Reading{CPU: fanOutCPU, RunQueue: 0.1, DemandWaiting: true})
	}
	if got := c.Budget(); got != MaxSlots(8) {
		t.Errorf("with the queue drained to 0.1 per core the budget recovered only to %d of "+
			"%d, with CPU at %.2f throughout; B_max must be reachable while builds are "+
			"running, which is the defect GOV-31 left open", got, MaxSlots(8), fanOutCPU)
	}
}

// TestTheLoopSettlesWhereTheMachineSaturates closes the loop against the MEASURED response of
// this host, rather than asserting against a reading chosen by hand.
//
// IT IS A MODEL AND IT IS LABELLED ONE. `runQueueFor` is the concurrency curve of
// docs/run-queue-sweep.md section 4 — real measurements of what N concurrent builds do to the
// run queue on this machine — interpolated between its points. Feeding the loop its own
// consequences is the only way a unit test can ask "where does this settle", and the honest
// caveat is that the curve was measured at steady state while `loadavg1` takes a minute to
// get there. The fan-out arms in docs/climb-condition.md section 4 are the measurement; this
// is the mechanism that explains their shape.
//
// WHAT IT PROTECTS. The operating point is set by where the curve crosses the threshold, and
// both the curve and the threshold can move. If a later change puts the settling point back at
// the floor, that is the GOV-32 defect returning; if it puts it at the ceiling, the loop has
// stopped governing. Either way this fails and says which.
func TestTheLoopSettlesWhereTheMachineSaturates(t *testing.T) {
	c := &Controller{Max: MaxSlots(8)}
	var last []int
	for i := 0; i < 200; i++ {
		b := c.Budget()
		// CPU pegged from two builds onward, and 0.77 at one: the measured curve.
		cpu := 1.00
		if b <= 1 {
			cpu = 0.77
		}
		c.Step(Reading{CPU: cpu, RunQueue: runQueueFor(b), DemandWaiting: true})
		if i >= 180 {
			last = append(last, c.Budget())
		}
	}

	lo, hi := last[0], last[0]
	for _, b := range last {
		if b < lo {
			lo = b
		}
		if b > hi {
			hi = b
		}
	}
	if lo <= Floor {
		t.Errorf("driven by this machine's own measured response the budget still visits the "+
			"floor (range %d..%d over the last 20 ticks). At one build this host reads CPU "+
			"0.77 and run queue 1.22 per core — not full and not oversubscribed — so a loop "+
			"that settles there is over-controlling, which is what GOV-32 fixed", lo, hi)
	}
	if hi >= MaxSlots(8) {
		t.Errorf("the budget reached the ceiling %d (range %d..%d) against a run queue of "+
			"%.2f per core at that concurrency; the loop is not reacting to saturation",
			MaxSlots(8), lo, hi, runQueueFor(MaxSlots(8)))
	}
	t.Logf("closed against the measured curve, the budget settles in %d..%d slots "+
		"(run queue %.2f..%.2f per core)", lo, hi, runQueueFor(lo), runQueueFor(hi))
}

// runQueueFor is this host's measured run-queue-per-core at N concurrent builds, interpolated.
//
// The points are docs/run-queue-sweep.md section 4's concurrency curve, measured ungoverned on
// a settled machine, one level at a time. Above the last point it extrapolates from the final
// segment, which is steep on purpose: that is where the curve actually goes.
func runQueueFor(builds int) float64 {
	curve := []struct{ n, rq float64 }{
		{1, 1.22}, {2, 1.53}, {3, 2.55}, {4, 2.85}, {6, 4.83}, {8, 24.98},
	}
	n := float64(builds)
	if n <= curve[0].n {
		return curve[0].rq
	}
	for i := 1; i < len(curve); i++ {
		if n <= curve[i].n {
			lo, hi := curve[i-1], curve[i]
			f := (n - lo.n) / (hi.n - lo.n)
			return lo.rq + f*(hi.rq-lo.rq)
		}
	}
	return curve[len(curve)-1].rq
}
