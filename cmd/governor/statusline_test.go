package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The statusline suite (GOV-10).
//
// Its subject is a surface a person reads on every assistant message, so the properties
// being held are different from the gate's. The gate must produce nothing when it fails;
// this must produce a true sentence when it fails. Both must be quick, and both must exit 0
// — for the gate because a non-zero hook is a failed spawn, and for this because Claude
// Code discards a statusline command's output entirely unless it exited 0
// (docs/hook-surface.md section 14).

// statuslineCeiling is the latency this command is held to.
//
// The Done-when sets the budget at 50 ms and this is that plus the slack a process is
// allowed getting to and from the work, on the same reasoning as the gate suite's
// failOpenCeiling. It is asserted over the failure modes and not only the happy path,
// because a status line that is fast against a live daemon and slow against a dead one is
// slow exactly when a person is most likely to be looking at it.
//
// It is asserted through timed, which discounts what a loaded runner cost rather than what
// this command did. latency_test.go says why, and why the 70 is not the thing to raise.
const statuslineCeiling = 70 * time.Millisecond

// sampleStatusLinePayload is a real statusLine payload, from docs/hook-surface.md section
// 14 with the fields this command does not read trimmed to keep the test legible.
const sampleStatusLinePayload = `{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e",` +
	`"transcript_path":"/Users/you/.claude/projects/x/2ed4bf1c.jsonl","cwd":"/tmp/work",` +
	`"permission_mode":"default","agent_type":"main","model":{"id":"claude-opus-5",` +
	`"display_name":"Opus 5"},"workspace":{"current_dir":"/tmp/work","project_dir":"/tmp/work",` +
	`"added_dirs":[]},"version":"2.1.265","output_style":{"name":"default"},` +
	`"exceeds_200k_tokens":false}`

const sampleSessionID = "2ed4bf1c-470b-4e08-9b48-4f310663cb6e"

// isolateState points the cache at a temp directory.
//
// Every test in this file calls it, and that is not tidiness: without it the cache is
// written to the developer's own ~/.claude/governor, where it would both pollute a real
// install and make the suite's results depend on whether a daemon happened to be running.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)
	return dir
}

