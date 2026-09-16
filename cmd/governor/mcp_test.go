package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The advisory channel's suite (GOV-16, design.md section 8).
//
// Its subject is the same one the gate's suite has: what happens when the answer cannot be
// obtained. An advisory tool that failed closed would refuse builds on a machine with no
// daemon running at all — punishing precisely the session that bothered to ask — so every
// failure shape in gate_test.go's table is run through this path too, and every one of them
// must end in granted: true.
//
// The other half is agreement. A tool that says yes where the gate says no is worse than no
// tool, because the model acts on it and is then parked; the test at the bottom of this file
// is what holds the two answers together over real commands against a real daemon.

// callMCP drives the server over one line of input and returns the decoded reply.
func callMCP(t *testing.T, dialer transport.Dialer, budget time.Duration, in string) rpcResponse {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := serveMCP(strings.NewReader(in+"\n"), &stdout, &stderr, dialer, budget); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var reply rpcResponse
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply %q: %v (stderr: %s)", stdout.String(), err, stderr.String())
	}
	return reply
}

// askFor is one request_build call, returning the advice the model would read.
//
// It reads structuredContent rather than the text block deliberately: the text is what the
// model sees and the struct is the same values, so asserting on the struct while the text is
// generated from it would let the two drift. It checks they agree instead.
func askFor(t *testing.T, endpoint, command string, dialer transport.Dialer) (advice, string) {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":`+
		`{"name":%q,"arguments":{"command":%q,"why":"a test"}}}`, toolName, command)
	if code := serveMCP(strings.NewReader(req+"\n"), &stdout, &stderr, dialer, adviseBudget); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var reply struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Structured advice `json:"structuredContent"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if reply.Error != nil {
		t.Fatalf("tools/call returned a JSON-RPC error %+v: the tool must always answer, "+
			"because a tool that errors is a tool the model stops calling", reply.Error)
	}
	if len(reply.Result.Content) == 0 {
		t.Fatalf("no content block in %q", stdout.String())
	}

	var fromText advice
	if err := json.Unmarshal([]byte(reply.Result.Content[0].Text), &fromText); err != nil {
		t.Fatalf("content text is not the advice JSON: %q: %v", reply.Result.Content[0].Text, err)
	}
	if fromText != reply.Result.Structured {
		t.Errorf("the text block and structuredContent disagree:\n text = %+v\n struct = %+v\n"+
			"a model reads the text and a program reads the struct; they must be one answer",
			fromText, reply.Result.Structured)
	}
	return reply.Result.Structured, stderr.String()
}

// expensive is a command the classifier gates, and cheap is one it does not. Both are taken
// through classify rather than asserted, so a change to the predicate makes this suite fail
// loudly rather than silently testing the wrong branch.
const (
	expensiveCommand = "go build ./..."
	cheapCommand     = "cat README.md"
)

func TestTheFixtureCommandsAreWhatThisSuiteThinksTheyAre(t *testing.T) {
	if !decide(bashPayload(expensiveCommand)).take {
		t.Fatalf("%q is no longer classified as build-shaped; every capacity case in this "+
			"file is now testing the cheap path instead", expensiveCommand)
	}
	if decide(bashPayload(cheapCommand)).take {
		t.Fatalf("%q is now classified as build-shaped; the no-round-trip test is no longer "+
			"testing what it claims", cheapCommand)
	}
}

func bashPayload(command string) hookPayload {
	var p hookPayload
	p.ToolName = buildToolName
	p.ToolInput.Command = command
	return p
}

// TestTheAdvisoryToolDegradesOpen is GOV-16's third clause, and it is deliberately the same
// table as TestGateFailsOpen.
//
// The failure modes are identical because the client code is: dialAndSend and readLineInto
// are shared with the gate. What is NOT shared is the decision made afterwards, and that is
// what this asserts — every one of these ends in the tool telling the model to go ahead.
func TestTheAdvisoryToolDegradesOpen(t *testing.T) {
	tests := []struct {
		name     string
		endpoint func(t *testing.T) string
		dialer   transport.Dialer
		wantDiag string
	}{
		{
			name:     "no socket at all",
			endpoint: func(t *testing.T) string { return "unix://" + filepath.Join(sockDir(t), "absent") },
			wantDiag: "dial",
		},
		{
			name: "socket file present, nobody listening",
			endpoint: func(t *testing.T) string {
				sock := filepath.Join(sockDir(t), "stale")
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				ln.(*net.UnixListener).SetUnlinkOnClose(false)
				ln.Close()
				return "unix://" + sock
			},
			wantDiag: "dial",
		},
		{
			name: "accepts and never replies",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { select {} })
			},
			wantDiag: "read advice",
		},
		{
			name: "replies with garbage",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte("not json at all\n")) })
			},
			wantDiag: "malformed reply",
		},
		{
			name: "replies with an unterminated line and holds the connection",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":1,"granted":fal`))
					select {}
				})
			},
			wantDiag: "read advice",
		},
		{
			name: "accepts then closes without replying",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {})
			},
			wantDiag: "read advice",
		},
		{
			name: "daemon speaks an unknown protocol version",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":99,"granted":false,"reason":"unsupported_version"}` + "\n"))
				})
			},
			wantDiag: "speaks protocol v99",
		},
		{
			name:     "connect never returns",
			endpoint: func(t *testing.T) string { return "unix://" + filepath.Join(sockDir(t), "unreachable") },
			dialer:   blockedDialer{},
			wantDiag: "dial",
		},
		{
			name:     "endpoint is a scheme v1 does not speak",
			endpoint: func(t *testing.T) string { return "tcp://broker.example:7000" },
			wantDiag: "unsupported endpoint scheme",
		},
		{
			// The daemon refuses, but names a reason this binary has no handling for. The
			// gate fails open on an unrecognised refusal; this must too, and for the same
			// reason — a refusal nobody can interpret is not evidence of anything.
			name: "refused for a reason this binary does not know",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":1,"granted":false,"reason":"quota_exhausted_tier_two"}` + "\n"))
				})
			},
			wantDiag: "does not recognise",
		},
		{
			// THE UPGRADE CASE, AND IT IS NOT HYPOTHETICAL. This exact reply came back from
			// the daemon actually running on the development machine the first time the
			// finished server was pointed at it: a build from before GOV-16, healthy, on a
			// protocol version this binary speaks, which simply has no `advise` operation and
			// says so. Read as a refusal it refuses every build for the length of the
			// upgrade, and nothing about the exchange looks like a failure while it happens.
			name: "daemon predates the advise operation",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":1,"ok":false,"reason":"unknown_op","in_use":0}` + "\n"))
				})
			},
			wantDiag: "does not recognise",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d transport.Dialer = transport.Dial{}
			if tt.dialer != nil {
				d = tt.dialer
			}

			var got advice
			var diag string
			took := timed(func() { got, diag = askFor(t, tt.endpoint(t), expensiveCommand, d) })

			if !got.Granted {
				t.Errorf("granted = false with no reachable daemon: %+v\n"+
					"an advisory tool that fails closed refuses builds on a machine that is "+
					"not governed at all", got)
			}
			if took.exceeds(adviseBudget + failOpenCeiling) {
				t.Errorf("took %v, want under %v", took, adviseBudget+failOpenCeiling)
			}
			if tt.wantDiag != "" && !strings.Contains(diag, tt.wantDiag) {
				t.Errorf("stderr = %q, want it to mention %q: each case must degrade for the "+
					"reason it is named after rather than by luck", diag, tt.wantDiag)
			}
		})
	}
}

