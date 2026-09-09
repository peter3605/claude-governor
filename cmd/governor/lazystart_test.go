package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// Lazy daemon start (GOV-11).
//
// The thing under test is a decision, not a fork: when a gate finds nobody on the socket,
// does it start a daemon, and — much more importantly — when does it decline to? The fork
// itself is four lines of os/exec and is covered once, end to end, through the real binary
// at the bottom of this file.
//
// Every test here that is not the end-to-end one replaces execDaemon, because the default
// would refuse anyway: under `go test` os.Executable() is the test binary and forkDaemon
// declines to run it. That refusal is itself tested, and it is the guard that makes the
// substitution a convenience rather than a load-bearing safety measure.

// recordStarts swaps execDaemon for a recorder and returns the slice it appends to.
func recordStarts(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := execDaemon
	execDaemon = func(state string) error {
		got = append(got, state)
		return nil
	}
	t.Cleanup(func() { execDaemon = prev })
	return &got
}

// TestLazyStartForksADaemonWhenTheSocketIsDead is the ordinary case: the governor is
// installed, its state directory is there, and nothing is listening.
func TestLazyStartForksADaemonWhenTheSocketIsDead(t *testing.T) {
	state := sockDir(t)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(state, "sock"))
	starts := recordStarts(t)

	var diag bytes.Buffer
	lazilyStartDaemon(&diag)

	if len(*starts) != 1 {
		t.Fatalf("forked %d daemons, want 1 (diag: %s)", len(*starts), diag.String())
	}
	if (*starts)[0] != state {
		t.Errorf("forked with state %q, want %q", (*starts)[0], state)
	}
	if !strings.Contains(diag.String(), "proceeds ungoverned") {
		t.Errorf("diag = %q, want it to say this spawn is still ungoverned", diag.String())
	}
}

// TestLazyStartRateLimitsRepeatedAttempts is the fan-out case and the crash-loop case at
// once. Ten gates finding a dead socket in quick succession must not fork ten daemons.
func TestLazyStartRateLimitsRepeatedAttempts(t *testing.T) {
	state := sockDir(t)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(state, "sock"))
	starts := recordStarts(t)

	var diag bytes.Buffer
	for i := 0; i < 10; i++ {
		lazilyStartDaemon(&diag)
	}

	if len(*starts) != 1 {
		t.Fatalf("forked %d daemons across 10 gates, want 1 (diag: %s)", len(*starts), diag.String())
	}
	if !strings.Contains(diag.String(), "attempted within the last") {
		t.Errorf("diag = %q, want it to name the rate limit", diag.String())
	}
}

// TestLazyStartTriesAgainOnceTheCooldownHasPassed is the other half of the rate limit, and
// it is the half that matters for a machine whose daemon has genuinely died: the limit must
// expire, or the first failed start would leave the governor off forever.
func TestLazyStartTriesAgainOnceTheCooldownHasPassed(t *testing.T) {
	state := sockDir(t)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(state, "sock"))
	starts := recordStarts(t)

	var diag bytes.Buffer
	lazilyStartDaemon(&diag)

	// Age the marker rather than sleeping for the cooldown: the property is "older than
	// lazyStartCooldown", and sleeping five seconds to observe it would make the shipped
	// constant untestable the moment anyone lengthened it.
	marker := filepath.Join(state, daemonStartFile)
	old := time.Now().Add(-lazyStartCooldown - time.Second)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatalf("age the marker: %v", err)
	}

	lazilyStartDaemon(&diag)
	if len(*starts) != 2 {
		t.Fatalf("forked %d daemons, want 2: the cooldown must expire (diag: %s)", len(*starts), diag.String())
	}
}

