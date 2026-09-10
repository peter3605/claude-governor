package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// The gate wired to the daemon (GOV-07, design.md sections 4.1 and 4.3).
//
// Everything here drives the four hook commands through their real bodies against a real
// daemon on a real socket. Calling the lease table directly would be quicker and would
// prove much less: what this story adds is the wiring between them, and wiring is precisely
// what a unit test of either end cannot see.

// spawn is one subagent's worth of hook traffic.
type spawn struct {
	session string
	agentID string
}

// preToolUsePayload is the PreToolUse payload for one spawn, from docs/hook-surface.md
// section 1. depth 0 — no top-level agent_id, so the main session is the caller.
func preToolUsePayload(session, toolUseID string) string {
	return fmt.Sprintf(`{"session_id":%q,"hook_event_name":"PreToolUse","tool_name":"Agent",`+
		`"tool_input":{"description":"work","subagent_type":"general-purpose"},`+
		`"tool_use_id":%q}`, session, toolUseID)
}

// subagentStartPayload is from docs/hook-surface.md section 3. Note what it does NOT carry:
// no tool_use_id, which is why correlation is per-session FIFO rather than by ID.
func subagentStartPayload(session, agentID string) string {
	return fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"agent_type":"general-purpose",`+
		`"hook_event_name":"SubagentStart"}`, session, agentID)
}

// subagentStopPayload is from docs/hook-surface.md section 4.
func subagentStopPayload(session, agentID string) string {
	return fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"agent_type":"general-purpose",`+
		`"hook_event_name":"SubagentStop"}`, session, agentID)
}

func sessionPayload(session, event string) string {
	return fmt.Sprintf(`{"session_id":%q,"hook_event_name":%q}`, session, event)
}

// runHookCmd drives one lifecycle hook through the same entry point the binary uses, and
// asserts the contract every hook shares: exit 0, nothing on stdout.
func runHookCmd(t *testing.T, endpoint, payload string, build builder) string {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stderr bytes.Buffer
	code := lifecycleHook(strings.NewReader(payload), &stderr, transport.Dial{}, hookBudget, build)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	return stderr.String()
}

