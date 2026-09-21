package lease

import (
	"fmt"
	"math"
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

// demandWindow is how long a refusal keeps a session counted as contending for fair share
// (design.md section 6.1), once it is no longer the one asking.
//
// It matches maxParkedWait, which is the longest the daemon will hold a caller before
// refusing it: a session that was refused less than a full parked wait ago is either still
// parked or about to ask again, and in both cases it genuinely wants a slot. Longer would let
// a session that has given up go on depressing everyone else's share; shorter would let an
// incumbent recapture the slot it freed in the gap between a waiter being refused and being
// served, which is the starvation this window exists to close.
//
// It is deliberately a decay rather than a flag cleared on success: a session that just took
// a slot in a build loop still wants the next one, and clearing on success would make its
// demand flicker in and out of view once per acquire.
const demandWindow = 30 * time.Second

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
	// Weight is the session's share for max-min fair allocation (design.md section 6.1),
	// acted on by shareLocked since GOV-18. Zero and negative are read as the default 1.
	//
	// ONE MECHANISM, MORE THAN ONE TRIGGER. This field is the only weighting scheme there is
	// and nothing should be built beside it — but what moves it is open: GOV-B01's
	// `governor boost` sets it because a person said so, and GOV-B02's focus boost would set
	// the same field because the window came forward. §6.1 used to say the focus boost was
	// the only thing that may ever move a weight, which read as a ban on the manual one.
	Weight int
	// BoostedUntil is when a temporary weight reverts to 1. Zero means the weight is not
	// temporary and nothing will revert it.
	//
	// A BOOST EXPIRES FOR THE SAME REASON A PINNED BUDGET DOES (design.md section 9.1). It is
	// a manual override of a fairness rule, so a forgotten one is a session quietly taking
	// more than its share of every contended budget for as long as the daemon lives — and
	// unlike a pinned budget, nothing about the machine looks wrong while it happens.
	BoostedUntil time.Time
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
	// demanding remembers when a session was last refused, which is how a session that is
	// not the current caller still counts as contending for fair share. Swept by Reap.
	demanding map[string]time.Time
	seq       uint64
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
		byID:      map[string]*Lease{},
		byAgent:   map[string]string{},
		byTool:    map[string]string{},
		pending:   map[string][]string{},
		sessions:  map[string]Session{},
		demanding: map[string]time.Time{},
		opt:       opt,
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
	ok, _ := t.fitsLocked(r.Session, normalWeight(r.Weight), limit)
	return ok
}

// Refusal names WHICH half of the admission rule would refuse this request: "" when it would
// be admitted, "capacity" when the machine itself has no room, "share" when the machine has
// room but this session has reached its fair share.
//
// It exists so the refusal text can stop saying "Machine at capacity" when the machine is not
// at capacity. Since GOV-18 a session can be refused at 4 of 6 slots because the other two
// belong to somebody else, and design.md section 4.2 is explicit that a reason clause stating
// a fact the daemon did not send is a fabrication. It shares fitsLocked rather than
// re-deriving anything, for the same reason Fits does.
func (t *Table) Refusal(r Request, limit int) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	ok, byShare := t.fitsLocked(r.Session, normalWeight(r.Weight), limit)
	switch {
	case ok:
		return ""
	case byShare:
		return "share"
	default:
		return "capacity"
	}
}