// TestLazyStartRecordsTheAttemptBeforeForking pins the ordering that makes the rate limit
// work at all.
//
// A marker written only after a successful fork would not bound the case it exists for: a
// daemon that cannot start fails the fork every time, so every spawn on the machine would
// fork again. The attempt is what is recorded, not the success.
func TestLazyStartRecordsTheAttemptBeforeForking(t *testing.T) {
	state := sockDir(t)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(state, "sock"))

	var attempts int
	prev := execDaemon
	execDaemon = func(string) error {
		attempts++
		return fmt.Errorf("this daemon cannot start")
	}
	t.Cleanup(func() { execDaemon = prev })

	var diag bytes.Buffer
	for i := 0; i < 5; i++ {
		lazilyStartDaemon(&diag)
	}

	if attempts != 1 {
		t.Errorf("attempted %d forks after a failing start, want 1", attempts)
	}
	if !strings.Contains(diag.String(), "this daemon cannot start") {
		t.Errorf("diag = %q, want it to carry the start failure", diag.String())
	}
}

// TestLazyStartDoesNothingWithoutAStateDirectory is the uninstall case, and it is the one
// that would be a real bug rather than an inefficiency.
//
// A session that was live when `governor uninstall` ran still holds the hooks it registered,
// so its next spawn reaches this code on a machine that has deliberately been cleaned. Both
// halves matter: no daemon is started, and — the reason the check is a stat rather than a
// MkdirAll — the directory uninstall just removed is not put back.
func TestLazyStartDoesNothingWithoutAStateDirectory(t *testing.T) {
	gone := filepath.Join(sockDir(t), "removed")
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(gone, "sock"))
	starts := recordStarts(t)

	var diag bytes.Buffer
	lazilyStartDaemon(&diag)

	if len(*starts) != 0 {
		t.Errorf("forked %d daemons for an uninstalled governor, want 0", len(*starts))
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("state directory %s exists: a lazy start must not recreate what uninstall removed", gone)
	}
	if !strings.Contains(diag.String(), "no state directory") {
		t.Errorf("diag = %q, want it to say why no daemon was started", diag.String())
	}
}

// TestLazyStartRefusesAnEndpointItCannotParse covers the tier-two seam: an endpoint in a
// scheme v1 does not speak has no socket directory, so there is nowhere to start a daemon
// and nothing sensible to guess.
func TestLazyStartRefusesAnEndpointItCannotParse(t *testing.T) {
	t.Setenv(transport.EndpointEnv, "tcp://broker.example:9000")
	starts := recordStarts(t)

	var diag bytes.Buffer
	lazilyStartDaemon(&diag)

	if len(*starts) != 0 {
		t.Errorf("forked %d daemons for a tcp:// endpoint, want 0", len(*starts))
	}
	if !strings.Contains(diag.String(), "not starting a daemon") {
		t.Errorf("diag = %q, want it to decline explicitly", diag.String())
	}
}

// TestForkDaemonRefusesToRunABinaryThatIsNotTheGovernor is the guard, tested against the
// real forkDaemon rather than a stub.
//
// The gate spawns ITSELF under another subcommand, so os.Executable() had better be the
// governor. Under `go test` it is the test binary, and running that with "daemon" runs the
// whole suite again with an argument it does not understand — the shape GOV-21 hit when
// doctor ran `<hooked binary> version`. This test is why every other test in this file can
// safely be wrong about stubbing execDaemon.
func TestForkDaemonRefusesToRunABinaryThatIsNotTheGovernor(t *testing.T) {
	err := forkDaemon(sockDir(t))
	if err == nil {
		t.Fatal("forkDaemon accepted the test binary; it must refuse anything not named governor")
	}
	if !strings.Contains(err.Error(), "refusing to run it as a daemon") {
		t.Errorf("error = %v, want it to name the refusal", err)
	}
}

