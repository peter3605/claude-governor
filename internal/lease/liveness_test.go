package lease

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// PIDLiveness against processes that really die (GOV-09).
//
// Everything GOV-06 shipped exercised the reaper through a stub Liveness and a hand-wound
// clock, so the *interface* was proven and the *implementation* never was: no test had ever
// run PIDLiveness against a process that actually exited. That gap matters more than it
// looks. PIDLiveness is the only thing standing between a crashed session and a budget that
// leaks a slot per agent until the confirmed TTL expires half an hour later, and it is
// consulted nowhere else in the codebase — so a bug in it is invisible until the fleet has
// quietly deadlocked to zero.
//
// These tests spawn real children, read their real start times, and kill them.

// startTimeOf reads a live process's start time the way the gate does, then truncates it to
// whole seconds the way the wire does.
//
// The truncation is not incidental. `confirm` carries pid_started_at as unix SECONDS
// (design.md section 7) and the daemon rebuilds it with time.Unix(sec, 0), so the value
// PIDLiveness compares against has already lost its sub-second part. A tolerance that only
// covered Linux's btime granularity and not this rounding would reap live agents on macOS
// too, and no existing test would have noticed.
func startTimeOf(t *testing.T, pid int) time.Time {
	t.Helper()
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		t.Fatalf("process.NewProcess(%d): %v", pid, err)
	}
	ms, err := p.CreateTimeWithContext(context.Background())
	if err != nil {
		t.Fatalf("CreateTime(%d): %v", pid, err)
	}
	return time.Unix(time.UnixMilli(ms).Unix(), 0)
}

// spawnChild starts a real, long-lived child and returns it with its wire-shaped start time.
//
// The returned kill function does NOT just signal: it signals and then waits. A killed child
// of this test process stays in the process table as a zombie until its parent reaps it, and
// a zombie is still findable — gopsutil returns it happily and its start time still matches,
// so PIDLiveness would correctly report it alive. Killing without waiting would therefore
// produce a test that fails for a reason that has nothing to do with the code under test.
//
// It is worth being precise about why that is a test artefact and not a production bug: the
// daemon is never the parent of a Claude Code session, so it never owes anyone a wait(). A
// session that dies is reaped by its own shell, or by init once that shell is gone. The one
// case where a session could linger as a zombie — a parent that outlives it without waiting —
// is exactly what the confirmed TTL is a backstop for, since liveness cannot see it.
func spawnChild(t *testing.T) (pid int, started time.Time, kill func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no sleep(1)")
	}
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid = cmd.Process.Pid
	started = startTimeOf(t, pid)

	killed := false
	kill = func() {
		if killed {
			return
		}
		killed = true
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait() // collect the zombie; see above
	}
	t.Cleanup(kill)
	return pid, started, kill
}

// The central claim, and the one GOV-06 could not make: a process that really dies is really
// reported dead.
func TestPIDLivenessSeesARealProcessDie(t *testing.T) {
	pid, started, kill := spawnChild(t)
	l := Lease{ID: "L-1", State: Confirmed, OwnerPID: pid, OwnerStarted: started}

	if !(PIDLiveness{}).Alive(l) {
		t.Fatalf("Alive=false for a running pid %d started %v; the owner is alive and its "+
			"lease must not be reaped", pid, started)
	}

	kill()

	if (PIDLiveness{}).Alive(l) {
		t.Errorf("Alive=true for pid %d after SIGKILL; a dead owner's lease would be held "+
			"until its TTL and the budget would leak that slot", pid)
	}
}

// The start time has to be doing real work, not decorating a PID check.
//
// A PIDLiveness that only asked "does this PID exist?" would pass every other test in this
// file. This is the one that fails it. A recycled PID is the production shape: a session
// dies, the kernel hands its number to something unrelated, and a lease that only remembered
// the number stays alive forever pointing at a stranger.
func TestPIDLivenessRejectsARecycledPID(t *testing.T) {
	pid, started, _ := spawnChild(t)

	// The same live PID, remembered with the start time of a process that held that number
	// long ago. This is precisely what PID reuse looks like from the table's side.
	stale := Lease{
		ID: "L-2", State: Confirmed,
		OwnerPID:     pid,
		OwnerStarted: started.Add(-1 * time.Hour),
	}
	if (PIDLiveness{}).Alive(stale) {
		t.Errorf("Alive=true for pid %d remembered with a start time an hour earlier; "+
			"PIDLiveness is checking the PID and ignoring the start time, so a recycled "+
			"PID would keep a dead agent's lease alive indefinitely", pid)
	}

	// And the control: the same PID with its own start time is alive. Without this the test
	// above would also pass against an implementation that reported everything dead.
	fresh := Lease{ID: "L-3", State: Confirmed, OwnerPID: pid, OwnerStarted: started}
	if !(PIDLiveness{}).Alive(fresh) {
		t.Errorf("Alive=false for pid %d with its own start time; the discrimination above "+
			"is not evidence unless this passes", pid)
	}
}