// statusDaemon serves canned status replies and counts the requests it was asked.
//
// It is a scripted listener rather than a real daemon for the rendering tests, because the
// facts being rendered — a CPU reading, an elevated thermal state — are ones no daemon can
// currently produce: cpu is null until GOV-12 and thermal comes from the host this suite
// runs on. A test that could only assert what today's daemon happens to send would not test
// the renderer at all. The wire-level tests below use the real daemon.
//
// The recorded request is behind a mutex and the reply line is built once, up front. Both
// were races the first time this was written, and `go test -race` caught them where the
// suite did not: two connections are served concurrently, and `append(raw, '\n')` on a
// shared slice with spare capacity writes into one backing array from both of them.
func statusDaemon(t *testing.T, reply any) (string, *atomic.Int64, func() []byte) {
	t.Helper()
	var calls atomic.Int64
	var mu sync.Mutex
	var last []byte
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	line := append(raw, '\n')
	endpoint := listenerFunc(t, func(c net.Conn, request []byte) {
		calls.Add(1)
		mu.Lock()
		last = append([]byte(nil), request...)
		mu.Unlock()
		c.Write(line)
	})
	return endpoint, &calls, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// runStatusline drives the command end to end and holds it to the contract every case
// shares: a non-empty single line, inside the ceiling.
//
// It drives statuslineCommand rather than statusline because since GOV-33 the cache is
// written by the command and not by the renderer. Calling the inner function here would
// leave every caching test asserting against a cache nothing ever writes — passing, and
// measuring nothing. The ceiling still covers the write, which is where it was before.
func runStatuslineFor(t *testing.T, endpoint, payload string, width int) string {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	took := timed(func() {
		statuslineCommand(strings.NewReader(payload), &stdout, &stderr, transport.Dial{}, statuslineBudget, width)
	})
	line := strings.TrimSuffix(stdout.String(), "\n")

	if took.exceeds(statuslineCeiling) {
		t.Errorf("took %v, want under %v (stderr: %s)", took, statuslineCeiling, stderr.String())
	}
	if line == "" {
		t.Errorf("rendered an empty line; every failure must still say something")
	}
	if strings.Contains(line, "\n") {
		t.Errorf("rendered %q, want a single line", line)
	}
	return line
}

func f64(v float64) *float64 { return &v }

// TestStatuslineShape holds the line the story specifies, verbatim.
//
// The example in the acceptance criterion and in design.md section 9 is the whole assertion
// here: a status line whose shape drifts is one a person has to re-read rather than glance
// at, and there is no other place that shape is written down as a test.
func TestStatuslineShape(t *testing.T) {
	isolateState(t)
	endpoint, _, _ := statusDaemon(t, statusReply{
		V: protocolVersion, Budget: 5, InUse: 3, Slots: 3, CPU: f64(0.61), Thermal: "nominal",
	})

	got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
	if want := "3/5 slots - cpu 61% - thermal ok"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

// TestStatuslineRendersWhatTheDaemonSent covers the clauses that are conditional, in both
// directions. Each of these omissions is a fact the daemon did not send, and a status line
// that filled one in would be stating a number nobody measured.
func TestStatuslineRendersWhatTheDaemonSent(t *testing.T) {
	tests := []struct {
		name  string
		reply statusReply
		want  string
	}{
		{
			// The shipped state until GOV-12: the daemon has no sampler and sends null.
			// "cpu 0%" here would tell a reader the machine is idle, which is a claim.
			name:  "null cpu produces no cpu clause",
			reply: statusReply{V: protocolVersion, Budget: 4, InUse: 2, Slots: 2, CPU: nil, Thermal: "nominal"},
			want:  "2/4 slots - thermal ok",
		},
		{
			// Every Linux host, forever. It is a statement about the measurement, not
			// about the machine, so it gets no width on the line.
			name:  "unknown thermal produces no thermal clause",
			reply: statusReply{V: protocolVersion, Budget: 4, InUse: 0, Slots: 0, CPU: f64(0.1), Thermal: "unknown"},
			want:  "0/4 slots - cpu 10%",
		},
		{
			name:  "neither signal leaves the slot count alone",
			reply: statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "unknown"},
			want:  "1/4 slots",
		},
		{
			// Not softened to "ok" or anything else. This is the state the control loop
			// cuts the budget on and it appears in the daemon's own word.
			name:  "an elevated thermal state is reported verbatim",
			reply: statusReply{V: protocolVersion, Budget: 4, InUse: 4, Slots: 4, CPU: f64(0.97), Thermal: "serious"},
			want:  "4/4 slots - cpu 97% - thermal serious",
		},
		{
			// A full budget and a full budget with a queue behind it read identically
			// without this, and they are not the same situation.
			name:  "a wait queue is named when it is not empty",
			reply: statusReply{V: protocolVersion, Budget: 4, InUse: 4, Slots: 4, Waiting: 2, Thermal: "nominal"},
			want:  "4/4 slots - 2 waiting - thermal ok",
		},
		{
			name:  "an empty wait queue is not mentioned",
			reply: statusReply{V: protocolVersion, Budget: 4, InUse: 4, Slots: 4, Waiting: 0, Thermal: "nominal"},
			want:  "4/4 slots - thermal ok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			endpoint, _, _ := statusDaemon(t, tc.reply)
			if got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0); got != tc.want {
				t.Errorf("line = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStatuslineReadsTheSessionFromStdin is the "reads the statusLine JSON on stdin" clause.
//
// It is asserted by consequence rather than by inspection: the session's own slot count can
// only appear if session_id was read off stdin and matched against the daemon's sessions
// array. A command that ignored stdin would still render a line, and would still render the
// right machine-wide numbers — which is exactly why this needs its own test.
func TestStatuslineReadsTheSessionFromStdin(t *testing.T) {
	reply := statusReply{
		V: protocolVersion, Budget: 5, InUse: 3, Slots: 3, Thermal: "nominal",
		Sessions: []sessionStat{
			{Session: "someone-else", Confirmed: 1},
			{Session: sampleSessionID, Tentative: 1, Confirmed: 1},
		},
	}

	t.Run("the session's own slots are named", func(t *testing.T) {
		isolateState(t)
		endpoint, _, _ := statusDaemon(t, reply)
		// Tentative and confirmed together: a tentative lease is a spawn in flight, and it
		// is a slot this session is consuming right now.
		if got, want := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0), "3/5 slots (2 yours) - thermal ok"; got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
	})

	t.Run("a session holding nothing is not told so", func(t *testing.T) {
		isolateState(t)
		endpoint, _, _ := statusDaemon(t, reply)
		payload := `{"session_id":"a-third-session","cwd":"/tmp"}`
		if got, want := runStatuslineFor(t, endpoint, payload, 0), "3/5 slots - thermal ok"; got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
	})

	t.Run("an unreadable payload still renders the machine-wide line", func(t *testing.T) {
		isolateState(t)
		endpoint, _, _ := statusDaemon(t, reply)
		// A harness upgrade that changes the payload must cost the session-local clause,
		// never the line.
		if got, want := runStatuslineFor(t, endpoint, "not json at all", 0), "3/5 slots - thermal ok"; got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
	})
}

// TestStatuslineCacheIsNotSharedBetweenSessions is the reason the cache holds the reply
// rather than the finished line.
//
// The cache file is shared by every session on the machine. If it held rendered text, the
// second session here would be shown the first session's slot count as its own for up to a
// second — a wrong number, on the surface whose whole purpose is to be trusted at a glance.
func TestStatuslineCacheIsNotSharedBetweenSessions(t *testing.T) {
	isolateState(t)
	endpoint, calls, _ := statusDaemon(t, statusReply{
		V: protocolVersion, Budget: 5, InUse: 3, Slots: 3, Thermal: "nominal",
		Sessions: []sessionStat{
			{Session: sampleSessionID, Confirmed: 2},
			{Session: "second-session", Confirmed: 1},
		},
	})

	first := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
	second := runStatuslineFor(t, endpoint, `{"session_id":"second-session"}`, 0)

	if want := "3/5 slots (2 yours) - thermal ok"; first != want {
		t.Errorf("first session line = %q, want %q", first, want)
	}
	if want := "3/5 slots (1 yours) - thermal ok"; second != want {
		t.Errorf("second session line = %q, want %q", second, want)
	}
	// The point of the test is that the second render was served from the cache and was
	// still session-correct. If it went to the daemon instead, the assertion above passed
	// for the wrong reason.
	if got := calls.Load(); got != 1 {
		t.Errorf("daemon was asked %d times, want 1: the second render must come from the cache", got)
	}
}

// TestStatuslineCaches is the trap in the story: this command runs on every assistant
// message in every session, and a daemon round trip per render makes it the busiest thing
// the governor does.
func TestStatuslineCaches(t *testing.T) {
	t.Run("a second render inside the TTL does not ask the daemon", func(t *testing.T) {
		isolateState(t)
		endpoint, calls, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})

		for i := 0; i < 5; i++ {
			runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("daemon was asked %d times over 5 renders, want 1", got)
		}
	})

	t.Run("a stale cache is not used", func(t *testing.T) {
		dir := isolateState(t)
		endpoint, calls, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})

		runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		// Age the entry rather than sleeping out the TTL: the freshness rule is what is
		// being tested, and a suite nobody wants to run is a suite that stops being run.
		ageCache(t, dir, -2*statuslineCacheTTL)
		runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)

		if got := calls.Load(); got != 2 {
			t.Errorf("daemon was asked %d times, want 2: an expired cache must be refetched", got)
		}
	})

	t.Run("a cache stamped in the future is not used", func(t *testing.T) {
		dir := isolateState(t)
		endpoint, calls, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})

		runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		// A clock that jumped backwards, or a cache copied from another machine. Treating
		// it as fresh would pin the line at a stale reading until the clock caught up,
		// which for a large jump is indistinguishable from the governor having frozen.
		ageCache(t, dir, time.Hour)
		runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)

		if got := calls.Load(); got != 2 {
			t.Errorf("daemon was asked %d times, want 2: a future-stamped cache must be refetched", got)
		}
	})

	t.Run("a corrupt cache is a miss, not a failure", func(t *testing.T) {
		dir := isolateState(t)
		endpoint, calls, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})

		// Half a write, which is what a reader would see if the write were not atomic.
		if err := os.WriteFile(filepath.Join(dir, statuslineCacheFile), []byte(`{"at":"2026-`), 0o644); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		if got, want := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0), "1/4 slots - thermal ok"; got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("daemon was asked %d times, want 1", got)
		}
	})
}

