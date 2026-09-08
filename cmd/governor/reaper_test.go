package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
	"github.com/shirou/gopsutil/v4/process"
)

// The reaper, end to end (GOV-09).
//
// GOV-06 built the reaper and tested it through a hand-wound clock and a stub Liveness. That
// proved the branching and nothing about the machinery underneath: no test had killed a real
// process, and no test had let the daemon's own two-second ticker do the collecting. Those
// two gaps are the whole of this file, because the reaper is the mechanism the budget's
// correctness actually rests on — the stop hooks are assumed never to fire (invariant 2),
// so every leak the design expects is collected here or not at all.

// killableSession starts a real process to stand in for a session's `claude`, and returns
// what `session start` would send for it.
//
// A real process is the point. The daemon resolves liveness through PIDLiveness, which reads
// the system process table, so a fabricated PID would test the arithmetic and skip the part
// that has never run.
func killableSession(t *testing.T) (pid int, startedAt int64, kill func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no sleep(1)")
	}
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in session: %v", err)
	}
	pid = cmd.Process.Pid

	// The gate sends whole unix seconds (design.md section 7), so this is the same lossy
	// value the daemon will rebuild with time.Unix.
	startedAt = processStartUnix(t, pid)

	killed := false
	kill = func() {
		if killed {
			return
		}
		killed = true
		_ = cmd.Process.Kill()
		// Wait, not just signal. A killed child of the test process stays in the process
		// table as a zombie until its parent reaps it, and a zombie is still findable — the
		// reaper would correctly report it alive and this test would fail for a reason with
		// nothing to do with the daemon. The daemon is never a session's parent in
		// production, so it never owes anyone this wait.
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(kill)
	return pid, startedAt, kill
}

// processStartUnix reads a live process's start time in whole unix seconds, which is the
// shape `confirm` and `session start` carry on the wire (design.md section 7).
func processStartUnix(t *testing.T, pid int) int64 {
	t.Helper()
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		t.Fatalf("process.NewProcess(%d): %v", pid, err)
	}
	ms, err := p.CreateTimeWithContext(context.Background())
	if err != nil {
		t.Fatalf("CreateTime(%d): %v", pid, err)
	}
	return time.UnixMilli(ms).Unix()
}

// startReapTestDaemon runs a daemon whose lease TTLs are short enough to watch, and returns
// its endpoint and a stop function.
//
// The reap INTERVAL is deliberately left at the shipped two seconds. The claim these tests
// make is "within one reaper interval", and shortening the interval would turn that into a
// claim about a number no user ever runs.
func startReapTestDaemon(t *testing.T, limit int, opt lease.Options) (endpoint string, stop func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "govreap")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "s")
	if len(path) > unixPathMax {
		t.Skipf("socket path %q is %d bytes, past the sockaddr_un limit", path, len(path))
	}
	endpoint = "unix://" + path
	stop = serveOn(t, endpoint, limit, opt)
	return endpoint, stop
}

