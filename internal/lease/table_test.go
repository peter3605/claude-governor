package lease

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a hand-wound time source, so the reaper can be tested without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// deadLiveness reports every lease's owner as gone.
type deadLiveness struct{}

func (deadLiveness) Alive(Lease) bool { return false }

// liveLiveness reports every owner as running.
type liveLiveness struct{}

func (liveLiveness) Alive(Lease) bool { return true }

func TestLifecycleTentativeConfirmedReleased(t *testing.T) {
	tbl := NewTable(Options{})

	l, ok := tbl.Acquire("s1", 0, 4)
	if !ok {
		t.Fatal("acquire refused with an empty table")
	}
	if l.State != Tentative {
		t.Fatalf("a fresh lease is %v, want Tentative", l.State)
	}
	if tbl.InUse() != 1 {
		t.Fatalf("in use %d after acquire, want 1 — a tentative lease must hold its slot", tbl.InUse())
	}

	c, ok := tbl.Confirm("s1", l.ID, "ag_1", 4242, time.Unix(1_700_000_000, 0))
	if !ok {
		t.Fatal("confirm of a tentative lease was refused")
	}
	if c.State != Confirmed || c.AgentID != "ag_1" || c.OwnerPID != 4242 {
		t.Fatalf("confirmed lease is %+v, want state Confirmed, agent ag_1, pid 4242", c)
	}
	if tbl.InUse() != 1 {
		t.Fatalf("in use %d after confirm, want 1 — confirming must not create a second lease", tbl.InUse())
	}

	if !tbl.Release("s1", "ag_1") {
		t.Fatal("release of a confirmed lease reported nothing held")
	}
	if tbl.InUse() != 0 {
		t.Fatalf("in use %d after release, want 0", tbl.InUse())
	}
}

// The budget is a ceiling, not a suggestion. This is the single-threaded statement of the
// invariant; TestLeaseCountNeverExceedsBudget is the concurrent one.
func TestAcquireStopsAtTheLimit(t *testing.T) {
	tbl := NewTable(Options{})
	for i := range 3 {
		if _, ok := tbl.Acquire("s1", 0, 3); !ok {
			t.Fatalf("acquire %d of 3 refused", i+1)
		}
	}
	if _, ok := tbl.Acquire("s1", 0, 3); ok {
		t.Fatal("acquire granted a fourth lease against a budget of 3")
	}
	if tbl.InUse() != 3 {
		t.Fatalf("in use %d, want 3", tbl.InUse())
	}
}

// SubagentStart fires again when an agent is resumed (design.md section 4.3). A second
// confirm must be a no-op — not a second lease, and not a second FIFO pop, which would
// silently bind some other pending spawn's lease to this agent.
func TestConfirmIsIdempotentPerAgent(t *testing.T) {
	tbl := NewTable(Options{})
	a, _ := tbl.Acquire("s1", 0, 4)
	b, _ := tbl.Acquire("s1", 0, 4)

	first, ok := tbl.Confirm("s1", "", "ag_1", 1, time.Time{})
	if !ok || first.ID != a.ID {
		t.Fatalf("first confirm bound %q, want the oldest tentative lease %q", first.ID, a.ID)
	}

	again, ok := tbl.Confirm("s1", "", "ag_1", 1, time.Time{})
	if !ok {
		t.Fatal("a repeat confirm for a live agent was refused; resume must be a no-op")
	}
	if again.ID != first.ID {
		t.Fatalf("repeat confirm bound %q, want the lease the agent already holds %q", again.ID, first.ID)
	}
	if tbl.InUse() != 2 {
		t.Fatalf("in use %d after a repeat confirm, want 2 — it must not create a lease", tbl.InUse())
	}

	// The crucial part: b is still tentative and unclaimed. A second FIFO pop would have
	// eaten it here, and the next real agent would have found nothing to bind.
	second, ok := tbl.Confirm("s1", "", "ag_2", 2, time.Time{})
	if !ok || second.ID != b.ID {
		t.Fatalf("second agent bound %q, want the still-pending lease %q", second.ID, b.ID)
	}
}

