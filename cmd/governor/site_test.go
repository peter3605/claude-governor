package main

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/calibrate"
	"github.com/peter3605/claude-governor/internal/classify"
	"github.com/peter3605/claude-governor/internal/signals"
)

// The landing page (GOV-42): site/index.html, published to GitHub Pages by pages.yml.
//
// The page is the THIRD place this project's measured numbers live, after docs/design.md and
// the README, and the first two have both drifted (GOV-B20, GOV-B21). So it is held the way
// GOV-36 holds the guide: every measured figure on it is wrapped in data-fig="<key>", and each
// key is recomputed here from its source of truth — the constants in internal/budget and
// internal/classify, the gate's own refusal and status renderers, the measurement records in
// docs/, and CHANGELOG.md for the version.
//
// Wrapping a figure is not optional. TestThePageHasNoUnsourcedFigure fails on any digit in the
// visible text that is neither inside a data-fig nor explicitly declared data-nofig, so a
// number pasted into the page without a source fails CI rather than waiting to go stale.

const pageURL = "https://peter3605.github.io/claude-governor/"

func sitePath(name string) string { return filepath.Join("..", "..", "site", name) }

func readSite(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(sitePath("index.html"))
	if err != nil {
		t.Fatalf("read site/index.html: %v", err)
	}
	return string(b)
}

