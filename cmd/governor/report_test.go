package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for `governor doctor --report` (GOV-40). See report.go for why the report is built in
// two layers; these tests hold each of them.

// planted is in every string the report must never print, so one substring check covers them
// all — and none of the safe fields the report is supposed to carry contain it.
const planted = "PLANTED"

// TestTheReportIsSafeToPasteInPublic is GOV-40's clause 2.
//
// It seeds every source the report reads — the daemon log and the trace log the gate writes
// its decisions to — with the things a real machine puts there: a working directory naming a
// private repository, command text with a branch name and credentials in it, a bearer token,
// a vendor token, an opaque high-entropy token, a password in key=value form, and absolute
// paths under the user's home. Then it asserts none of it comes out.
//
// The bar is GOV-40's clause 2: no command text, no path outside the state directory, no
// environment, no repository or branch name, no credential. (The story pointed at
// docs/bash-corpus.md for a screening standard; that document describes how the corpus was
// collected and states no such rule, so the rule is this test.) It is asserted on the whole
// output rather than per section, so a section added later is covered without anybody
// remembering to add it here.
//
// AND IT ASSERTS THE SAFE FIELDS ARE PRESENT. A report that printed nothing would pass every
// absence check above; this is what stops that being a way to make the test green.
func TestTheReportIsSafeToPasteInPublic(t *testing.T) {
	w := newWorld(t)

	repo := filepath.Join(w.home, "Repos", planted+"-private-repo")
	vendor := "ghp_" + planted + "0123456789abcdefABCDEF"
	opaque := "Zq9" + planted + "x7k3Lm2Np8Rs4Tv6Wb1Yc5"
	session := "1f2e3d4c-5b6a-4789-8abc-def012345678"

	log := strings.Join([]string{
		"governor daemon: reaped 1 lease(s)",
		"governor daemon: session " + session + " ended, released 0 lease(s)",
		"governor daemon: recovered from handler panic: exec curl -H 'Authorization: Bearer " + opaque + "' in " + repo,
		"governor daemon: config " + filepath.Join(repo, ".env") + ": not found, using built-in defaults",
		"governor daemon: malformed request, closing connection: token=" + opaque + " password=" + planted + "hunter2",
		"governor daemon: " + vendor,
		"governor daemon: " + opaque,
		"governor daemon: listening on unix://" + filepath.Join(w.stateDir, "sock"),
		"",
	}, "\n")
	writeFile(t, filepath.Join(w.stateDir, daemonLogFile), log)

	records := strings.Join([]string{
		// A build the gate refused. The command field does not exist in the real record — it
		// is planted to prove that a field added later is dropped by construction.
		`{"ts":"2026-11-01T10:00:00.123Z","kind":"build","v":3,"build":{"session_id":"` + session +
			`","shapes":["pytest"],"class":"heavy","weight":3.75,"outcome":"denied","why":"refused: ` + opaque + ` at ` + repo +
			`","cwd":"` + repo + `","command":"git push origin feature/` + planted + `-branch"}}`,
		`{"ts":"2026-11-01T10:00:05Z","kind":"build","v":3,"build":{"session_id":"` + session +
			`","shapes":["go test"],"class":"medium","weight":1,"outcome":"parked","waited_ms":1544,"cwd":"` + repo + `"}}`,
		// An agent record carries a working directory and is not a decision at all.
		`{"ts":"2026-11-01T10:00:06Z","kind":"agent","v":3,"agent":{"event":"SubagentStart","session_id":"` + session +
			`","agent_id":"a1","cwd":"` + repo + `"}}`,
		"",
	}, "\n")
	writeFile(t, filepath.Join(w.stateDir, "samples.jsonl"), records)

	var plain, stderr bytes.Buffer
	plainCode := run([]string{"doctor"}, &plain, &stderr)
	var out bytes.Buffer
	code := run([]string{"doctor", "--report"}, &out, &stderr)
	report := out.String()

	for _, leak := range []string{planted, w.home, w.stateDir, "Repos", "Bearer", "hunter2"} {
		if strings.Contains(report, leak) {
			t.Errorf("the report contains %q, which must never reach a public issue:\n%s", leak, report)
		}
	}

	for _, want := range []string{
		// Every section, so a reader can tell "nothing to show" from "not looked at".
		"governor doctor --report", "--- doctor ---", "--- daemon log", "--- recent gate decisions",
		// The header facts clause 1 names.
		"governor     " + binaryVersion, "install ", "cores", "thermal", "claude code", "budget",
		// The decision, by its safe fields: outcome, class, weight, shape, wait, short session.
		"denied", "heavy", "w3.8", "pytest", "parked", "waited 1544ms", "1f2e3d4c",
		// The reason text survives with the secret and the path taken out of it.
		"why: refused: <redacted> at <path>",
		// The log survives too, with ids shortened and the state directory named, not spelled.
		"governor daemon: reaped 1 lease(s)", "session 1f2e3d4c ended", "unix://<state>/sock",
		// Doctor's own verdict, unchanged.
		"daemon", "fix",
		issueChooserURL,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report is missing %q:\n%s", want, report)
		}
	}

	if code != plainCode {
		t.Errorf("--report exited %d and plain doctor %d: a script must see the same verdict either way", code, plainCode)
	}
}

