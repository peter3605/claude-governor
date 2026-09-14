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
// What this does mean is that RunQueueMax below is doing more of the work than the setpoint
// is, which is the opposite of what section 5.2 implied.
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
// MEASURED, AND IT IS THE SIGNAL THAT ACTUALLY DISCRIMINATES. Over the same 312,515 samples
// the run queue is p50 0.351, p90 0.820, p95 2.946, p99 27.103, max 49.620. Two things follow.
//
// First, 1.2 sits just above the 90th percentile, so it fires on genuinely loaded moments and
// not on ordinary work — 7.68% of samples, against 4.89% for CPU over the setpoint. It is
// catching contention that CPU% misses, which is exactly the job section 5.1 gives it.
//
// Second, and this is why the threshold is not raised: the tail is enormous. A run queue of
// 27 per core at p99, and 49.6 at maximum, is a machine with hundreds of runnable threads on
// eight cores. CPU% cannot express that at all — it saturates at 1.0 and stops carrying
// information — so at the extreme end the run queue is the ONLY signal that still moves.
// Raising this threshold would blind the loop in precisely the state it exists for.
//
// 1.2 is the design's number and the measurement endorses it, which is worth recording as
// plainly as a correction would be.
const RunQueueMax = 1.2

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
	case r.CPU > Setpoint+SetpointBand || r.RunQueue > RunQueueMax:
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
	case Setpoint-r.CPU > Headroom && r.RunQueue <= RunQueueMax && r.DemandWaiting:
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
