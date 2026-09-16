package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/trace"
	"github.com/peter3605/claude-governor/internal/transport"
)

// buildPayloadFor is a PreToolUse payload for one Bash command, shaped exactly as the
// harness sends it (docs/hook-surface.md section 2, which is where `cwd` on a Bash payload
// is measured rather than assumed).
func buildPayloadFor(cmd string) string {
	p := map[string]any{
		"session_id":      "2ed4bf1c-470b-4e08-9b48-4f310663cb6e",
		"cwd":             "/tmp/work",
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": cmd},
		"tool_use_id":     "toolu_013XAeD4MymdfZHskv22UtK3",
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// buildRecords reads back every build record the gate wrote during this test.
//
// It reads the file the gate actually wrote rather than intercepting the call, because the
// thing under test is a line on disk that somebody will group and count weeks later. A test
// that asserted on a struct handed to a fake writer would pass while the JSON tags were
// wrong, and the JSON tags are the whole artefact.
func buildRecords(t *testing.T) []trace.Build {
	t.Helper()
	path, err := trace.LogPath()
	if err != nil {
		t.Fatalf("resolve the trace log: %v", err)
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the trace log: %v", err)
	}
	var out []trace.Build
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var r trace.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("the trace log carries a line nothing can parse: %q: %v", line, err)
		}
		if r.Kind == trace.KindBuild {
			if r.Build == nil {
				t.Fatalf("a record of kind %q carries no build object: %q", r.Kind, line)
			}
			out = append(out, *r.Build)
		}
	}
	return out
}

// isolateTrace points the trace log at a directory of this test's own and returns it.
//
// TestMain already isolates the package from the developer's real ~/.claude/governor, so
// this is not about safety — it is about each case seeing only its OWN records. Without it
// every test in the package appends to one file and "the gate wrote exactly one record" is
// a statement about the whole package's history.
func isolateTrace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(trace.StateDirEnv, dir)
	return dir
}

// oneBuildRecord runs the gate against an endpoint and returns the single record it wrote.
func oneBuildRecord(t *testing.T, endpoint, cmd string) trace.Build {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	if code := gate(strings.NewReader(buildPayloadFor(cmd)), &stdout, &stderr, transport.Dial{},
		gateBudget, protocolVersion); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	recs := buildRecords(t)
	if len(recs) != 1 {
		t.Fatalf("the gate wrote %d build records, want exactly 1 (stderr: %s)", len(recs), stderr.String())
	}
	return recs[0]
}

// TestEveryGatedBuildIsRecordedWithItsOutcome is GOV-17's instrument, one case per outcome.
//
// The four outcomes are not decoration and the split between them is what the story is scored
// on: granted, parked and failed-open are builds that RAN, and denied is one that did not.
// Clause 3 asks whether sessions stopped running their tests, and that question is only
// answerable if a build which ran ungoverned is distinguishable from one that was refused.
func TestEveryGatedBuildIsRecordedWithItsOutcome(t *testing.T) {
	grant := `{"v":1,"ok":true,"lease":"L-0d41","budget":6,"in_use":4,"share":2,"source":"local"}`
	refuse := `{"v":1,"ok":false,"reason":"capacity","budget":6,"in_use":6,"slots":6,` +
		`"cpu":0.94,"thermal":"fair","source":"local"}`

	cases := []struct {
		name     string
		endpoint func(t *testing.T) string
		want     string
	}{
		{
			name: "granted immediately",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(grant + "\n")) })
			},
			want: outcomeGranted,
		},
		{
			name: "parked, then granted",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":1,"ok":false,"reason":"waiting","wait_ms":100}` + "\n"))
					c.Write([]byte(grant + "\n"))
				})
			},
			want: outcomeParked,
		},
		{
			name: "denied",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(refuse + "\n")) })
			},
			want: outcomeDenied,
		},
		{
			name: "no daemon at all",
			endpoint: func(t *testing.T) string {
				return "unix://" + filepath.Join(sockDir(t), "absent")
			},
			want: outcomeFailedOpen,
		},
		{
			name: "a daemon of another protocol version",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":99,"ok":true,"lease":"L-ff"}` + "\n"))
				})
			},
			want: outcomeFailedOpen,
		},
		{
			name: "a refusal this binary does not understand",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) {
					c.Write([]byte(`{"v":1,"ok":false,"reason":"quota_exhausted_tier_two"}` + "\n"))
				})
			},
			want: outcomeFailedOpen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateTrace(t)
			got := oneBuildRecord(t, tc.endpoint(t), "go test ./...")

			if got.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q (why: %q)", got.Outcome, tc.want, got.Why)
			}
			if got.Class != "medium" {
				t.Errorf("class = %q, want medium", got.Class)
			}
			if got.SessionID == "" {
				t.Error("the record names no session, so it cannot be attributed")
			}
			// Every outcome that is not a clean grant has to say WHY, or a window that went
			// wrong is unreadable afterwards.
			if tc.want != outcomeGranted && tc.want != outcomeParked && got.Why == "" {
				t.Errorf("outcome %q carries no reason", got.Outcome)
			}
		})
	}
}

