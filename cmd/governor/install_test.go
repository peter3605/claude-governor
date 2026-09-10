package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Tests for `governor install` / `governor uninstall` (GOV-21).
//
// The one that makes this trustworthy is TestUninstallLeavesTheMachineByteIdentical. What
// is being asked of a stranger is to run our binary in a hook on every subagent spawn, and
// being able to prove it comes out clean is most of why they would say yes — so the
// property is asserted against a full recursive digest of the home directory, not against a
// list of paths someone remembered to check.

// stagedHome points HOME, the state directory and the endpoint at disposable places.
//
// HOME is what install resolves everything from, so setting it is what keeps these tests
// off the developer's own ~/.claude. The state directory is set separately and NOT under
// HOME on purpose: on macOS a t.TempDir() path is already long, and the socket beneath it
// would run past sockaddr_un's limit — the failure transport.MaxSocketPath exists to name.
func stagedHome(t *testing.T) paths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(sockDir(t), "gov")
	t.Setenv(stateDirEnvForTest, state)
	t.Setenv(endpointEnvForTest, "unix://"+filepath.Join(state, "sock"))

	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if p.home != home {
		t.Fatalf("staged home is %s, but paths resolved %s", home, p.home)
	}
	return p
}

// governor runs one subcommand in-process and returns its exit code and streams.
func governor(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// installOK runs `governor install --no-daemon` and fails the test if it does not succeed.
//
// --no-daemon in every test but the daemon ones: starting a detached process from a unit
// test leaves something running past the test that spawned it if anything goes wrong, and
// the daemon's own start and stop are covered directly in TestInstallStartsAndUninstallStopsTheDaemon.
func installOK(t *testing.T, extra ...string) string {
	t.Helper()
	code, stdout, stderr := governor(t, append([]string{"install", "--no-daemon"}, extra...)...)
	if code != 0 {
		t.Fatalf("governor install: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return stdout
}

func uninstallOK(t *testing.T, extra ...string) string {
	t.Helper()
	code, stdout, stderr := governor(t, append([]string{"uninstall"}, extra...)...)
	if code != 0 {
		t.Fatalf("governor uninstall: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return stdout
}

// ---------------------------------------------------------------------------
// the acceptance test
// ---------------------------------------------------------------------------

// snapshot is a recursive digest of every path under root.
//
// Content for regular files, target for symlinks, presence for directories. It is a
// digest of CONTENT and not of metadata: mtimes move for reasons that have nothing to do
// with us — a temp directory reused, a filesystem with coarse timestamps — and a test that
// failed on those would be a test people learn to re-run rather than believe.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "symlink -> " + target
		case d.IsDir():
			out[rel] = "dir"
		default:
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("file %s mode %s", sha256.Sum256(b), info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func diffSnapshots(before, after map[string]string) []string {
	var out []string
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			out = append(out, "REMOVED  "+path)
			continue
		}
		if got != want {
			out = append(out, "CHANGED  "+path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			out = append(out, "ADDED    "+path)
		}
	}
	sort.Strings(out)
	return out
}

// realisticSettings is a settings.json with the user's own configuration in it, including a
// statusLine of their own and hooks of their own.
//
// Deliberately NOT what Go's encoder would produce: two-space indent is right but the key
// order is the user's, there is a trailing structure our marshaller would reorder, and the
// hooks are ones install must not touch. Any of those coming back different after uninstall
// is a real regression, and a fixture formatted the way we write files would hide all three.
const realisticSettings = `{
  "model": "opusplan",
  "statusLine": {
    "type": "command",
    "command": "~/bin/my-own-statusline.sh"
  },
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "~/bin/notify.sh session" }
        ]
      }
    ]
  },
  "env": { "FOO": "bar & baz <qux>" }
}
`

// THE acceptance test: uninstall leaves the machine byte-identical to before install.
//
// design.md section 13.1's first principle, and the reason anyone would run a hook on every
// spawn. It is asserted over the whole home directory rather than over the paths section
// 13.3 lists, on purpose: 13.3 being exhaustive is the thing under test, so a test that
// consulted it would pass exactly when 13.3 was wrong.
func TestUninstallLeavesTheMachineByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		// linkDir is whether ~/.local/bin exists, which is what decides whether install
		// puts a symlink on the PATH.
		linkDir bool
	}{
		{name: "no settings file at all"},
		{name: "a settings file the user configured", settings: realisticSettings},
		{name: "an empty settings file", settings: "{}\n"},
		{name: "with ~/.local/bin on the machine", settings: realisticSettings, linkDir: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := stagedHome(t)
			if tc.settings != "" {
				mkdirAll(t, filepath.Dir(p.settings))
				writeFileT(t, p.settings, tc.settings)
			}
			if tc.linkDir {
				mkdirAll(t, filepath.Dir(p.link))
			}

			before := snapshot(t, p.home)

			installOK(t)
			// Sanity: the install must actually have done something, or this test would
			// pass for a command that does nothing at all.
			if diff := diffSnapshots(before, snapshot(t, p.home)); len(diff) == 0 {
				t.Fatal("install changed nothing; this test would pass vacuously")
			}

			uninstallOK(t)

			if diff := diffSnapshots(before, snapshot(t, p.home)); len(diff) > 0 {
				t.Errorf("uninstall did not leave %s byte-identical:\n  %s",
					p.home, strings.Join(diff, "\n  "))
			}
		})
	}
}

// The settings file specifically, byte for byte.
//
// Separated from the walk above because it is the file a person would notice, and because
// the failure it guards against is subtle: Go's json encoder sorts map keys, so a settings
// file that round-tripped through our marshaller comes back parsing the same and reading
// differently. restoreSettings prefers the backup's original bytes for exactly that reason,
// and this is what holds it there.
func TestUninstallRestoresTheUsersSettingsBytes(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)

	installOK(t)

	// While installed, the user's own statusLine is replaced by ours — that is what they
	// asked for — and their own hooks are untouched.
	during := readJSON(t, p.settings)
	statusLine, _ := during["statusLine"].(map[string]any)
	if cmd, _ := statusLine["command"].(string); !strings.HasSuffix(cmd, " statusline") {
		t.Errorf("installed statusLine command is %q, want the governor's", cmd)
	}
	if _, ok := during["hooks"]; !ok {
		t.Error("install removed the user's own hooks from settings.json")
	}
	if during["model"] != "opusplan" {
		t.Errorf("install changed the user's model setting to %v", during["model"])
	}

	uninstallOK(t)

	got, err := os.ReadFile(p.settings)
	if err != nil {
		t.Fatalf("read settings after uninstall: %v", err)
	}
	if string(got) != realisticSettings {
		t.Errorf("settings.json is not byte-identical after uninstall.\n got: %q\nwant: %q",
			got, realisticSettings)
	}
}

// A settings file edited while the governor was installed must keep the edit.
//
// This is why uninstall is surgical rather than a wholesale restore of the backup. Someone
// who installs, then adds a hook of their own a week later, then uninstalls, must keep the
// hook — a restore-from-backup would silently delete a week of their configuration and
// nothing would say so.
func TestUninstallKeepsChangesMadeSinceInstall(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)

	installOK(t)

	// The user adds something of their own after installing.
	settings := readJSON(t, p.settings)
	settings["permissions"] = map[string]any{"allow": []any{"Bash(ls:*)"}}
	writeJSON(t, p.settings, settings)

	uninstallOK(t)

	after := readJSON(t, p.settings)
	if _, ok := after["permissions"]; !ok {
		t.Error("uninstall destroyed a setting added after install")
	}
	// The user's OWN statusLine comes back, because install replaced it rather than adding
	// beside it — there is only one status line. Asserting the key is simply absent would be
	// asserting that uninstall deletes a setting install did not create.
	sl, ok := after["statusLine"].(map[string]any)
	if !ok {
		t.Fatal("uninstall did not restore the statusLine install replaced")
	}
	if sl["command"] != "~/bin/my-own-statusline.sh" {
		t.Errorf("statusLine command is %v, want the user's own back", sl["command"])
	}
	if after["model"] != "opusplan" {
		t.Errorf("uninstall changed the user's model setting to %v", after["model"])
	}
}

