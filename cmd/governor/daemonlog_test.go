package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The daemon log (GOV-B29). Dogfooding showed the budget at 1 on four evenings, and nothing
// on disk said what cut it, when, or when it came back. Each test below pins one of the
// answers that were missing.

// lockedBuffer is a log sink a test can read while the daemon's goroutines write to it. A
// release line is written AFTER its reply, so the test has to poll for it, and a plain
// bytes.Buffer read during a write is a data race.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// rfc3339Line is a log line with the timestamp every daemon line now leads with.
var rfc3339Line = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2}) \S`)

// budgetMoves returns the budget-transition lines in log, without timestamp or prefix.
func budgetMoves(log string) []string {
	var out []string
	for _, l := range logLines(bytes.NewBufferString(log)) {
		for _, verb := range []string{"cut ", "climb ", "restore ", "ceiling ", "move "} {
			if strings.HasPrefix(l, verb) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// TestEveryBudgetChangeIsALineNamingItsCause is clause 1. It drives the daemon's own tick,
// d.step, through every cause the controller has. That includes a ceiling change arriving
// through the real config reload, and each one has to come out as exactly one line with the
// reading next to its threshold.
//
// Every tick that does NOT move the budget writes nothing. The idle stretch below is 60
// ticks and the thermal recovery several more, and a log that grew a line per tick would
// fail the exact count.
func TestEveryBudgetChangeIsALineNamingItsCause(t *testing.T) {
	state := t.TempDir()
	t.Setenv(transport.StateDirEnv, state)
	path := filepath.Join(state, config.File)
	writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 4, "run_queue_max": 6.0,
		"calibrated_at": "2026-11-13T00:00:00Z"})

	var log lockedBuffer
	d := newDaemon(4, &log)
	cal, loaded := loadCalibration(io.Discard)
	d.loaded, d.configStamp, d.calibratedBMax = &loaded, stampOf(path), cal.BMax
	d.loop = &budget.Controller{Max: 4, RunQueueMax: resolveRunQueueMax(cal, io.Discard)}

	// Capacity 12 puts MaxSlots at 10, so the calibrated b_max is what sets the ceiling.
	tick := func(rq float64, thermal signals.Thermal, demand bool) {
		d.step(signals.Sample{RunQueue: rq, Thermal: thermal, CPUCapacity: 12}, demand, false)
	}
	idle := func(n int) {
		for i := 0; i < n; i++ {
			tick(0.5, signals.Nominal, false)
		}
	}

	tick(0.5, signals.Nominal, false) // first tick: starts at the ceiling, nothing moved
	tick(10, signals.Nominal, false)  // run queue over the cut: 4 -> 2
	tick(0.5, signals.Nominal, true)  // recovered, but the hold counter is not yet clean
	tick(0.5, signals.Nominal, true)  // recovered with demand waiting: 2 -> 3
	idle(budget.IdleTicksBeforeReset) // idle long enough: 3 -> 4
	tick(0.5, signals.Serious, false) // thermal: 4 -> 1
	idle(budget.NominalTicksBeforeIncrease + budget.IdleTicksBeforeReset)

	// The ceiling moving, through the reload a `governor calibrate` triggers. The budget is
	// at the ceiling, so it follows it up, and then it is clamped back down.
	time.Sleep(10 * time.Millisecond) // a new mtime for the stamp
	writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 6, "run_queue_max": 6.0,
		"calibrated_at": "2026-11-13T01:00:00Z"})
	d.reloadConfig()
	idle(1)
	time.Sleep(10 * time.Millisecond)
	writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 3, "run_queue_max": 6.0,
		"calibrated_at": "2026-11-13T02:00:00Z"})
	d.reloadConfig()
	idle(1)

	want := []string{
		"cut 4->2 cause=run-queue rq=10.00 cut=6.00 ceiling=4",
		"climb 2->3 cause=climb rq=0.50 climb=5.50 ceiling=4",
		"restore 3->4 cause=idle rq=0.50 climb=5.50 ceiling=4",
		"cut 4->1 cause=thermal thermal=serious ceiling=4",
		"restore 1->4 cause=idle rq=0.50 climb=5.50 ceiling=4",
		"ceiling 4->6 cause=ceiling ceiling=4->6",
		"ceiling 6->3 cause=ceiling ceiling=6->3",
	}
	got := budgetMoves(log.String())
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("budget lines:\n  %s\nwant:\n  %s\nwhole log:\n%s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "), log.String())
	}
	for _, l := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if !rfc3339Line.MatchString(l) {
			t.Errorf("untimestamped line: %q", l)
		}
	}
}

// TestEveryReleaseIsALineNamingWhatReleasedIt is clause 2, over the socket the hooks use.
// Each path a lease can leave by is taken once, and each must produce one line carrying
// session, kind, class, weight, how long it was held, and what released it. Together those
// are what slot-seconds by class are summed from (GOV-B33).
func TestEveryReleaseIsALineNamingWhatReleasedIt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	var log lockedBuffer
	endpoint, _ := startTestDaemonConfigured(t, 8, lease.Options{Now: clock.Now, TentativeTTL: time.Minute},
		func(d *daemon) { d.diag = newDaemonLog(&log) })

	build := func(session, toolUse, class string, slots float64) {
		t.Helper()
		r := acquireReq(session)
		r["tool_use_id"], r["kind"], r["class"], r["slots"] = toolUse, "build", class, slots
		if got := call(t, endpoint, r); got.Lease == "" {
			t.Fatalf("build %s was not granted: %+v", toolUse, got)
		}
	}
	finish := func(session, toolUse, event string) {
		t.Helper()
		call(t, endpoint, map[string]any{"v": protocolVersion, "op": "finish", "session": session,
			"tool_use_id": toolUse, "hook_event": event})
	}

	build("s1", "tu_heavy", "heavy", 2)
	clock.advance(83456 * time.Millisecond)
	finish("s1", "tu_heavy", "PostToolUse")

	build("s1", "tu_failed", "medium", 1)
	clock.advance(2 * time.Second)
	finish("s1", "tu_failed", "PostToolUseFailure")

	// An older hook sends no event, and the only one it could have been is PostToolUse.
	// A value nobody registered `finish` for is not echoed into a log that gets pasted.
	build("s1", "tu_old", "medium", 1)
	finish("s1", "tu_old", "")
	build("s1", "tu_odd", "<script>", 1)
	finish("s1", "tu_odd", "rm -rf /")

	call(t, endpoint, acquireReq("s1"))
	call(t, endpoint, map[string]any{"v": protocolVersion, "op": "confirm", "session": "s1", "agent_id": "ag_1"})
	clock.advance(10 * time.Second)
	call(t, endpoint, map[string]any{"v": protocolVersion, "op": "release", "session": "s1", "agent_id": "ag_1"})

	call(t, endpoint, acquireReq("s2"))
	clock.advance(time.Second)
	call(t, endpoint, map[string]any{"v": protocolVersion, "op": "session", "session": "s2", "action": "end"})

	call(t, endpoint, acquireReq("s3"))
	clock.advance(2 * time.Minute) // past the tentative TTL: the reaper's to collect

	want := []string{
		"release session=s1 kind=build class=heavy weight=2.00 held=1m23.456s by=PostToolUse",
		"release session=s1 kind=build class=medium weight=1.00 held=2s by=PostToolUseFailure",
		"release session=s1 kind=build class=medium weight=1.00 held=0s by=PostToolUse",
		"release session=s1 kind=build class=- weight=1.00 held=0s by=PostToolUse",
		"release session=s1 kind=agent class=- weight=1.00 held=10s by=SubagentStop",
		"release session=s2 kind=agent class=- weight=1.00 held=1s by=SessionEnd",
		"release session=s3 kind=agent class=- weight=1.00 held=2m0s by=reaper:expired",
	}
	releases := func() []string {
		var out []string
		for _, l := range logLines(bytes.NewBufferString(log.String())) {
			if strings.HasPrefix(l, "release ") {
				out = append(out, l)
			}
		}
		return out
	}
	deadline := time.Now().Add(3 * reapInterval)
	for len(releases()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := releases(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("release lines:\n  %s\nwant:\n  %s\nwhole log:\n%s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "), log.String())
	}
}

// TestNoDaemonLogLineIsUntimestamped is clause 3, on a real `governor daemon` writing the
// real file the way install and a lazy start launch it: stdout and stderr both opened on
// daemon.log. It does the things that produced the untimestamped lines dogfooding could not
// date: start-up, a connection closed before it sent anything (read request: EOF), a
// session ending, and a lease released. Then every line must parse with a leading RFC 3339
// timestamp.
func TestNoDaemonLogLineIsUntimestamped(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real binary")
	}
	state := sockDir(t)
	endpoint := "unix://" + filepath.Join(state, "sock")
	logPath := filepath.Join(state, daemonLogFile)
	lf, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()

	cmd := exec.Command(realGovernor(t), "daemon")
	cmd.Env = append(os.Environ(), transport.EndpointEnv+"="+endpoint, transport.StateDirEnv+"="+state)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)

	deadline := time.Now().Add(5 * time.Second)
	for !daemonAnswers(endpoint) {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not answer within 5s; log:\n%s", readDaemonLog(state))
		}
		time.Sleep(20 * time.Millisecond)
	}

	if c, err := net.Dial("unix", filepath.Join(state, "sock")); err == nil {
		c.Close() // read request: EOF
	}
	r := acquireReq("e2e")
	r["kind"], r["class"], r["slots"], r["tool_use_id"] = "build", "medium", 1, "tu_e2e"
	call(t, endpoint, r)
	call(t, endpoint, map[string]any{"v": protocolVersion, "op": "finish", "session": "e2e", "tool_use_id": "tu_e2e"})
	call(t, endpoint, acquireReq("e2e"))
	call(t, endpoint, map[string]any{"v": protocolVersion, "op": "session", "session": "e2e", "action": "end"})
	time.Sleep(100 * time.Millisecond) // the release lines follow their replies
	stop()

	raw := readDaemonLog(state)
	for _, wantIn := range []string{"listening on", "read request: EOF", "session e2e ended",
		"by=PostToolUse", "by=SessionEnd"} {
		if !strings.Contains(raw, wantIn) {
			t.Errorf("the log has no %q line; log:\n%s", wantIn, raw)
		}
	}
	for _, l := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if !rfc3339Line.MatchString(l) {
			t.Errorf("untimestamped line: %q", l)
		}
	}
}

// TestALineAssembledFromSeveralWritesIsStampedOnce: the stamp goes at the start of a line,
// not at the start of a Write. Otherwise a line built by two Fprintf calls would carry a
// timestamp in its middle, and a line-oriented reader would split it into two records.
func TestALineAssembledFromSeveralWritesIsStampedOnce(t *testing.T) {
	var b bytes.Buffer
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	w := &stampWriter{w: &b, now: func() time.Time { return at }}
	io.WriteString(w, "one ")
	io.WriteString(w, "line\ntwo\nthree")
	io.WriteString(w, " ends\n")
	want := "2026-11-13T09:30:00Z one line\n2026-11-13T09:30:00Z two\n2026-11-13T09:30:00Z three ends\n"
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// TestTheDaemonLogRotatesAtItsBound is clause 4's bound. Past daemonLogMaxBytes the file
// becomes daemon.log.1 and a fresh daemon.log begins. The descriptor the daemon was handed
// follows the new file, which is what keeps a panic trace written straight to fd 2 out of
// the rotated generation.
func TestTheDaemonLogRotatesAtItsBound(t *testing.T) {
	old := daemonLogMaxBytes
	daemonLogMaxBytes = 64
	t.Cleanup(func() { daemonLogMaxBytes = old })

	state := t.TempDir()
	path := filepath.Join(state, daemonLogFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sink := daemonLogSink(f, f, state)
	if _, ok := sink.(*rotatingLog); !ok {
		t.Fatalf("stderr opened on the state directory's daemon.log was not given a bound: %T", sink)
	}
	w := newDaemonLog(sink)
	first := strings.Repeat("x", 70) + "\n"
	io.WriteString(w, first)            // crosses the bound: rotated after this write
	io.WriteString(w, "after rotate\n") // lands in the fresh file
	f.WriteString("raw fd write\n")     // as the runtime would: straight to the descriptor

	rotated, err := os.ReadFile(path + daemonLogRotatedSuffix)
	if err != nil {
		t.Fatalf("no rotated generation: %v", err)
	}
	if !strings.Contains(string(rotated), first) || strings.Contains(string(rotated), "after rotate") {
		t.Errorf("daemon.log.1 = %q, want exactly the line that crossed the bound", rotated)
	}
	live, _ := os.ReadFile(path)
	if !strings.Contains(string(live), "after rotate") || !strings.Contains(string(live), "raw fd write") {
		t.Errorf("daemon.log = %q, want both later writes in the fresh file", live)
	}

	// Anything that is not the state directory's daemon.log is left alone: a terminal, a
	// test buffer, tools/sweep's per-arm file.
	other, _ := os.Create(filepath.Join(t.TempDir(), "elsewhere.log"))
	defer other.Close()
	if _, ok := daemonLogSink(other, other, state).(*rotatingLog); ok {
		t.Error("a log file outside the state directory was given the state directory's bound")
	}
}

// TestAReleaseLineIsWrittenAfterTheReply is clause 5 for the release path. The hook gets its
// answer before the log line exists, so writing the log adds nothing to the time it waits.
func TestAReleaseLineIsWrittenAfterTheReply(t *testing.T) {
	d := newDaemon(4, io.Discard)
	// The log writer waits for the client to have its reply. If the line were written
	// first, handle would block here before replying, the client would block reading, and
	// the wait would time out. That is reported as the defect, not as a hang.
	replied := make(chan struct{})
	var mu sync.Mutex
	var logged []string
	d.diag = newDaemonLog(writerFunc(func(p []byte) (int, error) {
		when := "after"
		select {
		case <-replied:
		case <-time.After(time.Second):
			when = "before"
		}
		mu.Lock()
		logged = append(logged, when+" the reply: "+strings.TrimSpace(string(p)))
		mu.Unlock()
		return len(p), nil
	}))
	d.table.Take(lease.Request{Session: "s", Kind: lease.Build, ToolUseID: "tu"})

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); d.handle(server) }()
	body, _ := json.Marshal(map[string]any{"v": protocolVersion, "op": "finish", "session": "s", "tool_use_id": "tu"})
	client.SetDeadline(time.Now().Add(5 * time.Second))
	client.Write(append(body, '\n'))
	buf := make([]byte, 512)
	if _, err := client.Read(buf); err != nil {
		t.Fatalf("no reply: %v", err)
	}
	close(replied)
	client.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "after the reply: ") || !strings.Contains(logged[0], "release ") {
		t.Fatalf("logged = %q, want one release line, written after the reply", logged)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
