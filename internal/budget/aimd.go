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
// WHAT THIS PARAGRAPH USED TO CONCLUDE WAS TRUE OF THE TRACE AND FALSE OF BUILD LOAD, and
// GOV-31 measured the difference. It said RunQueueMax was "doing more of the work than the
// setpoint is". Over the Phase 0 trace that holds — ordinary use is above this setpoint on
// 4.27% of samples and above the run-queue threshold on more. Under a build fan-out it is
// exactly backwards: host CPU sits at p50 1.00 and is over Setpoint+SetpointBand on 72-99% of
// ticks, so THIS constant decides every cut and the run-queue threshold decides none of them.
//
// The two regimes disagree because CPU is bimodal, and a build is the upper mode. Both
// statements are kept because the loop meets both kinds of load, and a reader tuning either
// constant needs to know which regime the number in front of them came from.
// docs/run-queue-sweep.md section 3 has the daemon logs that show the cut happening.
//
// THE PRACTICAL CONSEQUENCE IS NOT A NEW SETPOINT. It is that the budget cannot climb back
// while any build is running, because the increase branch requires CPU below
// Setpoint-Headroom = 0.65. On this machine that means a fan-out floors the budget and pins it
// there until the load drains. Whether that is correct behaviour or an over-control is a
// question about this constant and Headroom together, and it is not settled here.
const Setpoint = 0.75

// SetpointBand is how far above Setpoint utilisation must go before the budget is halved.
//
// Unchanged from the design. The band exists so that sitting exactly at the setpoint is a
// stable state rather than a cut on every other tick.
const SetpointBand = 0.05

// Headroom is how much slack there must be before the budget climbs.
//
// Unchanged from the design at 0.10. With the bimodal distribution above, this is satisfied
// for the great majority of ticks — the median tick has 0.565 of headroom — which is the
// correct behaviour for a machine that is genuinely idle most of the time.
const Headroom = 0.10

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

// Step advances the loop by one tick and returns the new budget.
//
// The order of the branches is the policy and must not be rearranged: a severe thermal state
// floors the budget whatever else is true, a cut beats a climb, and a climb happens only when
// something is actually waiting for the room.
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
	case r.CPU > Setpoint+SetpointBand || r.RunQueue > c.runQueueMax():
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
	case Setpoint-r.CPU > Headroom && r.RunQueue <= c.runQueueMax() && r.DemandWaiting:
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
