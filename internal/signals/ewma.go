package signals

import (
	"math"
	"time"
)

// DefaultCPUWindow is the EWMA time constant for CPU utilisation (design.md section 5.1,
// "gopsutil/cpu, 5 s EWMA").
//
// Five seconds is short enough that the loop reacts inside a couple of AIMD ticks and long
// enough to ride out the spikes that a single instantaneous read is full of. GOV-12's notes
// give the reason it exists at all: a control loop wired to a raw sample oscillates, and a
// loop that oscillates is very hard to debug after the fact.
const DefaultCPUWindow = 5 * time.Second

// EWMA is a time-aware exponentially weighted moving average.
//
// Time-aware rather than tick-counted, which matters here: the sampler ticks on a
// time.Ticker that a loaded machine will skip, so "the last five samples" and "the last five
// seconds" are different windows precisely when the signal matters most. Weighting by the
// interval actually observed keeps the window meaning five seconds whether the ticks arrived
// on time or not.
//
// Not safe for concurrent use. One EWMA, one sampling goroutine.
type EWMA struct {
	tau    time.Duration
	value  float64
	at     time.Time
	primed bool
}

// NewEWMA returns an EWMA with the given time constant.
func NewEWMA(tau time.Duration) *EWMA {
	return &EWMA{tau: tau}
}

// Update folds in one reading taken at now, and returns the new average.
//
// The first reading is adopted whole rather than averaged against a zero that was never
// measured. Starting from zero would make the daemon report an idle machine for the first
// several seconds of its life — the same class of false claim that statusReply.CPU's null
// exists to avoid, arriving through the back door.
func (e *EWMA) Update(v float64, now time.Time) float64 {
	if !e.primed {
		e.value, e.at, e.primed = v, now, true
		return e.value
	}
	dt := now.Sub(e.at)
	if dt <= 0 {
		// Two readings at the same instant, or a clock that went backwards. Neither is a
		// reason to discard the sample or to weight it by a negative interval.
		e.value = v
		return e.value
	}
	e.at = now
	if e.tau <= 0 {
		e.value = v
		return e.value
	}
	// alpha = 1 - e^(-dt/tau): the standard time-aware smoothing factor. A gap far longer
	// than tau drives alpha to 1, so a sampler that was starved for a minute adopts the new
	// reading outright instead of blending it with a minute-old one.
	alpha := 1 - math.Exp(-float64(dt)/float64(e.tau))
	e.value += alpha * (v - e.value)
	return e.value
}

// Value returns the current average, and whether any reading has been folded in yet.
//
// The bool is not decoration: an un-primed EWMA and one averaging a genuine zero both hold
// 0.0, and the daemon reports the first as null rather than claiming the machine is idle.
func (e *EWMA) Value() (float64, bool) { return e.value, e.primed }