// TestACheapCommandCostsNoRoundTrip is GOV-16's second clause and GOV-15's trap.
//
// It is asserted with a dialer that FAILS THE TEST if it is called, rather than by timing:
// a latency assertion would pass on a fast local socket and the regression it guards against
// — someone moving the classify call after the dial — would ship. 81.6% of Bash calls land
// here, and a tool that is slow on `ls` is a tool the model routes around.
func TestACheapCommandCostsNoRoundTrip(t *testing.T) {
	got, _ := askFor(t, "unix:///nonexistent/there/is/no/daemon", cheapCommand, refusingDialer{t})
	if !got.Granted {
		t.Errorf("granted = false for %q: %+v", cheapCommand, got)
	}
	if !strings.Contains(got.Reason, "not build-shaped") {
		t.Errorf("reason = %q, want it to say the command is not governed", got.Reason)
	}
}

// refusingDialer fails the test if anything dials. See TestACheapCommandCostsNoRoundTrip.
type refusingDialer struct{ t *testing.T }

func (d refusingDialer) Dial(context.Context, string) (net.Conn, error) {
	d.t.Errorf("the advisory tool opened a socket for a command the classifier rates cheap; " +
		"clause 2 is that this costs no round trip")
	return nil, fmt.Errorf("must not dial")
}