// strays returns the statusline temp files left in dir.
func strays(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, statuslineCacheFile+".*"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	return found
}

// TestStatuslineRendersWithoutTouchingTheDisk is GOV-33's root invariant, and it is stated
// about the renderer rather than about the temp file on purpose.
//
// The leak was never a missing cleanup. writeCache's `defer os.Remove(tmp.Name())` was
// correct and present the whole time. It was that the work ran in a goroutine the budget is
// enforced by ABANDONING — statusline returns on its timer, the command prints, the process
// exits, and a deferred function in an abandoned goroutine does not run at exit. Anything
// holding an unrenamed temp file at that moment leaks it, and over six days on one machine
// that came to 133 files: 9 of them zero bytes, caught between os.CreateTemp and the first
// write, the rest caught between the write and the rename.
//
// So the durable guard is not "no temp file is left behind" — that is the symptom, and a
// test for it passes for any number of fragile implementations. It is that THE ABANDONABLE
// FUNCTION TOUCHES NO FILE AT ALL. Hold that, and there is nothing for an abandoned
// goroutine to be holding, whichever line it is abandoned on. The statusline is allowed to
// run to completion here precisely so the assertion is about what it writes rather than
// about how far it got.
func TestStatuslineRendersWithoutTouchingTheDisk(t *testing.T) {
	dir := isolateState(t)
	endpoint, _, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})
	t.Setenv(transport.EndpointEnv, endpoint)

	var stderr bytes.Buffer
	line, pending := statusline(strings.NewReader(sampleStatusLinePayload), &stderr, transport.Dial{}, statuslineBudget, 0)

	if line == "" {
		t.Fatal("rendered nothing; the case is only meaningful if the render succeeded")
	}
	// The reply is carried out to be cached, not cached here. Without this the test would
	// also pass for a statusline that had simply stopped working.
	if pending == nil {
		t.Fatal("statusline returned nothing to cache after a live daemon answered it")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("statusline created %s; the renderer must touch no file, or an abandoned "+
			"run can leave one behind", e.Name())
	}
}

