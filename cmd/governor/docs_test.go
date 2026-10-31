package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/peter3605/claude-governor/internal/signals"
)

// The user documentation (GOV-36): docs/guide.md, docs/how-it-works.md and the docs/ index.
//
// Prose is not compiled, and this repository has paid for that twice — GOV-B09 found
// design.md illustrating a status line that no longer rendered, a year after it changed. These
// tests are what stop the two pages written for strangers from becoming a second, drifting
// source of truth: every status line they show is one render prints, every refusal they quote
// is one the gate writes, every command they name is in the table, every link into design.md
// lands on a heading that exists, and every file in docs/ is classified.

// docsPath is a file in the repository's docs/ directory, relative to this package.
func docsPath(name string) string { return filepath.Join("..", "..", "docs", name) }

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(docsPath(name))
	if err != nil {
		t.Fatalf("read docs/%s: %v", name, err)
	}
	return string(b)
}

// guideStatusLine is one line the guide shows, and the view that produces it.
type guideStatusLine struct {
	name string
	line string
}

// guideStatusLines renders every line guide.md illustrates.
//
// A func rather than a var because three of them carry a deadline, and a deadline is relative
// to now. Each sits thirty seconds past a whole minute, so the rendered "42m left" is stable
// for half a minute after the view is built — far longer than a render takes.
//
// TestTheDocsIllustrateTheStatusLineThatActuallyRenders adds these to its own corpus, so the
// guide is held to the renderer in both directions by that test as well as by the one below.
func guideStatusLines() []guideStatusLine {
	cpu := func(f float64) *float64 { return &f }
	in := func(d time.Duration) time.Time { return time.Now().Add(d + 30*time.Second) }
	views := []struct {
		name string
		v    statusView
	}{
		{"governed, with both load signals", statusView{
			OK: true, InUse: 3, Slots: 3, Budget: 5, CPU: cpu(0.61), Thermal: signals.Nominal}},
		{"governed, holding only agents", statusView{
			OK: true, InUse: 3, Slots: 0.2, Budget: 6, CPU: cpu(0.12), Thermal: signals.Nominal}},
		{"governed and idle", statusView{OK: true, Budget: 4}},
		{"this session holds one", statusView{
			OK: true, InUse: 3, Slots: 3, Budget: 5, Mine: 1, CPU: cpu(0.61), Thermal: signals.Nominal}},
		{"full, with a queue, thermal fair", statusView{
			OK: true, InUse: 4, Slots: 4, Budget: 4, Waiting: 2, CPU: cpu(0.97), Thermal: signals.Fair}},
		{"floored by thermal serious", statusView{
			OK: true, InUse: 1, Slots: 1, Budget: 1, CPU: cpu(0.99), Thermal: signals.Serious}},
		{"floored by thermal critical", statusView{OK: true, Budget: 1, Thermal: signals.Critical}},
		{"full", statusView{OK: true, InUse: 4, Slots: 4, Budget: 4}},
		{"pinned", statusView{OK: true, InUse: 2, Slots: 2, Budget: 2, Pinned: true}},
		{"pinned, with time left", statusView{
			OK: true, InUse: 2, Slots: 2, Budget: 2, Pinned: true, PinnedUntil: in(42 * time.Minute)}},
		{"boosted", statusView{OK: true, InUse: 1, Slots: 1, Budget: 6, Boosted: true}},
		{"boosted, with time left", statusView{
			OK: true, InUse: 1, Slots: 1, Budget: 6, Boosted: true, BoostedUntil: in(55 * time.Minute)}},
		{"paused", statusView{OK: true, Paused: true}},
		{"paused, with time left", statusView{OK: true, Paused: true, PausedUntil: in(14 * time.Minute)}},
		{"a session whose hooks never loaded", statusView{OK: true, Ungoverned: true}},
		{"no daemon", statusView{}},
	}
	out := make([]guideStatusLine, 0, len(views))
	for _, v := range views {
		out = append(out, guideStatusLine{name: v.name, line: render(v.v, 0)})
	}
	return out
}

