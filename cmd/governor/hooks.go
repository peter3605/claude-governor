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

	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The lifecycle hooks (GOV-07, design.md sections 4.1 and 4.3).
//
// confirm, release and session are the other three quarters of the gate's job. The gate
// takes a tentative lease before a spawn; these bind it to the agent that resulted, return
// it when that agent stops, and tell the daemon when a whole session comes and goes.
//
// They are held to the same contract as the gate, because they run in the same place: a
// hook Claude Code is blocked on. Every path exits 0 with an empty stdout inside the
// budget. A confirm that hangs is a session that hangs, and the fact that it would fail
// "safely" from the budget's point of view — the lease stays tentative and is reaped in
// 30 s — is no comfort to a user whose session has stopped.
//
// What is deliberately NOT here: any decision. Only the gate speaks to the model, and only
// to refuse. These three report to stderr and are otherwise silent.

// hookBudget is the wall-clock allowance for one lifecycle hook, matching the gate's
// (design.md section 4.4).
//
// The gate's 100 ms is set by what a user will tolerate on a spawn, and nothing about these
// three makes a person more patient. Sharing the number also means one story cannot quietly
// regress the other's latency: the fail-open suite's ceiling covers all four commands.
const hookBudget = 100 * time.Millisecond

// hookRequest is the confirm, release and session messages of design.md section 7.
//
// One struct for three operations because they overlap almost entirely and the fields an
// operation does not use are simply omitted. The tags match section 7 exactly; the daemon
// decodes all three into its own loose request struct.
type hookRequest struct {
	V         int       `json:"v"`
	Op        string    `json:"op"`
	Principal principal `json:"principal"`
	Session   string    `json:"session"`

	// confirm
	Lease string `json:"lease,omitempty"`

	// confirm, release
	AgentID string `json:"agent_id,omitempty"`

	// confirm, session start
	PID          int   `json:"pid,omitempty"`
	PIDStartedAt int64 `json:"pid_started_at,omitempty"`

	// session
	Action string `json:"action,omitempty"`
	Weight int    `json:"weight,omitempty"`
}

// hookAck is as much of a confirm, release or session reply as a hook needs to tell a
// daemon that acted from one that merely closed the connection. Nothing is acted on.
type hookAck struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	Lease  string `json:"lease"`
	InUse  int    `json:"in_use"`
}

// lifecyclePayload is the union of the SubagentStart, SubagentStop, SessionStart and
// SessionEnd payloads, as measured in docs/hook-surface.md sections 3, 4 and 5.
//
// Decoded loosely, like the gate's: a harness upgrade that renames a field must degrade to
// a thinner request, never to a failed hook.
type lifecyclePayload struct {
	SessionID string `json:"session_id"`
	// AgentID is the agent being started or stopped — on these events it names the new
	// agent, not the caller as it does on PreToolUse (docs/hook-surface.md section 3).
	AgentID string `json:"agent_id"`
	// HookEventName is the fallback for which session action this is, when the subcommand
	// was invoked without one.
	HookEventName string `json:"hook_event_name"`
}

func runConfirm(_ []string, _ io.Writer, stderr io.Writer) int {
	return lifecycleHook(os.Stdin, stderr, transport.Dial{}, hookBudget, buildConfirm)
}

func runRelease(_ []string, _ io.Writer, stderr io.Writer) int {
	return lifecycleHook(os.Stdin, stderr, transport.Dial{}, hookBudget, buildRelease)
}

func runSession(args []string, _ io.Writer, stderr io.Writer) int {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	return lifecycleHook(os.Stdin, stderr, transport.Dial{}, hookBudget, buildSession(action))
}

// builder turns a decoded hook payload into the request to send, or reports that there is
// nothing to send at all.
type builder func(p lifecyclePayload, diag io.Writer) (hookRequest, bool)