// serveOn starts a daemon on an existing endpoint and returns a function that stops it and
// waits for it to be gone. Separated from startReapTestDaemon so the restart test can bring
// a second daemon up on the same socket path.
func serveOn(t *testing.T, endpoint string, limit int, opt lease.Options) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	d := newDaemonWithLeases(limit, io.Discard, opt)
	ln, err := transport.Listen{}.Listen(ctx, endpoint)
	if err != nil {
		cancel()
		t.Fatalf("listen on %s: %v", endpoint, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.serve(ctx, ln)
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}

// waitForInUse polls status until in_use reaches want, and reports what it saw if it never
// does. Polling rather than sleeping once, so the test measures how long collection actually
// took instead of asserting against a sleep that happened to be long enough.
func waitForInUse(t *testing.T, endpoint string, want int, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(within)
	last := -1
	for {
		r := call(t, endpoint, map[string]any{
			"v": protocolVersion, "op": "status",
			"principal": map[string]any{"user": "tester", "team": nil},
		})
		last = r.InUse
		if last == want {
			return time.Since(start)
		}
		if time.Now().After(deadline) {
			t.Fatalf("in_use is %d after %v, want %d", last, within, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func sessionStartReq(session string, pid int, startedAt int64) map[string]any {
	return map[string]any{
		"v": protocolVersion, "op": "session", "action": "start",
		"principal": map[string]any{"user": "tester", "team": nil},
		"session":   session, "pid": pid, "pid_started_at": startedAt, "weight": 1,
	}
}

func confirmReq(session, leaseID, agentID string, pid int, startedAt int64) map[string]any {
	return map[string]any{
		"v": protocolVersion, "op": "confirm",
		"principal": map[string]any{"user": "tester", "team": nil},
		"session":   session, "lease": leaseID, "agent_id": agentID,
		"pid": pid, "pid_started_at": startedAt,
	}
}

// The story's central claim: SIGKILL a session mid-fan-out and every lease it held comes
// back within one reaper interval.
//
// "Mid-fan-out" is doing real work in that sentence. It is the state where some spawns have
// reached SubagentStart and one has not, so the session holds confirmed leases AND a
// tentative one at the moment it dies. Those are collected by two different mechanisms — a
// tentative lease has no owner of its own, because OwnerPID is bound at Confirm and nowhere
// else — and a test that confirmed everything first would have missed that the tentative one
// used to sit there for its full thirty-second TTL with its session already gone.
func TestLeasesOfAKilledSessionAreReclaimedWithinOneReaperInterval(t *testing.T) {
	pid, startedAt, kill := killableSession(t)
	// The confirmed TTL is left at its default: nothing here may pass because a TTL expired.
	// Liveness has to be what collects these, or the test is not testing the kill.
	endpoint, _ := startReapTestDaemon(t, 8, lease.Options{})

	const session = "S-killed"
	if r := call(t, endpoint, sessionStartReq(session, pid, startedAt)); r.OK == nil || !*r.OK {
		t.Fatalf("session start: %+v", r)
	}

	// Four spawns: three that reached SubagentStart, one still tentative.
	var leases []string
	for i := 0; i < 4; i++ {
		r := call(t, endpoint, acquireReq(session))
		if r.OK == nil || !*r.OK {
			t.Fatalf("acquire %d refused: %+v", i, r)
		}
		leases = append(leases, r.Lease)
	}
	for i := 0; i < 3; i++ {
		r := call(t, endpoint, confirmReq(session, leases[i], fmt.Sprintf("agent-%d", i), pid, startedAt))
		if r.OK == nil || !*r.OK {
			t.Fatalf("confirm %d: %+v", i, r)
		}
	}

	st := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "status",
		"principal": map[string]any{"user": "tester", "team": nil},
	})
	if st.InUse != 4 {
		t.Fatalf("in_use=%d before the kill, want 4", st.InUse)
	}
	var confirmed, tentative int
	for _, s := range st.Sessions {
		if s.Session == session {
			confirmed, tentative = s.Confirmed, s.Tentative
		}
	}
	if confirmed != 3 || tentative != 1 {
		t.Fatalf("session holds %d confirmed and %d tentative, want 3 and 1 — this test is "+
			"only meaningful if both kinds are held at the moment of the kill", confirmed, tentative)
	}

	kill()

	// One reaper interval is two seconds; the slack is for the poll and for a loaded CI box,
	// not for a second interval to come round.
	took := waitForInUse(t, endpoint, 0, reapInterval+2*time.Second)
	t.Logf("all 4 leases of a SIGKILLed session reclaimed in %v (reap interval %v)", took, reapInterval)

	// And the slot is genuinely usable again, not merely uncounted.
	if r := call(t, endpoint, acquireReq("S-next")); r.OK == nil || !*r.OK {
		t.Errorf("acquire after the reap was refused: %+v", r)
	}
}

// The other half of the same claim: a spawn that was announced and never happened is
// collected at its TTL, with no owner to ask about.
//
// This is the abandoned-spawn case — the user declined the permission prompt, the turn was
// interrupted, the model changed its mind. Nothing dies, nothing reports anything, and the
// only thing that reclaims the slot is the tentative TTL running out. The session here is
// deliberately left ALIVE so that liveness cannot be what collects it.
func TestUnconfirmedTentativeLeaseIsReclaimedAtItsTTL(t *testing.T) {
	pid, startedAt, _ := killableSession(t)

	const ttl = 500 * time.Millisecond
	endpoint, _ := startReapTestDaemon(t, 4, lease.Options{TentativeTTL: ttl})

	const session = "S-abandoned"
	if r := call(t, endpoint, sessionStartReq(session, pid, startedAt)); r.OK == nil || !*r.OK {
		t.Fatalf("session start: %+v", r)
	}
	r := call(t, endpoint, acquireReq(session))
	if r.OK == nil || !*r.OK {
		t.Fatalf("acquire refused: %+v", r)
	}

	took := waitForInUse(t, endpoint, 0, ttl+reapInterval+2*time.Second)
	if took < ttl {
		t.Errorf("the tentative lease went in %v, before its %v TTL had run — a spawn that "+
			"is merely slow to reach SubagentStart would have its slot taken away from it",
			took, ttl)
	}
	t.Logf("unconfirmed tentative lease reclaimed in %v (ttl %v, reap interval %v)", took, ttl, reapInterval)

	// The session is still alive, so this must not have been liveness doing the work.
	st := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "status",
		"principal": map[string]any{"user": "tester", "team": nil},
	})
	found := false
	for _, s := range st.Sessions {
		if s.Session == session {
			found = true
		}
	}
	if !found {
		t.Errorf("the session was dropped along with its lease, but its process is still "+
			"running; status reports %+v", st.Sessions)
	}
}

// A confirmed lease whose owner is alive is spared, however long it has been held.
//
// This is the guard on the test above. A reaper that collected everything on a timer would
// satisfy both TTL tests and would be catastrophically wrong: it would free slots out from
// under running agents, and the budget would over-subscribe the machine exactly when it is
// busiest.
func TestALiveOwnersConfirmedLeaseSurvivesSeveralReaperIntervals(t *testing.T) {
	pid, startedAt, _ := killableSession(t)

	// A tentative TTL far shorter than the test's runtime, so that the lease would certainly
	// be collected if confirming it did not move it onto the confirmed TTL.
	endpoint, _ := startReapTestDaemon(t, 4, lease.Options{TentativeTTL: 250 * time.Millisecond})

	const session = "S-alive"
	if r := call(t, endpoint, sessionStartReq(session, pid, startedAt)); r.OK == nil || !*r.OK {
		t.Fatalf("session start: %+v", r)
	}
	acq := call(t, endpoint, acquireReq(session))
	if acq.OK == nil || !*acq.OK {
		t.Fatalf("acquire refused: %+v", acq)
	}
	if r := call(t, endpoint, confirmReq(session, acq.Lease, "agent-live", pid, startedAt)); r.OK == nil || !*r.OK {
		t.Fatalf("confirm: %+v", r)
	}

	// Long enough for two full reaper passes and many tentative TTLs.
	time.Sleep(2*reapInterval + 500*time.Millisecond)

	st := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "status",
		"principal": map[string]any{"user": "tester", "team": nil},
	})
	if st.InUse != 1 {
		t.Fatalf("in_use=%d after %v with the owner still running, want 1 — the reaper is "+
			"freeing a slot out from under a live agent", st.InUse, 2*reapInterval)
	}
}

