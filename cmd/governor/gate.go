package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The gate (GOV-05 and GOV-07, design.md section 4).
//
// This file is invariant 1: the gate fails open. It runs as the PreToolUse hook on every
// subagent spawn, inside a hook Claude Code is blocked on, so every path through it where
// the daemon does not give a clear answer ends in exit 0 with empty stdout within the
// budget below — no socket, no listener, a listener that accepts and never answers, a
// listener that answers with rubbish, a daemon speaking a protocol version this binary does
// not know. A concurrency limiter that fails badly is worse than no limiter at all, because
// it fails silently and the user blames Claude Code.
//
// GOV-07 added the one path that is not silent: a daemon that clearly refuses. That is the
// whole of the enforcement, and it is deliberately the narrowest possible opening in
// invariant 1 — a refusal is honoured only when the daemon said so in a reason this binary
// recognises. Everything else, including a refusal this version cannot interpret, still
// fails open.
//
// What is still NOT here: block-and-wait and the depth limit (GOV-08). wait_ms goes out as
// zero because there is no code here to perform a wait, and asking for one the gate cannot
// serve would be a request the daemon honours by holding the session open.

// gateBudget is the whole wall-clock allowance for one gate invocation, covering reading
// stdin, connecting, writing and reading the reply. design.md section 4.4 sets the connect
// deadline at 100 ms; this applies the same number to the invocation end to end, because
// the session is blocked for the total, not for any one step of it.
//
// A var, not a const, so tests can shrink it. Nothing else may write it.
var gateBudget = 100 * time.Millisecond

// ioMargin is how far inside the budget the socket deadline sits.
//
// Without it the connection deadline and the outer backstop expire at the same instant and
// race, and which one reports the failure is a coin toss. That matters more than tidiness:
// if the read deadline were ever dropped, a suite that accepts either mechanism would still
// pass, on the backstop, and the trap this story exists to avoid would be back. So the
// socket deadline fires first and by design, and the select in withBudget is left as a true
// last resort for the failures no socket deadline can see — chiefly a read of stdin that
// never returns, which happens before there is a socket at all.
const ioMargin = 10 * time.Millisecond

// maxPayload bounds the hook payload read from stdin. The payload carries the agent's
// prompt, so it is not small, but it is not unbounded either; a truncated read fails to
// parse and the gate fails open, which is the right answer to a payload this size.
const maxPayload = 1 << 20

// maxResponse bounds one reply line. A daemon that streams without ever sending a newline
// is a hang, and the read deadline alone would not stop it from filling memory first.
const maxResponse = 64 << 10

// principal identifies who is asking. It is single-user in v1 and nothing reads it.
//
// It is carried anyway because retrofitting identity into an already-shipped wire format is
// far more expensive than carrying a field nobody uses, and this is the cheapest moment it
// will ever cost (design.md section 7).
type principal struct {
	User string  `json:"user"`
	Team *string `json:"team"`
}

// acquireRequest is the acquire message of design.md section 7.
type acquireRequest struct {
	V         int       `json:"v"`
	Op        string    `json:"op"`
	Principal principal `json:"principal"`
	Session   string    `json:"session"`
	ToolUseID string    `json:"tool_use_id"`
	Depth     int       `json:"depth"`
	WaitMS    int       `json:"wait_ms"`
}

// acquireResponse is the granted and refused replies of design.md section 7, decoded into
// one struct. The refusal's fields are what let the deny name a cause rather than a number.
type acquireResponse struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Lease  string `json:"lease"`
	Reason string `json:"reason"`
	Budget int    `json:"budget"`
	InUse  int    `json:"in_use"`
	// CPU is a pointer because the daemon has no sampler until GOV-12 and sends null
	// rather than a fabricated 0.0, which would read as an idle machine. A deny must not
	// claim a CPU figure it was not given.
	CPU     *float64        `json:"cpu"`
	Thermal signals.Thermal `json:"thermal"`
}

// hookPayload is the PreToolUse payload, measured in GOV-01 and recorded in
// docs/hook-surface.md. Decoded loosely: a harness upgrade that adds or renames a field
// must degrade to a thinner request, never to a failed hook.
type hookPayload struct {
	SessionID string `json:"session_id"`
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	// AgentID is the *caller*, not the agent about to be created. Absent at top level means
	// the main session is spawning (depth 0); present means a subagent is (depth >= 1).
	// It is the only depth signal the surface carries — there is no depth field
	// (docs/hook-surface.md section 2).
	AgentID string `json:"agent_id"`
}

// denyOutput is the PreToolUse decision Claude Code reads from stdout, measured verbatim in
// GOV-01 (docs/hook-surface.md section 10.2).
//
// permissionDecision is only ever "deny" here. "allow" must never be emitted: it suppresses
// the user's own permission prompts as a side effect, which is a security regression traded
// for nothing. The grant path is exit 0 with empty stdout — silence means "no opinion".
type denyOutput struct {
	HookSpecificOutput denyBody `json:"hookSpecificOutput"`
}

type denyBody struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// refusalsThisVersionHonours are the daemon reasons that become a deny.
//
// The set is closed on purpose. A refusal whose reason this binary does not recognise is
// treated exactly like an unreachable daemon, because during an upgrade there will be a
// daemon of one version and gates of another, and the posture in that window is settled:
// ungoverned spawns are acceptable, a wrongly blocked one is not. GOV-08 adds "depth" here
// when it lands the depth limit.
var refusalsThisVersionHonours = map[string]bool{"capacity": true}