// fitsLocked is THE ADMISSION RULE, and it is WORK-CONSERVING because it has to be. The
// budget is an integer count of slots where one slot is one Medium build, so a Heavy build
// weighs 3.75 and a budget floored at 1 (invariant 4) could never admit one. Refusing it
// would not be conservative, it would be a deadlock: the machine would sit idle refusing the
// only work it had. So a request that does not fit an EMPTY table is admitted anyway and
// simply runs alone. That is what invariant 4 means by "still makes progress, one at a time"
// once slots stopped being interchangeable.
//
// GOV-18 ADDED THE SECOND HALF. Global capacity says whether the MACHINE has room; fair share
// says whether THIS SESSION may have it. Both must pass. The order matters only for cost:
// capacity is one comparison and share is a walk over sessions, so capacity is asked first
// and the walk is skipped entirely whenever the machine is full.
//
// It reports, second, whether it was FAIR SHARE that refused rather than capacity — the two
// are a different message to the caller, and Refusal is what turns that into one.
//
// GOV-34 WIDENED THE ESCAPE FROM "EMPTY" TO "THIS SESSION HOLDS NOTHING", because an empty
// table is not the only way a request can be refused forever. A Heavy build weighs 3.75 and
// this machine's calibrated ceiling is 4, so it does not fit beside ANY other lease, however
// small — and the table is not empty, so the escape above never fired. The 2026-09-21
// incident is that exact case: one session held 1.00 for ten minutes, another session holding
// nothing was refused five times for "capacity" at 1.00/4, and no amount of waiting inside
// maxParkedWait could have changed the answer. Invariant 6 says every live session is
// guaranteed one slot before any session gets a second, and that is precisely what was
// violated: the refused session had none and the holder had one.
//
// THE ESCAPE NEEDS FREE HEADROOM, AND THAT IS WHAT SEPARATES IT FROM AN ORDINARY QUEUE. A
// machine at exactly 4 of 4 really is at capacity: a session asking for its first slot there
// is refused because there is nothing to give it, waiting genuinely works, and §6.2's FIFO
// hands it the next release. That case must keep refusing, and `t.weight < limit` is what
// keeps it refusing. The incident is the other case — 1.00 of 4, three slots free, refused —
// where waiting is not what fixes it because the free space is not the binding constraint,
// the size of the request against it is. So the rule reads: while the machine has room, a
// session holding nothing is never turned away for lack of it.
//
// WHAT BOUNDS IT is the pair of conditions, and neither is redundant. `t.weight < limit`
// means the escape may not fire while an escape is already outstanding, so at most ONE lease
// is over budget at a time and the table's total weight never exceeds limit plus one lease.
// `heldLocked(session) == 0` means a session gets at most one such admission and its SECOND
// request faces the full rule, so a session cannot walk over the budget one lease at a time.
// Together they deliver invariant 6 sequentially — every session eventually gets its first
// slot, in the order §6.2's FIFO decides — rather than seating every session at once, which
// on a four-slot ceiling would be a promise the machine cannot keep.
//
// THE EMPTY-TABLE CASE IS KEPT SEPARATE even though the wider rule subsumes its capacity
// half, because it also skips fair share: on an empty table a session refused within
// demandWindow still counts as contending, so shareLocked can bind against a machine with
// nothing running at all. Deleting the branch would reintroduce the deadlock by that door.
func (t *Table) fitsLocked(session string, weight float64, limit int) (ok, byShare bool) {
	if len(t.byID) == 0 {
		// Work-conserving, and it precedes fair share deliberately: an empty table means
		// nobody is being deprived of anything, so there is no share to enforce and the
		// deadlock this escape exists to prevent is the only live concern.
		return true, false
	}
	if t.weight+weight > float64(limit) {
		if t.weight < float64(limit) && t.heldLocked(session) == 0 {
			// The machine has room, this session holds none of it, and nothing is over
			// budget yet: it is asking for its first slot and invariant 6 guarantees it
			// one. Fair share is skipped for the same reason the empty-table branch skips
			// it — a session that holds nothing cannot be over its share of anything.
			return true, false
		}
		return false, false
	}
	if weight+t.heldLocked(session) > t.shareLocked(session, weight, limit) {
		return false, true
	}
	return true, false
}

// heldLocked is how much budget one session already holds, in slots.
//
// It walks the leases rather than keeping a per-session running total. That is a deliberate
// choice about where to pay: a running total would have to be maintained in dropLocked and in
// Acquire, and dropLocked's comment is emphatic that being the ONLY removal path is what keeps
// the global weight honest. A second incrementally-maintained sum is a second thing that can
// silently disagree with the table, and the table is small — this is a walk over held leases,
// not over history. See TestFairShareIsCheapOnTheHotPath for what it actually costs.
func (t *Table) heldLocked(session string) float64 {
	var w float64
	for _, l := range t.byID {
		if l.Session == session {
			w += l.Weight
		}
	}
	return w
}

