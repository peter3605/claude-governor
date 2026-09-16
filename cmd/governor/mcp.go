package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// The advisory channel (GOV-16, design.md section 8).
//
// The gate alone produces a bad experience: the model starts a build, is refused, and burns
// tokens learning what it could have been told. This is the half that tells it first — a
// stdio MCP server exposing one tool, `governor__request_build`, which answers "may I run
// this command now" with a boolean and, when the answer is no, with how long to wait.
//
// THREE PROPERTIES DECIDE THE SHAPE OF THIS FILE, and all three are GOV-16's clauses:
//
//  1. IT AGREES WITH THE GATE. The classification is decide(), the SAME function gate.go
//     calls on the same payload shape, in the same process image — not a second predicate
//     that matches it today. The capacity answer comes from the daemon's advise operation,
//     which resolves it through lease.Table.Fits, which IS Acquire's admission rule. A model
//     told "go ahead" and then parked by the gate would learn to stop asking, and an
//     advisory channel nobody consults is worse than none, because it still costs a process.
//
//  2. A CHEAP COMMAND COSTS NO ROUND TRIP. 81.6% of `Bash` calls are not build-shaped
//     (design.md section 5.5), and those are answered by a ~5 us pure function with no
//     socket, exactly as the gate answers them. A tool that is slow on `ls` gets worked
//     around, and a worked-around tool is a worked-around governor.
//
//  3. IT DEGRADES OPEN. Every failure that ends the gate's decision in "proceed" ends this
//     one in granted: true. Invariant 1 is not weaker here for being off the hot path —
//     advice that failed closed would stall a session that was trying to cooperate, which is
//     precisely the session that should be rewarded.
//
// It is also inside the thing being measured. A stdio MCP server is a descendant of the
// session process and its CPU lands in claude_cpu, unlike the HTTP and native-host servers
// of phase-0-findings.md section 5 — and hook-surface.md section 20.4 measured that it is
// started at EVERY session start, called or not. So: no sampler, no ticker, no goroutine
// that outlives a call, and the same one static binary as everything else.

// mcpProtocolVersion is the MCP protocol version this server reports.
//
// It is echoed from the client's initialize when the client offers one, and this is only the
// fallback for a client that offers nothing. Claude Code 2.1.273 sends "2025-11-25"
// (hook-surface.md section 20.3); echoing rather than asserting is what the probe measured
// working, and a server that insisted on its own version was not tested.
const mcpProtocolVersion = "2025-11-25"

// adviseBudget is the whole allowance for asking the daemon about one command.
//
// It is larger than the gate's 100 ms (design.md section 4.4) because the two are blocking
// different things: the gate runs inside a hook the harness is blocked on, where the cost of
// waiting is a stalled session, while this is a tool call the model chose to make. It is
// still bounded and still small, because the model is waiting on it either way and a tool
// that takes a visible moment to say "yes" is one that gets skipped next time.
//
// A var, not a const, so tests can shrink it. Nothing else may write it.
var adviseBudget = 250 * time.Millisecond

// toolName is the tool as this server advertises it.
//
// A session sees it as `mcp__plugin_claude-governor_governor__governor__request_build`.
// Claude Code PREFIXES `mcp__plugin_<plugin>_<server>__` onto this name rather than replacing
// it, so `governor` lands in the string three times — measured in hook-surface.md section
// 20.5, after the obvious guess turned out to be wrong and cost a probe run. Anything that
// allowlists this tool, or names it to the model in prose, needs that spelling and not this
// one; a mismatch presents as the model reporting it lacks permission, which reads exactly
// like a server that failed to start.
//
// The `governor__` here is what makes it stutter, and it stays because design.md section 8
// specifies this name. Dropping it is an interface change, not a tidy-up.
const toolName = "governor__request_build"

// toolDescription is what the model reads when it decides whether to call this at all, so it
// is written for that decision and not as documentation.
//
// It says what the tool is FOR, what it costs (nothing, for the common case), and what to do
// with each answer — because a tool whose refusal the model cannot act on produces a retry
// loop, which is the expensive failure mode this whole channel exists to avoid.
const toolDescription = `Ask whether this machine has capacity to run an expensive build or test command right now.

Call this BEFORE starting a build, test run, or other heavy command (go build, go test, cargo, pytest, npm run build, xcodebuild, make...). Cheap commands are answered instantly and do not need to be asked about.

Returns granted=true (run it now), or granted=false with wait_hint_ms (do not run it; wait that long and ask again). A refusal means other work on this machine is using every slot — it is not a fault in your command, and running it anyway will be refused by the gate.`

