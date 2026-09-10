package signals

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// selfProcStart is this test process's own start time, formatted the way the harness writes
// procStart. A registry entry built with it describes a process that really is running,
// which is the only way to test the liveness path without spawning something.
func selfProcStart(t *testing.T) string {
	t.Helper()
	p, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("own process: %v", err)
	}
	ms, err := p.CreateTime()
	if err != nil {
		t.Fatalf("own create time: %v", err)
	}
	return time.UnixMilli(ms).UTC().Format(procStartLayout)
}

// writeEntry drops one <pid>.json into dir.
func writeEntry(t *testing.T, dir string, pid int, e map[string]any) {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	name := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(name, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLiveSessionsReadsTheHarnessRegistry(t *testing.T) {
	dir := t.TempDir()
	self := os.Getpid()
	started := time.Now().Add(-3 * time.Hour).Truncate(time.Millisecond)

	writeEntry(t, dir, self, map[string]any{
		"pid":       self,
		"sessionId": "2bcbd3bd-a469-4be6-add4-e528a09b4e34",
		"cwd":       "/Users/peterho/Repos/claude-governor",
		"startedAt": started.UnixMilli(),
		"procStart": selfProcStart(t),
		"version":   "2.1.267",
		"status":    "busy",
		"kind":      "interactive",
	})

	live, err := LiveSessions(dir)
	if err != nil {
		t.Fatalf("LiveSessions: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d sessions, want 1: %+v", len(live), live)
	}
	s := live[0]
	if s.ID != "2bcbd3bd-a469-4be6-add4-e528a09b4e34" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.PID != self {
		t.Errorf("PID = %d, want %d", s.PID, self)
	}
	if !s.Started.Equal(started) {
		t.Errorf("Started = %v, want %v", s.Started, started)
	}
	if s.CWD != "/Users/peterho/Repos/claude-governor" {
		t.Errorf("CWD = %q", s.CWD)
	}
	if s.Version != "2.1.267" {
		t.Errorf("Version = %q", s.Version)
	}
	if s.Status != "busy" {
		t.Errorf("Status = %q", s.Status)
	}
}

// A missing registry is "unknown", not "zero sessions". The distinction is the whole point:
// a caller that read zero from an absent directory would report a machine full of live
// sessions as empty, and every check built on it would pass.
func TestLiveSessionsDistinguishesAnAbsentRegistryFromAnEmptyOne(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		_, err := LiveSessions(filepath.Join(t.TempDir(), "nope"))
		if !errors.Is(err, ErrNoRegistry) {
			t.Fatalf("err = %v, want ErrNoRegistry", err)
		}
	})
	t.Run("present but empty", func(t *testing.T) {
		live, err := LiveSessions(t.TempDir())
		if err != nil {
			t.Fatalf("LiveSessions: %v", err)
		}
		if len(live) != 0 {
			t.Fatalf("got %d sessions, want 0", len(live))
		}
	})
}

