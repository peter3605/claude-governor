package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"

	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// reply is the union of the five response shapes in design.md section 7, for decoding only.
//
// The daemon sends a distinct type per operation; a test that has to know which one came
// back before it can read it would assert less than it should. OK is a pointer so that
// "absent" (a status reply) is distinguishable from "false" (a refusal).
type reply struct {
	V         int             `json:"v"`
	OK        *bool           `json:"ok"`
	Reason    string          `json:"reason"`
	Lease     string          `json:"lease"`
	Budget    int             `json:"budget"`
	InUse     int             `json:"in_use"`
	Waiting   int             `json:"waiting"`
	WaitMS    int             `json:"wait_ms"`
	Denials   int             `json:"denials"`
	Share     int             `json:"share"`
	Source    string          `json:"source"`
	Supported []int           `json:"supported"`
	CPU       *float64        `json:"cpu"`
	Thermal   signals.Thermal `json:"thermal"`
	Sources   []sourceStat    `json:"sources"`
	Sessions  []sessionStat   `json:"sessions"`
}

// unixPathMax is the practical ceiling on a unix socket path. sockaddr_un.sun_path is 104
// bytes on darwin and 108 on linux, and a test whose temp directory pushes past it should
// say so rather than fail as if the daemon were broken.
const unixPathMax = 100

// startTestDaemon runs a daemon on a real socket and returns its endpoint.
//
// The daemon is exercised over the wire rather than by calling dispatch directly. That is
// the point of these tests: the concurrency invariant, the malformed-line rule and the
// version reply are all properties of the server, and a test that skipped the socket would
// not have tested the thing the gate actually talks to.
func startTestDaemon(t *testing.T, limit int) (string, *daemon) {
	t.Helper()
	return startTestDaemonWithLeases(t, limit, lease.Options{})
}

// startTestDaemonWithLeases is startTestDaemon over a lease table configured by opt, so a
// test can drive the clock rather than sleep out a thirty-second TTL.
func startTestDaemonWithLeases(t *testing.T, limit int, opt lease.Options) (string, *daemon) {
	t.Helper()

	dir, err := os.MkdirTemp("", "govd")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "s")
	if len(path) > unixPathMax {
		t.Skipf("socket path %q is %d bytes, past the sockaddr_un limit", path, len(path))
	}
	endpoint := "unix://" + path

	ctx, cancel := context.WithCancel(context.Background())
	d := newDaemonWithLeases(limit, io.Discard, opt)
	ln, err := transport.Listen{}.Listen(ctx, endpoint)
	if err != nil {
		cancel()
		t.Fatalf("listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return endpoint, d
}

// callRaw sends one line and returns the one line that comes back, or the read error.
//
// The dial retries on a refused connection. That is not papering over a flaky daemon: on
// darwin a unix connect to a listener whose accept backlog is full is refused immediately
// rather than blocking (design.md section 4.4, measured at 128 queued), so 200 workers
// dialling at once will see ECONNREFUSED from a daemon that is listening perfectly well.
// The gate's answer to that in production is to fail open; a test measuring the lease
// invariant instead waits for the queue to drain, because a connection it never made would
// weaken the very count it is asserting.
func callRaw(t *testing.T, endpoint, line string) (string, error) {
	t.Helper()

	var conn net.Conn
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err = transport.Dial{}.Dial(context.Background(), endpoint)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ECONNREFUSED) || time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", endpoint, err)
		}
		time.Sleep(time.Millisecond)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}
	return bufio.NewReader(conn).ReadString('\n')
}

// call sends one request and decodes the reply.
func call(t *testing.T, endpoint string, req map[string]any) reply {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	line, err := callRaw(t, endpoint, string(body)+"\n")
	if err != nil {
		t.Fatalf("read reply to %v: %v", req, err)
	}
	var r reply
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("decode reply %q: %v", line, err)
	}
	return r
}

func acquireReq(session string) map[string]any {
	return map[string]any{
		"v": protocolVersion, "op": "acquire",
		"principal": map[string]any{"user": "tester", "team": nil},
		"session":   session, "tool_use_id": "toolu_test", "depth": 0, "wait_ms": 0,
	}
}

