package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeHome points settingsPath at a temporary home and returns the settings path.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return filepath.Join(home, ".claude", "settings.json")
}

func writeSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings is not valid JSON after our write: %v\n%s", err, b)
	}
	return m
}

func install(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"trace", "install"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func uninstall(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"trace", "uninstall"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestInstallAddsTheThreeHooks(t *testing.T) {
	path := fakeHome(t)
	writeSettings(t, path, `{"model":"opus"}`)

	if code, _, stderr := install(t); code != 0 {
		t.Fatalf("install: exit %d: %s", code, stderr)
	}

	got := readSettings(t, path)
	if got["model"] != "opus" {
		t.Errorf("existing settings lost: %+v", got)
	}
	hooks, _ := got["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatal("no hooks key written")
	}
	for _, e := range traceEvents {
		entries, _ := hooks[e.event].([]any)
		if len(entries) != 1 {
			t.Errorf("%s: want 1 entry, got %d", e.event, len(entries))
		}
	}
}

// Invariant 7: running install twice changes nothing the second time.
func TestInstallIsIdempotent(t *testing.T) {
	path := fakeHome(t)
	writeSettings(t, path, `{"model":"opus"}`)

	if code, _, stderr := install(t); code != 0 {
		t.Fatalf("first install: exit %d: %s", code, stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	code, stdout, stderr := install(t)
	if code != 0 {
		t.Fatalf("second install: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "already installed") {
		t.Errorf("second install did not report itself as a no-op: %s", stdout)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(after, again) {
		t.Errorf("second install changed the file:\n--- after first ---\n%s\n--- after second ---\n%s", after, again)
	}

	backups, _ := filepath.Glob(path + ".governor-trace-backup-*")
	if len(backups) != 1 {
		t.Errorf("want exactly 1 backup after two installs, got %d: %v", len(backups), backups)
	}
}

// The story's last clause puts these hooks in a real config, so the undo has to be real too:
// uninstall must leave the file byte-for-byte as it found it, formatting included, not
// merely semantically equal.
func TestUninstallRestoresTheFileByteForByte(t *testing.T) {
	path := fakeHome(t)
	// Deliberately not the formatting our marshaller produces: tabs, unusual key order, a
	// trailing newline. A round trip through us must not silently reformat someone's file.
	original := "{\n\t\"model\": \"opus\",\n\t\"env\": {\"FOO\": \"bar\"},\n\t\"hooks\": {\n\t\t\"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"/bin/true\"}]}]\n\t}\n}\n"
	writeSettings(t, path, original)

	if code, _, stderr := install(t); code != 0 {
		t.Fatalf("install: exit %d: %s", code, stderr)
	}
	if code, _, stderr := uninstall(t); code != 0 {
		t.Fatalf("uninstall: exit %d: %s", code, stderr)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != original {
		t.Errorf("uninstall did not restore the file byte-for-byte\n--- want ---\n%q\n--- got ---\n%q", original, got)
	}

	backups, _ := filepath.Glob(path + ".governor-trace-backup-*")
	if len(backups) != 0 {
		t.Errorf("uninstall left %d backup(s) behind: %v", len(backups), backups)
	}
}

// A wholesale restore of the backup would be simpler and wrong: anything added to settings
// between install and uninstall would be destroyed. Removal is surgical.
func TestUninstallKeepsChangesMadeAfterInstall(t *testing.T) {
	path := fakeHome(t)
	writeSettings(t, path, `{"model":"opus"}`)

	if code, _, stderr := install(t); code != 0 {
		t.Fatalf("install: exit %d: %s", code, stderr)
	}

	// The user edits their settings while the hooks are installed.
	edited := readSettings(t, path)
	edited["statusLine"] = map[string]any{"type": "command", "command": "/bin/echo hi"}
	hooks := edited["hooks"].(map[string]any)
	// Deliberately an event this installer does NOT register, so that the assertions below
	// can be absolute: the whole key must survive, and every one of our own events must be
	// gone entirely. A user hook sharing an event WITH ours is a different property and has
	// its own test (TestUninstallLeavesOtherHooksOnTheSameEvent).
	hooks["Notification"] = []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "/usr/bin/say done"}},
	}}
	b, _ := json.MarshalIndent(edited, "", "  ")
	writeSettings(t, path, string(b)+"\n")

	if code, _, stderr := uninstall(t); code != 0 {
		t.Fatalf("uninstall: exit %d: %s", code, stderr)
	}

	got := readSettings(t, path)
	if got["model"] != "opus" {
		t.Error("model setting lost")
	}
	if got["statusLine"] == nil {
		t.Error("statusLine added after install was destroyed by uninstall")
	}
	remaining, _ := got["hooks"].(map[string]any)
	if remaining == nil {
		t.Fatal("hooks key removed although an unrelated Notification hook remained")
	}
	if remaining["Notification"] == nil {
		t.Error("unrelated Notification hook was removed")
	}
	for _, e := range traceEvents {
		if remaining[e.event] != nil {
			t.Errorf("%s hook survived uninstall: %+v", e.event, remaining[e.event])
		}
	}
}

// Our hooks share their events with hooks other tools install. Uninstall must take exactly
// ours out of a shared event and leave the neighbours in place.
func TestUninstallLeavesOtherHooksOnTheSameEvent(t *testing.T) {
	path := fakeHome(t)
	writeSettings(t, path, `{
      "hooks": {
        "SessionStart": [
          {"hooks": [{"type": "command", "command": "/opt/other/tool --session"}]}
        ],
        "SubagentStop": [
          {"hooks": [
            {"type": "command", "command": "/opt/other/tool --stop"}
          ]}
        ]
      }
    }`)

	if code, _, stderr := install(t); code != 0 {
		t.Fatalf("install: exit %d: %s", code, stderr)
	}
	if code, _, stderr := uninstall(t); code != 0 {
		t.Fatalf("uninstall: exit %d: %s", code, stderr)
	}

	got := readSettings(t, path)
	hooks, _ := got["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatal("hooks removed entirely")
	}
	for _, ev := range []string{"SessionStart", "SubagentStop"} {
		entries, _ := hooks[ev].([]any)
		if len(entries) != 1 {
			t.Fatalf("%s: want the 1 foreign entry to remain, got %d: %+v", ev, len(entries), entries)
		}
		raw, _ := json.Marshal(entries)
		if !strings.Contains(string(raw), "/opt/other/tool") {
			t.Errorf("%s: foreign hook lost: %s", ev, raw)
		}
		if strings.Contains(string(raw), "trace") {
			t.Errorf("%s: our hook survived: %s", ev, raw)
		}
	}
	if hooks["SubagentStart"] != nil {
		t.Error("SubagentStart should be gone: it held only our hook")
	}
}

// Refusing beats guessing. A settings file we cannot parse is one we cannot merge into, and
// overwriting it would destroy configuration we never read.
func TestInstallRefusesToWriteOverInvalidJSON(t *testing.T) {
	path := fakeHome(t)
	broken := `{"model": "opus",,,`
	writeSettings(t, path, broken)

	code, _, stderr := install(t)
	if code == 0 {
		t.Error("exit 0 on unparseable settings, want non-zero")
	}
	if !strings.Contains(stderr, "Refusing to write") {
		t.Errorf("stderr did not say it refused: %s", stderr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != broken {
		t.Errorf("the file was modified anyway:\n%s", got)
	}
	if backups, _ := filepath.Glob(path + ".governor-trace-backup-*"); len(backups) != 0 {
		t.Errorf("a backup was written for a refused install: %v", backups)
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	path := fakeHome(t)
	writeSettings(t, path, `{"model":"opus"}`)
	before, _ := os.ReadFile(path)

	code, stdout, stderr := install(t, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "would write") {
		t.Errorf("dry run did not print the plan: %s", stdout)
	}
	if !strings.Contains(stdout, "trace agent") {
		t.Errorf("dry run did not show the hook commands: %s", stdout)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("dry run modified the file")
	}
	if backups, _ := filepath.Glob(path + ".governor-trace-backup-*"); len(backups) != 0 {
		t.Errorf("dry run wrote a backup: %v", backups)
	}
}

func TestInstallCreatesSettingsWhenThereIsNone(t *testing.T) {
	path := fakeHome(t)
	if code, _, stderr := install(t); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	got := readSettings(t, path)
	if got["hooks"] == nil {
		t.Error("no hooks written")
	}
	// Nothing existed, so there was nothing to back up.
	if backups, _ := filepath.Glob(path + ".governor-trace-backup-*"); len(backups) != 0 {
		t.Errorf("backed up a file that did not exist: %v", backups)
	}
}

func TestUninstallOnACleanMachineIsANoOp(t *testing.T) {
	path := fakeHome(t)
	writeSettings(t, path, `{"model":"opus"}`)
	before, _ := os.ReadFile(path)

	code, stdout, stderr := uninstall(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("did not report a no-op: %s", stdout)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("uninstall modified a file it had nothing to remove from")
	}
}

// isTraceCommand decides what uninstall deletes, so a mistake here either strands our hooks
// or eats somebody else's.
func TestIsTraceCommandMatchesOnlyOurs(t *testing.T) {
	yes := []string{
		"/Users/x/.local/share/claude-governor/bin/governor trace agent",
		"/opt/bin/governor trace ensure",
		"governor trace sample",
		// The release artefacts and the test binary. A matcher that misses these leaves
		// hooks that uninstall cannot remove.
		"/opt/bin/governor-darwin-arm64 trace agent",
		"/tmp/go-build123/b001/governor.test trace ensure",
		"/Users/Bob Smith/bin/governor trace agent", // a path with a space in it
	}
	no := []string{
		"",
		"/opt/bin/governor gate",          // GOV-18's hook, not ours
		"/opt/bin/governor trace install", // not a hook command
		"/opt/bin/other-tool trace agent", // not our binary
		"/usr/bin/strace agent",           // substring coincidence
		"trace agent",                     // no binary at all
		"/opt/bin/governor confirm # trace agent", // ours mentioned in a comment
		"/opt/bin/governor gate; rm -rf / trace agent",
	}
	for _, cmd := range yes {
		if !isTraceCommand(cmd) {
			t.Errorf("isTraceCommand(%q) = false, want true", cmd)
		}
	}
	for _, cmd := range no {
		if isTraceCommand(cmd) {
			t.Errorf("isTraceCommand(%q) = true, want false", cmd)
		}
	}
}

func TestWriteAtomicPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeAtomic(path, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"a":1}` {
		t.Errorf("content = %q", b)
	}
	// No temp files left over.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("writeAtomic left debris: %v", entries)
	}
}

func TestMergeIsDefensiveAboutOddShapes(t *testing.T) {
	// Every one of these is settings.json holding something where we expect a structure.
	// A panic here happens while holding the user's only copy of their configuration.
	cases := []string{
		`{"hooks": "not an object"}`,
		`{"hooks": {"SubagentStart": "not an array"}}`,
		`{"hooks": {"SubagentStart": [null, 42, "x"]}}`,
		`{"hooks": {"SubagentStart": [{"hooks": "not an array"}]}}`,
		`{"hooks": {"SubagentStart": [{"hooks": [null, {"command": 7}]}]}}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			path := fakeHome(t)
			writeSettings(t, path, body)
			if code, _, stderr := install(t); code != 0 {
				t.Fatalf("install: exit %d: %s", code, stderr)
			}
			if code, _, stderr := uninstall(t); code != 0 {
				t.Fatalf("uninstall: exit %d: %s", code, stderr)
			}
			// It need not round-trip to the original — the input was malformed — but it must
			// still be valid JSON with our hooks gone.
			got := readSettings(t, path)
			if hooks, ok := got["hooks"].(map[string]any); ok {
				for _, e := range traceEvents {
					if entries, ok := hooks[e.event].([]any); ok {
						raw, _ := json.Marshal(entries)
						if strings.Contains(string(raw), "trace agent") ||
							strings.Contains(string(raw), "trace ensure") {
							t.Errorf("%s: our hook survived uninstall: %s", e.event, raw)
						}
					}
				}
			}
		})
	}
}

func TestMergeTraceHooksReportsWhatItAdded(t *testing.T) {
	settings := map[string]any{}
	added, merged := mergeTraceHooks(settings, "/opt/bin/governor")
	if added != len(traceEvents) {
		t.Errorf("added = %d, want %d", added, len(traceEvents))
	}
	again, _ := mergeTraceHooks(merged, "/opt/bin/governor")
	if again != 0 {
		t.Errorf("second merge added %d, want 0", again)
	}
	// A different install location is a different hook and must be added.
	moved, _ := mergeTraceHooks(merged, "/usr/local/bin/governor")
	if moved != len(traceEvents) {
		t.Errorf("relocated binary added %d hooks, want %d", moved, len(traceEvents))
	}
}

// encoding/json escapes &, < and > by default, which would silently rewrite the user's own
// prose the first time we merged a hook into a settings file containing an ampersand. The
// real settings file on this machine contains one.
func TestSettingsMarshalDoesNotEscapeHTML(t *testing.T) {
	path := fakeHome(t)
	const prose = "sensitive data & audiences <all> of them"
	writeSettings(t, path, `{"note":"`+prose+`"}`)

	if code, _, stderr := install(t); code != 0 {
		t.Fatalf("install: exit %d: %s", code, stderr)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), prose) {
		t.Errorf("the user's text was escaped by our write:\n%s", b)
	}
	if strings.Contains(string(b), `\u0026`) {
		t.Errorf("ampersand was escaped to \\u0026:\n%s", b)
	}
}

func TestJSONEqualIgnoresFormatting(t *testing.T) {
	a := []byte("{\n  \"a\": 1,\n  \"b\": [1, 2]\n}\n")
	b := []byte(`{"b":[1,2],"a":1}`)
	if !jsonEqual(a, b) {
		t.Error("jsonEqual said two identical documents differ")
	}
	if jsonEqual(a, []byte(`{"a":2}`)) {
		t.Error("jsonEqual said two different documents match")
	}
	if jsonEqual(a, []byte(`not json`)) {
		t.Error("jsonEqual matched against unparseable input")
	}
}

func TestRemoveTraceHooksDropsEmptyScaffolding(t *testing.T) {
	settings := map[string]any{}
	mergeTraceHooks(settings, "/opt/bin/governor")
	if n := removeTraceHooks(settings); n != len(traceEvents) {
		t.Fatalf("removed %d, want %d", n, len(traceEvents))
	}
	if !reflect.DeepEqual(settings, map[string]any{}) {
		t.Errorf("uninstall left scaffolding behind: %+v", settings)
	}
}
