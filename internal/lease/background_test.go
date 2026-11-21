package lease

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// GOV-B41: a backgrounded build's holder is its background task, and the task's output file
// is what says whether it is still running (docs/hook-surface.md section 26).

// writeTask lays out a task output file the way Claude Code 2.1.327 does:
// <root>/<cwd slug>/<session>/tasks/<task>.output.
func writeTask(t *testing.T, root, session, task, content string) string {
	t.Helper()
	dir := filepath.Join(root, "-Users-someone-Repos-earbrief", session, "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, task+".output")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func bgLease(session, task string) Lease {
	return Lease{Session: session, Kind: Build, State: Confirmed, BackgroundTask: task}
}

// The status lines below are the measured ones, verbatim, including the blank line Claude Code
// leaves between the command's output and the status line.
func TestABackgroundTaskIsAliveUntilItsOutputFileSaysOtherwise(t *testing.T) {
	const session = "4aabf99f-58e6-4633-97b5-9dbdeb26aca8"
	for _, c := range []struct {
		name    string
		content string
		alive   bool
	}{
		{"running, output so far", "============ test session starts ============\ncollected 412 items\n", true},
		{"running, nothing written yet", "", true},
		{"exited 0", "412 passed in 201.3s\n\n[exited with code 0]\n", false},
		{"exited non-zero", "bg-done\n\n[exited with code 3]\n", false},
		{"killed", "\n[killed]\n", false},
		{"killed, no newline after", "\n[killed]", false},
		// A status line that is not the LAST line is the command's own output, not the end.
		{"status line mid-output", "\n[exited with code 0]\nstill going\n", true},
		{"a test name that merely mentions it", "test_reports_[killed]_state PASSED\n", true},
	} {
		root := t.TempDir()
		writeTask(t, root, session, "bqodkcz89", c.content)
		v := PIDLiveness{taskRoot: root}
		if got := v.Alive(bgLease(session, "bqodkcz89")); got != c.alive {
			t.Errorf("%s: Alive = %v, want %v", c.name, got, c.alive)
		}
	}
}

// Every way of not finding or not trusting the file is "alive", per the Liveness contract: a
// dead lease kept costs a slot until the TTL, a live one reaped over-subscribes the machine.
func TestABackgroundTaskThatCannotBeFoundIsAlive(t *testing.T) {
	const session = "s-1"
	root := t.TempDir()
	v := PIDLiveness{taskRoot: root}
	if !v.Alive(bgLease(session, "bmissing1")) {
		t.Error("a task with no output file was reported dead")
	}

	// Two files for one session and task is a layout nothing measured.
	writeTask(t, root, session, "btwice", "[exited with code 0]\n")
	other := filepath.Join(root, "-another-slug", session, "tasks")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "btwice.output"), []byte("[killed]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !v.Alive(bgLease(session, "btwice")) {
		t.Error("an ambiguous pair of output files was read as an ended task")
	}

	// IDs off the wire that would turn the pattern into something else.
	writeTask(t, root, session, "bdone", "[exited with code 0]\n")
	for _, l := range []Lease{bgLease("*", "bdone"), bgLease(session, "*"), bgLease("../"+session, "bdone"),
		bgLease(session, "b/../bdone")} {
		if !v.Alive(l) {
			t.Errorf("session %q task %q was resolved by pattern and reported dead", l.Session, l.BackgroundTask)
		}
	}
}

// A foreground build lease still goes by its owner PID, which it never has, so it is alive and
// left to PostToolUse and the TTL exactly as before.
func TestAForegroundBuildIsNotJudgedByAnyTaskFile(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "s", "bdone", "[exited with code 0]\n")
	v := PIDLiveness{taskRoot: root}
	if !v.Alive(Lease{Session: "s", Kind: Build, State: Confirmed}) {
		t.Error("a foreground build lease was reported dead")
	}
}

// The reaper collects a subagent's backgrounded build the moment its task ends, and says so in
// the release's Why. This is the earbrief case: all 31 of its backgrounded builds were in
// subagents that lived 434 to 1,302 s, so neither the notification nor SubagentStop could
// return them before the TTL did.
func TestTheReaperReturnsABackgroundBuildWhenItsTaskEnds(t *testing.T) {
	const session = "4aabf99f-58e6-4633-97b5-9dbdeb26aca8"
	root := t.TempDir()
	out := writeTask(t, root, session, "bc5ffqpn4", "collected 412 items\n")
	now := time.Unix(1_800_000_000, 0)
	tbl := NewTable(Options{Liveness: PIDLiveness{taskRoot: root}, Now: func() time.Time { return now }})

	if _, ok := tbl.Acquire(Request{Session: session, Kind: Build, Weight: 1, ToolUseID: "toolu_pytest"}, 4); !ok {
		t.Fatal("acquire refused on an empty table")
	}
	if !tbl.MarkBackground(session, "toolu_pytest", "bc5ffqpn4", "ad08ea6c22cc50ff3") {
		t.Fatal("MarkBackground found no lease")
	}
	if gone := tbl.Reap(); len(gone) != 0 {
		t.Fatalf("the reaper released a build whose task is still running: %+v", gone)
	}

	if err := os.WriteFile(out, []byte("collected 412 items\n412 passed\n\n[exited with code 0]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := tbl.Reap()
	if len(gone) != 1 || gone[0].Why != "task-ended" || gone[0].BackgroundTask != "bc5ffqpn4" {
		t.Fatalf("after the task ended the reaper released %+v, want the one lease, why task-ended", gone)
	}
	if tbl.Slots() != 0 {
		t.Errorf("%.2f slots still held", tbl.Slots())
	}
}