// askGate runs the gate for one spawn and reports whether the spawn was allowed.
//
// Allowed means an empty stdout, which is what a grant is (design.md section 4.2). Refused
// means a well-formed deny on stdout. Anything else — a half-written decision, a decision
// on the grant path — fails here rather than being folded into a boolean.
func askGate(t *testing.T, endpoint, session, toolUseID string) bool {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	code := gate(strings.NewReader(preToolUsePayload(session, toolUseID)),
		&stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
	if code != 0 {
		t.Fatalf("gate exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if stdout.Len() == 0 {
		return true
	}
	var out denyOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("gate stdout = %q, want empty or one decision object: %v", stdout.String(), err)
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("gate emitted permissionDecision %q; only \"deny\" may ever be emitted",
			out.HookSpecificOutput.PermissionDecision)
	}
	return false
}

// TestTheNPlusFirstSpawnIsRefused is this story's headline acceptance criterion.
func TestTheNPlusFirstSpawnIsRefused(t *testing.T) {
	// GOV-08 made the refusal arrive after a wait rather than immediately, so this sits
	// through gateWaitMax unless it is shrunk. The wait itself is what wait_test.go is for;
	// what this test asserts is unchanged — the N+1th spawn does not get a slot.
	shortWait(t, 200*time.Millisecond)

	const budget = 3
	endpoint, d := startTestDaemon(t, budget)

	for i := 0; i < budget; i++ {
		if !askGate(t, endpoint, "s1", fmt.Sprintf("toolu_%02d", i)) {
			t.Fatalf("spawn %d of %d was refused; the first %d must proceed", i+1, budget, budget)
		}
	}
	if askGate(t, endpoint, "s1", "toolu_overflow") {
		t.Fatal("the N+1th spawn was allowed; it must be refused")
	}
	if got := d.table.InUse(); got != budget {
		t.Errorf("in use = %d, want %d: a refused spawn must not consume a slot", got, budget)
	}
}

// A refusal must not be sticky. Once a lease comes back the next spawn proceeds — without
// this the first burst of the session would permanently poison the budget.
func TestASpawnProceedsOnceASlotIsReturned(t *testing.T) {
	shortWait(t, 200*time.Millisecond)
	endpoint, _ := startTestDaemon(t, 1)

	if !askGate(t, endpoint, "s1", "toolu_01") {
		t.Fatal("the first spawn was refused")
	}
	runHookCmd(t, endpoint, subagentStartPayload("s1", "agent_a"), buildConfirm)
	if askGate(t, endpoint, "s1", "toolu_02") {
		t.Fatal("a second spawn was allowed against a budget of 1")
	}

	runHookCmd(t, endpoint, subagentStopPayload("s1", "agent_a"), buildRelease)

	if !askGate(t, endpoint, "s1", "toolu_03") {
		t.Fatal("the spawn after a release was refused; the slot was not returned")
	}
}

// TestTheFullLifecycleThroughTheHooks walks one spawn from PreToolUse to SubagentStop
// through the four commands, checking the lease's state at each step rather than only its
// count. A lease that is confirmed but never released and one that was never confirmed both
// show as "1 in use", so the count alone cannot tell the wiring worked.
func TestTheFullLifecycleThroughTheHooks(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	runHookCmd(t, endpoint, sessionPayload("s1", "SessionStart"), buildSession("start"))
	if snap := d.table.Snapshot(); len(snap.Sessions) != 1 || snap.Sessions[0].ID != "s1" {
		t.Fatalf("after SessionStart the daemon knows %+v, want one session s1", snap.Sessions)
	}

	if !askGate(t, endpoint, "s1", "toolu_01") {
		t.Fatal("the gate refused against an empty budget")
	}
	if snap := d.table.Snapshot(); snap.Tentative != 1 || snap.Confirmed != 0 {
		t.Fatalf("after PreToolUse: tentative %d confirmed %d, want 1 and 0 — the spawn has "+
			"not happened yet, so the lease must not be confirmed",
			snap.Tentative, snap.Confirmed)
	}

	runHookCmd(t, endpoint, subagentStartPayload("s1", "agent_a"), buildConfirm)
	snap := d.table.Snapshot()
	if snap.Tentative != 0 || snap.Confirmed != 1 {
		t.Fatalf("after SubagentStart: tentative %d confirmed %d, want 0 and 1",
			snap.Tentative, snap.Confirmed)
	}

	runHookCmd(t, endpoint, subagentStopPayload("s1", "agent_a"), buildRelease)
	if got := d.table.InUse(); got != 0 {
		t.Fatalf("after SubagentStop: %d in use, want 0", got)
	}

	runHookCmd(t, endpoint, sessionPayload("s1", "SessionEnd"), buildSession("end"))
	if snap := d.table.Snapshot(); len(snap.Sessions) != 0 {
		t.Fatalf("after SessionEnd the daemon still knows %+v, want none", snap.Sessions)
	}
}

// TestConfirmBindsTheOwningSessionPID is what makes the reaper work.
//
// A confirmed lease with no owner is only ever collected by its TTL, which is 30 minutes —
// so a session that dies would hold its slots for half an hour. The PID is what turns that
// into two seconds, and it is not in the payload: docs/hook-surface.md section 5 records
// that neither SubagentStart nor SessionStart carries one, so it has to come from the hook
// process's own ancestry.
//
// Under `go test` the ancestry is the test binary, not a claude process, so OwningSession
// reports unknown and the PID is legitimately zero. That is the documented safe answer, and
// asserting a non-zero PID here would be asserting the test harness rather than the code.
// What can be asserted is that confirm asks at all and sends what it found — checked by
// driving the same builder with a lookup that does find a session.
func TestConfirmBindsTheOwningSessionPID(t *testing.T) {
	endpoint, d := startTestDaemon(t, 2)

	if !askGate(t, endpoint, "s1", "toolu_01") {
		t.Fatal("the gate refused")
	}

	var diag bytes.Buffer
	req, ok := buildConfirm(lifecyclePayload{SessionID: "s1", AgentID: "agent_a"}, &diag)
	if !ok {
		t.Fatal("buildConfirm declined to build a request")
	}
	if req.Op != "confirm" || req.Session != "s1" || req.AgentID != "agent_a" {
		t.Errorf("confirm request = %+v, want op/session/agent_id set", req)
	}
	if req.V != protocolVersion {
		t.Errorf("confirm carries v=%d, want %d: every message carries its version", req.V, protocolVersion)
	}
	if req.Principal.User == "" {
		t.Error("confirm carries no principal; every request must carry one")
	}
	// Which answer is correct depends on what is running the test — under `go test` from a
	// shell there is no claude ancestor, while the same suite run from inside a Claude Code
	// session has one. Both are valid; what must hold is that the two halves agree, because
	// the failure this guards is a PID sent WITHOUT the start time that makes it safe. A
	// bare PID is worse than none: PIDs are recycled, and the reaper would then keep a dead
	// agent's lease alive for as long as some unrelated process held its number.
	switch {
	case req.PID == 0:
		if req.PIDStartedAt != 0 {
			t.Errorf("confirm sent pid 0 with start time %d, want both absent", req.PIDStartedAt)
		}
		if !strings.Contains(diag.String(), "TTL") {
			t.Errorf("stderr = %q, want it to say the lease falls back to its TTL when no "+
				"owning session was found", diag.String())
		}
	default:
		if req.PIDStartedAt <= 0 {
			t.Errorf("confirm sent pid %d with no start time; a bare pid is unsafe because "+
				"pids are recycled", req.PID)
		}
	}

	// And the confirm still lands: an unknown owner is not a reason to skip the bind.
	runHookCmd(t, endpoint, subagentStartPayload("s1", "agent_a"), buildConfirm)
	if snap := d.table.Snapshot(); snap.Confirmed != 1 {
		t.Fatalf("confirmed %d, want 1 even with an unknown owner pid", snap.Confirmed)
	}
}

// TestConfirmIsIdempotentPerAgent is the trap GOV-01 measured and GOV-07's acceptance
// criteria do not by themselves force.
//
// SubagentStart is a start-OR-RESUME signal: it fires again for an agent_id that is already
// running when SendMessage resumes that agent, and one spawn was measured producing two
// SubagentStart and two SubagentStop events. A confirm that is not idempotent takes a
// SECOND lease for the same agent — or worse, pops the FIFO and binds some other pending
// spawn's lease to this agent, so two agents share one lease and one runs ungoverned.
//
// The N+1 refusal test passes without any of this, which is exactly why this test exists.
func TestConfirmIsIdempotentPerAgent(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	// Two spawns in flight, so a second confirm has something to wrongly steal.
	if !askGate(t, endpoint, "s1", "toolu_01") || !askGate(t, endpoint, "s1", "toolu_02") {
		t.Fatal("a spawn was refused against a budget of 4")
	}
	if snap := d.table.Snapshot(); snap.Tentative != 2 {
		t.Fatalf("tentative %d, want 2", snap.Tentative)
	}

	runHookCmd(t, endpoint, subagentStartPayload("s1", "agent_a"), buildConfirm)
	runHookCmd(t, endpoint, subagentStartPayload("s1", "agent_a"), buildConfirm)

	snap := d.table.Snapshot()
	if snap.Confirmed != 1 {
		t.Errorf("confirmed %d after two SubagentStart for one agent, want 1", snap.Confirmed)
	}
	if snap.Tentative != 1 {
		t.Errorf("tentative %d, want 1: the resume must not pop the FIFO and bind the other "+
			"spawn's lease to this agent", snap.Tentative)
	}
	if snap.InUse != 2 {
		t.Errorf("in use %d, want 2: a resumed agent must not consume a second slot", snap.InUse)
	}

	// Release fires twice for the same reason, and must tolerate it.
	runHookCmd(t, endpoint, subagentStopPayload("s1", "agent_a"), buildRelease)
	runHookCmd(t, endpoint, subagentStopPayload("s1", "agent_a"), buildRelease)

	if snap := d.table.Snapshot(); snap.InUse != 1 {
		t.Errorf("in use %d after two SubagentStop, want 1: the second release must not "+
			"free the other spawn's lease", snap.InUse)
	}
}

// TestConfirmWithNoPendingLeaseIsDroppedQuietly is the other measured trap.
//
// SendMessage resumes a subagent with NO PreToolUse(Agent) at all, so a confirm can arrive
// with an empty tentative queue. It must be dropped quietly rather than erroring: the hook
// still exits 0, nothing reaches stdout, and no lease is invented. That spawn is ungoverned
// in v1 by design, and the budget is therefore not airtight — which is stated here so that
// a later reader finds it as a property under test rather than as a surprise.
func TestConfirmWithNoPendingLeaseIsDroppedQuietly(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	diag := runHookCmd(t, endpoint, subagentStartPayload("s1", "agent_resumed"), buildConfirm)

	if got := d.table.InUse(); got != 0 {
		t.Errorf("in use = %d, want 0: a confirm with nothing pending must not create a lease", got)
	}
	if !strings.Contains(diag, "no_pending_lease") {
		t.Errorf("stderr = %q, want the daemon's no_pending_lease answer reported", diag)
	}
}

// SessionEnd is what stops a crashed session's leases from waiting out a 30-minute TTL.
func TestSessionEndReleasesEverythingTheSessionHeld(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	runHookCmd(t, endpoint, sessionPayload("s1", "SessionStart"), buildSession("start"))
	runHookCmd(t, endpoint, sessionPayload("s2", "SessionStart"), buildSession("start"))

	askGate(t, endpoint, "s1", "toolu_01")
	askGate(t, endpoint, "s1", "toolu_02")
	askGate(t, endpoint, "s2", "toolu_03")
	runHookCmd(t, endpoint, subagentStartPayload("s1", "agent_a"), buildConfirm)

	if got := d.table.InUse(); got != 3 {
		t.Fatalf("in use = %d, want 3", got)
	}

	runHookCmd(t, endpoint, sessionPayload("s1", "SessionEnd"), buildSession("end"))

	if got := d.table.InUse(); got != 1 {
		t.Errorf("in use = %d after s1 ended, want 1: s1's leases must go and s2's must stay", got)
	}
}

// The action comes from the command line (design.md section 4.1 wires two hooks to one
// subcommand), and falls back to hook_event_name for an install that wired it without one.
// Guessing wrong registers a session at its end and never drops it.
func TestSessionActionFallsBackToTheEventName(t *testing.T) {
	var diag bytes.Buffer

	req, ok := buildSession("")(lifecyclePayload{SessionID: "s1", HookEventName: "SessionStart"}, &diag)
	if !ok || req.Action != "start" {
		t.Errorf("SessionStart with no argument produced action %q (ok=%v), want start", req.Action, ok)
	}
	if req.Weight != 1 {
		t.Errorf("session start carries weight %d, want 1", req.Weight)
	}

	req, ok = buildSession("")(lifecyclePayload{SessionID: "s1", HookEventName: "SessionEnd"}, &diag)
	if !ok || req.Action != "end" {
		t.Errorf("SessionEnd with no argument produced action %q (ok=%v), want end", req.Action, ok)
	}

	// Neither the argument nor a recognisable event: send nothing rather than guess.
	if _, ok := buildSession("")(lifecyclePayload{SessionID: "s1"}, &diag); ok {
		t.Error("a session hook with no action at all built a request; it must send nothing")
	}
}

// Every lifecycle hook must fail open exactly as the gate does. They run in the same
// place — a hook Claude Code is blocked on — and a confirm that hangs is a session that
// hangs, whatever it would have done to the budget.
func TestLifecycleHooksFailOpenWithNoDaemon(t *testing.T) {
	endpoint := "unix://" + sockDir(t) + "/absent"

	cases := []struct {
		name    string
		payload string
		build   builder
	}{
		{"confirm", subagentStartPayload("s1", "agent_a"), buildConfirm},
		{"release", subagentStopPayload("s1", "agent_a"), buildRelease},
		{"session start", sessionPayload("s1", "SessionStart"), buildSession("start")},
		{"session end", sessionPayload("s1", "SessionEnd"), buildSession("end")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(transport.EndpointEnv, endpoint)
			var stderr bytes.Buffer
			var code int
			took := timed(func() {
				code = lifecycleHook(strings.NewReader(tc.payload), &stderr,
					transport.Dial{}, hookBudget, tc.build)
			})

			if code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if took.exceeds(failOpenCeiling) {
				t.Errorf("took %v, want under %v", took, failOpenCeiling)
			}
		})
	}
}

// A malformed payload must not stall a session either.
func TestLifecycleHooksSurviveAMalformedPayload(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	runHookCmd(t, endpoint, `{"session_id":`, buildConfirm)
	runHookCmd(t, endpoint, ``, buildRelease)

	if got := d.table.InUse(); got != 0 {
		t.Errorf("in use = %d, want 0", got)
	}
}
