package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The payloads below are the ones Claude Code 2.1.327 sent in docs/hook-surface.md section 26,
// verbatim apart from the paths and the trimmed command text.

// backgroundedPayload is the PostToolUse for a Bash call that passed its timeout and was moved
// to the background (probe P2), or, with agentID set, the same from inside a subagent (P6).
func backgroundedPayload(session, toolUseID, taskID, agentID string) string {
	agent := ""
	if agentID != "" {
		agent = `"agent_id":"` + agentID + `","agent_type":"general-purpose",`
	}
	return `{"session_id":"` + session + `","cwd":"/w","prompt_id":"p","permission_mode":"bypassPermissions",` +
		agent + `"hook_event_name":"PostToolUse","tool_name":"Bash",` +
		`"tool_input":{"command":"pytest -q","timeout":120000},` +
		`"tool_response":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false,` +
		`"backgroundTaskId":"` + taskID + `","timedOutAfterMs":120000},` +
		`"tool_use_id":"` + toolUseID + `","duration_ms":120050}`
}

// notificationPayload is the UserPromptSubmit Claude Code fires when a main-thread background
// task ends (probes P1, P2, P3, P5). The prompt is the notification itself.
func notificationPayload(session, toolUseID, taskID, status string) string {
	prompt := "<task-notification>\n<task-id>" + taskID + "</task-id>\n<tool-use-id>" + toolUseID +
		"</tool-use-id>\n<output-file>/tmp/claude-501/w/" + session + "/tasks/" + taskID +
		".output</output-file>\n<status>" + status + "</status>\n<summary>Background command " +
		"\"pytest -q\" " + status + " (exit code 0)</summary>\n</task-notification>"
	raw, _ := json.Marshal(map[string]any{
		"session_id": session, "transcript_path": "/t.jsonl", "cwd": "/w", "prompt_id": "p2",
		"permission_mode": "bypassPermissions", "hook_event_name": "UserPromptSubmit", "prompt": prompt,
	})
	return string(raw)
}

func subagentStoppedPayload(session, agentID string) string {
	return `{"session_id":"` + session + `","cwd":"/w","permission_mode":"bypassPermissions",` +
		`"agent_id":"` + agentID + `","agent_type":"general-purpose","hook_event_name":"SubagentStop",` +
		`"stop_hook_active":false,"background_tasks":[],"session_crons":[]}`
}

// acquireMedium asks for one Medium build for session, without waiting.
func acquireMedium(t *testing.T, endpoint, session, toolUseID string) acquireResponse {
	t.Helper()
	line, err := callRaw(t, endpoint, `{"v":1,"op":"acquire","session":"`+session+`","kind":"build",`+
		`"class":"medium","slots":1,"tool_use_id":"`+toolUseID+`","wait_ms":0}`+"\n")
	if err != nil {
		t.Fatalf("acquire %s: %v", toolUseID, err)
	}
	var r acquireResponse
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("acquire %s reply %q: %v", toolUseID, line, err)
	}
	return r
}

// mustFire runs the registered hook for event, and fails if hooks.json registers none: an
// unregistered event is GOV-49's defect, a release written and never run.
func mustFire(t *testing.T, event, tool, payload string) {
	t.Helper()
	if !harnessFires(t, event, tool, payload) {
		t.Fatalf("hooks.json registers nothing for %s, so the background build's end is never seen", event)
	}
}

// TestABackgroundedBuildIsNamedAndThenReleasedByItsNotification replays the earbrief session
// of 2026-11-14 to 11-20 (GOV-B41, criterion 3).
//
// A foreground pytest passed its Bash timeout and was moved to the background. PostToolUse
// fired with a backgroundTaskId and, correctly, released nothing. The session's next build was
// then refused, and the refusal named nobody. With a one-slot budget the replay is: build
// granted, backgrounded, same session asks again. That ask must be refused (the build IS still
// running) under the own-session head, naming the background task. Then the task ends, the
// notification arrives as a UserPromptSubmit, and the next ask must be admitted.
func TestABackgroundedBuildIsNamedAndThenReleasedByItsNotification(t *testing.T) {
	endpoint, d := startTestDaemon(t, 1)
	t.Setenv(transport.EndpointEnv, endpoint)

	if r := acquireMedium(t, endpoint, "earbrief", "toolu_pytest"); !r.OK {
		t.Fatalf("the first build was refused against an empty table: %+v", r)
	}
	mustFire(t, "PostToolUse", "Bash", backgroundedPayload("earbrief", "toolu_pytest", "bqodkcz89", ""))
	if got := d.table.Slots(); got != 1 {
		t.Fatalf("the backgrounding PostToolUse left %.2f slots held, want 1: it is a launch receipt, "+
			"and the build behind it is still running (GOV-15)", got)
	}

	r := acquireMedium(t, endpoint, "earbrief", "toolu_next")
	if r.OK {
		t.Fatalf("a second build was admitted beside a backgrounded one that is still running")
	}
	if !reflect.DeepEqual(r.OwnBackground, []string{"bqodkcz89"}) {
		t.Errorf("the refusal names background tasks %q, want [bqodkcz89]", r.OwnBackground)
	}
	text := denyReason(r, intent{kind: "build"})
	if strings.HasPrefix(text, "Not enough free build slots") {
		t.Errorf("the refusal is the bare capacity sentence the earbrief session got 29 times:\n%s", text)
	}
	if !strings.HasPrefix(text, "This session's own earlier build is holding the slots") {
		t.Errorf("the refusal does not lead with the session's own build:\n%s", text)
	}
	if !strings.Contains(text, "bqodkcz89") {
		t.Errorf("the refusal does not name the background task holding the slot:\n%s", text)
	}

	mustFire(t, "UserPromptSubmit", "", notificationPayload("earbrief", "toolu_pytest", "bqodkcz89", "completed"))
	if r := acquireMedium(t, endpoint, "earbrief", "toolu_after"); !r.OK {
		t.Fatalf("after the task notification the next build was still refused (slots=%.2f/%d): the "+
			"backgrounded build held its slot past its own end", r.Slots, r.Budget)
	}
}