// TestAnExpensiveCommandIsGrantedWhenThereIsCapacity is the first half of clause 2's second
// sentence, against a real daemon rather than a stub.
func TestAnExpensiveCommandIsGrantedWhenThereIsCapacity(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 6)

	got, _ := askFor(t, endpoint, expensiveCommand, transport.Dial{})
	if !got.Granted {
		t.Errorf("granted = false on an empty machine: %+v", got)
	}
	if got.WaitHintMS != 0 {
		t.Errorf("wait_hint_ms = %d on a grant, want 0", got.WaitHintMS)
	}
}

// TestAnExpensiveCommandIsRefusedWithAWaitHintWhenSaturated is the other half.
//
// The wait hint is the field that makes the refusal actionable — "not now, in about twenty
// seconds" is something a model can do, where a bare no produces either a retry loop or a
// hunt for a different way to build.
func TestAnExpensiveCommandIsRefusedWithAWaitHintWhenSaturated(t *testing.T) {
	endpoint, d := startTestDaemon(t, 1)
	fill(t, d, 1)

	got, _ := askFor(t, endpoint, expensiveCommand, transport.Dial{})
	if got.Granted {
		t.Fatalf("granted = true on a saturated machine: %+v", got)
	}
	if got.WaitHintMS <= 0 {
		t.Errorf("wait_hint_ms = %d, want a positive hint: a refusal a model cannot act on "+
			"produces a retry loop, which is the failure this channel exists to prevent",
			got.WaitHintMS)
	}
	if !strings.Contains(got.Reason, "capacity") {
		t.Errorf("reason = %q, want it to name capacity as the cause", got.Reason)
	}
}

// TestAskingDoesNotConsumeTheBudget is the property that makes this tool safe to call
// speculatively, and it is not obvious from the outside: an advisory question that took a
// lease would be a second, invisible way to exhaust the machine, and the model would be
// punished for cooperating.
func TestAskingDoesNotConsumeTheBudget(t *testing.T) {
	endpoint, d := startTestDaemon(t, 6)

	before := d.table.Slots()
	for i := 0; i < 20; i++ {
		askFor(t, endpoint, expensiveCommand, transport.Dial{})
	}
	if after := d.table.Slots(); after != before {
		t.Errorf("slots %v -> %v after 20 advisory calls, want unchanged", before, after)
	}
	if n := d.table.InUse(); n != 0 {
		t.Errorf("in use = %d after 20 advisory calls, want 0", n)
	}
}

// TestAskingDoesNotCountAsADenial guards the subtler half of the same property.
//
// The gate escalates its refusal text on the daemon's per-session denial count, and by the
// third it is telling the model to stop and hand the machine back to the user. If asking
// politely counted, a model that consulted the tool three times would meet that escalation
// from a gate it had not yet reached — being shouted at for doing the right thing.
func TestAskingDoesNotCountAsADenial(t *testing.T) {
	endpoint, d := startTestDaemon(t, 1)
	fill(t, d, 1)

	for i := 0; i < 5; i++ {
		got, _ := askFor(t, endpoint, expensiveCommand, transport.Dial{})
		if got.Granted {
			t.Fatalf("granted on a full machine")
		}
	}

	// The first real refusal must be the FIRST, denials = 1, not the sixth.
	reply := acquireOverWire(t, endpoint, expensiveCommand)
	if reply.Denials != 1 {
		t.Errorf("denials = %d on the first actual refusal, want 1: advisory calls are "+
			"being counted against the session", reply.Denials)
	}
}

