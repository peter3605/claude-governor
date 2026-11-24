package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// GOV-B56, through the real hooks. These payloads are the ones hook-surface.md section 29
// recorded from probe S1 on Claude Code 2.1.331, verbatim apart from the session, paths and
// the command text: a BACKGROUND subagent backgrounds a shell task and ends its turn, its
// SubagentStop lists itself and the task as running, the task's end wakes it with a second
// SubagentStart for the same agent_id and no PreToolUse, and its final SubagentStop lists
// only itself.

const wakeAgent = "ae7683e44cec32a5f"

// subagentStopWithTasks is a SubagentStop carrying background_tasks, as section 29 measured.
func subagentStopWithTasks(session, agentID, tasks string) string {
	return fmt.Sprintf(`{"session_id":%q,"cwd":"/w","prompt_id":"p","permission_mode":"default",`+
		`"agent_id":%q,"agent_type":"general-purpose","effort":{"level":"medium"},`+
		`"hook_event_name":"SubagentStop","stop_hook_active":false,`+
		`"last_assistant_message":"launched","background_tasks":[%s],"session_crons":[]}`,
		session, agentID, tasks)
}

func selfTask(agentID string) string {
	return fmt.Sprintf(`{"id":%q,"type":"subagent","status":"running",`+
		`"description":"Run background heartbeat command","agent_type":"general-purpose"}`, agentID)
}

func shellTask(id string) string {
	return fmt.Sprintf(`{"id":%q,"type":"shell","status":"running",`+
		`"description":"Run background build","command":"go test ./..."}`, id)
}

// subagentBashPayload is a Bash PreToolUse from inside a subagent: agent_id names the caller.
func subagentBashPayload(session, agentID, toolUseID, cmd string) string {
	return fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"hook_event_name":"PreToolUse",`+
		`"tool_name":"Bash","tool_input":{"command":%q,"run_in_background":true},"tool_use_id":%q}`,
		session, agentID, cmd, toolUseID)
}

// subagentBackgroundedPayload is the PostToolUse launch receipt of that call.
func subagentBackgroundedPayload(session, agentID, toolUseID, taskID string) string {
	return fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"hook_event_name":"PostToolUse",`+
		`"tool_name":"Bash","tool_use_id":%q,"tool_response":{"backgroundTaskId":%q}}`,
		session, agentID, toolUseID, taskID)
}

func TestABackgroundSubagentIsLeasedAgainWhenItsTaskWakesIt(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)
	const s = "S-wake"
	runHookCmd(t, endpoint, sessionPayload(s, "SessionStart"), buildSession("start"))

	if !askGate(t, endpoint, s, "toolu_spawn") {
		t.Fatal("the spawn was refused against an empty budget")
	}
	runHookCmd(t, endpoint, subagentStartPayload(s, wakeAgent), buildConfirm)

	// The subagent backgrounds a build and ends its turn.
	if !askGateWith(t, endpoint, subagentBashPayload(s, wakeAgent, "toolu_bg", "go test ./...")) {
		t.Fatal("the subagent's build was refused against an empty budget")
	}
	runHookCmd(t, endpoint, subagentBackgroundedPayload(s, wakeAgent, "toolu_bg", "bkgbuild1"), buildFinish)
	runHookCmd(t, endpoint,
		subagentStopWithTasks(s, wakeAgent, selfTask(wakeAgent)+","+shellTask("bkgbuild1")), buildRelease)

	if got := heldAgents(d); len(got) != 0 {
		t.Errorf("the idle subagent still holds an agent lease: %v", got)
	}
	if got := d.table.BackgroundTasks(s); !slices.Equal(got, []string{"bkgbuild1"}) {
		t.Fatalf("background tasks after the idle stop = %v, want [bkgbuild1]: "+
			"the build is still running, and a background subagent's stop does not kill it", got)
	}

	// The task ends and wakes it: SubagentStart again, same agent_id, no PreToolUse.
	stderr := runHookCmd(t, endpoint, subagentStartPayload(s, wakeAgent), buildConfirm)
	if !strings.Contains(stderr, `ok=true reason="wake"`) {
		t.Errorf("the wake's confirm said %q, want ok=true reason=\"wake\"", stderr)
	}
	if got := heldAgents(d); !slices.Equal(got, []string{wakeAgent}) {
		t.Fatalf("agent leases after the wake = %v, want [%s]", got, wakeAgent)
	}

	// Its final stop lists only itself: the agent lease and the build lease both go.
	runHookCmd(t, endpoint, subagentStopWithTasks(s, wakeAgent, selfTask(wakeAgent)), buildRelease)
	if n := d.table.InUse(); n != 0 {
		t.Fatalf("in use %d after the subagent's final stop, want 0", n)
	}
}