// A resumed subagent reaches the CPU with no PreToolUse at all, so there is no tentative
// lease to bind. That is not an error and must not consume anything.
func TestConfirmWithNoPendingLeaseIsDroppedQuietly(t *testing.T) {
	tbl := NewTable(Options{})
	if _, ok := tbl.Confirm("s1", "", "ag_ghost", 1, time.Time{}); ok {
		t.Fatal("confirm succeeded with an empty pending queue")
	}
	if tbl.InUse() != 0 {
		t.Fatalf("in use %d, want 0 — a dropped confirm must not create a lease", tbl.InUse())
	}
}

// Confirms bind the oldest unconfirmed tentative lease for that session, and sessions do
// not borrow from each other.
func TestConfirmIsPerSessionFIFO(t *testing.T) {
	tbl := NewTable(Options{})
	a1, _ := tbl.Acquire("s1", 0, 8)
	b1, _ := tbl.Acquire("s2", 0, 8)
	a2, _ := tbl.Acquire("s1", 0, 8)

	got, _ := tbl.Confirm("s1", "", "ag_a", 1, time.Time{})
	if got.ID != a1.ID {
		t.Fatalf("s1 bound %q, want its oldest %q", got.ID, a1.ID)
	}
	got, _ = tbl.Confirm("s1", "", "ag_b", 2, time.Time{})
	if got.ID != a2.ID {
		t.Fatalf("s1 bound %q, want its next %q", got.ID, a2.ID)
	}
	got, _ = tbl.Confirm("s2", "", "ag_c", 3, time.Time{})
	if got.ID != b1.ID {
		t.Fatalf("s2 bound %q, want its own %q — a session must not take another's lease", got.ID, b1.ID)
	}
}

// SubagentStop fires more than once for the same agent.
func TestReleaseToleratesRepeats(t *testing.T) {
	tbl := NewTable(Options{})
	l, _ := tbl.Acquire("s1", 0, 4)
	tbl.Confirm("s1", l.ID, "ag_1", 1, time.Time{})

	if !tbl.Release("s1", "ag_1") {
		t.Fatal("first release reported nothing held")
	}
	if tbl.Release("s1", "ag_1") {
		t.Fatal("second release claimed to free a lease that was already gone")
	}
	if tbl.InUse() != 0 {
		t.Fatalf("in use %d, want 0", tbl.InUse())
	}
}

// A release naming the wrong session must change nothing at all. Freeing the lease would
// return a slot that is still in use; dropping the agent index instead would leave a live
// lease that no later release could ever reach.
func TestReleaseFromTheWrongSessionChangesNothing(t *testing.T) {
	tbl := NewTable(Options{})
	l, _ := tbl.Acquire("s1", 0, 4)
	tbl.Confirm("s1", l.ID, "ag_1", 1, time.Time{})

	if tbl.Release("s2", "ag_1") {
		t.Fatal("a release from another session freed the lease")
	}
	if tbl.InUse() != 1 {
		t.Fatalf("in use %d after a mismatched release, want 1", tbl.InUse())
	}
	// The owning session can still release it: the index must have survived.
	if !tbl.Release("s1", "ag_1") {
		t.Fatal("the owning session could no longer release its own lease")
	}
	if tbl.InUse() != 0 {
		t.Fatalf("in use %d, want 0", tbl.InUse())
	}
}

// A confirm with no agent id still takes the slot: the spawn has happened and refusing
// would under-count the budget. It simply has no name to be released by, and the reaper is
// what collects it.
func TestConfirmWithNoAgentIDStillHoldsItsSlot(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{ConfirmedTTL: time.Minute, Now: c.now})
	l, _ := tbl.Acquire("s1", 0, 4)

	got, ok := tbl.Confirm("s1", l.ID, "", 1, time.Time{})
	if !ok {
		t.Fatal("confirm with no agent id was refused; the slot is genuinely in use")
	}
	if got.State != Confirmed {
		t.Fatalf("lease state %v, want Confirmed", got.State)
	}
	if tbl.InUse() != 1 {
		t.Fatalf("in use %d, want 1", tbl.InUse())
	}
	c.advance(2 * time.Minute)
	if n := tbl.Reap(); n != 1 {
		t.Fatalf("reaped %d, want 1 — the TTL is the only thing that can collect it", n)
	}
}