// buildConfirm binds a tentative lease to the agent that SubagentStart announced.
//
// It sends no lease ID because there is none to send: SubagentStart carries no tool_use_id
// and shares no per-spawn key with PreToolUse (docs/hook-surface.md section 3), so the
// daemon pairs this with the oldest unconfirmed tentative lease for the session. That is
// the design, not a fallback (design.md section 4.3).
//
// The two hazards this hook has to survive are both the daemon's to handle and both are
// measured facts rather than defensive coding — a resumed agent produces a second
// SubagentStart for an agent_id that already holds a lease, and a resumed subagent produces
// a confirm with no tentative lease waiting at all. Sending the same well-formed request
// either way is what lets the daemon be the single place those are decided.
func buildConfirm(p lifecyclePayload, diag io.Writer) (hookRequest, bool) {
	req := hookRequest{
		V: protocolVersion, Op: "confirm",
		Principal: localPrincipal(),
		Session:   p.SessionID,
		AgentID:   p.AgentID,
	}
	req.PID, req.PIDStartedAt = owningSessionPID(diag)
	return req, true
}

// buildRelease returns a lease at SubagentStop. It keys off agent_id, which that payload
// always carries, so it needs no correlation.
func buildRelease(p lifecyclePayload, _ io.Writer) (hookRequest, bool) {
	return hookRequest{
		V: protocolVersion, Op: "release",
		Principal: localPrincipal(),
		Session:   p.SessionID,
		AgentID:   p.AgentID,
	}, true
}

// buildSession registers or drops a whole session.
//
// The action comes from the command line, because that is how design.md section 4.1 wires
// it — `governor session start` and `governor session end` are two hooks pointing at one
// subcommand. The payload's hook_event_name is the fallback for an install that wired it
// without the argument, which is a real possibility once anyone edits their own settings:
// guessing wrong here would register a session at its end and never drop it.
func buildSession(action string) builder {
	return func(p lifecyclePayload, diag io.Writer) (hookRequest, bool) {
		if action == "" {
			switch p.HookEventName {
			case "SessionStart":
				action = "start"
			case "SessionEnd":
				action = "end"
			}
		}
		if action != "start" && action != "end" {
			fmt.Fprintf(diag, "governor session: no action (arg %q, hook_event_name %q)\n",
				action, p.HookEventName)
			return hookRequest{}, false
		}
		req := hookRequest{
			V: protocolVersion, Op: "session",
			Principal: localPrincipal(),
			Session:   p.SessionID,
			Action:    action,
		}
		if action == "start" {
			req.PID, req.PIDStartedAt = owningSessionPID(diag)
			// Every session's share is equal until max-min fair allocation lands
			// (design.md section 6.1). The field is carried now because a weight that
			// arrives after the wire format has shipped costs far more than one sent from
			// the start.
			req.Weight = 1
		}
		return req, true
	}
}

// owningSessionPID resolves the session process this hook belongs to, for the reaper.
//
// A zero PID means "unknown", and that is a deliberate answer rather than a failure: the
// daemon's PIDLiveness treats a lease with no owner as always alive and leaves it to its
// TTL, whereas a *wrong* owner — the shell that ran the hook, say — is reported dead within
// two seconds and takes a live agent's lease with it. Guessing is the expensive direction,
// so this never guesses.
func owningSessionPID(diag io.Writer) (int, int64) {
	// Bounded: this is a per-spawn path, and the walk reads the process table.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	pid, started, ok := signals.OwningSession(ctx)
	if !ok {
		fmt.Fprintf(diag, "governor: no %s ancestor found; lease will rely on its TTL\n",
			signals.ClaudeProcessName)
		return 0, 0
	}
	if started.IsZero() {
		return pid, 0
	}
	return pid, started.Unix()
}

// lifecycleHook is the body of confirm, release and session.
//
// It is the gate's shape without the decision: same budget, same backstop, same silence on
// stdout. It returns the process exit code, which is always 0.
func lifecycleHook(stdin io.Reader, stderr io.Writer, dialer transport.Dialer, budget time.Duration, build builder) int {
	return withBudget(stderr, budget, func(diag io.Writer, deadline time.Time) {
		var p lifecyclePayload
		if !decodePayload(stdin, diag, &p) {
			return
		}
		req, ok := build(p, diag)
		if !ok {
			return
		}
		var ack hookAck
		if !exchange(diag, dialer, deadline, req, &ack) {
			return
		}
		if ack.V != protocolVersion {
			fmt.Fprintf(diag, "governor %s: daemon speaks protocol v%d, this binary speaks v%d\n",
				req.Op, ack.V, protocolVersion)
			return
		}
		fmt.Fprintf(diag, "governor %s: ok=%v reason=%q lease=%q in_use=%d\n",
			req.Op, ack.OK, ack.Reason, ack.Lease, ack.InUse)
	})
}

