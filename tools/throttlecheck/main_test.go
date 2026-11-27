package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/pm-m1.txt is a real capture from the M1 Air this story measures, taken 2026-11-24
// with `sudo powermetrics -i 1000 -n 3 --samplers cpu_power,thermal,tasks` on macOS 25E253.
// Only the other processes' rows were stripped from the tasks table; every other line,
// including the per-CPU frequency lines a loose pattern could mistake for the cluster's, is
// verbatim. The machine was loaded at the time, which is why the P-cluster is busy and short of
// its 3204 MHz top bin: the reference is measured per run for exactly that reason.
func pmFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/pm-m1.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParsesEveryFieldTheVerdictReads(t *testing.T) {
	p, err := parsePowermetrics(strings.NewReader(pmFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Samples) != 3 || p.UnparsedHeaders != 0 || p.MultiplePCluster {
		t.Fatalf("got %d samples, %d unparsed headers, multi-P %v; want 3, 0, false",
			len(p.Samples), p.UnparsedHeaders, p.MultiplePCluster)
	}
	want := []struct {
		at              time.Time
		p, res, e, kern float64
	}{
		{time.Date(2026, 11, 24, 21, 41, 3, 0, time.UTC), 3083, 98.46, 1469, 494.24},
		{time.Date(2026, 11, 24, 21, 41, 4, 0, time.UTC), 3085, 99.62, 1543, 691.03},
		{time.Date(2026, 11, 24, 21, 41, 5, 0, time.UTC), 3116, -1, 1333, 204.44},
	}
	for i, w := range want {
		s := p.Samples[i]
		if !s.At.Equal(w.at) || s.PFreqMHz != w.p || s.EFreqMHz != w.e || s.KernelCPUms != w.kern || s.Pressure != "Nominal" {
			t.Errorf("interval %d misread: %+v", i, s)
		}
		if w.res >= 0 && s.PResidency != w.res {
			t.Errorf("interval %d residency %v, want %v", i, s.PResidency, w.res)
		}
		if !s.HasP || !s.HasPRes || !s.HasE || !s.HasKernel {
			t.Errorf("interval %d missing a field: %+v", i, s)
		}
	}
}

// The capture is all Nominal; the states the verdict reports alongside must parse too.
func TestPressureLevelsOtherThanNominalParse(t *testing.T) {
	in := strings.Replace(pmFixture(t), "Current pressure level: Nominal", "Current pressure level: Fair", 1)
	p, err := parsePowermetrics(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if p.Samples[0].Pressure != "Fair" || p.Samples[1].Pressure != "Nominal" {
		t.Errorf("pressure misread: %q, %q", p.Samples[0].Pressure, p.Samples[1].Pressure)
	}
}

// TestAnUnknownFormatIsAnErrorNotACoolMachine is the reason this tool can be trusted to say
// "does not throttle": a file it cannot read never reaches the verdict.
func TestAnUnknownFormatIsAnErrorNotACoolMachine(t *testing.T) {
	for name, in := range map[string]string{
		"empty":           "",
		"plist":           "<?xml version=\"1.0\"?><plist><dict><key>processor</key></dict></plist>",
		"renamed cluster": strings.ReplaceAll(pmFixture(t), "P-Cluster HW active frequency", "Performance cluster freq"),
	} {
		if _, err := parsePowermetrics(strings.NewReader(in)); err == nil {
			t.Errorf("%s: parsed without error; an unreadable file must not produce a verdict", name)
		}
	}
}

func busyRun(freqs []float64) []row {
	t0 := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	rows := make([]row, len(freqs))
	for i, f := range freqs {
		rows[i] = row{pmSample: pmSample{At: t0.Add(time.Duration(i) * time.Second), PFreqMHz: f, HasP: true, PResidency: 100, HasPRes: true}}
	}
	return rows
}

func TestASustainedDropIsAThrottleWithItsOnset(t *testing.T) {
	var f []float64
	for i := 0; i < 600; i++ {
		f = append(f, 3204)
	}
	for i := 0; i < 600; i++ {
		f = append(f, 2400)
	}
	s := analyse(busyRun(f), defaultThresholds, 3204)
	if !s.Throttled {
		t.Fatalf("not throttled: %s", s.Verdict)
	}
	// The drop starts at 600 s. The 60 s median crosses once 31 of its seconds are slow, so the
	// first confirming window ends at 630 s.
	if s.FirstBelow != 600*time.Second || s.Confirmed != 630*time.Second {
		t.Errorf("first below %v, confirmed %v; want 10m0s, 10m30s", s.FirstBelow, s.Confirmed)
	}
	if s.MinSustainedMHz != 2400 || s.ThrottledBusySeconds != 600 {
		t.Errorf("min sustained %v, throttled seconds %d", s.MinSustainedMHz, s.ThrottledBusySeconds)
	}
}

// TestAnEarlyRampKeepsItsPeakAsTheReference is GOV-B54 run A's shape: 2988 MHz for eight
// seconds, then a fall of about 32 MHz a second to a 1700 MHz floor. A reference measured over
// the opening minutes would itself be throttled and hide most of the drop.
func TestAnEarlyRampKeepsItsPeakAsTheReference(t *testing.T) {
	var f []float64
	for i := 0; i < 1800; i++ {
		v := 2988.0
		if i >= 8 {
			v = 2988 - 32*float64(i-8)
		}
		if v < 1700 {
			v = 1700
		}
		f = append(f, v)
	}
	f[3] = 3204 // one stray top-bin interval must not become the reference
	s := analyse(busyRun(f), defaultThresholds, 3204)
	if s.RefMHz != 2988 {
		t.Errorf("reference %v, want the 2988 the load started at", s.RefMHz)
	}
	// 90% of 2988 is 2689.2, first crossed at 8 + (2988-2689.2)/32 = 17.3 s.
	if !s.Throttled || s.FirstBelow != 18*time.Second {
		t.Errorf("throttled %v, first below %v; want true, 18s (%s)", s.Throttled, s.FirstBelow, s.Verdict)
	}
	if s.MinSustainedMHz != 1700 {
		t.Errorf("min sustained %v, want 1700", s.MinSustainedMHz)
	}
}

func TestBriefDipsAreNotAThrottle(t *testing.T) {
	var f []float64
	for i := 0; i < 900; i++ {
		v := 3204.0
		if i%30 < 10 { // a third of every half-minute slow: never a majority of any minute
			v = 2000
		}
		f = append(f, v)
	}
	if s := analyse(busyRun(f), defaultThresholds, 3204); s.Throttled {
		t.Errorf("throttled on brief dips: %s", s.Verdict)
	}
}

// TestIdleClusterIsInconclusive: a low clock on an idle cluster is power management, not a
// throttle, and a run whose load never held the P-cores cannot answer the question either way.
func TestIdleClusterIsInconclusive(t *testing.T) {
	rows := busyRun(make([]float64, 900))
	for i := range rows {
		rows[i].PFreqMHz, rows[i].PResidency = 600, 3
	}
	s := analyse(rows, defaultThresholds, 3204)
	if s.Throttled || !strings.HasPrefix(s.Verdict, "INCONCLUSIVE") {
		t.Errorf("idle run gave %q", s.Verdict)
	}
}

func TestBudgetAtIsTheLastTransitionAtOrBefore(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	steps := []budgetStep{{t0, 3}, {t0.Add(time.Minute), 1}}
	for _, c := range []struct {
		at   time.Duration
		want int
	}{{-time.Second, -1}, {0, 3}, {30 * time.Second, 3}, {time.Minute, 1}, {time.Hour, 1}} {
		if got := budgetAt(steps, t0.Add(c.at)); got != c.want {
			t.Errorf("budgetAt(+%v) = %d, want %d", c.at, got, c.want)
		}
	}
}

func TestBudgetLinesParse(t *testing.T) {
	m := reBudget.FindStringSubmatch("2026-11-24T14:29:57-05:00 budget cut 3->1 cause=run-queue rq=56.96 cut=5.07 ceiling=3")
	if m == nil || m[3] != "1" {
		t.Fatalf("budget line not parsed: %v", m)
	}
	if reBudget.MatchString("2026-11-24T14:42:09-05:00 budget release session=x kind=agent") {
		t.Error("a release line is not a budget transition")
	}
}