// TestStatuslineLeavesNoTempFileBehind is the symptom, checked on every path that reaches
// the disk.
//
// IT DOES NOT REPRODUCE THE ORIGINAL LEAK AND CANNOT, which is worth writing down here so
// that nobody later reads a green run as proof the bug is caught. This test was run against
// the pre-GOV-33 code and PASSED. The orphan needed the PROCESS to exit while the worker
// goroutine was between os.CreateTemp and os.Rename; in-process that goroutine always runs
// to completion and renames the file into place, so there is nothing left to find. Only
// killing a real process inside a window of a few microseconds would show it, and a test
// built on that timing would fail for reasons unrelated to the bug more often than for it.
//
// The cause is guarded next door by TestStatuslineRendersWithoutTouchingTheDisk, which does
// fail against the old code. What THIS test buys is forward cover, and it is cheap: if some
// later change starts writing a temp file from a path that leaves one lying around under
// ordinary operation, this notices. The budget case is included for that reason rather than
// as a reproduction.
func TestStatuslineLeavesNoTempFileBehind(t *testing.T) {
	tests := []struct {
		name  string
		stdin func() io.Reader
		down  bool
	}{
		{name: "an ordinary render", stdin: func() io.Reader { return strings.NewReader(sampleStatusLinePayload) }},
		{name: "the budget expires mid-flight", stdin: func() io.Reader { return blockingReader{} }},
		{name: "the daemon is unreachable", stdin: func() io.Reader { return strings.NewReader(sampleStatusLinePayload) }, down: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateState(t)
			if tc.down {
				t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))
			} else {
				endpoint, _, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})
				t.Setenv(transport.EndpointEnv, endpoint)
			}

			var stdout, stderr bytes.Buffer
			statuslineCommand(tc.stdin(), &stdout, &stderr, transport.Dial{}, statuslineBudget, 0)

			// The abandoned goroutine is still running when the budget case returns, which
			// is the entire mechanism. Give it room to do the damage it used to do, so a
			// pass means the write is gone rather than merely not finished yet.
			time.Sleep(50 * time.Millisecond)

			if left := strays(t, dir); len(left) > 0 {
				t.Errorf("left %d temp file(s) behind: %v", len(left), left)
			}
		})
	}
}

// ageCache shifts the cache's timestamp by d.
func ageCache(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	path := filepath.Join(dir, statuslineCacheFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var c cachedStatus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse cache: %v", err)
	}
	c.At = c.At.Add(d)
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

// TestStatuslineNeverCreatesTheStateDirectory guards design.md section 13.3.
//
// Uninstall is only verifiable if the list of paths this binary touches is exhaustive, and
// a command that runs on every assistant message and recreates the state directory is a
// path uninstall silently stops covering. It is also the wrong behaviour on its own terms:
// a machine where the governor is not installed should not acquire governor state because
// somebody wired the statusline up by hand.
func TestStatuslineNeverCreatesTheStateDirectory(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "never-created")
	t.Setenv(transport.StateDirEnv, absent)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))

	// Through the command, not the renderer: writeCache is the only thing here that could
	// create the directory, and since GOV-33 it runs in the command. Calling statusline
	// alone would assert that a function which no longer touches the disk does not touch
	// the disk.
	var stdout, stderr bytes.Buffer
	statuslineCommand(strings.NewReader(sampleStatusLinePayload), &stdout, &stderr, transport.Dial{}, statuslineBudget, 0)
	line := strings.TrimSuffix(stdout.String(), "\n")

	if !strings.Contains(line, "daemon not running") {
		t.Errorf("line = %q, want it to report the daemon is down", line)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Errorf("state directory %s exists after a statusline run; err = %v", absent, err)
	}
}

// TestStatuslineWhenTheDaemonIsUnreachable is the "renders correctly when the daemon is
// down, says so rather than erroring" clause.
//
// The failure modes are the gate suite's, for the same reason: they are the ways a daemon
// can be absent, and the point of enumerating them is that "down" is not one condition. The
// assertions are the opposite of the gate's though. Where the gate must produce nothing,
// this must produce a sentence, and it must produce it within the same allowance.
func TestStatuslineWhenTheDaemonIsUnreachable(t *testing.T) {
	tests := []struct {
		name     string
		endpoint func(t *testing.T) string
	}{
		{
			name: "no socket on disk",
			endpoint: func(t *testing.T) string {
				return "unix://" + filepath.Join(sockDir(t), "absent")
			},
		},
		{
			name: "a listener that accepts and never answers",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { select {} })
			},
		},
		{
			name: "a listener that answers with rubbish",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { c.Write([]byte("not json\n")) })
			},
		},
		{
			name: "a listener that closes without answering",
			endpoint: func(t *testing.T) string {
				return listenerFunc(t, func(c net.Conn, _ []byte) { c.Close() })
			},
		},
		{
			// Invariant 8: an unrecognised version is answered, and the answer is treated
			// exactly like an unreachable daemon. The numbers in a reply from a protocol
			// this binary does not speak are not numbers it may put on a status line.
			name: "a daemon speaking a protocol this binary does not know",
			endpoint: func(t *testing.T) string {
				ep, _, _ := statusDaemon(t, map[string]any{"v": protocolVersion + 1, "budget": 99, "in_use": 99})
				return ep
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			line := runStatuslineFor(t, tc.endpoint(t), sampleStatusLinePayload, 0)
			if !strings.Contains(line, "daemon not running") {
				t.Errorf("line = %q, want it to say the daemon is not running", line)
			}
			// The number that would be wrong. A line that reported a budget while the
			// daemon was unreachable would be inventing one.
			if strings.Contains(line, "slots") {
				t.Errorf("line = %q, want no slot count when there is no daemon to have counted", line)
			}
		})
	}
}