// advisoryRefusalsHonoured are the daemon reasons this binary will turn into a "no".
//
// THE SET IS CLOSED, for the reason the gate's refusalsThisVersionHonours is closed, and a
// live daemon proved the point before this was written. An older daemon — one from before
// GOV-16 — does not know the `advise` op at all and answers `{"ok":false,
// "reason":"unknown_op"}` in protocol v1, which is a version this binary speaks. Read as a
// refusal, that answer refuses EVERY build for the whole of an upgrade window: the tool
// would tell a cooperating session that a machine with a perfectly healthy daemon is full,
// and it would do it silently, because nothing about that exchange looks like a failure.
//
// So an unrecognised reason is treated exactly as an unreachable daemon is. The posture is
// the one invariant 8 settles for the gate and it is no different here: during an upgrade
// there will be a daemon of one version and clients of another, and an ungoverned build is
// acceptable where a wrong refusal is not.
var advisoryRefusalsHonoured = map[string]bool{"capacity": true, "depth": true}

// requestBuildArgs is the tool's input. `command` is the whole point; `why` is accepted
// because design.md section 8 specifies it and because a one-line reason is what makes the
// daemon's log readable by a person, but nothing decides anything on it.
type requestBuildArgs struct {
	Command string `json:"command"`
	Why     string `json:"why"`
}

// adviseRequest is the advise message of design.md section 7.
type adviseRequest struct {
	V         int       `json:"v"`
	Op        string    `json:"op"`
	Principal principal `json:"principal"`
	Session   string    `json:"session"`
	Kind      string    `json:"kind"`
	Class     string    `json:"class"`
	Slots     float64   `json:"slots"`
}

// adviseResponse is the daemon's answer, decoded.
type adviseResponse struct {
	V          int     `json:"v"`
	Granted    bool    `json:"granted"`
	WaitHintMS int     `json:"wait_hint_ms"`
	Reason     string  `json:"reason"`
	Budget     int     `json:"budget"`
	InUse      int     `json:"in_use"`
	Slots      float64 `json:"slots"`
}

// advice is what the tool answers, and it is the shape design.md section 8 specifies:
// granted, wait_hint_ms, reason.
type advice struct {
	Granted    bool   `json:"granted"`
	WaitHintMS int    `json:"wait_hint_ms"`
	Reason     string `json:"reason"`
}

func runMCP(_ []string, stdout, stderr io.Writer) int {
	return serveMCP(os.Stdin, stdout, stderr, transport.Dial{}, adviseBudget)
}

// serveMCP is the whole server: one JSON-RPC message per line, in and out.
//
// It returns 0 on a clean end of input, which is how Claude Code stops a stdio server — it
// closes stdin. Every other failure is per-message and answered on the wire, because a
// server that exits on a bad line takes the advisory channel down for the rest of the
// session over one malformed request.
func serveMCP(stdin io.Reader, stdout, stderr io.Writer, dialer transport.Dialer, budget time.Duration) int {
	in := bufio.NewScanner(stdin)
	// A tool call carries a whole command line and an initialize carries the client's
	// capabilities, so the default 64 KiB token is generous but not obviously enough; this
	// matches maxPayload, the bound the gate puts on a hook payload for the same reason.
	in.Buffer(make([]byte, 0, 64<<10), maxPayload)

	out := json.NewEncoder(stdout)
	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		reply, send := handleRPC(line, stderr, dialer, budget)
		if !send {
			continue
		}
		if err := out.Encode(reply); err != nil {
			// stdout is the transport. If it cannot be written the session is gone, and
			// there is nothing left for this process to do.
			fmt.Fprintf(stderr, "governor mcp: write reply: %v\n", err)
			return 0
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintf(stderr, "governor mcp: read: %v\n", err)
	}
	return 0
}