// A FOREGROUND subagent's SubagentStop lists its task as running without listing itself,
// and the task is killed a moment later (hook-surface.md sections 26 and 29, probe S3). Its
// backgrounded build is released at that stop, exactly as GOV-B41 did.
func TestAForegroundSubagentsStopStillReleasesItsBackgroundBuild(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)
	const s = "S-fg"
	runHookCmd(t, endpoint, sessionPayload(s, "SessionStart"), buildSession("start"))
	askGate(t, endpoint, s, "toolu_spawn")
	runHookCmd(t, endpoint, subagentStartPayload(s, "ab57e784fce4848cb"), buildConfirm)
	askGateWith(t, endpoint, subagentBashPayload(s, "ab57e784fce4848cb", "toolu_bg", "go test ./..."))
	runHookCmd(t, endpoint, subagentBackgroundedPayload(s, "ab57e784fce4848cb", "toolu_bg", "bkgbuild2"), buildFinish)

	runHookCmd(t, endpoint, subagentStopWithTasks(s, "ab57e784fce4848cb", shellTask("bkgbuild2")), buildRelease)
	if n := d.table.InUse(); n != 0 {
		t.Fatalf("in use %d after a foreground subagent stopped, want 0: its task is killed", n)
	}
}

// What the release hook reads off each measured SubagentStop.
func TestBuildReleaseReadsWhetherTheSubagentRunsInTheBackground(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		bg      bool
		running []string
	}{
		{"background, idle (S1 +6.4 s)", subagentStopWithTasks("s", wakeAgent, selfTask(wakeAgent)+","+shellTask("br8i8xjdp")), true, []string{"br8i8xjdp"}},
		{"background, final (S1 +26.8 s)", subagentStopWithTasks("s", wakeAgent, selfTask(wakeAgent)), true, nil},
		{"foreground (S3 +5.7 s)", subagentStopWithTasks("s", "ab57e784fce4848cb", shellTask("b4i5nu3jd")), false, []string{"b4i5nu3jd"}},
		{"another subagent listed, not this one", subagentStopWithTasks("s", wakeAgent, selfTask("a0000000000000000")), false, nil},
		{"no background_tasks (2.1.263 shape)", subagentStopPayload("s", wakeAgent), false, nil},
		{"a task id that is not task-shaped", subagentStopWithTasks("s", wakeAgent, shellTask("../../etc")), false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p lifecyclePayload
			if !decodePayload(strings.NewReader(c.payload), io.Discard, &p) {
				t.Fatal("the payload did not decode")
			}
			r, ok := buildRelease(p, io.Discard)
			if !ok {
				t.Fatal("release built nothing to send")
			}
			if r.BackgroundAgent != c.bg || !slices.Equal(r.Running, c.running) {
				t.Errorf("background_agent=%v running=%v, want %v %v", r.BackgroundAgent, r.Running, c.bg, c.running)
			}
		})
	}
}

// The gate names the subagent a SendMessage resume targets, and nothing else.
func TestTheGateSendsTheResumeTarget(t *testing.T) {
	resume := hookPayload{ToolName: "SendMessage"}
	resume.ToolInput.To = "ab6d25847dc0e2b9f"
	if got := acquireRequestFor(resume, intent{kind: "agent"}, protocolVersion).Resume; got != "ab6d25847dc0e2b9f" {
		t.Errorf("a SendMessage resume sent resume=%q, want the agent it targets", got)
	}
	teammate := hookPayload{ToolName: "SendMessage"}
	teammate.ToolInput.To = "reviewer"
	spawn := hookPayload{ToolName: "Agent"}
	for _, p := range []hookPayload{teammate, spawn} {
		if got := acquireRequestFor(p, intent{kind: "agent"}, protocolVersion).Resume; got != "" {
			t.Errorf("%s to %q sent resume=%q, want none", p.ToolName, p.ToolInput.To, got)
		}
	}
}

func heldAgents(d *daemon) []string {
	var out []string
	for _, l := range d.table.Leases() {
		if l.Kind.String() == "agent" && l.AgentID != "" {
			out = append(out, l.AgentID)
		}
	}
	slices.Sort(out)
	return out
}