// shareLocked is this session's max-min fair entitlement, in slots (design.md section 6.1).
//
// MAX-MIN BY PROGRESSIVE FILLING. Every session with revealed demand gets an equal share of
// the budget per unit of its weight; a session whose demand is SMALLER than that share takes
// only what it wants and the remainder is redistributed among the rest, repeatedly, until
// either nothing is left over or every remaining session is capped. That redistribution is
// the second sentence of section 6.1 and it is why this is not simply limit/n.
//
// DEMAND IS REVEALED, NOT DECLARED. Nothing states a demand up front, so it is read off the
// table: a session's demand is what it HOLDS, except for a session that is asking or was
// refused recently, which demands MORE than it holds and is therefore unbounded (see the two
// comments in the body — both are load-bearing, and the rule decides nothing without them).
// A session that holds nothing, is not asking and has not been refused is not a contender at
// all: it consumes no share and cannot deprive anyone. That is what makes the common
// single-session case free — one contender's share is the whole budget, so the rule never
// binds until a second session actually wants something.
//
// WHY NOT RESERVE FOR REGISTERED-BUT-IDLE SESSIONS. Six registered sessions of which one is
// building would otherwise cap that one at a sixth of the machine while five-sixths sat idle,
// which is the opposite of what a governor is for. The guarantee in section 6.1 — every live
// session gets one slot before any gets a second — is a guarantee about sessions that ASK,
// and it is delivered by the arithmetic below rather than by holding capacity empty: a
// contender's cap is level*weight, which is at least one whole slot whenever the budget can
// seat one slot per contender, so a session reaching for its first is admitted while a
// session reaching past its share is not.
func (t *Table) shareLocked(session string, asking float64, limit int) float64 {
	// demand per contending session, and the weight each contends with.
	demand := map[string]float64{}
	for _, l := range t.byID {
		demand[l.Session] += l.Weight
	}
	// A SESSION THAT IS ASKING DEMANDS MORE THAN IT HOLDS, and saying so is what makes this
	// rule bind at all. Reading the asker's demand as "what it holds plus this one request"
	// puts total demand at sum(held)+w, which reaches the budget at exactly the moment the
	// capacity check above already refuses — so the share cap would sit permanently in the
	// shadow of capacity and never once decide anything. The first draft of this function did
	// that, and its clause 3 test passed with fair share entirely removed.
	//
	// Unbounded is also the honest reading: a session mid-fan-out has revealed that it wants
	// another slot and has told us nothing about where it would stop.
	demand[session] = math.Inf(1)
	// A session refused recently is STILL demanding even though it is not the caller now.
	// Without this the table only ever sees the demand of whoever happens to be calling, so a
	// session that frees a slot and immediately asks again recaptures it before the session
	// it was freed for is ever woken — starvation with a fair-share rule bolted to the side.
	for id, at := range t.demanding {
		if t.opt.Now().Sub(at) <= demandWindow {
			demand[id] = math.Inf(1)
		}
	}

	if len(demand) == 1 {
		// The only contender. Its share is the whole budget, which is what makes the
		// uncontended hot path a map of one entry and no filling loop at all.
		return float64(limit)
	}

	type contender struct {
		id     string
		d      float64
		w      float64
		capped bool
	}
	cs := make([]contender, 0, len(demand))
	for id, d := range demand {
		cs = append(cs, contender{id: id, d: d, w: t.weightOfLocked(id)})
	}

	// Progressive filling. Each round hands every uncapped contender an equal share per unit
	// weight; any whose demand is under that share is satisfied, capped at its demand, and
	// its leftover returned to the pool for the next round. It terminates because every round
	// either caps at least one contender or settles.
	remaining := float64(limit)
	for {
		var totalW float64
		for _, c := range cs {
			if !c.capped {
				totalW += c.w
			}
		}
		if totalW <= 0 {
			break
		}
		level := remaining / totalW
		progressed := false
		for i := range cs {
			if cs[i].capped || cs[i].d > level*cs[i].w {
				continue
			}
			cs[i].capped = true
			remaining -= cs[i].d
			progressed = true
		}
		if !progressed {
			// Nobody else is satisfied below the level, so this level is the answer for
			// every contender still uncapped.
			for _, c := range cs {
				if c.id == session && !c.capped {
					return level * c.w
				}
			}
			break
		}
	}
	// Everyone was satisfied inside the budget, this session included: it may have what it
	// asked for.
	return demand[session]
}

// weightOfLocked is a session's registered share weight, defaulting to 1.
//
// A session holding leases without having registered gets the default rather than zero. Zero
// would make its entitlement zero and deny it forever, and the leases prove it is real; the
// registration is what is missing, not the session.
//
// AN EXPIRED BOOST IS DROPPED HERE, on the read, rather than only by the reaper. This is the
// function fair share actually consults, so expiring at the point of use means no admission
// decision can ever be made against a boost whose deadline has passed — including on a daemon
// whose reaper is wedged, and including from a path added later that does not know the reaper
// sweeps these.
func (t *Table) weightOfLocked(id string) float64 {
	s, ok := t.sessions[id]
	if !ok {
		return 1
	}
	s = t.expireBoostLocked(s)
	if s.Weight > 0 {
		return float64(s.Weight)
	}
	return 1
}

