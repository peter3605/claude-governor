// Command buildstats reads a GOV-17 window out of the trace log and reports the figures
// docs/advice.md's reading rule is scored against.
//
// It exists because the figures were originally documented as jq and grep one-liners, and one of
// them was wrong in a way that inverted a clause. The clause-3 count
// (`grep -E '(^|\s)(test|check|bench)$'`) returned 93 test-suite runs where the first four days
// held 144: it matched `go test` and silently missed all 51 `pytest` invocations. Clause 3 is
// the FIRST gate in that document's reading rule, so a query blind to a third of the evidence
// would have scored a go-test-only result as the whole answer. The categorisation now lives in
// internal/classify.ShapeRunKind, beside the vocabulary it has to agree with and under test.
//
// TWO DENOMINATOR CHOICES ARE MADE HERE AND BOTH MATTER MORE THAN THEY LOOK.
//
// AWAKE TIME, NOT WALL CLOCK. The sampler ticks at 1 Hz and writes unconditionally, so the
// count of sample records in a period IS the number of seconds the machine was awake in it.
// Rates are reported per awake hour. On a closed laptop the difference is not cosmetic: over
// 2026-09-17..21 four calendar days carried 9.5 awake hours, and two of those days recorded
// zero builds because the lid was shut — 566 and 430 sleep/wake events respectively. Reported
// per calendar day, those two days read as a session that stopped building.
//
// ACTIVE DAYS, NOT DAYS. A day with no builds tells you nothing about whether advice is
// landing, so the halves test splits the days that HAVE builds. advice.md pre-registered the
// rule this feeds: a change smaller than the gap between the two baseline halves is noise.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peter3605/claude-governor/internal/classify"
)

// record is the subset of internal/trace.Record this report reads. It is decoded loosely on
// purpose: the log is append-only across schema versions and a field this tool does not know
// about must never stop it reading the ones it does.
type record struct {
	TS    string `json:"ts"`
	Kind  string `json:"kind"`
	Build *struct {
		SessionID string   `json:"session_id"`
		Shapes    []string `json:"shapes"`
		Class     string   `json:"class"`
		Outcome   string   `json:"outcome"`
		WaitedMS  float64  `json:"waited_ms"`
		CWD       string   `json:"cwd"`
	} `json:"build"`
	Sample *struct {
		// ClaudeCPU is cores busy across every Claude Code session process AND its
		// transitive descendants — the quantity tools/childcpu decomposes live, and the
		// one clause 2 scores. Reading it here rather than in a scratch query is
		// deliberate: the retired clause-3 grep published a wrong count precisely
		// because the reading lived outside the tool that owns the vocabulary.
		ClaudeCPU float64 `json:"claude_cpu"`
	} `json:"sample"`
}

type day struct {
	awakeSec                            int
	granted, parked, denied, failedOpen int
	testRuns, ambiguous, notTest        int
	sessions                            map[string]bool
	// childCores holds one entry per awake second, so a percentile over it is a percentile
	// over the window itself rather than over the builds in it.
	childCores []float64
}

func (d *day) builds() int { return d.granted + d.parked + d.denied + d.failedOpen }

// ran counts builds that actually executed. granted, parked and failed-open all ran; denied did
// not. Merging failed-open into granted would hide the ungoverned ones, and merging it into
// denied would count a build that ran as one that was stopped.
func (d *day) ran() int { return d.granted + d.parked + d.failedOpen }

