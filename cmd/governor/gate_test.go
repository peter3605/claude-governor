package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// The fail-open suite (GOV-05).
//
// Its subject is not the gate's feature, it is the gate's failure. Every test here asserts
// the same three things — exit 0, empty stdout, and back inside the latency ceiling — over
// every way the far end can go wrong. design.md section 4.4 sets the budget at 100 ms; the
// ceiling asserted here is 120 ms, the budget plus the slack a hook process is allowed to
// take getting to and from the work.
//
// These tests must keep passing unchanged when GOV-07 wires the gate to a real daemon. The
// most likely regression there is a path that starts waiting on the daemon in a case that
// used to short-circuit, and that regression is invisible except here.
const failOpenCeiling = 120 * time.Millisecond

// samplePayload is a real PreToolUse payload for a depth-0 spawn, from
// docs/hook-surface.md section 1 with the prompt trimmed.
const samplePayload = `{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e",` +
	`"cwd":"/tmp/work","prompt_id":"77e4b2be-6d77-422e-9f12-a613e4c7138b",` +
	`"permission_mode":"bypassPermissions","hook_event_name":"PreToolUse","tool_name":"Agent",` +
	`"tool_input":{"description":"Count lines","subagent_type":"general-purpose"},` +
	`"tool_use_id":"toolu_013XAeD4MymdfZHskv22UtK3"}`

// runGateFor exercises the gate against one endpoint and asserts the fail-open contract.
// It returns the stderr diagnostics so a test can check the gate reported the right reason.
func runGateFor(t *testing.T, endpoint, payload string, dialer transport.Dialer) string {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := gate(strings.NewReader(payload), &stdout, &stderr, dialer, gateBudget)
	elapsed := time.Since(start)

	if code != 0 {
		t.Errorf("exit code = %d, want 0: the gate must never fail a spawn", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty: the grant path is exit 0 and silence, never a decision", stdout.String())
	}
	if elapsed > failOpenCeiling {
		t.Errorf("took %v, want under %v", elapsed.Round(time.Millisecond), failOpenCeiling)
	}
	return stderr.String()
}

// sockDir is a directory short enough to hold a unix socket path.
//
// Not t.TempDir(): it embeds the test name in the path, and a unix socket path is limited to
// 104 bytes on darwin (108 on Linux). Several of the names below are long enough to blow
// that, and the failure is `bind: invalid argument` — which reads like a bad argument to
// Listen rather than like a path length, and costs a while to recognise.
func sockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gov")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// listenerFunc starts a unix listener at a fresh path and serves each connection with h,
// handing it the request line the gate sent. It returns the endpoint URL. The listener is
// closed when the test ends; h may block indefinitely, and the close is what unblocks it.
//
// The harness reads the request before h runs because that is what a daemon does, and
// because not doing it made two tests racy: a handler that closed or replied while the gate
// was still writing produced EPIPE on the write instead of the read failure being tested.
// The gate failed open either way — that is the contract and it held — but a suite that
// reaches the read only sometimes is not evidence about the read.
func listenerFunc(t *testing.T, h func(c net.Conn, request []byte)) string {
	t.Helper()
	sock := filepath.Join(sockDir(t), "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				// Generous relative to the gate's 100 ms budget: this deadline only exists
				// so a gate that never writes cannot wedge the goroutine for the whole run.
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				req, _ := bufio.NewReader(c).ReadString('\n')
				c.SetReadDeadline(time.Time{})
				h(c, []byte(req))
			}()
		}
	}()
	return "unix://" + sock
}

// blockedDialer never returns from Dial until ctx is done. It is how the connect half of
// the deadline is tested: on darwin a unix connect to a listener with a full backlog is
// refused immediately rather than blocking, so a real socket cannot produce a hung connect
// portably. The seam can, which is one of the things the seam is for.
type blockedDialer struct{}

