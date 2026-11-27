// Command throttlecheck settles whether the chip throttled during one load run (GOV-B54).
//
// GOV-13 drove ten minutes of saturating load and thermalState never left nominal; GOV-03's
// trace shows it reaching fair under longer load. Neither says what the CPU clock did, and the
// clock is the thing that matters: a throttled P-cluster delivers less work per slot whatever
// the thermal API reports. This lines up three records of the same run by timestamp:
//
//   - powermetrics text output, run BY HAND with sudo as a one-off instrument. It is never a
//     dependency of the product (project rule: no sudo, ever); this tool only reads its file.
//   - samples.jsonl, the governor's own 1 Hz sampler (cpu, loadavg1, runqueue, thermal).
//   - daemon.log, for the budget the daemon was holding at each second.
//
// and prints one summary per run, optionally with the per-second table behind it:
//
//	go run ./tools/throttlecheck -pm ~/gov-b54/pm-A-saturate.txt -marks ~/gov-b54/marks.txt -run A
//
// What "throttled" means here is written down once, in verdict(), so the number that ends up in
// design.md section 5.3 is reproducible from the raw files rather than from a reading of them.
//
// A powermetrics file in which no P-cluster frequency line parses is an ERROR, not a run that
// did not throttle. A format this tool does not recognise would otherwise look exactly like a
// cool machine, which is the one mistake this story exists to avoid.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pmSample is one powermetrics interval. Fields that did not appear in the interval are NaN-free
// by construction: the Has* flags say whether they were read.
type pmSample struct {
	At          time.Time
	PFreqMHz    float64 // P-Cluster HW active frequency
	PResidency  float64 // P-Cluster HW active residency, percent
	EFreqMHz    float64
	HasP        bool
	HasPRes     bool
	HasE        bool
	Pressure    string  // thermal sampler: Nominal, Fair, Serious, Critical, ...
	KernelCPUms float64 // kernel_task CPU ms/s from the tasks sampler
	HasKernel   bool
}

var (
	reHeader   = regexp.MustCompile(`^\*\*\* Sampled system activity \((.+?)\) \(([\d.]+)ms elapsed\) \*\*\*`)
	rePFreq    = regexp.MustCompile(`^P\d*-Cluster HW active frequency:\s*([\d.]+)\s*MHz`)
	rePRes     = regexp.MustCompile(`^P\d*-Cluster HW active residency:\s*([\d.]+)%`)
	reEFreq    = regexp.MustCompile(`^E-Cluster HW active frequency:\s*([\d.]+)\s*MHz`)
	rePressure = regexp.MustCompile(`^Current pressure level:\s*(\S+)`)
	reKernel   = regexp.MustCompile(`^kernel_task\s+0\s+([\d.]+)`)
)

// pmTimeLayouts are the forms powermetrics has been seen to print its sample header in. The
// first is the documented one; the rest are tolerated so a locale or release difference fails
// as "unparsed header" in the count below rather than silently shifting every timestamp.
var pmTimeLayouts = []string{
	"Mon Jan _2 15:04:05 2006 -0700",
	"Mon Jan 2 15:04:05 2006 -0700",
	"Mon Jan _2 15:04:05 2006 MST",
	time.UnixDate, // date(1)'s own default form
}

type pmParse struct {
	Samples          []pmSample
	UnparsedHeaders  int
	MultiplePCluster bool // an M1 Pro/Max has P0 and P1; this tool reads the first only
}