func main() {
	home, _ := os.UserHomeDir()
	path := flag.String("log", filepath.Join(home, ".claude", "governor", "samples.jsonl"),
		"trace log to read")
	since := flag.String("since", "", "first date to report, YYYY-MM-DD (default: first build)")
	until := flag.String("until", "", "last date to report, YYYY-MM-DD inclusive")
	flag.Parse()

	f, err := os.Open(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "buildstats: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	days := map[string]*day{}
	shapes := map[string]int{}
	repos := map[string]int{}
	var waits []float64
	get := func(d string) *day {
		if days[d] == nil {
			days[d] = &day{sessions: map[string]bool{}}
		}
		return days[d]
	}

	sc := bufio.NewScanner(f)
	// The log carries whole days of 1 Hz samples and grows to hundreds of megabytes, so the
	// buffer is raised once rather than failing on a long line somewhere in the middle.
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	var bad int
	for sc.Scan() {
		line := sc.Bytes()
		// Cheap prefilter: the overwhelming majority of lines are samples, and unmarshalling
		// every one of them to learn that costs more than the whole report.
		isBuild := strings.Contains(string(line), `"kind":"build"`)
		if !isBuild && !strings.Contains(string(line), `"kind":"sample"`) {
			continue
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			bad++
			continue
		}
		if len(r.TS) < 10 {
			bad++
			continue
		}
		d := r.TS[:10]
		if *since != "" && d < *since {
			continue
		}
		if *until != "" && d > *until {
			continue
		}
		switch r.Kind {
		case "sample":
			dd := get(d)
			dd.awakeSec++
			if r.Sample != nil {
				dd.childCores = append(dd.childCores, r.Sample.ClaudeCPU)
			}
		case "build":
			if r.Build == nil {
				bad++
				continue
			}
			dd := get(d)
			switch r.Build.Outcome {
			case "granted":
				dd.granted++
			case "parked":
				dd.parked++
				waits = append(waits, r.Build.WaitedMS)
			case "denied":
				dd.denied++
			case "failed-open":
				dd.failedOpen++
			default:
				bad++
			}
			dd.sessions[r.Build.SessionID] = true
			if r.Build.CWD != "" {
				repos[filepath.Base(r.Build.CWD)]++
			}
			// Clause 3 counts suites that RAN. A denied build is not a test run that
			// disappeared, it is one the governor stopped, and counting it here would credit
			// the governor's own refusals as session behaviour.
			if r.Build.Outcome == "denied" {
				continue
			}
			for _, s := range r.Build.Shapes {
				shapes[s]++
				switch classify.ShapeRunKind(s) {
				case classify.TestRun:
					dd.testRuns++
				case classify.AmbiguousRun:
					dd.ambiguous++
				default:
					dd.notTest++
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "buildstats: read %s: %v\n", *path, err)
		os.Exit(1)
	}

	keys := make([]string, 0, len(days))
	for d := range days {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	// Trim leading days that carry no builds. Before the instrument shipped the log is all
	// samples, and reporting those as zero-build days would invent months of apparent silence.
	for len(keys) > 0 && days[keys[0]].builds() == 0 {
		keys = keys[1:]
	}
	if len(keys) == 0 {
		fmt.Println("no build records in", *path)
		fmt.Println("The instrument ships with GOV-17; a log older than that install has none.")
		return
	}

	fmt.Printf("%-12s %8s %7s %7s %8s %7s %7s %7s %8s %7s %7s\n",
		"day", "awake_h", "builds", "b/h", "granted", "parked", "denied", "f-open",
		"testrun", "ambig", "other")
	var tot day
	for _, k := range keys {
		d := days[k]
		printRow(k, d)
		tot.awakeSec += d.awakeSec
		tot.granted += d.granted
		tot.parked += d.parked
		tot.denied += d.denied
		tot.failedOpen += d.failedOpen
		tot.testRuns += d.testRuns
		tot.ambiguous += d.ambiguous
		tot.notTest += d.notTest
	}
	fmt.Println(strings.Repeat("-", 96))
	printRow("TOTAL", &tot)
	fmt.Println()

	awakeH := float64(tot.awakeSec) / 3600
	fmt.Printf("awake time            %.2f h over %d day(s) with builds, from %s to %s\n",
		awakeH, countActive(days, keys), keys[0], keys[len(keys)-1])
	fmt.Printf("builds                %d, of which %d ran and %d were denied\n",
		tot.builds(), tot.ran(), tot.denied)
	if awakeH > 0 {
		fmt.Printf("binding events        %d (parked+denied) = %.2f per awake hour\n",
			tot.parked+tot.denied, float64(tot.parked+tot.denied)/awakeH)
		fmt.Printf("denials               %d = %.2f per awake hour\n",
			tot.denied, float64(tot.denied)/awakeH)
	}
	fmt.Printf("test-suite runs       %d  (ambiguous %d, other gated work %d)\n",
		tot.testRuns, tot.ambiguous, tot.notTest)
	if tot.failedOpen > 0 {
		fmt.Printf("FAILED OPEN           %d of %d builds (%.1f%%) ran ungoverned — advice.md "+
			"discards a window where this is a large share\n",
			tot.failedOpen, tot.builds(), 100*float64(tot.failedOpen)/float64(tot.builds()))
	}
	fmt.Println()

	fmt.Println("shapes that ran:")
	for _, s := range sortedByCount(shapes) {
		fmt.Printf("  %5d  %-18s %s\n", shapes[s], s, classify.ShapeRunKind(s))
	}
	fmt.Println()
	fmt.Println("repos:")
	for _, r := range sortedByCount(repos) {
		fmt.Printf("  %5d  %s\n", repos[r], r)
	}
	if len(waits) > 0 {
		sort.Float64s(waits)
		fmt.Printf("\npark waits (ms)       n=%d  min %.0f  median %.0f  max %.0f\n",
			len(waits), waits[0], waits[len(waits)/2], waits[len(waits)-1])
	}
	if bad > 0 {
		fmt.Printf("\n%d line(s) could not be read and are excluded.\n", bad)
	}

	childCoresReport(days, keys)
	halves(days, keys)
}

// pct returns the p'th percentile of a, which it sorts in place. p is a fraction.
func pct(a []float64, p float64) float64 {
	if len(a) == 0 {
		return 0
	}
	sort.Float64s(a)
	i := int(math.Round(float64(len(a)-1) * p))
	return a[i]
}

// childCoresReport prints clause 2's scored quantity per active day.
//
// It reports the whole upper tail rather than the peak alone because the peak is an extreme
// order statistic, not the "distribution statistic over every sampled second" advice.md
// reaches for when it elects to score it: one unlucky second moves the max and moves nothing
// else. The columns are here so the choice of scored statistic is made against its own
// measured stability rather than against the phrase.
func childCoresReport(days map[string]*day, keys []string) {
	var active []string
	for _, k := range keys {
		if days[k].builds() > 0 && len(days[k].childCores) > 0 {
			active = append(active, k)
		}
	}
	if len(active) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("child cores (clause 2's scored quantity — cores busy in Claude Code and its " +
		"descendants, one reading per awake second):")
	fmt.Printf("  %-12s %8s %7s %7s %7s %7s %7s\n",
		"day", "n", "p50", "p90", "p95", "p99", "max")
	for _, k := range active {
		c := days[k].childCores
		fmt.Printf("  %-12s %8d %7.2f %7.2f %7.2f %7.2f %7.2f\n",
			k, len(c), pct(c, .50), pct(c, .90), pct(c, .95), pct(c, .99), pct(c, 1))
	}
}

func printRow(label string, d *day) {
	awakeH := float64(d.awakeSec) / 3600
	bph := 0.0
	if awakeH > 0 {
		bph = float64(d.builds()) / awakeH
	}
	fmt.Printf("%-12s %8.2f %7d %7.1f %8d %7d %7d %7d %8d %7d %7d\n",
		label, awakeH, d.builds(), bph, d.granted, d.parked, d.denied, d.failedOpen,
		d.testRuns, d.ambiguous, d.notTest)
}

func countActive(days map[string]*day, keys []string) int {
	n := 0
	for _, k := range keys {
		if days[k].builds() > 0 {
			n++
		}
	}
	return n
}

func sortedByCount(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if m[ks[i]] != m[ks[j]] {
			return m[ks[i]] > m[ks[j]]
		}
		return ks[i] < ks[j]
	})
	return ks
}

// halves runs advice.md's pre-registered noise floor: split the window's ACTIVE days in two and
// compare those halves to each other before comparing anything to an after-window. A difference
// between the halves is the smallest change the window can distinguish from nothing.
func halves(days map[string]*day, keys []string) {
	var active []string
	for _, k := range keys {
		if days[k].builds() > 0 {
			active = append(active, k)
		}
	}
	fmt.Println()
	if len(active) < 2 {
		fmt.Printf("halves test           NOT POSSIBLE: %d active day(s). advice.md's noise "+
			"floor needs two halves, so this window cannot score a change.\n", len(active))
		return
	}
	mid := len(active) / 2
	a, b := agg(days, active[:mid]), agg(days, active[mid:])
	fmt.Println("halves test (advice.md's noise floor — the smallest change this window can see):")
	fmt.Printf("  first  %-24s %6.1f builds/h   %5.2f denials/h   %5.2f parks/h   %d test runs\n",
		span(active[:mid]), rate(a, a.builds()), rate(a, a.denied), rate(a, a.parked), a.testRuns)
	fmt.Printf("  second %-24s %6.1f builds/h   %5.2f denials/h   %5.2f parks/h   %d test runs\n",
		span(active[mid:]), rate(b, b.builds()), rate(b, b.denied), rate(b, b.parked), b.testRuns)
	r1, r2 := rate(a, a.builds()), rate(b, b.builds())
	if r1 > 0 && r2 > 0 {
		ratio := r2 / r1
		if ratio < 1 {
			ratio = 1 / ratio
		}
		verdict := "comparable"
		if ratio > 2 {
			verdict = "NOT COMPARABLE — advice.md discards a pair differing by more than ~2x"
		}
		fmt.Printf("  build-volume ratio between halves: %.2fx  (%s)\n", ratio, verdict)
	}
	if rate(a, a.denied) == 0 || rate(b, b.denied) == 0 {
		fmt.Println("  NOTE: one half has zero denials. Denials per hour cannot carry a " +
			"before/after comparison on this window — there is no stable baseline to beat.")
	}
	peakHalves(a, b)
}

// peakHalves applies the same pre-registered noise floor to clause 2's SCORED quantity.
//
// Clause 2 was amended on 2026-09-20 to score the peak because denials per hour failed this
// test. Nothing had yet run the test on the replacement, so this does. The gap between the two
// halves is the smallest change an after-window can be believed to show.
func peakHalves(a, b *day) {
	if len(a.childCores) == 0 || len(b.childCores) == 0 {
		return
	}
	fmt.Println("  the SCORED quantity's own noise floor (same test, applied to child cores):")
	for _, st := range []struct {
		name string
		p    float64
	}{{"p95", .95}, {"p99", .99}, {"max", 1}} {
		x, y := pct(a.childCores, st.p), pct(b.childCores, st.p)
		gap := math.Abs(x - y)
		mean := (x + y) / 2
		rel := 0.0
		if mean > 0 {
			rel = 100 * gap / mean
		}
		fmt.Printf("    %-4s %6.2f vs %6.2f cores   gap %5.2f (%5.1f%%) — an after-window "+
			"must beat this to be readable\n", st.name, x, y, gap, rel)
	}
	fmt.Println("    A window whose halves differ by more than the effect being looked for " +
		"cannot score that effect, whichever statistic is elected.")
}

func agg(days map[string]*day, keys []string) *day {
	t := &day{sessions: map[string]bool{}}
	for _, k := range keys {
		d := days[k]
		t.awakeSec += d.awakeSec
		t.granted += d.granted
		t.parked += d.parked
		t.denied += d.denied
		t.failedOpen += d.failedOpen
		t.testRuns += d.testRuns
		t.ambiguous += d.ambiguous
		t.notTest += d.notTest
		t.childCores = append(t.childCores, d.childCores...)
	}
	return t
}

func rate(d *day, n int) float64 {
	h := float64(d.awakeSec) / 3600
	if h == 0 {
		return 0
	}
	return float64(n) / h
}

func span(keys []string) string {
	if len(keys) == 1 {
		return keys[0]
	}
	return keys[0] + ".." + keys[len(keys)-1]
}
