package lease

import (
	"testing"
	"time"
)

// GOV-B56. A BACKGROUND subagent (run_in_background: true on the Agent call) fires
// SubagentStop every time it ends a turn, not only when it is finished. If it left a
// background task running, that task's end wakes it: a second SubagentStart with the SAME
// agent_id and no PreToolUse anywhere (hook-surface.md section 29, probes S1 and S2). The
// idle SubagentStop has already released its lease, so before GOV-B56 the wake's confirm
// found nothing pending and the subagent worked on with no lease. 36 of the 37 wakes on this
// machine to 2026-11-23 ran that way, about 1.5 hours of unleased subagent work.
//
// heldBy counts the subagents holding a confirmed lease, by agent_id.
func heldBy(tbl *Table) map[string]int {
	out := map[string]int{}
	for _, l := range tbl.Leases() {
		if l.Kind == Agent && l.State == Confirmed && l.AgentID != "" {
			out[l.AgentID]++
		}
	}
	return out
}

// This replays edgefinder session b961a293's subagent a3a16c4a, 2026-11-23, with the reaper
// ticking every 2 s: confirmed at 20:16:07, idle SubagentStop at 20:26:55 (it had just
// backgrounded `sleep 40`), woken by that task at 20:27:23, then three more idle stops and
// wakes 4 to 44 s apart, then its final stop. On 7b9b169 every wake answered no lease.
//
// It uses only the API that existed at 7b9b169, so it compiles there and fails there.
func TestABackgroundSubagentWokenByItsOwnTaskIsLeasedAgain(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now, Liveness: liveLiveness{}})
	tbl.StartSession(Session{ID: "s1", PID: 4242})
	const agent = "a3a16c4a4f889fb0f"

	wait := func(d time.Duration) {
		for ; d > 0; d -= 2 * time.Second {
			step := 2 * time.Second
			if d < step {
				step = d
			}
			c.advance(step)
			tbl.Reap()
		}
	}

	if _, ok := tbl.Acquire(Request{Session: "s1", Kind: Agent, Weight: 0.08}, 4); !ok {
		t.Fatal("acquire refused on an idle table")
	}
	if _, ok := tbl.Confirm("s1", "", agent, 4242, c.now()); !ok {
		t.Fatal("the spawn's own SubagentStart found no lease")
	}

	// Each (working, idle) pair: how long the subagent ran before an idle SubagentStop, and
	// how long it stayed idle before its task woke it. Taken from the transcript.
	cycles := []struct{ working, idle time.Duration }{
		{648 * time.Second, 28 * time.Second}, // 20:16:07 -> 20:26:55 -> 20:27:23
		{6 * time.Second, 4 * time.Second},    // -> 20:27:29 -> 20:27:33
		{22 * time.Second, 44 * time.Second},  // -> 20:27:55 -> 20:28:39
	}
	for i, cy := range cycles {
		wait(cy.working)
		if _, ok := tbl.Release("s1", agent); !ok {
			t.Fatalf("cycle %d: the idle SubagentStop found no lease to release", i+1)
		}
		if n := heldBy(tbl)[agent]; n != 0 {
			t.Fatalf("cycle %d: the agent still holds %d lease(s) while idle, want 0", i+1, n)
		}
		wait(cy.idle)
		if _, ok := tbl.Confirm("s1", "", agent, 4242, c.now()); !ok {
			t.Fatalf("cycle %d: the woken subagent's SubagentStart answered no lease; it runs ungoverned", i+1)
		}
		if n := heldBy(tbl)[agent]; n != 1 {
			t.Fatalf("cycle %d: the woken subagent holds %d leases, want exactly 1", i+1, n)
		}
	}

	// Its final SubagentStop leaves nothing behind.
	wait(336 * time.Second) // -> 20:34:15
	tbl.Release("s1", agent)
	tbl.Release("s1", agent) // SubagentStop fired twice at the end, 26 s apart
	if n := tbl.InUse(); n != 0 {
		t.Fatalf("in use %d after the subagent's final stop, want 0", n)
	}
}

// The wake must not take a lease admitted for a different spawn. Here a new spawn's
// PreToolUse comes while the background subagent is idle, and the subagent wakes before the
// new spawn's SubagentStart arrives. On 7b9b169 the wake popped the FIFO and took the new
// spawn's lease, which then started with nothing: one of the two ran unleased either way.
func TestAWakeDoesNotTakeTheLeaseOfASpawnStillToStart(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now, Liveness: liveLiveness{}})
	tbl.StartSession(Session{ID: "s1", PID: 4242})

	if _, ok := tbl.Acquire(Request{Session: "s1", Kind: Agent, Weight: 0.08}, 4); !ok {
		t.Fatal("acquire refused on an idle table")
	}
	tbl.Confirm("s1", "", "ag_bg", 4242, c.now())
	c.advance(time.Minute)
	tbl.Release("s1", "ag_bg") // idle stop

	if _, ok := tbl.Acquire(Request{Session: "s1", Kind: Agent, Weight: 0.08}, 4); !ok {
		t.Fatal("the new spawn's acquire was refused")
	}
	c.advance(time.Second)
	if _, ok := tbl.Confirm("s1", "", "ag_bg", 4242, c.now()); !ok { // the wake comes first
		t.Error("the woken subagent got no lease")
	}
	if _, ok := tbl.Confirm("s1", "", "ag_new", 4242, c.now()); !ok {
		t.Error("the new spawn found no lease: the wake took the one the gate admitted for it")
	}
	if got := heldBy(tbl); got["ag_bg"] != 1 || got["ag_new"] != 1 || len(got) != 2 {
		t.Fatalf("leases by agent = %v, want one each for ag_bg and ag_new", got)
	}
}

