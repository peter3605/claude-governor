package main

import (
	"testing"
	"time"
)

// Measured on the M1 Air: a P-core runs the wide block at 0.084 of the chain's time, an E-core
// at 0.167. These pairs reproduce those ratios at a P-core's 3 GHz and an E-core's 2 GHz.
var (
	pCore = pair{chainNs: 524_288, wideNs: 44_040}  // ratio 0.084, 3000 MHz at n=1<<19
	eCore = pair{chainNs: 786_432, wideNs: 131_334} // ratio 0.167, 2000 MHz
	slowP = pair{chainNs: 925_214, wideNs: 77_718}  // ratio 0.084, 1700 MHz: a throttled P-core
)

func cal(t *testing.T) calibration {
	t.Helper()
	c := newCalibration(1<<19, 1<<19, 3, 2*time.Millisecond,
		[]pair{pCore, pCore, pCore, eCore}, // interactive: mostly P, one stray E pair
		[]pair{eCore, eCore, eCore})
	if !c.Separable {
		t.Fatalf("0.084 against 0.167 must separate: %+v", c)
	}
	return c
}

func TestCalibrationReadsBothCoresAndTheirClocks(t *testing.T) {
	c := cal(t)
	if c.PRatio > 0.09 || c.ERatio < 0.16 {
		t.Errorf("ratios P %.3f E %.3f; the stray E pair must not pull the P ratio up", c.PRatio, c.ERatio)
	}
	if c.PMHz < 2990 || c.PMHz > 3010 || c.EMHz < 1990 || c.EMHz > 2010 {
		t.Errorf("clocks P %.0f E %.0f, want about 3000 and 2000", c.PMHz, c.EMHz)
	}
}

// The case the probe exists for: a throttled P-core is SLOWER than an E-core, so the clock
// alone would call it an E-core. The ratio must still say P.
func TestAThrottledPCoreIsStillLabelledP(t *testing.T) {
	c := cal(t)
	if got := classify(slowP.ratio(), c); got != "P" {
		t.Errorf("1700 MHz P-core labelled %s", got)
	}
	if got := classify(eCore.ratio(), c); got != "E" {
		t.Errorf("E-core labelled %s", got)
	}
	if m := mhz(1<<19, slowP.chainNs, 3); m < 1690 || m > 1710 {
		t.Errorf("throttled P-core reads %.0f MHz, want about 1700", m)
	}
}

// A tick reports the P-cluster: a faster pair that ran on an E-core must not stand in for it.
func TestATickPrefersAPPairOverAFasterEPair(t *testing.T) {
	c := cal(t)
	fastE := pair{chainNs: 700_000, wideNs: 116_900} // ratio 0.167, faster than slowP
	best, core, nP := choose([]pair{fastE, slowP}, c)
	if best != slowP || core != "P" || nP != 1 {
		t.Errorf("chose %+v (%s, %d P pairs), want the P pair", best, core, nP)
	}
	best, core, _ = choose([]pair{fastE, eCore}, c)
	if best != fastE || core != "E" {
		t.Errorf("with no P pair, chose %+v (%s), want the fastest E pair labelled E", best, core)
	}
}

func TestIndistinctRatiosLabelNothing(t *testing.T) {
	c := newCalibration(1<<19, 1<<19, 3, 0, []pair{pCore}, []pair{{chainNs: 524_288, wideNs: 50_000}})
	if c.Separable || classify(pCore.ratio(), c) != "?" {
		t.Errorf("ratios 1.14x apart were labelled: %+v", c)
	}
	if c := newCalibration(1<<19, 1<<19, 3, 0, nil, nil); c.Separable {
		t.Errorf("an empty calibration was separable: %+v", c)
	}
}
