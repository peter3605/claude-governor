package lease

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The measured case (GOV-B50). Session 1becacfd's implementer "Fix round 2 for T-047" was
// granted at 19:45:51 on 11-21, ran for 78.2 min, and its SubagentStop at 21:04:03 found no
// lease: the reaper had released it at 20:15:52, 30 min in, while it was still working.
const (
	measuredSession = "1becacfd-6788-4e52-ab2f-d442d19187c5"
	measuredAgent   = "accad2351131e65b0"
)

// writeAgent lays out a subagent's two files the way Claude Code does, under root, and returns
// the transcript's path. meta is the meta.json body.
func writeAgent(t *testing.T, root, session, agent, meta string) string {
	t.Helper()
	dir := filepath.Join(root, "-Users-peterho-Repos-stepstool", session, "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-"+agent+".meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	jl := filepath.Join(dir, "agent-"+agent+".jsonl")
	if err := os.WriteFile(jl, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return jl
}

// touch sets a transcript's mtime, standing in for the subagent writing to it.
func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

const (
	metaRunning  = `{"agentType":"implementer","description":"Fix round 2 for T-047","toolUseId":"toolu_x","spawnDepth":1,"requestShape":"foreground"}`
	metaFinished = `{"agentType":"implementer","description":"Fix round 2 for T-047","toolUseId":"toolu_x","spawnDepth":1,"requestShape":"foreground","finished":true}`
)

func measuredTable(t *testing.T, root string, c *clock) (*Table, Lease) {
	t.Helper()
	tbl := NewTable(Options{Now: c.now, Liveness: PIDLiveness{projectsRoot: root, taskRoot: t.TempDir()}})
	l, ok := tbl.Acquire(Request{Session: measuredSession, Weight: 0.08}, 4)
	if !ok {
		t.Fatal("acquire refused on an empty table")
	}
	// No PID: the owner check answers "alive" and leaves the lease to its TTL, so the test
	// isolates the TTL path this story is about.
	if _, ok := tbl.Confirm(measuredSession, l.ID, measuredAgent, 0, time.Time{}); !ok {
		t.Fatal("confirm refused")
	}
	return tbl, l
}

// A subagent still running at the TTL keeps its lease, for as long as it keeps working, and
// SubagentStop returns it at its real end. Against the code before GOV-B50 this fails at the
// first Reap past 30 min, which released the lease with Why "expired".
func TestAStillWorkingSubagentKeepsItsLeasePastTheTTL(t *testing.T) {
	root := t.TempDir()
	c := newClock()
	jl := writeAgent(t, root, measuredSession, measuredAgent, metaRunning)
	tbl, _ := measuredTable(t, root, c)

	// 78 minutes, the subagent writing to its transcript every minute, the reaper every 2 s
	// as in the daemon. Nothing may be released.
	for elapsed := time.Duration(0); elapsed < 78*time.Minute; elapsed += 2 * time.Second {
		c.advance(2 * time.Second)
		if elapsed%time.Minute == 0 {
			touch(t, jl, c.now())
		}
		if gone := tbl.Reap(); len(gone) != 0 {
			t.Fatalf("at %v the reaper released a subagent still writing its transcript: %+v", elapsed, gone)
		}
	}
	if tbl.InUse() != 1 {
		t.Fatalf("in use %d at minute 78, want 1", tbl.InUse())
	}
	// Its end. The meta file is finished at SubagentStop, and the release is SubagentStop's.
	if _, ok := tbl.Release(measuredSession, measuredAgent); !ok {
		t.Fatal("SubagentStop at minute 78 found no lease — the measured failure")
	}
}

// The TTL is still the backstop for every way of not knowing. Each of these is reaped at the
// TTL exactly as before GOV-B50.
func TestAnAgentLeaseWithoutEvidenceOfWorkIsStillReapedAtTheTTL(t *testing.T) {
	cases := []struct {
		name string
		meta string // "" lays out no files at all
		// write is whether the transcript is written after the grant.
		write bool
	}{
		// 11-19: a sleep killed the subagent mid-response, no SubagentStop arrived, and the
		// meta file was finished anyway. This is the one lost hook the TTL exists for.
		{"finished without a SubagentStop", metaFinished, true},
		{"no files: another CLAUDE_CONFIG_DIR, or a Claude Code that keeps them elsewhere", "", false},
		{"meta file not the measured shape", `not json`, true},
		{"unfinished but the transcript has not been written since the grant", metaRunning, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			c := newClock()
			if tc.meta != "" {
				jl := writeAgent(t, root, measuredSession, measuredAgent, tc.meta)
				at := c.now().Add(-time.Minute)
				if tc.write {
					at = c.now().Add(5 * time.Minute)
				}
				touch(t, jl, at)
			}
			tbl, _ := measuredTable(t, root, c)

			c.advance(DefaultConfirmedTTL - time.Second)
			if gone := tbl.Reap(); len(gone) != 0 {
				t.Fatalf("released before the TTL: %+v", gone)
			}
			c.advance(2 * time.Second)
			gone := tbl.Reap()
			if len(gone) != 1 || gone[0].Why != "expired" {
				t.Fatalf("at the TTL the reaper released %+v, want the one lease, why expired", gone)
			}
		})
	}
}

// A renewal is not a lease for life. A subagent that stops writing — hung, or its meta file
// never finished — is renewed once for the work it did and then reaped a TTL later.
func TestARenewedLeaseIsReapedWhenItsSubagentStopsWriting(t *testing.T) {
	root := t.TempDir()
	c := newClock()
	jl := writeAgent(t, root, measuredSession, measuredAgent, metaRunning)
	tbl, _ := measuredTable(t, root, c)

	c.advance(10 * time.Minute)
	touch(t, jl, c.now()) // its last write
	c.advance(DefaultConfirmedTTL - 10*time.Minute + time.Second)
	if gone := tbl.Reap(); len(gone) != 0 {
		t.Fatalf("a subagent that wrote since its grant was released at the first TTL: %+v", gone)
	}
	c.advance(DefaultConfirmedTTL + time.Second)
	gone := tbl.Reap()
	if len(gone) != 1 || gone[0].Why != "expired" {
		t.Fatalf("a TTL with no writes after renewal released %+v, want the one lease, why expired", gone)
	}
	if gone[0].RenewedAt.IsZero() {
		t.Error("the released lease does not record its renewal")
	}
}

// The machine sleeps with the subagent mid-work. The lease's clock stops, the wall does not,
// and the transcript's last write is from before the sleep. Measured three times on 11-22,
// where subagents lived 164 to 257 wall-clock minutes across sleeps. It is renewed.
func TestASubagentThatSleptThroughItsTTLIsRenewed(t *testing.T) {
	root := t.TempDir()
	c := newClock()
	jl := writeAgent(t, root, measuredSession, measuredAgent, metaRunning)
	tbl, l := measuredTable(t, root, c)

	touch(t, jl, l.GrantedAt.Add(20*time.Minute))
	// Monotonic time reaches the TTL; the wall is hours on. The fake clock is wall and lease
	// clock at once, so put the write well before "now" to stand for the sleep.
	c.advance(3 * time.Hour)
	if gone := tbl.Reap(); len(gone) != 0 {
		t.Fatalf("a subagent that wrote since its grant was released after a sleep: %+v", gone)
	}
}

// Only a confirmed Agent lease is ever renewed. A build's TTL is section 5.6's measured
// control constant, and a tentative lease has no subagent to ask about.
func TestWorkingIsOnlyAskedAboutConfirmedAgentLeases(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, measuredSession, measuredAgent, metaRunning)
	v := PIDLiveness{projectsRoot: root}
	base := Lease{Session: measuredSession, AgentID: measuredAgent, GrantedAt: time.Unix(0, 0)}

	ok := base
	ok.Kind, ok.State = Agent, Confirmed
	if !v.Working(ok) {
		t.Fatal("the measured running subagent was not reported working")
	}
	for name, l := range map[string]Lease{
		"build":        func() Lease { b := ok; b.Kind = Build; return b }(),
		"tentative":    func() Lease { b := ok; b.State = Tentative; return b }(),
		"unsafe agent": func() Lease { b := ok; b.AgentID = "../x"; return b }(),
		"no agent":     func() Lease { b := ok; b.AgentID = ""; return b }(),
	} {
		if v.Working(l) {
			t.Errorf("%s: reported working", name)
		}
	}
}
