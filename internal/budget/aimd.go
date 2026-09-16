package budget

import "math"

// The control loop (design.md section 5.2, GOV-15 clauses 2 and 3).
//
// Additive increase, multiplicative decrease — borrowed from TCP congestion control and the
// right shape for the same reason: reclaim headroom cautiously, give it back immediately.
// The mechanism is inherited wholesale from the design; what GOV-15 changed is the quantity
// it controls. B is build concurrency in slots, not a count of agents (section 5.2's own
// note, and internal/budget/weight.go for the unit).
//
// EVERY CONSTANT BELOW IS MEASURED, and the measurement is recorded at the constant rather
// than in a commit message. The trace is the one Phase 0 collected: 312,515 host samples at
// 1 Hz over 2026-09-08 to 2026-09-14, read back with the same reader `governor trace` writes.
// Two of the numbers came out differently from what the design assumed, and both are stated
// as findings rather than quietly applied.

// Reading is one tick of the signals the loop acts on.
//
// It is a local struct rather than signals.Sample on purpose. The loop reads four numbers and
// a bool; taking the whole sample would let a later change make the controller depend on a
// field nobody reasoned about, and would make the replay test construct a host reading in
// order to assert a budget.
type Reading struct {
	// CPU is host utilisation in [0,1], already smoothed. The loop does NOT smooth it
	// itself: signals.EWMA owns that, so the window is set in one place.
	CPU float64
	// RunQueue is loadavg1 over usable cores — contention that CPU% cannot see, because
	// CPU% saturates at 1.0 and a run queue does not.
	RunQueue float64
	// ThermalSevere is `serious` or `critical`, NOT `fair`. See Controller.Step.
	ThermalSevere bool
	// DemandWaiting is whether anything is actually asking for a slot. Without it the first
	// fan-out after an idle period arrives with an unearned budget.
	DemandWaiting bool
}

// Setpoint is the host utilisation the loop aims to sit under.
//
// MEASURED, AND THE FINDING IS THAT IT BARELY MATTERS. The design said "tune setpoint against
// the Phase 0 traces, not by feel", which assumed the choice was sensitive. Over 312,515
// samples it is not, because host CPU on this machine is BIMODAL rather than continuous — p50
// 0.185, p90 0.425, then p95 0.728 and p99 1.000. The machine is either quiet or pegged, and
// almost nothing sits in between. The share of time above a candidate setpoint therefore
// hardly moves across the whole plausible range:
//
//	> 0.60   5.93% of samples
//	> 0.65   5.49%
//	> 0.70   5.15%
//	> 0.75   4.89%   <- kept
//	> 0.80   4.68%
//	> 0.85   4.46%
//
// 1.5 points of coverage across a 25-point range of setpoint. So the design's 0.75 is KEPT,
// and it is kept for a measured reason rather than retuned to a more precise-looking number
// that the data cannot support. Reporting a tuned value here — 0.71, say — would be inventing
// a precision the distribution does not have.
//
// THE LOOP NO LONGER READS THIS CONSTANT, AND GOV-32 IS WHY. Everything above is still true of
// the trace; what changed is that a utilisation setpoint turned out to be the wrong thing to
// control a BUILD budget with, and the evidence is three measurements deep.
//
// FIRST, IT DECIDED EVERYTHING UNDER THE LOAD THAT MATTERS. GOV-31 found that over the Phase 0
// trace ordinary use is above this setpoint on 4.27% of samples and above the run-queue
// threshold on more — but under a build fan-out it is exactly backwards: host CPU sits at p50
// 1.00 and is over Setpoint+SetpointBand on 72-99% of ticks, so this constant decided every
// cut and the run-queue threshold decided none of them. Five values of RunQueueMax from 1.2 to
// 6.0 produced no difference any experiment could detect, because none of them were consulted.
//
// SECOND, THE INCREASE BRANCH COULD NOT FIRE AT ALL. The climb required CPU below
// Setpoint-Headroom = 0.65. Counted rather than reasoned about (GOV-32 clause 1): 0% of an
// ungoverned ten-way fan-out's busy ticks, 4% of a governed arm's, and 0 of the replay trace's
// 471 ticks at CPU >= 0.99. B_max was unreachable whenever any real build was running.
//
// THIRD — AND THIS IS THE ONE THAT DECIDED IT — THE OPERATING POINT IT DROVE TO IS NOT A FULL
// MACHINE. At one build, where the old loop floored and stayed, this host measures host CPU
// 0.77 and run queue 1.22 per core. Saturation begins at two builds. So "the machine really is
// full and one at a time is right" is not a judgement call that could go either way; it is
// false on this host, which sat 23% idle with nine build requests waiting. Note where 0.77
// lands: between the climb at 0.65 and the cut at 0.80, which is to say the loop had CONVERGED
// ON ITS SETPOINT and was behaving exactly as an AIMD controller with a utilisation setpoint of
// 0.75 is specified to. The defect was never in the mechanism. It was that a utilisation
// setpoint of 0.75 asks for a machine three-quarters busy, and asking a build governor for that
// is asking it to leave a quarter of the machine unused while work is queued.
//
// WHY THE CONSTANT IS STILL HERE. tools/sweep reports `cpu_share_above_cut` against it, which
// is how a reader of a sweep result tells "CPU was pegged" from "CPU was quiet" at a glance,
// and the distribution above is the reason the loop can afford to ignore utilisation: a signal
// that is bimodal between 0.185 and 1.000 carries almost nothing in between to control on.
// Headroom, which only the removed climb condition read, was deleted rather than left to look
// live. docs/climb-condition.md is the whole argument; section 5 of it is the loop's new shape.
const Setpoint = 0.75

