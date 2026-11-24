package lease

import (
	"fmt"
	"testing"
	"time"
)

// GOV-B55. In auto permission mode Claude Code runs PreToolUse for each Agent block as it
// streams, but holds every tool in the message until the message has finished streaming
// (hook-surface.md section 28). A spawn's SubagentStart can therefore come later than the
// tentative TTL, by however long the rest of the message took to write.
//
// This replays the measured case, edgefinder session b961a293 on 2026-11-23: three Agent
// spawns in one message at +0 s, +16 s and +33 s, every SubagentStart at +34.9 s, the reaper
// ticking every 2 s against the real 30 s tentative TTL. On the code before GOV-B55 the first
// lease was reaped at +30 s and the third confirm answered no_pending_lease, so one subagent
// ran with no lease.
//
// It counts leased subagents rather than matching lease IDs. Confirm binds per-session FIFO,
// so the subagent left unleased was never necessarily the one whose lease lapsed.
func TestAConfirmLaterThanTheTentativeTTLStillLeasesEverySpawn(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now, Liveness: liveLiveness{}})
	tbl.StartSession(Session{ID: "s1", PID: 4242})

	start := c.now()
	spawnsAt := []time.Duration{0, 16 * time.Second, 33 * time.Second}
	const startsAt = 34900 * time.Millisecond
	const tick = 2 * time.Second

	acquired := 0
	for elapsed := time.Duration(0); elapsed < startsAt; elapsed = c.now().Sub(start) {
		for acquired < len(spawnsAt) && spawnsAt[acquired] <= elapsed {
			if _, ok := tbl.Acquire(Request{Session: "s1", Kind: Agent, Weight: 0.08}, 4); !ok {
				t.Fatalf("acquire %d refused on an idle table", acquired+1)
			}
			acquired++
		}
		tbl.Reap()
		step := tick
		if left := startsAt - elapsed; left < step {
			step = left
		}
		c.advance(step)
	}
	if acquired != len(spawnsAt) {
		t.Fatalf("acquired %d spawns, want %d", acquired, len(spawnsAt))
	}

	for i := range spawnsAt {
		agent := fmt.Sprintf("ag_%d", i+1)
		if _, ok := tbl.Confirm("s1", "", agent, 4242, start); !ok {
			t.Errorf("confirm for %s answered no lease: a spawn the gate admitted runs ungoverned", agent)
		}
	}

	leased := map[string]bool{}
	for _, l := range tbl.Leases() {
		if l.Kind == Agent && l.State == Confirmed && l.AgentID != "" {
			leased[l.AgentID] = true
		}
	}
	if len(leased) != len(spawnsAt) {
		t.Fatalf("%d of %d subagents hold a lease after their SubagentStarts, want all of them: %v",
			len(leased), len(spawnsAt), leased)
	}

	// Each subagent's lease is released by its own SubagentStop, as an on-time one would be.
	for agent := range leased {
		if _, ok := tbl.Release("s1", agent); !ok {
			t.Errorf("release of %s found no lease", agent)
		}
	}
	if n := tbl.InUse(); n != 0 {
		t.Fatalf("in use %d after every subagent stopped, want 0", n)
	}
}

// The tentative TTL keeps its job (GOV-09): a spawn that never happened gives its slot back
// at 30 s. Waiting for a late SubagentStart must cost no budget, or every refused Agent call
// would hold its slot for the whole late-confirm window instead.
func TestALapsedSpawnHoldsNoBudgetWhileItWaits(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now})

	if _, ok := tbl.Acquire(Request{Session: "s1", Kind: Agent}, 1); !ok {
		t.Fatal("acquire refused on an idle table")
	}
	c.advance(DefaultTentativeTTL + time.Second)
	if n := len(tbl.Reap()); n != 1 {
		t.Fatalf("reaped %d, want the one unconfirmed tentative lease", n)
	}
	if n := tbl.InUse(); n != 0 {
		t.Fatalf("in use %d after the tentative lease lapsed, want 0", n)
	}
	if _, ok := tbl.Acquire(Request{Session: "s2", Kind: Agent}, 1); !ok {
		t.Fatal("a budget of 1 refused another session while a lapse was waiting: the lapse holds budget")
	}
}

