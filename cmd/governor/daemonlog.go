package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/lease"
)

// The daemon log (GOV-B29).
//
// GOV-11 made this log the record of when the budget BOUND (peaks, parks, denials). It did
// not record when the budget MOVED, or when a slot came back. So when dogfooding (GOV-38)
// showed the budget at 1 on four evenings, nothing could say what cut it, when, or when it
// came back. Many of its other lines also carried no timestamp, so the `broken pipe` lines
// in ledger #2 could not be matched against anything. Three rules close that:
//
//   - EVERY LINE IS TIMESTAMPED, BY THE WRITER RATHER THAN BY EACH CALL SITE. stampWriter
//     prefixes each line with RFC 3339. A new Fprintf to d.diag cannot forget it, which is how
//     the untimestamped lines came about.
//   - A BUDGET LINE IS A TRANSITION, NEVER A TICK. The controller steps at 1 Hz, so a line per
//     tick would be 86,400 lines a day. budgetLine is written only when the number changes,
//     and it names the branch that changed it (budget.Controller.Moved) and the reading
//     against the threshold that branch tested.
//   - A RELEASE LINE NAMES WHAT RELEASED THE LEASE. Every path that drops a lease returns it
//     (lease.Released), and the daemon logs it after the reply has gone. That gives
//     slot-seconds by class, which GOV-B33 needs.
//
// VOLUME AND BOUND. On this machine the log reached 365 lines (33 KB) between 2026-11-02 and
// 2026-11-13, so about 3 KB a day. The new lines add one release per lease, which is about
// 75 builds plus a few dozen agents on a heavy day (Days 6 and 7 of docs/dogfooding.md), and
// a handful of transitions per cut episode: one cut, then either a climb per slot or one
// restore. That is roughly 150-300 lines, or 15-30 KB, a day. The worst case has no such
// limit. A run queue sitting exactly on the cut level could move the budget every tick,
// which would be about 86,400 lines and 8 MB in a day. So the file is bounded the way
// GOV-B26 bounded samples.jsonl. At daemonLogMaxBytes it is renamed to daemon.log.1 and a
// fresh file begins. That keeps one generation, so the log never takes more than about
// 2 x daemonLogMaxBytes on disk.
//
// daemonLogMaxBytes is 4 MiB: over a year of ordinary days live, and half a day of the
// pathological case. A larger bound would only hold more of a flapping loop, and one
// generation of that is enough to see the flapping.
var daemonLogMaxBytes int64 = 4 << 20

// daemonLogRotatedSuffix names the one kept generation, as trace.RotatedSuffix does.
const daemonLogRotatedSuffix = ".1"

// stampWriter prefixes every line written through it with an RFC 3339 timestamp.
//
// It tracks whether the last write ended mid-line, so a line assembled from several writes
// is stamped once, at its start. It is not safe for concurrent use on its own; the daemon
// always wraps it in a syncWriter, and each Write goes to the sink as one call so two
// stamped lines cannot interleave inside a line.
type stampWriter struct {
	w   io.Writer
	mid bool
	now func() time.Time
}

func (s *stampWriter) Write(p []byte) (int, error) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	stamp := now().Format(time.RFC3339) + " "
	var out bytes.Buffer
	rest := p
	for len(rest) > 0 {
		if !s.mid {
			out.WriteString(stamp)
		}
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			out.Write(rest)
			s.mid = true
			break
		}
		out.Write(rest[:i+1])
		s.mid = false
		rest = rest[i+1:]
	}
	if _, err := s.w.Write(out.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

// newDaemonLog is the writer every daemon line goes through: timestamped, then serialised.
// A writer that is already one is returned as it is, so runDaemon can build it once, write
// its own start-up lines through it, and hand the same one to newDaemon without a second
// stamp on every line.
func newDaemonLog(w io.Writer) *syncWriter {
	if s, ok := w.(*syncWriter); ok {
		if _, stamped := s.w.(*stampWriter); stamped {
			return s
		}
	}
	return &syncWriter{w: &stampWriter{w: w}}
}

// rotatingLog is the daemon's own file when that file is the state directory's daemon.log.
//
// The daemon does not open the log. `governor install` and a lazy start open it and pass it
// as the daemon's stdout and stderr. So rotation cannot just open a new file and switch to
// it. File descriptors 1 and 2 would still point at the renamed generation, and so would
// anything the runtime writes to them, such as a panic trace. Instead the new file is
// dup2'd over both descriptors. Every writer in the process then follows it, including
// ones that never go through this type.
//
// IT RUNS AFTER THE WRITE and costs one fstat in the common case, as rotateIfFull does in
// internal/trace. It runs inside the syncWriter's lock, so the daemon's own writers cannot
// race it. A second daemon that lost the bind race (design.md section 4.5) may still write
// a line or two to the old file before it exits, and that is harmless. Every failure is
// silent and leaves the log as it was. A missed rotation is retried on the next write, and
// a log that ends up a little over the bound costs nothing.
type rotatingLog struct {
	f    *os.File // the descriptor written to: the daemon's stderr
	fds  []int    // every descriptor that names the log, all redirected on rotation
	path string
	max  int64
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	n, err := r.f.Write(p)
	if err == nil {
		r.rotateIfFull()
	}
	return n, err
}

func (r *rotatingLog) rotateIfFull() {
	fi, err := r.f.Stat()
	if err != nil || fi.Size() < r.max {
		return
	}
	// Only if the path still names the file being written. Anything else means someone
	// moved it, and renaming whatever is there now would discard a file this did not write.
	cur, err := os.Stat(r.path)
	if err != nil || !os.SameFile(fi, cur) {
		return
	}
	if err := os.Rename(r.path, r.path+daemonLogRotatedSuffix); err != nil {
		return
	}
	nf, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer nf.Close()
	for _, fd := range r.fds {
		_ = unix.Dup2(int(nf.Fd()), fd)
	}
}

// daemonLogSink returns what the daemon should write its log to.
//
// When stderr is the state directory's daemon.log, which it is whenever install or a lazy
// start launched this daemon, the answer is a rotatingLog over it. Otherwise stderr is
// returned unchanged. That covers a terminal, a test's buffer, and tools/sweep's
// per-arm file, which is written somewhere else and removed when the arm ends.
func daemonLogSink(stdout, stderr io.Writer, stateDir string) io.Writer {
	f, ok := stderr.(*os.File)
	if !ok || stateDir == "" {
		return stderr
	}
	path := filepath.Join(stateDir, daemonLogFile)
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return stderr
	}
	cur, err := os.Stat(path)
	if err != nil || !os.SameFile(fi, cur) {
		return stderr
	}
	r := &rotatingLog{f: f, fds: []int{int(f.Fd())}, path: path, max: daemonLogMaxBytes}
	if o, ok := stdout.(*os.File); ok && o.Fd() != f.Fd() && sameOpenFile(o, f) {
		r.fds = append(r.fds, int(o.Fd()))
	}
	return r
}

