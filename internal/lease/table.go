package lease

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Default TTLs. Both are backstops, not the primary mechanism, and they are deliberately
// far apart.
//
// A tentative lease is reclaimed quickly because the thing it is waiting for — the spawn
// PreToolUse announced — either happens within moments or never happens at all: the user
// declines the permission prompt, the turn is interrupted, the model changes its mind.
// Without this reclaim, every abandoned spawn would consume a slot permanently.
//
// A confirmed lease is reclaimed slowly because the cost of the two mistakes is not
// symmetric. Reaping a live agent's lease double-counts the budget and over-subscribes the
// machine; keeping a dead one costs one slot until the TTL expires. Liveness is what
// actually collects confirmed leases (a session that died takes its leases with it); the
// TTL only catches an agent whose SubagentStop was lost while its session kept running.
const (
	DefaultTentativeTTL = 30 * time.Second
	DefaultConfirmedTTL = 30 * time.Minute
)

// Session is a registered Claude Code session, from the `session` operation of design.md
// section 7.
type Session struct {
	ID string
	// PID is the session's claude process, used to reap its leases when it dies.
	PID int
	// PIDStarted disambiguates a recycled PID.
	PIDStarted time.Time
	// Weight is the session's share for max-min fair allocation (design.md section 6.1).
	// It is recorded here and not yet acted on: allocation is a later story, and a weight
	// that arrives after the wire format has shipped is far more expensive than one
	// carried from the start.
	Weight int
	// StartedAt is when the session registered.
	StartedAt time.Time
}

// Options configure a Table. The zero value of each field takes the documented default.
type Options struct {
	TentativeTTL time.Duration
	ConfirmedTTL time.Duration
	// Liveness answers whether a confirmed lease's owner is still running. Nil means
	// nothing is ever reaped for liveness, only for TTL.
	Liveness Liveness
	// Now is the clock, injectable so the reaper can be tested without sleeping.
	Now func() time.Time
}

// Table is the lease table: one entry per subagent slot, tentative until the spawn is
// confirmed, reclaimed by TTL or by the death of its owner (design.md section 4.3).
//
// Everything is in memory and nothing is persisted. That is correct for one machine rather
// than a shortcut (section 12.2): a restart loses every lease and the reaper cleans up in
// seconds, where durable lease state would buy consistency the failure mode does not need.
type Table struct {
	mu       sync.Mutex
	byID     map[string]*Lease
	byAgent  map[string]string   // agent_id -> lease ID, confirmed leases only
	pending  map[string][]string // session -> FIFO of tentative lease IDs
	sessions map[string]Session
	seq      uint64
	opt      Options
}

// NewTable builds an empty table.
func NewTable(opt Options) *Table {
	if opt.TentativeTTL <= 0 {
		opt.TentativeTTL = DefaultTentativeTTL
	}
	if opt.ConfirmedTTL <= 0 {
		opt.ConfirmedTTL = DefaultConfirmedTTL
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Table{
		byID:     map[string]*Lease{},
		byAgent:  map[string]string{},
		pending:  map[string][]string{},
		sessions: map[string]Session{},
		opt:      opt,
	}
}

// Acquire takes a tentative lease for one spawn, if the budget has room for it.
//
// The limit is passed in and checked here rather than compared by the caller, because the
// check and the insert have to be one atomic step. A caller that reads InUse, decides, and
// then inserts has a window in which every concurrent caller reads the same number and they
// all decide yes — which is exactly the invariant the 200-call concurrency test exists to
// catch. Holding one lock across both is the whole of the fix.
//
// depth is recorded but not enforced. Denying nested fan-out is a later story (design.md
// section 6.3); carrying the value now means that story changes a policy rather than a
// wire format.
func (t *Table) Acquire(session string, depth, limit int) (Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.byID) >= limit {
		return Lease{}, false
	}
	now := t.opt.Now()
	t.seq++
	l := &Lease{
		ID:        fmt.Sprintf("L-%04x", t.seq),
		Session:   session,
		State:     Tentative,
		Depth:     depth,
		GrantedAt: now,
		ExpiresAt: now.Add(t.opt.TentativeTTL),
	}
	t.byID[l.ID] = l
	t.pending[session] = append(t.pending[session], l.ID)
	return *l, true
}

