package budget

// The budget's unit, and the one decision GOV-15's clause 1 asked to be made explicitly
// rather than left to a default.
//
// THE QUESTION. Phase 0 moved the controlled variable from agent count to build concurrency,
// which left an open choice: do agent spawns stop taking a lease altogether, or do they take
// one weighted at their measured cost? Both are defensible and leaving both in was not.
//
// THE DECISION: agents keep taking a lease, weighted. Dropping them would discard a measured
// cost — +0.098 cores at the median marginal, which across a wide fan-out is real — and it
// would leave the product with no control over subagent concurrency at all. The reason agent
// COUNT was the wrong controller is not that agents are free; it is that their cost is small
// and near-constant while build cost is zero almost always and occasionally enormous
// (median 0.000, p99 3.318, max 7.822). A budget that can express both sizes governs both.
// A budget that counts them alike governs neither, which is what v1 did.
//
// WHAT THAT FORCES. Once an agent and a Heavy build are both in the pool, the pool cannot be
// a count — 0.098 cores and 4.5 cores are not both "one slot", and a table that said so
// would be repeating the error the pivot exists to correct. So the budget stays an INTEGER
// number of slots, which is what design.md sections 5.2 and 6 have always called it and what
// `governor status` renders, and the LEASE carries a fractional weight against it. One slot
// is one Medium build. Nothing outside this file needs to know the conversion.
//
// WHY NOT DENOMINATE THE BUDGET IN CORES DIRECTLY. It was the first design and it is worse.
// Invariant 4 floors the budget at 1, and a floor of one core cannot admit a Heavy build at
// 4.5, so the floor would have had to become "the largest class" — a constant that changes
// whenever the class table does. Keeping the budget in slots keeps invariant 4 readable and
// puts the awkward case in one place: Table.Acquire's work-conserving rule, which admits a
// request that does not fit an empty table rather than deadlocking on it.

// SlotCores is how many cores one slot of budget represents.
//
// It is the measured p90 peak child cores of a Medium build — an ordinary compile or test
// run — from docs/bash-corpus.md. Choosing the Medium class as the unit rather than a round
// number means the common case weighs exactly 1 and the arithmetic below stays legible in a
// status line: "3/6 slots" means about three ordinary builds' worth of load.
const SlotCores = 1.2

// AgentCores is what one live subagent costs, and it is a MARGINAL at population level, not
// a per-agent measurement.
//
// docs/phase-0-findings.md is explicit that a per-agent cost cannot be separated on this
// instrument: the sampler is host-wide and the machine never ran fewer than three concurrent
// sessions. This is claude-tree root cores while at least one reconstructable agent lifetime
// was open, against root cores while none was — 2,367 agent-live samples against 250,193
// baseline — taken at the median, which is the figure that population supports.
//
// The p99 of that marginal is +0.214 and the max is NEGATIVE (-0.246): the busiest root
// seconds had no agent live at all. So unlike the build classes, which are p90s because the
// loop is protecting against saturation, this is deliberately NOT a tail figure. There is no
// agent tail to protect against; that is the finding.
const AgentCores = 0.098

// SlotsFor converts expected peak cores into slots of budget.
//
// It never returns less than a small positive weight: a lease that weighed zero would be
// admitted unconditionally and forever, which is not governance.
func SlotsFor(cores float64) float64 {
	w := cores / SlotCores
	if w < minWeight {
		return minWeight
	}
	return w
}

// AgentWeight is what one subagent spawn holds. Roughly 0.08 slots, so about twelve
// concurrent agents weigh the same as one ordinary build — which is the ratio Phase 0
// measured and the ratio a fixed agent budget of 4 was unable to express.
var AgentWeight = SlotsFor(AgentCores)

// minWeight is the floor on any single lease's weight.
//
// It exists so that a future cost class measured at or near zero cannot produce a lease that
// consumes no budget. Small enough to be well under an agent, large enough that a thousand
// of them still add up to something a budget would notice.
const minWeight = 0.01