// TestStatuslineSurvivesAStdinThatNeverCloses is the one hang no socket deadline can catch,
// because it happens before there is a socket. It is the same failure the gate's suite
// covers with blockingReader, and it matters here for a different reason: a statusline
// command that never exits is a child process per assistant message that never exits.
func TestStatuslineSurvivesAStdinThatNeverCloses(t *testing.T) {
	isolateState(t)
	endpoint, _, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	took := timed(func() {
		statuslineCommand(blockingReader{}, &stdout, &stderr, transport.Dial{}, statuslineBudget, 0)
	})
	line := strings.TrimSuffix(stdout.String(), "\n")

	if took.exceeds(statuslineCeiling) {
		t.Errorf("took %v, want under %v", took, statuslineCeiling)
	}
	if line == "" {
		t.Errorf("rendered nothing; the budget expiring must still produce a line")
	}
}

// TestStatuslineExitsZero is why every path above may report a problem in words but never in
// a status code. Claude Code discards a statusline command's stdout entirely unless it
// exited 0 (docs/hook-surface.md section 14), so a non-zero exit does not show the user an
// error — it shows them nothing, which is the one outcome this command exists to prevent.
func TestStatuslineExitsZero(t *testing.T) {
	isolateState(t)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))

	var stdout, stderr bytes.Buffer
	if code := runStatusline(nil, &stdout, &stderr); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Errorf("stdout = %q, want a newline-terminated line", stdout.String())
	}
}

// TestStatuslineRespectsColumns holds the width clause.
//
// Clauses are dropped from the end rather than the text being cut mid-word, because they
// are ordered by how much a reader needs them: the slot count is the point of the line and
// the thermal state is context for it. A narrow terminal should lose the context and keep
// the point.
func TestStatuslineRespectsColumns(t *testing.T) {
	full := "3/5 slots - cpu 61% - thermal ok"
	tests := []struct {
		width int
		want  string
	}{
		{0, full},         // unset: not a constraint, and the usual case
		{-1, full},        // nonsense: also not a constraint
		{len(full), full}, // exactly enough
		{len(full) - 1, "3/5 slots - cpu 61%"},
		{19, "3/5 slots - cpu 61%"},
		{18, "3/5 slots"},
		{9, "3/5 slots"},
		{8, "3/5 slot"}, // narrower than the first clause: cut, because there is no clause left to drop
		{0 + 1, "3"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("width %d", tc.width), func(t *testing.T) {
			isolateState(t)
			endpoint, _, _ := statusDaemon(t, statusReply{
				V: protocolVersion, Budget: 5, InUse: 3, Slots: 3, CPU: f64(0.61), Thermal: "nominal",
			})
			got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, tc.width)
			if got != tc.want {
				t.Errorf("width %d: line = %q, want %q", tc.width, got, tc.want)
			}
			if tc.width > 0 && len([]rune(got)) > tc.width {
				t.Errorf("width %d: line %q is %d runes wide", tc.width, got, len([]rune(got)))
			}
		})
	}
}

// TestStatuslineDropsTheSessionSuffixBeforeCutting is a defect the unit tests above did not
// catch and driving the real binary did.
//
// At COLUMNS=12 the line rendered as "2/5 slots (1" — a slot count sliced through the
// middle, which is worse than no count at all because it still looks like a number. The
// session-local suffix is an addition to the slot count rather than part of it, so it is
// dropped whole before any text is cut.
func TestStatuslineDropsTheSessionSuffixBeforeCutting(t *testing.T) {
	reply := statusReply{
		V: protocolVersion, Budget: 5, InUse: 2, Slots: 2, Thermal: "nominal",
		Sessions: []sessionStat{{Session: sampleSessionID, Confirmed: 1}},
	}
	tests := []struct {
		width int
		want  string
	}{
		{40, "2/5 slots (1 yours) - thermal ok"},
		{32, "2/5 slots (1 yours) - thermal ok"},
		{31, "2/5 slots (1 yours)"},
		{19, "2/5 slots (1 yours)"},
		{18, "2/5 slots"}, // the suffix goes as a unit, rather than being cut to "(1"
		{12, "2/5 slots"},
		{9, "2/5 slots"},
		{5, "2/5 s"}, // nothing left to drop: only now is text cut
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("width %d", tc.width), func(t *testing.T) {
			isolateState(t)
			endpoint, _, _ := statusDaemon(t, reply)
			if got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, tc.width); got != tc.want {
				t.Errorf("width %d: line = %q, want %q", tc.width, got, tc.want)
			}
		})
	}
}