// Confirm binds a tentative lease to a live agent at SubagentStart.
//
// Three behaviours here are measured facts about the hook surface (design.md section 4.3),
// not defensive coding:
//
//   - SubagentStart fires again when an agent is resumed, so confirm MUST be idempotent per
//     agent_id. A second confirm for an agent that already holds a lease is a no-op — not a
//     second lease, and importantly not a second FIFO pop, which would silently confirm some
//     *other* pending spawn's lease against this agent.
//   - SubagentStart carries no tool_use_id and shares no per-spawn key with PreToolUse, so
//     the binding is per-session FIFO: the oldest unconfirmed tentative lease for this
//     session. Measured correct 4/4 under a four-call burst, because the two hooks strictly
//     interleave — but that ordering is observed rather than promised, which is why the
//     reaper is what makes a mispairing survivable instead of corrupting.
//   - A resumed subagent reaches the CPU with no PreToolUse at all, so a confirm with an
//     empty pending queue is dropped quietly. It is not an error and must not be logged as
//     one; v1 simply does not govern that spawn.
//
// leaseID is honoured when it names a tentative lease of this session, and ignored
// otherwise. It costs nothing to prefer an exact answer when the wire happens to carry one,
// and FIFO remains the rule for the case that actually occurs.
//
// An empty agentID still confirms: the spawn has happened and the slot is genuinely in use,
// so refusing would under-count the budget. Such a lease simply has no name to release it
// by, and the reaper is what collects it. Every SubagentStop payload measured in GOV-02
// carried an agent_id, so this is the degenerate case rather than the expected one.
func (t *Table) Confirm(session, leaseID, agentID string, pid int, pidStarted time.Time) (Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if id, ok := t.byAgent[agentID]; ok && agentID != "" {
		if l, ok := t.byID[id]; ok {
			return *l, true
		}
		// The lease was reaped out from under a live agent. Drop the stale index rather
		// than resurrecting it; the next confirm falls through to FIFO.
		delete(t.byAgent, agentID)
	}

	id := t.takePendingLocked(session, leaseID)
	if id == "" {
		return Lease{}, false
	}
	now := t.opt.Now()
	l := t.byID[id]
	l.State = Confirmed
	l.AgentID = agentID
	l.OwnerPID = pid
	l.OwnerStarted = pidStarted
	l.ExpiresAt = now.Add(t.opt.ConfirmedTTL)
	if agentID != "" {
		t.byAgent[agentID] = id
	}
	return *l, true
}

// takePendingLocked removes and returns one tentative lease ID for the session, preferring
// want when it is a valid tentative lease of that session. Entries whose lease has already
// been reaped are discarded as they are passed.
func (t *Table) takePendingLocked(session, want string) string {
	queue := t.pending[session]
	keep := queue[:0]
	found := ""

	for _, id := range queue {
		l, ok := t.byID[id]
		if !ok || l.State != Tentative {
			continue // reaped, or somehow already confirmed
		}
		if found == "" && (want == "" || id == want) {
			found = id
			continue
		}
		keep = append(keep, id)
	}
	// An explicit want that did not match anything still deserves the FIFO answer.
	if found == "" && want != "" && len(keep) > 0 {
		found, keep = keep[0], keep[1:]
	}

	if len(keep) == 0 {
		delete(t.pending, session)
	} else {
		t.pending[session] = append([]string(nil), keep...)
	}
	return found
}

// Release returns a confirmed lease to the pool at SubagentStop.
//
// SubagentStop fires more than once for the same agent, so a release for an agent holding
// no lease is a no-op reported as false, never an error.
func (t *Table) Release(session, agentID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	id, ok := t.byAgent[agentID]
	if !ok {
		return false
	}
	l, ok := t.byID[id]
	if !ok {
		// The index outlived the lease it pointed at. Drop the dangling entry.
		delete(t.byAgent, agentID)
		return false
	}
	if session != "" && l.Session != session {
		// Another session is claiming this agent. This should not happen, and the answer is
		// to do nothing at all: dropping the lease would free a slot still in use, and
		// dropping the index would leave a live lease that no release can ever reach. Left
		// alone, the lease stays correct and the reaper remains its backstop.
		return false
	}
	t.dropLocked(l)
	return true
}