// A lapse is claimable only inside lateConfirmWindow. After that the spawn is taken to have
// been abandoned, and a confirm finds nothing, as a resumed subagent's always has.
func TestALapseOlderThanTheWindowIsNotReinstated(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now})

	tbl.Acquire(Request{Session: "s1", Kind: Agent}, 4)
	c.advance(DefaultTentativeTTL + time.Second)
	tbl.Reap()
	c.advance(lateConfirmWindow + time.Second)
	if _, ok := tbl.Confirm("s1", "", "ag_1", 1, time.Time{}); ok {
		t.Fatal("a confirm past the late-confirm window reinstated a lapse")
	}
	if n := tbl.InUse(); n != 0 {
		t.Fatalf("in use %d, want 0", n)
	}
}

// The window is swept by the reaper too, so a session that never confirms does not keep a
// lapse in memory for the life of the daemon.
func TestTheReaperSweepsUnclaimedLapses(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now})

	tbl.Acquire(Request{Session: "s1", Kind: Agent}, 4)
	c.advance(DefaultTentativeTTL + time.Second)
	tbl.Reap()
	if len(tbl.lapsed["s1"]) != 1 {
		t.Fatalf("lapsed = %v, want one lapse for s1", tbl.lapsed)
	}
	c.advance(lateConfirmWindow + time.Second)
	tbl.Reap()
	if len(tbl.lapsed) != 0 {
		t.Fatalf("lapsed = %v after the window, want it swept", tbl.lapsed)
	}
}

// Only a spawn the gate admitted can be reinstated. A session with no lapse still answers a
// confirm with nothing, so a resumed subagent stays ungoverned exactly as before (design.md
// section 4.3), and one session's lapse is never claimed by another's confirm.
func TestALapseIsClaimedOnlyBySpawnsOfItsOwnSession(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now})

	tbl.Acquire(Request{Session: "s1", Kind: Agent}, 4)
	c.advance(DefaultTentativeTTL + time.Second)
	tbl.Reap()

	if _, ok := tbl.Confirm("s2", "", "ag_other", 1, time.Time{}); ok {
		t.Fatal("a confirm in another session claimed s1's lapse")
	}
	l, ok := tbl.Confirm("s1", "", "ag_1", 1, time.Time{})
	if !ok {
		t.Fatal("s1's own late confirm was not reinstated")
	}
	if l.LapsedAt.IsZero() {
		t.Error("a reinstated lease does not say when it lapsed")
	}
	if _, ok := tbl.Confirm("s1", "", "ag_2", 1, time.Time{}); ok {
		t.Fatal("one lapse was reinstated twice")
	}
}

// A lapse belongs to a live session. SessionEnd forgets it with everything else.
func TestSessionEndForgetsItsLapses(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now})

	tbl.Acquire(Request{Session: "s1", Kind: Agent}, 4)
	c.advance(DefaultTentativeTTL + time.Second)
	tbl.Reap()
	tbl.ReleaseSession("s1")
	if _, ok := tbl.Confirm("s1", "", "ag_1", 1, time.Time{}); ok {
		t.Fatal("a confirm after SessionEnd reinstated a lapse")
	}
}

// A Build lease is born confirmed and has no SubagentStart to wait for, so its expiry is
// never a lapse.
func TestAnExpiredBuildLeaseIsNotALapse(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now})

	tbl.Acquire(Request{Session: "s1", Kind: Build, ToolUseID: "toolu_1"}, 4)
	c.advance(DefaultBuildTTL + time.Second)
	tbl.Reap()
	if len(tbl.lapsed) != 0 {
		t.Fatalf("lapsed = %v, want none from a Build lease", tbl.lapsed)
	}
}