func (blockedDialer) Dial(ctx context.Context, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// blockingReader never returns from Read and never reaches EOF. It stands in for a harness
// that does not close the hook's stdin — the one hang no deadline on the socket can catch,
// because the gate never gets as far as the socket.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestGateFailsOpen(t *testing.T) {
	tests := []struct {
		name string
		// endpoint is built per test, since most need a live listener.
		endpoint func(t *testing.T) string
		dialer   transport.Dialer
		payload  string
		// wantDiag is a fragment the stderr diagnostic must contain, so that each case is
		// shown to fail for the reason it is named after rather than by luck.
		wantDiag string
	}{
		{
			name: "no socket at all",
			endpoint: func(t *testing.T) string {
				return "unix://" + filepath.Join(sockDir(t), "absent")
			},
			wantDiag: "dial",
		},
		{
			name: "socket file present, nobody listening",
			endpoint: func(t *testing.T) string {
				// A daemon that died leaves its socket inode behind. Go unlinks the file on
				// Close by default, which is the one thing that would NOT reproduce this.
				sock := filepath.Join(sockDir(t), "stale")
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				ln.(*net.UnixListener).SetUnlinkOnClose(false)
				ln.Close()
				if _, err := os.Stat(sock); err != nil {
					t.Fatalf("stale socket should still exist: %v", err)
				}
				return "unix://" + sock
			},
			wantDiag: "dial",
		},
		{
			name: "accepts and never replies",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { select {} })
			},
			wantDiag: "read response",
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
				// The nastiest shape: bytes arrive, so "did anything come back?" says yes,
				// but the line never ends. Only a read deadline gets out of this.
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":1,"ok":tr`))
					select {}
				})
			},
			wantDiag: "read response",
		},
		{
			name: "accepts then closes without replying",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {})
			},
			wantDiag: "read response",
		},
		{
			name: "daemon speaks an unknown protocol version",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":99,"ok":false,"reason":"unsupported_version","supported":[99]}` + "\n"))
				})
			},
			wantDiag: "failing open",
		},
		{
			name: "connect never returns",
			endpoint: func(t *testing.T) string {
				return "unix://" + filepath.Join(sockDir(t), "unreachable")
			},
			dialer: blockedDialer{},
			// The dial is what gives up here, on the context deadline gate passes it — not
			// the backstop. The backstop's own case is the blocked stdin below.
			wantDiag: "dial",
		},
		{
			name: "endpoint is a scheme v1 does not speak",
			endpoint: func(t *testing.T) string {
				return "tcp://broker.example:7000"
			},
			wantDiag: "unsupported endpoint scheme",
		},
		{
			name: "payload is not JSON",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":1,"ok":true}` + "\n"))
				})
			},
			payload:  "this is not a hook payload",
			wantDiag: "parse payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := tt.payload
			if payload == "" {
				payload = samplePayload
			}
			var d transport.Dialer = transport.Dial{}
			if tt.dialer != nil {
				d = tt.dialer
			}
			diag := runGateFor(t, tt.endpoint(t), payload, d)
			if !strings.Contains(diag, tt.wantDiag) {
				t.Errorf("stderr = %q, want it to mention %q", diag, tt.wantDiag)
			}
		})
	}
}

// TestGateFailsOpenWhenStdinNeverCloses is separate because it cannot use the table's
// payload: the point is a reader that never returns at all.
func TestGateFailsOpenWhenStdinNeverCloses(t *testing.T) {
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := gate(blockingReader{}, &stdout, &stderr, transport.Dial{}, gateBudget)
	elapsed := time.Since(start)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if elapsed > failOpenCeiling {
		t.Errorf("took %v, want under %v", elapsed.Round(time.Millisecond), failOpenCeiling)
	}
	if !strings.Contains(stderr.String(), "budget") {
		t.Errorf("stderr = %q, want it to report the expired budget", stderr.String())
	}
}

// TestGateIsSilentOnAGrant is what is left of GOV-05's TestGateIsSilentOnEveryAnswer.
//
// That test pinned the v1 boundary — the gate asks and, whatever comes back, stays silent —
// and said in its own comment that it was there to notice if GOV-07 arrived early and
// half-built. GOV-07 is this story, so the half of it covering a refusal has moved to
// TestGateDeniesWhenTheDaemonRefuses below. The grant half is unchanged and stays here,
// because it is the more important of the two: silence *is* the grant, and a gate that
// started writing something on the happy path would suppress the user's own permission
// prompts on every spawn.
func TestGateIsSilentOnAGrant(t *testing.T) {
	reply := `{"v":1,"ok":true,"lease":"L-0d41","budget":6,"in_use":4,"share":2,"source":"local"}`
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(reply + "\n")) })

	diag := runGateFor(t, endpoint, samplePayload, transport.Dial{})
	if !strings.Contains(diag, "L-0d41") {
		t.Errorf("stderr = %q, want the granted lease named", diag)
	}
}

