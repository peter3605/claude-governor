package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// runAShape is GOV-B54 run A: 2988 MHz for eight seconds, falling 32 MHz a second to 1700.
func runAShape(n int) []float64 {
	f := make([]float64, n)
	for i := range f {
		v := 2988.0
		if i >= 8 {
			v = math.Max(1700, 2988-32*float64(i-8))
		}
		f[i] = v
	}
	return f
}

func probesFor(rows []row, f func(i int, pm float64) (core string, mhz float64)) []probeReading {
	ps := make([]probeReading, len(rows))
	for i, r := range rows {
		core, mhz := f(i, r.PFreqMHz)
		ps[i] = probeReading{At: r.At.Add(200 * time.Millisecond), Kind: "probe", Core: core, MHz: mhz, Cost: 0.006}
	}
	return ps
}

func TestAProbeThatReadsTheClockTracks(t *testing.T) {
	rows := busyRun(runAShape(900))
	s := analyse(rows, defaultThresholds, 3204)
	// Reads 1% high, and lands on an E-core one tick in ten: those must be left out, not scored.
	ps := probesFor(rows, func(i int, pm float64) (string, float64) {
		if i%10 == 0 {
			return "E", 2064
		}
		return "P", pm * 1.01
	})
	sc := scoreProbe(rows, ps, defaultThresholds)
	if !sc.ProbeThrottled || !s.Throttled {
		t.Fatalf("probe throttled %v, pm throttled %v", sc.ProbeThrottled, s.Throttled)
	}
	if sc.PShare < 0.89 || sc.PShare > 0.91 || sc.Correlation < 0.999 {
		t.Errorf("P share %.2f, correlation %.4f", sc.PShare, sc.Correlation)
	}
	if sc.MedianErrMHz <= 0 || sc.MedianAbsErrMHz > 35 {
		t.Errorf("median error %+.0f, |error| %.0f; want a small positive bias", sc.MedianErrMHz, sc.MedianAbsErrMHz)
	}
	if d := sc.ProbeFirstBelow - s.FirstBelow; d < -2*time.Second || d > 2*time.Second {
		t.Errorf("probe first below at %v, powermetrics at %v", sc.ProbeFirstBelow, s.FirstBelow)
	}
}

func TestAProbeThatStaysFlatMisses(t *testing.T) {
	rows := busyRun(runAShape(900))
	ps := probesFor(rows, func(int, float64) (string, float64) { return "P", 2988 })
	if sc := scoreProbe(rows, ps, defaultThresholds); sc.ProbeThrottled {
		t.Errorf("a flat probe reported a throttle")
	}
}

// A probe that never reaches a P-core cannot speak for the P-cluster, however its readings move.
func TestEOnlyReadingsAreNotScored(t *testing.T) {
	rows := busyRun(runAShape(900))
	ps := probesFor(rows, func(int, float64) (string, float64) { return "E", 2064 })
	sc := scoreProbe(rows, ps, defaultThresholds)
	if sc.BusyP != 0 || sc.ProbeThrottled || sc.Joined != 900 {
		t.Errorf("E-only probe scored: %+v", sc)
	}
}

// Idle seconds are excluded on both sides: an idle P-cluster clocks down by design.
func TestIdleSecondsAreNotScored(t *testing.T) {
	rows := busyRun(make([]float64, 300))
	for i := range rows {
		rows[i].PFreqMHz, rows[i].PResidency = 600, 5
	}
	ps := probesFor(rows, func(int, float64) (string, float64) { return "P", 1000 })
	if sc := scoreProbe(rows, ps, defaultThresholds); sc.Joined != 0 || sc.ProbeThrottled {
		t.Errorf("idle seconds were scored: %+v", sc)
	}
}

// A temperature is scored against the clock's drop, not as a clock: a sensor that heats as
// the P-cluster slows reads strongly negative, and the report gives its value at the drop.
func TestATemperatureIsScoredAgainstTheDrop(t *testing.T) {
	rows := busyRun(runAShape(900))
	s := analyse(rows, defaultThresholds, 3204)
	ps := probesFor(rows, func(i int, pm float64) (string, float64) { return "P", pm })
	for i := range ps {
		c := 40 + (2988-rows[i].PFreqMHz)/20 // 40 C cool, about 104 C at the 1700 floor
		ps[i].PTempC = &c
	}
	sc := scoreProbe(rows, ps, defaultThresholds)
	if len(sc.Temps) != 900 || sc.TempCorr > -0.999 {
		t.Fatalf("%d temperatures, correlation %.3f; want 900 and about -1", len(sc.Temps), sc.TempCorr)
	}
	if c := tempNear(sc.Temps, s.FirstBelow); c < 50 || c > 60 {
		t.Errorf("temperature at first below %.1f C, want the ~55 C the shape gives there", c)
	}
	var b strings.Builder
	printProbe(&b, sc, s)
	if !strings.Contains(b.String(), "probe temp") {
		t.Errorf("no temperature line in:\n%s", b.String())
	}
}

// A probe with no temperature prints no temperature line, rather than a line of zeros.
func TestNoTemperatureNoLine(t *testing.T) {
	rows := busyRun(runAShape(300))
	s := analyse(rows, defaultThresholds, 3204)
	sc := scoreProbe(rows, probesFor(rows, func(int, float64) (string, float64) { return "P", 2988 }), defaultThresholds)
	var b strings.Builder
	printProbe(&b, sc, s)
	if strings.Contains(b.String(), "probe temp") {
		t.Errorf("temperature line without temperatures:\n%s", b.String())
	}
}
