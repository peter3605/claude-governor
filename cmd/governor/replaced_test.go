package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
)

// GOV-48. Observed on 2026-11-01 straight after a deliberate daemon restart: `governor
// status` printed
//
//	session 7cea028a... confirmed 1 ungoverned
//
// for the one session on the machine holding a lease, which it could only have got by loading
// the hooks and running the gate. "Ungoverned" means restart this session (GOV-29), so a
// status that says it about a governed session is giving an instruction that is false.

// statusLines renders `governor status` for one daemon's own status reply, as the command
// would print it after reading that reply off the socket.
func statusLines(t *testing.T, d *daemon) string {
	t.Helper()
	raw, err := json.Marshal(d.status(request{V: protocolVersion, Op: "status"}))
	if err != nil {
		t.Fatal(err)
	}
	var reply statusResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printStatus(&out, cachedStatus{At: time.Now(), Reply: reply})
	return out.String()
}

// sessionLine picks the status line for one session out of the report.
func sessionLine(t *testing.T, report, id string) string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "session "+id+" ") {
			return line
		}
	}
	t.Fatalf("no status line for session %s in:\n%s", id, report)
	return ""
}

func TestAReplacedDaemonDoesNotCallAGovernedSessionUngoverned(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	hooksAt := now.Add(-2 * time.Hour)

	// governed started after the hooks were installed and registered with the daemon of the
	// time. Its PID is this test's own process, so the reaper sees it alive throughout.
	governed := signals.LiveSession{
		ID: "7cea028a-0000-4000-8000-000000000001", PID: os.Getpid(),
		Started: now.Add(-time.Hour), Kind: "interactive",
	}
	// neverLoaded is the case GOV-29 is about: open before the hooks existed, ungoverned for
	// life. Nothing this story changes may make it any quieter.
	neverLoaded := signals.LiveSession{
		ID: "416e285a-0000-4000-8000-000000000002", PID: os.Getpid(),
		Started: now.Add(-3 * time.Hour), Kind: "interactive",
	}
	live := []signals.LiveSession{governed, neverLoaded}
	machine := func(d *daemon, startedAt time.Time) {
		d.liveSessions = func() ([]signals.LiveSession, error) { return live, nil }
		d.hooksInstalledAt = func() time.Time { return hooksAt }
		d.started = startedAt
	}

	// The first daemon: governed's SessionStart reaches it.
	first := newDaemon(4, io.Discard)
	machine(first, now.Add(-90*time.Minute))
	first.session(request{V: protocolVersion, Op: "session", Session: governed.ID, Action: "start",
		PID: governed.PID, Weight: 1})
	if line := sessionLine(t, statusLines(t, first), governed.ID); strings.Contains(line, "ungoverned") {
		t.Fatalf("the daemon it registered with already calls it ungoverned: %q", line)
	}

	// The daemon is replaced, as `governor install` does on every upgrade. The new one starts
	// with an empty session table, and SessionStart will never fire for governed again.
	var diag bytes.Buffer
	second := newDaemon(4, &diag)
	machine(second, now)

	t.Run("before it spawns anything it reads as registered with the replaced daemon", func(t *testing.T) {
		line := sessionLine(t, statusLines(t, second), governed.ID)
		if strings.Contains(line, "ungoverned") {
			t.Errorf("a session that registered with the previous daemon is called ungoverned: %q", line)
		}
		// doctor's wording, so the two commands share one vocabulary.
		if !strings.Contains(line, "registered with a daemon that has since been replaced") {
			t.Errorf("line = %q, want it to say the session registered with a replaced daemon", line)
		}
	})

	t.Run("the genuine case still says ungoverned, and why", func(t *testing.T) {
		line := sessionLine(t, statusLines(t, second), neverLoaded.ID)
		if !strings.Contains(line, "ungoverned (started before the hooks were installed)") {
			t.Errorf("a session that never loaded the hooks is not called ungoverned: %q", line)
		}
	})

	// The gate takes a lease for it, which only a loaded hook can do. This is the exact
	// state of the 2026-11-01 report.
	if _, ok := second.table.Acquire(lease.Request{Session: governed.ID, Kind: lease.Agent}, 4); !ok {
		t.Fatal("the replaced daemon refused a lease to a session on an idle machine")
	}
	t.Run("while it holds a lease it is never called ungoverned", func(t *testing.T) {
		line := sessionLine(t, statusLines(t, second), governed.ID)
		if strings.Contains(line, "ungoverned") {
			t.Errorf("a session holding a lease is called ungoverned: %q", line)
		}
	})

	// Clause 5: the confirm hook names the session's process, so the new daemon registers it
	// rather than leaving fair share to be computed over a list that is missing it.
	second.confirm(request{V: protocolVersion, Op: "confirm", Session: governed.ID,
		AgentID: "agent-1", PID: governed.PID})
	t.Run("a confirm re-registers it with the current daemon", func(t *testing.T) {
		var found bool
		for _, s := range second.table.Snapshot().Sessions {
			if s.ID == governed.ID {
				found = s.PID == governed.PID && !s.StartedAt.IsZero()
			}
		}
		if !found {
			t.Fatalf("the confirm did not re-register %s with the replaced daemon", governed.ID)
		}
		line := sessionLine(t, statusLines(t, second), governed.ID)
		if strings.Contains(line, "ungoverned") || strings.Contains(line, "replaced") {
			t.Errorf("a re-registered session still carries a label: %q", line)
		}
		if !strings.Contains(diag.String(), "re-registered") {
			t.Errorf("the daemon did not log the re-registration; diag = %q", diag.String())
		}
	})

	t.Run("and the genuine case is still ungoverned after all of that", func(t *testing.T) {
		line := sessionLine(t, statusLines(t, second), neverLoaded.ID)
		if !strings.Contains(line, "ungoverned") {
			t.Errorf("the session that never loaded the hooks stopped being called ungoverned: %q", line)
		}
	})
}

// A confirm that carries no PID registers nothing. The reaper skips a session with no PID, so
// adopting one would keep it in the table for the rest of the daemon's life.
func TestAConfirmWithoutAPIDDoesNotRegisterTheSession(t *testing.T) {
	d := newDaemon(4, io.Discard)
	d.confirm(request{V: protocolVersion, Op: "confirm", Session: "no-pid", AgentID: "a"})
	for _, s := range d.table.Snapshot().Sessions {
		if s.ID == "no-pid" {
			t.Fatalf("a PID-less confirm registered its session: %+v", s)
		}
	}
}