// SetpointBand is how far above Setpoint utilisation must go before it counts as pegged.
//
// Unchanged from the design at 0.05, and no longer a control input — see Setpoint. It survives
// as the width of the reporting threshold tools/sweep draws its CPU column against.
const SetpointBand = 0.05

// RunQueueMax is the run-queue-per-core above which the budget is halved.
//
// RAISED FROM 1.2 TO 2.0 BY GOV-31, and the sweep that raised it also found that this
// constant matters far less than section 5.2 assumed. docs/run-queue-sweep.md is the
// evidence; the short version is in three parts.
//
// FIRST, WHAT IT COSTS AND BUYS IN ORDINARY USE. Re-counted over the Phase 0 trace — 379,915
// host samples, p50 0.352, p90 0.813, p95 2.664, p99 24.584, max 49.620, materially the same
// distribution GOV-15 read off 312,515 of them — the share of ordinary samples a candidate
// threshold fires on is:
//
//	> 1.2   7.54%   <- was here
//	> 1.5   6.51%
//	> 2.0   5.62%   <- now here
//	> 2.5   5.13%
//	> 3.0   4.76%
//	> 6.0   3.75%
//
// against 4.27% for CPU over the setpoint. So 2.0 gives up 1.92 points of coverage, which is
// the same order as the 1.5 points that span the setpoint's ENTIRE plausible range and that
// GOV-15 judged too small to tune on. It keeps the run-queue test firing on more of the trace
// than the CPU test does, which is what makes it worth having at all.
//
// SECOND, WHY NOT ITS NEIGHBOURS. 1.5 clears one build's measured pressure by only 23%, and a
// developer machine with a slightly busier baseline re-creates the defect this story was filed
// for. From 2.5 upward the firing rate converges on the CPU test's own 4.27%, so the run-queue
// test stops carrying independent information in the one regime where it is currently doing
// the work; and 3.0 is above what THREE concurrent builds produce at p50 (2.55 measured), so
// three builds on an eight-core machine would never trip it.
//
// THIRD, AND MOST IMPORTANTLY, THIS CONSTANT IS NEARLY INERT UNDER BUILD LOAD — which is the
// regime the governor exists for, and the reason the sweep GOV-31 asked for came back empty.
// Five values from 1.2 to 6.0, measured twice under the same ten-way fan-out, produced no
// difference the experiment could detect: build CPU 205.2, 188.7, 194.2, 202.6 core-seconds
// against a run-to-run variance on a FIXED threshold of 30%. The cause is measured rather than
// argued. A fan-out pegs host CPU, so `CPU > Setpoint+SetpointBand` is true on 72-99% of ticks
// and decides the cut on its own, while the increase needs CPU below 0.65 and so cannot fire
// at all while a build runs. The run-queue test is shadowed on both sides.
//
// THE FIGURE THIS STORY WAS FILED ON WAS WRONG, and correcting it is why the change is small.
// GOV-15 reported one `go build -a` driving the run queue to ~2.9 per core on its own, making
// 1.2 "too tight by a factor of two to three". Measured on a settled machine it is 1.22 — over
// 1.2, but by 2%. The 2.9 was read off a ten-way fan-out arm. Worse, `loadavg1` is a
// one-minute decaying average and one build runs eight seconds, so most of that 1.22 is the
// machine's baseline rather than the build: the signal cannot see a short build at all. Any
// threshold here is a statement about SUSTAINED load, which is a property of the instrument
// and not something a different number fixes.
const RunQueueMax = 2.0