// The guide shows every state the status line can print, each exactly as render prints it.
//
// Containment of a corpus is not enough on its own: a corpus that quietly stopped covering a
// clause would pass. So the corpus is also checked against the renderer's own vocabulary —
// every clause clauses() can add, every word thermalWord() can produce for a state the
// platform reports, and each of the three lines that replace the slot count. A clause added
// to the renderer without a line here and in the guide fails the second half.
func TestTheGuideShowsEveryLineTheStatusLineCanPrint(t *testing.T) {
	guide := readDoc(t, "guide.md")
	lines := guideStatusLines()

	var all []string
	for _, g := range lines {
		all = append(all, g.line)
		if !strings.Contains(guide, "`"+g.line+"`") && !strings.Contains(guide, "\n"+g.line+"\n") {
			t.Errorf("guide.md does not show the status line for %s, which renders as %q", g.name, g.line)
		}
	}
	corpus := strings.Join(all, "\n")

	// Every clause the renderer can emit, derived from the renderer where it can be.
	want := []string{
		render(statusView{}, 0),                           // daemon down
		render(statusView{OK: true, Ungoverned: true}, 0), // hooks never loaded
		render(statusView{OK: true, Paused: true}, 0),     // paused
		" slots",
		" yours)",
		" waiting",
		"cpu ",
		"boosted",
		"pinned",
		" left",
	}
	for _, th := range []signals.Thermal{signals.Nominal, signals.Fair, signals.Serious, signals.Critical} {
		if w := thermalWord(th); w != "" {
			want = append(want, "thermal "+w)
		}
	}
	for _, w := range want {
		if !strings.Contains(corpus, w) {
			t.Errorf("no status line the guide shows contains %q, which the renderer can print", w)
		}
	}
}

// The refusals the guide quotes are the ones the gate writes, word for word.
//
// The guide quotes them so a person recognises one when a session reports it, and a quotation
// that drifted would send them looking for text that never appears. Each is rendered from
// denyReason with the numbers the guide shows.
func TestTheGuideQuotesTheRefusalsTheGateActuallyWrites(t *testing.T) {
	guide := readDoc(t, "guide.md")
	cpu := 0.93
	for name, text := range map[string]string{
		"a spawn refused on a full machine": denyReason(acquireResponse{
			Reason: "capacity", Budget: 4, Slots: 4, Asking: 0.08, CPU: &cpu,
			Thermal: signals.Nominal, Denials: 1,
		}, intent{kind: "spawn"}),
		"a nested spawn": denyReason(acquireResponse{Reason: "depth"}, intent{kind: "spawn"}),
		"a build too big for the room left": denyReason(acquireResponse{
			Reason: "capacity", Budget: 4, Slots: 1, Asking: 3.75,
			Thermal: signals.Nominal, Denials: 1,
		}, intent{kind: "build"}),
	} {
		if !strings.Contains(guide, text) {
			t.Errorf("guide.md does not quote %s as the gate writes it:\n%s", name, text)
		}
	}
	for _, head := range []string{"Machine at capacity", "This session is at its fair share", "Not enough free build slots"} {
		if !strings.Contains(guide, "`"+head+"`") {
			t.Errorf("guide.md's table of refusal heads does not name %q", head)
		}
	}
}

// namedSubcommandRE finds `governor <word>` where text reads as a command to type: at the start
// of a line, after a quote or backtick, after a colon, or after a shell `&&`.
//
// Prose says things like "it runs governor on every tool call in every session", and a scan
// that counted those would report `governor on` as a missing subcommand — a guard that cries
// wolf is a guard somebody deletes. Hyphens are allowed because `plugin-path` has one.
var namedSubcommandRE = regexp.MustCompile("(?m)(?:^|\"|`|: |&& )governor ([a-z][a-z-]*)")

// namedSubcommands returns every subcommand text names as a command to type.
func namedSubcommands(text string) map[string]bool {
	named := map[string]bool{}
	for _, m := range namedSubcommandRE.FindAllStringSubmatch(text, -1) {
		named[m[1]] = true
	}
	return named
}

// markdownCode is the code in a markdown document: its fenced blocks and inline code spans,
// one per line. Prose is left out because a wrapped sentence can start a line with the word
// "governor", and what a reader copies and runs is the code.
func markdownCode(text string) string {
	var code []string
	fenced := regexp.MustCompile("(?ms)^```[a-z]*\n(.*?)^```")
	for _, m := range fenced.FindAllStringSubmatch(text, -1) {
		code = append(code, m[1])
	}
	prose := fenced.ReplaceAllString(text, "")
	for _, m := range regexp.MustCompile("`([^`\n]+)`").FindAllStringSubmatch(prose, -1) {
		code = append(code, m[1])
	}
	return strings.Join(code, "\n")
}

