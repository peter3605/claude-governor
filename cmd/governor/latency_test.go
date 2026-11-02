package main

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// The latency assertions in this package share one problem, and this file is the one answer
// to it (GOV-B08).
//
// Three commands here are held to a wall-clock ceiling: the gate's failOpenCeiling, the
// statusline's statuslineCeiling, and session start's sessionStartCeiling — the first two a
// budget plus twenty milliseconds, the third those twenty on top of two stacked budgets.
// The twenty is the slack a process is allowed getting to and from the work. On an idle
// laptop it is generous. On a shared macOS CI runner it is about one scheduling quantum,
// and on 2026-09-09 that cost a re-run at the 10x multiplier when
// TestStatuslineWhenTheDaemonIsUnreachable came in at 72 ms against a 70 ms ceiling, in a
// file the pull request had not touched.
//
// WHAT WAS NOT CHANGED, AND WHY. Not the budgets: 50 ms for the statusline and 100 ms for
// the gate are product promises — one runs on every assistant message, the other on every
// spawn — and raising a promise so a test stops failing is moving the goalposts. Not the
// slack either: a ceiling loose enough never to flake is a ceiling that no longer catches
// the regression it exists for, which is a path that starts waiting on the daemon where it
// used to short-circuit. All three ceilings are exactly where they were.
//
// WHAT CHANGED IS WHAT THE CEILING IS COMPARED AGAINST. A loaded runner does not make this
// code slower. It makes every goroutine on the machine late, including ones with no work to
// do at all — and being run late is precisely how a 50 ms budget backstopped by a timer
// returns at 72 ms. So each timed run now also measures that: a pacer goroutine that sleeps
// in short hops for the life of the operation and keeps the worst lateness it saw. The
// assertion subtracts it, and what is left is the time the code itself spent, which is what
// the ceiling was always about.
//
// This is a discount for scheduling, not for slowness. A stall in the code — the 200 ms one
// this story's acceptance criterion inserts — does not make a sleeping goroutine late, is
// not measured as noise, is not subtracted, and still turns the assertion red. Measured:
// with the stall in place the failures read "203ms (0s of it scheduler lateness)", and
// re-measured after GOV-B10 changed what the pacer counts, "206ms (1ms of it scheduler
// lateness)" — the millisecond being what it now costs to get the pacer onto a processor
// under -race, and nowhere near enough to discount a stall into passing.
//
// WHAT THE DISCOUNT COSTS. The noise figure is the worst lateness of the whole window, and
// the operation only ever suffered one late wakeup, so on a badly starved machine it
// subtracts more than the operation actually lost. Measured under GOMAXPROCS=1 with four
// spinning goroutines: a 50 ms sleep took 68 ms and was charged 48 ms of noise. That is
// deliberate — the estimator errs toward not failing, because a false red here costs a
// macOS re-run at the 10x multiplier and teaches the next person to widen the ceiling. The
// price is that on a runner that late, a regression smaller than the load itself would slip
// through. On a runner that late nothing smaller than the load is measurable anyway, and
// the 200 ms case that this guard exists for is not close to that line.
//
// THE NEXT PERSON: if one of these ceilings starts flaking again, the number is not the
// thing to change. Either the noise floor is not being measured over the whole operation, or
// something really did get slower.
//
// WHAT GOV-B10 FOUND, because the first version of the pacer measured only its ticks. A
// goroutine that is never scheduled cannot report how late it is. Under GOMAXPROCS=1 with
// four spinners — the loaded runner in miniature, below — the pacer was starved out of the
// window entirely on 40 of 100 measured runs: zero ticks, the first select finding stop
// already closed, and a discount of exactly nothing handed back for the most starved
// machines of the set. The discount went quiet on precisely the runners it exists for, and
// the guard beneath went red one run in ten saying so.
//
// The answer is that a wakeup is late from the moment it was owed, whether or not the
// goroutine is around to see it. The pacer is owed its first instruction when it is created
// and every later one a tick after the last, so it now opens its account at creation and
// settles it on whichever channel wins the final race. Being starved for the whole window
// is no longer indistinguishable from a machine with nothing to report.

