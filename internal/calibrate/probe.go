package calibrate

import (
	"context"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultProbeDuration is how long ONE phase of the probe runs, and DefaultProbeRepetitions
// is how many three-phase repetitions it takes.
//
// 250 ms x 3 phases x 5 repetitions is 3.75 seconds of load: the "bounded and polite — a few
// seconds, clearly announced" the story asks for, and short enough that an installer running
// it does not look like it has hung. One phase is long enough for the quantity being measured
// because THROUGHPUT IS INSTANTANEOUS: how many operations N workers complete per second is
// available the moment they are running, unlike a one-minute load average, which is the signal
// the probe-length trap is about. Those are different instruments and only one of them has a
// window.
//
// THE REPETITIONS ARE NOT REDUNDANCY, THEY ARE THE ESTIMATOR — see Probe's "why the maximum".
// A single three-phase reading was measured on this host and is not usable: twelve of them,
// taken as the machine settled from loadavg1 16.7 to 8.8, reported effective cores ranging
// 1.89 to 6.05, a 3.2x spread on a quantity that is a fixed property of the hardware, with
// four of the twelve failing the drift check outright. The maximum over five is stable: six
// trials of it, over background load from 7.9 to 12.1, gave 5.70, 5.71, 6.31, 5.69, 6.53 and
// 6.81. That is what makes five the count rather than one.
const (
	DefaultProbeDuration    = 250 * time.Millisecond
	DefaultProbeRepetitions = 5
)

// ProbeMinUsableReps is how many repetitions must survive the drift check for the probe to
// report a result.
//
// Two rather than one, because the estimator is a maximum and a maximum over a single sample
// is that sample — which is the reading measured above to be unusable. Two is not much
// evidence, but it is the point at which "the largest of these" starts meaning anything, and
// requiring more would fail the probe on a busy machine rather than degrade it, when degrading
// to the uncalibrated core count is exactly what an unmeasurable host should do.
const ProbeMinUsableReps = 2

// ProbeSkipAbove is the ambient run queue per core above which the probe does not run.
//
// A PROBE ON AN ALREADY-SATURATED MACHINE MEASURES THE OTHER WORKLOAD. The ratio the probe
// takes is partly self-correcting — both phases run under the same background load, so
// contention cancels to first order — but that stops being true once the machine is oversub-
// scribed, because the single-worker phase is then competing too and the denominator sags.
// Past that point the reading is neither accurate nor worth taking, and adding load to a
// machine already at four times its run queue is the rude version of a measurement nobody can
// use. Skipping is honest: EffectiveCapacity falls back to CPUCapacity, which is exactly
// today's behaviour.
//
// 4.0 rather than something tighter, because this host's own ambient load was measured at 2.91
// per core mid-work (docs/climb-condition.md §4.3) and a gate that skipped there would skip on
// every developer machine the governor is for — which would quietly make the probe dead code
// on precisely the population it was written for.
const ProbeSkipAbove = 4.0

// ProbeMaxDrift is how far the two single-worker phases may disagree before the probe is
// reported as failed, as a fraction of their mean.
//
// THE INTERLEAVE IS THE METHOD AND THIS IS THE CHECK ON IT. docs/climb-condition.md §4.4 had
// to discard an entire experiment because an un-interleaved comparison drew a moment when the
// machine was busy with unrelated work, and produced the right headline for the wrong reason;
// what caught it was running the arms interleaved and timing a canary. This probe does the
// same thing at a smaller scale: it measures one worker, then N, then one worker again, and
// the two single-worker phases are the canary. If the machine moved under the measurement they
// disagree, and a ratio taken across a machine that changed speed halfway through is not a
// core count.
//
// 25% is loose on purpose. A fanless laptop genuinely varies this much between two 400 ms
// windows, and a threshold tight enough to be satisfying would fail the probe most of the time
// on the hardware it is for — which would be a stricter-looking check that measured nothing.
const ProbeMaxDrift = 0.25

// ProbeResult is what the load probe found.
type ProbeResult struct {
	// OK is whether the probe ran and its result is usable. Everything below is meaningless
	// when it is false.
	OK bool
	// Skipped is whether the probe declined to run rather than failing, and Reason says
	// which. A skip is a decision and a failure is a defect; reporting them alike would make
	// a busy machine look like a broken one.
	Skipped bool
	Reason  string
	// EffectiveCores is parallel throughput divided by single-worker throughput: how many
	// cores' worth of work this machine actually delivers when asked for Workers of them.
	// It is the MAXIMUM over the usable repetitions; see Probe.
	EffectiveCores float64
	// Workers is how many the parallel phase ran.
	Workers int
	// Drift is the drift of the repetition that produced EffectiveCores, as a fraction of
	// the mean of its two single-worker phases.
	Drift float64
	// Reps and UsableReps are how many repetitions ran and how many survived the drift
	// check. The pair is recorded because "6.1 cores, from 5 of 5" and "6.1 cores, from 2 of
	// 5" are different amounts of evidence for the same number, and a provenance note that
	// could not tell them apart would be hiding the thing a reader should judge it on.
	Reps       int
	UsableReps int
	// Spread is the range of the usable repetitions, largest minus smallest. It is the
	// honest width of this measurement: the estimator is a maximum precisely because the
	// noise is one-sided, so a wide spread means the machine was busy, not that the hardware
	// is uncertain.
	Spread float64
	// Duration is how long one phase ran.
	Duration time.Duration
}

// Probe measures how many cores this host actually delivers under parallel load.
//
// # Why a core count is not a core count
//
// `runtime.NumCPU()` reports 8 on an Apple silicon laptop with four performance cores and four
// efficiency cores. Both kinds run work and the scheduler uses both, so counting only the fast
// ones under-reports — which is exactly why signals.cpuCapacity refuses to. But eight is not
// eight either: the efficiency cores are a fraction of the speed, so a machine asked for eight
// cores of compiler delivers materially fewer than eight cores of compiling. `budget.MaxSlots`
// divides capacity by 1.2 cores per slot, so that error lands straight in B_max. Measured on
// this host, the probe reports about 6.1 of 8 — which is four fast cores plus four at roughly
// half throughput, and is the number the hardware predicts.
//
// A ratio of measured throughputs sees this and a core count cannot. It also, for free, covers
// every other reason a core is not a core — SMT siblings sharing an execution unit, a noisy
// neighbour on a shared cloud host, a CPU quota expressed in a way the cgroup reader missed,
// thermal limits on a machine that has been busy. None of those are enumerated anywhere; all
// of them show up as throughput that did not scale.
//
// # The shape: three phases, repeated, and the maximum taken
//
// One repetition is one worker, then Workers of them, then one worker again. Its ratio is the
// parallel throughput over the mean of the two single-worker phases, and those two phases are
// also the canary that says whether the machine held still — ProbeMaxDrift, which is the
// lesson docs/climb-condition.md §4.4 paid for with a discarded run.
//
// # Why the MAXIMUM over repetitions, and not the mean or the median
//
// BECAUSE THE NOISE IS ONE-SIDED, AND THAT IS A FACT ABOUT THE MECHANISM RATHER THAN A HOPE.
// The only thing background work can do to this ratio is depress it: other runnable threads
// steal a share of the parallel phase, so every contaminated repetition reads LOW and none can
// read high. The hardware's true parallel scaling is therefore the upper envelope of the
// samples, not their centre — and the mean of a set of measurements whose errors all point the
// same way is a biased estimate of the wrong thing.
//
// THE ALTERNATIVE WAS MEASURED AND IS NOT USABLE, which is why this is not an argument from
// first principles alone. A single repetition, taken twelve times on this host as it settled
// from loadavg1 16.7 to 8.8, reported 1.89, 3.13, 3.83, 3.94, 5.33, 5.40, 5.47, 6.05 and three
// failures: a 3.2x spread on a fixed property of the hardware. The maximum over five, taken six
// times over background load from 7.9 to 12.1, reported 5.70, 5.71, 6.31, 5.69, 6.53 and 6.81.
// The estimator that is stable across a 50% change in background load is the one that is
// measuring the machine.
//
// Spread is reported rather than hidden, and it is what a reader should judge the number on: a
// wide spread means the machine was busy during the probe, not that the hardware is uncertain.
//
// # Politeness
//
// Announced before it starts, bounded by construction rather than by a deadline anything has
// to enforce, and declined outright on a machine that is already loaded. It runs one goroutine
// per core in quarter-second bursts: enough to be measured, not enough to heat a chassis,
// which matters because on a fanless machine a probe long enough to throttle the host measures
// the throttling rather than the host.
func Probe(ctx context.Context, cores int, ambient float64, opts Options) ProbeResult {
	workers := cores
	if workers < 1 {
		workers = runtime.NumCPU()
	}
	d := opts.ProbeDuration
	if d <= 0 {
		d = DefaultProbeDuration
	}
	reps := opts.probeRepetitions()
	r := ProbeResult{Workers: workers, Duration: d, Reps: reps}

	if workers < 2 {
		// Nothing to measure: parallel throughput over single throughput on one core is 1
		// by construction, and reporting that as a measurement would dress up an identity
		// as a reading.
		r.Skipped = true
		r.Reason = "one usable core, so there is no parallel scaling to measure"
		return r
	}
	if ambient > ProbeSkipAbove {
		r.Skipped = true
		r.Reason = "the machine is already loaded, so a probe would measure the other workload"
		return r
	}
	if err := ctx.Err(); err != nil {
		r.Skipped = true
		r.Reason = "cancelled before the probe started"
		return r
	}

	opts.announce(probeNotice(workers, time.Duration(reps)*3*d))

	best, worst, bestDrift := 0.0, 0.0, 0.0
	for i := 0; i < reps; i++ {
		if ctx.Err() != nil {
			break
		}
		eff, drift, ok := oneRep(ctx, workers, d)
		if !ok {
			continue
		}
		r.UsableReps++
		if eff > best {
			best, bestDrift = eff, drift
		}
		if worst == 0 || eff < worst {
			worst = eff
		}
	}

	if r.UsableReps < ProbeMinUsableReps {
		// Distinguished from a skip: the probe ran and the machine would not hold still for
		// it. Falling back to the uncalibrated core count is the same outcome either way,
		// but they are different facts and the provenance note says which happened.
		r.Reason = "the machine would not hold still: too few repetitions survived the drift check"
		return r
	}
	r.EffectiveCores = math.Round(best*100) / 100
	r.Spread = math.Round((best-worst)*100) / 100
	r.Drift = bestDrift
	r.OK = true
	return r
}

// oneRep runs a single three-phase repetition and returns its scaling ratio.
//
// A repetition that fails its drift check returns ok false and is dropped rather than clamped
// to something. The maximum over the survivors is the estimator; feeding it a reading taken
// across a machine that changed speed would let exactly the wrong sample win, because a
// denominator that sagged mid-repetition produces a ratio that is too HIGH.
func oneRep(ctx context.Context, workers int, d time.Duration) (eff, drift float64, ok bool) {
	one := spin(ctx, 1, d)
	many := spin(ctx, workers, d)
	oneAgain := spin(ctx, 1, d)

	if ctx.Err() != nil {
		return 0, 0, false
	}
	single := (one + oneAgain) / 2
	if single <= 0 || many <= 0 {
		return 0, 0, false
	}
	drift = math.Abs(oneAgain-one) / single
	if drift > ProbeMaxDrift {
		return 0, drift, false
	}
	eff = many / single
	if eff < 1 {
		// Parallel throughput below one worker's is a machine so contended that the answer
		// is not about cores. Clamped rather than dropped, because it is a real lower bound
		// and the estimator is a maximum, so it can only lose.
		eff = 1
	}
	if eff > float64(workers) {
		// Above the worker count means the measurement caught the machine getting FASTER
		// between phases — the reverse of drift, and the same non-reading. Clamping matters
		// more here than anywhere else in this file precisely because the estimator is a
		// maximum: an unclamped outlier would win every time.
		eff = float64(workers)
	}
	return eff, drift, true
}

// spin runs n workers for d and returns total operations per second across them.
//
// The kernel is integer mixing with no allocation and no syscalls, accumulated into an atomic
// that is read at the end. It has to be work the compiler cannot delete — a loop whose result
// is discarded is a loop that measures the optimiser — which is what the accumulate-and-return
// shape is for, and why the counter is incremented per batch rather than per iteration: the
// atomic must not become the thing being measured.
func spin(ctx context.Context, n int, d time.Duration) float64 {
	var ops atomic.Uint64
	var wg sync.WaitGroup
	deadline := time.Now().Add(d)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			x := seed*2654435761 + 1
			var done uint64
			for {
				if time.Now().After(deadline) || ctx.Err() != nil {
					break
				}
				// A batch between clock reads, so that time.Now() is not what is being
				// timed. 4096 is small enough to keep the overrun past the deadline
				// negligible and large enough that the clock read is noise.
				for j := 0; j < 4096; j++ {
					x ^= x << 13
					x ^= x >> 7
					x ^= x << 17
				}
				done += 4096
				// Keep x live across the batch so the mixing cannot be optimised away.
				if x == 0 {
					x = seed + 1
				}
			}
			ops.Add(done)
		}(uint64(i))
	}
	wg.Wait()

	elapsed := time.Since(deadline.Add(-d)).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(ops.Load()) / elapsed
}