// RunQueueBand is how far BELOW RunQueueMax the run queue must fall before the budget climbs.
//
// IT IS THE DEAD BAND THAT MAKES THE LOOP SETTLE INSTEAD OF HUNTING, and it is the exact
// analogue of SetpointBand, which has done the same job for the CPU test since the design was
// written: without a gap between the level that cuts and the level that climbs, the operating
// point is a value the loop cuts at and then immediately climbs back to.
//
// WHY IT IS NEEDED NOW AND WAS NOT BEFORE. Until GOV-32 the climb was gated on CPU and the cut
// on CPU or the run queue, so the two tests were never the same test and no gap was required
// between them. GOV-32 moved the climb onto the run queue — the only signal that still carries
// information when the machine is full — and the moment both directions read one signal, the
// band stops being optional. docs/climb-condition.md section 5 has the arithmetic.
//
// THE VALUE IS CHOSEN AGAINST THE MEASURED CONCURRENCY CURVE, not picked for roundness. On
// this host the run queue produced by N concurrent builds is 1.22 at one, 1.53 at two and 2.55
// at three (docs/run-queue-sweep.md section 4). A band of 0.5 puts the climb threshold at 1.5,
// which sits just below two builds' 1.53 and well above one build's 1.22 — so the loop climbs
// off the floor and then holds at two, which is where the same curve puts this machine's
// throughput optimum. A narrower band of 0.25 would climb at 1.75, above two builds' 1.53, so
// the loop would climb to three, find 2.55 over the cut, and halve back to one: the hunt the
// band exists to prevent. A wider band of 1.0 would climb at 1.0, below even one build's 1.22,
// and the budget could never leave the floor at all — the previous defect in a new signal.
//
// THE GAP IS NARROW AND THE SIGNAL IS NOISY, which is stated here rather than discovered
// later. 1.53 and 1.5 differ by 2%, and loadavg1 is a one-minute decaying average, so the
// operating point is not a fixed point so much as a region the loop hunts within. The replay
// test bounds that hunting at 10% of ticks; what it does in practice is in the same document.
const RunQueueBand = 0.5

// NominalTicksBeforeIncrease is the hysteresis on the way back up from a thermal cut
// (design.md section 5.3): require clean ticks before the additive increase resumes, so the
// budget does not oscillate against the platform's own governor.
const NominalTicksBeforeIncrease = 3

// MaxSlots is the most build concurrency a host should ever allow, in slots.
//
// DERIVED FROM THE MACHINE, NOT A CONSTANT — the project rule that defaults come from the
// host and nothing tuned on one laptop ships as anyone else's default. One slot is one Medium
// build at 1.2 cores of expected peak, so the ceiling is however many of those fit in the
// cores this process may actually use.
//
// capacity is signals.Sample.CPUCapacity, which is the cgroup quota where one is set and the
// host's core count otherwise. That distinction matters here more than anywhere: a container
// limited to two cores on a 64-core host would otherwise be handed a ceiling of 53.
//
// The design's old expression was min(2 * performance_cores, 12), which counted AGENTS. It
// cannot be carried over: two per performance core was a statement about how many mostly-idle
// I/O-bound things fit beside each other, and a build is neither mostly idle nor I/O-bound.
// On this 8-core machine the two disagree by a factor of five in cores — 8 slots would be 9.6
// cores of expected peak on a machine that has 8.
func MaxSlots(capacity float64) int {
	n := int(math.Floor(capacity / SlotCores))
	if n < Floor {
		return Floor
	}
	return n
}