// The tolerance exists for a measured reason, and it has to be wide enough for the wire.
//
// Linux derives a process's start time from /proc/stat's btime at one-second granularity,
// and `confirm` sends it as whole seconds anyway. Both roundings push in the same direction,
// so a lease can legitimately remember its owner as having started up to a second before or
// after gopsutil's current answer. Exact comparison would reap live agents on every Linux
// host — the same one-second granularity that cost GOV-02 its CPU attribution.
func TestPIDLivenessToleratesTheStartTimeGranularity(t *testing.T) {
	pid, started, _ := spawnChild(t)

	for _, skew := range []time.Duration{-time.Second, -500 * time.Millisecond, 0, time.Second} {
		l := Lease{ID: "L-4", State: Confirmed, OwnerPID: pid, OwnerStarted: started.Add(skew)}
		if !(PIDLiveness{}).Alive(l) {
			t.Errorf("Alive=false for a live pid whose remembered start time is off by %v; "+
				"that is inside the rounding the wire and /proc/stat already impose, so "+
				"this reaps live agents", skew)
		}
	}
}

// Every uncertain answer is "alive". This is the Liveness contract, and it is a deliberate
// asymmetry rather than caution: reaping a live agent's lease double-counts the budget and
// over-subscribes the machine, while keeping a dead one costs a single slot until its TTL.
func TestPIDLivenessReportsAliveWhenItCannotTell(t *testing.T) {
	pid, started, _ := spawnChild(t)

	cases := []struct {
		name  string
		lease Lease
	}{
		{"no owner was ever bound", Lease{OwnerPID: 0, OwnerStarted: started}},
		{"a negative pid", Lease{OwnerPID: -1, OwnerStarted: started}},
		{"the pid exists but nothing to check it against", Lease{OwnerPID: pid}},
	}
	for _, c := range cases {
		c.lease.State = Confirmed
		if !(PIDLiveness{}).Alive(c.lease) {
			t.Errorf("%s: Alive=false, want true — an unknown owner must be left to its "+
				"TTL, never reaped on a guess", c.name)
		}
	}
}

// The reaper's own use of the real implementation, rather than a stub: a confirmed lease
// whose owner is killed is collected, and one whose owner is alive is not.
//
// This is the join the other tests do not make. Reap only consults Liveness for confirmed
// leases, so a table wired to PIDLiveness could still fail to collect anything if that
// branch were wrong, and every test above would still pass.
func TestReaperCollectsARealDeadOwnerAndSparesALiveOne(t *testing.T) {
	deadPID, deadStarted, killDead := spawnChild(t)
	livePID, liveStarted, _ := spawnChild(t)

	tbl := NewTable(Options{Liveness: PIDLiveness{}})
	for _, o := range []struct {
		session, agent string
		pid            int
		started        time.Time
	}{
		{"doomed", "a-dead", deadPID, deadStarted},
		{"survivor", "a-live", livePID, liveStarted},
	} {
		if _, ok := tbl.Acquire(o.session, 0, 10); !ok {
			t.Fatalf("Acquire(%s): no room in a budget of 10", o.session)
		}
		if _, ok := tbl.Confirm(o.session, "", o.agent, o.pid, o.started); !ok {
			t.Fatalf("Confirm(%s): the lease just acquired was not pending", o.session)
		}
	}
	if got := tbl.InUse(); got != 2 {
		t.Fatalf("InUse=%d before the kill, want 2", got)
	}

	// Nothing is dead yet, so nothing may be collected. A reaper that dropped a lease here
	// would be freeing a slot out from under a running agent.
	if n := tbl.Reap(); n != 0 {
		t.Fatalf("Reap dropped %d lease(s) while both owners were alive, want 0", n)
	}

	killDead()

	if n := tbl.Reap(); n != 1 {
		t.Fatalf("Reap dropped %d lease(s) after one owner was killed, want exactly 1", n)
	}
	if got := tbl.InUse(); got != 1 {
		t.Fatalf("InUse=%d after the reap, want 1 — the surviving agent keeps its slot", got)
	}
	snap := tbl.Snapshot()
	for _, s := range snap.Sessions {
		if s.ID == "doomed" && s.Confirmed != 0 {
			t.Errorf("the killed session still holds %d confirmed lease(s)", s.Confirmed)
		}
		if s.ID == "survivor" && s.Confirmed != 1 {
			t.Errorf("the live session holds %d confirmed lease(s), want 1", s.Confirmed)
		}
	}
}
