package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestClause1WaitsForTheBaselineWindow fails the build if GOV-17's clause 1 appears in the tree
// while docs/advice.md still reports the baseline window as open.
//
// WHY THIS IS A TEST AND NOT A NOTE. Clause 1 ships a standing instruction telling sessions to
// consult the governor before an expensive build. Clause 2 scores the machine before and after
// that instruction exists, so the whole series in ~/.claude/governor/samples.jsonl counts as a
// *before* observation ONLY while no such instruction has ever been injected. Shipping clause 1
// mid-window converts the baseline into a mixture of the two conditions, and there is no way to
// separate them afterwards — the window has to be thrown away and re-run, which on this machine
// is roughly eight calendar days.
//
// A note would not hold. The reminder channel is .claude/.current-story, which /lc:start
// overwrites with whatever story a session is working on, so GOV-17's handoff stops surfacing
// the moment another story is picked up. The failure is silent, unrecoverable and arrives via
// ordinary workflow, which is the combination that earns a mechanical guard.
//
// IT RELEASES ITSELF. The guard reads the window state out of advice.md rather than carrying a
// date, so recording the baseline as closed is what permits clause 1 to ship. That makes writing
// the close down a precondition of the next step instead of a step to remember after it.
func TestClause1WaitsForTheBaselineWindow(t *testing.T) {
	root := repoRoot(t)

	if !baselineIsOpen(t, root) {
		t.Log("baseline window is not open in docs/advice.md; clause 1 is free to ship")
		return
	}

	for _, found := range clause1Artifacts(t, root) {
		t.Errorf("clause 1 appears to have shipped (%s) while docs/advice.md still reports "+
			"the baseline window as `open since`.\n\n"+
			"If that is deliberate, the baseline window MUST be closed first: run\n"+
			"  go run ./tools/buildstats --since <the window's start>\n"+
			"confirm it carries four qualifying days and failed-open 0, record the result in\n"+
			"docs/advice.md's Results section, set `Baseline window: closed <start>..<end>`\n"+
			"there, and open the after-window. Then this guard releases itself.\n\n"+
			"Shipping clause 1 while the baseline is open destroys the baseline and cannot be "+
			"undone.", found)
	}
}

var baselineStateLine = regexp.MustCompile(`(?m)^\s*-\s*\*\*Baseline window:\*\*\s*(.+)$`)

// errNoWindowState reports an advice.md with no state line at all. A guard that disappears when
// its switch does is not a guard, so this is a failure rather than a pass.
type errNoWindowState struct{}

func (errNoWindowState) Error() string {
	return "docs/advice.md has no `- **Baseline window:**` line. That line is the switch this " +
		"guard reads; removing it would silently disable the guard, so its absence is a " +
		"failure rather than a pass."
}

// baselineIsOpenIn is the window-state predicate, over advice.md's bytes so it can be exercised
// directly rather than only against whatever the repo happens to say today.
func baselineIsOpenIn(doc []byte) (bool, error) {
	m := baselineStateLine.FindSubmatch(doc)
	if m == nil {
		return false, errNoWindowState{}
	}
	return strings.Contains(string(m[1]), "open since"), nil
}

// baselineIsOpen reads the one line in advice.md that records the window state.
func baselineIsOpen(t *testing.T, root string) bool {
	t.Helper()
	path := filepath.Join(root, "docs", "advice.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s, which carries the window state: %v", path, err)
	}
	open, err := baselineIsOpenIn(b)
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// clause1Artifacts names the things whose presence means the standing instruction exists. All
// three are specific and absent today.
func clause1Artifacts(t *testing.T, root string) []string {
	t.Helper()
	found, err := clause1ArtifactsIn(root)
	if err != nil {
		t.Fatalf("walking the tree for clause 1's artifacts: %v", err)
	}
	return found
}

// clause1ArtifactsIn is the detection over an arbitrary root, so the guard can be pointed at a
// synthetic tree and shown to fire. Deletion is restricted under ~/Repos for agent sessions, so
// a fixture cannot be built in the repo itself and removed again.
func clause1ArtifactsIn(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		// 1. A skill checked into the repository.
		if strings.EqualFold(d.Name(), "SKILL.md") {
			found = append(found, rel)
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			src := string(b)
			// 2. A skill WRITTEN BY INSTALL, which is how this plugin ships every other
			// file it installs. There is no go:embed here — install.go:281 records that it
			// cannot reach out of its own directory — so `.mcp.json` and
			// `commands/governor.md` are Go string constants (pluginMCP, pluginCommand) and
			// a skill would be a third. THE FIRST VERSION OF THIS GUARD MISSED THAT
			// ENTIRELY: it looked only for a SKILL.md on disk, so the most likely way for
			// clause 1 to actually ship would have walked straight past it. design.md
			// §13.3 is what gave it away — it carried a row for
			// skills/heavy-build/SKILL.md that nothing writes, left over from planning
			// clause 1, which is exactly the path a real one would take.
			if strings.Contains(src, "SKILL.md") {
				found = append(found, rel+` (writes a SKILL.md)`)
			}
			// 3. additionalContext, the field that injects text into the model's context, as
			// distinct from systemMessage, which onboard.go uses to address the user. Only a
			// struct tag counts: the two files that discuss the difference in prose must not
			// trip this.
			if strings.Contains(src, `json:"additionalContext"`) {
				found = append(found, rel+` (json:"additionalContext")`)
			}
		}
		return nil
	})
	return found, err
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory, so the repo root cannot " +
				"be found")
		}
		dir = parent
	}
}