// TestGateDeniesWhenTheDaemonRefuses is the enforcement, and the only path in this file
// where the gate is not silent.
//
// It goes through runGateDeny rather than runGateFor because the two assert opposite things
// about stdout, which is the entire difference GOV-07 introduced.
func TestGateDeniesWhenTheDaemonRefuses(t *testing.T) {
	reply := `{"v":1,"ok":false,"reason":"capacity","budget":6,"in_use":6,` +
		`"cpu":0.94,"thermal":"fair","source":"local"}`
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(reply + "\n")) })

	out := runGateDeny(t, endpoint, samplePayload)

	if got := out.HookSpecificOutput.HookEventName; got != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", got)
	}
	if got := out.HookSpecificOutput.PermissionDecision; got != "deny" {
		t.Errorf("permissionDecision = %q, want deny", got)
	}

	// The reason reaches the model verbatim (docs/hook-surface.md section 10.2), and
	// design.md section 4.2 requires it to name the number, name the cause, and give one
	// concrete instruction. A vague refusal produces a retry loop.
	reason := out.HookSpecificOutput.PermissionDecisionReason
	for _, want := range []string{"6/6", "94%", "fair", "inline", "Do not retry"} {
		if !strings.Contains(reason, want) {
			t.Errorf("permissionDecisionReason = %q, missing %q", reason, want)
		}
	}
}

// A deny must never state a fact the daemon did not send. cpu is null until GOV-12 lands
// the sampler and thermal is "unknown" on Linux by design, so both clauses are conditional
// — and a reason that says "CPU 0%" on a machine nobody has measured is a fabrication that
// a person reading the refusal has no way to catch.
func TestGateDenyClaimsOnlyWhatTheDaemonSent(t *testing.T) {
	reply := `{"v":1,"ok":false,"reason":"capacity","budget":4,"in_use":4,` +
		`"cpu":null,"thermal":"unknown","source":"local"}`
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(reply + "\n")) })

	reason := runGateDeny(t, endpoint, samplePayload).HookSpecificOutput.PermissionDecisionReason

	if !strings.Contains(reason, "4/4") {
		t.Errorf("permissionDecisionReason = %q, want the slot count", reason)
	}
	for _, unwanted := range []string{"CPU", "thermal"} {
		if strings.Contains(reason, unwanted) {
			t.Errorf("permissionDecisionReason = %q, must not mention %q when the daemon sent none",
				reason, unwanted)
		}
	}
}

// A refusal this binary cannot interpret is treated exactly like an unreachable daemon.
//
// This is the narrowest reading of invariant 1 that still enforces anything, and it is the
// upgrade posture design.md section 7 settles: during an upgrade there will be a daemon of
// one version and gates of another, and a gate that honoured a reason it did not understand
// would block spawns on a policy it cannot explain to the user. Ungoverned spawns are
// acceptable in that window; a wrongly blocked one is not.
func TestGateFailsOpenOnARefusalItDoesNotUnderstand(t *testing.T) {
	reply := `{"v":1,"ok":false,"reason":"quota_exhausted_tier_two","budget":6,"in_use":2,"source":"remote"}`
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(reply + "\n")) })

	diag := runGateFor(t, endpoint, samplePayload, transport.Dial{})
	if !strings.Contains(diag, "does not recognise") {
		t.Errorf("stderr = %q, want it to name the unrecognised reason", diag)
	}
}

// runGateDeny runs the gate against a refusing daemon and returns the decoded decision. It
// asserts the parts of the contract a deny shares with a grant — exit 0, inside the
// latency ceiling — and that stdout carries exactly one well-formed decision.
func runGateDeny(t *testing.T, endpoint, payload string) denyOutput {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := gate(strings.NewReader(payload), &stdout, &stderr, transport.Dial{}, gateBudget)
	elapsed := time.Since(start)

	// A deny is still exit 0. The decision travels on stdout; a non-zero exit would be a
	// hook error, which is a different thing entirely and is not what a refusal means.
	if code != 0 {
		t.Errorf("exit code = %d, want 0 even for a deny (stderr: %s)", code, stderr.String())
	}
	if elapsed > failOpenCeiling {
		t.Errorf("took %v, want under %v", elapsed.Round(time.Millisecond), failOpenCeiling)
	}

	var out denyOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout = %q, want one PreToolUse decision object: %v", stdout.String(), err)
	}
	return out
}

