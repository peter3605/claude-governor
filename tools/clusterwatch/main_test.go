package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// The testdata files are one real capture from the M1 Air (clusterwatch -raw): the pmgr
// voltage-state tables, one cumulative IOReport sample, and the IOHID temperature list.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fake is a source scripted per tick. A nil error with empty text is a valid empty answer.
type fake struct {
	tables, temps string
	tErr, hErr    error
	samples       []string
	sErr          []error
	i             int
}

func (f *fake) Tables() (string, error) { return f.tables, f.tErr }
func (f *fake) Temps() (string, error)  { return f.temps, f.hErr }
func (f *fake) Sample() (string, error) {
	i := f.i
	f.i++
	if i < len(f.sErr) && f.sErr[i] != nil {
		return "", f.sErr[i]
	}
	if i < len(f.samples) {
		return f.samples[i], nil
	}
	return "", errors.New("script ran out")
}

// pSample is a one-line P-cluster sample in the M1 layout: IDLE then fifteen DVFS states.
func pSample(idle int64, ticks [15]int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PCPU\tIDLE=%d", idle)
	for i, t := range ticks {
		fmt.Fprintf(&b, "\tV%dP%d=%d", i, 14-i, t)
	}
	return b.String() + "\n"
}

func TestTheM1TablesMapToTheClustersByCount(t *testing.T) {
	tables, err := parseTables(fixture(t, "m1-tables.txt"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := tableFor(15, tables)
	if err != nil || p[0] != 600 || p[11] != 2988 || p[14] != 3204 {
		t.Errorf("P table %v (%v), want 600 .. 2988 at index 11 .. 3204", p, err)
	}
	e, err := tableFor(5, tables)
	if err != nil || e[4] != 2064 {
		t.Errorf("E table %v (%v), want a 2064 MHz top", e, err)
	}
	if _, ok := tables["voltage-states5"]; ok {
		t.Error("the non-sram table, whose frequencies are not frequencies, was kept")
	}
}

func TestTheRealSampleParses(t *testing.T) {
	ch, err := parseSample(fixture(t, "m1-sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch["PCPU"]) != 16 || len(ch["ECPU"]) != 6 || ch["PCPU"][0].Name != "IDLE" {
		t.Errorf("PCPU %d states, ECPU %d: %v", len(ch["PCPU"]), len(ch["ECPU"]), ch["PCPU"])
	}
}

// The reading the tool exists for: half the interval idle, the rest split between 2988 and
// 1500 MHz, reads 50% active at their tick-weighted mean.
func TestResidencyBecomesTheAverageActiveClock(t *testing.T) {
	var a, b [15]int64
	b[11], b[4] = 3_000_000, 9_000_000
	w := newWatcher(&fake{tables: fixture(t, "m1-tables.txt"), samples: []string{pSample(0, a), pSample(12_000_000, b)}}, nil)
	if r := w.next(); r.Status != "unknown" || !strings.Contains(r.Reason, "first sample") {
		t.Fatalf("first tick %+v: residency needs two samples", r)
	}
	r := w.next()
	want := (3*2988.0 + 9*1500.0) / 12
	if r.Status != "ok" || r.Core != "P" || math.Abs(r.MHz-want) > 0.01 || math.Abs(r.PActive-0.5) > 1e-9 {
		t.Errorf("got %+v, want P %.1f MHz at 50%% active", r, want)
	}
}

func TestThePClusterTemperatureIgnoresPlaceholders(t *testing.T) {
	c, ok := pClusterTemp(fixture(t, "m1-temps.txt") + "pACC MTR Temp Sensor99\t-21.60\npACC MTR Temp Sensor98\t400\nPMU tdie1\t99\n")
	if !ok || c < 20 || c > 60 {
		t.Errorf("P-cluster temperature %.1f (%v): placeholders or other sensors leaked in", c, ok)
	}
	if _, ok := pClusterTemp("PMU tdie1\t40\n"); ok {
		t.Error("no P-cluster sensor must be unknown, not another sensor's reading")
	}
}

// Every failure shape is an unknown reading, never an error, never a number.
func TestEveryFailureIsUnknown(t *testing.T) {
	tables := fixture(t, "m1-tables.txt")
	var z, one [15]int64
	one[11] = 1000
	ok := pSample(0, z)
	cases := map[string]*watcher{
		"framework missing":  newWatcher(nil, failure("libIOReport.dylib did not load")),
		"symbol missing":     newWatcher(nil, failure("a private symbol is missing")),
		"tables unreadable":  newWatcher(&fake{tErr: failure("no pmgr entry in the IORegistry"), samples: []string{ok, ok}}, nil),
		"tables unusable":    newWatcher(&fake{tables: "voltage-states5-sram\tzz\nvoltage-states5\t00000000\n", samples: []string{ok, ok}}, nil),
		"sample fails":       newWatcher(&fake{tables: tables, samples: []string{ok, ok}, sErr: []error{nil, failure("sample returned nothing")}}, nil),
		"sample unparseable": newWatcher(&fake{tables: tables, samples: []string{ok, "PCPU\tIDLE=lots\n"}}, nil),
		"sample empty":       newWatcher(&fake{tables: tables, samples: []string{ok, "\n"}}, nil),
		"no P channel":       newWatcher(&fake{tables: tables, samples: []string{"ECPU\tIDLE=1\tA=1\n", "ECPU\tIDLE=2\tA=2\n"}}, nil),
		"no table for the chip": newWatcher(&fake{tables: tables,
			samples: []string{"PCPU\tIDLE=0\tV0=0\tV1=0\n", "PCPU\tIDLE=5\tV0=5\tV1=5\n"}}, nil),
		"ambiguous tables": newWatcher(&fake{tables: tables + "voltage-states9-sram\t" + m1PTableTwin() + "\n",
			samples: []string{ok, pSample(10, one)}}, nil),
		"counter went backwards": newWatcher(&fake{tables: tables, samples: []string{pSample(100, one), pSample(50, one)}}, nil),
		"layout changed":         newWatcher(&fake{tables: tables, samples: []string{ok, "PCPU\tIDLE=5\tX=5\n"}}, nil),
		"no time elapsed":        newWatcher(&fake{tables: tables, samples: []string{ok, ok}}, nil),
	}
	for name, w := range cases {
		w.next()
		r := w.next()
		t.Logf("%-24s %s", name, r.Reason)
		if r.Status != "unknown" || r.Core != "?" || r.MHz != 0 || r.Reason == "" {
			t.Errorf("%s: %+v, want unknown with a reason", name, r)
		}
	}
}

// m1PTableTwin is a second, different, rising 15-entry table, so the P count matches two.
func m1PTableTwin() string {
	var b strings.Builder
	for i := 0; i < 15; i++ {
		hz := uint32(500+100*i) * 1_000_000
		fmt.Fprintf(&b, "%02x%02x%02x%02x00000000", byte(hz), byte(hz>>8), byte(hz>>16), byte(hz>>24))
	}
	return b.String()
}

// A failed sample must not leave a stale baseline behind: the next good sample has nothing
// valid to subtract from and is unknown, not a delta spanning the outage.
func TestAFailedSampleResetsTheBaseline(t *testing.T) {
	var a, b [15]int64
	b[11] = 1000
	w := newWatcher(&fake{tables: fixture(t, "m1-tables.txt"),
		samples: []string{pSample(0, a), "", pSample(10, b)}, sErr: []error{nil, failure("sample returned nothing")}}, nil)
	w.next()
	w.next()
	if r := w.next(); r.Status != "unknown" {
		t.Errorf("reading across a failed sample: %+v", r)
	}
}

// Temperature failing loses the temperature only; the clock reading stands.
func TestATemperatureFailureKeepsTheClock(t *testing.T) {
	var a, b [15]int64
	b[11] = 1000
	w := newWatcher(&fake{tables: fixture(t, "m1-tables.txt"), hErr: failure("a private symbol is missing"),
		samples: []string{pSample(0, a), pSample(1000, b)}}, nil)
	w.next()
	if r := w.next(); r.Status != "ok" || r.PTempC != nil || r.MHz != 2988 {
		t.Errorf("%+v, want the clock without a temperature", r)
	}
}
