// Package lease holds the lease table: one entry per subagent slot, tentative until the
// spawn is confirmed, reclaimed by TTL or by the death of its owner.
//
// The seam (design.md section 12.1) is Liveness. v1 answers "is the holder still alive?"
// from the owning PID and its start time, which is correct and free on one machine. A
// tier-two broker cannot see another host's processes and would answer from a heartbeat
// instead, behind this same interface.
package lease

import "time"

// State is where a lease is in the lifecycle of section 4.3.
type State int

const (
	// Tentative is a lease granted at PreToolUse, before the spawn has happened. It may
	// never be confirmed: the user can decline the permission prompt or interrupt the turn.
	Tentative State = iota
	// Confirmed is a lease bound to a live agent at SubagentStart.
	Confirmed
)

// Kind is what a lease was taken for, and it decides the whole of its lifecycle.
//
// The two are not variations on one thing. An Agent lease is tentative until SubagentStart
// confirms it and is released by SubagentStop, correlated per-session FIFO because
// SubagentStart carries no key. A Build lease is born confirmed — nothing announces that a
// command started — is keyed exactly by tool_use_id, and is released by a PostToolUse that
// may never arrive (design.md section 5.6, hook-surface.md section 19).
type Kind int

const (
	// Agent is a subagent spawn: the Agent, Task or SendMessage path.
	Agent Kind = iota
	// Build is a build-shaped Bash command, as decided gate-side by internal/classify.
	Build
)

func (k Kind) String() string {
	if k == Build {
		return "build"
	}
	return "agent"
}

// Lease is one holding against the budget.
//
// WEIGHT IS WHY THIS IS NOT A COUNT. Phase 0 measured a subagent's marginal cost at +0.098
// cores median while a Heavy build peaks at 4.5, and a table that counted both as "one slot"
// would be repeating the exact error the pivot exists to correct (design.md section 5.2).
// So the budget is an integer number of SLOTS, one slot being one Medium build, and a lease
// carries a fractional weight against it. See budget.WeightOf.
type Lease struct {
	ID      string
	Session string
	Kind    Kind
	State   State
	Depth   int
	// Weight is how much of the budget this lease holds, in slots.
	Weight float64
	// Class is the gate's cost class for a Build lease ("medium", "heavy"), carried only so
	// a release can be logged against it (GOV-B29). Nothing decides anything on it; Weight
	// is what admission reads. Empty on an Agent lease and from a gate that sends none.
	Class string
	// ToolUseID is the PreToolUse key a Build lease is released by. Empty on an Agent
	// lease, whose SubagentStart carries no such key and must be correlated FIFO instead.
	ToolUseID    string
	GrantedAt    time.Time
	ExpiresAt    time.Time
	AgentID      string
	OwnerPID     int
	OwnerStarted time.Time
}

// Liveness answers whether the holder of a lease is still running.
//
// It is consulted by the reaper, never by the gate, so it may be slower than the hot path
// allows. An implementation that cannot tell MUST report alive: reaping a live agent's
// lease double-counts the budget, while keeping a dead one costs a slot for one TTL.
type Liveness interface {
	Alive(l Lease) bool
}

// Released is a lease that has just left the table, and when and why.
//
// IT EXISTS SO THE DAEMON CAN LOG A RELEASE (GOV-B29). Every path that removes a lease
// returns one, rather than the table logging itself: the table holds its lock while it
// drops, and the log is a file write that belongs after the reply, outside any lock.
type Released struct {
	Lease
	// At is when the table dropped it, on the table's own clock, so that held time is
	// At - GrantedAt with both ends from the same source.
	At time.Time
	// Why is set by the reaper only: "expired", "session-dead" or "owner-dead". The other
	// paths are named by their caller, which knows which hook it is answering.
	Why string
}

// Held is how long the lease was held.
func (r Released) Held() time.Duration { return r.At.Sub(r.GrantedAt) }