// noiseTick is how often the pacer wakes.
//
// Short enough that a run which only lasts a 50 ms budget is still sampled twenty-odd times,
// and long enough that the pacer is not itself a load on the runner it is measuring.
const noiseTick = 2 * time.Millisecond

// latency is what one timed run cost, split into the part the code spent and the part this
// machine owes for having been busy elsewhere.
type latency struct {
	elapsed time.Duration
	noise   time.Duration
}

// own is the time attributable to the code under test.
func (l latency) own() time.Duration { return l.elapsed - l.noise }

// exceeds reports whether the code — not the runner — went over the ceiling.
func (l latency) exceeds(ceiling time.Duration) bool { return l.own() > ceiling }

// inconclusive reports whether this run cannot say anything about the ceiling at all: the
// runner was late by more than the whole ceiling, so no regression smaller than the load is
// measurable on it (see "what the discount costs" above).
//
// WHY THIS EXISTS, AND WHY IT IS NOT A WIDER CEILING (GOV-B19). The discount subtracts the
// single worst late wakeup, which is sound for one stall and undercounts an operation that
// was descheduled more than once. A darwin/arm64 runner did exactly that on PR #72:
// "took 218ms (124ms of it scheduler lateness), want under 70ms" for a statusline that reads
// a corrupt cache and makes one local round trip. Summing every late wakeup instead was
// measured and REJECTED: under GOMAXPROCS=1 with four spinners it discounted a genuine 200 ms
// stall to 0–18 ms, because a starved pacer is late on every tick even while the operation is
// asleep and losing nothing. The worst-of estimator still charged that stall 96–198 ms. So
// the estimator stays, the ceiling stays, and a run whose noise alone exceeds the ceiling is
// reported as unmeasurable rather than as a failure of code that did nothing wrong.
//
// What it costs: a real regression is missed on the runs that are this starved, and caught
// on every other. Measured idle, the 200 ms stall carried no noise at all.
func (l latency) inconclusive(ceiling time.Duration) bool { return l.noise >= ceiling }

// String is written for the failure message, so it shows the split rather than hiding it.
// A reader who sees a large lateness figure is looking at a runner problem; a reader who
// sees a small one is looking at a real regression.
func (l latency) String() string {
	return fmt.Sprintf("%v (%v of it scheduler lateness)",
		l.elapsed.Round(time.Millisecond), l.noise.Round(time.Millisecond))
}

// pace reports, until stop closes, the worst lateness of a goroutine that has nothing to do.
//
// launched is when the caller created the goroutine this runs on, and the delay before the
// first line here is the first thing charged: a goroutine is owed its first instruction
// immediately, and on a machine starved enough to withhold that for the whole window it is
// the only lateness there will be to report (GOV-B10).
//
// Both ways out of the select settle through the same statement, deliberately. The wakeup was
// owed at at+noiseTick regardless of which channel won the race, so arriving after that is
// lateness whether the pacer got a tick or found the operation already over, and arriving
// before it leaves the figure negative and changes nothing. One statement rather than one per
// branch means no exit can be reached without being accounted for.
func pace(launched time.Time, stop <-chan struct{}) time.Duration {
	late := time.Since(launched)
	for {
		at := time.Now()
		var over bool
		select {
		case <-stop:
			over = true
		case <-time.After(noiseTick):
		}
		late = max(late, time.Since(at)-noiseTick)
		if over {
			return late
		}
	}
}

// timed runs op and reports how long it took alongside how late this machine was running a
// goroutine that had nothing to do while it ran.
func timed(op func()) latency {
	stop := make(chan struct{})
	worst := make(chan time.Duration, 1)
	launched := time.Now()
	go func() { worst <- pace(launched, stop) }()

	start := time.Now()
	op()
	elapsed := time.Since(start)

	close(stop)
	return latency{elapsed: elapsed, noise: <-worst}
}