func TestReleaseSessionDropsEverythingItHeld(t *testing.T) {
	tbl := NewTable(Options{})
	tbl.StartSession(Session{ID: "s1", PID: 10})
	a, _ := tbl.Acquire("s1", 0, 8)
	tbl.Acquire("s1", 0, 8)
	tbl.Acquire("s2", 0, 8)
	tbl.Confirm("s1", a.ID, "ag_1", 1, time.Time{})

	if n := tbl.ReleaseSession("s1"); n != 2 {
		t.Fatalf("released %d leases for s1, want 2 (one confirmed, one tentative)", n)
	}
	if tbl.InUse() != 1 {
		t.Fatalf("in use %d, want 1 — s2's lease must survive s1 ending", tbl.InUse())
	}
	// The session's pending queue must go with it, or a later confirm for a dead session
	// would bind a lease that no longer exists.
	if _, ok := tbl.Confirm("s1", "", "ag_late", 1, time.Time{}); ok {
		t.Fatal("confirm bound a lease for a session that had ended")
	}
}

// Invariant 2: leases expire on their own. The stop hooks are assumed never to fire.
func TestReaperCollectsExpiredTentativeLeases(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{TentativeTTL: 30 * time.Second, Now: c.now})

	tbl.Acquire("s1", 0, 4)
	c.advance(29 * time.Second)
	if n := tbl.Reap(); n != 0 {
		t.Fatalf("reaped %d leases inside the TTL, want 0", n)
	}
	c.advance(2 * time.Second)
	if n := tbl.Reap(); n != 1 {
		t.Fatalf("reaped %d leases past the TTL, want 1", n)
	}
	if tbl.InUse() != 0 {
		t.Fatalf("in use %d after reaping, want 0 — an abandoned spawn must not hold a slot forever", tbl.InUse())
	}
	// The reaped lease must leave the pending queue with it, or the next confirm binds a
	// lease that is no longer in the table.
	if _, ok := tbl.Confirm("s1", "", "ag_1", 1, time.Time{}); ok {
		t.Fatal("confirm bound a reaped lease")
	}
}

func TestReaperCollectsConfirmedLeasesWhoseOwnerIsGone(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now, Liveness: deadLiveness{}})

	l, _ := tbl.Acquire("s1", 0, 4)
	tbl.Confirm("s1", l.ID, "ag_1", 4242, c.now())

	if n := tbl.Reap(); n != 1 {
		t.Fatalf("reaped %d leases whose owner is dead, want 1", n)
	}
	if tbl.InUse() != 0 {
		t.Fatalf("in use %d, want 0", tbl.InUse())
	}
}

// Liveness resolves every uncertainty towards "alive", so a lease it vouches for must
// survive right up to its TTL. Reaping a live agent double-counts the budget.
func TestReaperSparesConfirmedLeasesWhoseOwnerIsAlive(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{ConfirmedTTL: time.Hour, Now: c.now, Liveness: liveLiveness{}})

	l, _ := tbl.Acquire("s1", 0, 4)
	tbl.Confirm("s1", l.ID, "ag_1", 4242, c.now())

	c.advance(59 * time.Minute)
	if n := tbl.Reap(); n != 0 {
		t.Fatalf("reaped %d live leases, want 0", n)
	}
	// The TTL is still a backstop: a lost SubagentStop must not hold a slot indefinitely
	// just because its session is still running.
	c.advance(2 * time.Minute)
	if n := tbl.Reap(); n != 1 {
		t.Fatalf("reaped %d leases past the confirmed TTL, want 1", n)
	}
}

