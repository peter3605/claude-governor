package lease

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Default TTLs. Both are backstops, not the primary mechanism, and they are deliberately
// far apart. Both were checked against measured timings in GOV-09; the numbers are below,
// because a constant that cannot say where it came from is a constant nobody dares change.
//
// A tentative lease is reclaimed quickly because the thing it is waiting for — the spawn
// PreToolUse announced — either happens within moments or never happens at all: the user
// declines the permission prompt, the turn is interrupted, the model changes its mind.
// Without this reclaim, every abandoned spawn would consume a slot permanently.
//
// THE TENTATIVE TTL IS MEASURED. hook-surface.md section 7 timed PreToolUse to its own
// SubagentStart at 35–60 ms, with four spawns forced into one assistant message and
// serialised ~0.9 s apart. Thirty seconds is roughly 500x the observed gap, which is the
// right kind of margin for a backstop: it has to outlive a legitimately slow spawn, because
// reclaiming a lease out from under a spawn that is merely slow hands the same slot out
// twice.
//
// A confirmed lease is reclaimed slowly because the cost of the two mistakes is not
// symmetric. Reaping a live agent's lease double-counts the budget and over-subscribes the
// machine; keeping a dead one costs one slot until the TTL expires. Liveness is what
// actually collects confirmed leases (a session that died takes its leases with it); the
// TTL only catches an agent whose SubagentStop was lost while its session kept running, or
// an owner that lingers as a zombie, which liveness cannot see through.
//
// THE CONFIRMED TTL IS BOUNDED, NOT MEASURED, and the distinction is honest rather than
// pedantic. GOV-09 set out to calibrate it against the SubagentStart timings GOV-02 recorded
// and found few to calibrate against, which it attributed to SubagentStart not firing in
// interactive sessions. GOV-27 showed that reading was wrong — SubagentStart fires per
// Agent-tool spawn either way (hook-surface.md section 8.1) — but the shortage of samples was
// real, for the more mundane reason that the Agent tool is called rarely: 26 times in every
// transcript on the machine.
//
// What evidence exists bounds an agent's life well under a minute — 1.7 s to 8.3 s across the
// probe sessions, a p95 of 42.7 s and a maximum of 43.2 s across those 26 Agent tool calls,
// and 22.8-23.1 s across the three deliberately slow agents GOV-27 ran. Half an hour is ~40x
// the longest of those. It stays generous on purpose, and it stays a guess about the tail
// rather than a measurement of it.
const (
	DefaultTentativeTTL = 30 * time.Second
	DefaultConfirmedTTL = 30 * time.Minute
)

// DefaultBuildTTL is how long a Build lease lives without a PostToolUse to return it.
//
// IT IS NOT A BACKSTOP, AND THAT IS THE WHOLE POINT. For an agent the TTL catches a lost
// SubagentStop, which is rare. For a build, hook-surface.md section 19 MEASURED three
// ordinary outcomes that produce no usable release at all — the command errors (no
// PostToolUse is emitted), it was backgrounded, or it exceeded its timeout and was
// backgrounded rather than killed. A failing build is the normal state of a build under
// development, so the reaper is this path's primary mechanism and this constant is a control
// parameter rather than a safety margin.
//
// FIVE MINUTES, MEASURED. Over the 20,614 Bash calls tools/bashcorpus collects, the runtime
// of gated FOREGROUND calls is p50 6.4 s, p90 57.5 s, p99 360.7 s (design.md section 5.6).
// The share that outlives a candidate TTL:
//
//	30 s (DefaultTentativeTTL)  17.9%   <- one build in five reaped while still running
//	60 s                         9.8%
//	120 s                        5.6%
//	300 s                        1.4%   <- chosen
//	600 s                        0.5%
//
// The two errors are not the same size, which is what picks the number. Too SHORT reaps a
// lease out from under a running build, so the budget over-grants against real load — silent,
// and worst under exactly the fan-out this governor exists to control. Too LONG holds a slot
// after a failed build, which costs throughput, decays on its own, and is visible in
// `governor status`. 600 s buys 0.9 points of coverage for double the leak; 300 s covers
// 98.6% and is the right side of the trade.
//
// Reusing DefaultTentativeTTL here would have been the easy wiring and is wrong by a factor
// measured rather than guessed.
const DefaultBuildTTL = 300 * time.Second

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
	// BuildTTL is how long a Build lease lives. See DefaultBuildTTL: on this path the TTL
	// is the primary reclaim mechanism, not a backstop.
	BuildTTL time.Duration
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
	byTool   map[string]string   // tool_use_id -> lease ID, Build leases only
	pending  map[string][]string // session -> FIFO of tentative lease IDs
	sessions map[string]Session
	seq      uint64
	// weight is the sum of every held lease's Weight, maintained incrementally so that
	// admission does not walk the table. It is the authority on what is held; len(byID) is
	// only ever a count of entries.
	weight float64
	opt    Options
}

