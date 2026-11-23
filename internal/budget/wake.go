package budget

import (
	"math"
	"time"
)

// Waking up (GOV-B48, design.md section 5.6d).
//
// THE KERNEL'S LOAD AVERAGE STOPS WHILE THE MACHINE SLEEPS AND RESUMES FROM WHERE IT STOPPED.
// Measured on macOS 26.4.1 over the 130-odd sleeps of 2026-11-22: in every one, the first
// tick after the gap reads exactly the pre-sleep loadavg1 (47.85 -> 47.85 across 909 s,
// 186.68 -> 186.68 across 773 s), and the next kernel update applies ONE five-second decay,
// not the gap's worth (118.99 -> 109.46 across 319 s, which is 118.99 x e^(-5/60) = 109.47 to
// two decimals). So a machine that went to sleep mid-build wakes with a run queue that says
// it is still mid-build, and the 1-minute average takes minutes to forget it.
//
// That memory is not noise: it is EXACTLY known. loadavg is an exponential average updated
// every LoadavgPeriod with factor e^(-period/LoadavgTau), so k updates after waking it reads
//
//	la_k = la_0 * m_k + (1 - m_k) * n̄_k        where m_k = e^(-k*period/tau)
//
// with la_0 the frozen pre-sleep value and n̄_k the weighted mean of the runnable count SINCE
// WAKE. Solving for n̄_k removes the pre-sleep memory and nothing else, so a wake storm — and
// the measured full wake of 2026-11-22 20:31:34 had a real one, run queue 1.51 to 7.77 per
// core in fifteen seconds — still reads as the load it is. That is the property the story's
// third clause asks for: the stale reading must not refuse a build, and a machine that is
// genuinely busy at wake must not be oversubscribed because the governor chose to look away.

// SleepGap is how far wall-clock time must run ahead of monotonic time between two ticks for
// the daemon to say the machine slept.
//
// Go's monotonic clock is mach_absolute_time on darwin and CLOCK_MONOTONIC on Linux, and
// neither advances during sleep or suspend (checked in the go1.26 runtime source, design.md
// section 5.6d), so across a twelve-hour sleep the monotonic gap is about one tick and the
// wall gap is twelve hours. Comparing a wall gap to sampleInterval alone would also fire on an
// NTP step of the wall clock; the difference between the two clocks fires only on time the
// process did not experience.
//
// 30 s is far above a tick's jitter and far below the shortest sleep in the 2026-11-22 trace
// worth naming. Shorter gaps were all DarkWake hand-offs measured in single seconds.
const SleepGap = 30 * time.Second

// LoadavgPeriod and LoadavgTau are the kernel's load-average update interval and 1-minute
// time constant. Both are the same on darwin and Linux, and the 2026-11-22 trace shows the
// five-second step directly: loadavg1 changes on every fifth 1 Hz sample and holds between.
const (
	LoadavgPeriod = 5 * time.Second
	LoadavgTau    = 60 * time.Second
)

// wakeHoldTicks bounds how long the filter waits for the first kernel update after a wake.
// An update comes within LoadavgPeriod; seven ticks allows for a late first tick. If loadavg
// has still not changed by then, the runnable count has equalled the frozen average all
// along (the only way an update leaves it unchanged), so the reading was never stale.
const wakeHoldTicks = 7

// wakeMemoryFloor is the share of the run queue still owed to the pre-sleep average at which
// the filter stops correcting and hands raw readings back. 0.1 run-queue-per-core is a fifth
// of RunQueueBand: below it the correction cannot move a reading across either threshold.
const wakeMemoryFloor = 0.1

// WakeFilter turns the raw run queue into the one the loop should act on, across sleeps.
//
// It is a plain struct the daemon owns, not a seam: there is one way a kernel load average
// behaves across sleep on a given platform and nothing in tier two will replace it.
type WakeFilter struct {
	// Frozen is whether this platform's load average freezes across sleep and resumes from
	// the frozen value. signals.LoadavgFrozenAcrossSleep supplies it: measured true on
	// darwin; not yet measured on Linux, where the filter only detects and never corrects.
	Frozen bool

	last    float64 // the raw run queue on the previous tick
	waking  bool    // a sleep was detected and the pre-sleep memory has not yet decayed
	frozen  float64 // the run queue the kernel froze at sleep
	updates int     // kernel load-average updates seen since the wake
	held    int     // ticks since the wake that saw no update yet
	primed  bool    // last holds a real reading
	before  float64 // the raw run queue on the last tick before the most recent sleep
}

// Away is how long the machine was asleep between two ticks, or zero when it was not.
// wall and mono are the same interval measured on the wall clock and the monotonic clock.
func Away(wall, mono time.Duration) time.Duration {
	if d := wall - mono; d > SleepGap {
		return d
	}
	return 0
}

// Observe takes this tick's raw run queue and how long the machine was away before it (zero
// when it was not), and returns the run queue to act on. ok is false while the reading is
// still the frozen pre-sleep value and carries no information about the machine as it is
// now; the loop holds on such a tick.
func (f *WakeFilter) Observe(rq float64, away time.Duration) (out float64, ok bool) {
	prev, primed := f.last, f.primed
	f.last, f.primed = rq, true
	if away > 0 {
		f.before = prev
	}
	// A pre-sleep run queue under the floor is not worth a hold: it cannot move a reading
	// across either threshold, so there is nothing stale to wait out.
	if away > 0 && primed && f.Frozen && prev >= wakeMemoryFloor {
		f.waking, f.frozen, f.updates, f.held = true, prev, 0, 0
	}
	if !f.waking {
		return rq, true
	}
	if rq != prev {
		f.updates++
	}
	if f.updates == 0 {
		f.held++
		if f.held < wakeHoldTicks {
			return 0, false
		}
		f.updates = 1
	}
	m := math.Exp(-float64(f.updates) * LoadavgPeriod.Seconds() / LoadavgTau.Seconds())
	memory := f.frozen * m
	if memory < wakeMemoryFloor {
		f.waking = false
		return rq, true
	}
	fresh := (rq - memory) / (1 - m)
	if fresh < 0 {
		fresh = 0
	}
	return fresh, true
}

// Before is the raw run queue on the last tick before the most recent sleep: the value the
// kernel froze.
func (f *WakeFilter) Before() float64 { return f.before }

// Waking reports whether the filter is still correcting for a sleep.
func (f *WakeFilter) Waking() bool { return f.waking }
