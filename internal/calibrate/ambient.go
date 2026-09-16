package calibrate

import (
	"context"
	"sort"
	"time"

	"github.com/shirou/gopsutil/v4/load"
)

// Options tunes a calibration run. Its zero value is the shipped behaviour.
type Options struct {
	// NoProbe skips the load probe entirely. The ambient reading and the host shape are
	// still measured; B_max falls back to CPUCapacity, which is what it is today.
	NoProbe bool
	// AmbientSamples is how many load-average reads the ambient figure is taken over.
	// Zero means DefaultAmbientSamples.
	AmbientSamples int
	// AmbientInterval is the gap between those reads. Zero means DefaultAmbientInterval.
	AmbientInterval time.Duration
	// ProbeDuration is how long each phase of the load probe runs. Zero means
	// DefaultProbeDuration. One repetition of the probe runs three phases; see Probe.
	ProbeDuration time.Duration
	// ProbeRepetitions is how many repetitions the probe takes the maximum over. Zero means
	// DefaultProbeRepetitions.
	ProbeRepetitions int
	// Announce receives one line before anything that will be felt on the machine, so the
	// probe is never a surprise. Nil is silent, which is what a test wants.
	Announce func(string)
}

func (o Options) ambientSamples() int {
	if o.AmbientSamples > 0 {
		return o.AmbientSamples
	}
	return DefaultAmbientSamples
}

func (o Options) ambientInterval() time.Duration {
	if o.AmbientInterval > 0 {
		return o.AmbientInterval
	}
	return DefaultAmbientInterval
}

func (o Options) probeRepetitions() int {
	if o.ProbeRepetitions > 0 {
		return o.ProbeRepetitions
	}
	return DefaultProbeRepetitions
}

func (o Options) announce(s string) {
	if o.Announce != nil {
		o.Announce(s)
	}
}

// DefaultAmbientSamples and DefaultAmbientInterval are how the ambient reading is taken.
//
// FIVE READS 200 ms APART IS ONE SECOND, AND THAT IS NOT A CONTRADICTION OF THE PROBE-LENGTH
// TRAP. The trap — GOV-25's own, and the one most likely to cost a session — says a probe
// shorter than the averaging window of the signal it reads cannot calibrate a threshold on
// that signal, because `loadavg1` is a one-minute decaying average and a few-second probe
// barely moves it.
//
// THE WAY PAST IT IS TO STOP TRYING TO MOVE THE SIGNAL. The trap binds on measuring what LOAD
// THIS PROBE PRODUCES, because that requires the average to respond inside the probe. It does
// not bind on measuring what the machine was ALREADY DOING, because `loadavg1` read once is
// itself a sixty-second measurement — of the preceding minute of the developer's real work,
// already taken, already decayed, sitting in the kernel waiting to be read. The window this
// number needs has elapsed before `calibrate` starts.
//
// So the ambient figure needs no load probe at all, it costs nothing, and it is polite by
// construction because it perturbs nothing. The five reads are not there to lengthen a window;
// they are there so that one anomalous read cannot decide a threshold, and the median is taken
// for the same reason. A second of wall clock buys that guard, and over a second a one-minute
// average has barely moved, which is the point rather than a limitation.
//
// WHAT THIS DOES NOT FIX. One second is still one moment in the day. A machine calibrated at
// 07:00 before anything is open measures a different ambient load than the same machine at
// 15:00 with Xcode indexing — docs/climb-condition.md §4.3 measured 2.91 per core on this host
// mid-work against a settled 0.35. Measure records the reading and the time, `calibrate`
// prints both, and re-running it while actually working is the documented advice. A number
// nobody can trace back to a moment is a number that gets edited at random.
const (
	DefaultAmbientSamples  = 5
	DefaultAmbientInterval = 200 * time.Millisecond
)

// Ambient reports this host's background load: the raw one-minute load average, and that
// divided by the cores this process may actually use.
//
// The divisor is CPUCapacity rather than the core count, matching signals.Sample.RunQueue
// exactly. It has to: the threshold derived from this figure is compared against that field
// by the control loop, and two numbers that are supposed to be on the same scale must be
// divided by the same thing. A cgroup-limited host divides by the cores it may use in both
// places, or the calibration is against a scale the loop does not use.
//
// The third return says whether a reading was actually taken. A load average that could not be
// read returns false, and Derive then declines to calibrate a threshold at all rather than
// treating the host as idle: silence is not a quiet machine.
func Ambient(ctx context.Context, capacity float64, opts Options) (loadAvg1, perCore float64, ok bool) {
	n := opts.ambientSamples()
	every := opts.ambientInterval()

	readings := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				// A cancelled context is not a reason to discard what was already read.
				// Whatever landed before the deadline is a real measurement of this host.
				i = n
			case <-time.After(every):
			}
		}
		if avg, err := load.AvgWithContext(ctx); err == nil {
			readings = append(readings, avg.Load1)
		}
	}
	if len(readings) == 0 {
		return 0, 0, false
	}
	sort.Float64s(readings)
	loadAvg1 = readings[len(readings)/2]
	if capacity <= 0 {
		// A load average with nothing to divide it by is not a run queue per core, and the
		// threshold is denominated per core. Report the raw figure and refuse the derived one.
		return loadAvg1, 0, false
	}
	return loadAvg1, loadAvg1 / capacity, true
}