// TestTheGuardItselfFires exercises the guard against synthetic trees.
//
// A guard is only worth its comment if it can be shown to fail. It cannot be demonstrated
// against the repo — deletion is restricted under ~/Repos for agent sessions, so a fixture
// planted here could not be removed again — so the two predicates are pointed at a temp tree
// instead. If this test ever passes while TestClause1WaitsForTheBaselineWindow cannot fail, the
// guard has become decoration.
func TestTheGuardItselfFires(t *testing.T) {
	t.Run("the window-state switch", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			doc     string
			open    bool
			wantErr bool
		}{
			{"open", "- **Baseline window:** open since `2026-09-22`\n", true, false},
			{"closed releases the guard",
				"- **Baseline window:** closed `2026-09-22`..`2026-09-30`\n", false, false},
			{"not open", "- **Baseline window:** not open\n", false, false},
			{"the line is gone — a failure, never a pass",
				"# advice\n\nno state here at all\n", false, true},
		} {
			got, err := baselineIsOpenIn([]byte(c.doc))
			if (err != nil) != c.wantErr {
				t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
			}
			if err == nil && got != c.open {
				t.Errorf("%s: open = %v, want %v", c.name, got, c.open)
			}
		}
	})

	t.Run("clause 1's artifacts", func(t *testing.T) {
		// A clean tree carrying the things that must NOT trip the guard: prose about
		// additionalContext, and the pre-existing hookSpecificOutput tag on the deny path.
		clean := t.TempDir()
		write(t, clean, "cmd/governor/onboard.go",
			"// systemMessage, NOT hookSpecificOutput.additionalContext, and the difference\n")
		write(t, clean, "cmd/governor/gate.go",
			"type denyEnvelope struct{ HookSpecificOutput denyBody `json:\"hookSpecificOutput\"` }\n")
		write(t, clean, "cmd/governor/onboard_test.go",
			"// additionalContext is the exact reverse, so sending this as additionalContext\n")
		if got, err := clause1ArtifactsIn(clean); err != nil || len(got) != 0 {
			t.Errorf("a clean tree tripped the guard: %v (err %v)", got, err)
		}

		// The skill.
		skill := t.TempDir()
		write(t, skill, "skills/fleet-budget/SKILL.md", "# ask before you build\n")
		if got, _ := clause1ArtifactsIn(skill); len(got) != 1 {
			t.Errorf("a shipped SKILL.md did not trip the guard: %v", got)
		}

		// The injection itself.
		inject := t.TempDir()
		write(t, inject, "cmd/governor/session.go",
			"type ctx struct{ AdditionalContext string `json:\"additionalContext\"` }\n")
		if got, _ := clause1ArtifactsIn(inject); len(got) != 1 {
			t.Errorf(`a shipped json:"additionalContext" did not trip the guard: %v`, got)
		}

		// The skill as install would actually ship it: a Go string constant naming the file,
		// the same shape as pluginMCP and pluginCommand. The first version of this guard
		// missed this case, which is the whole reason it is pinned here.
		installed := t.TempDir()
		write(t, installed, "cmd/governor/install.go",
			"const pluginSkill = \"# ask before you build\\n\"\n"+
				"var skillPath = filepath.Join(plugin, \"skills\", \"heavy-build\", \"SKILL.md\")\n")
		if got, _ := clause1ArtifactsIn(installed); len(got) != 1 {
			t.Errorf("a skill written BY INSTALL did not trip the guard: %v", got)
		}

		// A _test.go file must not count, for either marker: the guard is about shipped
		// behaviour, and a test asserting an absence necessarily names the thing.
		onlyTest := t.TempDir()
		write(t, onlyTest, "cmd/governor/session_test.go",
			"want := `json:\"additionalContext\"`\n// and a SKILL.md is not written here\n")
		if got, _ := clause1ArtifactsIn(onlyTest); len(got) != 0 {
			t.Errorf("a test file tripped the guard: %v", got)
		}
	})
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