// TestTheAdvisoryToolAgreesWithTheGate is GOV-16's fourth clause.
//
// It runs real commands through BOTH paths against the SAME daemon at the same budget, and
// asserts the two answers agree: whatever the tool says about a command is what the gate
// does with it. A session told "go ahead" and then parked would learn to stop asking, and an
// advisory channel nobody consults still costs a process at every session start.
//
// The commands are drawn from both sides of the classifier and from the corpus shapes
// docs/bash-corpus.md names, so the test covers the cheap path (where the tool answers
// without a daemon at all) as well as the capacity path.
func TestTheAdvisoryToolAgreesWithTheGate(t *testing.T) {
	commands := []string{
		"go build ./...",
		"go test -race ./...",
		"cargo build --release",
		"python -m pytest tests/",
		"npm run build",
		"xcodebuild -scheme App test",
		"cat README.md",
		"grep -rn TODO .",
		"git status",
		"ls -la",
		"echo hello",
		"uv run pytest",
	}

	for _, full := range []bool{false, true} {
		name := "capacity free"
		if full {
			name = "machine saturated"
		}
		t.Run(name, func(t *testing.T) {
			// THE GUARD AGAINST A VACUOUS PASS. "Both said the same thing" is satisfied
			// trivially if everything is granted, which is exactly what a broken fill or a
			// classifier that stopped gating anything would produce — and the suite would go
			// green while asserting nothing. So the saturated run must actually refuse
			// something.
			refused := 0
			for _, cmd := range commands {
				t.Run(cmd, func(t *testing.T) {
					// A fresh daemon per command: a gate call that is GRANTED takes a real
					// lease, so reusing one daemon would let the answers to earlier commands
					// change the answer to later ones and the comparison would be against a
					// moving machine.
					endpoint, d := startTestDaemon(t, 1)
					if full {
						fill(t, d, 1)
					}

					toolSays, _ := askFor(t, endpoint, cmd, transport.Dial{})
					gateSays := gateAllows(t, endpoint, cmd)

					if toolSays.Granted != gateSays {
						t.Errorf("the tool says granted=%v and the gate says allow=%v for %q\n"+
							"reason: %s\n"+
							"one classifier, one answer: a model told go-ahead and then "+
							"refused learns to stop asking",
							toolSays.Granted, gateSays, cmd, toolSays.Reason)
					}
					if !toolSays.Granted {
						refused++
					}
				})
			}
			if full && refused == 0 {
				t.Errorf("not one command was refused on a saturated machine, so this run " +
					"compared two unconditional yeses and proved nothing")
			}
			if !full && refused != 0 {
				t.Errorf("%d commands were refused on an empty machine", refused)
			}
		})
	}
}

// gateAllows runs the real gate over a Bash payload and reports whether the spawn proceeds.
//
// Empty stdout is the grant (design.md section 4.2), and a deny is the only thing that is
// ever written there, so the presence of output is exactly the gate's answer.
func gateAllows(t *testing.T, endpoint, command string) bool {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	payload, err := json.Marshal(map[string]any{
		"session_id":      "gate-vs-tool",
		"hook_event_name": "PreToolUse",
		"tool_name":       buildToolName,
		"tool_input":      map[string]any{"command": command},
		"tool_use_id":     "toolu_agreement",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	var stdout, stderr bytes.Buffer
	// wait_max is shrunk to nothing so the gate answers now rather than parking for twenty
	// seconds. Parking IS the gate saying "no slot right now", which is the answer being
	// compared; waiting it out would only make the suite slow.
	restore := gateWaitMax
	gateWaitMax = 0
	defer func() { gateWaitMax = restore }()

	if code := gate(bytes.NewReader(payload), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion); code != 0 {
		t.Fatalf("gate exit = %d, want 0", code)
	}
	return stdout.Len() == 0
}

// acquireOverWire performs one real acquire and returns the daemon's reply, for the tests
// that need to see the daemon's own bookkeeping rather than the gate's rendering of it.
func acquireOverWire(t *testing.T, endpoint, command string) reply {
	t.Helper()
	in := decide(bashPayload(command))
	req, err := json.Marshal(acquireRequest{
		V: protocolVersion, Op: "acquire", Principal: localPrincipal(),
		Session: "asking-session", ToolUseID: "toolu_real", Tool: buildToolName,
		Kind: in.kind, Class: in.class, Slots: in.weight,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	line, err := callRaw(t, endpoint, string(req)+"\n")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var r reply
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return r
}

// fill takes leases until the machine is at or past its limit, so the capacity tests are
// saturated by the same admission rule everything else uses rather than by a field poke.
func fill(t *testing.T, d *daemon, limit int) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if _, ok := d.table.Acquire(lease.Request{
			Session: "filler", Kind: lease.Build, Weight: 1,
			ToolUseID: fmt.Sprintf("toolu_fill_%d", i),
		}, limit); !ok {
			return
		}
	}
	t.Fatalf("could not saturate a table with limit %d", limit)
}

// TestTheMCPHandshake replays the exact sequence Claude Code sent the probe in
// docs/hook-surface.md section 20.3, verbatim.
//
// It is verbatim on purpose. A handshake written from the specification is a handshake
// against a client nobody has, and the whole reason §20 exists is that this repo does not
// take the harness on documentation. If Claude Code changes what it sends, re-measuring is
// the fix and this test is where the old transcript is recorded.
func TestTheMCPHandshake(t *testing.T) {
	const initialize = `{"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
		`"capabilities":{"roots":{"listChanged":true},"elicitation":{}},` +
		`"clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.273",` +
		`"description":"Anthropic's agentic coding tool","websiteUrl":"https://claude.com/claude-code"}},` +
		`"jsonrpc":"2.0","id":0}`

	reply := callMCP(t, transport.Dial{}, adviseBudget, initialize)

	var got struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	remarshal(t, reply.Result, &got)

	if got.ProtocolVersion != "2025-11-25" {
		t.Errorf("protocolVersion = %q, want the client's own %q echoed back",
			got.ProtocolVersion, "2025-11-25")
	}
	if _, ok := got.Capabilities["tools"]; !ok {
		t.Errorf("capabilities = %v, want it to advertise tools", got.Capabilities)
	}
	if got.ServerInfo.Name != "governor" {
		t.Errorf("serverInfo.name = %q, want %q", got.ServerInfo.Name, "governor")
	}
	if string(reply.ID) != "0" {
		t.Errorf("id = %s, want the request's own id echoed back", reply.ID)
	}
}

// TestANotificationIsNeverAnswered is the one piece of JSON-RPC that a server written from
// memory gets wrong, and Claude Code sends it on every single connection.
//
// `notifications/initialized` has no id. Replying to a notification is a protocol error, and
// the discriminator is the ABSENCE OF AN ID rather than the method name — a server that
// special-cased this one method by name would answer the next notification the harness
// invents.
func TestANotificationIsNeverAnswered(t *testing.T) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`,
		`{"jsonrpc":"2.0","method":"some/method/invented/next/year"}`,
	} {
		var stdout, stderr bytes.Buffer
		if code := serveMCP(strings.NewReader(line+"\n"), &stdout, &stderr, transport.Dial{}, adviseBudget); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if stdout.Len() != 0 {
			t.Errorf("answered a notification with %q; a notification has no id and must be "+
				"met with silence", stdout.String())
		}
	}
}

