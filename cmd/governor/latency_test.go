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
// with the stall in place the failures read "203ms (0s of it scheduler lateness)".
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

// String is written for the failure message, so it shows the split rather than hiding it.
// A reader who sees a large lateness figure is looking at a runner problem; a reader who
// sees a small one is looking at a real regression.
func (l latency) String() string {
	return fmt.Sprintf("%v (%v of it scheduler lateness)",
		l.elapsed.Round(time.Millisecond), l.noise.Round(time.Millisecond))
}

// timed runs op and reports how long it took alongside how late this machine was running a
// goroutine that had nothing to do while it ran.
func timed(op func()) latency {
	stop := make(chan struct{})
	worst := make(chan time.Duration, 1)
	go func() {
		var late time.Duration
		for {
			at := time.Now()
			select {
			case <-stop:
				worst <- late
				return
			case <-time.After(noiseTick):
				if over := time.Since(at) - noiseTick; over > late {
					late = over
				}
			}
		}
	}()

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

	// A runtime with one processor and more spinning goroutines than it can run is the
	// loaded CI runner in miniature, and the pacer must notice. If this ever reports zero
	// the discount has become a no-op and every ceiling above is back to raw wall clock.
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
