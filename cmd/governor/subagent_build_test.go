package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// GOV-B35. The depth rule (design.md section 6.3) is about spawns: an agent that holds a slot
// while it waits for a slot for its own child is the nested deadlock the rule exists to
// prevent. A build holds its lease and finishes, so there is no such deadlock, and a subagent
// running `go test` is exactly the build concurrency the governor rations. These tests pin
// that a subagent's build is rationed by capacity and never refused for depth, and that a
// subagent's Agent spawn still is.

// subagentBuildPayload is a real PreToolUse shape for a Bash call made by a subagent: the
// top-level agent_id names the caller (docs/hook-surface.md section 2).
func subagentBuildPayload(session, toolUseID, command string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id":      session,
		"agent_id":        "a0dca3e4fa79a2f6f",
		"agent_type":      "general-purpose",
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_use_id":     toolUseID,
		"tool_input":      map[string]any{"command": command},
	})
	return string(b)
}

func runGatePayload(t *testing.T, endpoint, payload string) (string, int) {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)
	var stdout, stderr strings.Builder
	code := gate(strings.NewReader(payload), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
	return stdout.String(), code
}

// Done-when 1: a subagent's build against free slots is granted and holds one build lease.
func TestASubagentBuildIsGrantedByCapacity(t *testing.T) {
	endpoint, d := startTestDaemon(t, 8)

	stdout, code := runGatePayload(t, endpoint, subagentBuildPayload("s1", "toolu_sub_build", "go test ./..."))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty: a subagent's build with free slots is a grant", stdout)
	}
	if got := d.table.InUse(); got != 1 {
		t.Fatalf("in use = %d, want 1 lease", got)
	}
	if b, agents := d.table.Split(); b <= 0 || agents != 0 {
		t.Errorf("split = %.2f build slots, %d agents; want one build lease and no agent lease", b, agents)
	}
}

// Done-when 2: a subagent's build against a full budget is refused for capacity, in build
// words, and never with the nested-spawn text.
func TestASubagentBuildIsRefusedForCapacityNotDepth(t *testing.T) {
	shortWait(t, 200*time.Millisecond)
	endpoint, d := startTestDaemon(t, 4)
	fill(t, d, 4)

	stdout, code := runGatePayload(t, endpoint, subagentBuildPayload("s1", "toolu_sub_build", "go test ./..."))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var out denyOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("a build against a full budget was not refused; stdout = %q", stdout)
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("permissionDecision = %q, want deny", out.HookSpecificOutput.PermissionDecision)
	}
	reason := out.HookSpecificOutput.PermissionDecisionReason
	for _, bad := range []string{"Nested subagent", "Agent call"} {
		if strings.Contains(reason, bad) {
			t.Errorf("reason = %q; a build refusal must not contain %q", reason, bad)
		}
	}
	if !strings.Contains(reason, "capacity") && !strings.Contains(reason, "slots") {
		t.Errorf("reason = %q, want the capacity text", reason)
	}
}

// The gate sends depth only for a spawn. Version skew is handled at the daemon too (below),
// but a new gate must not ask an old daemon to refuse its build for depth.
func TestTheGateSendsDepthZeroForASubagentBuild(t *testing.T) {
	p := hookPayload{SessionID: "s1", AgentID: "a1", ToolName: "Bash"}
	if got := acquireRequestFor(p, intent{take: true, kind: "build", weight: 1}, protocolVersion).Depth; got != 0 {
		t.Errorf("build depth = %d, want 0", got)
	}
	if got := acquireRequestFor(p, intent{take: true, kind: "agent", weight: 0.1}, protocolVersion).Depth; got != 1 {
		t.Errorf("agent depth = %d, want 1: a subagent's spawn is still nested", got)
	}
}

// Done-when 4: an older gate sends depth=1 with its builds. The daemon keys the depth rule on
// kind, so that build is granted or refused for capacity, never for depth.
func TestTheDaemonIgnoresDepthOnABuild(t *testing.T) {
	d := newDaemon(4, io.Discard)

	switch r := d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1,
		ToolUseID: "toolu_old_gate", Depth: 1}).(type) {
	case grantReply:
	case refuseReply:
		t.Fatalf("a depth-1 build with free slots was refused for %q", r.Reason)
	default:
		t.Fatalf("reply = %#v, want a grant", r)
	}

	fill(t, d, 4)
	switch r := d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1,
		ToolUseID: "toolu_old_gate_2", Depth: 1}).(type) {
	case refuseReply:
		if r.Reason == "depth" {
			t.Fatal("a depth-1 build was refused for depth; builds are rationed by capacity")
		}
	case grantReply:
	default:
		t.Fatalf("reply = %#v, want a grant or a capacity refusal", r)
	}

	if a := d.advise(request{V: protocolVersion, Session: "s2", Kind: "build", Slots: 1, Depth: 1}); a.Reason == "depth" {
		t.Error("advise answered depth for a build")
	}

	// A spawn at depth 1 is still refused for depth, and so is an older gate's kindless
	// request, which leaseRequestFor already reads as an agent.
	for _, kind := range []string{"agent", ""} {
		r, ok := d.acquire(request{V: protocolVersion, Session: "s3", Kind: kind, Depth: 1}).(refuseReply)
		if !ok || r.Reason != "depth" {
			t.Errorf("kind %q at depth 1: reply = %#v, want reason=depth", kind, r)
		}
	}
}

// An older daemon still answers depth to a build. The gate must not then tell the model about
// an Agent call it never made.
func TestADepthAnswerToABuildNeverNamesTheAgentTool(t *testing.T) {
	reason := denyReason(acquireResponse{Reason: "depth"}, intent{kind: "build"})
	for _, bad := range []string{"Nested subagent", "Agent call", "Agent tool"} {
		if strings.Contains(reason, bad) {
			t.Errorf("reason = %q; must not contain %q for a build", reason, bad)
		}
	}
}