// TestTheToolIsAdvertised checks the one thing a session actually needs: that tools/list
// names the tool, with command required.
func TestTheToolIsAdvertised(t *testing.T) {
	reply := callMCP(t, transport.Dial{}, adviseBudget, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var got struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	remarshal(t, reply.Result, &got)

	if len(got.Tools) != 1 {
		t.Fatalf("advertised %d tools, want exactly 1", len(got.Tools))
	}
	tool := got.Tools[0]
	if tool.Name != toolName {
		t.Errorf("tool name = %q, want %q", tool.Name, toolName)
	}
	if _, ok := tool.InputSchema.Properties["command"]; !ok {
		t.Errorf("input schema has no `command` property: %v", tool.InputSchema.Properties)
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "command" {
		t.Errorf("required = %v, want exactly [command]: `why` is recorded, not demanded",
			tool.InputSchema.Required)
	}
	if tool.Description == "" {
		t.Errorf("the tool has no description; it is what the model reads when it decides " +
			"whether to call this at all")
	}
}

// TestAMalformedLineDoesNotEndTheSession is why the read loop continues rather than returns.
//
// The server is started once per session and lives for the whole of it (hook-surface.md
// section 20.4). A server that exited on one bad line would take the advisory channel down
// for the rest of that session, and the model would silently stop being able to ask.
func TestAMalformedLineDoesNotEndTheSession(t *testing.T) {
	in := "this is not json\n" +
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"

	var stdout, stderr bytes.Buffer
	if code := serveMCP(strings.NewReader(in), &stdout, &stderr, transport.Dial{}, adviseBudget); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), toolName) {
		t.Errorf("stdout = %q, want the tools/list that followed the bad line to be answered",
			stdout.String())
	}
}

// TestAnUnknownMethodIsAnErrorNotACrash keeps a future harness from taking the server down
// by asking for something it does not have.
func TestAnUnknownMethodIsAnErrorNotACrash(t *testing.T) {
	reply := callMCP(t, transport.Dial{}, adviseBudget,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`)
	if reply.Error == nil {
		t.Fatalf("want a JSON-RPC error for an unimplemented method, got %+v", reply.Result)
	}
	if reply.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601 (method not found)", reply.Error.Code)
	}
}

// remarshal moves a decoded `any` into a typed struct, which is how these tests read a
// result that the server built as a map.
func remarshal(t *testing.T, from any, into any) {
	t.Helper()
	raw, err := json.Marshal(from)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
}