// The daemon survives a restart without leaking the leases it held.
//
// In-memory lease state is a decision, not a shortcut (design.md section 12.2): a restart
// loses everything and the reaper cleans up in seconds. What "without leaking" therefore
// means is that the new daemon starts with its budget entirely free — the old daemon's
// leases must not survive as phantom occupancy that nothing can ever release, because no
// gate holds a handle that could release them any more.
//
// It also has to be able to take the socket at all. transport.Listen probes an existing
// socket before unlinking it, precisely so that two daemons cannot take turns replacing each
// other's, and a restart is the one case where that probe must find the old socket dead and
// proceed.
func TestDaemonRestartDoesNotLeakTheLeasesItHeld(t *testing.T) {
	pid, startedAt, _ := killableSession(t)

	const budgetN = 3
	endpoint, stop := startReapTestDaemon(t, budgetN, lease.Options{})

	const session = "S-restart"
	if r := call(t, endpoint, sessionStartReq(session, pid, startedAt)); r.OK == nil || !*r.OK {
		t.Fatalf("session start: %+v", r)
	}
	var held []string
	for i := 0; i < budgetN; i++ {
		r := call(t, endpoint, acquireReq(session))
		if r.OK == nil || !*r.OK {
			t.Fatalf("acquire %d refused: %+v", i, r)
		}
		held = append(held, r.Lease)
	}
	// Confirmed, so that these are leases with a live owner: the strongest form of "held".
	// Nothing about the owner changes across the restart, so anything that survived would
	// survive for the full confirmed TTL.
	for i, id := range held {
		if r := call(t, endpoint, confirmReq(session, id, fmt.Sprintf("agent-%d", i), pid, startedAt)); r.OK == nil || !*r.OK {
			t.Fatalf("confirm %d: %+v", i, r)
		}
	}
	if r := call(t, endpoint, acquireReq(session)); r.OK == nil || *r.OK {
		t.Fatalf("acquire at a full budget was granted: %+v", r)
	}

	stop()

	// Same endpoint, new daemon.
	serveOn(t, endpoint, budgetN, lease.Options{})

	st := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "status",
		"principal": map[string]any{"user": "tester", "team": nil},
	})
	if st.InUse != 0 {
		t.Errorf("in_use=%d immediately after a restart, want 0 — the previous daemon's "+
			"leases are phantom occupancy that no gate can release", st.InUse)
	}
	if st.Budget != budgetN {
		t.Errorf("budget=%d after a restart, want %d", st.Budget, budgetN)
	}
	if len(st.Sessions) != 0 {
		t.Errorf("the new daemon reports %d session(s), want none: %+v", len(st.Sessions), st.Sessions)
	}

	// The whole budget is available again.
	for i := 0; i < budgetN; i++ {
		r := call(t, endpoint, acquireReq(session))
		if r.OK == nil || !*r.OK {
			t.Fatalf("acquire %d after the restart was refused, so the budget did not come "+
				"back: %+v", i, r)
		}
	}

	// A gate that was mid-exchange when the daemon went down will send confirm and release
	// for leases the new daemon has never heard of. Both are ordinary answers, not errors:
	// this is the same shape as a resumed subagent, which reaches the CPU with no PreToolUse
	// at all (design.md section 4.3).
	if r := call(t, endpoint, confirmReq("S-ghost", held[0], "agent-ghost", pid, startedAt)); r.OK == nil || *r.OK {
		if r.Reason != "no_pending_lease" {
			t.Errorf("confirm of a lease from the previous daemon answered %q, want "+
				"no_pending_lease", r.Reason)
		}
	}
	rel := call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "release",
		"principal": map[string]any{"user": "tester", "team": nil},
		"session":   "S-ghost", "agent_id": "agent-ghost",
	})
	if rel.OK != nil && *rel.OK {
		t.Errorf("release of an agent from the previous daemon reported success: %+v", rel)
	}
}