// TestGateStartsADaemonOnlyWhenNothingIsListening is the narrowing that makes this safe to
// have on the hot path at all.
//
// A gate fails open for several reasons and only one of them means "there is no daemon". A
// daemon that ACCEPTED the connection and then went wrong — answered late, answered rubbish,
// answered in a protocol this binary does not speak — is a daemon, and forking a second one
// at it would be wrong twice over: the new one loses the bind race and exits, and it does so
// on every spawn for as long as the first stays wedged. errNoDaemon is what draws that line,
// and this is the test that holds it there.
func TestGateStartsADaemonOnlyWhenNothingIsListening(t *testing.T) {
	wedged := listenerFunc(t, func(c net.Conn, _ []byte) {
		// Accept and never answer. The gate fails open on its own read deadline.
		select {}
	})

	for _, tt := range []struct {
		name     string
		endpoint func(t *testing.T) string
		want     int
	}{
		{
			name:     "nothing listening",
			endpoint: func(t *testing.T) string { return "unix://" + filepath.Join(sockDir(t), "sock") },
			want:     1,
		},
		{
			name:     "a daemon that accepts and never answers",
			endpoint: func(t *testing.T) string { return wedged },
			want:     0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			starts := recordStarts(t)
			var stdout, stderr bytes.Buffer
			code := gate(strings.NewReader(samplePayload), &stdout, &stderr,
				transport.Dial{}, gateBudget)

			if code != 0 || stdout.Len() != 0 {
				t.Fatalf("code %d stdout %q, want 0 and empty", code, stdout.String())
			}
			if len(*starts) != tt.want {
				t.Errorf("started %d daemons, want %d (stderr: %s)", len(*starts), tt.want, stderr.String())
			}
		})
	}
}

// TestGateStartsADaemonWhenItFindsNoneEndToEnd is the acceptance test for this story.
//
// It runs the REAL binary's gate against a state directory with no daemon in it, exactly as
// a PreToolUse hook does, and then asserts that a daemon is answering on that socket. In
// process none of this is provable: the fork is the part that only exists in a real one, and
// forkDaemon's own name check means it cannot be reached any other way.
func TestGateStartsADaemonWhenItFindsNoneEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and forks a real binary")
	}
	state := sockDir(t)
	endpoint := "unix://" + filepath.Join(state, "sock")
	t.Cleanup(func() { stopLazilyStartedDaemon(t, state) })

	stdout, stderr := runRealGate(t, state, endpoint)
	if stdout != "" {
		t.Fatalf("gate stdout = %q, want empty: a gate that found no daemon must fail open silently", stdout)
	}
	if !strings.Contains(stderr, "started a daemon") {
		t.Fatalf("gate stderr = %q, want it to report starting a daemon", stderr)
	}

	// The daemon binds asynchronously and this spawn deliberately did not wait for it, so
	// the assertion is that it arrives — not that it had already arrived when the gate exited.
	deadline := time.Now().Add(5 * time.Second)
	for !daemonAnswers(endpoint) {
		if time.Now().After(deadline) {
			t.Fatalf("no daemon answered on %s within 5s; daemon log:\n%s", endpoint, readDaemonLog(state))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And the second gate is governed: it reaches the daemon rather than starting another.
	_, stderr2 := runRealGate(t, state, endpoint)
	if strings.Contains(stderr2, "started a daemon") {
		t.Errorf("the second gate started another daemon; stderr = %q", stderr2)
	}
	if !strings.Contains(stderr2, "granted lease") {
		t.Errorf("second gate stderr = %q, want it to report a granted lease", stderr2)
	}
}

// runRealGate runs `governor gate` as a real subprocess with one PreToolUse payload on
// stdin, and asserts the one contract every gate invocation has: exit 0.
func runRealGate(t *testing.T, state, endpoint string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(realGovernor(t), "gate")
	cmd.Env = append(os.Environ(),
		transport.EndpointEnv+"="+endpoint,
		transport.StateDirEnv+"="+state,
	)
	cmd.Stdin = strings.NewReader(preToolUsePayload("lazystart", "toolu_lazy0001"))

	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("gate exited %v, want 0 (stderr: %s)", err, errb.String())
	}
	return out.String(), errb.String()
}

// stopLazilyStartedDaemon kills whatever the test started.
//
// A leaked daemon outlives the test's temp directory — it holds a socket that has been
// deleted underneath it and stays resident until the machine reboots — so this is cleanup
// that has to happen rather than tidiness.
func stopLazilyStartedDaemon(t *testing.T, state string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, daemonPIDFile))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func readDaemonLog(state string) string {
	raw, _ := os.ReadFile(filepath.Join(state, daemonLogFile))
	return string(raw)
}
