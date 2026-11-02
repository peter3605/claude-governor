package calibrate

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
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
// THE REPETITIONS ARE NOT REDUNDANCY, THEY ARE THE ESTIMATOR — see Probe's "which repetition".
// A single three-phase reading was measured on this host and is not usable: twelve of them,
// taken as the machine settled from loadavg1 16.7 to 8.8, reported effective cores ranging
// 1.89 to 6.05, a 3.2x spread on a quantity that is a fixed property of the hardware, with
// four of the twelve failing the drift check outright. Five gives the estimator three usable
// repetitions to stand on with room for two to be lost to the drift check.
const (
	DefaultProbeDuration    = 250 * time.Millisecond
	DefaultProbeRepetitions = 5
)

// ProbeMinUsableReps is how many repetitions must survive the drift check for the probe to
// report a result.
//
// Three, because that is the fewest the estimator and its check can be computed from: the
// highest is discarded, the second-highest is the answer, and the third is what the answer is
// checked against (ProbeMaxDisagreement). In 30 recorded trials on this host the median trial
// kept all five and none kept fewer than four, so three costs nothing on the hardware it is for.
const ProbeMinUsableReps = 3

// ProbeMaxDisagreement is how far apart the two repetitions an answer rests on — the second-
// and third-highest usable — may be, as a fraction of the second, before the answer is not
// trusted. ProbeAttempts says what happens then.
//
// IT IS A DIFFERENT QUANTITY FROM THE DRIFT CHECK, AND HAS TO BE. ProbeMaxDrift compares the two
// single-worker phases INSIDE one repetition: it catches a machine that changed speed mid-
// repetition. It cannot catch a repetition whose two baselines were slow TOGETHER, and every
// repetition of every one of the seven GOV-47 runs passed it, including the one that cut this
// host's b_max by a quarter. This compares repetitions WITH EACH OTHER: whether the probe, asked
// the same question twice, gave the same answer.
//
// NOT THE SPREAD, WHICH WAS THE OBVIOUS CANDIDATE AND IS NOT PREDICTIVE. Over 30 recorded
// trials (docs/calibration.md §8) the full range of the usable repetitions correlated +0.12
// with how far the estimate landed from the host's centre; this gap correlated +0.61. The range
// is dominated by the LOWEST repetition, which the estimator already ignores, so a wide range
// is mostly harmless contention and rejecting on it would reject good answers.
//
// 6%, FROM THE SAME 30 TRIALS: 29 of them came in at or under 5.2%, and the one at 11.6% is
// the trial whose answer was furthest from the centre (4.99 against 5.64, the only one more
// than half a slot off). 6% sits just above the body of the distribution, and the one answer
// the bound exists to stop is stopped. Validated on 23 probes it was not set from (§8.4): 21
// passed at the first attempt, 2 on the retry, none declined, and every accepted answer gave
// the same b_max. One host's distribution is the evidence, and the calibration doc says so.
const ProbeMaxDisagreement = 0.06

// ProbeAttempts is how many times the whole probe may run before it declines.
//
// Two: a disagreement on the first attempt is answered by measuring again rather than by
// reporting a figure the probe itself has said it does not trust, and a second disagreement
// declines. At the 2-in-23 first-attempt rate measured in validation, two consecutive
// disagreements on a host like this one should be about one calibration in 130; a host that
// produces them routinely is one
// whose parallel throughput is not measurable in seconds, and the honest answer there is
// "not measured" (Resolve keeps whatever an earlier run did measure). Each attempt is announced.
const ProbeAttempts = 2

// ProbeSkipAbove is the ambient run queue per core above which the probe does not run.
//
// A PROBE ON AN ALREADY-SATURATED MACHINE MEASURES THE OTHER WORKLOAD. The ratio the probe
// takes is partly self-correcting — both phases run under the same background load, so
// contention cancels to first order — but that stops being true once the machine is oversub-
// scribed, because the single-worker phase is then competing too and the denominator sags.
// Past that point the reading is neither accurate nor worth taking, and adding load to a
// machine already at four times its run queue is the rude version of a measurement nobody can
// use. Skipping is honest: nothing is derived, and Resolve keeps whatever an earlier
// calibration measured.
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
	// It is the SECOND-HIGHEST usable repetition; see Probe.
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
	// Spread is the range of the usable repetitions, largest minus smallest. It is recorded,
	// not acted on: it is dominated by the lowest repetition, which the estimator ignores,
	// and it does not predict whether the answer is good (ProbeMaxDisagreement).
	Spread float64
	// Disagreement is how far apart the two repetitions the answer rests on are — the
	// second- and third-highest usable — as a fraction of the second. It is what decides
	// whether the figure is trusted; see ProbeMaxDisagreement.
	Disagreement float64
	// Attempts is how many times the whole probe ran: a second attempt follows a first whose
	// repetitions disagreed.
	Attempts int
	// Duration is how long one phase ran.
	Duration time.Duration
	// Samples is every repetition in the order it ran, usable or not. It is what Spread and
	// EffectiveCores are summaries of, kept so that the decision the probe made can be
	// re-made from the evidence rather than trusted (docs/calibration.md §8 was set from it).
	Samples []RepSample
}

