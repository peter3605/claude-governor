package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// The gate (GOV-05, design.md section 4).
//
// This file is invariant 1: the gate fails open. It runs as the PreToolUse hook on every
// subagent spawn, inside a hook Claude Code is blocked on, so every path through it ends in
// exit 0 with empty stdout within the budget below — no socket, no listener, a listener
// that accepts and never answers, a listener that answers with rubbish, a daemon speaking a
// protocol version this binary does not know. A concurrency limiter that fails badly is
// worse than no limiter at all, because it fails silently and the user blames Claude Code.
//
// What is deliberately NOT here: acting on the answer. The daemon does not exist yet
// (GOV-06), wiring the grant and refusal to real leases is GOV-07, and the deny reason,
// block-and-wait and depth limit are GOV-08. v1 asks the question and ignores the answer.
// That is not a placeholder — it is the order the invariant is built in, so that the
// failure path is proven before anything depends on the success path.

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
// socket deadline fires first and by design, and the select in gate is left as a true last
// resort for the failures no socket deadline can see — chiefly a read of stdin that never
// returns, which happens before there is a socket at all.
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

// acquireResponse is as much of the reply as v1 needs to decide whether it is an answer at
// all. GOV-07 reads the lease out of it; GOV-08 reads the refusal.
type acquireResponse struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
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

func runGate(_ []string, _ io.Writer, stderr io.Writer) int {
	return gate(os.Stdin, stderr, transport.Dial{}, gateBudget)
}

// gate is the whole hook body. It returns the process exit code, which is always 0.
//
// stdout is not a parameter because the gate never writes to it. That is not tidiness: an
// empty stdout with exit 0 is precisely the grant (design.md section 4.2), and the one
// alternative that suggests itself — permissionDecision "allow" — must never be emitted,
// because it suppresses the user's own permission prompts as a side effect. Silence means
// "no opinion", which is what a governor with nothing to say should say.
//
// The work runs in a goroutine and the budget is enforced by the select below rather than
// by the deadlines alone. The deadlines are the real mechanism and cover every case that
// has been thought of; this covers the ones that have not, including the one nothing else
// can — a read of stdin that never returns because the harness did not close it. There is
// deliberately no os.Exit here: an armed exit that a test forgets to disarm reports a pass
// for every test that never ran, which is a mistake this repo has already made once.
func gate(stdin io.Reader, stderr io.Writer, dialer transport.Dialer, budget time.Duration) int {
	deadline := time.Now().Add(budget)

	// Diagnostics go through a writer that can be switched off, so that work still in
	// flight when the budget expires cannot write to stderr after this function has
	// returned and the caller has started reading it.
	diag := &cutoffWriter{w: stderr}
	defer diag.off()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			// A panic on the hot path is still a spawn that must proceed.
			if r := recover(); r != nil {
				fmt.Fprintf(diag, "governor gate: recovered: %v\n", r)
			}
		}()
		gateOnce(stdin, diag, dialer, deadline)
	}()

	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		fmt.Fprintf(diag, "governor gate: budget of %v expired, failing open\n", budget)
	}
	return 0
}

// gateOnce asks the daemon for a lease. Every error it can produce means the same thing to
// the caller — fail open — so they are reported to stderr and otherwise swallowed.
func gateOnce(stdin io.Reader, diag io.Writer, dialer transport.Dialer, deadline time.Time) {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxPayload))
	if err != nil {
		fmt.Fprintf(diag, "governor gate: read stdin: %v\n", err)
		return
	}
	var p hookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		fmt.Fprintf(diag, "governor gate: parse payload: %v\n", err)
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

	endpoint, err := transport.DefaultEndpoint()
	if err != nil {
		fmt.Fprintf(diag, "governor gate: resolve endpoint: %v\n", err)
		return
	}

	// Everything on the socket shares one deadline, set inside the overall budget. One
	// clock, because the session is blocked for the total rather than for any one step.
	ioDeadline := deadline.Add(-ioMargin)

	ctx, cancel := context.WithDeadline(context.Background(), ioDeadline)
	defer cancel()

	conn, err := dialer.Dial(ctx, endpoint)
	if err != nil {
		fmt.Fprintf(diag, "governor gate: dial %s: %v\n", endpoint, err)
		return
	}
	defer conn.Close()

	// The deadline goes on the connection as well as on the dial. This is the case that
	// actually bites: a listener that accepts and then never writes leaves a plain read
	// blocked forever, and a connect-only deadline has already been satisfied by then.
	if err := conn.SetDeadline(ioDeadline); err != nil {
		fmt.Fprintf(diag, "governor gate: set deadline: %v\n", err)
		return
	}

	req := acquireRequestFor(p)
	line, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(diag, "governor gate: encode request: %v\n", err)
		return
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(diag, "governor gate: write request: %v\n", err)
		return
	}

	resp, err := readResponse(conn)
	if err != nil {
		fmt.Fprintf(diag, "governor gate: read response: %v\n", err)
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

	// v1 stops here. The daemon it is talking to does not exist yet, and honouring a
	// refusal is GOV-07's job followed by GOV-08's reason text. Reporting the answer to
	// stderr keeps it visible to `claude --debug` without it reaching the model.
	fmt.Fprintf(diag, "governor gate: daemon answered ok=%v reason=%q (v1 does not act on it)\n",
		resp.OK, resp.Reason)
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

// localPrincipal resolves who is asking, cheaply. USER, then the numeric uid — never
// user.Current(), which can reach a directory service, and nothing on this path may resolve
// a name (design.md section 4.4).
func localPrincipal() principal {
	u := os.Getenv("USER")
	if u == "" {
		u = strconv.Itoa(os.Getuid())
	}
	return principal{User: u}
}

// readResponse reads exactly one newline-terminated JSON line, bounded in both time (the
// connection deadline, set by the caller) and size.
func readResponse(conn net.Conn) (acquireResponse, error) {
	var resp acquireResponse
	r := bufio.NewReaderSize(io.LimitReader(conn, maxResponse), 4096)
	line, err := r.ReadString('\n')
	if err != nil {
		// A reply with no trailing newline is still a reply if the daemon then closed the
		// connection; anything else — a deadline, a reset — is a failure.
		if err != io.EOF || len(line) == 0 {
			return resp, err
		}
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return resp, fmt.Errorf("malformed reply: %w", err)
	}
	return resp, nil
}

// cutoffWriter forwards writes until off is called, then discards them.
//
// It exists so the fail-open path is race-free rather than merely fast: when the budget
// expires the gate returns while its worker may still be mid-write, and without this the
// worker would be writing to stderr as the caller reads it.
type cutoffWriter struct {
	mu   sync.Mutex
	w    io.Writer
	done bool
}

func (c *cutoffWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return len(p), nil
	}
	return c.w.Write(p)
}

func (c *cutoffWriter) off() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = true
}
