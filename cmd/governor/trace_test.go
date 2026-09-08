package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/trace"
)

// withStdin points os.Stdin at a file holding payload, the way a hook receives one.
func withStdin(t *testing.T, payload string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		f.Close()
	})
}

func traceStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(trace.StateDirEnv, dir)
	return dir
}

// TestTraceAgentAlwaysExitsZero is the story's hardest constraint, stated in its traps as
// "hooks exit 0 unconditionally".
//
// This body runs on every subagent start and stop. Phase 0 exists to measure a baseline, and
// a hook that can fail a session does not merely lose a record — it changes the behaviour of
// the thing being measured and gets the plugin blamed for breaking Claude Code. Every
// malformed, hostile and impossible input must still exit 0 and say nothing on stdout.
func TestTraceAgentAlwaysExitsZero(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"valid", `{"hook_event_name":"SubagentStart","session_id":"s","agent_id":"a","agent_type":"Explore"}`},
		{"empty", ``},
		{"whitespace", "   \n\t "},
		{"not json", `this is not json at all`},
		{"truncated json", `{"hook_event_name":"SubagentStart"`},
		{"json array", `[1,2,3]`},
		{"json null", `null`},
		{"json string", `"hello"`},
		{"wrong field types", `{"hook_event_name":42,"session_id":["x"],"agent_id":null}`},
		{"deeply nested", `{"a":` + strings.Repeat(`{"a":`, 200) + `1` + strings.Repeat(`}`, 200) + `}`},
		{"huge", `{"hook_event_name":"SubagentStop","prompt_id":"` + strings.Repeat("x", 1<<20) + `"}`},
		{"nul bytes", "{\"hook_event_name\":\"Sub\x00agentStart\"}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			traceStateDir(t)
			withStdin(t, tc.payload)

			var stdout, stderr bytes.Buffer
			code := run([]string{"trace", "agent"}, &stdout, &stderr)
			if code != 0 {
				t.Errorf("exit %d, want 0 (stderr: %s)", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("wrote %q to stdout; a hook's stdout is read as a decision and must stay empty", stdout.String())
			}
		})
	}
}

// A hook that cannot write its log still must not fail. Losing a record is a gap in a trace;
// failing the hook is a broken session.
func TestTraceAgentSurvivesAnUnwritableLog(t *testing.T) {
	base := t.TempDir()
	// A regular file where the state directory should be, so MkdirAll cannot succeed.
	blocker := filepath.Join(base, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv(trace.StateDirEnv, filepath.Join(blocker, "governor"))
	withStdin(t, `{"hook_event_name":"SubagentStart","agent_id":"a"}`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"trace", "agent"}, &stdout, &stderr); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("nothing on stderr; a dropped record should still leave a trail")
	}
}

func TestTraceAgentRecordsThePayload(t *testing.T) {
	dir := traceStateDir(t)
	withStdin(t, `{"hook_event_name":"SubagentStart","session_id":"sess-1","agent_id":"agent-9",`+
		`"agent_type":"Explore","prompt_id":"p-3","cwd":"/tmp/project","transcript_path":"/x/y.jsonl"}`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"trace", "agent"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	b, err := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var r trace.Record
	if err := json.Unmarshal(bytes.TrimSpace(b), &r); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, b)
	}
	if r.Kind != trace.KindAgent || r.Agent == nil {
		t.Fatalf("want an agent record, got %+v", r)
	}
	want := trace.Agent{
		Event: "SubagentStart", SessionID: "sess-1", AgentID: "agent-9",
		AgentType: "Explore", PromptID: "p-3", CWD: "/tmp/project",
	}
	if *r.Agent != want {
		t.Errorf("agent record\n got %+v\nwant %+v", *r.Agent, want)
	}
	if r.TS == "" {
		t.Error("no timestamp")
	}
}

// GOV-01 measured that SubagentStart also fires on resume, so one agent_id can legitimately
// produce several records. Phase 0 records what happened rather than deduplicating it — the
// duplicate is the finding, and a sampler that hid it would hide the reason confirm has to be
// idempotent in Phase 1.
func TestTraceAgentRecordsRepeatedStartsForOneAgent(t *testing.T) {
	dir := traceStateDir(t)
	for i := 0; i < 2; i++ {
		withStdin(t, `{"hook_event_name":"SubagentStart","session_id":"s","agent_id":"same-agent"}`)
		var stdout, stderr bytes.Buffer
		if code := run([]string{"trace", "agent"}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := len(bytes.Split(bytes.TrimSpace(b), []byte("\n"))); n != 2 {
		t.Errorf("want 2 records for two starts of one agent, got %d", n)
	}
}

func TestTraceRejectsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"trace", "nonesuch"}, &stdout, &stderr); code == 0 {
		t.Error("exit 0 for an unknown trace subcommand, want non-zero")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want empty", stdout.String())
	}
}

// TestWatchdogDisarms guards against a bug this file already had once.
//
// The hook bodies arm a watchdog that calls os.Exit(0) if they somehow block forever. Those
// bodies are also called in-process by the tests above, so a watchdog that is armed and never
// disarmed exits the test binary three seconds in — with status 0, reporting a pass for every
// test that never got to run. A green suite that skipped half of itself is worse than a red
// one.
func TestWatchdogDisarms(t *testing.T) {
	fired := make(chan struct{}, 4)
	orig := watchdogExit
	watchdogExit = func() { fired <- struct{}{} }
	t.Cleanup(func() { watchdogExit = orig })

	// Disarmed before it can fire: nothing happens.
	watchdog(20 * time.Millisecond)()
	select {
	case <-fired:
		t.Fatal("watchdog fired after being disarmed")
	case <-time.After(250 * time.Millisecond):
	}

	// Armed and left alone, it must fire — otherwise the check above proves nothing.
	watchdog(20 * time.Millisecond)
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog never fired while armed; the guard above is vacuous")
	}
}

// And the same property for the real hook body, rather than for the helper alone.
func TestTraceAgentLeavesNoArmedWatchdog(t *testing.T) {
	fired := make(chan struct{}, 4)
	orig := watchdogExit
	watchdogExit = func() { fired <- struct{}{} }
	origDeadline := hookDeadline
	hookDeadline = 20 * time.Millisecond
	t.Cleanup(func() {
		watchdogExit = orig
		hookDeadline = origDeadline
	})

	traceStateDir(t)
	withStdin(t, `{"hook_event_name":"SubagentStart","agent_id":"a"}`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"trace", "agent"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}

	select {
	case <-fired:
		t.Fatal("the hook body returned but left an armed watchdog behind")
	case <-time.After(250 * time.Millisecond):
	}
}
