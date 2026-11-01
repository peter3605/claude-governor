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

	// IN THE HOLD BAND THE RUN QUEUE STILL HOLDS IT. Above the climb level and at or under
	// the cut level the loop neither cuts nor climbs, and the run queue really is what keeps
	// the budget where it is, so the field keeps naming it.
	hold := (RunQueueMax + (RunQueueMax - RunQueueBand)) / 2
	c.Step(Reading{RunQueue: hold, DemandWaiting: true})
	if got := c.Cut(); got != "run queue" {
		t.Errorf("Cut() = %q in the hold band, want \"run queue\": it is still what holds the budget", got)
	}

	// THE FIELD NAMES WHAT HOLDS THE BUDGET NOW, NOT WHAT LAST CUT IT (GOV-46 clause 3). The
	// run queue has recovered; the budget is still below the ceiling, climbing back. Until
	// GOV-46 this asserted the opposite, and on 2026-11-01 that printed `cut run queue
	// (ceiling 4)` at cpu 0.42 on an idle machine minutes after the run queue had recovered.
	c.Step(Reading{RunQueue: 0, DemandWaiting: true})
	if c.Budget() >= 6 {
		t.Fatalf("budget = %d, want it still climbing back; this assertion needs headroom", c.Budget())
	}
	if got := c.Cut(); got != "" {
		t.Errorf("Cut() = %q against a recovered run queue, want \"\": the run queue is not "+
			"holding the budget any more", got)
	}

	// Back at the ceiling, there is still nothing to explain.
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

// The same rule with nobody asking: a recovered signal on an in-use machine whose budget nobody
// wants raised is not a cut either. The budget is unused, not held.
func TestARecoveredSignalIsNotReportedAsACutWithoutDemand(t *testing.T) {
	c := &Controller{Max: 6}
	c.Step(Reading{RunQueue: RunQueueMax * 4})
	for i := 0; i < NominalTicksBeforeIncrease+1; i++ {
		c.Step(Reading{RunQueue: 0, InUse: true})
	}
	if c.Budget() >= 6 {
		t.Fatalf("budget = %d, want it still below the ceiling for this assertion", c.Budget())
	}
	if got := c.Cut(); got != "" {
		t.Errorf("Cut() = %q with the run queue recovered, want \"\"", got)
	}
}

// Thermal's hysteresis holds the floor on purpose, so thermal is what holds it until the
// hysteresis ends — and then it is not.
func TestAThermalCutIsNamedUntilItsHysteresisEnds(t *testing.T) {
	c := &Controller{Max: 6}
	c.Step(Reading{ThermalSevere: true})
	for i := 0; i < NominalTicksBeforeIncrease; i++ {
		c.Step(Reading{RunQueue: 0, DemandWaiting: true})
		if got := c.Cut(); got != "thermal" {
			t.Fatalf("Cut() = %q during the thermal hysteresis (tick %d), want \"thermal\"", got, i)
		}
	}
	c.Step(Reading{RunQueue: 0, DemandWaiting: true})
	if got := c.Cut(); got != "" {
		t.Errorf("Cut() = %q after the hysteresis ended, want \"\"", got)
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