// TestTimedChargesTheRunnerForItsOwnLateness is the guard on the guard.
//
// Every latency ceiling in this package now subtracts what timed measured. A timed that
// measured nothing would quietly turn all of them back into the raw wall-clock assertions
// this file exists to replace, and not one of them would fail to say so. This is the only
// place that would.
//
// Only the direction is asserted, never a figure: how late a goroutine runs is the property
// of the machine this suite happens to be on, and a test that pinned a number for it would
// be the same mistake in a new place.
// TestALatencyTooNoisyToJudgeIsNotAFailure pins the three verdicts on the figures that
// produced them (GOV-B19): the darwin/arm64 run that flaked, the 200 ms stall the ceilings
// exist to catch (GOV-B08's "206ms (1ms of it scheduler lateness)"), and an ordinary pass.
func TestALatencyTooNoisyToJudgeIsNotAFailure(t *testing.T) {
	const ceiling = 70 * time.Millisecond
	for _, tc := range []struct {
		name         string
		l            latency
		inconclusive bool
		exceeds      bool
	}{
		{"the PR #72 flake: starved past the whole ceiling", latency{218 * time.Millisecond, 124 * time.Millisecond}, true, true},
		{"a real 200 ms stall on a runner that was on time", latency{206 * time.Millisecond, time.Millisecond}, false, true},
		{"an ordinary pass", latency{3 * time.Millisecond, 0}, false, false},
	} {
		if got := tc.l.inconclusive(ceiling); got != tc.inconclusive {
			t.Errorf("%s: inconclusive = %v, want %v", tc.name, got, tc.inconclusive)
		}
		if got := tc.l.exceeds(ceiling); got != tc.exceeds {
			t.Errorf("%s: exceeds = %v, want %v", tc.name, got, tc.exceeds)
		}
	}
}

func TestTimedChargesTheRunnerForItsOwnLateness(t *testing.T) {
	t.Run("the split is well formed", func(t *testing.T) {
		took := timed(func() { time.Sleep(5 * time.Millisecond) })
		if took.noise < 0 {
			t.Errorf("noise = %v, want it never negative", took.noise)
		}
		if took.own() > took.elapsed {
			t.Errorf("own = %v, elapsed = %v: the discount may only ever subtract", took.own(), took.elapsed)
		}
	})

	// The starved case above cannot be staged on demand — whether the runtime withholds
	// the pacer for a whole window is the scheduler's to decide, and on 40 runs in 100 it
	// did (GOV-B10). This stages the consequence instead, with no scheduler in it: a pacer
	// handed a creation instant already in the past and an operation already over is a
	// pacer that was never scheduled inside the window, and it must still say how long it
	// was held. Reporting nothing here is the discount going quiet, which is how the
	// ceilings above turn back into raw wall clock without a single test saying so.
	t.Run("a pacer that never ran inside the window is charged for the wait", func(t *testing.T) {
		const held = 50 * time.Millisecond

		over := make(chan struct{})
		close(over)

		late := pace(time.Now().Add(-held), over)

		if late < held {
			t.Errorf("late = %v after being held %v before its first instruction, want at least %v: "+
				"a goroutine that never ran is not a goroutine that was on time", late, held, held)
		}
	})

	// A runtime with one processor and more spinning goroutines than it can run is the
	// loaded CI runner in miniature, and the pacer must notice. If this ever reports zero
	// the discount has become a no-op and every ceiling above is back to raw wall clock.
	//
	// This is the subtest GOV-B10 fixed, and the fix was not here: the assertion is the
	// same one, still on the direction and still never on a figure. What changed is that
	// the pacer now charges for a window it was starved out of, so both ways the scheduler
	// can take this — pacing the operation, or never running until it is over — arrive at
	// some lateness rather than one of them arriving at none.
	t.Run("a starved runtime is charged for the delay it causes", func(t *testing.T) {
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

		stop := make(chan struct{})
		var spinning sync.WaitGroup
		for i := 0; i < 4; i++ {
			spinning.Add(1)
			go func() {
				defer spinning.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
				}
			}()
		}

		took := timed(func() { time.Sleep(20 * time.Millisecond) })
		close(stop)
		spinning.Wait()

		if took.noise <= 0 {
			t.Errorf("noise = %v with the runtime starved, want some: the pacer is not measuring anything",
				took.noise)
		}
	})
}
