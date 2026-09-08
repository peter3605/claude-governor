package lease

import (
	"context"
	"math"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// startTimeTolerance is how far two readings of the same process's start time may differ
// and still be considered the same process.
//
// It is not slack for its own sake. Linux derives a process's start time from /proc/stat's
// btime at one-second granularity, so the same process can read a second apart depending on
// when it is asked — a fact this repo learned the expensive way in GOV-02, where CPU
// attribution keyed on exact CreateTime scored every short-lived child as zero. Two seconds
// covers the rounding on both platforms. The cost of being generous is that a PID recycled
// within two seconds onto an identical start time is mistaken for its predecessor; the cost
// of being exact is reaping live agents on every Linux host, which is far worse.
const startTimeTolerance = 2 * time.Second

// PIDLiveness answers whether a lease's owner is still running, from the owning PID and its
// start time.
//
// This is the v1 implementation of the Liveness seam (design.md section 12.1). It is
// correct and free on one machine, where the daemon and the process it is asking about
// share a kernel. A tier-two broker cannot see another host's processes and would answer
// from a heartbeat instead, behind this same interface — which is the entire reason the
// interface exists rather than a direct call to process.NewProcess from the reaper.
//
// The start time is what makes it safe: PIDs are recycled, and a reaper that only checked
// existence would keep a dead agent's lease alive for as long as some unrelated process
// happened to hold its number.
type PIDLiveness struct{}

// Alive reports whether the lease's owner is still running.
//
// Every uncertain answer is "alive", per the Liveness contract: reaping a live agent's lease
// double-counts the budget and over-subscribes the machine, while keeping a dead one costs a
// single slot until its TTL expires. Those two mistakes are not the same size, so the
// ambiguity resolves in one direction only.
func (PIDLiveness) Alive(l Lease) bool {
	if l.OwnerPID <= 0 {
		return true // nothing was ever bound; the TTL is what collects this lease
	}
	p, err := process.NewProcess(int32(l.OwnerPID))
	if err != nil {
		// gopsutil reports a definite "no such process" here, which is the one unambiguous
		// dead signal available.
		return false
	}
	if l.OwnerStarted.IsZero() {
		return true // the PID exists and there is nothing to check it against
	}
	ms, err := p.CreateTimeWithContext(context.Background())
	if err != nil {
		return true
	}
	started := time.UnixMilli(ms)
	return math.Abs(started.Sub(l.OwnerStarted).Seconds()) <= startTimeTolerance.Seconds()
}