// ReleaseSession drops every lease held by a session, and forgets the session. It is the
// SessionEnd path, and it is what stops a crashed session's leases from waiting out their
// TTL.
func (t *Table) ReleaseSession(session string) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := 0
	for _, l := range t.leasesForLocked(session) {
		t.dropLocked(l)
		n++
	}
	delete(t.sessions, session)
	delete(t.pending, session)
	return n
}

// StartSession registers a session, or updates one that re-registers.
func (t *Table) StartSession(s Session) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if s.Weight <= 0 {
		s.Weight = 1
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = t.opt.Now()
	}
	t.sessions[s.ID] = s
}

// Reap drops leases that have outlived their TTL, and confirmed leases whose owner is gone.
// It returns how many it dropped.
//
// Invariant 2 (design.md section 3): leases expire on their own. The stop hooks are assumed
// never to fire — leaks are the default outcome here, not the edge case — so this is the
// mechanism the budget's correctness actually rests on.
func (t *Table) Reap() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.opt.Now()
	dropped := 0
	for _, l := range t.snapshotLeasesLocked() {
		switch {
		case !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt):
		case l.State == Confirmed && t.opt.Liveness != nil && !t.opt.Liveness.Alive(*l):
		default:
			continue
		}
		t.dropLocked(l)
		dropped++
	}
	return dropped
}

// dropLocked removes one lease and every index pointing at it.
func (t *Table) dropLocked(l *Lease) {
	delete(t.byID, l.ID)
	if l.AgentID != "" && t.byAgent[l.AgentID] == l.ID {
		delete(t.byAgent, l.AgentID)
	}
	if q, ok := t.pending[l.Session]; ok {
		keep := q[:0]
		for _, id := range q {
			if id != l.ID {
				keep = append(keep, id)
			}
		}
		if len(keep) == 0 {
			delete(t.pending, l.Session)
		} else {
			t.pending[l.Session] = append([]string(nil), keep...)
		}
	}
}

func (t *Table) leasesForLocked(session string) []*Lease {
	var out []*Lease
	for _, l := range t.byID {
		if l.Session == session {
			out = append(out, l)
		}
	}
	return out
}

func (t *Table) snapshotLeasesLocked() []*Lease {
	out := make([]*Lease, 0, len(t.byID))
	for _, l := range t.byID {
		out = append(out, l)
	}
	return out
}

// InUse is how many slots are held: tentative and confirmed alike.
//
// A tentative lease counts. It represents a spawn that is about to happen, and not counting
// it would let a burst of simultaneous PreToolUse calls each see room that the others are
// already taking.
func (t *Table) InUse() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byID)
}

// SessionStat is one session's line in a status snapshot.
type SessionStat struct {
	Session
	Tentative int
	Confirmed int
}

// Snapshot is a consistent view of the table for the status operation. Every count is taken
// under one lock, so status can never report a total that disagrees with its own parts.
type Snapshot struct {
	InUse     int
	Tentative int
	Confirmed int
	Sessions  []SessionStat
}

// Snapshot reads the table for status.
func (t *Table) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := Snapshot{}
	per := map[string]*SessionStat{}
	for id, sess := range t.sessions {
		per[id] = &SessionStat{Session: sess}
	}
	for _, l := range t.byID {
		st, ok := per[l.Session]
		if !ok {
			// A lease from a session that never registered, or has already ended. It is
			// still holding a slot, so it is still counted in the totals; it just has no
			// line of its own.
			st = &SessionStat{Session: Session{ID: l.Session}}
			per[l.Session] = st
		}
		if l.State == Confirmed {
			st.Confirmed++
			s.Confirmed++
		} else {
			st.Tentative++
			s.Tentative++
		}
	}
	s.InUse = len(t.byID)
	for _, st := range per {
		s.Sessions = append(s.Sessions, *st)
	}
	sort.Slice(s.Sessions, func(i, j int) bool { return s.Sessions[i].ID < s.Sessions[j].ID })
	return s
}