func TestDaemonServesTheFullLeaseLifecycle(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	if r := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "session", "action": "start",
		"principal": map[string]any{"user": "tester"},
		"session":   "s1", "pid": os.Getpid(), "weight": 1,
	}); r.OK == nil || !*r.OK {
		t.Fatalf("session start refused: %+v", r)
	}

	got := call(t, endpoint, acquireReq("s1"))
	if got.OK == nil || !*got.OK {
		t.Fatalf("acquire refused against an empty table: %+v", got)
	}
	if got.Lease == "" {
		t.Fatal("a granted acquire carried no lease id")
	}
	if got.Budget != 4 || got.InUse != 1 {
		t.Fatalf("acquire reported budget %d in use %d, want 4 and 1", got.Budget, got.InUse)
	}
	if got.Source != "local" {
		t.Fatalf("acquire reported source %q, want the one v1 source, %q", got.Source, "local")
	}

	if r := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "confirm", "session": "s1", "lease": got.Lease,
		"agent_id": "ag_1", "pid": os.Getpid(), "pid_started_at": time.Now().Unix(),
	}); r.OK == nil || !*r.OK {
		t.Fatalf("confirm refused: %+v", r)
	}

	st := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "status"})
	if st.InUse != 1 {
		t.Fatalf("status reports %d in use after a confirm, want 1", st.InUse)
	}
	if len(st.Sessions) != 1 || st.Sessions[0].Confirmed != 1 || st.Sessions[0].Tentative != 0 {
		t.Fatalf("status sessions %+v, want one session holding one confirmed lease", st.Sessions)
	}
	if len(st.Sources) != 1 || st.Sources[0].Name != "local" || st.Sources[0].Budget != 4 {
		t.Fatalf("status sources %+v, want exactly one local source with budget 4", st.Sources)
	}

	if r := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "release", "session": "s1", "agent_id": "ag_1",
	}); r.OK == nil || !*r.OK {
		t.Fatalf("release refused: %+v", r)
	}
	if n := d.table.InUse(); n != 0 {
		t.Fatalf("%d leases held after release, want 0", n)
	}
}

// Every request carries principal, and the daemon decodes it on every operation.
//
// Nothing reads it in v1, which is exactly why it needs a test: a field no code path
// consumes is the kind that gets deleted as dead weight, and retrofitting identity into an
// already-shipped wire format costs far more than carrying it does now. This is the cheapest
// moment it will ever cost.
func TestEveryRequestCarriesPrincipal(t *testing.T) {
	for _, op := range []string{"acquire", "confirm", "release", "session", "status"} {
		line := fmt.Sprintf(
			`{"v":1,"op":%q,"principal":{"user":"peter","team":null},"session":"abc123","agent_id":"ag_1","action":"start"}`, op)

		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("%s: decode: %v", op, err)
		}
		if req.Principal.User != "peter" {
			t.Errorf("%s: principal.user decoded as %q, want %q", op, req.Principal.User, "peter")
		}
		if req.Principal.Team != nil {
			t.Errorf("%s: principal.team decoded as %v, want null", op, req.Principal.Team)
		}
	}

	// A team, when a tier-two broker eventually sets one, must survive the same decode.
	var req request
	if err := json.Unmarshal([]byte(`{"v":1,"op":"acquire","principal":{"user":"peter","team":"platform"}}`), &req); err != nil {
		t.Fatalf("decode with a team: %v", err)
	}
	if req.Principal.Team == nil || *req.Principal.Team != "platform" {
		t.Errorf("principal.team decoded as %v, want \"platform\"", req.Principal.Team)
	}
}

// The daemon accepts a principal on every operation over the wire, not just in the struct.
func TestDaemonAcceptsAPrincipalOnEveryOperation(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)
	pr := map[string]any{"user": "peter", "team": nil}

	for _, req := range []map[string]any{
		{"v": protocolVersion, "op": "session", "action": "start", "principal": pr, "session": "s1", "pid": os.Getpid()},
		{"v": protocolVersion, "op": "acquire", "principal": pr, "session": "s1", "tool_use_id": "toolu_01", "depth": 0},
		{"v": protocolVersion, "op": "confirm", "principal": pr, "session": "s1", "agent_id": "ag_1", "pid": os.Getpid()},
		{"v": protocolVersion, "op": "release", "principal": pr, "session": "s1", "agent_id": "ag_1"},
		{"v": protocolVersion, "op": "status", "principal": pr},
		{"v": protocolVersion, "op": "session", "action": "end", "principal": pr, "session": "s1"},
	} {
		r := call(t, endpoint, req)
		if r.V != protocolVersion {
			t.Errorf("%v: reply is v%d, want v%d", req["op"], r.V, protocolVersion)
		}
	}
}