// TestFit drives the width logic directly, including the branch the renderer cannot reach.
//
// The rune-safe cut is NOT reachable through render() today, and that was established by
// mutating it to a byte cut and finding every test still green. The reason is structural
// rather than lucky: the cut only runs when one clause is left and still does not fit, the
// first clause is always the slot count or the daemon-down text, and both are ASCII. So a
// multi-byte thermal value is dropped as a whole clause long before any cut happens.
//
// It is kept, and tested here rather than deleted as dead code, because design.md section 9
// illustrated this line with a leading glyph and multi-byte separators until GOV-B09 deleted
// that illustration — which is to say the multi-byte line is one edit away rather than
// unimaginable. The day anyone renders one, a byte cut splits a character in the one place on
// screen reserved for the governor's own report, and the failure is a replacement glyph that
// looks like a font problem rather than like a bug here.
func TestFit(t *testing.T) {
	tests := []struct {
		name    string
		clauses []string
		width   int
		want    string
	}{
		{"unset width is not a constraint", []string{"a", "b"}, 0, "a - b"},
		{"negative width is not a constraint", []string{"a", "b"}, -5, "a - b"},
		{"exactly enough", []string{"abc", "de"}, 8, "abc - de"},
		{"one short drops the last clause", []string{"abc", "de"}, 7, "abc"},
		{"drops one clause at a time", []string{"keep", "keep", "drop"}, 11, "keep - keep"},
		{"drops from the end, not the front", []string{"keep", "drop", "drop"}, 10, "keep"},
		{"cuts only when one clause is left", []string{"abcdef"}, 3, "abc"},
		{
			// The branch render() cannot reach. A byte cut here returns invalid UTF-8.
			name: "cuts by runes, not bytes", clauses: []string{"◐ héllo wörld"}, width: 7,
			want: "◐ héllo",
		},
		{
			// Multi-byte text that fits by runes but not by bytes must not be touched:
			// the fast path in fit uses len() as an over-estimate and has to fall through.
			name: "multi-byte text within the width is left alone", clauses: []string{"◐ héllo"}, width: 7,
			want: "◐ héllo",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := fit(tc.clauses, tc.width)
			if got != tc.want {
				t.Errorf("fit(%q, %d) = %q, want %q", tc.clauses, tc.width, got, tc.want)
			}
			if tc.width > 0 && len([]rune(got)) > tc.width {
				t.Errorf("fit(%q, %d) = %q, which is %d runes wide", tc.clauses, tc.width, got, len([]rune(got)))
			}
			if strings.ContainsRune(got, '�') {
				t.Errorf("fit(%q, %d) = %q, which contains a replacement character: the cut split a rune",
					tc.clauses, tc.width, got)
			}
		})
	}
}

// TestStatuslineAgainstTheRealDaemon is the wire half.
//
// Everything above drives a scripted listener, which proves the renderer and proves nothing
// about whether this command and the daemon agree on the status operation. This asks the
// real daemon over a real socket, and it is the test that would fail if section 7's status
// reply and this file's statusResponse ever drifted apart.
func TestStatuslineAgainstTheRealDaemon(t *testing.T) {
	isolateState(t)
	endpoint, d := startTestDaemon(t, 4)

	t.Run("an idle daemon", func(t *testing.T) {
		if got, want := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0), "0/4 slots"; !strings.HasPrefix(got, want) {
			t.Errorf("line = %q, want it to start with %q", got, want)
		}
	})

	t.Run("slots in use, and whose they are", func(t *testing.T) {
		// Taken through the table rather than over the wire because the point here is what
		// the status reply says about leases, not how they were acquired.
		limit := 4
		if _, ok := d.table.Acquire(lease.Request{Session: sampleSessionID, Depth: 0}, limit); !ok {
			t.Fatalf("acquire: no room in a budget of %d", limit)
		}
		if _, ok := d.table.Acquire(lease.Request{Session: "another-session", Depth: 0}, limit); !ok {
			t.Fatalf("acquire: no room in a budget of %d", limit)
		}
		d.table.StartSession(lease.Session{ID: sampleSessionID, Weight: 1})

		// A fresh state dir, because the previous subtest cached an idle reading.
		isolateState(t)
		got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		if want := "2/4 slots (1 yours)"; !strings.HasPrefix(got, want) {
			t.Errorf("line = %q, want it to start with %q", got, want)
		}
	})

	t.Run("the request is the status op of section 7", func(t *testing.T) {
		// The daemon answered above, which already proves it accepted the request. This
		// asserts the field it dispatches on and the version every message carries, so a
		// rename shows up here rather than as a silently unrendered line.
		var req request
		if err := json.Unmarshal([]byte(statusRequestLine(t)), &req); err != nil {
			t.Fatalf("parse request: %v", err)
		}
		if req.Op != "status" {
			t.Errorf("op = %q, want %q", req.Op, "status")
		}
		if req.V != protocolVersion {
			t.Errorf("v = %d, want %d", req.V, protocolVersion)
		}
		if req.Principal.User == "" {
			t.Errorf("principal.user is empty; every request carries a principal (section 7)")
		}
	})
}

// statusRequestLine captures the exact bytes the statusline puts on the wire.
func statusRequestLine(t *testing.T) string {
	t.Helper()
	isolateState(t)
	endpoint, _, last := statusDaemon(t, statusReply{V: protocolVersion, Budget: 1})
	runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
	return string(last())
}