func readRepo(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// pageBody is the page's <body> with its script removed: what a reader sees, as markup.
func pageBody(t *testing.T) string {
	t.Helper()
	page := readSite(t)
	i := strings.Index(page, "<body>")
	if i < 0 {
		t.Fatal("site/index.html has no <body>")
	}
	return regexp.MustCompile(`(?s)<script>.*?</script>`).ReplaceAllString(page[i:], "")
}

// visibleText strips markup and entities and collapses whitespace.
func visibleText(markup string) string {
	s := regexp.MustCompile(`(?s)<svg.*?</svg>`).ReplaceAllString(markup, " ")
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, "")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// figOpen matches the opening tag of an element carrying data-fig.
var figOpen = regexp.MustCompile(`<(span|code|div|strong|pre)\b[^>]*\bdata-fig="([^"]+)"[^>]*>`)

type pageFig struct{ key, text string }

// element is one element found by its opening tag: where it starts and ends in the markup,
// and its inner markup. The close is found by counting that tag name's depth, because a
// figure such as a <pre> holds coloured <span>s and RE2 has no back-references to pair them.
type element struct {
	start, end int
	key, inner string
}

func elements(t *testing.T, markup string, open *regexp.Regexp) []element {
	t.Helper()
	var out []element
	for _, m := range open.FindAllStringSubmatchIndex(markup, -1) {
		tag := markup[m[2]:m[3]]
		key := ""
		if len(m) > 5 && m[4] >= 0 {
			key = markup[m[4]:m[5]]
		}
		tok := regexp.MustCompile(`<(/?)` + tag + `\b[^>]*>`)
		depth, pos := 1, m[1]
		for depth > 0 {
			loc := tok.FindStringSubmatchIndex(markup[pos:])
			if loc == nil {
				t.Fatalf("<%s> at offset %d is never closed", tag, m[0])
			}
			if loc[3] > loc[2] {
				depth--
			} else {
				depth++
			}
			if depth > 0 {
				pos += loc[1]
				continue
			}
			out = append(out, element{m[0], pos + loc[1], key, markup[m[1] : pos+loc[0]]})
		}
	}
	return out
}

func pageFigures(t *testing.T, body string) []pageFig {
	t.Helper()
	var out []pageFig
	for _, e := range elements(t, body, figOpen) {
		out = append(out, pageFig{e.key, visibleText(e.inner)})
	}
	return out
}

// blank replaces the given elements, nested or not, with a space each.
func blank(markup string, els []element) string {
	cut := make([]bool, len(markup))
	for _, e := range els {
		for i := e.start; i < e.end; i++ {
			cut[i] = true
		}
	}
	var b strings.Builder
	for i := 0; i < len(markup); i++ {
		switch {
		case !cut[i]:
			b.WriteByte(markup[i])
		case i == 0 || !cut[i-1]:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// mdRow returns the cells after the label of the first markdown table row whose first cell
// is label, with bold markers dropped.
func mdRow(t *testing.T, doc, name, label string) []string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(strings.ReplaceAll(cells[i], "**", ""))
		}
		if len(cells) > 1 && cells[0] == label {
			return cells[1:]
		}
	}
	t.Fatalf("%s has no table row labelled %q; the page's source for it has moved", name, label)
	return nil
}

var numberRE = regexp.MustCompile(`\d+(?:\.\d+)?`)

func numbers(s string) []string { return numberRE.FindAllString(s, -1) }

func num(t *testing.T, s string) float64 {
	t.Helper()
	ns := numbers(s)
	if len(ns) == 0 {
		t.Fatalf("no number in %q", s)
	}
	f, err := strconv.ParseFloat(ns[0], 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func find(t *testing.T, doc, name, pattern string) []string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s no longer matches %q; the page's source for that figure has moved", name, pattern)
	}
	return m[1:]
}

// expected is what each figure must say. A value is either numeric — the page's figure
// must agree with it to the precision the page prints — or verbatim text.
type expected struct {
	nums []float64
	text string // verbatim, compared after whitespace is collapsed
	from string // where it comes from, for the failure message
	// version compares text as a version label: case and a trailing full stop are the
	// page's typography, not the figure.
	version bool
}

func n(from string, v ...float64) expected { return expected{nums: v, from: from} }

func siteFigures(t *testing.T) map[string]expected {
	t.Helper()
	proof := readRepo(t, "docs/control-proof.md")
	corpus := readRepo(t, "docs/bash-corpus.md")
	design := readRepo(t, "docs/design.md")
	changelog := readRepo(t, "CHANGELOG.md")

	const cp = "docs/control-proof.md"
	admitted := mdRow(t, proof, cp, "builds admitted")
	cpu := mdRow(t, proof, cp, "build-test CPU in the window")
	p50 := mdRow(t, proof, cp, "run queue per core, p50")
	p90 := mdRow(t, proof, cp, "run queue per core, p90")
	rqMax := mdRow(t, proof, cp, "run queue per core, max")
	peak := mdRow(t, proof, cp, "peak descendant cores")
	hostCores, _ := strconv.ParseFloat(find(t, proof, cp, `(\d+) logical cores`)[0], 64)
	date := mdRow(t, proof, cp, "Date")[0]
	window, _ := strconv.ParseFloat(find(t, proof, cp, `childcpu -for (\d+)s`)[0], 64)

	const bc = "docs/bash-corpus.md"
	pct := func(label string) float64 { return num(t, mdRow(t, corpus, bc, label)[0]) }

	const dz = "docs/design.md"
	atof := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	climb := find(t, design, dz, `(\d+) of the replay trace's (\d+) ticks`)
	thermalHours := atof(find(t, design, dz, `not once in (\d+) hours`)[0])
	classifyUS := atof(find(t, design, dz, `~(\d+) µs classifier`)[0])
	probeCores := atof(find(t, design, dz, `\*\*([\d.]+)\*\* on that machine`)[0])

	version := find(t, changelog, "CHANGELOG.md", `(?m)^## \[(\d+)\.(\d+)\.(\d+)\]`)

	ungCPU, govCPU := num(t, cpu[0]), num(t, cpu[1])
	ungMax, govMax := num(t, rqMax[0]), num(t, rqMax[1])

	cpuPtr := func(f float64) *float64 { return &f }
	var status strings.Builder
	printStatus(&status, siteStatus())

	// The probe is three phases per repetition (internal/calibrate/probe.go).
	probe := (time.Duration(calibrate.DefaultProbeRepetitions) * 3 * calibrate.DefaultProbeDuration).Seconds()

	slotLine := render(siteStatusView(), 0)

	return map[string]expected{
		"version": {text: "v" + strings.Join(version, "."), from: "CHANGELOG.md's newest release", version: true},

		"host-cores":   n(cp, hostCores),
		"proof-date":   n(cp, numsOf(t, date)...),
		"proof-window": n(cp, window),
		"ung-admitted": n(cp, numsOf(t, admitted[0])...),
		"gov-admitted": n(cp, numsOf(t, admitted[1])...),
		"ung-cpu":      n(cp, ungCPU),
		"gov-cpu":      n(cp, govCPU),
		"cpu-ratio":    n(cp+", ungoverned over governed build CPU", ungCPU/govCPU),
		"ung-rq-p50":   n(cp, num(t, p50[0])),
		"gov-rq-p50":   n(cp, num(t, p50[1])),
		"ung-rq-p90":   n(cp, num(t, p90[0])),
		"gov-rq-p90":   n(cp, num(t, p90[1])),
		"ung-rq-max":   n(cp, ungMax),
		"gov-rq-max":   n(cp, govMax),
		"rq-ratio":     n(cp+", ungoverned over governed run-queue max", ungMax/govMax),
		"rq-threads":   n(cp+", run-queue max per core times cores", ungMax*hostCores),
		"ung-peak":     n(cp, num(t, peak[0])),
		"gov-peak":     n(cp, num(t, peak[1])),

		"slot-cores":       n("budget.SlotCores", budget.SlotCores),
		"agent-cores":      n("budget.AgentCores", budget.AgentCores),
		"agent-weight":     n("budget.AgentWeight", budget.AgentWeight),
		"light-weight":     n("classify.Light in slots", budget.SlotsFor(classify.Light.ExpectedPeakCores())),
		"medium-weight":    n("classify.Medium in slots", budget.SlotsFor(classify.Medium.ExpectedPeakCores())),
		"heavy-weight":     n("classify.Heavy in slots", budget.SlotsFor(classify.Heavy.ExpectedPeakCores())),
		"agents-per-build": n("one Medium build over budget.AgentWeight", budget.SlotsFor(classify.Medium.ExpectedPeakCores())/budget.AgentWeight),

		"catch-rate":      n(bc, pct("Catch rate")),
		"false-positives": n(bc, pct("False positives")),
		"gated-share":     n(bc, pct("Gated share")),
		"cost-captured":   n(bc, pct("Cost captured")),

		"climb-ticks":   n(dz, atof(climb[0]), atof(climb[1])),
		"thermal-hours": n(dz, thermalHours),
		"classify-us":   n(dz, classifyUS),
		"probe-cores":   n(dz+" and "+cp, probeCores, hostCores),
		"probe-seconds": n("calibrate.DefaultProbeRepetitions x 3 phases x DefaultProbeDuration", probe),
		"gate-wait":     n("gateWaitMax", gateWaitMax.Seconds()),

		"status-example":     {text: status.String(), from: "printStatus"},
		"statusline-example": {text: slotLine, from: "render"},
		"statusline-slots":   {text: strings.SplitN(slotLine, " (", 2)[0], from: "render"},
		"ungoverned-line":    {text: render(statusView{OK: true, Ungoverned: true}, 0), from: "render"},
		"refusal-example": {text: denyReason(acquireResponse{
			Reason: "capacity", Budget: 6, Slots: 3, Asking: budget.SlotsFor(classify.Heavy.ExpectedPeakCores()),
			CPU: cpuPtr(0.78), Thermal: signals.Nominal, Denials: 1,
		}, intent{kind: "build"}), from: "denyReason"},
	}
}

func numsOf(t *testing.T, s string) []float64 {
	t.Helper()
	var v []float64
	for _, x := range numbers(s) {
		f, _ := strconv.ParseFloat(x, 64)
		v = append(v, f)
	}
	return v
}

// The state §5 of the page shows: a six-slot machine, three builds held, one of them this
// session's, on a cool laptop at 78% CPU.
func siteStatus() cachedStatus {
	cpu := 0.78
	return cachedStatus{At: time.Now(), Reply: statusResponse{
		V: protocolVersion, Budget: 6, InUse: 3, Slots: 3, CPU: &cpu, Thermal: signals.Nominal,
		Sessions: []sessionStat{
			{Session: "16c20e52-7a3f-4b1e-9d02-5e8c41a7b3f9", Weight: 1, Confirmed: 1, Registered: true},
			{Session: "9b04a7e1-c6d5-4f80-a1b3-2e7f90d4c815", Weight: 1, Confirmed: 2, Registered: true},
		},
	}}
}

func siteStatusView() statusView {
	cpu := 0.78
	return statusView{OK: true, InUse: 3, Slots: 3, Budget: 6, Mine: 1, CPU: &cpu, Thermal: signals.Nominal}
}

// agrees reports whether the page's printed number is the source's, to the precision printed:
// "4.8" agrees with 4.836, "53" with 53.0, "0.08" with 0.0817.
func agrees(printed string, v float64) bool {
	p, err := strconv.ParseFloat(printed, 64)
	if err != nil {
		return false
	}
	decimals := 0
	if i := strings.IndexByte(printed, '.'); i >= 0 {
		decimals = len(printed) - i - 1
	}
	return math.Abs(p-v) <= 0.5*math.Pow(10, -float64(decimals))+1e-9
}

// Every figure on the page says what its source of truth says.
func TestEveryFigureOnThePageMatchesItsSource(t *testing.T) {
	want := siteFigures(t)
	figs := pageFigures(t, pageBody(t))
	if len(figs) == 0 {
		t.Fatal("found no data-fig on the page; this test has stopped testing anything")
	}
	seen := map[string]bool{}
	for _, f := range figs {
		w, ok := want[f.key]
		if !ok {
			t.Errorf("the page carries data-fig=%q, which has no source in this test; add one or remove the figure", f.key)
			continue
		}
		seen[f.key] = true
		if w.nums == nil {
			got, exp := f.text, strings.Join(strings.Fields(w.text), " ")
			if w.version {
				got = strings.TrimSuffix(strings.ToLower(got), ".")
			}
			if got != exp {
				t.Errorf("data-fig=%q is not what %s prints:\n page: %s\n %s: %s", f.key, w.from, got, w.from, exp)
			}
			continue
		}
		got := numbers(f.text)
		if len(got) != len(w.nums) {
			t.Errorf("data-fig=%q reads %q, %d numbers, but %s gives %d: %v", f.key, f.text, len(got), w.from, len(w.nums), w.nums)
			continue
		}
		for i := range got {
			if !agrees(got[i], w.nums[i]) {
				t.Errorf("data-fig=%q reads %q, but %s says %v", f.key, f.text, w.from, w.nums)
				break
			}
		}
	}
	var unused []string
	for k := range want {
		if !seen[k] {
			unused = append(unused, k)
		}
	}
	sort.Strings(unused)
	for _, k := range unused {
		t.Errorf("no data-fig=%q on the page; a source with nothing to hold is a figure that was removed or renamed", k)
	}
}

// allowedDigits is text with digits in it that is not a figure: section and figure ordinals,
// percentile labels, and names.
var allowedDigits = regexp.MustCompile(`§\d+|FIG\.\d+|\bp(?:50|90|99)\b|peter3605|\bM1\b`)

// No digit appears in the visible page except inside a sourced figure, a command a reader
// copies (checked against the README below), or an element declared data-nofig.
//
// This is the half that makes the other test complete. Without it, a number typed into the page
// with no data-fig would be checked by nothing, which is exactly how the README drifted.
func TestThePageHasNoUnsourcedFigure(t *testing.T) {
	body := pageBody(t)
	exempt := regexp.MustCompile(`<(span|code|div|strong|pre|a)\b[^>]*\bdata-(?:fig="[^"]*"|nofig|cmd)[^>]*>`)
	body = blank(body, elements(t, body, exempt))
	text := allowedDigits.ReplaceAllString(visibleText(body), " ")
	for _, m := range regexp.MustCompile(`.{0,40}\d.{0,40}`).FindAllString(text, -1) {
		t.Errorf("an unsourced number on the page: %q — wrap it in data-fig with a source in siteFigures, or mark it data-nofig if it is not a measurement", m)
	}
}

// Every command the page tells a reader to paste is one the README gives, and every lease
// weight and percentile bar is drawn at the figure printed beside it.
func TestThePageDrawsAndQuotesWhatItPrints(t *testing.T) {
	body := pageBody(t)
	readme := readRepo(t, "README.md")
	cmds := regexp.MustCompile(`(?s)<code data-cmd>(.*?)</code>`).FindAllStringSubmatch(body, -1)
	if len(cmds) == 0 {
		t.Fatal("found no data-cmd on the page; this test has stopped testing anything")
	}
	for _, m := range cmds {
		raw := html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(m[1], ""))
		for _, line := range strings.Split(raw, "\n") {
			for _, part := range strings.Split(strings.TrimPrefix(strings.TrimSpace(line), "$ "), " && ") {
				if part = strings.TrimSpace(part); !strings.Contains(readme, part) {
					t.Errorf("the page tells a reader to run %q, which README.md does not give", part)
				}
			}
		}
	}

	want := siteFigures(t)
	drawn := regexp.MustCompile(`data-at="([^"]+)" style="(?:--slots:([\d.]+)|(?:width|left):([\d.]+)%)`).FindAllStringSubmatch(body, -1)
	if len(drawn) < 10 {
		t.Fatalf("found %d drawn figures, want the four lease weights and six bar marks", len(drawn))
	}
	for _, m := range drawn {
		w, ok := want[m[1]]
		if !ok || len(w.nums) != 1 {
			t.Errorf("data-at=%q names no single-valued figure", m[1])
			continue
		}
		// FIG.04 is drawn in slots; FIG.02's axis runs 0-12.5 across 100%, so 8% a unit.
		if m[2] != "" && !agrees(m[2], w.nums[0]) {
			t.Errorf("FIG.04 draws %s at %s slots, but %s says %v", m[1], m[2], w.from, w.nums[0])
		}
		if m[3] != "" && !agrees(m[3], w.nums[0]*8) {
			t.Errorf("FIG.02 draws %s at %s%%, but %s puts it at %.1f%%", m[1], m[3], w.from, w.nums[0]*8)
		}
	}

	// FIG.01's traces: each is drawn at its arm's measured median and maximum (the caption
	// promises exactly that). y is plotted as 13 minus the value.
	for _, arm := range []string{"ung", "gov"} {
		m := regexp.MustCompile(`data-trace="` + arm + `"[^>]*points="([^"]+)"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("FIG.01 has no %s trace", arm)
		}
		var vs []float64
		for _, pt := range strings.Fields(m[1]) {
			y, _ := strconv.ParseFloat(strings.SplitN(pt, ",", 2)[1], 64)
			vs = append(vs, 13-y)
		}
		sort.Float64s(vs)
		med, max := vs[len(vs)/2], vs[len(vs)-1]
		if p := want[arm+"-rq-p50"].nums[0]; math.Abs(med-p) > 0.05 {
			t.Errorf("FIG.01 draws the %s trace with median %.2f, but its measured p50 is %v", arm, med, p)
		}
		if mx := want[arm+"-rq-max"].nums[0]; math.Abs(max-mx) > 0.005 {
			t.Errorf("FIG.01 draws the %s trace peaking at %.2f, but its measured max is %v", arm, max, mx)
		}
	}
}

// The page loads nothing from anywhere else: no analytics, no pixel, no font host.
//
// The page says "no telemetry" and the binary has no outbound network code; a tracker here
// would make that headline the first false thing a stranger reads. Links a reader clicks are
// fine; anything the browser fetches on its own is not.
func TestThePageLoadsNothingFromElsewhere(t *testing.T) {
	page := readSite(t)
	fetched := regexp.MustCompile(`(?i)(?:\bsrc=|<link\b[^>]*\bhref=|url\()\s*["']?([^"')\s>]+)`)
	for _, m := range fetched.FindAllStringSubmatch(page, -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "#") || strings.HasPrefix(ref, "data:") {
			continue
		}
		if strings.Contains(ref, "//") {
			if strings.Contains(m[0], "<link") && strings.Contains(m[0], `rel="canonical"`) {
				continue
			}
			t.Errorf("the page fetches %s from another origin", ref)
			continue
		}
		if _, err := os.Stat(sitePath(ref)); err != nil {
			t.Errorf("the page fetches %s, which is not in site/: %v", ref, err)
		}
	}
	for _, host := range []string{"googleapis", "gstatic", "googletagmanager", "google-analytics", "plausible", "segment", "hotjar"} {
		if strings.Contains(page, host) {
			t.Errorf("the page mentions %s", host)
		}
	}
	if !strings.Contains(page, "<link rel=\"canonical\" href=\""+pageURL+"\">") {
		t.Errorf("the page's canonical link is not %s", pageURL)
	}
}

// A shared link renders as a card: Open Graph and Twitter tags, pointing at a real image.
func TestThePageHasASocialPreview(t *testing.T) {
	page := readSite(t)
	for _, tag := range []string{
		`property="og:title"`, `property="og:description"`, `property="og:type"`,
		`property="og:url" content="` + pageURL + `"`,
		`property="og:image" content="` + pageURL + `og.png"`,
		`name="twitter:card" content="summary_large_image"`,
		`name="twitter:image" content="` + pageURL + `og.png"`,
	} {
		if !strings.Contains(page, tag) {
			t.Errorf("the page has no <meta %s>", tag)
		}
	}
	b, err := os.ReadFile(sitePath("og.png"))
	if err != nil {
		t.Fatalf("site/og.png: %v", err)
	}
	if len(b) < 24 || string(b[1:4]) != "PNG" {
		t.Fatal("site/og.png is not a PNG")
	}
	w, h := be32(b[16:20]), be32(b[20:24])
	if w != 1200 || h != 630 {
		t.Errorf("site/og.png is %dx%d; the card size every network renders is 1200x630", w, h)
	}
	if _, err := os.Stat(sitePath(".nojekyll")); err != nil {
		t.Errorf("site/.nojekyll is missing: %v", err)
	}
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// Every route in names the page: the README above the fold, the marketplace manifest, and the
// Homebrew formula.
func TestEveryRouteInLinksToThePage(t *testing.T) {
	readme := readRepo(t, "README.md")
	if i := strings.Index(readme, pageURL); i < 0 || i > strings.Index(readme, "\n## ") {
		t.Errorf("README.md does not link %s before its first section", pageURL)
	}

	var manifest struct {
		Plugins []struct {
			Homepage string `json:"homepage"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(readRepo(t, ".claude-plugin/marketplace.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Plugins) == 0 || manifest.Plugins[0].Homepage != pageURL {
		t.Errorf("the marketplace manifest's homepage is not %s: %+v", pageURL, manifest.Plugins)
	}

	// The formula is rendered at release time; tools/brewformula's own test holds the rendered
	// homepage line to this constant.
	formula := readRepo(t, "tools/brewformula/main.go")
	if !strings.Contains(formula, fmt.Sprintf("const homepage = %q", pageURL)) {
		t.Errorf("the Homebrew formula's homepage is not %s", pageURL)
	}
}