func TestDaemonRefusesAtCapacity(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 2)

	for i := range 2 {
		if r := call(t, endpoint, acquireReq("s1")); r.OK == nil || !*r.OK {
			t.Fatalf("acquire %d of 2 refused: %+v", i+1, r)
		}
	}
	r := call(t, endpoint, acquireReq("s1"))
	if r.OK == nil || *r.OK {
		t.Fatalf("acquire granted a third lease against a budget of 2: %+v", r)
	}
	if r.Reason != "capacity" {
		t.Fatalf("refusal reason %q, want %q", r.Reason, "capacity")
	}
	if r.Budget != 2 || r.InUse != 2 {
		t.Fatalf("refusal reported budget %d in use %d, want 2 and 2", r.Budget, r.InUse)
	}
	if r.Thermal == "" {
		t.Fatal("a refusal carried no thermal state; the reason text in GOV-08 needs it")
	}
}

// Invariant 8: a version mismatch degrades, it never deadlocks. The daemon answers rather
// than ignoring, and the gate treats that answer exactly as it treats an unreachable daemon.
func TestDaemonAnswersAnUnsupportedVersion(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	r := call(t, endpoint, map[string]any{"v": protocolVersion + 98, "op": "acquire", "session": "s1"})
	if r.OK == nil || *r.OK {
		t.Fatalf("a request in an unknown protocol version was granted: %+v", r)
	}
	if r.Reason != "unsupported_version" {
		t.Fatalf("reason %q, want %q", r.Reason, "unsupported_version")
	}
	if len(r.Supported) != 1 || r.Supported[0] != protocolVersion {
		t.Fatalf("supported versions %v, want [%d]", r.Supported, protocolVersion)
	}
	if r.V != protocolVersion {
		t.Fatalf("the reply itself is v%d, want v%d — the answer must be readable by the version that sent it", r.V, protocolVersion)
	}
	// An unknown version must not have taken a lease on the way to being refused.
	if st := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "status"}); st.InUse != 0 {
		t.Fatalf("%d leases held after a rejected version, want 0", st.InUse)
	}
}

// design.md section 7: the daemon MUST tolerate a malformed line by closing that connection
// only. One bad client never takes down the broker, because every other session on the
// machine is behind it.
func TestMalformedLineClosesOnlyThatConnection(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	for _, bad := range []string{
		"this is not json\n",
		"{\"v\":1,\"op\":\n",
		"[]\n",
		"\x00\x01\x02\n",
		strings.Repeat("x", 5000) + "\n",
	} {
		if line, err := callRaw(t, endpoint, bad); err == nil {
			t.Fatalf("malformed input %.20q was answered with %q, want the connection closed", bad, line)
		}
	}

	// The broker is still serving, on a new connection, with its table intact.
	r := call(t, endpoint, acquireReq("s1"))
	if r.OK == nil || !*r.OK {
		t.Fatalf("the daemon stopped serving after malformed input: %+v", r)
	}
}

func TestDaemonRejectsAnUnknownOp(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)
	r := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "mint_money", "session": "s1"})
	if r.OK == nil || *r.OK {
		t.Fatalf("an unknown op was granted: %+v", r)
	}
	if r.Reason != "unknown_op" {
		t.Fatalf("reason %q, want %q", r.Reason, "unknown_op")
	}
}

// A resumed subagent reaches the CPU with no PreToolUse at all (design.md section 4.3), so
// its confirm has nothing to bind. That is a quiet drop, not an error, and it must not
// invent a lease.
func TestConfirmWithNoPendingLeaseIsNotAnError(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	r := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "confirm", "session": "s1", "agent_id": "ag_resumed", "pid": os.Getpid(),
	})
	if r.OK == nil || *r.OK {
		t.Fatalf("confirm claimed to bind a lease that was never acquired: %+v", r)
	}
	if r.Reason != "no_pending_lease" {
		t.Fatalf("reason %q, want %q", r.Reason, "no_pending_lease")
	}
	if n := d.table.InUse(); n != 0 {
		t.Fatalf("%d leases held after an unbindable confirm, want 0", n)
	}
}

