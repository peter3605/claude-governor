package budget

import "testing"

// GOV-34: the controller says WHAT took the budget below its ceiling, so that a refusal, a
// status line and doctor can all quote one answer instead of each deriving their own.
//
// The derivation is the thing being prevented. GOV-32's own comment on Cutting and Climbing
// records what re-typing a loop condition elsewhere costs: tools/sweep evaluated a copy of the
// cut test against raw host CPU while the loop acted on a 5 s EWMA, and nobody could see the
// discrepancy because the two expressions lived in different files.
func TestTheControllerNamesWhatCutTheBudget(t *testing.T) {
	c := &Controller{Max: 6}

	// Nothing has been stepped: the budget starts at the ceiling, so nothing is cutting it.
	if got := c.Cut(); got != "" {
		t.Errorf("Cut() = %q before the first tick, want \"\": a budget at its ceiling is uncut", got)
	}

	// A quiet machine holds at the ceiling and still reports no cut.
	c.Step(Reading{RunQueue: 0})
	if got := c.Cut(); got != "" {
		t.Errorf("Cut() = %q on an idle machine at its ceiling, want \"\"", got)
	}

	// A deep run queue halves the budget, and the run queue is what did it.
	c.Step(Reading{RunQueue: RunQueueMax * 4})
	if c.Budget() >= 6 {
		t.Fatalf("budget = %d after a run-queue spike, want it cut below 6", c.Budget())
	}
	if got := c.Cut(); got != "run queue" {
		t.Errorf("Cut() = %q, want \"run queue\"", got)
	}

	// THE CAUSE OUTLIVES THE SIGNAL, DELIBERATELY. The run queue has recovered and the
	// budget is climbing back one slot per tick, but it is still below the ceiling and the
	// run queue is still why. Clearing it the moment the signal recovered would leave the
	// most common state — a machine partway back up — with a cut budget and nothing to
	// explain it, which is the state the 2026-09-21 report was written from.
	c.Step(Reading{RunQueue: 0, DemandWaiting: true})
	if c.Budget() >= 6 {
		t.Fatalf("budget = %d, want it still climbing back; this assertion needs headroom", c.Budget())
	}
	if got := c.Cut(); got != "run queue" {
		t.Errorf("Cut() = %q while still below the ceiling, want it to keep naming the run queue", got)
	}

	// Back at the ceiling, there is nothing left to explain.
	for i := 0; i < 10 && c.Budget() < 6; i++ {
		c.Step(Reading{RunQueue: 0, DemandWaiting: true})
	}
	if c.Budget() != 6 {
		t.Fatalf("budget = %d, want it back at the ceiling of 6", c.Budget())
	}
	if got := c.Cut(); got != "" {
		t.Errorf("Cut() = %q at the ceiling, want \"\": nothing is holding it down", got)
	}
}

// Thermal floors the budget whatever else is true, and says so in its own name rather than
// the run queue's. A person told "cut by run queue" on a throttling laptop goes looking for
// load that is not there.
func TestAThermalCutIsNamedAsThermal(t *testing.T) {
	c := &Controller{Max: 6}
	c.Step(Reading{ThermalSevere: true})

	if c.Budget() != Floor {
		t.Fatalf("budget = %d after a severe thermal reading, want the floor %d", c.Budget(), Floor)
	}
	if got := c.Cut(); got != "thermal" {
		t.Errorf("Cut() = %q, want \"thermal\"", got)
	}
}

// A host whose ceiling is the floor has no room to be cut, and must not report one.
func TestAFlooredCeilingReportsNoCut(t *testing.T) {
	c := &Controller{Max: Floor}
	c.Step(Reading{RunQueue: RunQueueMax * 4})

	if got := c.Cut(); got != "" {
		t.Errorf("Cut() = %q on a host whose ceiling IS the floor, want \"\": "+
			"the budget is not below anything", got)
	}
}
