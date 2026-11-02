package main

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const sample = `# Changelog

Preamble that belongs to no release.

## Format

Also not a release.

## [0.2.0] - 2026-12-01

### Added

- a thing

## [0.1.0] - 2026-10-29

The first release.
`

func TestEntryIsTheTextBetweenItsHeadingAndTheNext(t *testing.T) {
	got, err := Entry(strings.NewReader(sample), "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := "### Added\n\n- a thing\n"; got != want {
		t.Errorf("entry = %q, want %q", got, want)
	}
	got, err = Entry(strings.NewReader(sample), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := "The first release.\n"; got != want {
		t.Errorf("last entry = %q, want %q", got, want)
	}
}

// The release fails on a tag with no entry. This is the check that makes clause 2 a
// construction rather than a habit.
func TestATagWithNoEntryIsRefused(t *testing.T) {
	_, err := Entry(strings.NewReader(sample), "0.1.1")
	if !errors.Is(err, ErrNoEntry) {
		t.Fatalf("err = %v, want ErrNoEntry", err)
	}
	// A prefix of a real version is not that version.
	if _, err := Entry(strings.NewReader(sample), "0.2"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("0.2 matched something: %v", err)
	}
	// A non-release `## ` heading is not an entry, even if its name is asked for.
	if _, err := Entry(strings.NewReader(sample), "Format"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("the Format section was treated as an entry: %v", err)
	}
}

func TestAnEmptyEntryIsRefused(t *testing.T) {
	in := "## [0.3.0] - 2027-01-01\n\n   \n## [0.2.0] - 2026-12-01\n\nreal\n"
	if _, err := Entry(strings.NewReader(in), "0.3.0"); err == nil || errors.Is(err, ErrNoEntry) {
		t.Errorf("err = %v, want an empty-entry error", err)
	}
}

func TestAVersionWithTwoEntriesIsRefused(t *testing.T) {
	in := "## [0.2.0] - 2026-12-01\n\na\n\n## [0.2.0] - 2026-12-02\n\nb\n"
	if _, err := Entry(strings.NewReader(in), "0.2.0"); err == nil {
		t.Error("a duplicated version was accepted")
	}
}

// The binary, not just the function: release.yml relies on the exit status.
func TestTheCommandExitsNonZeroWithoutAnEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the command")
	}
	dir := t.TempDir()
	path := dir + "/CHANGELOG.md"
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "run", ".", "-version", "0.2.0", "-file", path).Output()
	if err != nil {
		t.Fatalf("an existing entry failed: %v", err)
	}
	if string(out) != "### Added\n\n- a thing\n" {
		t.Errorf("stdout = %q", out)
	}
	cmd := exec.Command("go", "run", ".", "-version", "9.9.9", "-file", path)
	out, err = cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("a missing entry exited %v, want non-zero", err)
	}
	if len(out) != 0 {
		t.Errorf("a missing entry printed %q to stdout; a release must never get a partial body", out)
	}
}

// The repository's own CHANGELOG.md, held to the format it states at the top.
func TestTheRepositoryChangelog(t *testing.T) {
	raw, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	versions, bodies, err := Entries(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}

	// Every release heading carries a date, and they run newest first.
	dated := regexp.MustCompile(`(?m)^## \[([^\]]+)\] - (\d{4}-\d{2}-\d{2})$`)
	dates := map[string]string{}
	for _, m := range dated.FindAllStringSubmatch(text, -1) {
		dates[m[1]] = m[2]
	}
	prev := ""
	for _, v := range versions {
		d, ok := dates[v]
		if !ok {
			t.Errorf("the heading for %s is not `## [%s] - YYYY-MM-DD`", v, v)
			continue
		}
		if prev != "" && d > prev {
			t.Errorf("%s (%s) is listed below a release dated %s; entries run newest first", v, d, prev)
		}
		prev = d
	}

	// The releases that exist have entries. Tags are the authority when the checkout has
	// them; CI's shallow checkout does not, so the published ones are also named here.
	want := []string{"0.1.0", "0.1.1", "0.1.2"}
	if out, err := exec.Command("git", "tag", "--list", "v*").Output(); err == nil {
		for _, tag := range strings.Fields(string(out)) {
			want = append(want, strings.TrimPrefix(tag, "v"))
		}
	}
	checked := map[string]bool{}
	for _, v := range want {
		if checked[v] {
			continue
		}
		checked[v] = true
		if strings.Contains(v, "-") {
			continue // a pre-release tag, such as a release-workflow test, needs no permanent entry
		}
		if bodies[v] == "" {
			t.Errorf("release %s has no entry in CHANGELOG.md", v)
		}
	}

	// An entry is also a GitHub release body, where a relative link resolves against the
	// release page and goes nowhere.
	rel := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	for _, v := range versions {
		for _, m := range rel.FindAllStringSubmatch(bodies[v], -1) {
			if !strings.Contains(m[1], "://") {
				t.Errorf("the %s entry links to %q; entries are published as release notes, so links must be absolute", v, m[1])
			}
		}
	}

	// Clause 5: this project keeps no Unreleased section (CHANGELOG.md, Format).
	if regexp.MustCompile(`(?mi)^## \[?unreleased`).MatchString(text) {
		t.Error("CHANGELOG.md has an Unreleased heading; the stated format is to have none")
	}

	// Clause 1: v0.1.1 says what it actually was.
	for _, s := range []string{"brew audit --strict", "`version`", "never"} {
		if !strings.Contains(bodies["0.1.1"], s) {
			t.Errorf("the 0.1.1 entry no longer mentions %q", s)
		}
	}

	// Clause 3: the format, semver and the two inner versions are stated.
	for _, s := range []string{"Keep a Changelog", "semver", "breaking change", "protocol", "config schema"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(s)) {
			t.Errorf("CHANGELOG.md no longer states %q", s)
		}
	}
}