// Controller is the AIMD loop over build slots. It is not safe for concurrent use; the daemon
// steps it from its own sampling goroutine and reads the result under the daemon's lock.
type Controller struct {
	// Max is the ceiling, from MaxSlots. Zero means Floor, which is the honest answer for a
	// host whose capacity has not been read yet.
	Max int

	// RunQueueMax overrides the package constant of the same name. Zero means use the
	// constant, which is what every caller outside the sweep harness does.
	//
	// IT EXISTS SO THE THRESHOLD CAN BE SWEPT, and for no other reason. GOV-31 had to
	// measure four values of it under the same load, which is impossible while it is a
	// compile-time constant; tools/sweep sets it through the daemon's GOVERNOR_RUNQUEUE_MAX
	// environment variable. It is deliberately NOT part of internal/config: config is the
	// surface `governor calibrate` writes, and putting a knob there asserts that the right
	// value is host-dependent, which is a claim the sweep has to earn. So it stays an
	// experiment-only escape hatch until a measurement says otherwise; see
	// docs/run-queue-sweep.md for what the sweep concluded about host dependence.
	RunQueueMax float64

	b int
	// clean counts consecutive ticks with no severe thermal state, for the hysteresis.
	clean int
	// started is false until the first Step, so that B begins at Max rather than at zero.
	started bool
}

// Budget is the current budget in slots.
func (c *Controller) Budget() int {
	if !c.started {
		return c.start()
	}
	return c.b
}

// start is the budget before any tick has landed.
//
// It begins at the CEILING rather than at the floor, and the direction is deliberate. AIMD
// climbs one slot per tick, so starting at the floor would leave a freshly started daemon
// throttling an idle machine for several seconds for no reason — and the daemon is started
// lazily by a gate, which means that is exactly the moment someone is trying to do something.
// Starting high and cutting fast is the same asymmetry the whole loop is built on: the
// multiplicative decrease reaches the floor in a few ticks if the machine really is busy.
func (c *Controller) start() int {
	if c.Max < Floor {
		return Floor
	}
	return c.Max
}

// runQueueMax is the threshold this controller cuts at: the field if it was set, the
// package constant otherwise.
func (c *Controller) runQueueMax() float64 {
	if c.RunQueueMax > 0 {
		return c.RunQueueMax
	}
	return RunQueueMax
}

// climbRunQueue is the level this controller climbs at, one band below the level it cuts at.
//
// IT IS DERIVED FROM runQueueMax RATHER THAN BEING A SECOND KNOB, so that sweeping the cut
// threshold moves the climb threshold with it and the dead band between them stays the same
// width. A sweep that moved one and not the other would be measuring two changes at once,
// which is how GOV-31's first pass produced an effect that belonged to the running order.
//
// It cannot go below the floor of the signal: a climb threshold at or under zero would never
// be satisfied and the budget could not leave Floor, which is the defect this story exists to
// remove, reintroduced by arithmetic.
func (c *Controller) climbRunQueue() float64 {
	at := c.runQueueMax() - RunQueueBand
	if at <= 0 {
		at = c.runQueueMax() / 2
	}
	return at
}

// Cutting and Climbing are the loop's two SIGNAL conditions, named and exported so that a
// measurement of how often each one fires evaluates the same expression the controller
// branches on.
//
// THEY EXIST BECAUSE GOV-32 NEEDED A NUMBER THAT COULD NOT BE READ OFF THE CODE. Its first
// clause asks what share of a build fan-out's ticks satisfy the increase condition, and the
// only honest way to answer is to run the loop's own test over measured readings. Re-typing
// `Setpoint - cpu > Headroom` into tools/sweep would measure a copy, and a copy is precisely
// what drifts: GOV-31's sweep reported `cpu_share_above_cut` from a re-typed cut condition
// evaluated against RAW host CPU, while the loop acts on a 5 s EWMA (section 5.1). Under a
// sustained fan-out both read 1.00 so that discrepancy changed no conclusion, but nobody
// could see it, because the two expressions lived in different files.
//
// NEITHER TAKES THE CONTROLLER'S STATE, and the omission is the point. The thermal hysteresis
// counter and DemandWaiting are facts about the loop's history and about the queue, not about
// the machine. Folding them in would answer "did the loop climb", where what both the sweep
// and section 5.2 need to know is "did the machine let it".
//
// CUT ON SATURATION, NOT ON UTILISATION. This condition used to read
// `r.CPU > Setpoint+SetpointBand || r.RunQueue > runQueueMax`, and GOV-32 removed the CPU
// disjunct. The reasoning is at Setpoint, where the constant it belonged to is; in one line,
// a machine at CPU 1.00 with a shallow run queue is a build machine WORKING, and a loop that
// reads that as trouble has no operating point above the floor while any build runs.
func Cutting(r Reading, runQueueMax float64) bool {
	return r.RunQueue > runQueueMax
}