// TestGateSendsTheAcquireRequest checks what actually goes on the wire. Nothing reads it
// yet, so without this the request could be wrong in every field and every other test here
// would still pass.
func TestGateSendsTheAcquireRequest(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantDepth int
		wantSess  string
		wantTool  string
	}{
		{
			name:      "depth 0 when no agent_id: the main session is spawning",
			payload:   samplePayload,
			wantDepth: 0,
			wantSess:  "2ed4bf1c-470b-4e08-9b48-4f310663cb6e",
			wantTool:  "toolu_013XAeD4MymdfZHskv22UtK3",
		},
		{
			// docs/hook-surface.md section 2: on PreToolUse, agent_id names the *caller*.
			// Its presence is the only depth signal the hook surface carries.
			name: "depth 1 when agent_id is present: a subagent is spawning",
			payload: `{"session_id":"s2","agent_id":"a0dca3e4fa79a2f6f","agent_type":"general-purpose",` +
				`"hook_event_name":"PreToolUse","tool_name":"Agent","tool_use_id":"toolu_nested"}`,
			wantDepth: 1,
			wantSess:  "s2",
			wantTool:  "toolu_nested",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make(chan []byte, 1)
			endpoint := listenerFunc(t, func(c net.Conn, request []byte) {
				got <- request
				c.Write([]byte(`{"v":1,"ok":true}` + "\n"))
			})
			runGateFor(t, endpoint, tt.payload, transport.Dial{})

			var line []byte
			select {
			case line = <-got:
			case <-time.After(time.Second):
				t.Fatal("daemon received no request")
			}
			if !bytes.HasSuffix(line, []byte("\n")) {
				t.Errorf("request %q is not newline-terminated; the protocol is newline-delimited JSON", line)
			}

			var req acquireRequest
			if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
				t.Fatalf("request is not JSON: %v (%q)", err, line)
			}
			if req.V != protocolVersion {
				t.Errorf("v = %d, want %d: every message carries the protocol version", req.V, protocolVersion)
			}
			if req.Op != "acquire" {
				t.Errorf("op = %q, want %q", req.Op, "acquire")
			}
			if req.Session != tt.wantSess {
				t.Errorf("session = %q, want %q", req.Session, tt.wantSess)
			}
			if req.ToolUseID != tt.wantTool {
				t.Errorf("tool_use_id = %q, want %q", req.ToolUseID, tt.wantTool)
			}
			if req.Depth != tt.wantDepth {
				t.Errorf("depth = %d, want %d", req.Depth, tt.wantDepth)
			}
			// GOV-08: the gate asks to be held rather than refused. The daemon is free to
			// ignore it, and an older daemon does; what matters here is that the ask is on
			// the wire and matches what the gate is actually prepared to wait for.
			if want := int(gateWaitMax / time.Millisecond); req.WaitMS != want {
				t.Errorf("wait_ms = %d, want %d", req.WaitMS, want)
			}
			// principal is unused in v1 and must still be on the wire: retrofitting identity
			// into a shipped format is the expensive alternative (design.md section 7).
			if req.Principal.User == "" {
				t.Error("principal.user is empty; it is carried in v1 precisely so tier two need not retrofit it")
			}
			if req.Principal.Team != nil {
				t.Errorf("principal.team = %v, want null in v1", *req.Principal.Team)
			}
		})
	}
}