func TestSessionEndReleasesEverythingThatSessionHeld(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)

	call(t, endpoint, acquireReq("s1"))
	call(t, endpoint, acquireReq("s1"))
	call(t, endpoint, acquireReq("s2"))
	if n := d.table.InUse(); n != 3 {
		t.Fatalf("%d leases held, want 3", n)
	}

	call(t, endpoint, map[string]any{"v": protocolVersion, "op": "session", "action": "end", "session": "s1"})
	if n := d.table.InUse(); n != 1 {
		t.Fatalf("%d leases held after s1 ended, want 1 — s2's must survive", n)
	}
}

// The daemon has no sampler until GOV-12. Reporting 0.0 would tell a statusline the machine
// is idle, which is a claim; null says "not measured", which is the truth.
func TestStatusReportsNullCPURatherThanAFabricatedZero(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(line, `"cpu":null`) {
		t.Fatalf("status reply %s carries no null cpu; a fabricated 0 reads as an idle machine", line)
	}
}

// A status reply's shape must not depend on what the table happens to hold. The statusline
// (GOV-10) and doctor (GOV-20) read this, and a sessions key that is an array when busy and
// absent when idle makes every reader handle two shapes for one fact.
func TestStatusKeepsItsShapeWhenNothingIsHeld(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	for _, key := range []string{"v", "budget", "in_use", "waiting", "cpu", "thermal", "sources", "sessions"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("status on an idle daemon omits %q; section 7 lists it unconditionally", key)
		}
	}
	if got := string(raw["sessions"]); got != "[]" {
		t.Errorf("sessions is %s with no session registered, want []", got)
	}
	// ok belongs to the request/response operations, not to status: status is not a
	// decision and has nothing to say yes or no to.
	if _, ok := raw["ok"]; ok {
		t.Error("status carries an \"ok\" field; it answers no question")
	}
}

// The three acquire outcomes each have their own shape in section 7, and GOV-07 is about to
// read them. A grant must not carry a refusal's fields, and a refusal must carry the ones
// GOV-08 needs to name a cause.
func TestAcquireRepliesMatchTheirSpecifiedShapes(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 1)

	grant, err := callRaw(t, endpoint, mustJSON(t, acquireReq("s1"))+"\n")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var g map[string]json.RawMessage
	if err := json.Unmarshal([]byte(grant), &g); err != nil {
		t.Fatalf("decode %q: %v", grant, err)
	}
	for _, key := range []string{"v", "ok", "lease", "budget", "in_use", "share", "source"} {
		if _, ok := g[key]; !ok {
			t.Errorf("a granted acquire omits %q", key)
		}
	}
	if _, ok := g["reason"]; ok {
		t.Error("a granted acquire carries a refusal reason")
	}

	refuse, err := callRaw(t, endpoint, mustJSON(t, acquireReq("s1"))+"\n")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	var r map[string]json.RawMessage
	if err := json.Unmarshal([]byte(refuse), &r); err != nil {
		t.Fatalf("decode %q: %v", refuse, err)
	}
	for _, key := range []string{"v", "ok", "reason", "budget", "in_use", "cpu", "thermal", "source"} {
		if _, ok := r[key]; !ok {
			t.Errorf("a refused acquire omits %q, which GOV-08 needs to name a cause", key)
		}
	}
	if _, ok := r["lease"]; ok {
		t.Error("a refused acquire carries a lease id")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

// The Done-when's named test, over the wire: 200 interleaved acquire and release calls
// against a daemon, asserting the lease count never exceeds the budget.
//
// Both the daemon's own table and every reply's in_use are checked. The wire reading matters
// independently: a table that stayed correct while the daemon reported something else would
// still mislead the statusline, the doctor and the deny reason GOV-08 builds on.
//
// The readings are taken by the workers themselves rather than by a polling goroutine. The
// count can only rise on an acquire, so the worker that just acquired reads at the moment
// the peak happens; a tight poller would contend on the table's mutex with the workers it
// is watching and catch nothing they do not.
//
// Run with -race.
func TestDaemonLeaseCountNeverExceedsBudget(t *testing.T) {
	const (
		limit   = 4
		workers = 200
	)
	endpoint, d := startTestDaemon(t, limit)

	// peak is the highest the daemon's own table ever reached; overWire is the highest it
	// ever reported. Both are raised with a compare-and-swap loop, never a load-then-store,
	// which would lose a raised value to a concurrent writer and quietly weaken the test.
	var peak, overWire atomic.Int64
	raise := func(target *atomic.Int64, n int64) {
		for {
			old := target.Load()
			if n <= old || target.CompareAndSwap(old, n) {
				return
			}
		}
	}

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := fmt.Sprintf("s%d", i%4)

			got := call(t, endpoint, acquireReq(session))
			raise(&overWire, int64(got.InUse))
			if got.OK == nil || !*got.OK {
				return
			}
			raise(&peak, int64(d.table.InUse()))

			agent := fmt.Sprintf("ag_%d", i)
			call(t, endpoint, map[string]any{
				"v": protocolVersion, "op": "confirm", "session": session,
				"lease": got.Lease, "agent_id": agent, "pid": os.Getpid(),
			})
			raise(&peak, int64(d.table.InUse()))
			call(t, endpoint, map[string]any{
				"v": protocolVersion, "op": "release", "session": session, "agent_id": agent,
			})
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("as many as %d leases were held at once against a budget of %d", got, limit)
	}
	if got := peak.Load(); got == 0 {
		t.Fatal("no lease was ever observed to be held; the test proved nothing")
	}
	if got := overWire.Load(); got > limit {
		t.Fatalf("the daemon reported in_use %d on the wire against a budget of %d", got, limit)
	}
	if n := d.table.InUse(); n != 0 {
		t.Fatalf("%d leases held after every worker released, want 0", n)
	}
}