// Every `governor <subcommand>` the user docs show is a subcommand of this binary.
//
// The same guard doctor's fix strings have (TestEveryDoctorFixNamesARealCommand), applied to
// the two pages a stranger copies commands from.
func TestEveryCommandTheDocsNameIsReal(t *testing.T) {
	for _, doc := range []string{"guide.md", "how-it-works.md"} {
		named := namedSubcommands(markdownCode(readDoc(t, doc)))
		if len(named) == 0 {
			t.Errorf("found no `governor <command>` in %s; this test has stopped testing anything", doc)
		}
		for name := range named {
			if !registered(name) {
				t.Errorf("%s names `governor %s`, which is not a subcommand of this binary", doc, name)
			}
		}
	}
}

// githubSlug is the anchor GitHub gives a markdown heading: lower case, punctuation other
// than hyphens and underscores dropped, spaces to hyphens. An em dash between spaces therefore
// leaves a double hyphen, which is why "10. Hook surface — confirmed" is #10-hook-surface--confirmed.
func githubSlug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_',
			unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// headingSlugs is the set of anchors a markdown file's headings produce.
func headingSlugs(text string) map[string]bool {
	slugs := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#{1,6} +(.+?) *$`).FindAllStringSubmatch(text, -1) {
		slugs[githubSlug(m[1])] = true
	}
	return slugs
}

// Every link from the user docs into another doc lands on a heading that exists.
//
// "Every claim links to the design.md section it summarises" is only worth anything if the
// links resolve. design.md's headings carry section numbers and change when a section is
// retitled, and a dead anchor on GitHub silently opens the top of the file — the reader lands
// somewhere plausible and never learns the link was wrong.
func TestTheDocsLinkToHeadingsThatExist(t *testing.T) {
	link := regexp.MustCompile(`\]\(([a-z0-9-]+\.md)?#([^)]+)\)`)
	for _, doc := range []string{"guide.md", "how-it-works.md"} {
		text := readDoc(t, doc)
		links := link.FindAllStringSubmatch(text, -1)
		if len(links) == 0 {
			t.Errorf("found no section links in %s; this test has stopped testing anything", doc)
		}
		for _, m := range links {
			target := m[1]
			if target == "" {
				target = doc
			}
			if !headingSlugs(readDoc(t, target))[m[2]] {
				t.Errorf("%s links to %s#%s, which is not a heading in %s", doc, target, m[2], target)
			}
		}
	}
}

// how-it-works.md links to design.md for what it summarises, and from every section.
//
// The story's clause is that every claim links to the design.md section it summarises. Whether
// a sentence is a claim is not something a test can judge, but a section of the tour with no
// link into design.md at all certainly fails it, so that is what is checked.
func TestEveryHowItWorksSectionCitesDesign(t *testing.T) {
	text := readDoc(t, "how-it-works.md")
	sections := regexp.MustCompile(`(?m)^## `).Split(text, -1)
	for _, s := range sections[1:] {
		title := strings.SplitN(s, "\n", 2)[0]
		if !strings.Contains(s, "](design.md#") {
			t.Errorf("how-it-works.md section %q links to no section of design.md", title)
		}
	}
}

// Every file in docs/ has exactly one line in docs/README.md classifying it.
//
// The index is only useful if it is complete, and a measurement record added without a line
// is exactly how a reader comes to treat one as a spec — or a spec as a record they may ignore.
func TestEveryDocsFileIsIndexed(t *testing.T) {
	entries, err := os.ReadDir(docsPath(""))
	if err != nil {
		t.Fatalf("read docs/: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		present[name] = true
	}

	line := regexp.MustCompile(`(?m)^- \[([^\]]+)\]\(([^)]+)\) — (spec|guide|measurement record): \S`)
	indexed := map[string]int{}
	for _, m := range line.FindAllStringSubmatch(readDoc(t, "README.md"), -1) {
		if m[1] != m[2] {
			t.Errorf("docs/README.md labels %q but links %q", m[1], m[2])
		}
		indexed[m[2]]++
	}

	var missing, phantom []string
	for name := range present {
		switch indexed[name] {
		case 0:
			missing = append(missing, name)
		case 1:
		default:
			t.Errorf("docs/README.md classifies %s %d times; it should be once", name, indexed[name])
		}
	}
	for name := range indexed {
		if !present[name] {
			phantom = append(phantom, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(phantom)
	for _, name := range missing {
		t.Errorf("docs/%s has no line in docs/README.md; add one classifying it as spec, guide or measurement record", name)
	}
	for _, name := range phantom {
		t.Errorf("docs/README.md indexes %s, which is not in docs/", name)
	}
	if len(indexed) == 0 {
		t.Error("docs/README.md has no index lines; this test has stopped testing anything")
	}
}