func TestLiveSessionsSkipsWhatIsNotALiveSession(t *testing.T) {
	dir := t.TempDir()
	self := os.Getpid()
	now := time.Now()

	// The one real session, which must survive every distractor below.
	writeEntry(t, dir, self, map[string]any{
		"pid": self, "sessionId": "real", "startedAt": now.UnixMilli(),
		"procStart": selfProcStart(t),
	})

	// A PID that is not running. 0x7FFFFFFF is above every pid_max in use.
	writeEntry(t, dir, 2147483647, map[string]any{
		"pid": 2147483647, "sessionId": "dead", "startedAt": now.UnixMilli(),
		"procStart": now.UTC().Format(procStartLayout),
	})

	// Our own PID, but recorded as having started at a different time: a stale file whose
	// PID the kernel has since handed to something else.
	writeEntry(t, dir, 4242, map[string]any{
		"pid": self, "sessionId": "recycled", "startedAt": now.UnixMilli(),
		"procStart": now.UTC().Add(-72 * time.Hour).Format(procStartLayout),
	})

	// An entry with no session id is not a session anyone can act on.
	writeEntry(t, dir, 4243, map[string]any{"pid": self, "procStart": selfProcStart(t)})

	// Files this must not read: the sibling auth token, and unparseable JSON.
	if err := os.WriteFile(filepath.Join(dir, "5364.abc123.key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "9999.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	live, err := LiveSessions(dir)
	if err != nil {
		t.Fatalf("LiveSessions: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d sessions, want 1: %+v", len(live), live)
	}
	if live[0].ID != "real" {
		t.Errorf("ID = %q, want %q", live[0].ID, "real")
	}
}

// The recycled-PID guard has to be the reason the entry was skipped, not a side effect of
// the PID being dead. This asserts the same PID is accepted with the right start time and
// rejected with the wrong one, so the guard cannot pass by accident.
func TestLiveSessionsGuardsAgainstARecycledPID(t *testing.T) {
	self := os.Getpid()
	now := time.Now()

	for _, tc := range []struct {
		name      string
		procStart string
		want      int
	}{
		{"start time matches the running process", selfProcStart(t), 1},
		{"start time is from a process that has gone", now.UTC().Add(-time.Hour).Format(procStartLayout), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeEntry(t, dir, self, map[string]any{
				"pid": self, "sessionId": "s", "startedAt": now.UnixMilli(),
				"procStart": tc.procStart,
			})
			live, err := LiveSessions(dir)
			if err != nil {
				t.Fatalf("LiveSessions: %v", err)
			}
			if len(live) != tc.want {
				t.Fatalf("got %d sessions, want %d", len(live), tc.want)
			}
		})
	}
}

// procStart is written in UTC. Parsing it as LOCAL time shifts every session by the machine's
// zone offset — enough to place a session that started after the install before it, and report
// a governed session as ungoverned for life.
//
// It asserts against a FIXED string and a fixed expected instant rather than round-tripping
// through selfProcStart, which formats with .UTC() and so would only ever check the parser
// against the test's own assumption. Mutating time.UTC to time.Local in sessions.go must turn
// this red, and it did not when the assertion was a round trip and CI ran in UTC — the one
// test named for the hazard was the one test that could not catch it on the machine that gates
// the pull request.
func TestProcStartIsParsedAsUTCAndNotAsLocalTime(t *testing.T) {
	const written = "Wed Sep  9 23:54:48 2026" // as the harness writes it: UTC
	want := time.Date(2026, 9, 9, 23, 54, 48, 0, time.UTC)

	// The test supplies its own non-UTC local zone rather than trusting the machine's.
	//
	// Without this the test cannot fail on a host whose local zone IS UTC, because there
	// time.Local and time.UTC are the same location and a time.Local mutation has no
	// observable effect at all. CI runs ubuntu-latest, which is UTC — so the one test named
	// for this hazard was the one test guaranteed not to catch it on the machine that gates
	// the pull request. Verified by mutation: with this in place, changing time.UTC to
	// time.Local in parseProcStart turns this red under TZ=UTC as well as under TZ=America/*.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata on this machine: %v", err)
	}
	saved := time.Local
	time.Local = ny
	t.Cleanup(func() { time.Local = saved })

	// parseProcStart, not time.ParseInLocation. Calling the standard library here would assert
	// the standard library's behaviour, survive any mutation of the line under test, and prove
	// nothing — which is what the first version of this test did.
	if got := parseProcStart(written); !got.Equal(want) {
		t.Errorf("parseProcStart(%q) = %v, want %v (a UTC/local mix-up shifts it by the zone offset)", written, got, want)
	}

	// Unparseable input is the zero time, not a guess.
	if got := parseProcStart("not a date"); !got.IsZero() {
		t.Errorf("parseProcStart(%q) = %v, want the zero time", "not a date", got)
	}
}

