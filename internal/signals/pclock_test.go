package signals

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// The testdata files are one real capture from the M1 Air (tools/clusterwatch -raw): the pmgr
// voltage-state tables and one cumulative IOReport sample.
func pclockFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakePClock is a source scripted per tick.
type fakePClock struct {
	tables  string
	tErr    error
	samples []string
	sErr    []error
	i       int
}

func (f *fakePClock) Tables() (string, error) { return f.tables, f.tErr }
func (f *fakePClock) Sample() (string, error) {
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

// m1P is a one-line P-cluster sample in the M1 layout: IDLE then fifteen DVFS states.
func m1P(idle int64, ticks [15]int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PCPU\tIDLE=%d", idle)
	for i, t := range ticks {
		fmt.Fprintf(&b, "\tV%dP%d=%d", i, 14-i, t)
	}
	return b.String() + "\n"
}

func TestPClockMapsTheM1TablesByCount(t *testing.T) {
	tables, err := parsePClockTables(pclockFixture(t, "pclock-m1-tables.txt"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := tableFor(15, tables)
	if err != nil || p[0] != 600 || p[11] != 2988 || p[14] != 3204 {
		t.Errorf("P table %v (%v), want 600 .. 2988 at index 11 .. 3204", p, err)
	}
	if _, ok := tables["voltage-states5"]; ok {
		t.Error("the non-sram table, whose frequencies are not frequencies, was kept")
	}
	if ch, err := parsePClockSample(pclockFixture(t, "pclock-m1-sample.txt")); err != nil || len(ch["PCPU"]) != 16 {
		t.Errorf("real sample: %d PCPU states (%v)", len(ch["PCPU"]), err)
	}
}

// Half the interval idle, the rest split 1:3 between 2988 and 1500 MHz, is 50% active at the
// tick-weighted mean: the reading the cut is built on.
func TestPClockIsTheResidencyWeightedClock(t *testing.T) {
	var a, b [15]int64
	b[11], b[4] = 3_000_000, 9_000_000
	p := newPClock(&fakePClock{tables: pclockFixture(t, "pclock-m1-tables.txt"), samples: []string{m1P(0, a), m1P(12_000_000, b)}}, nil)
	if r := p.next(); r.Known || !strings.Contains(r.Reason, "first sample") {
		t.Fatalf("first reading %+v: residency needs two samples", r)
	}
	r := p.next()
	want := (3*2988.0 + 9*1500.0) / 12
	if !r.Known || math.Abs(r.MHz-want) > 0.01 || math.Abs(r.Active-0.5) > 1e-9 || r.TopMHz != 3204 || math.Abs(r.Frac-want/3204) > 1e-9 {
		t.Errorf("got %+v, want %.1f MHz at 50%% active", r, want)
	}
}

// Every failure shape is unknown with its own reason: never an error, never a number.
func TestPClockEveryFailureIsUnknown(t *testing.T) {
	tables := pclockFixture(t, "pclock-m1-tables.txt")
	var z, one [15]int64
	one[11] = 1000
	ok := m1P(0, z)
	twin := func() string { // a second, different, rising 15-entry table
		var b strings.Builder
		for i := 0; i < 15; i++ {
			hz := uint32(500+100*i) * 1_000_000
			fmt.Fprintf(&b, "%02x%02x%02x%02x00000000", byte(hz), byte(hz>>8), byte(hz>>16), byte(hz>>24))
		}
		return b.String()
	}
	cases := map[string]struct {
		p    *pclock
		want string
	}{
		"framework missing":      {newPClock(nil, pclockFailure("libIOReport.dylib did not load")), "did not load"},
		"symbol missing":         {newPClock(nil, pclockFailure("an IOReport symbol is missing")), "symbol"},
		"platform without it":    {newPClock(nil, pclockFailure("no IOReport on this platform")), "platform"},
		"tables unreadable":      {newPClock(&fakePClock{tErr: pclockFailure("no pmgr entry in the IORegistry"), samples: []string{ok, ok}}, nil), "pmgr"},
		"tables unusable":        {newPClock(&fakePClock{tables: "voltage-states5-sram\tzz\nvoltage-states5\t00000000\n", samples: []string{ok, ok}}, nil), "no usable"},
		"sample fails":           {newPClock(&fakePClock{tables: tables, samples: []string{ok, ok}, sErr: []error{nil, pclockFailure("IOReport sample returned nothing")}}, nil), "returned nothing"},
		"sample unparseable":     {newPClock(&fakePClock{tables: tables, samples: []string{ok, "PCPU\tIDLE=lots\n"}}, nil), "unparseable"},
		"sample empty":           {newPClock(&fakePClock{tables: tables, samples: []string{ok, "\n"}}, nil), "no channels"},
		"no P channel":           {newPClock(&fakePClock{tables: tables, samples: []string{"ECPU\tIDLE=1\tA=1\n", "ECPU\tIDLE=2\tA=2\n"}}, nil), "no P-cluster"},
		"no table for the chip":  {newPClock(&fakePClock{tables: tables, samples: []string{"PCPU\tIDLE=0\tV0=0\tV1=0\n", "PCPU\tIDLE=5\tV0=5\tV1=5\n"}}, nil), "no voltage table"},
		"ambiguous tables":       {newPClock(&fakePClock{tables: tables + "voltage-states9-sram\t" + twin() + "\n", samples: []string{ok, m1P(10, one)}}, nil), "ambiguous"},
		"counter went backwards": {newPClock(&fakePClock{tables: tables, samples: []string{m1P(100, one), m1P(50, one)}}, nil), "backwards"},
		"layout changed":         {newPClock(&fakePClock{tables: tables, samples: []string{ok, "PCPU\tIDLE=5\tX=5\n"}}, nil), "layout"},
		"no time elapsed":        {newPClock(&fakePClock{tables: tables, samples: []string{ok, ok}}, nil), "no ticks"},
	}
	for name, c := range cases {
		c.p.next()
		r := c.p.next()
		if r.Known || r.MHz != 0 || r.Active != 0 || r.Frac != 0 || !strings.Contains(r.Reason, c.want) {
			t.Errorf("%s: %+v, want unknown because %q", name, r, c.want)
		}
	}
}

// A failed sample must not leave a stale baseline: the next good one has nothing valid to
// subtract from, rather than a delta spanning the outage.
func TestPClockAFailedSampleResetsTheBaseline(t *testing.T) {
	var a, b [15]int64
	b[11] = 1000
	p := newPClock(&fakePClock{tables: pclockFixture(t, "pclock-m1-tables.txt"),
		samples: []string{m1P(0, a), "", m1P(10, b)}, sErr: []error{nil, pclockFailure("IOReport sample returned nothing")}}, nil)
	p.next()
	p.next()
	if r := p.next(); r.Known {
		t.Errorf("reading across a failed sample: %+v", r)
	}
}

// The real sampler, on whatever host runs the suite. On a Mac with IOReport the second sample
// is a known reading in physical range; on Linux, in a CGO_ENABLED=0 build, or in a macOS VM
// without the framework, it is unknown WITH a reason. Either is correct; a known reading out
// of range, or an unknown one with no reason, is not.
func TestThePClockReadingIsEitherRealOrExplained(t *testing.T) {
	s := NewSampler()
	ctx := context.Background()
	if _, err := s.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pc := got.PClock
	t.Logf("P-cluster: %+v", pc)
	switch {
	case pc.Known && (pc.Active < 0 || pc.Active > 1 || (pc.Active > 0 && (pc.MHz < 100 || pc.MHz > 10000))):
		t.Errorf("known reading out of physical range: %+v", pc)
	case !pc.Known && (pc.Reason == "" || pc.MHz != 0):
		t.Errorf("unknown reading without a reason, or with a number: %+v", pc)
	}
}