// TestGovernorStatusDoesNotUseTheStatuslineCache is the split between the two commands.
//
// `governor status` is typed by a person who has just been told by the status line that
// something is wrong. Answering them from a cache written by the very line that sent them
// there is how a stale reading gets confirmed twice and believed. It must also not WRITE
// that cache: a command run by hand should not change what every session on the machine
// displays for the next second.
func TestGovernorStatusDoesNotUseTheStatuslineCache(t *testing.T) {
	dir := isolateState(t)
	endpoint, calls, _ := statusDaemon(t, statusReply{
		V: protocolVersion, Budget: 6, InUse: 2, Slots: 2, Waiting: 1, CPU: f64(0.5), Thermal: "nominal",
	})
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	if code := runStatus(nil, &stdout, &stderr); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"budget 6", "in use 2", "waiting 1", "cpu 0.50", "thermal nominal"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q is missing %q", stdout.String(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, statuslineCacheFile)); !os.IsNotExist(err) {
		t.Errorf("governor status wrote the statusline cache; err = %v", err)
	}

	// A second run goes to the daemon again rather than to anything it left behind.
	stdout.Reset()
	runStatus(nil, &stdout, &stderr)
	if got := calls.Load(); got != 2 {
		t.Errorf("daemon was asked %d times over 2 runs, want 2", got)
	}
}

// TestGovernorStatusWhenTheDaemonIsDown holds the one fact this command can still report
// without a daemon. Thermal is read from this process, so an unreachable daemon costs the
// budget and the lease table rather than every fact.
func TestGovernorStatusWhenTheDaemonIsDown(t *testing.T) {
	isolateState(t)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))

	var stdout, stderr bytes.Buffer
	if code := runStatus(nil, &stdout, &stderr); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "daemon not running") {
		t.Errorf("stdout = %q, want it to say the daemon is not running", stdout.String())
	}
	if !strings.Contains(stdout.String(), "thermal ") {
		t.Errorf("stdout = %q, want a locally-read thermal line", stdout.String())
	}
	if strings.Contains(stdout.String(), "budget ") {
		t.Errorf("stdout = %q, want no budget line when there is no daemon to have set one", stdout.String())
	}
}

// The docs illustrate the line the renderer actually prints (GOV-B09).
//
// design.md is the file this repo tells every session to read before writing code, and its
// section 9 is where a stranger forms their picture of the one surface they will look at
// every day. It illustrated the line with a leading spinner glyph and middle-dot separators
// for a year after GOV-10 shipped it with neither, because prose is not compiled: the
// correction landed in the paragraph above the illustration and left the illustration alone,
// and by the time GOV-22 rewrote section 13's account of the same line, two sections of one
// document disagreed about it.
//
// Set equality, not containment in either direction, and that is the whole design. Requiring
// only that every illustration renders passes when a file illustrates nothing at all, which
// is the guard going quiet rather than red — the failure mode this repo distrusts most.
// Requiring only that every rendered line is illustrated passes when someone deletes an
// example. Matching the two sets exactly makes a new illustration, a deleted illustration
// and a changed renderer each red, and each red names which.
//
// The corpus is the views the docs illustrate, not every view render can produce. Adding an
// example therefore costs naming the view that produces it, which is the intended price: an
// illustration nobody can name a view for is an illustration nobody checked.
func TestTheDocsIllustrateTheStatusLineThatActuallyRenders(t *testing.T) {
	cpu := 0.61
	renders := map[string]string{} // line -> the view that produced it
	for name, v := range map[string]statusView{
		"section 9, a governed session with both load signals": {
			OK: true, InUse: 3, Slots: 3, Budget: 5, CPU: &cpu, Thermal: signals.Nominal,
		},
		"section 9, the idle count GOV-29 found was being misread": {
			OK: true, InUse: 0, Slots: 0, Budget: 4,
		},
		"section 9, a session whose hooks never loaded": {
			OK: true, Ungoverned: true,
		},
		"section 13, a budget filled by leases that outlived their agents": {
			OK: true, InUse: 4, Slots: 4, Budget: 4,
		},
		// GOV-B01's two override states. Both are deadline-free here on purpose: a rendered
		// "42m left" is a moving target no document could illustrate, so the clause that
		// names the remaining time is covered by TestTheOverrideClausesNameWhenTheyEnd and
		// this guard holds the shape of the line itself.
		"section 9.1, a session whose budget a person has pinned": {
			OK: true, InUse: 2, Slots: 2, Budget: 2, Pinned: true,
		},
		"section 9.1, a session whose gating a person has paused": {
			OK: true, Paused: true,
		},
		"section 9.1, a session whose weight a person has boosted": {
			OK: true, InUse: 1, Slots: 1, Budget: 6, Boosted: true,
		},
	} {
		renders[render(v, 0)] = name
	}

	// Shape, not a list of known strings: a list cannot notice a sixth illustration being
	// added. Matched anywhere in the span rather than anchored, because the illustration this
	// story deleted opened with a glyph and would have slipped an anchored pattern — the one
	// thing the guard exists to catch must be a thing it can see.
	shaped := regexp.MustCompile(`\d+/\d+ slots|\bcpu \d+%|\bthermal (ok|fair|serious|critical)\b|\d+ waiting|governor: (ungoverned|daemon not running|gating paused)|\bpinned\b|\bboosted\b`)
	span := regexp.MustCompile("`([^`]*)`")

	illustrated := map[string]string{} // span -> file:line
	for _, doc := range []string{
		filepath.Join("..", "..", "docs", "design.md"),
		filepath.Join("..", "..", "README.md"),
	} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range span.FindAllStringSubmatch(line, -1) {
				if shaped.MatchString(m[1]) {
					illustrated[m[1]] = fmt.Sprintf("%s:%d", doc, i+1)
				}
			}
		}
	}

	for line, where := range illustrated {
		if _, ok := renders[line]; !ok {
			t.Errorf("%s illustrates the status line as %q, which render never prints",
				where, line)
		}
	}
	for line, view := range renders {
		if _, ok := illustrated[line]; !ok {
			t.Errorf("render prints %q for %s, and neither design.md nor README.md "+
				"illustrates it", line, view)
		}
	}
}