// rpcRequest is one JSON-RPC 2.0 message.
//
// ID IS A json.RawMessage AND IS COMPARED AGAINST NIL, which is the discriminator between a
// request and a notification and is the one piece of JSON-RPC this server must not get
// wrong. Claude Code sends `notifications/initialized` with no id (hook-surface.md section
// 20.3) and answering it is a protocol error. Decoding the id as a number would also be
// wrong: the spec allows a string, and echoing back something of a different type than was
// sent is how a client comes to think its request is still outstanding.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleRPC answers one message. The bool reports whether there is anything to send: a
// notification, and a line that is not JSON at all, are both answered with silence.
func handleRPC(line []byte, diag io.Writer, dialer transport.Dialer, budget time.Duration) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// No id could be recovered, so there is nobody to answer. Dropping it is the only
		// option the protocol leaves, and it is better than guessing an id.
		fmt.Fprintf(diag, "governor mcp: malformed message: %v\n", err)
		return rpcResponse{}, false
	}
	if req.ID == nil {
		// A notification. The client is not waiting for anything and MUST NOT be replied to.
		return rpcResponse{}, false
	}

	reply := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		reply.Result = initializeResult(req.Params)
	case "tools/list":
		reply.Result = toolsListResult()
	case "tools/call":
		reply.Result = callTool(req.Params, diag, dialer, budget)
	case "ping":
		// The spec's keepalive. An empty result is the whole of it.
		reply.Result = struct{}{}
	default:
		reply.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return reply, true
}

func initializeResult(params json.RawMessage) map[string]any {
	// Echo the client's protocol version rather than asserting our own. This is what the
	// probe in hook-surface.md section 20 measured working against 2.1.273; a server that
	// answered with a different version was not tested, and this is not the file to find out
	// in.
	version := mcpProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "governor", "version": binaryVersion},
	}
}

func toolsListResult() map[string]any {
	return map[string]any{"tools": []any{map[string]any{
		"name":        toolName,
		"description": toolDescription,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The exact command you are about to run.",
				},
				"why": map[string]any{
					"type":        "string",
					"description": "One line on what it is for. Recorded, not judged.",
				},
			},
			"required": []any{"command"},
		},
	}}}
}

