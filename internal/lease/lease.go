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

// Lease is one slot in the budget.
type Lease struct {
	ID           string
	Session      string
	State        State
	Depth        int
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
