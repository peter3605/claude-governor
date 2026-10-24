package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GOV-17 clause 1: the standing instruction and the skill.
//
// Every property below fails SILENTLY if it regresses. A wrong tool name is a model told to
// call something it does not have; a skill at the wrong path registers nothing; a description
// that has grown into a paragraph is context paid on every turn of every session. None of
// those errors anywhere, which is why each is pinned here rather than trusted to survive an
// edit.

// The repository's copy and the bytes install writes are the same file.
func TestTheShippedSkillMatchesTheRepository(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "skills", skillName, skillFileName))
	if err != nil {
		t.Fatalf("reading the repository's copy of the skill: %v", err)
	}
	if string(onDisk) != string(pluginSkill()) {
		t.Error("skills/heavy-build/SKILL.md and the bytes install writes have diverged; " +
			"regenerate cmd/governor/skill_md.go from the markdown")
	}
}

// skillFrontmatter splits the skill's YAML frontmatter into its keys. It reads only the flat
// `key: value` lines this file uses, which is all it needs to be held to.
func skillFrontmatter(t *testing.T) map[string]string {
	t.Helper()
	md := string(pluginSkill())
	if !strings.HasPrefix(md, "---\n") {
		t.Fatal("the skill has no frontmatter, so it has no description and nothing of it is " +
			"resident (hook-surface.md section 21.2)")
	}
	end := strings.Index(md[4:], "\n---\n")
	if end < 0 {
		t.Fatal("the skill's frontmatter is never closed")
	}
	fm := map[string]string{}
	for _, line := range strings.Split(md[4:4+end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("frontmatter line %q is not key: value — a wrapped description is a "+
				"multi-line value this file does not use", line)
		}
		fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return fm
}

// The skill's name is its directory, and its description is one short line that says when to
// reach for it and when not to.
//
// THE DESCRIPTION IS THE RESIDENT HALF (hook-surface.md section 21.2): it is in every session's
// context from the start, and the body is not. So it is held short, and it must carry its own
// negative scope, or it invites the model to load the skill before every command.
func TestTheSkillDescriptionIsShortAndScoped(t *testing.T) {
	fm := skillFrontmatter(t)
	if fm["name"] != skillName {
		t.Errorf("frontmatter name = %q, want %q, the directory install writes it to", fm["name"], skillName)
	}
	desc := fm["description"]
	if desc == "" {
		t.Fatal("the skill has no description, so a session never learns it exists")
	}
	if len(desc) > 250 {
		t.Errorf("description is %d characters; it is resident in every session, keep it to one "+
			"short line (under 250)", len(desc))
	}
	if !strings.Contains(desc, "Not for ordinary shell work") {
		t.Error("the description does not say what the skill is NOT for, which invites loading " +
			"it before every command")
	}
}

// The standing instruction names the tool exactly as a session sees it.
//
// hook-surface.md section 20.5 measured the name: Claude Code prefixes
// `mcp__plugin_<plugin>_<server>__` onto the tool's own name. Composed here from the same
// constants install and the MCP server use, so renaming any of them fails this rather than
// leaving every session told to call a tool it does not have.
func TestTheAdviceNamesTheToolASessionSees(t *testing.T) {
	b, err := pluginMCP()
	if err != nil {
		t.Fatal(err)
	}
	var mcp struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatal(err)
	}
	if len(mcp.MCPServers) != 1 {
		t.Fatalf("want exactly one MCP server, have %d", len(mcp.MCPServers))
	}
	var server string
	for k := range mcp.MCPServers {
		server = k
	}
	seen := "mcp__plugin_" + pluginName + "_" + server + "__" + toolName

	for name, text := range map[string]string{
		"the standing instruction": sessionStartAdvice,
		"the skill":                string(pluginSkill()),
	} {
		if !strings.Contains(text, seen) {
			t.Errorf("%s does not name %s, the tool a session actually has", name, seen)
		}
	}
	// And the skill name it points at is the one section 21.2 measured: manifest name, colon,
	// skill directory.
	if !strings.Contains(sessionStartAdvice, pluginName+":"+skillName) {
		t.Errorf("the standing instruction does not point at %s:%s", pluginName, skillName)
	}
}

// The standing instruction is short, says what not to ask about, and says the answer never
// decides whether a test runs.
//
// THE LAST IS CLAUSE 3, and it is the one that matters. A session told to check before building
// can respond by not building, and a lower load bought by skipping the test suite is a worse
// machine that every load metric reports as a better one.
func TestTheAdviceIsShortNegativeScopedAndKeepsTheTests(t *testing.T) {
	if n := len(sessionStartAdvice); n > 1000 {
		t.Errorf("the standing instruction is %d characters; it is in context on every turn of "+
			"every session, keep it under 1000", n)
	}
	for _, want := range []struct{ phrase, why string }{
		{"Do NOT call it", "it must say what not to ask about, or the model asks before ls"},
		{"If unsure, do not call", "uncertainty must resolve to not asking; the gate is underneath"},
		{"never whether", "clause 3: the answer decides when a test runs, never whether"},
		{"do not skip, shrink or drop a test run", "clause 3, stated as an instruction"},
	} {
		if !strings.Contains(sessionStartAdvice, want.phrase) {
			t.Errorf("the standing instruction lost %q: %s", want.phrase, want.why)
		}
	}
	md := string(pluginSkill())
	if !strings.Contains(md, "never **whether**") {
		t.Error("the skill body lost clause 3: the answer decides when a test runs, never whether")
	}
}

// Install writes the skill where a skills-dir plugin's skills are read, and uninstall removes it.
func TestInstallWritesTheSkillAndUninstallRemovesIt(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	want := filepath.Join(p.plugin, "skills", "heavy-build", "SKILL.md")
	if p.skill != want {
		t.Fatalf("the skill is at %s, want %s: hook-surface.md section 21 measured a plugin's "+
			"skills at <plugin>/skills/<name>/SKILL.md", p.skill, want)
	}
	got, err := os.ReadFile(p.skill)
	if err != nil {
		t.Fatalf("install did not write the skill: %v", err)
	}
	if string(got) != string(pluginSkill()) {
		t.Error("the installed skill differs from the bytes this binary ships")
	}

	uninstallOK(t)
	for _, path := range []string{p.skill, filepath.Dir(p.skill), filepath.Dir(filepath.Dir(p.skill))} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived uninstall (%v)", path, err)
		}
	}
}

// design.md section 13.3 carries the skill's row, IN THE TABLE.
//
// The string alone is not enough: design.md names skills/heavy-build/SKILL.md in prose in two
// other places, one of them recording that its row was removed while nothing wrote it. Only the
// table is the normative list uninstall is checked against.
func TestSectionThirteenThreeNamesTheSkill(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "design.md"))
	if err != nil {
		t.Fatalf("read design.md: %v", err)
	}
	row := "| `~/.claude/skills/claude-governor/skills/heavy-build/SKILL.md`"
	if !strings.Contains(string(b), row) {
		t.Errorf("design.md section 13.3 has no row starting %s; it is the normative list of "+
			"what touches the user's machine, and uninstall is only verifiable while it is "+
			"exhaustive", row)
	}
}