// A second daemon must not unlink the socket the first is serving on: they would take turns
// replacing each other and every gate would reach whichever bound last.
func TestListenRefusesASecondDaemonOnTheSameEndpoint(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	ln, err := transport.Listen{}.Listen(context.Background(), endpoint)
	if err == nil {
		ln.Close()
		t.Fatal("a second daemon bound an endpoint another was already serving")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("error %q does not name the cause", err)
	}
	// The first daemon is untouched.
	if r := call(t, endpoint, acquireReq("s1")); r.OK == nil || !*r.OK {
		t.Fatalf("the original daemon stopped serving: %+v", r)
	}
}

// An interactive fan-out holds one lease per running agent, and holds it for as long as the
// agent runs (GOV-27).
//
// This test exists because the opposite was believed, recorded in the backlog as a launch
// blocker, and was wrong. GOV-09 measured 62 SubagentStop records against zero SubagentStart
// across 25 interactive sessions and concluded that SubagentStart does not fire interactively
// — from which it followed that confirm never runs, no lease is ever bound, and every lease is
// reaped at the 30 s tentative TTL while its agent is still running. GOV-27 re-measured and
// found the correlation confounded: those sessions had made almost no Agent tool calls at all
// (3 in the whole window), because most subagents on that machine came from a spawn path that
// never passes this gate. Driven properly — three Agent spawns in an ordinary interactive
// session — SubagentStart fired for all three, all three confirmed, and the daemon reaped
// nothing. docs/hook-surface.md section 8.1 records the run.
//
// So what is pinned here is the behaviour that actually exists, in the shape the live hooks
// actually produce it:
//
//   - confirm carries NO lease id, because SubagentStart has no tool_use_id to put in one
//     (hook-surface.md section 3). The pairing is per-session FIFO, and the older lifecycle
//     test passes an explicit lease id, so that path was covered and this one was not.
//   - a confirmed lease survives well past the TENTATIVE TTL. That is the assertion the
//     misdiagnosis would have inverted: the proposed fix was to stop reaping unconfirmed
//     leases, which would have removed the only thing that reclaims an abandoned spawn.
//   - the reaper runs throughout, on the real 2 s interval, and takes nothing.
func TestInteractiveFanOutKeepsALeasePerRunningAgent(t *testing.T) {
	clock := &testClock{now: time.Now()}
	endpoint, d := startTestDaemonWithLeases(t, 4, lease.Options{Now: clock.Now})

	if r := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "session", "action": "start",
		"principal": map[string]any{"user": "tester"},
		"session":   "s1", "pid": os.Getpid(), "pid_started_at": time.Now().Unix(), "weight": 1,
	}); r.OK == nil || !*r.OK {
		t.Fatalf("session start refused: %+v", r)
	}

	// Three spawns announced, as a fan-out in one assistant message announces them.
	for i := 1; i <= 3; i++ {
		if got := call(t, endpoint, acquireReq("s1")); got.OK == nil || !*got.OK {
			t.Fatalf("acquire %d refused with %d/4 in use: %+v", i, got.InUse, got)
		}
	}
	if st := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "status"}); st.InUse != 3 ||
		st.Sessions[0].Tentative != 3 {
		t.Fatalf("after three acquires: in_use %d, tentative %d, want 3 and 3",
			st.InUse, st.Sessions[0].Tentative)
	}

	// Three SubagentStart hooks arrive. Each sends an agent_id and the owning session's PID
	// and no lease id at all, which is exactly what buildConfirm puts on the wire.
	agents := []string{"ag_1", "ag_2", "ag_3"}
	for _, ag := range agents {
		if r := call(t, endpoint, map[string]any{
			"v": protocolVersion, "op": "confirm", "session": "s1",
			"agent_id": ag, "pid": os.Getpid(), "pid_started_at": time.Now().Unix(),
		}); r.OK == nil || !*r.OK {
			t.Fatalf("confirm for %s refused: %+v", ag, r)
		}
	}

	st := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "status"})
	if st.InUse != 3 || st.Sessions[0].Confirmed != 3 || st.Sessions[0].Tentative != 0 {
		t.Fatalf("after three confirms: in_use %d, confirmed %d, tentative %d, want 3, 3 and 0",
			st.InUse, st.Sessions[0].Confirmed, st.Sessions[0].Tentative)
	}
	// Every lease names a live owner. A lease with no owner is one the reaper cannot ask
	// about, which is what made the tentative TTL the only thing collecting it.
	if pids := ownerPIDs(d); len(pids) != 3 {
		t.Fatalf("owner pids on held leases: %v, want three non-zero", pids)
	}

	// The agents keep running. Push well past the tentative TTL — the exact window in which
	// the budget was believed to over-subscribe — and let the real reaper have several turns.
	clock.advance(4 * lease.DefaultTentativeTTL)
	waitForReaps(t, d)

	if n := d.table.InUse(); n != 3 {
		t.Fatalf("%d leases held %v after the fan-out started, want all 3 still held: a live "+
			"agent's slot was handed back while it was still running", n, 4*lease.DefaultTentativeTTL)
	}

	// Each agent stops, and each stop returns exactly its own slot.
	for i, ag := range agents {
		r := call(t, endpoint, map[string]any{
			"v": protocolVersion, "op": "release", "session": "s1", "agent_id": ag,
		})
		if r.OK == nil || !*r.OK {
			t.Fatalf("release of %s refused: %+v", ag, r)
		}
		if want := 2 - i; r.InUse != want {
			t.Fatalf("after releasing %s, in_use %d, want %d", ag, r.InUse, want)
		}
	}
}