// ---------------------------------------------------------------------------
// idempotency
// ---------------------------------------------------------------------------

// Installing twice changes nothing the second time.
//
// The classic installer bug is the second run appending a duplicate hook, after which every
// spawn takes two leases and the budget is silently halved. The assertion is over the whole
// home directory rather than over the hook count, because the duplicate-hook bug is only
// the most famous member of the family — a second backup file left behind is the same
// defect with a quieter symptom.
func TestInstallTwiceChangesNothingTheSecondTime(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)
	mkdirAll(t, filepath.Dir(p.link))

	installOK(t)
	afterFirst := snapshot(t, p.home)

	// A timestamped backup name has one-second resolution, so a second install inside the
	// same second could not produce a distinguishable name even if it wrongly wrote one.
	// Waiting past the boundary is what makes "no second backup" a real assertion.
	time.Sleep(1100 * time.Millisecond)

	second := installOK(t)
	afterSecond := snapshot(t, p.home)

	if diff := diffSnapshots(afterFirst, afterSecond); len(diff) > 0 {
		t.Errorf("the second install changed the machine:\n  %s", strings.Join(diff, "\n  "))
	}
	if n := strings.Count(second, "created"); n != 0 {
		t.Errorf("the second install reported %d creations; it should report only unchanged\n%s", n, second)
	}
	if !strings.Contains(second, "unchanged") {
		t.Errorf("the second install did not report anything as unchanged:\n%s", second)
	}

	// And the hooks file itself holds exactly one registration per event.
	hooks := readHooks(t, p.hooks)
	for event, entries := range hooks {
		if len(entries) != 1 {
			t.Errorf("%s has %d matcher entries after two installs, want 1", event, len(entries))
		}
		for _, e := range entries {
			if len(e.Hooks) != 1 {
				t.Errorf("%s has %d commands after two installs, want 1", event, len(e.Hooks))
			}
		}
	}
}

// Uninstalling twice is not an error, and the second one changes nothing.
func TestUninstallTwiceIsANoOp(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)

	installOK(t)
	uninstallOK(t)
	afterFirst := snapshot(t, p.home)
	uninstallOK(t)

	if diff := diffSnapshots(afterFirst, snapshot(t, p.home)); len(diff) > 0 {
		t.Errorf("the second uninstall changed the machine:\n  %s", strings.Join(diff, "\n  "))
	}
}