// RepSample is one repetition of the probe.
type RepSample struct {
	// Eff is the clamped scaling ratio. Zero for a repetition that produced no reading.
	Eff float64
	// Drift is the disagreement between its two single-worker phases.
	Drift float64
	// Usable is whether it survived the drift check.
	Usable bool
	// Attempt is which attempt of the probe it belongs to, from 1.
	Attempt int
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
// this host, the probe reports about 5.6 of 8 — four fast cores plus four at roughly 0.4 of
// their throughput. (Before GOV-47 this said 6.1; that figure was a maximum, and see below.)
//
// A ratio of measured throughputs sees this and a core count cannot. It also, for free, covers
// every other reason a core is not a core — SMT siblings sharing an execution unit, a noisy
// neighbour on a shared cloud host, a CPU quota expressed in a way the cgroup reader missed,
// thermal limits on a machine that has been busy. None of those are enumerated anywhere; all
// of them show up as throughput that did not scale.
//
// # The shape: three phases, repeated, and the second-highest taken
//
// One repetition is one worker, then Workers of them, then one worker again. Its ratio is the
// parallel throughput over the mean of the two single-worker phases, and those two phases are
// also the canary that says whether the machine held still — ProbeMaxDrift, which is the
// lesson docs/climb-condition.md §4.4 paid for with a discarded run.
//
// # Which repetition, and why not the maximum
//
// THE NOISE IS TWO-SIDED, AND THE MAXIMUM PICKS THE WRONG SIDE OF IT. Background work steals a
// share of the parallel phase and reads LOW; that much is what the estimator used to assume was
// all there is. But a single-worker baseline that runs slow — scheduled onto an efficiency core,
// or caught behind another thread — shrinks the denominator and reads HIGH, and the maximum
// selects precisely that repetition. Measured over 30 trials on this host (docs/calibration.md
// §8): the repetition that won the maximum had a median drift of 0.123 against 0.011 for every
// other repetition, and the maximum ranged 5.01 to 7.05 — enough to move b_max between 4 and 5
// on eleven trials in thirty. A drift gate cannot repair it: tightened to 0.03 it still kept
// the 7.05, because both baselines of that repetition were slow together and agreed.
//
// THE SECOND-HIGHEST KEEPS WHAT THE MAXIMUM WAS FOR AND DROPS WHAT IT GOT WRONG. It is still
// the upper envelope, so contention that depresses most repetitions still does not drag the
// answer down; but one inflated repetition can no longer win. Over the same 30 trials it ranged
// 4.99 to 5.80 with a standard deviation of 0.18 against the maximum's 0.35, and gave b_max 4
// on every one. The median and the mean were tried on the same data and are worse (0.25, 0.27):
// they let contention back in.
//
// AND IT IS CHECKED BEFORE IT IS USED. The second-highest is only trusted when the third-highest
// agrees with it (ProbeMaxDisagreement); otherwise the probe measures again, and then declines
// (ProbeAttempts). A probe that cannot produce a trusted figure reports that, and Derive writes
// nothing from it.
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

	for attempt := 1; attempt <= ProbeAttempts; attempt++ {
		if ctx.Err() != nil {
			break
		}
		if attempt == 1 {
			opts.announce(probeNotice(workers, time.Duration(reps)*3*d))
		} else {
			opts.announce(probeRetryNotice(r.Reason, time.Duration(reps)*3*d))
		}
		r.Attempts = attempt
		var samples []RepSample
		for i := 0; i < reps; i++ {
			if ctx.Err() != nil {
				break
			}
			eff, drift, ok := oneRep(ctx, workers, d)
			samples = append(samples, RepSample{Eff: eff, Drift: drift, Usable: ok, Attempt: attempt})
		}
		r.Samples = append(r.Samples, samples...)
		if summarise(&r, samples) {
			return r
		}
	}
	if r.Reason == "" {
		r.Reason = "cancelled before the probe finished"
	}
	return r
}

// summarise judges one attempt's repetitions, filling r, and reports whether they produced a
// figure that can be trusted. It is pure so the decision can be tested against repetitions
// recorded on real hardware rather than against whatever the test machine happens to do.
func summarise(r *ProbeResult, samples []RepSample) bool {
	var usable []RepSample
	for _, s := range samples {
		if s.Usable {
			usable = append(usable, s)
		}
	}
	sort.Slice(usable, func(i, j int) bool { return usable[i].Eff > usable[j].Eff })
	r.UsableReps = len(usable)
	r.OK, r.EffectiveCores, r.Spread, r.Drift, r.Disagreement = false, 0, 0, 0, 0

	if len(usable) < ProbeMinUsableReps {
		// Distinguished from a skip: the probe ran and the machine would not hold still for
		// it. A skip is a decision and this is a failed measurement; the provenance note
		// says which happened.
		r.Reason = "the machine would not hold still: too few repetitions survived the drift check"
		return false
	}
	est, next := usable[1], usable[2]
	r.Spread = round2(usable[0].Eff - usable[len(usable)-1].Eff)
	r.Disagreement = round3((est.Eff - next.Eff) / est.Eff)
	if r.Disagreement > ProbeMaxDisagreement {
		r.Reason = fmt.Sprintf("the repetitions disagreed: the two the answer rests on are %.0f%% apart, "+
			"more than the %.0f%% a figure is trusted at", r.Disagreement*100, ProbeMaxDisagreement*100)
		return false
	}
	r.EffectiveCores = round2(est.Eff)
	r.Drift = est.Drift
	r.Reason = ""
	r.OK = true
	return true
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// oneRep runs a single three-phase repetition and returns its scaling ratio.
//
// A repetition that fails its drift check returns ok false and is dropped rather than clamped
// to something: a denominator that sagged mid-repetition produces a ratio that is too HIGH,
// and the estimator takes the upper envelope. Its raw ratio is still returned, for the record.
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
		// The raw ratio is returned for the record only; ok false means no caller uses it.
		return many / single, drift, false
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
		// between phases — the reverse of drift, and the same non-reading. Clamped so that a
		// physically impossible figure is never what the estimator reports.
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