// TestGateTakesNoLeaseForOtherTools guards the case where the hook matcher is wider than it
// should be. A lease taken for a Bash call is a slot never returned.
func TestGateTakesNoLeaseForOtherTools(t *testing.T) {
	dialed := make(chan struct{}, 1)
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
		dialed <- struct{}{}
		c.Write([]byte(`{"v":1,"ok":true}` + "\n"))
	})
	payload := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash",` +
		`"tool_input":{"command":"wc -l README.md"},"tool_use_id":"toolu_bash"}`

	diag := runGateFor(t, endpoint, payload, transport.Dial{})
	if !strings.Contains(diag, "not a subagent spawn") {
		t.Errorf("stderr = %q, want it to say the payload was not a spawn", diag)
	}
	select {
	case <-dialed:
		t.Error("the gate contacted the daemon for a Bash call; only Agent and Task take a lease")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestGateGovernsBothToolSpellings: 2.1.263 sends tool_name "Agent", and the same version
// reports the same tool as "Task" elsewhere (docs/hook-surface.md section 1). Both must be
// governed, and so must a payload whose shape this binary does not recognise at all —
// guessing "not an agent" there would silently stop governing after a harness change.
func TestGateGovernsBothToolSpellings(t *testing.T) {
	for _, name := range []string{"Agent", "Task", ""} {
		t.Run("tool_name="+name, func(t *testing.T) {
			dialed := make(chan struct{}, 1)
			endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
				dialed <- struct{}{}
				c.Write([]byte(`{"v":1,"ok":true}` + "\n"))
			})
			payload := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"` + name + `","tool_use_id":"t"}`
			runGateFor(t, endpoint, payload, transport.Dial{})
			select {
			case <-dialed:
			case <-time.After(time.Second):
				t.Errorf("the gate did not contact the daemon for tool_name %q", name)
			}
		})
	}
}

// TestGateWritesNothingToStdoutViaTheCommandSurface goes through run() rather than gate(),
// because the contract that matters is the process's stdout, and a command table wired to
// the wrong function would not be caught by testing gate() directly.
func TestGateWritesNothingToStdoutViaTheCommandSurface(t *testing.T) {
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))

	// runGate reads os.Stdin, so give it one that is already at EOF.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved; r.Close() })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := run([]string{"gate"}, &stdout, &stderr)
	elapsed := time.Since(start)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if elapsed > failOpenCeiling {
		t.Errorf("took %v, want under %v", elapsed.Round(time.Millisecond), failOpenCeiling)
	}
}

func TestDefaultEndpoint(t *testing.T) {
	t.Run("explicit endpoint wins", func(t *testing.T) {
		t.Setenv(transport.EndpointEnv, "unix:///somewhere/else.sock")
		t.Setenv(transport.StateDirEnv, "/ignored")
		got, err := transport.DefaultEndpoint()
		if err != nil {
			t.Fatalf("DefaultEndpoint: %v", err)
		}
		if got != "unix:///somewhere/else.sock" {
			t.Errorf("endpoint = %q", got)
		}
	})

	t.Run("state dir places the socket", func(t *testing.T) {
		t.Setenv(transport.EndpointEnv, "")
		t.Setenv(transport.StateDirEnv, "/var/tmp/gov")
		got, err := transport.DefaultEndpoint()
		if err != nil {
			t.Fatalf("DefaultEndpoint: %v", err)
		}
		if got != "unix:///var/tmp/gov/sock" {
			t.Errorf("endpoint = %q", got)
		}
	})

	t.Run("default is under the home directory", func(t *testing.T) {
		t.Setenv(transport.EndpointEnv, "")
		t.Setenv(transport.StateDirEnv, "")
		got, err := transport.DefaultEndpoint()
		if err != nil {
			t.Fatalf("DefaultEndpoint: %v", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home directory: %v", err)
		}
		want := "unix://" + filepath.Join(home, ".claude", "governor", "sock")
		if got != want {
			t.Errorf("endpoint = %q, want %q", got, want)
		}
	})
}

func TestDialRejectsOtherSchemes(t *testing.T) {
	for _, endpoint := range []string{"tcp://host:1", "https://host/sock", "/plain/path"} {
		t.Run(endpoint, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, err := transport.Dial{}.Dial(ctx, endpoint)
			if err == nil {
				conn.Close()
				t.Fatalf("Dial(%q) succeeded; v1 speaks unix:// only", endpoint)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Dial(%q) waited instead of refusing: %v", endpoint, err)
			}
		})
	}
}

// TestGateTakesALeaseForASendMessageResume covers the second spawn path, measured in GOV-28
// and recorded in docs/hook-surface.md section 8.3.
//
// Resuming an agent through SendMessage puts a subagent back on the CPU with no
// PreToolUse(Agent) anywhere: the surface emits PreToolUse(SendMessage), then SubagentStart
// for the agent that was already running, then its work, then SubagentStop. Before this the
// gate saw none of it, so a resumed agent ran holding no lease at all.
//
// The confirm and release halves need no change — SubagentStart fires on a resume and
// SubagentStop still carries the agent_id — so this acquire is the whole of the fix.
func TestGateTakesALeaseForASendMessageResume(t *testing.T) {
	dialed := make(chan struct{}, 1)
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
		dialed <- struct{}{}
		c.Write([]byte(`{"v":1,"ok":true}` + "\n"))
	})
	// Verbatim shape from the GOV-28 probe C timeline.
	payload := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"SendMessage",` +
		`"tool_input":{"to":"ac05ddd6e5bdf6c51","message":"also report the first line"},` +
		`"tool_use_id":"toolu_resume"}`

	runGateFor(t, endpoint, payload, transport.Dial{})
	select {
	case <-dialed:
	case <-time.After(time.Second):
		t.Error("the gate took no lease for a SendMessage resume; a resumed agent runs ungoverned")
	}
}

// TestGateTakesNoLeaseForAnOrdinarySendMessage is the other half of the pair, and the reason
// the matcher could not simply be widened to SendMessage and left there.
//
// SendMessage addresses teammates, other local sessions and cloud sessions by name. None of
// those is a subagent and none produces a SubagentStop, so a lease taken for one would be a
// slot consumed forever — the exact failure TestGateTakesNoLeaseForOtherTools guards for
// Bash, arriving through a tool the matcher now deliberately admits.
func TestGateTakesNoLeaseForAnOrdinarySendMessage(t *testing.T) {
	for _, to := range []string{
		"claude-code-guide", // a teammate or agent addressed by name
		"",                  // no recipient at all
		"AC05DDD6E5BDF6C51", // right shape, wrong case: not an id this version emits
		"ac05ddd6e5bdf6c5",  // sixteen characters, one short
	} {
		t.Run("to="+to, func(t *testing.T) {
			dialed := make(chan struct{}, 1)
			endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
				dialed <- struct{}{}
				c.Write([]byte(`{"v":1,"ok":true}` + "\n"))
			})
			payload := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"SendMessage",` +
				`"tool_input":{"to":"` + to + `"},"tool_use_id":"toolu_msg"}`

			diag := runGateFor(t, endpoint, payload, transport.Dial{})
			if !strings.Contains(diag, "is not a subagent resume") {
				t.Errorf("stderr = %q, want it to say the message was not a resume", diag)
			}
			select {
			case <-dialed:
				t.Errorf("the gate took a lease for SendMessage to %q, which is not a subagent", to)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// TestGateTakesNoLeaseForASkillInvocation is a regression test for a belief, not for a line
// of code.
//
// GOV-27 recorded that the 63 subagent stops the gate could not see were "strongly
// correlated with Skill invocations", and GOV-28 was filed to widen the matcher to catch
// them. Measured directly, a Skill invocation emits PreToolUse(tool_name="Skill"), runs
// INLINE in the calling session, and spawns nothing: subagent_stats reported
// {"spawned": 0} and no SubagentStart or SubagentStop followed
// (docs/hook-surface.md section 8.3).
//
// Taking a lease for one would consume a slot that nothing ever returns, because no
// SubagentStop is coming. The test exists so that the next reader who finds that correlation
// in the log has to delete an explicit assertion, and read why, before acting on it.
func TestGateTakesNoLeaseForASkillInvocation(t *testing.T) {
	dialed := make(chan struct{}, 1)
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
		dialed <- struct{}{}
		c.Write([]byte(`{"v":1,"ok":true}` + "\n"))
	})
	payload := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Skill",` +
		`"tool_input":{"skill":"countlines"},"tool_use_id":"toolu_skill"}`

	diag := runGateFor(t, endpoint, payload, transport.Dial{})
	if !strings.Contains(diag, "not a subagent spawn") {
		t.Errorf("stderr = %q, want it to say the payload was not a spawn", diag)
	}
	select {
	case <-dialed:
		t.Error("the gate took a lease for a Skill invocation, which spawns no subagent")
	case <-time.After(50 * time.Millisecond):
	}
}