// budgetLine describes one budget transition, or returns "" when there was none.
//
// The verb is how the budget moved, and cause= is the branch of the controller that moved
// it. Each cause shows the reading next to the threshold it was tested against. A line that
// says only "cut" cannot settle whether it was the run queue or Chrome, because the reader
// still has to guess at the load.
//
//	cut 4->2 cause=run-queue rq=7.10 cut=6.59 ceiling=4
//	cut 4->1 cause=thermal thermal=serious ceiling=4
//	climb 2->3 cause=climb rq=3.20 climb=5.59 ceiling=4
//	restore 1->4 cause=idle rq=0.40 climb=5.59 ceiling=4
//	ceiling 4->6 cause=ceiling ceiling=4->6
func budgetLine(from, to int, cause string, c *budget.Controller, rq float64, thermal string, ceilingFrom int) string {
	if from == to {
		return ""
	}
	switch cause {
	case budget.MovedThermal:
		return fmt.Sprintf("cut %d->%d cause=thermal thermal=%s ceiling=%d", from, to, thermal, c.Max)
	case budget.MovedRunQueue:
		return fmt.Sprintf("cut %d->%d cause=run-queue rq=%.2f cut=%.2f ceiling=%d", from, to, rq, c.CutAt(), c.Max)
	case budget.MovedClimb:
		return fmt.Sprintf("climb %d->%d cause=climb rq=%.2f climb=%.2f ceiling=%d", from, to, rq, c.ClimbAt(), c.Max)
	case budget.MovedIdle:
		return fmt.Sprintf("restore %d->%d cause=idle rq=%.2f climb=%.2f ceiling=%d", from, to, rq, c.ClimbAt(), c.Max)
	case budget.MovedCeiling:
		return fmt.Sprintf("ceiling %d->%d cause=ceiling ceiling=%d->%d", from, to, ceilingFrom, c.Max)
	default:
		// The controller moved the budget and named no cause. That should not happen, and the
		// line is still written, because a missing transition is the defect this log exists
		// to fix.
		return fmt.Sprintf("move %d->%d cause=unknown rq=%.2f ceiling=%d", from, to, rq, c.Max)
	}
}

// The names a release line gives for what released a lease. The reaper's are refined by
// lease.Released.Why.
const (
	releasedByPostToolUse = "PostToolUse"
	releasedBySubagentEnd = "SubagentStop"
	releasedBySessionEnd  = "SessionEnd"
	releasedByReaper      = "reaper"
	releasedByHandback    = "handback"
)

// releaseLine describes one released lease.
//
//	release session=… kind=build class=heavy weight=2.00 held=1m23.456s by=PostToolUse
//
// kind and class are the dimensions slot-seconds are summed over (GOV-B33). weight is the
// lease's share of the budget, so weight x held is the slot-seconds it consumed. A lease
// whose gate sent no class (every agent lease, and a build from an older gate) logs
// class=-, so the field is always present and never shifts the ones after it.
func releaseLine(r lease.Released, by string) string {
	class := r.Class
	if class == "" {
		class = "-"
	}
	if r.Why != "" {
		by += ":" + r.Why
	}
	return fmt.Sprintf("release session=%s kind=%s class=%s weight=%.2f held=%s by=%s",
		r.Session, r.Kind, class, r.Weight, r.Held().Round(time.Millisecond), by)
}

// finishedBy names the hook that finished a build, from what the hook sent.
//
// It is a closed vocabulary, not an echo. The value comes off the wire, and the log is pasted
// into public issues by `doctor --report`. Anything that is not one of the two events
// `finish` is registered on reads as PostToolUse, which is what an empty value means: a hook
// from before GOV-B29 that does not send it.
func finishedBy(event string) string {
	if event == "PostToolUseFailure" {
		return event
	}
	return releasedByPostToolUse
}

// sameOpenFile reports whether two writers are open on one file. Anything that is not an
// *os.File, or cannot be stat'd, is not the same file as anything.
func sameOpenFile(a, b io.Writer) bool {
	fa, ok := a.(*os.File)
	if !ok {
		return false
	}
	fb, ok := b.(*os.File)
	if !ok {
		return false
	}
	ia, err := fa.Stat()
	if err != nil {
		return false
	}
	ib, err := fb.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(ia, ib)
}
