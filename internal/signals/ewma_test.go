package signals

import (
	"math"
	"testing"
	"time"
)

// The first reading has nothing to average against. Adopting it whole is the difference
// between a daemon that reports what it measured and one that reports a machine at 0% for
// the first several seconds of its life — a claim nobody made, arriving as a default.
func TestEWMAAdoptsItsFirstReading(t *testing.T) {
	e := NewEWMA(DefaultCPUWindow)

	if v, primed := e.Value(); primed || v != 0 {
		t.Fatalf("before any reading: value=%v primed=%v, want 0/false", v, primed)
	}

	now := time.Now()
	if got := e.Update(0.8, now); got != 0.8 {
		t.Errorf("first Update = %v, want the reading itself (0.8)", got)
	}
	if v, primed := e.Value(); !primed || v != 0.8 {
		t.Errorf("after one reading: value=%v primed=%v, want 0.8/true", v, primed)
	}
}

// The window is five SECONDS, not five samples. A sampler on a loaded machine skips ticks,
// and that is exactly when the signal matters, so a gap must carry more weight than a
// punctual tick rather than the same weight.
func TestEWMAIsTimeAware(t *testing.T) {
	start := time.Now()

	punctual := NewEWMA(5 * time.Second)
	punctual.Update(0, start)
	afterOneSecond := punctual.Update(1, start.Add(time.Second))

	delayed := NewEWMA(5 * time.Second)
	delayed.Update(0, start)
	afterTenSeconds := delayed.Update(1, start.Add(10*time.Second))

	if !(afterTenSeconds > afterOneSecond) {
		t.Errorf("a 10 s gap moved the average to %v and a 1 s gap to %v: the window is counting ticks, not time",
			afterTenSeconds, afterOneSecond)
	}
	// A gap of twice the time constant should be most of the way there; a single 1 s tick
	// against a 5 s window should not be.
	if afterOneSecond > 0.5 {
		t.Errorf("one 1 s tick moved a 5 s window to %v, want well under half", afterOneSecond)
	}
	if afterTenSeconds < 0.5 {
		t.Errorf("a 10 s gap moved a 5 s window only to %v, want most of the way", afterTenSeconds)
	}
}

// A step input must converge on the new level rather than settling short of it, which is
// what an incorrectly applied smoothing factor looks like and is invisible in a single call.
func TestEWMAConvergesOnASteadySignal(t *testing.T) {
	e := NewEWMA(5 * time.Second)
	now := time.Now()
	e.Update(0, now)

	for i := 1; i <= 60; i++ {
		now = now.Add(time.Second)
		e.Update(0.75, now)
	}

	got, _ := e.Value()
	if math.Abs(got-0.75) > 0.001 {
		t.Errorf("after a minute of a steady 0.75 the average is %v, want ~0.75", got)
	}
}

// A clock that does not advance, or goes backwards, must not produce a negative weight.
// The sampler runs unattended for days and NTP steps are not hypothetical.
func TestEWMASurvivesANonAdvancingClock(t *testing.T) {
	e := NewEWMA(5 * time.Second)
	now := time.Now()
	e.Update(0.2, now)

	if got := e.Update(0.9, now); got != 0.9 {
		t.Errorf("same-instant Update = %v, want the new reading (0.9)", got)
	}
	if got := e.Update(0.4, now.Add(-time.Minute)); got != 0.4 {
		t.Errorf("backwards-clock Update = %v, want the new reading (0.4)", got)
	}
	if v, _ := e.Value(); math.IsNaN(v) || math.IsInf(v, 0) {
		t.Errorf("value went non-finite: %v", v)
	}
}