func runGate(_ []string, stdout, stderr io.Writer) int {
	return gate(os.Stdin, stdout, stderr, transport.Dial{}, gateBudget)
}

// gate is the whole hook body. It returns the process exit code, which is always 0 — even
// for a refusal, which is carried on stdout rather than in the exit status
// (docs/hook-surface.md section 10.2).
func gate(stdin io.Reader, stdout, stderr io.Writer, dialer transport.Dialer, budget time.Duration) int {
	return withBudget(stderr, budget, func(diag io.Writer, deadline time.Time) {
		gateOnce(stdin, stdout, diag, dialer, deadline)
	})
}

// gateOnce asks the daemon for a lease and acts on the answer. Every error it can produce
// means the same thing to the caller — fail open — so they are reported to stderr and
// otherwise swallowed.
func gateOnce(stdin io.Reader, stdout, diag io.Writer, dialer transport.Dialer, deadline time.Time) {
	var p hookPayload
	if !decodePayload(stdin, diag, &p) {
		return
	}

	// The hook matcher is a regex covering both spellings of the tool, but a matcher is
	// configuration and configuration drifts. On 2.1.263 the payload's tool_name is Agent
	// and a Task matcher fires for it too (docs/hook-surface.md section 1), so accept
	// either and take a lease for nothing else. An empty tool_name is let through: it means
	// a payload shape this binary does not recognise, and guessing "not an agent" would
	// silently stop governing.
	if p.ToolName != "" && p.ToolName != "Agent" && p.ToolName != "Task" {
		fmt.Fprintf(diag, "governor gate: not a subagent spawn (tool_name %q)\n", p.ToolName)
		return
	}

	var resp acquireResponse
	if !exchange(diag, dialer, deadline, acquireRequestFor(p), &resp) {
		return
	}

	if resp.V != protocolVersion {
		// A version mismatch is answered, not ignored — but the gate treats the answer
		// exactly as it treats an unreachable daemon. During an upgrade there will be a
		// daemon of one version and gates of another, and ungoverned spawns are acceptable
		// where a stalled session is not.
		fmt.Fprintf(diag, "governor gate: daemon speaks protocol v%d, this binary speaks v%d: failing open\n",
			resp.V, protocolVersion)
		return
	}

	if resp.OK {
		// The grant. Nothing is written anywhere: exit 0 and an empty stdout *is* the
		// grant, and the lease is released by the release hook or reaped, never by this
		// process (design.md section 4.3).
		fmt.Fprintf(diag, "governor gate: granted lease %s (%d/%d in use)\n",
			resp.Lease, resp.InUse, resp.Budget)
		return
	}

	if !refusalsThisVersionHonours[resp.Reason] {
		fmt.Fprintf(diag, "governor gate: refused for reason %q, which this binary does not "+
			"recognise: failing open\n", resp.Reason)
		return
	}

	out, err := json.Marshal(denyOutput{denyBody{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: denyReason(resp),
	}})
	if err != nil {
		// Failing to encode a refusal means the spawn proceeds. That is the correct way
		// round: the alternative is a session blocked by a bug in this binary.
		fmt.Fprintf(diag, "governor gate: encode deny: %v: failing open\n", err)
		return
	}
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		fmt.Fprintf(diag, "governor gate: write deny: %v\n", err)
		return
	}
	fmt.Fprintf(diag, "governor gate: refused (%d/%d in use)\n", resp.InUse, resp.Budget)
}

// denyReason is the text that reaches the model verbatim (docs/hook-surface.md section
// 10.2), in the shape design.md section 4.2 specifies.
//
// Name the number, name the cause, and give one concrete instruction. A vague refusal
// produces a retry loop, which is the expensive failure here: the model reads "no" as
// "not yet" and spends the budget it was just denied on asking again.
//
// Every clause after the slot count is conditional, because a clause that states a fact the
// daemon did not send is a fabrication. CPU is null until GOV-12 lands the sampler, and
// thermal is "unknown" on Linux by design.
func denyReason(resp acquireResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fleet at capacity (%d/%d slots", resp.InUse, resp.Budget)
	if resp.CPU != nil {
		fmt.Fprintf(&b, ", CPU %.0f%%", *resp.CPU*100)
	}
	if resp.Thermal != "" && resp.Thermal != signals.Unknown {
		fmt.Fprintf(&b, ", thermal: %s", resp.Thermal)
	}
	b.WriteString("). Run this task inline yourself rather than delegating it. " +
		"Do not retry the Agent call.")
	return b.String()
}

// acquireRequestFor builds the wire request from the hook payload.
func acquireRequestFor(p hookPayload) acquireRequest {
	depth := 0
	if p.AgentID != "" {
		depth = 1
	}
	return acquireRequest{
		V:         protocolVersion,
		Op:        "acquire",
		Principal: localPrincipal(),
		Session:   p.SessionID,
		ToolUseID: p.ToolUseID,
		Depth:     depth,
		// Zero until GOV-08 lands block-and-wait. Asking for a wait the gate has no code to
		// perform would be a request the daemon honours by holding the session open.
		WaitMS: 0,
	}
}