func parsePowermetrics(r io.Reader) (pmParse, error) {
	var out pmParse
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur *pmSample
	flush := func() {
		if cur != nil {
			out.Samples = append(out.Samples, *cur)
			cur = nil
		}
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := reHeader.FindStringSubmatch(line); m != nil {
			flush()
			at, ok := parsePMTime(m[1])
			if !ok {
				out.UnparsedHeaders++
				continue // drop this interval's lines: they cannot be placed in time
			}
			cur = &pmSample{At: at}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case rePFreq.MatchString(line):
			if cur.HasP {
				out.MultiplePCluster = true
				continue
			}
			cur.PFreqMHz, cur.HasP = num(rePFreq, line)
		case rePRes.MatchString(line):
			if !cur.HasPRes {
				cur.PResidency, cur.HasPRes = num(rePRes, line)
			}
		case reEFreq.MatchString(line):
			cur.EFreqMHz, cur.HasE = num(reEFreq, line)
		case rePressure.MatchString(line):
			cur.Pressure = rePressure.FindStringSubmatch(line)[1]
		case reKernel.MatchString(line):
			cur.KernelCPUms, cur.HasKernel = num(reKernel, line)
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return out, err
	}
	withP := 0
	for _, s := range out.Samples {
		if s.HasP {
			withP++
		}
	}
	if withP == 0 {
		return out, fmt.Errorf("no P-cluster frequency parsed from %d interval(s) (%d unparsed headers): "+
			"the powermetrics format is not the one this tool knows, and that must not read as 'no throttle'",
			len(out.Samples), out.UnparsedHeaders)
	}
	return out, nil
}

func parsePMTime(s string) (time.Time, bool) {
	for _, l := range pmTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func num(re *regexp.Regexp, line string) (float64, bool) {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	return f, err == nil
}

// govSample is the subset of a samples.jsonl "sample" record this tool reads.
type govSample struct {
	At       time.Time
	CPU      float64
	LoadAvg1 float64
	RunQueue float64
	Thermal  string
}

func readGovSamples(path string, from, to time.Time) ([]govSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []govSample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec struct {
			TS     time.Time `json:"ts"`
			Kind   string    `json:"kind"`
			Sample *struct {
				CPU      float64 `json:"cpu"`
				LoadAvg1 float64 `json:"loadavg1"`
				RunQueue float64 `json:"runqueue"`
				Thermal  string  `json:"thermal"`
			} `json:"sample"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Kind != "sample" || rec.Sample == nil {
			continue
		}
		if rec.TS.Before(from) || rec.TS.After(to) {
			continue
		}
		out = append(out, govSample{rec.TS, rec.Sample.CPU, rec.Sample.LoadAvg1, rec.Sample.RunQueue, rec.Sample.Thermal})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, sc.Err()
}

// budgetStep is one budget transition from daemon.log: "budget cut 3->1 cause=...".
type budgetStep struct {
	At time.Time
	B  int
}

var reBudget = regexp.MustCompile(`^(\S+) budget \S+ (\d+)->(\d+)`)

func readBudget(path string) ([]budgetStep, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []budgetStep
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		m := reBudget.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, m[1])
		if err != nil {
			continue
		}
		b, _ := strconv.Atoi(m[3])
		out = append(out, budgetStep{at, b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, sc.Err()
}

// budgetAt is the budget in effect at t, or -1 when no transition precedes it.
func budgetAt(steps []budgetStep, t time.Time) int {
	i := sort.Search(len(steps), func(i int) bool { return steps[i].At.After(t) })
	if i == 0 {
		return -1
	}
	return steps[i-1].B
}

// nearestGov returns the governor sample closest to t, if one lies within maxGap.
func nearestGov(gs []govSample, t time.Time, maxGap time.Duration) (govSample, bool) {
	i := sort.Search(len(gs), func(i int) bool { return !gs[i].At.Before(t) })
	best, ok := govSample{}, false
	bestD := maxGap + 1
	for _, j := range []int{i - 1, i} {
		if j < 0 || j >= len(gs) {
			continue
		}
		d := gs[j].At.Sub(t)
		if d < 0 {
			d = -d
		}
		if d <= maxGap && d < bestD {
			best, ok, bestD = gs[j], true, d
		}
	}
	return best, ok
}

// row is one second of the joined run.
type row struct {
	pmSample
	Gov    govSample
	HasGov bool
	Budget int
}

// thresholds are the definition of a throttle. They live in one place so the verdict printed
// and the verdict recorded in design.md cannot drift apart.
type thresholds struct {
	BusyResidency float64       // P-cluster active residency at or above this counts as busy
	DropFraction  float64       // busy frequency below this fraction of the reference is a drop
	Sustain       time.Duration // a drop must last this long, in the rolling median, to count
	// The reference is the highest RefSmooth-second rolling median of busy seconds inside the
	// opening RefWindow: the clock the load ran at before heat had any say.
	//
	// It is the PEAK and not a percentile of the window because GOV-B54's first run started
	// falling eight seconds in and was 10% down by eighteen. A p90 over the opening two minutes
	// was already a throttled number (2773 against an observed 2988), which understated the
	// drop and put the onset at zero. The short median keeps one stray fast interval from
	// becoming the reference.
	RefWindow time.Duration
	RefSmooth time.Duration
}

var defaultThresholds = thresholds{
	BusyResidency: 95,
	DropFraction:  0.90,
	Sustain:       60 * time.Second,
	RefWindow:     120 * time.Second,
	RefSmooth:     5 * time.Second,
}

type summary struct {
	Intervals, Busy         int
	RefMHz, NominalMaxMHz   float64
	MinSustainedMHz         float64       // lowest rolling-Sustain median over busy seconds
	FirstBelow              time.Duration // first busy second under the cut in the confirming window
	Confirmed               time.Duration // end of the first Sustain window whose median is under the cut
	Throttled               bool
	ThrottledBusySeconds    int
	Pressure                map[string]int
	GovThermal              map[string]int
	KernelEarly, KernelLate float64 // kernel_task ms/s, mean over first and last fifth of busy seconds
	LoadEarly, LoadLate     float64
	FreqEarly, FreqLate     float64
	BudgetSeen              map[int]int
	GovMissing              int
	Verdict                 string
}

func analyse(rows []row, th thresholds, nominalMax float64) summary {
	s := summary{
		Intervals:     len(rows),
		NominalMaxMHz: nominalMax,
		Pressure:      map[string]int{},
		GovThermal:    map[string]int{},
		BudgetSeen:    map[int]int{},
	}
	var busy []row
	for _, r := range rows {
		if r.Pressure != "" {
			s.Pressure[r.Pressure]++
		}
		if r.HasGov {
			s.GovThermal[r.Gov.Thermal]++
		} else {
			s.GovMissing++
		}
		s.BudgetSeen[r.Budget]++
		if r.HasP && r.HasPRes && r.PResidency >= th.BusyResidency {
			busy = append(busy, r)
		}
	}
	s.Busy = len(busy)
	if len(busy) < int(th.RefWindow/time.Second) {
		s.Verdict = fmt.Sprintf("INCONCLUSIVE: %d busy P-cluster seconds of %d, need at least %d; the run was too short or its load did not hold the P-cores",
			len(busy), len(rows), int(th.RefWindow/time.Second))
		return s
	}

	start := busy[0].At
	k := int(th.RefSmooth / time.Second)
	for i := 0; i+k <= len(busy) && busy[i+k-1].At.Sub(start) <= th.RefWindow; i++ {
		win := make([]float64, 0, k)
		for _, r := range busy[i : i+k] {
			win = append(win, r.PFreqMHz)
		}
		if med := pct(win, 0.5); med > s.RefMHz {
			s.RefMHz = med
		}
	}
	cut := th.DropFraction * s.RefMHz

	// Rolling median over the Sustain window, in busy seconds. A single slow interval is noise;
	// a minute of them is the clock.
	w := int(th.Sustain / time.Second)
	s.MinSustainedMHz = s.RefMHz
	for i := 0; i+w <= len(busy); i++ {
		win := make([]float64, 0, w)
		for _, r := range busy[i : i+w] {
			win = append(win, r.PFreqMHz)
		}
		med := pct(win, 0.5)
		if med < s.MinSustainedMHz {
			s.MinSustainedMHz = med
		}
		if med < cut && !s.Throttled {
			s.Throttled = true
			s.Confirmed = busy[i+w-1].At.Sub(start)
			for _, r := range busy[i : i+w] {
				if r.PFreqMHz < cut {
					s.FirstBelow = r.At.Sub(start)
					break
				}
			}
		}
	}
	for _, r := range busy {
		if r.PFreqMHz < cut {
			s.ThrottledBusySeconds++
		}
	}

	fifth := len(busy) / 5
	if fifth == 0 {
		fifth = 1
	}
	s.FreqEarly, s.FreqLate = meanOf(busy[:fifth], freqOf), meanOf(busy[len(busy)-fifth:], freqOf)
	s.KernelEarly, s.KernelLate = meanOf(busy[:fifth], kernelOf), meanOf(busy[len(busy)-fifth:], kernelOf)
	s.LoadEarly, s.LoadLate = meanOf(busy[:fifth], loadOf), meanOf(busy[len(busy)-fifth:], loadOf)

	if s.Throttled {
		s.Verdict = fmt.Sprintf("THROTTLES: busy P-cluster under %.0f MHz (%.0f%% of %.0f) from %s, confirmed by %s, bottoming at %.0f MHz (%.0f%% of the reference)",
			cut, th.DropFraction*100, s.RefMHz, s.FirstBelow.Round(time.Second), s.Confirmed.Round(time.Second),
			s.MinSustainedMHz, 100*s.MinSustainedMHz/s.RefMHz)
	} else {
		s.Verdict = fmt.Sprintf("DOES NOT THROTTLE: busy P-cluster median never fell below %.0f MHz (%.0f%% of %.0f) over %d busy seconds",
			cut, th.DropFraction*100, s.RefMHz, len(busy))
	}
	return s
}

func freqOf(r row) (float64, bool)   { return r.PFreqMHz, r.HasP }
func kernelOf(r row) (float64, bool) { return r.KernelCPUms, r.HasKernel }
func loadOf(r row) (float64, bool)   { return r.Gov.LoadAvg1, r.HasGov }

func meanOf(rs []row, f func(row) (float64, bool)) float64 {
	sum, n := 0.0, 0
	for _, r := range rs {
		if v, ok := f(r); ok {
			sum += v
			n++
		}
	}
	if n == 0 {
		return -1
	}
	return sum / float64(n)
}

// pct is the nearest-rank percentile of a copy of a; 0 of nothing.
func pct(a []float64, p float64) float64 {
	if len(a) == 0 {
		return 0
	}
	c := append([]float64(nil), a...)
	sort.Float64s(c)
	i := int(p*float64(len(c)-1) + 0.5)
	return c[i]
}

func readMarks(path, run string) (from, to time.Time, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return from, to, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != run {
			continue
		}
		t, perr := time.Parse(time.RFC3339, f[2])
		if perr != nil {
			return from, to, fmt.Errorf("marks: %q: %v", line, perr)
		}
		switch f[1] {
		case "start":
			from = t
		case "end":
			to = t
		}
	}
	if from.IsZero() || to.IsZero() {
		return from, to, fmt.Errorf("marks: run %q needs both a start and an end line", run)
	}
	return from, to, nil
}

func main() {
	home, _ := os.UserHomeDir()
	state := filepath.Join(home, ".claude", "governor")
	pm := flag.String("pm", "", "powermetrics text output for the run (required)")
	samples := flag.String("samples", filepath.Join(state, "samples.jsonl"), "governor samples.jsonl")
	samples1 := flag.String("samples1", filepath.Join(state, "samples.jsonl.1"), "rotated samples file, read too if present")
	daemon := flag.String("daemon", filepath.Join(state, "daemon.log"), "governor daemon.log")
	marks := flag.String("marks", "", `file of "<run> start|end <RFC3339>" lines`)
	run := flag.String("run", "", "run label in -marks")
	nominal := flag.Float64("pmax", 3204, "the P-cluster's nominal maximum in MHz (M1: 3204), reported alongside the measured reference")
	tsv := flag.Bool("tsv", false, "also print the per-second joined table")
	probe := flag.String("probe", "", "tools/clockprobe output for the same run, to score against powermetrics")
	flag.Parse()
	if err := mainErr(*pm, *samples, *samples1, *daemon, *marks, *run, *probe, *nominal, *tsv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "throttlecheck:", err)
		os.Exit(1)
	}
}

func mainErr(pmPath, samplesPath, samples1Path, daemonPath, marksPath, run, probePath string, nominal float64, tsv bool, w io.Writer) error {
	if pmPath == "" {
		return errors.New("-pm is required")
	}
	f, err := os.Open(pmPath)
	if err != nil {
		return err
	}
	parsed, err := parsePowermetrics(f)
	f.Close()
	if err != nil {
		return err
	}

	from, to := parsed.Samples[0].At, parsed.Samples[len(parsed.Samples)-1].At
	if marksPath != "" {
		if from, to, err = readMarks(marksPath, run); err != nil {
			return err
		}
	}

	var gov []govSample
	for _, p := range []string{samples1Path, samplesPath} {
		g, err := readGovSamples(p, from.Add(-5*time.Second), to.Add(5*time.Second))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		gov = append(gov, g...)
	}
	sort.Slice(gov, func(i, j int) bool { return gov[i].At.Before(gov[j].At) })
	steps, err := readBudget(daemonPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var rows []row
	for _, p := range parsed.Samples {
		if p.At.Before(from) || p.At.After(to) {
			continue
		}
		g, ok := nearestGov(gov, p.At, 1500*time.Millisecond)
		rows = append(rows, row{pmSample: p, Gov: g, HasGov: ok, Budget: budgetAt(steps, p.At)})
	}
	if len(rows) == 0 {
		return fmt.Errorf("no powermetrics interval falls inside %s .. %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	if tsv {
		fmt.Fprintln(w, "at\tp_mhz\tp_res\te_mhz\tpressure\tkernel_ms\tcpu\tloadavg1\trunqueue\tthermal\tbudget")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%.0f\t%.1f\t%.0f\t%s\t%.1f\t%.3f\t%.2f\t%.2f\t%s\t%d\n",
				r.At.UTC().Format(time.RFC3339), r.PFreqMHz, r.PResidency, r.EFreqMHz, r.Pressure,
				r.KernelCPUms, r.Gov.CPU, r.Gov.LoadAvg1, r.Gov.RunQueue, r.Gov.Thermal, r.Budget)
		}
		fmt.Fprintln(w)
	}

	s := analyse(rows, defaultThresholds, nominal)
	printSummary(w, s, from, to, parsed)
	if probePath != "" {
		probes, err := readProbe(probePath, from, to)
		if err != nil {
			return err
		}
		printProbe(w, scoreProbe(rows, probes, defaultThresholds), s)
	}
	return nil
}

func printSummary(w io.Writer, s summary, from, to time.Time, p pmParse) {
	fmt.Fprintf(w, "window          %s .. %s (%s)\n", from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), to.Sub(from).Round(time.Second))
	fmt.Fprintf(w, "intervals       %d powermetrics, %d busy P-cluster (residency >= %.0f%%), %d without a governor sample\n",
		s.Intervals, s.Busy, defaultThresholds.BusyResidency, s.GovMissing)
	if p.UnparsedHeaders > 0 {
		fmt.Fprintf(w, "WARNING         %d powermetrics headers did not parse and were dropped\n", p.UnparsedHeaders)
	}
	if p.MultiplePCluster {
		fmt.Fprintln(w, "WARNING         more than one P-cluster per interval; only the first was read")
	}
	fmt.Fprintf(w, "P-cluster       reference %.0f MHz (nominal max %.0f), lowest sustained %.0f MHz, %d busy seconds under the cut\n",
		s.RefMHz, s.NominalMaxMHz, s.MinSustainedMHz, s.ThrottledBusySeconds)
	fmt.Fprintf(w, "first vs last   P %.0f -> %.0f MHz   kernel_task %.1f -> %.1f ms/s   loadavg1 %.1f -> %.1f\n",
		s.FreqEarly, s.FreqLate, s.KernelEarly, s.KernelLate, s.LoadEarly, s.LoadLate)
	fmt.Fprintf(w, "pressure (pm)   %s\n", counts(s.Pressure))
	fmt.Fprintf(w, "thermal (gov)   %s\n", counts(s.GovThermal))
	budgets := map[string]int{}
	for b, n := range s.BudgetSeen {
		k := strconv.Itoa(b)
		if b < 0 {
			k = "unknown"
		}
		budgets[k] = n
	}
	fmt.Fprintf(w, "budget          %s\n", counts(budgets))
	fmt.Fprintf(w, "verdict         %s\n", s.Verdict)
}

func counts(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}