// Climbing is the additive-increase condition on the signals alone. The caller adds
// DemandWaiting; see Cutting for why it is not folded in here.
//
// CLIMB ON THE SIGNAL THAT STILL MOVES WHEN THE MACHINE IS FULL. This used to read
// `Setpoint-r.CPU > Headroom && r.RunQueue <= runQueueMax`, whose CPU half requires
// utilisation below Setpoint-Headroom = 0.65 and is therefore unsatisfiable under any load
// this governor exists for. Measured: 0 of the trace's 471 saturated ticks and 0 of a
// ten-way fan-out's busy ticks (docs/climb-condition.md). climbAt is RunQueueMax-RunQueueBand,
// so the level that climbs sits strictly below the level that cuts and the loop has somewhere
// to stand between them.
func Climbing(r Reading, climbAt float64) bool {
	return r.RunQueue <= climbAt
}

// Step advances the loop by one tick and returns the new budget.
//
// The order of the branches is the policy and must not be rearranged: a severe thermal state
// floors the budget whatever else is true, a cut beats a climb, and a climb happens only when
// something is actually waiting for the room.
//
// THERE ARE FOUR OUTCOMES, NOT THREE, AND THE FOURTH IS THE ONE GOV-32 ADDED. Falling through
// every case is a HOLD, and it is where the loop is supposed to spend its time under build
// load: the run queue is above the climb threshold and at or below the cut threshold, meaning
// the machine is busy and coping. Before GOV-32 that state did not exist for a saturated
// machine — utilisation over the setpoint cut on its own, so "busy" and "in trouble" were the
// same reading and the budget had nowhere to stand between the floor and a cut. RunQueueBand
// is the width of that resting place.
func (c *Controller) Step(r Reading) int {
	if !c.started {
		c.b = c.start()
		c.started = true
	}
	max := c.Max
	if max < Floor {
		max = Floor
	}

	switch {
	case r.ThermalSevere:
		// THERMAL IS A BACKSTOP AND IT IS NOT LOAD-BEARING ON THIS CLASS OF MACHINE, which
		// GOV-15 clause 3 required be decided explicitly rather than inherited.
		//
		// The evidence: over 312,515 samples thermal read `nominal` 97.15% of the time and
		// `fair` 2.85%, and `serious` or `critical` NEVER — not once in 101 hours including
		// the busiest seconds on the machine. GOV-13 separately ran ten minutes of saturating
		// 8-core load on this chassis with loadavg peaking at 66 and thermalState never left
		// `nominal`, corroborated by an independent Swift read, the raw notify key, and
		// `pmset -g therm` showing no warning level.
		//
		// THE ONE CHANGE FROM THE DESIGN: the cut fires at `serious` and above, NOT at
		// `fair`. signals.Thermal.Elevated() is true for `fair` and flooring the budget on it
		// would throttle this machine to one slot for 2.85% of its life — nearly three hours
		// in the measured window — on a reading that came with no corroborating evidence of
		// throttling at all. That is a real throughput cost for no measured benefit, and it
		// would fire hardest during long builds, which is when it is most expensive.
		//
		// So the trigger is kept but narrowed to the states that have never yet been
		// observed. It is honest to say that this branch is, on this hardware, dead code
		// carried for a different chassis — a fanless Air is not a Mac Pro under sustained
		// load, and a governor that removed the check would have nothing to say on hardware
		// that does throttle.
		c.b = Floor
		c.clean = 0
	case Cutting(r, c.runQueueMax()):
		// MULTIPLICATIVE DECREASE, on the run queue alone. It halves rather than stepping
		// down because the cost of being over is bounded by how fast you can get back, and
		// the climb is one slot per second.
		c.clean++
		c.b = c.b / 2
		if c.b < Floor {
			c.b = Floor
		}
	case c.clean < NominalTicksBeforeIncrease:
		// Coming back from a thermal cut. Hold at the floor until the machine has been clean
		// for three consecutive ticks, so the budget does not oscillate against the
		// platform's own governor (design.md section 5.3).
		c.clean++
	case Climbing(r, c.climbRunQueue()) && r.DemandWaiting:
		// DemandWaiting gates the increase: do not climb when nobody is asking, or the first
		// fan-out after an idle period arrives with an unearned budget.
		if c.b < max {
			c.b++
		}
	}

	if c.b > max {
		// The ceiling can move under the loop — `governor calibrate` rewrites it, and a
		// cgroup quota can change — so it is enforced on every tick rather than only on the
		// increase.
		c.b = max
	}
	return c.b
}