// callTool answers tools/call. It never returns a JSON-RPC error: a tool that errors teaches
// the model the tool is broken, and this one has a correct answer for every input — when it
// cannot find out, the answer is yes (clause 3).
func callTool(params json.RawMessage, diag io.Writer, dialer transport.Dialer, budget time.Duration) map[string]any {
	var p struct {
		Name      string           `json:"name"`
		Arguments requestBuildArgs `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		fmt.Fprintf(diag, "governor mcp: malformed tools/call: %v\n", err)
		return toolResult(advice{Granted: true, Reason: "advisory unavailable: malformed request"})
	}
	if p.Name != toolName {
		return toolResult(advice{Granted: true, Reason: "unknown tool " + p.Name})
	}
	return toolResult(adviseOn(p.Arguments, diag, dialer, budget))
}

// adviseOn is the answer to one question, and the ordering in it is clause 2.
//
// The classifier runs FIRST and, four times in five, is the whole of the decision — no
// socket is opened, no endpoint is resolved, nothing is dialled. Only a command the gate
// would actually take a lease for is worth a round trip, and asking the daemon about `ls`
// would spend more time in the kernel than the command does.
func adviseOn(args requestBuildArgs, diag io.Writer, dialer transport.Dialer, budget time.Duration) advice {
	// The same function the gate calls, on the same payload shape. This is not a
	// reimplementation that happens to agree — it is the identical code path, and that is
	// what clause 4 asks for.
	in := decide(hookPayload{
		ToolName: buildToolName,
		ToolInput: struct {
			To      string `json:"to"`
			Command string `json:"command"`
		}{Command: args.Command},
	})
	if !in.take {
		return advice{Granted: true, Reason: "not build-shaped: this command is not governed, run it"}
	}

	resp, err := askDaemon(dialer, budget, adviseRequest{
		V:         protocolVersion,
		Op:        "advise",
		Principal: localPrincipal(),
		// NO SESSION IS SENT, and that is deliberate rather than an omission. This process
		// is not a hook and never receives a hook payload, so the only way it could name a
		// session is an environment variable — and no environment variable carrying one was
		// measured in hook-surface.md section 20.1, which recorded exactly what the harness
		// puts in this process's environment. Nothing in advise reads it: the answer is
		// about the machine, not about the asker. A later story that needs the correlation
		// has a measured way to get it, `_meta.claudecode/toolUseId` on the tools/call
		// (section 20.3), and should use that rather than guessing at a variable name.
		Session: "",
		Kind:    in.kind,
		Class:   in.class,
		Slots:   in.weight,
	})
	if err != nil {
		// Degrade open, and SAY SO in the reason rather than reporting a bare yes. A model
		// that is told the governor is unreachable can mention it; one told only "granted"
		// cannot tell an idle machine from an absent daemon.
		fmt.Fprintf(diag, "governor mcp: %v\n", err)
		return advice{Granted: true, Reason: "governor unavailable: not governing this command"}
	}
	if resp.Granted {
		return advice{Granted: true, Reason: fmt.Sprintf(
			"capacity available (%s/%d build slots in use)", formatSlots(resp.Slots), resp.Budget)}
	}
	if !advisoryRefusalsHonoured[resp.Reason] {
		fmt.Fprintf(diag, "governor mcp: daemon refused for reason %q, which this binary does "+
			"not recognise: not governing this command\n", resp.Reason)
		return advice{Granted: true, Reason: "governor unavailable: not governing this command"}
	}
	if resp.Reason == "depth" {
		return advice{Granted: false, Reason: "nested subagent spawns do not get build slots; " +
			"run this in the main session"}
	}
	return advice{
		Granted:    false,
		WaitHintMS: resp.WaitHintMS,
		Reason: fmt.Sprintf("machine at capacity (%s/%d build slots in use). Do not run this "+
			"command now: %s and ask again. This is not a build failure.",
			formatSlots(resp.Slots), resp.Budget, waitAdvice(resp.WaitHintMS)),
	}
}

// waitAdvice turns the hint into the instruction half of the refusal.
//
// A hint of zero must never render as "wait about 0s", which reads as "retry immediately"
// and produces exactly the tight retry loop this channel exists to prevent. A daemon that
// refuses without a hint is telling the model to wait for an unknown period, and that is what
// the text then says.
func waitAdvice(hintMS int) string {
	if hintMS <= 0 {
		return "wait for other builds on this machine to finish"
	}
	return fmt.Sprintf("wait about %ds", (hintMS+999)/1000)
}

// askDaemon performs the round trip, bounded in time by the caller's budget.
//
// Every error it can return means the same thing to adviseOn — grant — so they are
// distinguished only for the log. This mirrors the gate's structure deliberately: the set of
// failures is exactly the set gate_test.go's table enumerates, and mcp_test.go runs the same
// table through here.
func askDaemon(dialer transport.Dialer, budget time.Duration, req adviseRequest) (adviseResponse, error) {
	var resp adviseResponse

	conn, replies, err := dialAndSend(dialer, time.Now().Add(budget), req)
	if err != nil {
		return resp, err
	}
	defer conn.Close()

	if err := readLineInto(replies, &resp); err != nil {
		return resp, fmt.Errorf("read advice: %w", err)
	}
	if resp.V != protocolVersion {
		// A daemon speaking another protocol version is treated exactly as an unreachable
		// one, for the reason invariant 8 gives: during an upgrade there will be a daemon of
		// one version and clients of another, and an ungoverned build is acceptable where a
		// wrong answer is not.
		return resp, fmt.Errorf("daemon speaks protocol v%d, this binary speaks v%d", resp.V, protocolVersion)
	}
	return resp, nil
}

// toolResult wraps the advice in the MCP content envelope.
//
// It is sent BOTH as text and as structuredContent: the text is what a model reads, and the
// JSON is what anything programmatic reads. The text is the JSON rather than prose because
// the fields are the interface design.md section 8 specifies, and a model that gets prose
// back has to parse a sentence to find wait_hint_ms.
func toolResult(a advice) map[string]any {
	body, err := json.Marshal(a)
	if err != nil {
		// Unreachable for this struct, and still not a place to return an error: a tool
		// call that fails is a tool the model stops trusting.
		body = []byte(`{"granted":true,"wait_hint_ms":0,"reason":"advisory unavailable"}`)
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(body)}},
		"structuredContent": a,
	}
}