// A SendMessage resume takes its tentative lease for one named subagent, and Confirm hands it
// to that subagent only. FIFO passes over it in both orders, and the resumed agent does not
// get a second, minted lease beside it.
func TestAResumeLeaseBindsToTheAgentItWasTakenFor(t *testing.T) {
	for _, resumeFirst := range []bool{true, false} {
		c := newClock()
		tbl := NewTable(Options{Now: c.now, Liveness: liveLiveness{}})
		tbl.StartSession(Session{ID: "s1", PID: 4242})

		tbl.Acquire(Request{Session: "s1", Kind: Agent, Weight: 0.08}, 4)
		tbl.Confirm("s1", "", "ag_x", 4242, c.now())
		tbl.Release("s1", "ag_x") // finished

		resume := func() {
			if _, ok := tbl.Acquire(Request{Session: "s1", Kind: Agent, Weight: 0.08, Resume: "ag_x"}, 4); !ok {
				t.Fatal("the resume's acquire was refused")
			}
		}
		spawn := func() {
			if _, ok := tbl.Acquire(Request{Session: "s1", Kind: Agent, Weight: 0.08}, 4); !ok {
				t.Fatal("the new spawn's acquire was refused")
			}
		}
		if resumeFirst {
			resume()
			spawn()
		} else {
			spawn()
			resume()
		}

		y, ok := tbl.Confirm("s1", "", "ag_y", 4242, c.now())
		if !ok || y.Resume != "" {
			t.Errorf("resumeFirst=%v: the new spawn got %+v, ok=%v; want its own untargeted lease", resumeFirst, y, ok)
		}
		x, ok := tbl.Confirm("s1", "", "ag_x", 4242, c.now())
		if !ok || x.Resume != "ag_x" || x.Woken {
			t.Errorf("resumeFirst=%v: the resumed agent got %+v, ok=%v; want the lease its resume took", resumeFirst, x, ok)
		}
		if n := len(tbl.Leases()); n != 2 {
			t.Errorf("resumeFirst=%v: %d leases held, want 2: no orphaned tentative lease, no minted extra", resumeFirst, n)
		}
	}
}

// What stays as it was: a SubagentStart for an agent this table never governed, with
// nothing pending or lapsed, still answers no lease (design.md section 4.3), and a session's
// end forgets the agents it governed.
func TestOnlyAGovernedAgentIsLeasedOnWake(t *testing.T) {
	c := newClock()
	tbl := NewTable(Options{Now: c.now})
	tbl.StartSession(Session{ID: "s1", PID: 4242})

	if _, ok := tbl.Confirm("s1", "", "ag_stranger", 4242, c.now()); ok {
		t.Error("an agent never governed, with nothing pending, was leased")
	}

	tbl.Acquire(Request{Session: "s1", Kind: Agent}, 4)
	tbl.Confirm("s1", "", "ag_1", 4242, c.now())
	tbl.Release("s1", "ag_1")
	if _, ok := tbl.Confirm("s2", "", "ag_1", 4242, c.now()); ok {
		t.Error("a confirm from another session was leased for this session's agent")
	}
	tbl.ReleaseSession("s1")
	if _, ok := tbl.Confirm("s1", "", "ag_1", 4242, c.now()); ok {
		t.Error("an agent of an ended session was leased on wake")
	}
}

// The second half (GOV-B56): a background subagent's idle SubagentStop must not release the
// backgrounded builds it lists as still running. They run on (hook-surface.md section 29,
// 40 of 40 heartbeats in S1); a foreground subagent's are killed, and keep is nil for it.
func TestReleaseBackgroundOfKeepsTasksStillRunning(t *testing.T) {
	tbl := NewTable(Options{})
	build := func(tool, task string) {
		tbl.Acquire(Request{Session: "s1", Kind: Build, ToolUseID: tool}, 4)
		if !tbl.MarkBackground("s1", tool, task, "ag_bg") {
			t.Fatalf("MarkBackground %s found no lease", tool)
		}
	}
	build("toolu_1", "bkeep")
	build("toolu_2", "bdone")

	got := tbl.ReleaseBackgroundOf("s1", "ag_bg", []string{"bkeep"})
	if len(got) != 1 || got[0].Lease.BackgroundTask != "bdone" {
		t.Fatalf("idle stop released %+v, want only the task no longer listed as running", got)
	}
	if left := tbl.BackgroundTasks("s1"); len(left) != 1 || left[0] != "bkeep" {
		t.Fatalf("background tasks left = %v, want [bkeep]", left)
	}
	if got := tbl.ReleaseBackgroundOf("s1", "ag_bg", nil); len(got) != 1 {
		t.Fatalf("a stop listing nothing running released %d, want the last one", len(got))
	}
}