// TestAFailedBackgroundTaskIsReleasedToo: a background task that exits non-zero is reported
// with status "failed" (probe P3, exit 3), and it has ended just the same.
func TestAFailedBackgroundTaskIsReleasedToo(t *testing.T) {
	endpoint, d := startTestDaemon(t, 1)
	t.Setenv(transport.EndpointEnv, endpoint)

	acquireMedium(t, endpoint, "s", "toolu_bg")
	mustFire(t, "PostToolUse", "Bash", backgroundedPayload("s", "toolu_bg", "b1njo6u3q", ""))
	mustFire(t, "UserPromptSubmit", "", notificationPayload("s", "toolu_bg", "b1njo6u3q", "failed"))
	if got := d.table.Slots(); got != 0 {
		t.Errorf("a failed background task's lease is still held: %.2f slots", got)
	}
}

// TestASubagentsBackgroundBuildIsReleasedAtSubagentStop covers the shape most of the earbrief
// leases had: a subagent's build passes its timeout. Its notification goes into the subagent's
// context and fires no hook (probe P6), and the task is killed when the subagent stops (P7).
func TestASubagentsBackgroundBuildIsReleasedAtSubagentStop(t *testing.T) {
	endpoint, d := startTestDaemon(t, 2)
	t.Setenv(transport.EndpointEnv, endpoint)

	acquireMedium(t, endpoint, "s", "toolu_sub")
	mustFire(t, "PostToolUse", "Bash", backgroundedPayload("s", "toolu_sub", "b0kn8kly4", "ad08ea6c22cc50ff3"))

	// The control: another agent stopping is not this one.
	mustFire(t, "SubagentStop", "", subagentStoppedPayload("s", "a5cc39cc37cd75d42"))
	if got := d.table.Slots(); got != 1 {
		t.Fatalf("a different subagent's stop released this one's build: %.2f slots, want 1", got)
	}

	mustFire(t, "SubagentStop", "", subagentStoppedPayload("s", "ad08ea6c22cc50ff3"))
	if got := d.table.Slots(); got != 0 {
		t.Errorf("SubagentStop left the subagent's killed background build holding %.2f slots", got)
	}
}

// TestNotifiedToolUseIDs holds the prefix rule: a prompt is a task notification only when it
// starts with one.
func TestNotifiedToolUseIDs(t *testing.T) {
	for _, c := range []struct {
		name   string
		prompt string
		want   []string
	}{
		{"one", "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>toolu_A</tool-use-id>\n</task-notification>",
			[]string{"toolu_A"}},
		{"two in one prompt", "<task-notification><tool-use-id>toolu_A</tool-use-id></task-notification>\n" +
			"<task-notification><tool-use-id>toolu_B</tool-use-id></task-notification>", []string{"toolu_A", "toolu_B"}},
		{"leading space", "  \n<task-notification><tool-use-id>toolu_A</tool-use-id>", []string{"toolu_A"}},
		{"quoted by a person", "why did <task-notification><tool-use-id>toolu_A</tool-use-id> happen?", nil},
		{"ordinary prompt", "run the tests", nil},
		{"unterminated", "<task-notification><tool-use-id>toolu_A", nil},
	} {
		if got := notifiedToolUseIDs(c.prompt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAnOrdinaryPromptSendsNothing: UserPromptSubmit fires on every prompt a person types, and
// none of those may cost a socket round trip.
func TestAnOrdinaryPromptSendsNothing(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "UserPromptSubmit",
		"prompt": "please run the tests"})
	var p lifecyclePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if _, send := buildSettle(p, &strings.Builder{}); send {
		t.Errorf("an ordinary prompt produced a settle request")
	}
}

// TestTheDaemonRefusesAStrangeTaskID: the ID is written into the release log, which is pasted
// into public issues, so the daemon accepts only the shape every measured ID had.
func TestTheDaemonRefusesAStrangeTaskID(t *testing.T) {
	for _, id := range []string{"b1njo6u3q", "bqodkcz89"} {
		if !taskIDShaped(id) {
			t.Errorf("measured task ID %q was refused", id)
		}
	}
	for _, id := range []string{"", "B1", "b1 by=reaper", "b1\nrelease", strings.Repeat("a", 33)} {
		if taskIDShaped(id) {
			t.Errorf("task ID %q was accepted", id)
		}
	}
}

// TestTheReleaseLogNamesABackgroundTask: bg= is how criterion 5 tells a backgrounded build the
// reaper expired from an ordinary one.
func TestTheReleaseLogNamesABackgroundTask(t *testing.T) {
	r := lease.Released{Lease: lease.Lease{Session: "s", Kind: lease.Build, Class: "medium", Weight: 1,
		BackgroundTask: "bqodkcz89"}, Why: "expired"}
	line := releaseLine(r, releasedByReaper)
	if !strings.HasSuffix(line, "by=reaper:expired bg=bqodkcz89") {
		t.Errorf("release line %q does not end with the reaper and the task", line)
	}
	r.BackgroundTask = ""
	if strings.Contains(releaseLine(r, releasedByReaper), "bg=") {
		t.Errorf("a foreground build's release line carries bg=")
	}
}
