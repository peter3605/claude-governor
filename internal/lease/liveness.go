package lease

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
//
// A BACKGROUNDED BUILD'S HOLDER IS ITS TASK, not a process this daemon has a PID for (GOV-B41).
// Claude Code writes each background task's output to
// /tmp/claude-<uid>/<cwd slug>/<session>/tasks/<task>.output and appends a status line,
// "[exited with code N]" or "[killed]", when the task ends. That was measured on 8 of 8 probe
// tasks and 96 of 99 task files on disk, and the 3 without the line were still held open by
// a running process (hook-surface.md section 26). A subagent's task that ends while the
// subagent works on raises no hook at all, so for that case this file is the ONLY end signal.
// Every way of not finding it, or not recognising it, answers "alive", so a Claude Code that
// moves the file costs the TTL and nothing more.
type PIDLiveness struct {
	// taskRoot overrides /tmp/claude-<uid>, for tests.
	taskRoot string
}

// Alive reports whether the lease's owner is still running.
//
// Every uncertain answer is "alive", per the Liveness contract: reaping a live agent's lease
// double-counts the budget and over-subscribes the machine, while keeping a dead one costs a
// single slot until its TTL expires. Those two mistakes are not the same size, so the
// ambiguity resolves in one direction only.
func (v PIDLiveness) Alive(l Lease) bool {
	if l.Kind == Build && l.BackgroundTask != "" {
		return v.taskAlive(l.Session, l.BackgroundTask)
	}
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

// taskEnded matches the status line Claude Code appends to a background task's output file when
// the task ends, and nothing else: it must be the file's LAST line. Measured forms are
// "\n[exited with code 0]\n", "\n[exited with code 3]\n" and "\n[killed]\n".
var taskEnded = regexp.MustCompile(`(?:^|\n)\[(?:exited with code -?[0-9]+|killed)\]\n?$`)

// taskTail is how much of the end of an output file is read. The status line is at most 32
// bytes; the rest is margin.
const taskTail = 64

// pathSafe reports whether s can be one path element: no separators, no "..", nothing a
// pattern would expand. Session and task IDs reach this from the wire.
var pathSafe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// taskAlive reports whether background task task of session is still running, from its output
// file. Every uncertain answer is true, per the Liveness contract.
//
// The file is found by pattern rather than by rebuilding the cwd slug, because the slug is the
// session's STARTING directory, which no payload this daemon receives states reliably. The
// session ID and task ID together are unique, so exactly one match is required. Zero matches
// is a Claude Code that keeps the file elsewhere, CLAUDE_CODE_TMPDIR among the ways, and more
// than one is a layout nothing measured.
func (v PIDLiveness) taskAlive(session, task string) bool {
	if !pathSafe.MatchString(session) || !pathSafe.MatchString(task) {
		return true
	}
	root := v.taskRoot
	if root == "" {
		root = filepath.Join("/tmp", "claude-"+strconv.Itoa(os.Getuid()))
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", session, "tasks", task+".output"))
	if err != nil || len(matches) != 1 {
		return true
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return true
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return true
	}
	off := fi.Size() - taskTail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return true
	}
	return !taskEnded.Match(buf)
}