// TestTheScrubberKeepsWhatAReportNeeds is the other half: a scrubber that redacted everything
// would be safe and useless. These are the shapes real doctor output and real log lines carry.
func TestTheScrubberKeepsWhatAReportNeeds(t *testing.T) {
	w := newWorld(t)
	sc := newReportScrubber()

	for _, c := range []struct{ in, want string }{
		// Ratios, platforms and URLs have slashes and are not paths.
		{"budget 3/6 slots held", "budget 3/6 slots held"},
		{"host darwin/arm64, 8 cores", "host darwin/arm64, 8 cores"},
		{"report it at " + issueChooserURL, "report it at " + issueChooserURL},
		// The install's own paths are what a hooks or binary finding is about.
		{"in " + filepath.Join(w.home, ".claude", "skills", pluginName, "hooks", "hooks.json"),
			"in ~/.claude/skills/" + pluginName + "/hooks/hooks.json"},
		{"settings " + filepath.Join(w.home, ".claude", "settings.json"), "settings ~/.claude/settings.json"},
		{"link " + filepath.Join(w.home, ".local", "bin", "governor"), "link ~/.local/bin/governor"},
		// The state directory, in either spelling.
		{"socket " + filepath.Join(w.stateDir, "sock") + " is yours", "socket <state>/sock is yours"},
		// A short build digest is not a secret.
		{"build 7673c843d7a0", "build 7673c843d7a0"},
		// A path ends at the colon that introduces what is wrong with it.
		{"config " + filepath.Join(w.home, ".claude", "settings.json") + ": not found", "config ~/.claude/settings.json: not found"},
		{"config " + filepath.Join(w.home, "x", ".env") + ": not found", "config <path>: not found"},
		// Anything else is the user's.
		{"hook in " + filepath.Join(w.home, "work", "proj", ".claude", "settings.json"), "hook in <path>"},
		{"re-run `/opt/somewhere/governor doctor`", "re-run `<path> doctor`"},
		{"pid 7  /var/folders/x/T/Test1/001/bin/governor  age 5h", "pid 7  <path>  age 5h"},
	} {
		if got := sc.line(c.in); got != c.want {
			t.Errorf("scrub(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheIssueTemplatesAskForTheReport is GOV-40's clause 3: every issue template's first
// required field is the report, and it spells the command exactly as this binary accepts it.
// A template asking for a flag the binary does not have would send every reporter into a usage
// error at the one moment they were trying to help.
func TestTheIssueTemplatesAskForTheReport(t *testing.T) {
	const command = "governor doctor --report"
	dir := filepath.Join("..", "..", ".github", "ISSUE_TEMPLATE")
	for _, name := range []string{"bug.yml", "wrong-refusal.yml"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The first input field, which is the first `- type:` that is not a markdown preamble.
		var first string
		for _, block := range strings.Split(string(raw), "  - type: ")[1:] {
			if !strings.HasPrefix(block, "markdown") {
				first = block
				break
			}
		}
		if !strings.Contains(first, command) || !strings.Contains(first, "required: true") {
			t.Errorf("%s: the first input field must require the output of `%s`, and say the command verbatim:\n%s",
				name, command, first)
		}
	}

	var stdout, stderr bytes.Buffer
	newWorld(t)
	if code := run(strings.Fields(command)[1:], &stdout, &stderr); code == 2 {
		t.Errorf("`%s` is a usage error (%s): the templates ask for a command this binary does not accept", command, stderr.String())
	}
}
