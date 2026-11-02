// Command changelog prints one release's entry from CHANGELOG.md, which release.yml publishes
// as that release's notes.
//
// It is what makes a release's GitHub notes and its CHANGELOG entry the same text by
// construction (GOV-39). The release workflow runs it first, against the tag being built.
// When the tag has no entry, or only an empty one, it exits non-zero, so the release fails
// before any binary is built and before anything is published. The entry therefore has to be
// written and merged before the tag, which is what CONTRIBUTING.md's Releasing section says.
//
// An entry is everything between its `## [X.Y.Z]` heading and the next `## ` heading, with the
// heading itself left out. GitHub shows the release's title above the body, so repeating the
// heading would print the version twice.
//
// Usage: changelog -version 0.1.3 [-file CHANGELOG.md] > notes.md
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// heading matches a release heading: `## [0.1.2] - 2026-10-31`. The date is optional to the
// parser so that a missing date is reported as a format problem by the repository test rather
// than as "no entry", which would send someone looking for an entry that is right there.
var heading = regexp.MustCompile(`^## \[([^\]]+)\]`)

func main() {
	version := flag.String("version", "", "the release version, without the leading v (0.1.3)")
	file := flag.String("file", "CHANGELOG.md", "the changelog to read")
	flag.Parse()
	if *version == "" {
		fmt.Fprintln(os.Stderr, "changelog: -version is required")
		os.Exit(2)
	}
	f, err := os.Open(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "changelog:", err)
		os.Exit(1)
	}
	defer f.Close()
	entry, err := Entry(f, *version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "changelog: %s: %v\n", *file, err)
		os.Exit(1)
	}
	fmt.Print(entry)
}

// Entries parses a changelog into its release entries, keyed by version, in file order.
// Duplicate versions are an error: which of the two would be published depends on whoever
// reads the file, so it is refused outright.
func Entries(r io.Reader) (versions []string, bodies map[string]string, err error) {
	bodies = map[string]string{}
	var cur string
	var b strings.Builder
	flush := func() {
		if cur != "" {
			bodies[cur] = strings.TrimSpace(b.String())
		}
		b.Reset()
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## ") {
			flush()
			cur = ""
			if m := heading.FindStringSubmatch(line); m != nil {
				cur = m[1]
				if _, dup := bodies[cur]; dup {
					return nil, nil, fmt.Errorf("version %s has two entries", cur)
				}
				bodies[cur] = ""
				versions = append(versions, cur)
			}
			continue
		}
		if cur != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	flush()
	return versions, bodies, nil
}

// ErrNoEntry is returned when the changelog has no heading for the version.
var ErrNoEntry = errors.New("no entry")

// Entry returns one version's entry, ending in a single newline. A version with no heading,
// or a heading with nothing under it, is an error: publishing an empty body would be the
// silent-release problem this tool exists to remove.
func Entry(r io.Reader, version string) (string, error) {
	_, bodies, err := Entries(r)
	if err != nil {
		return "", err
	}
	body, ok := bodies[version]
	if !ok {
		return "", fmt.Errorf("%w for %s: write a `## [%s] - YYYY-MM-DD` entry and merge it before tagging", ErrNoEntry, version, version)
	}
	if body == "" {
		return "", fmt.Errorf("the entry for %s is empty", version)
	}
	return body + "\n", nil
}
