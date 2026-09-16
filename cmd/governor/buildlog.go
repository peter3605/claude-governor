package main

import (
	"fmt"
	"io"
	"time"

	"github.com/peter3605/claude-governor/internal/trace"
)

// The outcomes a gated build can have. The set is closed and the names are written to disk,
// so they are constants rather than string literals scattered through the gate: a window is
// scored by grouping on this field weeks after it was written, and a typo'd outcome is a
// silently miscounted window rather than an error anybody sees.
const (
	// outcomeGranted — a lease was issued immediately. The build ran.
	outcomeGranted = "granted"
	// outcomeParked — the daemon held the request and then granted it. The build ran late.
	outcomeParked = "parked"
	// outcomeDenied — the gate emitted a deny. The build did NOT run.
	outcomeDenied = "denied"
	// outcomeFailedOpen — no usable answer from the daemon. The build ran, ungoverned.
	outcomeFailedOpen = "failed-open"
)

// buildRecorder accumulates what happened to one gated build and writes a single trace record.
//
// WHY A RECORDER AND NOT A CALL AT EACH EXIT. gateOnce has nine return paths and every one of
// them is a real outcome — that is what failing open means. A write at each site is nine
// chances to add a tenth path and forget one, and the failure mode is not a crash: it is a
// window that under-counts builds, read months later as a finding about how sessions behave.
// So the outcome is a variable, the write is deferred once, and adding a return path cannot
// silently drop a record.
//
// WHY IT DEFAULTS TO FAILED-OPEN. The zero value has to be the outcome that means "the gate
// did not reach a decision", because that is precisely the state a path which forgot to set an
// outcome is in. Defaulting to granted would book an ungoverned build as a governed one.
type buildRecorder struct {
	rec      trace.Build
	outcome  string
	why      string
	waitedMS float64
}

// newBuildRecorder starts recording a gated build. It performs no I/O.
func newBuildRecorder(p hookPayload, in intent) *buildRecorder {
	return &buildRecorder{
		rec: trace.Build{
			SessionID: p.SessionID,
			Shapes:    in.shapes,
			Class:     in.class,
			Weight:    in.weight,
			CWD:       p.CWD,
		},
		outcome: outcomeFailedOpen,
		why:     "the gate reached no decision",
	}
}

// EVERY METHOD BELOW IS NIL-SAFE, AND THAT IS THE INTERFACE RATHER THAN AN ACCIDENT.
//
// gateOnce handles agent spawns and builds on one path, and only a build is recorded — so the
// recorder is nil for every agent spawn that comes through. The alternative to nil-safety is
// an `if rec != nil` around all six call sites in the gate, on the hot path, guarding an
// instrument. That is noise in the one function in this repo that most needs to be readable,
// and it is one forgotten check away from a nil dereference INSIDE the gate, which is a panic
// on a subagent spawn. withBudget would recover it and fail open, so it would not break the
// machine — it would do something worse, and silently stop governing while the instrument
// looked fine.
//
// Last call wins, which is the right rule: a parked request that is then granted is a park,
// and a parked request whose daemon then dies is a fail-open.

// grantedNow records a lease issued without a wait. The build ran.
func (b *buildRecorder) grantedNow() {
	if b == nil {
		return
	}
	b.outcome, b.why = outcomeGranted, ""
}

// parkedThenGranted records a build the daemon held and then admitted. The build ran late,
// and how late is the number that says whether parking is tolerable or merely survivable.
func (b *buildRecorder) parkedThenGranted(waited time.Duration) {
	if b == nil {
		return
	}
	b.outcome, b.why, b.waitedMS = outcomeParked, "", float64(waited)/float64(time.Millisecond)
}

// denied records a refusal the gate actually emitted. The build did NOT run.
func (b *buildRecorder) denied(reason string) {
	if b == nil {
		return
	}
	b.outcome, b.why = outcomeDenied, reason
}

// failOpen records that no usable answer was reached. The build ran, ungoverned.
func (b *buildRecorder) failOpen(why string) {
	if b == nil {
		return
	}
	b.outcome, b.why = outcomeFailedOpen, why
}

// write emits the record. It is deferred by the caller so that it runs AFTER the decision has
// been written to stdout, which is the ordering the whole thing depends on.
//
// THE ORDER IS A FAIL-OPEN ARGUMENT, NOT A TIDINESS ONE. trace.Append opens a file, and an
// open can block indefinitely on a wedged or unreachable filesystem. withBudget would abandon
// this goroutine and the spawn would proceed, which is correct — but if the write ran BEFORE
// the deny was emitted, a slow disk would silently convert every refusal into a grant. The
// governor would stop governing and nothing would say so. Deferring it puts the instrument
// strictly downstream of enforcement: the worst a wedged disk can now do is lose a record.
//
// WHAT IT COSTS, measured rather than asserted: 29.5 us per record on this host
// (BenchmarkAppendBuild, darwin/arm64, 8 cores, 2000 iterations), which is 0.03% of the
// gate's 100 ms budget. It is paid only on the 18.4% of Bash calls that are gated, and those
// already pay a unix round trip, so the instrument is comfortably inside the noise of the
// thing it is instrumenting. Re-measure if this record grows fields.
//
// Losing a record is accepted and stated rather than defended against. If the budget expires
// while this is in flight the process exits and the line is never written, so a window's build
// count is a floor rather than an exact figure. That is the correct trade for an instrument
// on the hot path, and the reading rule in docs/advice.md says so.
func (b *buildRecorder) write(diag io.Writer) {
	if b == nil {
		return
	}
	b.rec.Outcome = b.outcome
	b.rec.Why = b.why
	b.rec.WaitedMS = b.waitedMS
	if err := trace.Append(trace.Record{Kind: trace.KindBuild, Build: &b.rec}); err != nil {
		// Diagnostic only. An instrument that could fail a spawn would be worse than no
		// instrument, and this is the line that makes sure it cannot.
		fmt.Fprintf(diag, "governor gate: record build: %v\n", err)
	}
}
