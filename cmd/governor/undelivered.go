package main

import (
	"fmt"
	"time"
)

// Replies the client never received (GOV-B49).
//
// Before this, a reply the daemon could not write was one line, "write reply: ... broken
// pipe", with no op and no client. GOV-38 counted 156 of them in a week and could explain 28
// (sleep/wake) without being able to say who the rest were for. Two kinds of hang-up are
// expected, and neither is a fault:
//
//   - THE CLIENT HAD ALREADY PASSED ITS DEADLINE. Every client has a hard budget (the gate
//     and the hooks 100 ms, the status line 50 ms) and drops the connection when it runs
//     out. The gate has failed open by then, and the status line has rendered from its cache.
//     The request's deadline_ms (encodeRequest) says whether that is what happened.
//   - THE MACHINE SLEPT. A reply that is cut off across a sleep/wake transition is the
//     machine stopping, not the daemon. It shows as the wall clock running ahead of the
//     monotonic clock across the exchange (the test budget.Away applies to ticks, with a smaller gap), or as a
//     hang-up just after the sampler saw a wake.
//
// Those two are logged as ordinary lines ("reply undelivered ... cause=deadline|sleep").
// Anything else keeps the "write reply:" error line, now with the same fields, because an
// unexplained hang-up is exactly what that line exists to show (the story's trap: do not
// silence it everywhere).

// replySleepGap is how far the wall clock may run ahead of the monotonic clock across one
// exchange before the exchange is taken to have spanned a sleep. budget.SleepGap is 30 s
// because a tick is a second apart and must not mistake a clock adjustment for a sleep. An
// exchange lasts at most a few seconds, and a short dark wake still cuts it off, so a much
// smaller gap is used here. NTP slews the clock rather than stepping it, which a gap of a
// whole second will not see.
const replySleepGap = time.Second

// replyWakeWindow is how soon after a wake the sampler noticed a hang-up is still blamed on
// the wake. The sampler ticks once a second, so it sees a wake up to a second late, and
// GOV-38 found the hang-ups clustered within 2 s of the transitions in pmset -g log.
const replyWakeWindow = 2 * time.Second

// exchangeInfo is what the log needs to say about one request/reply exchange.
type exchangeInfo struct {
	op     string
	client string
	part   string    // "only", or "interim"/"final" for a parked acquire
	start  time.Time // when the daemon accepted the connection
	// givesUp is when the client drops the connection, zero when it is not known (an older
	// client sends no deadline_ms).
	givesUp time.Time
}

// knownOps is every op the daemon answers. Anything else is logged as "other", so a client
// cannot write arbitrary text into the log through the op field.
var knownOps = map[string]bool{
	"acquire": true, "confirm": true, "release": true, "finish": true, "background": true,
	"settle": true, "session": true, "status": true, "advise": true, "control": true,
}

// exchangeOf describes the exchange for req, accepted at start.
func exchangeOf(req request, start time.Time) exchangeInfo {
	ex := exchangeInfo{op: "other", part: "only", start: start}
	if knownOps[req.Op] {
		ex.op = req.Op
	}
	ex.client = clientOf(ex.op, req.Client)
	if req.DeadlineMS > 0 {
		ex.givesUp = time.UnixMilli(req.DeadlineMS)
	}
	return ex
}

// clientOf names the sender. Every op but status has exactly one sender. Status has three,
// which name themselves; a status with no recognised name is from an older binary.
func clientOf(op, named string) string {
	switch op {
	case "acquire":
		return "gate"
	case "confirm", "release", "finish", "background", "settle", "session":
		return "hook"
	case "advise":
		return "mcp"
	case "control":
		return "control"
	case "status":
		switch named {
		case "statusline", "doctor", "install":
			return named
		}
	}
	return "-"
}

// hangupCause classifies a reply that could not be written. took and wall are the same
// interval, from accept to the failed write, measured on the monotonic and the wall clock;
// lastWake is when the sampler last saw the machine wake, zero if never.
func hangupCause(took, wall time.Duration, now, givesUp, lastWake time.Time) string {
	if wall-took > replySleepGap {
		return "sleep"
	}
	if !lastWake.IsZero() {
		if since := now.Sub(lastWake); since >= 0 && since <= replyWakeWindow {
			return "sleep"
		}
	}
	if !givesUp.IsZero() && !now.Before(givesUp) {
		return "deadline"
	}
	return "unexplained"
}

// undeliveredFields is the part of the line both forms share.
func undeliveredFields(ex exchangeInfo, took time.Duration, now time.Time, cause string) string {
	left := "deadline=unknown"
	if !ex.givesUp.IsZero() {
		left = fmt.Sprintf("left=%s", fmtMS(ex.givesUp.Sub(now)))
	}
	return fmt.Sprintf("op=%s client=%s reply=%s took=%s %s cause=%s",
		ex.op, ex.client, ex.part, fmtMS(took), left, cause)
}

// fmtMS renders a duration in milliseconds to a tenth, signed when negative: "63.2ms",
// "-12.0ms". Fixed units make a week of lines sortable with nothing but awk.
func fmtMS(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// undelivered logs a reply the client did not receive.
func (d *daemon) undelivered(ex exchangeInfo, err error, now time.Time) {
	took := now.Sub(ex.start)
	wall := now.Round(0).Sub(ex.start.Round(0))
	var lastWake time.Time
	if ns := d.lastWake.Load(); ns != 0 {
		lastWake = time.Unix(0, ns)
	}
	cause := hangupCause(took, wall, now, ex.givesUp, lastWake)
	fields := undeliveredFields(ex, took, now, cause)
	if cause == "unexplained" {
		fmt.Fprintf(d.diag, "governor daemon: write reply: %v %s\n", err, fields)
		return
	}
	fmt.Fprintf(d.diag, "governor daemon: reply undelivered %s\n", fields)
}
