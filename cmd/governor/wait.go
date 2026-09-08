package main

import (
	"sync"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
)

// The wait queue and the denial log (GOV-08, design.md sections 4.2 and 6.2).
//
// These are the two halves of "prefer waiting over denying". The queue is what lets a spawn
// stall briefly instead of being refused, which costs no tokens and requires the model to
// reason about nothing. The denial log is what stops the refusals that do happen from
// turning into a retry loop.

// denyWindow is how long a session's refusals are remembered for the purpose of escalating
// the text. It is a sliding window rather than a session total, because a session that was
// refused twenty minutes ago and is being refused again now is not in a retry loop; it has
// simply come back to a busy machine.
const denyWindow = 60 * time.Second

// denyCooloff is the rate limit. Inside it, a session that has just been refused is refused
// again immediately rather than parked.
//
// This is what "repeat denials are rate-limited" buys, and it protects the queue rather
// than the caller: a session retrying in a loop would otherwise take a place in the FIFO
// every time and hold a connection for the whole of wait_max on each attempt, crowding out
// sessions that are asking once and waiting properly. It is deliberately much shorter than
// denyWindow — one retry a few seconds later still gets a real wait.
const denyCooloff = 5 * time.Second

// waiter is one parked acquire.
type waiter struct {
	session string
	depth   int
	// granted has room for one lease and is written exactly once, by the pump, for a waiter
	// it has just removed from the queue. Buffering it is what lets the pump hand over a
	// lease without ever blocking on a waiter that has stopped listening.
	granted chan lease.Lease
}

// waitQueue is the FIFO of parked acquires, across all sessions (design.md section 6.2).
//
// One queue rather than one per session, and strictly ordered: the next freed slot goes to
// whoever has waited longest, so a session spawning in a tight loop cannot starve a quiet
// one. Max-min fair share (section 6.1) will change who is allowed to *join* this queue;
// it does not change the order once here.
type waitQueue struct {
	mu sync.Mutex
	q  []*waiter
}

func (q *waitQueue) enqueue(w *waiter) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.q = append(q.q, w)
}

// drop removes w from the queue, and reports a lease if one had already been handed to it.
//
// That return is the whole reason this is not a plain remove. A waiter timing out at the
// same instant as the pump granting it a lease is a real race on a busy machine, and the
// two wrong answers are both bad: refusing a caller that was granted a slot wastes it for
// the lease's whole TTL, and dropping the queue entry while the lease sits unread leaks it.
// Resolving both under one lock means the lease always has exactly one owner.
func (q *waitQueue) drop(w *waiter) (lease.Lease, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Removed first, unconditionally. The pump always dequeues before it hands over a
	// lease, so a delivered waiter is normally gone from the queue already — but making
	// that an assumption here would leave a stale entry behind the moment it stopped being
	// true, and a stale entry is a slot handed to a caller that will never read it.
	for i, x := range q.q {
		if x == w {
			q.q = append(q.q[:i], q.q[i+1:]...)
			break
		}
	}
	select {
	case l := <-w.granted:
		return l, true
	default:
	}
	return lease.Lease{}, false
}

func (q *waitQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.q)
}

// denyLog counts recent refusals per session, so the reason text can escalate and so a
// session in a retry loop can be refused without being parked.
type denyLog struct {
	mu  sync.Mutex
	m   map[string]denyState
	now func() time.Time
}

type denyState struct {
	count int
	last  time.Time
}

func newDenyLog(now func() time.Time) *denyLog {
	if now == nil {
		now = time.Now
	}
	return &denyLog{m: map[string]denyState{}, now: now}
}

// record notes a refusal and returns how many this session has had inside the window,
// counting this one.
func (d *denyLog) record(session string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	st := d.m[session]
	if st.last.IsZero() || now.Sub(st.last) > denyWindow {
		st.count = 0
	}
	st.count++
	st.last = now
	d.m[session] = st
	return st.count
}

// throttled reports whether this session was refused too recently to be parked again.
func (d *denyLog) throttled(session string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	st, ok := d.m[session]
	return ok && d.now().Sub(st.last) < denyCooloff
}

// sweep forgets sessions with no refusal inside the window. Called from the reaper, because
// otherwise this map is the one thing in the daemon that grows with every session that ever
// connected and never shrinks.
func (d *denyLog) sweep() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	for s, st := range d.m {
		if now.Sub(st.last) > denyWindow {
			delete(d.m, s)
		}
	}
}