// Section 9's state-directory summary names every file section 13.3 does (GOV-B09).
//
// The second instance of this story's defect, found in the same section as the first. §13.3
// is the normative list of paths — CLAUDE.md says so, and TestUninstallLeavesNothing depends
// on it — while §9 carries a one-line summary of the part under ~/.claude/governor/ for the
// package tour. Four paths reached §13.3 without reaching the summary: daemon.start (GOV-11),
// onboarded (GOV-22), and the sampler's lock and log (GOV-02).
//
// Nobody was careless. A summary in a fenced tree eighty lines from the table it summarises
// is not something a change to the table reminds anyone about, and the only thing that ever
// would is a test. So this is deliberately not a check of §9 against the filesystem or
// against install.go: those already have their own guards, and a third would drift from both.
// It holds one claim only — that the two places in this document naming these paths name the
// same ones — because that is the claim that went false.
func TestSectionNineNamesEveryStateFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "design.md"))
	if err != nil {
		t.Fatalf("read design.md: %v", err)
	}
	doc := string(b)

	// Every ~/.claude/governor/<file> path the document names anywhere, which in practice is
	// §13.3's two tables. Matched by shape rather than by locating the section, so that
	// renumbering a section cannot quietly empty this set.
	named := map[string]bool{}
	for _, m := range regexp.MustCompile("`~/\\.claude/governor/([^`/]+)`").FindAllStringSubmatch(doc, -1) {
		named[m[1]] = true
	}
	if len(named) == 0 {
		t.Fatal("found no state-directory paths in design.md at all, so this test is checking nothing")
	}

	// The summary, which is the text between the tree's "state  →" arrow and the "config  →"
	// line below it, and may wrap over several lines.
	sum := regexp.MustCompile(`(?s)state\s+→ ~/\.claude/governor/\s+(.*?)\nconfig\s+→`).FindStringSubmatch(doc)
	if sum == nil {
		t.Fatal("section 9's tree no longer has a `state  → ~/.claude/governor/` line to check")
	}
	summarised := map[string]bool{}
	for _, f := range strings.FieldsFunc(sum[1], func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		summarised[f] = true
	}

	for f := range named {
		if !summarised[f] {
			t.Errorf("§13.3 names ~/.claude/governor/%s and section 9's summary does not", f)
		}
	}
	for f := range summarised {
		if !named[f] {
			t.Errorf("section 9's summary names %s, which §13.3's table does not — so either "+
				"the summary invented it or uninstall does not know about it", f)
		}
	}
}

// The override clauses say how long is left, and say nothing when there is nothing to say.
//
// TestTheDocsIllustrateTheStatusLineThatActuallyRenders covers the SHAPE of these two lines but
// cannot cover this: a rendered countdown changes every second, so a document that quoted one
// would be wrong by the next tick and the guard would be holding the docs to a moving target.
// This is the other half — that the deadline appears at all, which is the whole of GOV-B01's
// trap, and that an absent one produces no clause rather than a fabricated deadline.
func TestTheOverrideClausesNameWhenTheyEnd(t *testing.T) {
	in42 := time.Now().Add(42 * time.Minute)

	pinned := render(statusView{OK: true, InUse: 2, Slots: 2, Budget: 2, Pinned: true, PinnedUntil: in42}, 0)
	if !strings.Contains(pinned, "pinned") {
		t.Fatalf("a pinned budget does not say so: %q", pinned)
	}
	if !strings.Contains(pinned, "left") {
		t.Errorf("a pinned budget does not say when it ends: %q — an override whose expiry "+
			"is invisible is one nobody can reason about later, which is when it does harm", pinned)
	}

	paused := render(statusView{OK: true, Paused: true, PausedUntil: in42}, 0)
	if !strings.Contains(paused, "paused") || !strings.Contains(paused, "left") {
		t.Errorf("a paused line does not say what it is or when it ends: %q", paused)
	}

	// No deadline, no clause. A surface that printed "pinned until unknown" would be stating
	// a fact the daemon did not send, which is the rule every other clause on this line follows.
	noDeadline := render(statusView{OK: true, InUse: 2, Slots: 2, Budget: 2, Pinned: true}, 0)
	if strings.Contains(noDeadline, "left") || strings.Contains(noDeadline, "until") {
		t.Errorf("invented a deadline the daemon never sent: %q", noDeadline)
	}

	// An already-passed deadline is not rendered as a negative countdown either.
	stale := render(statusView{
		OK: true, InUse: 2, Slots: 2, Budget: 2,
		Pinned: true, PinnedUntil: time.Now().Add(-time.Minute),
	}, 0)
	if strings.Contains(stale, "-") && strings.Contains(stale, "left") {
		t.Errorf("rendered a deadline that has already passed: %q", stale)
	}
}