// Reclaiming an abandoned spawn's slot is the whole reason the tentative state exists.
func TestReapedTentativeSlotBecomesAvailableAgain(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{TentativeTTL: 30 * time.Second, Now: c.now})

	for range 2 {
		tbl.Acquire("s1", 0, 2)
	}
	if _, ok := tbl.Acquire("s1", 0, 2); ok {
		t.Fatal("acquire exceeded the budget")
	}
	c.advance(31 * time.Second)
	tbl.Reap()
	if _, ok := tbl.Acquire("s1", 0, 2); !ok {
		t.Fatal("the slot an abandoned spawn held was never returned")
	}
}

func TestSnapshotCountsAgreeWithTheirTotal(t *testing.T) {
	tbl := NewTable(Options{})
	tbl.StartSession(Session{ID: "s1", PID: 10})
	tbl.StartSession(Session{ID: "s2", PID: 20, Weight: 3})

	a, _ := tbl.Acquire("s1", 0, 8)
	tbl.Acquire("s1", 0, 8)
	tbl.Acquire("s2", 0, 8)
	tbl.Confirm("s1", a.ID, "ag_1", 1, time.Time{})

	s := tbl.Snapshot()
	if s.InUse != 3 || s.Confirmed != 1 || s.Tentative != 2 {
		t.Fatalf("snapshot totals %+v, want in use 3, confirmed 1, tentative 2", s)
	}
	if len(s.Sessions) != 2 {
		t.Fatalf("snapshot lists %d sessions, want 2", len(s.Sessions))
	}
	if s.Sessions[0].ID != "s1" || s.Sessions[0].Confirmed != 1 || s.Sessions[0].Tentative != 1 {
		t.Fatalf("s1 line is %+v, want 1 confirmed and 1 tentative", s.Sessions[0])
	}
	if s.Sessions[1].Weight != 3 {
		t.Fatalf("s2 weight is %d, want the registered 3", s.Sessions[1].Weight)
	}
	// An unregistered session's weight defaults rather than staying zero.
	if s.Sessions[0].Weight != 1 {
		t.Fatalf("s1 weight is %d, want the default 1", s.Sessions[0].Weight)
	}
}

// The table's own statement of the budget ceiling under concurrency. Acquire checks the
// limit and inserts under one lock; a version that read the count, decided, and then
// inserted would let every concurrent caller see the same room and all take it.
//
// The assertion reads InUse rather than a counter the test keeps itself. That is not a
// detail: a shadow counter incremented after the grant and decremented before the release
// under-reports (it can miss the violation), and one decremented after the release
// over-reports (a release and an acquire that interleave make it exceed the budget when the
// table never did). InUse is the table's own count under the table's own lock, so a reading
// above the limit is a real violation and a reading below it is not a false alarm.
//
// Each worker reads it, and no separate poller does. A goroutine polling InUse in a tight
// loop would take the table's mutex millions of times a second, contending with the very
// workers it is watching, and it would buy nothing: the count can only rise on an acquire,
// so a reading taken by the worker that just acquired is taken at the moment the peak
// happens.
//
// Run with -race.
func TestLeaseCountNeverExceedsBudgetUnderConcurrency(t *testing.T) {
	const (
		limit   = 4
		workers = 200
	)
	tbl := NewTable(Options{})

	var peak atomic.Int64
	observe := func() {
		n := int64(tbl.InUse())
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				return
			}
		}
	}

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := fmt.Sprintf("s%d", i%4)
			l, ok := tbl.Acquire(session, 0, limit)
			if !ok {
				return
			}
			observe()
			agent := fmt.Sprintf("ag_%d", i)
			tbl.Confirm(session, l.ID, agent, i+1, time.Time{})
			observe()
			tbl.Release(session, agent)
		}()
	}
	wg.Wait()
	observe()

	if got := peak.Load(); got > limit {
		t.Fatalf("as many as %d leases were held at once against a budget of %d", got, limit)
	}
	if got := peak.Load(); got == 0 {
		t.Fatal("no lease was ever observed to be held; the test proved nothing")
	}
	if tbl.InUse() != 0 {
		t.Fatalf("in use %d after every worker released, want 0", tbl.InUse())
	}
}