// expireBoostLocked reverts a session whose boost has run out, and returns the current session.
//
// IT IS SHARED BY THE ADMISSION PATH AND THE SNAPSHOT, and it has to be. The first version
// expired only in weightOfLocked, which fair share consults — so admission was correct while
// Snapshot went on REPORTING the boosted weight indefinitely, and `governor status` would have
// shown a session at weight 2 that was being admitted at weight 1. A surface disagreeing with
// the decision it is describing is worse than either being wrong on its own, because it makes
// the decision impossible to check.
func (t *Table) expireBoostLocked(s Session) Session {
	if s.BoostedUntil.IsZero() || t.opt.Now().Before(s.BoostedUntil) {
		return s
	}
	s.Weight, s.BoostedUntil = 1, time.Time{}
	t.sessions[s.ID] = s
	return s
}

// Boost sets a session's share weight until until, and reports whether the session was known.
//
// It is NOT a second weighting mechanism — it writes Session.Weight, the one field fair share
// has read since GOV-18, and adds a deadline to it. design.md section 6.1 says there must be
// one mechanism; it does not say there may be only one trigger, and GOV-B02 (still in Backlog,
// and possibly to be dropped) would set the same field from window focus.
//
// An unknown session is refused rather than invented. A weight with no session attached would
// sit in the table forever, counting toward the global weight in every fair-share walk on
// behalf of nothing, and there is no event that would ever remove it.
func (t *Table) Boost(session string, weight int, until time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, ok := t.sessions[session]
	if !ok {
		return false
	}
	if weight < 1 {
		weight = 1
	}
	s.Weight, s.BoostedUntil = weight, until
	t.sessions[session] = s
	return true
}

// WeightOf is how much budget a request will contend for, in slots, for a caller that needs
// to REPORT that number rather than compare it.
//
// It exists because GOV-34 made a refusal name the weight the request asked for — without it
// "1/4 build slots" is a sentence that refutes itself — and the number a refusal quotes has to
// be the number the rule used. Re-deriving it at the call site is how a refusal ends up
// quoting 0 for the pre-GOV-15 request shape that the table reads as a whole slot.
func WeightOf(r Request) float64 { return normalWeight(r.Weight) }

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
	if ok, _ := t.fitsLocked(r.Session, w, limit); !ok {
		// A refusal is the only way a session's unmet demand becomes visible to anyone but
		// itself, so it is recorded here and nowhere else. Fits MUST NOT record it: the
		// advisory channel is called speculatively and would otherwise let a model inflate
		// its own share by asking questions (design.md section 8).
		t.demanding[r.Session] = t.opt.Now()
		return Lease{}, false
	}
	// Satisfied for now. The mark is left to decay rather than cleared, because a session
	// that just took a slot in a build loop still wants the next one, and clearing here would
	// make its demand flicker in and out of view once per acquire.

	return t.grantLocked(r, w), true
}

// Take grants a lease without consulting the budget at all.
//
// IT IS THE PAUSED PATH OF design.md SECTION 9.1 AND NOTHING ELSE SHOULD USE IT. A pause means
// a person has said "stop capping me"; the lease is still taken so that the table, the status
// line and the reaper go on telling the truth about what is running, and only the admission
// test is skipped. Everything downstream of the grant — TTL, liveness, release, the weight
// total — is identical to Acquire's, because it is the same code.
//
// It records no demand. Demand is what a REFUSAL reveals (section 6.1), and this path cannot
// refuse; marking a session as demanding here would inflate its fair share for thirty seconds
// after the pause ended, letting a pause quietly buy an advantage that outlives it.
func (t *Table) Take(r Request) Lease {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.grantLocked(r, normalWeight(r.Weight))
}

// grantLocked mints the lease. It is the whole of the grant, shared by Acquire and Take so
// that the paused path cannot drift from the governed one.
func (t *Table) grantLocked(r Request, w float64) Lease {
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
	return *l
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
		delete(t.demanding, id)
	}
	// Expired demand marks, for the same reason denyLog is swept from the reaper: otherwise
	// this map grows with every session that was ever refused and never shrinks.
	for id, at := range t.demanding {
		if now.Sub(at) > demandWindow {
			delete(t.demanding, id)
		}
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
		// Expired here too, not only on the admission path: see expireBoostLocked.
		per[id] = &SessionStat{Session: t.expireBoostLocked(sess)}
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