// withBudget runs fn under a wall-clock budget and returns 0 whatever happens.
//
// This is invariant 1 made reusable. The work runs in a goroutine and the budget is
// enforced by the select rather than by fn's own deadlines: those are the real mechanism
// and cover every failure that has been thought of, while this covers the ones that have
// not, including the one nothing else can — a read of stdin that never returns because the
// harness did not close it, which happens before there is a socket to put a deadline on.
//
// There is deliberately no os.Exit here. An armed exit that a test forgets to disarm
// reports a pass for every test that never ran, which is a mistake this repo has already
// made once (GOV-02).
func withBudget(stderr io.Writer, budget time.Duration, fn func(diag io.Writer, deadline time.Time)) int {
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
				fmt.Fprintf(diag, "governor: recovered: %v\n", r)
			}
		}()
		fn(diag, deadline)
	}()

	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		fmt.Fprintf(diag, "governor: budget of %v expired, failing open\n", budget)
	}
	return 0
}

// decodePayload reads one bounded hook payload from stdin into v.
func decodePayload(stdin io.Reader, diag io.Writer, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxPayload))
	if err != nil {
		fmt.Fprintf(diag, "governor: read stdin: %v\n", err)
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		fmt.Fprintf(diag, "governor: parse payload: %v\n", err)
		return false
	}
	return true
}

// exchange sends one request to the daemon and decodes one reply into out.
//
// Everything on the socket shares one deadline set inside the overall budget — one clock,
// because the session is blocked for the total rather than for any one step of it. The
// deadline goes on the connection as well as on the dial: a daemon that accepts and then
// never writes leaves a plain read blocked forever, and by then a connect-only deadline has
// already been satisfied.
//
// Every failure means the same thing to every caller — fail open — so they are reported to
// stderr and reduced to a single false.
func exchange(diag io.Writer, dialer transport.Dialer, deadline time.Time, req any, out any) bool {
	endpoint, err := transport.DefaultEndpoint()
	if err != nil {
		fmt.Fprintf(diag, "governor: resolve endpoint: %v\n", err)
		return false
	}

	ioDeadline := deadline.Add(-ioMargin)

	ctx, cancel := context.WithDeadline(context.Background(), ioDeadline)
	defer cancel()

	conn, err := dialer.Dial(ctx, endpoint)
	if err != nil {
		fmt.Fprintf(diag, "governor: dial %s: %v\n", endpoint, err)
		return false
	}
	defer conn.Close()

	if err := conn.SetDeadline(ioDeadline); err != nil {
		fmt.Fprintf(diag, "governor: set deadline: %v\n", err)
		return false
	}

	line, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(diag, "governor: encode request: %v\n", err)
		return false
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(diag, "governor: write request: %v\n", err)
		return false
	}
	if err := readLineInto(conn, out); err != nil {
		fmt.Fprintf(diag, "governor: read response: %v\n", err)
		return false
	}
	return true
}

// readLineInto reads exactly one newline-terminated JSON line, bounded in both time (the
// connection deadline, set by the caller) and size.
func readLineInto(conn net.Conn, out any) error {
	r := bufio.NewReaderSize(io.LimitReader(conn, maxResponse), 4096)
	line, err := r.ReadString('\n')
	if err != nil {
		// A reply with no trailing newline is still a reply if the daemon then closed the
		// connection; anything else — a deadline, a reset — is a failure.
		if err != io.EOF || len(line) == 0 {
			return err
		}
	}
	if err := json.Unmarshal([]byte(line), out); err != nil {
		return fmt.Errorf("malformed reply: %w", err)
	}
	return nil
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

// cutoffWriter forwards writes until off is called, then discards them.
//
// It exists so the fail-open path is race-free rather than merely fast: when the budget
// expires the hook returns while its worker may still be mid-write, and without this the
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