// Uninstall on a machine that never had it installed is a no-op, not a failure.
func TestUninstallWithNothingInstalled(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)

	before := snapshot(t, p.home)
	uninstallOK(t)

	if diff := diffSnapshots(before, snapshot(t, p.home)); len(diff) > 0 {
		t.Errorf("uninstall touched a machine it was never installed on:\n  %s", strings.Join(diff, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// what install actually writes
// ---------------------------------------------------------------------------

type hookEntry struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	} `json:"hooks"`
}

func readHooks(t *testing.T, path string) map[string][]hookEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Hooks map[string][]hookEntry `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", path, err, raw)
	}
	return doc.Hooks
}

// Every hook design.md section 4.1 requires is registered, on the right event, running the
// right subcommand, against a binary that exists.
//
// The (event, subcommand) pair is the assertion rather than the event alone, for the reason
// doctor's misregistered() gives: a SubagentStart wired to `governor release` returns leases
// at the moment they should be bound, and every symptom of that reads as a budget bug rather
// than a wiring one.
func TestInstallRegistersEveryHook(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	hooks := readHooks(t, p.hooks)
	if len(hooks) != len(wantedHooks) {
		t.Errorf("hooks.json has %d events, want %d: %v", len(hooks), len(wantedHooks), keysOf(hooks))
	}
	for _, w := range wantedHooks {
		entries, ok := hooks[w.event]
		if !ok || len(entries) != 1 || len(entries[0].Hooks) != 1 {
			t.Errorf("%s: want exactly one hook, got %v", w.event, entries)
			continue
		}
		h := entries[0].Hooks[0]
		if h.Type != "command" {
			t.Errorf("%s: type is %q, want \"command\"", w.event, h.Type)
		}
		want := "${CLAUDE_PLUGIN_ROOT}/bin/governor " + w.args
		if h.Command != want {
			t.Errorf("%s: command is %q, want %q", w.event, h.Command, want)
		}
		if w.matched && entries[0].Matcher == "" {
			t.Errorf("%s: no matcher, so the gate would fire on every tool call", w.event)
		}
		if !w.matched && entries[0].Matcher != "" {
			t.Errorf("%s: has matcher %q; only the gate takes one", w.event, entries[0].Matcher)
		}
	}

	// The binary the hooks point at is really there and really runs.
	if fi, err := os.Stat(p.binary); err != nil {
		t.Fatalf("stat installed binary: %v", err)
	} else if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is mode %s, which nothing can execute", fi.Mode().Perm())
	}
}

// The gate's matcher covers every tool name that can put a subagent on the CPU.
//
// This is the check with no symptom when it is wrong: the hook is registered, the binary
// exists, the daemon is healthy, and the gate is simply never invoked. GOV-01 was spent
// measuring the real tool name and GOV-28 found the second spawn path; a matcher written by
// hand here would be free to lose either of them silently.
func TestInstalledMatcherCoversEverySpawnPath(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	hooks := readHooks(t, p.hooks)
	var matcher string
	for _, w := range wantedHooks {
		if w.matched {
			matcher = hooks[w.event][0].Matcher
		}
	}
	if matcher == "" {
		t.Fatal("no matcher on the gate hook")
	}
	re, err := regexp.Compile(matcher)
	if err != nil {
		t.Fatalf("matcher %q does not compile as a regexp: %v", matcher, err)
	}
	for _, name := range spawnToolNames {
		if !re.MatchString(name) {
			t.Errorf("matcher %q does not match %q, so that spawn path is ungoverned", matcher, name)
		}
	}
}

// The gate hook carries an explicit timeout.
//
// design.md section 10.1 measured the default at over 150 s with no established ceiling.
// Inheriting it would mean the wait window rests on a number nobody has bounded.
func TestGateHookSetsAnExplicitTimeout(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	hooks := readHooks(t, p.hooks)
	for _, w := range wantedHooks {
		if !w.matched {
			continue
		}
		if got := hooks[w.event][0].Hooks[0].Timeout; got != gateHookTimeout {
			t.Errorf("gate hook timeout is %d, want %d", got, gateHookTimeout)
		}
	}
}

// The plugin manifest is one Claude Code will load: a name, a version and a description.
func TestInstallWritesAPluginManifest(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	manifest := readJSON(t, p.manifest)
	if manifest["name"] != pluginName {
		t.Errorf("manifest name is %v, want %q", manifest["name"], pluginName)
	}
	if manifest["version"] != binaryVersion {
		t.Errorf("manifest version is %v, want %q", manifest["version"], binaryVersion)
	}
	if s, _ := manifest["description"].(string); s == "" {
		t.Error("manifest has no description")
	}
}

// The plugin goes where Claude Code actually looks.
//
// GOV-21 measured that ~/.claude/plugins/<name>/ is loaded by nothing on 2.1.265 and that
// ~/.claude/skills/<name>/ auto-loads (hook-surface.md section 15). The path is asserted
// rather than left implicit because getting it wrong is silent in every direction: the files
// are all present, doctor's hook check passes on the file it can read, and no spawn is ever
// governed.
func TestPluginGoesWhereClaudeCodeLoadsIt(t *testing.T) {
	p := stagedHome(t)
	want := filepath.Join(p.home, ".claude", "skills", pluginName)
	if p.plugin != want {
		t.Fatalf("plugin directory is %s, want %s", p.plugin, want)
	}
	installOK(t)
	if _, err := os.Stat(filepath.Join(want, "hooks", "hooks.json")); err != nil {
		t.Errorf("no hooks.json in the directory Claude Code loads: %v", err)
	}
}

// install prints exactly what it touched, and every path it prints is really there.
//
// design.md section 13.2's requirement. A report that names a path install did not write is
// worse than no report: it is what someone would consult before deciding whether to trust
// the thing, and it would be lying.
func TestInstallPrintsWhatItTouched(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)

	out := installOK(t)

	for _, want := range []string{p.plugin, p.settings} {
		if !strings.Contains(out, short(want)) {
			t.Errorf("install did not mention %s:\n%s", short(want), out)
		}
	}
	// Every path it claims to have created exists.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "created") && !strings.HasPrefix(line, "updated") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		path := strings.Replace(fields[1], "~", p.home, 1)
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("install reported %q but %s does not exist", line, path)
		}
	}
}

// --dry-run writes nothing at all.
func TestPluginInstallDryRunWritesNothing(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)

	before := snapshot(t, p.home)
	code, stdout, stderr := governor(t, "install", "--dry-run")
	if code != 0 {
		t.Fatalf("governor install --dry-run: exit %d (stderr: %s)", code, stderr)
	}
	if diff := diffSnapshots(before, snapshot(t, p.home)); len(diff) > 0 {
		t.Errorf("--dry-run changed the machine:\n  %s", strings.Join(diff, "\n  "))
	}
	if !strings.Contains(stdout, "would") {
		t.Errorf("--dry-run did not say what it would do:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// refusing to write
// ---------------------------------------------------------------------------

// A settings file that cannot be parsed is a settings file that must not be merged into.
//
// Overwriting it with our own idea of its contents would destroy configuration we never
// read. Refusing is the whole point of not blind-writing — and it refuses BEFORE creating
// anything, because half an install leaves files the user did not ask for and was not told
// about.
func TestInstallRefusesAnUnparseableSettingsFile(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, "{ this is not json ")

	before := snapshot(t, p.home)
	code, _, stderr := governor(t, "install", "--no-daemon")
	if code == 0 {
		t.Error("install exited 0 over a settings file it cannot parse")
	}
	if !strings.Contains(stderr, "Refusing to write") {
		t.Errorf("install did not say it was refusing:\n%s", stderr)
	}
	if diff := diffSnapshots(before, snapshot(t, p.home)); len(diff) > 0 {
		t.Errorf("install wrote something before refusing:\n  %s", strings.Join(diff, "\n  "))
	}
}

// A statusLine belonging to somebody else survives uninstall.
//
// Someone who installs the governor, then later points their status line at their own
// script, must keep it. Removing "the statusLine key" rather than "the statusLine we wrote"
// would delete it, and the symptom — a status line that silently stopped working — is one
// nobody would connect back to an uninstall.
func TestUninstallLeavesSomebodyElsesStatusLine(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	settings := readJSON(t, p.settings)
	settings["statusLine"] = map[string]any{"type": "command", "command": "~/bin/mine.sh"}
	writeJSON(t, p.settings, settings)

	uninstallOK(t)

	after := readJSON(t, p.settings)
	sl, ok := after["statusLine"].(map[string]any)
	if !ok {
		t.Fatal("uninstall removed a statusLine that was not ours")
	}
	if sl["command"] != "~/bin/mine.sh" {
		t.Errorf("statusLine command is %v, want the user's own", sl["command"])
	}
}

// ---------------------------------------------------------------------------
// the refresh interval (GOV-B07)
// ---------------------------------------------------------------------------

// Install writes a refreshInterval, and it is longer than the cache it has to see past.
//
// Without the key Claude Code runs the status line on events only, so a session waiting on a
// fan-out — which produces no assistant message until the fan-out ends — displays whatever
// it captured before the spawns, for the whole duration of the thing worth watching.
//
// The second assertion ties the interval to statuslineCacheTTL rather than to the literal 2,
// because that relationship is the load-bearing half of the choice: at an interval equal to
// the TTL a lone session's timer fires exactly at expiry and whether it reads the daemon or
// a stale cache is a race. Pinning only the number would let someone shorten the interval,
// or lengthen the TTL, and pass.
func TestInstallWritesAStatusLineRefreshInterval(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	sl, ok := readJSON(t, p.settings)["statusLine"].(map[string]any)
	if !ok {
		t.Fatal("install wrote no statusLine")
	}
	got, ok := sl["refreshInterval"].(float64)
	if !ok {
		t.Fatalf("statusLine has no refreshInterval: %v", sl)
	}
	if got != statuslineRefreshInterval {
		t.Errorf("refreshInterval is %v, want %d", got, statuslineRefreshInterval)
	}
	if interval := time.Duration(got) * time.Second; interval <= statuslineCacheTTL {
		t.Errorf("refreshInterval of %v is not longer than the %v cache; a timer firing at "+
			"expiry races the cache instead of reading through it", interval, statuslineCacheTTL)
	}
}

// Installing over a governor statusLine from before this key existed adds it.
//
// The upgrade path, and the one that silently does nothing if idempotency is keyed on the
// wrong fields. Every machine already running the plugin has a statusLine with our command
// and no refreshInterval; an equality check over only the keys the old version wrote would
// call that "unchanged", print a line saying it had looked, and leave exactly the population
// this story exists for on the event-only status line.
func TestInstallAddsTheRefreshIntervalToAnOlderGovernorStatusLine(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	// What a pre-GOV-B07 install left behind: our command, no interval.
	settings := readJSON(t, p.settings)
	sl, _ := settings["statusLine"].(map[string]any)
	delete(sl, "refreshInterval")
	settings["statusLine"] = sl
	writeJSON(t, p.settings, settings)

	out := installOK(t)

	after, _ := readJSON(t, p.settings)["statusLine"].(map[string]any)
	if _, ok := after["refreshInterval"]; !ok {
		t.Errorf("install over an older statusLine did not add refreshInterval: %v", after)
	}
	if !strings.Contains(out, "updated") {
		t.Errorf("install reported nothing as updated while adding the key:\n%s", out)
	}
}

// Uninstall gives back a status line that had a refreshInterval of the user's own, exactly.
//
// The byte-identical restore is asserted over the whole home directory elsewhere, but not
// with this key on both sides of the replacement. Ours is removed surgically rather than by
// restoring the backup wholesale (design.md section 13.3), so adding a subkey changes what
// "ours" looks like — and a user whose own status line refreshes every 10 s must get 10 s
// back, not our 2, and not a key we introduced to their file.
func TestUninstallRestoresAStatusLineThatHadItsOwnRefreshInterval(t *testing.T) {
	const theirs = `{
  "statusLine": {
    "type": "command",
    "command": "~/bin/my-own-statusline.sh",
    "refreshInterval": 10
  }
}
`
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, theirs)

	installOK(t)

	during, _ := readJSON(t, p.settings)["statusLine"].(map[string]any)
	if during["refreshInterval"] != float64(statuslineRefreshInterval) {
		t.Errorf("installed refreshInterval is %v, want ours (%d)",
			during["refreshInterval"], statuslineRefreshInterval)
	}

	uninstallOK(t)

	got, err := os.ReadFile(p.settings)
	if err != nil {
		t.Fatalf("read settings after uninstall: %v", err)
	}
	if string(got) != theirs {
		t.Errorf("settings.json is not byte-identical after uninstall.\n got: %q\nwant: %q", got, theirs)
	}
}

// A `governor` already on the PATH is not ours to replace, and not ours to delete.
func TestInstallLeavesSomebodyElsesBinaryOnThePath(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.link))
	writeFileT(t, p.link, "#!/bin/sh\necho not ours\n")

	installOK(t)
	if got := readFileT(t, p.link); !strings.Contains(got, "not ours") {
		t.Errorf("install replaced a binary it did not put there: %q", got)
	}
	uninstallOK(t)
	if _, err := os.Stat(p.link); err != nil {
		t.Errorf("uninstall deleted a binary it did not put there: %v", err)
	}
}

// ---------------------------------------------------------------------------
// state and purge
// ---------------------------------------------------------------------------

// Plain uninstall keeps the state; --purge removes it.
//
// And samples.jsonl survives a plain uninstall either way: design.md section 13.3 says it
// goes by hand because it is the Phase 0 measurement, and an uninstall that deleted it
// would throw away the phase the setpoint is derived from.
func TestPurgeRemovesStateAndPlainUninstallDoesNot(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	mkdirAll(t, p.state)
	writeFileT(t, filepath.Join(p.state, "samples.jsonl"), "{\"sample\":1}\n")
	writeFileT(t, filepath.Join(p.state, "statusline.json"), "{}\n")
	writeFileT(t, filepath.Join(p.state, onboardMarker), "2026-09-09T00:00:00Z\n")
	mkdirAll(t, p.data)
	writeFileT(t, filepath.Join(p.data, "config.json"), "{\"schema\":1}\n")
	writeFileT(t, filepath.Join(p.data, onboardMarker), "2026-09-09T00:00:00Z\n")

	uninstallOK(t)

	if _, err := os.Stat(filepath.Join(p.state, "samples.jsonl")); err != nil {
		t.Errorf("plain uninstall removed the Phase 0 measurement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.state, "statusline.json")); err == nil {
		t.Error("plain uninstall left the status line cache behind")
	}
	if _, err := os.Stat(filepath.Join(p.data, "config.json")); err != nil {
		t.Errorf("plain uninstall removed the config, which is --purge's job: %v", err)
	}
	// The onboarding marker, in both the homes it can have (GOV-22). In the state directory
	// it goes with everything else there; in the plugin data directory it survives, because
	// reinstalling after an ordinary uninstall is the same machine and the same person, and
	// re-showing a notice they have already read and acted on is nagging.
	if _, err := os.Stat(filepath.Join(p.state, onboardMarker)); err == nil {
		t.Error("plain uninstall left the onboarding marker in the state directory")
	}
	if _, err := os.Stat(filepath.Join(p.data, onboardMarker)); err != nil {
		t.Errorf("plain uninstall removed the onboarding marker, so an ordinary reinstall "+
			"would nag a user who has already read it: %v", err)
	}

	installOK(t)
	uninstallOK(t, "--purge")

	if _, err := os.Stat(p.state); err == nil {
		t.Error("--purge left the state directory behind")
	}
	if _, err := os.Stat(p.data); err == nil {
		t.Error("--purge left the plugin data directory behind")
	}
}

// --keep-backup leaves the settings backup in place.
func TestKeepBackupLeavesTheBackup(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)

	installOK(t)
	uninstallOK(t, "--keep-backup")

	matches, _ := filepath.Glob(p.settings + governorBackupGlob)
	if len(matches) != 1 {
		t.Fatalf("found %d backups after --keep-backup, want 1", len(matches))
	}
	if got := readFileT(t, matches[0]); got != realisticSettings {
		t.Errorf("the kept backup is not the original bytes:\n%q", got)
	}
}

// The Phase 0 trace installer's backups are not this command's to delete.
//
// Two families of backup share a settings file. A `governor uninstall` that swept away
// `.governor-trace-backup-*` would leave `trace uninstall` with nothing to restore the
// user's original formatting from, and the loss would only be discovered by whoever ran the
// two in that order.
func TestUninstallLeavesTheTraceBackupsAlone(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.settings))
	writeFileT(t, p.settings, realisticSettings)
	traceBackup := p.settings + ".governor-trace-backup-20260101T000000Z"
	writeFileT(t, traceBackup, realisticSettings)

	installOK(t)
	uninstallOK(t)

	if _, err := os.Stat(traceBackup); err != nil {
		t.Errorf("uninstall deleted the trace installer's backup: %v", err)
	}
}

// ---------------------------------------------------------------------------
// driving the real binary
// ---------------------------------------------------------------------------

// realGovernor builds the shipped binary once and returns its path.
//
// The tests that start a daemon or run doctor CANNOT use the in-process runner, and the
// reason is worth recording because it looked like a product bug for a while. install
// copies os.Executable() into the plugin, and under `go test` that is the TEST binary — so
// doctor's `<hooked binary> version` ran the whole suite instead of printing three lines and
// was killed at its 2 s budget ("signal: killed", which reads exactly like the code-signature
// SIGKILL design.md section 13.4 warns about), and the daemon child ran the suite instead of
// binding a socket.
//
// Driving the real binary is also the stronger evidence, and it is what GOV-10 and GOV-20
// did for the same reason: an installer is a claim about what happens on a machine, and an
// in-process call proves it about a function.
// stagedEnv is the environment that points a governor process at a staged home.
func stagedEnv(p paths) []string {
	return append(os.Environ(),
		"HOME="+p.home,
		stateDirEnvForTest+"="+p.state,
		endpointEnvForTest+"=unix://"+filepath.Join(p.state, "sock"),
	)
}

// originalHome is the real home directory, captured before any test can stage another one.
var originalHome = os.Getenv("HOME")

var (
	realGovernorOnce sync.Once
	realGovernorPath string
	realGovernorErr  error
)

func realGovernor(t *testing.T) string {
	t.Helper()
	realGovernorOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gov-bin")
		if err != nil {
			realGovernorErr = err
			return
		}
		realGovernorPath = filepath.Join(dir, "governor")
		realGovernorErr = goBuildGovernor(realGovernorPath)
	})
	if realGovernorErr != nil {
		t.Fatalf("build governor: %v", realGovernorErr)
	}
	return realGovernorPath
}

// buildGovernorTo builds the binary to out with extra `go build` flags, for a test that needs
// one built differently — a release-style version stamp, say.
func buildGovernorTo(t *testing.T, out string, extra ...string) {
	t.Helper()
	if err := goBuildGovernor(out, extra...); err != nil {
		t.Fatalf("build governor: %v", err)
	}
}

func goBuildGovernor(out string, extra ...string) error {
	args := append([]string{"build"}, extra...)
	args = append(args, "-o", out, ".")
	cmd := exec.Command("go", args...)
	// HOME explicitly, because stagedHome has already pointed the process's HOME at a
	// temp directory by the time the first test asks for a binary. Left inherited, the
	// Go toolchain would put its module cache under that directory, write it read-only,
	// and the test's own cleanup would then fail to remove it.
	cmd.Env = append(os.Environ(), "HOME="+originalHome)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	return nil
}

// runReal runs the shipped binary against a staged home, as a real subprocess.
func runReal(t *testing.T, p paths, args ...string) (int, string, string) {
	t.Helper()
	return runRealBinary(t, realGovernor(t), p, args...)
}

// runRealBinary is runReal for a nominated binary, so an upgrade can be staged by running a
// differently built governor against a home the current one installed into.
func runRealBinary(t *testing.T, binary string, p paths, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = stagedEnv(p)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run governor %v: %v", args, err)
	}
	return code, stdout.String(), stderr.String()
}

// ---------------------------------------------------------------------------
// the daemon
// ---------------------------------------------------------------------------

// install starts the daemon and uninstall stops it.
//
// Over a real socket with a real detached process, because that is the part that cannot be
// checked in-process: the thing being asserted is that a process outlives the command that
// started it and then dies when told to. A daemon left running after uninstall is the one
// piece of residue a user cannot see in a directory listing.
func TestInstallStartsAndUninstallStopsTheDaemon(t *testing.T) {
	p := stagedHome(t)

	code, stdout, stderr := runReal(t, p, "install")
	if code != 0 {
		t.Fatalf("governor install: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Stopping it is the last thing this test does, but a failure anywhere in between must
	// not leave a daemon running past the test that started it.
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })

	endpoint := "unix://" + filepath.Join(p.state, "sock")
	if !daemonAnswers(endpoint) {
		t.Fatalf("no daemon answering on %s after install\n%s\n%s", endpoint, stdout, daemonLog(t, p))
	}
	pid, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid: %v", err)
	}

	// A second install must not start a second daemon.
	if _, out, _ := runReal(t, p, "install"); !strings.Contains(out, "already running") {
		t.Errorf("a second install did not notice the running daemon:\n%s", out)
	}
	if after, err := readDaemonPID(p.state); err != nil || after != pid {
		t.Errorf("a second install replaced the daemon: pid %d became %d (%v)", pid, after, err)
	}

	if code, out, errOut := runReal(t, p, "uninstall"); code != 0 {
		t.Fatalf("governor uninstall: exit %d\n%s\n%s", code, out, errOut)
	}

	if daemonAnswers(endpoint) {
		t.Error("a daemon is still answering after uninstall")
	}
	if _, err := os.Stat(filepath.Join(p.state, daemonPIDFile)); err == nil {
		t.Error("uninstall left the pid file behind")
	}
	if procAlive(pid) {
		t.Errorf("daemon pid %d is still running after uninstall", pid)
	}
}

// procAlive reports whether a pid is still a live process.
func procAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// daemonLog returns the daemon's log, which is where the reason for a failed start is.
func daemonLog(t *testing.T, p paths) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.state, daemonLogFile))
	if err != nil {
		return "(no daemon log: " + err.Error() + ")"
	}
	return "daemon log:\n" + string(b)
}

// --no-daemon installs the files and starts nothing.
func TestNoDaemonStartsNothing(t *testing.T) {
	p := stagedHome(t)
	out := installOK(t)

	if daemonAnswers("unix://" + filepath.Join(p.state, "sock")) {
		t.Error("--no-daemon started a daemon")
	}
	if !strings.Contains(out, "--no-daemon") {
		t.Errorf("install did not say the daemon was not started:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// doctor is the acceptance instrument
// ---------------------------------------------------------------------------

// A successful install makes `governor doctor` exit 0 with every check passing.
//
// GOV-20 built doctor before this story on purpose: it is what tells you whether the
// installer worked, and its own story note asks this one to make that an explicit check
// rather than eyeballing settings.json. It is the strongest single assertion available
// here, because it is the check-list a stranger will run and it was written without
// knowledge of how this installer would work.
//
// It runs against a REAL daemon over a REAL socket. doctor's hook check stats the binary
// the hooks name and execs it for its version, so an in-process fake would exercise none of
// what it is here to prove.
func TestDoctorPassesEveryCheckAfterAnInstall(t *testing.T) {
	p := stagedHome(t)

	code, stdout, stderr := runReal(t, p, "install")
	if code != 0 {
		t.Fatalf("governor install: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })

	code, out, _ := runReal(t, p, "doctor")
	if code != 0 {
		t.Errorf("governor doctor: exit %d after a clean install, want 0\n%s\n%s", code, out, daemonLog(t, p))
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("governor doctor reported a failure after a clean install:\n%s", out)
	}
	// A skip is not a pass. With a healthy install nothing is downstream of a failure, so
	// every check must actually have run — a report of eight skips would exit 0 too.
	if strings.Contains(out, "skip") {
		t.Errorf("governor doctor skipped a check after a clean install:\n%s", out)
	}
	if !strings.Contains(out, "all passing") {
		t.Errorf("governor doctor did not report every check passing:\n%s", out)
	}

	// And once uninstalled it must go back to failing: a doctor that passes on a machine
	// with nothing installed is a doctor that would pass on a broken one.
	if code, _, _ := runReal(t, p, "uninstall"); code != 0 {
		t.Fatal("uninstall failed")
	}
	if code, out, _ := runReal(t, p, "doctor"); code == 0 {
		t.Errorf("governor doctor exited 0 with nothing installed:\n%s", out)
	}
}

// install ends by running doctor, and fails when doctor does (GOV-22).
//
// The gap this closes: every file can land, the daemon can be started, and the machine can
// still govern nothing — a socket it cannot bind, a stale binary earlier on the PATH, a
// matcher that does not cover the real tool name. Without this, the first evidence of that
// is an ungoverned fan-out hours later, in a session where nobody is reading an installer's
// output. So the installer runs the check-list itself, on the spot, and its exit code is
// the check-list's.
func TestInstallVerifiesItselfWithDoctor(t *testing.T) {
	t.Run("a clean install prints a passing report", func(t *testing.T) {
		p := stagedHome(t)
		code, stdout, stderr := runReal(t, p, "install")
		t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })
		if code != 0 {
			t.Fatalf("governor install: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "governor doctor ---") {
			t.Errorf("install did not run doctor:\n%s", stdout)
		}
		if !strings.Contains(stdout, "all passing") {
			t.Errorf("install ran doctor but did not print a passing report:\n%s\n%s",
				stdout, daemonLog(t, p))
		}
	})

	// --no-daemon is the one case where a failing daemon check means the user got exactly
	// what they asked for, so it reports that it did not verify rather than printing a
	// report of failures it had just guaranteed.
	t.Run("--no-daemon says it did not verify", func(t *testing.T) {
		stagedHome(t)
		out := installOK(t)
		if strings.Contains(out, "governor doctor ---") {
			t.Errorf("--no-daemon ran doctor, whose daemon checks it had just made fail:\n%s", out)
		}
		if !strings.Contains(out, "Not verified") {
			t.Errorf("--no-daemon did not say the install was unverified:\n%s", out)
		}
	})

	// The one that makes the other two worth having: an install that writes every file and
	// still leaves nothing governed must exit non-zero. A state directory that cannot be
	// created gives the daemon nowhere to bind, which is a real failure mode (a $HOME long
	// enough to overrun sockaddr_un reaches the same place) and is invisible in a directory
	// listing — every plugin file is present and correct.
	t.Run("an install the daemon cannot survive exits non-zero", func(t *testing.T) {
		p := stagedHome(t)
		blocked := filepath.Join(p.home, "not-a-directory")
		if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
			t.Fatalf("stage a blocked state dir: %v", err)
		}
		cmd := exec.Command(realGovernor(t), "install")
		cmd.Env = append(os.Environ(),
			"HOME="+p.home,
			stateDirEnvForTest+"="+blocked,
			endpointEnvForTest+"=unix://"+filepath.Join(blocked, "sock"),
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })

		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("governor install exited 0 with a daemon that cannot bind\nstdout: %s\nstderr: %s",
				stdout.String(), stderr.String())
		}
		if got := exitErr.ExitCode(); got != 1 {
			t.Errorf("exit code = %d, want 1 (2 is a usage error, and this is not one)", got)
		}
		if !strings.Contains(stdout.String(), "FAIL") {
			t.Errorf("install failed without naming a failing check:\n%s", stdout.String())
		}
		// The files really did land. Without this the test would pass just as well against
		// an installer that refused early, which is a different thing entirely.
		if _, statErr := os.Stat(p.hooks); statErr != nil {
			t.Errorf("the hooks were not written, so this is not the failure being tested: %v", statErr)
		}
	})
}

// Every command doctor names as a fix is a command this binary actually has.
//
// GOV-20's own story note: "a diagnostic naming a fix that cannot work is the exact failure
// GOV-20 exists to prevent, and it found one of its own that way." Its fix strings named
// `governor install` and `governor daemon` before either existed; now that install does,
// this holds the whole set to commands in the table rather than to whichever ones somebody
// remembered to update.
func TestEveryDoctorFixNamesARealCommand(t *testing.T) {
	raw, err := os.ReadFile("doctor.go")
	if err != nil {
		t.Fatalf("read doctor.go: %v", err)
	}
	// `governor <word>` only where the text reads as a command to type: at the start of a
	// string, after a colon, after a shell `&&`, or inside backticks. Prose says things like
	// "it runs governor on every tool call in every session", and a scan that counted those
	// would report `governor on` as a missing subcommand — a guard that cries wolf is a
	// guard somebody deletes.
	re := regexp.MustCompile("(?:^|\"|`|: |&& )governor ([a-z]+)")
	named := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		named[m[1]] = true
	}
	if len(named) == 0 {
		t.Fatal("found no `governor <command>` fix in doctor.go; this test has stopped testing anything")
	}
	for name := range named {
		if !registered(name) {
			t.Errorf("doctor names `governor %s` as a fix, but it is not a subcommand of this binary", name)
		}
	}
}

// doctor finds the hooks in the directory install writes them to.
//
// The story's own trap: doctor reads a fixed list of files, and hooks written somewhere that
// list does not cover would be reported as "not registered" for a perfectly good install.
// This asserts the two agree by construction rather than by memory.
func TestDoctorLooksWhereInstallWrites(t *testing.T) {
	p := stagedHome(t)
	sources, err := hookSources()
	if err != nil {
		t.Fatalf("hookSources: %v", err)
	}
	for _, src := range sources {
		if src.path == p.hooks {
			return
		}
	}
	var paths []string
	for _, src := range sources {
		paths = append(paths, src.path)
	}
	t.Errorf("doctor does not read %s, which is where install writes the hooks; it reads %v", p.hooks, paths)
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// stateDirEnvForTest and endpointEnvForTest name the transport overrides without importing
// the package into every test that stages a home.
const (
	stateDirEnvForTest = "GOVERNOR_STATE_DIR"
	endpointEnvForTest = "GOVERNOR_ENDPOINT"
)

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFileT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(readFileT(t, path)), &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return m
}

func writeJSON(t *testing.T, path string, v map[string]any) {
	t.Helper()
	b, err := marshalSettings(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	writeFileT(t, path, string(b))
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