// An announced spawn that never happens is still reclaimed at the tentative TTL (GOV-27).
//
// This is the other half of the test above and the reason the fix GOV-27 proposed was not
// applied. The tentative TTL is what collects a spawn the user declined or interrupted, and a
// lease that no longer expires while unconfirmed is a slot lost until the session ends. Both
// properties have to hold at once, so both are asserted next to each other.
func TestAnAnnouncedSpawnThatNeverHappensIsStillReclaimed(t *testing.T) {
	clock := &testClock{now: time.Now()}
	endpoint, d := startTestDaemonWithLeases(t, 4, lease.Options{Now: clock.Now})

	if got := call(t, endpoint, acquireReq("s1")); got.OK == nil || !*got.OK {
		t.Fatalf("acquire refused against an empty table: %+v", got)
	}
	if n := d.table.InUse(); n != 1 {
		t.Fatalf("%d leases held after one acquire, want 1", n)
	}

	// No confirm ever arrives: the spawn was announced and did not happen.
	clock.advance(lease.DefaultTentativeTTL + time.Second)
	waitForReaps(t, d)

	if n := d.table.InUse(); n != 0 {
		t.Fatalf("%d leases held past the tentative TTL with no confirm, want 0: an abandoned "+
			"spawn is holding a slot", n)
	}
}

// ownerPIDs is the non-zero owning PID of every lease currently held.
func ownerPIDs(d *daemon) []int {
	var out []int
	for _, l := range d.table.Leases() {
		if l.OwnerPID != 0 {
			out = append(out, l.OwnerPID)
		}
	}
	return out
}

// waitForReaps blocks until the daemon's reaper has certainly run at least twice, so that a
// test asserting what the reaper did or did not take is not racing it.
func waitForReaps(t *testing.T, d *daemon) {
	t.Helper()
	time.Sleep(reapInterval*2 + 250*time.Millisecond)
}

// testClock is a hand-wound clock, safe for the reaper goroutine to read while a test writes.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