// NewTable builds an empty table.
func NewTable(opt Options) *Table {
	if opt.TentativeTTL <= 0 {
		opt.TentativeTTL = DefaultTentativeTTL
	}
	if opt.ConfirmedTTL <= 0 {
		opt.ConfirmedTTL = DefaultConfirmedTTL
	}
	if opt.BuildTTL <= 0 {
		opt.BuildTTL = DefaultBuildTTL
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Table{
		byID:     map[string]*Lease{},
		byAgent:  map[string]string{},
		byTool:   map[string]string{},
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
// depth is recorded here and enforced by the caller. The daemon refuses a nested spawn
// before it ever reaches this function (design.md section 6.3), so a lease in this table is
// always depth 0; the field is kept because a lease that cannot say what it was taken for is
// harder to debug than one that can.
//
// Request says what is being taken and how much of the budget it wants. The two kinds differ
// in every part of their lifecycle, so they are one function only because the ADMISSION rule
// is genuinely shared and must stay atomic — see the paragraph above.
type Request struct {
	Session string
	Depth   int
	Kind    Kind
	// Weight is how much of the budget this lease holds, in slots. Zero takes the kind's
	// default, which is one slot — an agent asking for nothing would be ungoverned.
	Weight float64
	// ToolUseID keys a Build lease for release. Ignored for an Agent lease.
	ToolUseID string
}

// Fits reports whether Acquire would admit this request right now, WITHOUT taking a lease,
// changing any state, or counting a denial.
//
// IT EXISTS FOR THE ADVISORY CHANNEL (design.md section 8) AND IT SHARES ACQUIRE'S RULE BY
// CALLING IT, not by restating it. GOV-16's fourth clause is that what the MCP tool tells a
// model must match what the gate will do to it, and the cheapest way for that to stop being
// true is two copies of the admission arithmetic drifting a release apart. So there is one
// copy, in fitsLocked, and Acquire is its only other caller.
//
// The answer is a snapshot and nothing reserves anything on the strength of it: a slot can be
// taken between this call and the acquire it advised. That is correct — advice steers, the
// gate enforces — and it is why this is not a two-phase reservation.
func (t *Table) Fits(r Request, limit int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fitsLocked(normalWeight(r.Weight), limit)
}

// fitsLocked is THE ADMISSION RULE, and it is WORK-CONSERVING because it has to be. The
// budget is an integer count of slots where one slot is one Medium build, so a Heavy build
// weighs 3.75 and a budget floored at 1 (invariant 4) could never admit one. Refusing it
// would not be conservative, it would be a deadlock: the machine would sit idle refusing the
// only work it had. So a request that does not fit an EMPTY table is admitted anyway and
// simply runs alone. That is what invariant 4 means by "still makes progress, one at a time"
// once slots stopped being interchangeable.
func (t *Table) fitsLocked(weight float64, limit int) bool {
	return len(t.byID) == 0 || t.weight+weight <= float64(limit)
}

// normalWeight is how a request's weight is read before anything compares it. A request that
// names no weight is one whole slot — the shape every gate before GOV-15 sent — and reading
// an absent weight as zero would admit an unbounded number of them.
func normalWeight(w float64) float64 {
	if w <= 0 {
		return 1
	}
	return w
}

// Acquire takes a lease if the budget has room for it.
func (t *Table) Acquire(r Request, limit int) (Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	w := normalWeight(r.Weight)
	if !t.fitsLocked(w, limit) {
		return Lease{}, false
	}

	now := t.opt.Now()
	t.seq++
	l := &Lease{
		ID:        fmt.Sprintf("L-%04x", t.seq),
		Session:   r.Session,
		Kind:      r.Kind,
		Depth:     r.Depth,
		Weight:    w,
		ToolUseID: r.ToolUseID,
		GrantedAt: now,
	}
	if r.Kind == Build {
		// Born confirmed: nothing announces that a command started, so there is no second
		// event to confirm against and a tentative state would only ever expire
		// (hook-surface.md section 19, design.md section 4.3).
		l.State = Confirmed
		l.ExpiresAt = now.Add(t.opt.BuildTTL)
		if r.ToolUseID != "" {
			t.byTool[r.ToolUseID] = l.ID
		}
	} else {
		l.State = Tentative
		l.ExpiresAt = now.Add(t.opt.TentativeTTL)
		t.pending[r.Session] = append(t.pending[r.Session], l.ID)
	}
	t.byID[l.ID] = l
	t.weight += w
	return *l, true
}

// ReleaseBuild returns a Build lease by the tool_use_id it was taken with.
//
// Unlike the agent path this needs no correlation at all: PostToolUse carries the SAME
// tool_use_id as the PreToolUse that took the lease, measured in every pairing observed
// (hook-surface.md section 19). The session is checked for the same reason Release checks it.
func (t *Table) ReleaseBuild(session, toolUseID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	id, ok := t.byTool[toolUseID]
	if !ok {
		return false
	}
	l, ok := t.byID[id]
	if !ok {
		delete(t.byTool, toolUseID)
		return false
	}
	if session != "" && l.Session != session {
		return false
	}
	t.dropLocked(l)
	return true
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

// Drop removes one lease by ID, whatever state it is in. It reports whether there was one.
//
// This is the hand-back path (GOV-08): the wait queue can grant a lease to a caller that has
// gone away between being handed it and being told, and that slot should return to the pool
// now rather than waiting out a TTL. It is deliberately not part of the release path, which
// keys off agent_id and has a session to check against; a lease that was never confirmed has
// neither.
func (t *Table) Drop(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	l, ok := t.byID[id]
	if !ok {
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

// Reap drops leases that have outlived their TTL, leases whose registered session is gone,
// and confirmed leases whose own owner is gone. It returns how many it dropped.
//
// Invariant 2 (design.md section 3): leases expire on their own. The stop hooks are assumed
// never to fire — leaks are the default outcome here, not the edge case — so this is the
// mechanism the budget's correctness actually rests on.
func (t *Table) Reap() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.opt.Now()
	dead := t.deadSessionsLocked()
	dropped := 0
	for _, l := range t.snapshotLeasesLocked() {
		switch {
		case !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt):
		case dead[l.Session]:
		case l.State == Confirmed && t.opt.Liveness != nil && !t.opt.Liveness.Alive(*l):
		default:
			continue
		}
		t.dropLocked(l)
		dropped++
	}
	// A session whose process is gone is not coming back, and leaving it registered would
	// have the reaper stat a dead PID every two seconds forever and have status keep
	// reporting a holder that no longer exists.
	for id := range dead {
		delete(t.sessions, id)
		delete(t.pending, id)
	}
	return dropped
}

// deadSessionsLocked names the registered sessions whose own process is gone.
//
// This is what makes a SIGKILLed session's *tentative* leases collectable (GOV-09). A
// tentative lease has no owner of its own — OwnerPID is bound at Confirm and nowhere else —
// so the confirmed-lease liveness check below cannot see it, and before this it waited out
// the full tentative TTL while its session was already gone. Mid-fan-out is exactly the
// state where that matters: some leases confirmed, one still tentative, and the session
// killed between the two.
//
// The normal path for this is SessionEnd calling ReleaseSession. This is the path for when
// there is no normal path — kill -9, a crashed session, a machine that slept — which is the
// case the reaper exists for in the first place.
//
// It asks Liveness about a synthesised Lease carrying the session's own PID rather than
// widening the interface to take a Session. The seam has one method for one question, "is
// this holder still running?", and a session is just another holder; adding a second method
// would be a change to the tier-two contract for no gain, since a heartbeat implementation
// answers both from the same table.
func (t *Table) deadSessionsLocked() map[string]bool {
	if t.opt.Liveness == nil {
		return nil
	}
	var dead map[string]bool
	for id, s := range t.sessions {
		// A session that registered no PID is left to its leases' TTLs. The owner walk
		// reports unknown rather than guessing (design.md section 4.3), and an unknown
		// owner must never be treated as a dead one.
		if s.PID <= 0 {
			continue
		}
		if t.opt.Liveness.Alive(Lease{Session: id, OwnerPID: s.PID, OwnerStarted: s.PIDStarted}) {
			continue
		}
		if dead == nil {
			dead = map[string]bool{}
		}
		dead[id] = true
	}
	return dead
}

// dropLocked removes one lease and every index pointing at it.
//
// It is the ONLY place a lease leaves the table, which is what lets the running weight be
// maintained incrementally rather than recomputed: a second removal path would silently
// leak budget that no test would see until the machine stopped granting.
func (t *Table) dropLocked(l *Lease) {
	if _, ok := t.byID[l.ID]; ok {
		t.weight -= l.Weight
		if t.weight < 0 {
			// Defence against a double-drop rounding the sum negative, which would hand out
			// budget that does not exist. The table is the authority on what is held.
			t.weight = 0
		}
	}
	delete(t.byID, l.ID)
	if l.AgentID != "" && t.byAgent[l.AgentID] == l.ID {
		delete(t.byAgent, l.AgentID)
	}
	if l.ToolUseID != "" && t.byTool[l.ToolUseID] == l.ID {
		delete(t.byTool, l.ToolUseID)
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

// InUse is how many LEASES are held: tentative and confirmed, agents and builds alike.
//
// A tentative lease counts. It represents a spawn that is about to happen, and not counting
// it would let a burst of simultaneous PreToolUse calls each see room that the others are
// already taking.
//
// IT IS A COUNT, NOT THE BUDGET FIGURE, and since GOV-15 those are two different numbers.
// Admission compares Slots, because a subagent and a `cargo build` are not the same size and
// the whole pivot was noticing that. This stays a count because a count is what the status
// line's per-session breakdown and the daemon's own logs are actually reporting — "this
// session holds three things" — and because a reader who sees "in use" expects to be told how
// many. Nothing decides anything on it.
func (t *Table) InUse() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byID)
}

// Slots is the exact sum of held lease weights, in slots of budget. This is what admission
// compares, and it is the authority on how full the machine is.
func (t *Table) Slots() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.weight
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
	Slots     float64
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
	s.Slots = t.weight
	for _, st := range per {
		s.Sessions = append(s.Sessions, *st)
	}
	sort.Slice(s.Sessions, func(i, j int) bool { return s.Sessions[i].ID < s.Sessions[j].ID })
	return s
}

// Leases is a copy of every lease currently held, in no particular order.
//
// Snapshot answers "how many, and whose", which is what status needs. This answers "which
// exactly", which is what a test asserting per-lease detail needs — chiefly that a held lease
// names a live owner, since a lease with no owner is one the reaper cannot ask Liveness about
// and can only collect by TTL.
//
// It returns copies: a caller holding a *Lease could otherwise mutate the table's own entry
// with no lock held.
func (t *Table) Leases() []Lease {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]Lease, 0, len(t.byID))
	for _, l := range t.byID {
		out = append(out, *l)
	}
	return out
}