// A weaker companion to the test above: the parsed value matches the OS's own idea of this
// process's start time. It cannot catch a UTC/local mix-up on a machine running in UTC, which
// is why the fixed-string test above exists.
func TestProcStartRoundTripsAgainstTheRunningProcess(t *testing.T) {
	dir := t.TempDir()
	self := os.Getpid()
	writeEntry(t, dir, self, map[string]any{
		"pid": self, "sessionId": "s", "procStart": selfProcStart(t),
	})

	live, err := LiveSessions(dir)
	if err != nil {
		t.Fatalf("LiveSessions: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d sessions, want 1", len(live))
	}

	p, err := process.NewProcess(int32(self))
	if err != nil {
		t.Fatalf("own process: %v", err)
	}
	ms, err := p.CreateTime()
	if err != nil {
		t.Fatalf("own create time: %v", err)
	}
	want := time.UnixMilli(ms).Truncate(time.Second)
	if got := live[0].ProcStarted.Truncate(time.Second); !got.Equal(want) {
		t.Errorf("ProcStarted = %v, want %v (a local/UTC mix-up shifts this by the zone offset)", got, want)
	}
}

// A registry whose files exist but cannot be understood must NOT read as an empty machine.
//
// That reading — "no Claude Code sessions running" — is the same false all-clear this package
// was written to remove, arriving through a different door. A harness that renamed sessionId
// or changed procStart's format would silently turn six live sessions into zero, and every
// surface downstream would report a governed machine.
func TestLiveSessionsWillNotCallAnUnreadableRegistryEmpty(t *testing.T) {
	t.Run("files present, none readable", func(t *testing.T) {
		dir := t.TempDir()
		for _, pid := range []int{101, 102, 103} {
			writeEntry(t, dir, pid, map[string]any{"whatever": "the harness renamed everything"})
		}
		if _, err := LiveSessions(dir); !errors.Is(err, ErrNoRegistry) {
			t.Fatalf("err = %v, want an unreadable-registry error", err)
		}
	})

	// The complement, and the one that stops the rule from swallowing a true zero: files whose
	// PROCESS is gone are stale, not unreadable. A directory of them really does mean nothing
	// is running, and that answer is correct.
	t.Run("files present and readable, but every process is gone", func(t *testing.T) {
		dir := t.TempDir()
		writeEntry(t, dir, 2147483647, map[string]any{
			"pid": 2147483647, "sessionId": "gone", "startedAt": time.Now().UnixMilli(),
			"procStart": time.Now().UTC().Format(procStartLayout),
		})
		live, err := LiveSessions(dir)
		if err != nil {
			t.Fatalf("a stale file is not a measurement failure: %v", err)
		}
		if len(live) != 0 {
			t.Fatalf("got %d sessions, want 0", len(live))
		}
	})

	// One bad file among good ones must not blind it to the rest.
	t.Run("one unreadable file alongside a live one", func(t *testing.T) {
		dir := t.TempDir()
		writeEntry(t, dir, os.Getpid(), map[string]any{
			"pid": os.Getpid(), "sessionId": "real", "startedAt": time.Now().UnixMilli(),
			"procStart": selfProcStart(t),
		})
		writeEntry(t, dir, 999, map[string]any{"nothing": "useful"})
		live, err := LiveSessions(dir)
		if err != nil {
			t.Fatalf("LiveSessions: %v", err)
		}
		if len(live) != 1 || live[0].ID != "real" {
			t.Fatalf("got %+v, want the one real session", live)
		}
	})
}

// The directory belongs to the harness. A path that is not a regular file must not be read:
// a fifo would block the daemon forever, and /dev/zero would allocate until it died — taking
// the budget for the whole machine with it.
func TestLiveSessionsWillNotReadWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(os.DevNull, filepath.Join(dir, "700.json")); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}
	writeEntry(t, dir, os.Getpid(), map[string]any{
		"pid": os.Getpid(), "sessionId": "real", "startedAt": time.Now().UnixMilli(),
		"procStart": selfProcStart(t),
	})
	live, err := LiveSessions(dir)
	if err != nil {
		t.Fatalf("LiveSessions: %v", err)
	}
	if len(live) != 1 || live[0].ID != "real" {
		t.Fatalf("got %+v, want only the real session", live)
	}
}