// TestRecordingABuildNeverSuppressesADeny is the fail-open ordering, and it is the test this
// file exists for.
//
// The instrument writes to a file, and opening a file can fail or block. If that write ran
// before the refusal reached stdout, an unwritable disk would silently convert every deny
// into a grant: the governor would stop governing, the session would notice nothing, and
// `doctor` would report a healthy machine. Deferring the write puts the instrument strictly
// downstream of enforcement, and this pins it there.
//
// The state directory is pointed at a path that cannot be created — a directory whose parent
// is a regular file — so trace.Append fails on MkdirAll for every record.
func TestRecordingABuildNeverSuppressesADeny(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(trace.StateDirEnv, filepath.Join(blocker, "state"))

	refuse := `{"v":1,"ok":false,"reason":"capacity","budget":6,"in_use":6,"slots":6,` +
		`"cpu":0.94,"thermal":"fair","source":"local"}`
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(refuse + "\n")) })
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	code := gate(strings.NewReader(buildPayloadFor("go test ./...")), &stdout, &stderr,
		transport.Dial{}, gateBudget, protocolVersion)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	var out denyOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout = %q, which is not a decision: %v\nAn instrument that cannot write "+
			"must not be able to swallow a refusal.", stdout.String(), err)
	}
	if got := out.HookSpecificOutput.PermissionDecision; got != "deny" {
		t.Errorf("permissionDecision = %q, want deny even though the trace log is unwritable", got)
	}
	if !strings.Contains(stderr.String(), "record build") {
		t.Errorf("stderr = %q, want the failed write reported as a diagnostic", stderr.String())
	}
}

// TestTheBuildRecordCarriesTheShapeAndNeverTheCommand is the privacy boundary at the point
// where it actually reaches disk.
//
// internal/classify tests that Result.Shapes drops arguments. This tests the thing that
// matters operationally: that nothing ELSE on the record smuggles the command text back in.
// The gate reads `tool_input.command` and it would be entirely natural for someone to add it
// to the record later "for debugging" — at which point every API key passed as an environment
// assignment on a build line is on disk in plain text.
func TestTheBuildRecordCarriesTheShapeAndNeverTheCommand(t *testing.T) {
	isolateTrace(t)
	grant := `{"v":1,"ok":true,"lease":"L-0d41","budget":6,"in_use":1,"source":"local"}`
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(grant + "\n")) })

	const secret = "SUPER_SECRET_TOKEN_9f3a"
	cmd := "DEPLOY_KEY=" + secret + " make deploy-to-acme-prod"

	rec := oneBuildRecord(t, endpoint, cmd)

	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, "deploy-to-acme-prod", "DEPLOY_KEY"} {
		if bytes.Contains(blob, []byte(leak)) {
			t.Errorf("the build record carries %q onto disk:\n%s", leak, blob)
		}
	}
	if len(rec.Shapes) == 0 {
		t.Fatal("the record names no shape, so a test run cannot be told from a build")
	}
	if rec.Shapes[0] != "make" {
		t.Errorf("shapes = %v, want [make]", rec.Shapes)
	}
}

// TestNothingButAGatedBuildIsRecorded keeps the instrument off the cheap path.
//
// 81.6% of Bash calls are not build-shaped and must cost nothing — no socket, and now no file
// either. An agent spawn is excluded for a different reason: it is already fully described by
// daemon.log, and recording it here would double-count the window.
func TestNothingButAGatedBuildIsRecorded(t *testing.T) {
	grant := `{"v":1,"ok":true,"lease":"L-0d41","budget":6,"in_use":1,"source":"local"}`

	t.Run("a cheap command", func(t *testing.T) {
		isolateTrace(t)
		endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(grant + "\n")) })
		t.Setenv(transport.EndpointEnv, endpoint)

		var stdout, stderr bytes.Buffer
		gate(strings.NewReader(buildPayloadFor("git status")), &stdout, &stderr,
			transport.Dial{}, gateBudget, protocolVersion)

		if recs := buildRecords(t); len(recs) != 0 {
			t.Errorf("a cheap command wrote %d build records, want none: %+v", len(recs), recs)
		}
	})

	t.Run("an agent spawn", func(t *testing.T) {
		isolateTrace(t)
		endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte(grant + "\n")) })
		t.Setenv(transport.EndpointEnv, endpoint)

		var stdout, stderr bytes.Buffer
		gate(strings.NewReader(samplePayload), &stdout, &stderr,
			transport.Dial{}, gateBudget, protocolVersion)

		if recs := buildRecords(t); len(recs) != 0 {
			t.Errorf("an agent spawn wrote %d build records, want none: %+v", len(recs), recs)
		}
	})
}

// TestAParkedBuildRecordsHowLongItWaited. A park is the cost the budget imposes on work that
// still happened, and clause 2 is scored partly on it. "Parked" without a duration cannot
// distinguish a 30 ms hiccup from a 20 s stall, which are opposite findings.
func TestAParkedBuildRecordsHowLongItWaited(t *testing.T) {
	isolateTrace(t)
	grant := `{"v":1,"ok":true,"lease":"L-0d41","budget":6,"in_use":6,"source":"local"}`
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
		c.Write([]byte(`{"v":1,"ok":false,"reason":"waiting","wait_ms":100}` + "\n"))
		c.Write([]byte(grant + "\n"))
	})

	rec := oneBuildRecord(t, endpoint, "go test ./...")

	if rec.Outcome != outcomeParked {
		t.Fatalf("outcome = %q, want %q", rec.Outcome, outcomeParked)
	}
	if rec.WaitedMS <= 0 {
		t.Errorf("waited_ms = %v, want a positive duration", rec.WaitedMS)
	}
}
