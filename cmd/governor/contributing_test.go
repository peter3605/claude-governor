package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The maintainer guide (GOV-37): CONTRIBUTING.md at the repository root.
//
// It is the only place the release procedure is written down where a contributor can see it,
// and a procedure is exactly the kind of prose that rots quietly: a script is renamed, a
// release.yml job is split, and the guide goes on describing a path nobody can follow. These
// tests hold every repository path, script, release job, make target, test name and doc anchor
// the guide names to what actually exists.

// repoPath is a path relative to the repository root, from this package.
func repoPath(rel string) string { return filepath.Join("..", "..", filepath.FromSlash(rel)) }

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(repoPath(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// repoRoots are the top-level entries a path in the guide can start with. A fixed list rather
// than whatever the root holds today, so that a guide naming `script/x.sh` when the directory
// is `scripts/` is still checked, and fails, instead of being skipped as "not a repo path".
// Build artefacts (bin/) and other repositories' paths (the tap's Formula/) are deliberately
// absent: the guide names them, and they are not in this tree.
var repoRoots = []string{
	".github", ".claude-plugin", "cmd", "commands", "docs", "internal", "scripts", "skills", "tools",
	"CHANGELOG.md", "CONTRIBUTING.md", "LICENSE", "Makefile", "README.md", "go.mod", "install.sh",
}

// guidePathRefs returns every repository path the guide names, as written, with any #anchor
// kept: link targets, and path-shaped words inside its code.
func guidePathRefs(text string) []string {
	seen := map[string]bool{}
	add := func(ref string) {
		ref = strings.TrimPrefix(ref, "./")
		ref = strings.TrimRight(ref, ".,:;")
		if ref == "" || seen[ref] {
			return
		}
		root := strings.SplitN(strings.SplitN(ref, "#", 2)[0], "/", 2)[0]
		for _, r := range repoRoots {
			if root == r {
				seen[ref] = true
				return
			}
		}
	}
	for _, m := range regexp.MustCompile(`\]\(([^)\s]+)\)`).FindAllStringSubmatch(text, -1) {
		if !strings.Contains(m[1], "://") && !strings.HasPrefix(m[1], "#") {
			add(m[1])
		}
	}
	for _, word := range strings.FieldsFunc(markdownCode(text), func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || strings.ContainsRune("\"'`()|<>=", r)
	}) {
		add(word)
	}
	refs := make([]string, 0, len(seen))
	for r := range seen {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	return refs
}

// Every repository path and script the guide names exists, and every anchor it links to is a
// heading in the file it points at.
func TestEveryPathTheMaintainerGuideNamesExists(t *testing.T) {
	refs := guidePathRefs(readRepoFile(t, "CONTRIBUTING.md"))
	if len(refs) < 10 {
		t.Fatalf("found only %d repository paths in CONTRIBUTING.md (%v); this test has stopped testing anything", len(refs), refs)
	}
	for _, ref := range refs {
		path, anchor, _ := strings.Cut(ref, "#")
		if _, err := os.Stat(repoPath(path)); err != nil {
			t.Errorf("CONTRIBUTING.md names %s, which does not exist in the repository", path)
			continue
		}
		if anchor != "" && !headingSlugs(readRepoFile(t, path))[anchor] {
			t.Errorf("CONTRIBUTING.md links to %s#%s, which is not a heading in %s", path, anchor, path)
		}
	}
}

// workflowJobs is the set of job ids a GitHub workflow file defines: the keys indented one
// level under its top-level `jobs:`.
func workflowJobs(t *testing.T, rel string) map[string]bool {
	t.Helper()
	jobs := map[string]bool{}
	_, body, ok := strings.Cut(readRepoFile(t, rel), "\njobs:\n")
	if !ok {
		t.Fatalf("%s has no top-level jobs:", rel)
	}
	for _, m := range regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):\s*$`).FindAllStringSubmatch(body, -1) {
		jobs[m[1]] = true
	}
	if len(jobs) == 0 {
		t.Fatalf("parsed no jobs out of %s", rel)
	}
	return jobs
}

// section is the text of the "## <title>" section of a markdown document, up to the next
// "## " heading.
func section(t *testing.T, text, title string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, "\n## "+title+"\n")
	if !ok {
		t.Fatalf("CONTRIBUTING.md has no '## %s' section", title)
	}
	body, _, _ := strings.Cut(after, "\n## ")
	return body
}

var namedJobRE = regexp.MustCompile("`([a-z][a-z0-9_-]*)` job")

// The release procedure names release.yml's jobs, all of them, and only jobs that exist.
//
// Two checks. Anywhere in the guide, "`x` job" must be a job in ci.yml or release.yml, and in
// the Releasing section it must be a job in release.yml. And the Releasing section's job list
// (the "- **`x`**" bullets) must be exactly release.yml's jobs: a job added to the workflow
// without a line saying what it checks is a release step the next maintainer cannot see.
func TestTheMaintainerGuideNamesReleaseJobsThatExist(t *testing.T) {
	text := readRepoFile(t, "CONTRIBUTING.md")
	release := workflowJobs(t, ".github/workflows/release.yml")
	ci := workflowJobs(t, ".github/workflows/ci.yml")

	for _, m := range namedJobRE.FindAllStringSubmatch(text, -1) {
		if !release[m[1]] && !ci[m[1]] {
			t.Errorf("CONTRIBUTING.md names the `%s` job, which is in neither ci.yml nor release.yml", m[1])
		}
	}

	releasing := section(t, text, "Releasing")
	for _, m := range namedJobRE.FindAllStringSubmatch(releasing, -1) {
		if !release[m[1]] {
			t.Errorf("CONTRIBUTING.md's Releasing section names the `%s` job, which is not in release.yml", m[1])
		}
	}

	listed := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^- \\*\\*`([a-z][a-z0-9_-]*)`\\*\\*").FindAllStringSubmatch(releasing, -1) {
		listed[m[1]] = true
	}
	for job := range listed {
		if !release[job] {
			t.Errorf("CONTRIBUTING.md's Releasing section describes the `%s` job, which is not in release.yml", job)
		}
	}
	for job := range release {
		if !listed[job] {
			t.Errorf("release.yml has a `%s` job that CONTRIBUTING.md's Releasing section does not describe", job)
		}
	}
}

// Every `make <target>` the guide shows is a target in the Makefile, and every test it names
// by function is defined somewhere in the repository.
func TestTheMaintainerGuideNamesRealTargetsAndTests(t *testing.T) {
	text := readRepoFile(t, "CONTRIBUTING.md")
	code := markdownCode(text)

	targets := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([a-z][a-z0-9_-]*):`).FindAllStringSubmatch(readRepoFile(t, "Makefile"), -1) {
		targets[m[1]] = true
	}
	makes := regexp.MustCompile(`(?m)(?:^|\s)make ([a-z][a-z0-9_-]*)`).FindAllStringSubmatch(code, -1)
	if len(makes) == 0 {
		t.Error("found no `make <target>` in CONTRIBUTING.md; this test has stopped testing anything")
	}
	for _, m := range makes {
		if !targets[m[1]] {
			t.Errorf("CONTRIBUTING.md shows `make %s`, which is not a Makefile target", m[1])
		}
	}

	defined := map[string]bool{}
	err := filepath.WalkDir(repoPath(""), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "_to_delete") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`).FindAllStringSubmatch(string(b), -1) {
			defined[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository for tests: %v", err)
	}
	named := regexp.MustCompile("`(Test[A-Z][A-Za-z0-9_]*)`").FindAllStringSubmatch(text, -1)
	if len(named) == 0 {
		t.Error("found no test names in CONTRIBUTING.md; this test has stopped testing anything")
	}
	for _, m := range named {
		if !defined[m[1]] {
			t.Errorf("CONTRIBUTING.md names %s, which is not a test function in the repository", m[1])
		}
	}
}
