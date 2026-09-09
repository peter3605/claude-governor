package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// `governor install` and `governor uninstall` (GOV-21, design.md section 13).
//
// One command in, one command out. What is being asked of a stranger here is to run our
// binary inside a hook on every subagent spawn, and the only thing that makes that a
// reasonable ask is being able to prove it comes out clean — so the test that matters in
// install_test.go is not that install works, it is that uninstall leaves the machine
// byte-identical to before it ran.
//
// WHERE THE HOOKS GO, AND WHY IT IS NOT WHERE design.md ORIGINALLY SAID.
//
// §13.3 named ~/.claude/plugins/claude-governor/ as the plugin directory. Measured on
// 2.1.265 (docs/hook-surface.md section 15) that path is INERT: ~/.claude/plugins holds
// marketplace caches, an installed_plugins.json and a data/ directory, and a directory
// dropped beside them is loaded by nothing. Registering the gate there would have been the
// exact failure GOV-01 exists to prevent — nothing errors, the install looks complete, and
// no spawn is ever governed.
//
// What does load, with no settings.json entry at all, is ~/.claude/skills/<name>/. It
// auto-loads as <name>@skills-dir, its hooks/hooks.json fires for PreToolUse WITH a matcher,
// and ${CLAUDE_PLUGIN_ROOT} and ${CLAUDE_PLUGIN_DATA} both expand inside the command string.
// All three were verified end to end before a line of this file was written; the write-up
// is hook-surface.md section 15 and design.md sections 9 and 13.3 are corrected to match.
//
// The consequence for this file is the good one: the five hooks live in a file we own
// entirely, so the only thing merged into the user's settings.json is the statusLine key.
// Everything else is a directory that either exists or does not.

// pluginName is the plugin's directory name and its name in `claude plugin list`.
const pluginName = "claude-governor"

// installedBinaryName is what the binary is called inside the plugin.
//
// The release artefacts are governor-darwin-arm64 and friends, and install.sh downloads one
// of those. It is renamed on the way in so that ${CLAUDE_PLUGIN_ROOT}/bin/governor is one
// string on every platform rather than four.
const installedBinaryName = "governor"

// daemonStartTimeout is how long install waits for a daemon it just started to answer.
//
// A daemon that has not bound its socket by then has failed for a reason the log will
// carry, and install says so rather than claiming a start it cannot see.
const daemonStartTimeout = 3 * time.Second

// daemonStopTimeout is how long uninstall waits for a signalled daemon to go away.
const daemonStopTimeout = 5 * time.Second

// paths is every location install touches, resolved once.
//
// It is a struct rather than a set of functions because design.md section 13.3 is
// normative: uninstall is only verifiable if the list of paths is exhaustive, and a list
// that is assembled in two places drifts. Everything install writes and everything
// uninstall removes comes from here.
type paths struct {
	home     string
	plugin   string // ~/.claude/skills/claude-governor
	manifest string // <plugin>/.claude-plugin/plugin.json
	hooks    string // <plugin>/hooks/hooks.json
	binary   string // <plugin>/bin/governor
	settings string // ~/.claude/settings.json
	// link is a convenience symlink onto the user's PATH. It is only ever written when its
	// parent directory ALREADY exists — see linkDir.
	link string
	// state is the governor's own directory: socket, pid file, daemon log, status cache.
	state string
	// data is ${CLAUDE_PLUGIN_DATA} for this plugin, which Claude Code creates and names.
	// Only `uninstall --purge` touches it.
	data string
}

func resolvePaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, err
	}
	state, err := transport.StateDir()
	if err != nil {
		return paths{}, err
	}
	plugin := filepath.Join(home, ".claude", "skills", pluginName)
	return paths{
		home:     home,
		plugin:   plugin,
		manifest: filepath.Join(plugin, ".claude-plugin", "plugin.json"),
		hooks:    filepath.Join(plugin, "hooks", "hooks.json"),
		binary:   filepath.Join(plugin, "bin", installedBinaryName),
		settings: filepath.Join(home, ".claude", "settings.json"),
		link:     filepath.Join(home, ".local", "bin", installedBinaryName),
		state:    state,
		// Claude Code derives this from the plugin's name and the marketplace it came
		// from; a skills-dir plugin's marketplace is the literal string "skills-dir"
		// (hook-surface.md section 15).
		data: filepath.Join(home, ".claude", "plugins", "data", pluginName+"-skills-dir"),
	}, nil
}

// stateFiles are the files inside the state directory that uninstall removes.
//
// Named individually rather than removing the directory wholesale, because design.md
// section 13.3 says samples.jsonl is removed BY HAND and not by uninstall: it is the Phase 0
// measurement, and throwing it away on uninstall would throw away the phase. The directory
// itself is removed only once it is empty, so a machine that still has its samples keeps
// them and everything else goes.
var stateFiles = []string{"sock", "statusline.json", daemonPIDFile, daemonLogFile, daemonStartFile, onboardMarker}

// ---------------------------------------------------------------------------
// the plugin's files
// ---------------------------------------------------------------------------

// pluginManifest is .claude-plugin/plugin.json, the file that makes the directory a plugin.
func pluginManifest() ([]byte, error) {
	return marshalSettings(map[string]any{
		"name":        pluginName,
		"version":     binaryVersion,
		"description": "One CPU budget behind every Claude Code session on this machine.",
	})
}

// pluginHooks is hooks/hooks.json: the five hooks of design.md section 4.1.
//
// Generated from wantedHooks rather than written out, so that install and doctor cannot
// disagree about what a correct install looks like. doctor's whole job is to tell you
// whether this file is right, and a diagnostic checking against its own private copy of the
// answer is a diagnostic that passes a broken install.
//
// The matcher is spawnToolNames joined, for the reason section 10.1 gives: the tool answers
// to Agent and Task within one version, and SendMessage is a second spawn path. A matcher
// that misses any of the three leaves that path ungoverned and nothing says so.
func pluginHooks() ([]byte, error) {
	hooks := map[string]any{}
	for _, w := range wantedHooks {
		inner := map[string]any{
			"type":    "command",
			"command": "${CLAUDE_PLUGIN_ROOT}/bin/" + installedBinaryName + " " + w.args,
		}
		// Set explicitly on the gate rather than inherited. Section 10.1 measured the
		// default at over 150 s with no established ceiling, which is not a number to
		// build a 20 s wait window on.
		if w.matched {
			inner["timeout"] = gateHookTimeout
		}
		entry := map[string]any{"hooks": []any{inner}}
		if w.matched {
			entry["matcher"] = strings.Join(spawnToolNames, "|")
		}
		hooks[w.event] = []any{entry}
	}
	return marshalSettings(map[string]any{"hooks": hooks})
}

// gateHookTimeout bounds the gate hook, in seconds (design.md section 4.1).
//
// 30 s gives the 20 s wait_max headroom. It bounds the WAIT and not the failure path — the
// 100 ms connect deadline in section 4.4 is what protects a session from a wedged daemon.
const gateHookTimeout = 30

// statusLineSetting is the settings.json value that wires up the status line.
func statusLineSetting(binary string) map[string]any {
	return map[string]any{"type": "command", "command": binary + " statusline"}
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

// step is one thing install did or would do, for the report at the end.
//
// design.md section 13.2 requires install to "print exactly what it touched", and the
// distinction between touched and already-correct is the whole of idempotency: a second run
// prints the same lines with "unchanged" against every one of them and changes no bytes.
type step struct {
	path    string
	action  string // "created", "updated", "unchanged", "skipped"
	comment string
}

type installer struct {
	p     paths
	steps []step
	// changed is whether anything on disk actually moved. It gates the settings backup:
	// a backup written on a no-op run is a file the second run left behind, which is
	// exactly the idempotency failure this command is tested against.
	changed bool
}

func (in *installer) record(path, action, comment string) {
	in.steps = append(in.steps, step{path: path, action: action, comment: comment})
	if action != "unchanged" && action != "skipped" {
		in.changed = true
	}
}

func runInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("governor install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "print what would change and write nothing")
	noDaemon := fs.Bool("no-daemon", false, "install the files but do not start the daemon")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintf(stderr, "governor install: %v\n", err)
		return 1
	}
	in := &installer{p: p}

	// Read the settings file FIRST and refuse on anything unparseable, before creating a
	// single directory. Half an install is worse than none: it leaves files behind that the
	// user did not ask for and did not get told about, and uninstall is the only thing that
	// knows they are there.
	original, settings, err := loadSettings(p.settings)
	if err != nil {
		fmt.Fprintf(stderr, "governor install: %v\n", err)
		fmt.Fprintln(stderr, "Refusing to write. Fix or move that file and run this again.")
		return 1
	}

	self, err := selfPath()
	if err != nil {
		fmt.Fprintf(stderr, "governor install: %v\n", err)
		return 1
	}

	manifest, err := pluginManifest()
	if err != nil {
		fmt.Fprintf(stderr, "governor install: manifest: %v\n", err)
		return 1
	}
	hooks, err := pluginHooks()
	if err != nil {
		fmt.Fprintf(stderr, "governor install: hooks: %v\n", err)
		return 1
	}

	// Settings are merged into an in-memory copy first, so that --dry-run can report the
	// change without any of the write path running.
	settingsChanged := mergeStatusLine(settings, p.binary)
	var settingsOut []byte
	if settingsChanged {
		if settingsOut, err = marshalSettings(settings); err != nil {
			fmt.Fprintf(stderr, "governor install: marshal settings: %v\n", err)
			return 1
		}
	}

	if *dryRun {
		in.planFile(p.manifest, manifest)
		in.planFile(p.hooks, hooks)
		in.planBinary(self)
		in.planSettings(settingsChanged)
		in.planLink()
		in.report(stdout, "would")
		if settingsChanged {
			fmt.Fprintf(stdout, "\n--- %s ---\n", p.settings)
			stdout.Write(settingsOut)
		}
		return 0
	}

	if err := in.writeFile(p.manifest, manifest); err != nil {
		fmt.Fprintf(stderr, "governor install: %v\n", err)
		return 1
	}
	if err := in.writeFile(p.hooks, hooks); err != nil {
		fmt.Fprintf(stderr, "governor install: %v\n", err)
		return 1
	}
	if err := in.installBinary(self); err != nil {
		fmt.Fprintf(stderr, "governor install: %v\n", err)
		return 1
	}
	if settingsChanged {
		// Back up before touching it, and only when there is something to back up. The
		// backup is timestamped so that a second install after a manual edit cannot
		// silently overwrite the record of what was there first.
		if len(original) > 0 {
			backup := backupName(p.settings, time.Now())
			if err := os.WriteFile(backup, original, 0o600); err != nil {
				fmt.Fprintf(stderr, "governor install: write backup: %v\n", err)
				return 1
			}
			in.record(backup, "created", "backup of your settings, restored by uninstall")
		}
		if err := writeAtomic(p.settings, settingsOut); err != nil {
			fmt.Fprintf(stderr, "governor install: write settings: %v\n", err)
			return 1
		}
		in.record(p.settings, "updated", "statusLine")
	} else {
		in.record(p.settings, "unchanged", "statusLine already points here")
	}
	if err := in.installLink(); err != nil {
		fmt.Fprintf(stderr, "governor install: %v\n", err)
		return 1
	}

	in.report(stdout, "")

	if *noDaemon {
		fmt.Fprintln(stdout, "\ndaemon not started (--no-daemon)")
	} else {
		startDaemon(p, stdout, stderr)
	}

	fmt.Fprintln(stdout, "\nRemove it with: governor uninstall")
	// The hooks are read when a session starts, so a session already running is not
	// governed yet. Saying so is the difference between "it does not work" and "restart".
	fmt.Fprintln(stdout, "Sessions already running pick the hooks up when they next start.")

	if *noDaemon {
		// Not verified, and saying so beats printing a report whose daemon-dependent
		// checks were always going to fail. --no-daemon asks for half an install on
		// purpose, and doctor cannot tell that apart from a broken one.
		fmt.Fprintln(stdout, "\nNot verified, because the daemon was not started. Check it with: governor doctor")
		return 0
	}
	return verifyInstall(stdout, stderr)
}

// verifyInstall ends the install by running doctor and printing its findings (GOV-22).
//
// An install that wrote every file and started a daemon can still be a machine that governs
// nothing: a stale binary earlier on the PATH, a matcher that does not cover the real tool
// name, a socket owned by another user. Every one of those is a check doctor already makes,
// and the cost of not making it here is that the first evidence is an ungoverned fan-out,
// hours later, in a session where nobody is reading an installer's output.
//
// The exit code is doctor's, so install.sh — and anything else scripting this — fails on a
// bad install instead of reporting success and leaving it to be found later. It is not
// reached under --no-daemon, which is the one case where a failing daemon check means the
// user got what they asked for.
func verifyInstall(stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, "\n--- governor doctor ---")
	code := runDoctor(nil, stdout, stderr)
	if code == 0 {
		return 0
	}
	fmt.Fprintln(stdout, "\nThe files are installed, but this machine is not governing anything yet.")
	fmt.Fprintln(stdout, "Fix what is named above, then run: governor doctor")
	return code
}

// planFile records what writeFile would do, without doing it.
func (in *installer) planFile(path string, want []byte) {
	if same, _ := fileHasContent(path, want); same {
		in.record(path, "unchanged", "")
		return
	}
	if _, err := os.Stat(path); err == nil {
		in.record(path, "updated", "")
		return
	}
	in.record(path, "created", "")
}

func (in *installer) planBinary(self string) {
	if same, _ := sameContent(self, in.p.binary); same {
		in.record(in.p.binary, "unchanged", "")
		return
	}
	in.record(in.p.binary, "created", "copy of "+self)
}

func (in *installer) planSettings(changed bool) {
	if !changed {
		in.record(in.p.settings, "unchanged", "statusLine already points here")
		return
	}
	if _, err := os.Stat(in.p.settings); err == nil {
		in.record(backupName(in.p.settings, time.Now()), "created", "backup of your settings")
	}
	in.record(in.p.settings, "updated", "statusLine")
}

func (in *installer) planLink() {
	dir := filepath.Dir(in.p.link)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		in.record(in.p.link, "skipped", dir+" does not exist")
		return
	}
	if target, err := os.Readlink(in.p.link); err == nil && target == in.p.binary {
		in.record(in.p.link, "unchanged", "")
		return
	}
	if _, err := os.Lstat(in.p.link); err == nil {
		in.record(in.p.link, "skipped", "something else is already there")
		return
	}
	in.record(in.p.link, "created", "→ "+in.p.binary)
}

// writeFile writes want to path, and does nothing at all when the bytes already match.
//
// Doing nothing is the point. Rewriting a file with identical content still changes its
// mtime, and "install twice changes nothing the second time" is a promise about the machine
// rather than about the file's contents.
func (in *installer) writeFile(path string, want []byte) error {
	if same, err := fileHasContent(path, want); err != nil {
		return err
	} else if same {
		in.record(path, "unchanged", "")
		return nil
	}
	existed := false
	if _, err := os.Stat(path); err == nil {
		existed = true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := writeAtomic(path, want); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if existed {
		in.record(path, "updated", "")
	} else {
		in.record(path, "created", "")
	}
	return nil
}

// installBinary copies the running executable into the plugin.
//
// NEVER a copy onto the existing file. design.md section 13.4 measured this: on macOS, `cp`
// over a running binary invalidates its code signature and the kernel then SIGKILLs every
// subsequent execution — exit 137, no output, no error anywhere. During an upgrade that
// would land on every hook of every live session at once, and a process killed by a signal
// cannot exit 0 however carefully it is written, so the fail-open guarantee goes with it.
// Writing a sibling and renaming over the old name swaps a directory entry and leaves
// running processes on the old inode.
func (in *installer) installBinary(self string) error {
	if same, err := sameContent(self, in.p.binary); err != nil {
		return err
	} else if same {
		in.record(in.p.binary, "unchanged", "")
		return nil
	}
	if self == in.p.binary {
		// Nothing to copy and nothing to compare: this IS the installed binary, being run
		// from its final home. sameContent only reported otherwise if it could not read it.
		in.record(in.p.binary, "unchanged", "")
		return nil
	}

	src, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("open %s: %w", self, err)
	}
	defer src.Close()

	dir := filepath.Dir(in.p.binary)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, installedBinaryName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close binary: %w", err)
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := os.Rename(name, in.p.binary); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	in.record(in.p.binary, "created", "copy of "+self)
	return nil
}

// installLink puts `governor` on the user's PATH, when there is somewhere obvious to put it.
//
// ~/.local/bin is created by nothing here. A directory conjured into a user's home so that
// our symlink has somewhere to live is a directory uninstall would have to decide whether to
// remove, and removing a standard PATH directory because we made it is worse than the
// convenience is worth. When it is absent the report says so and names the full path
// instead, which still works.
//
// Something else already at that name is left strictly alone. It is far more likely to be
// another tool than a stale link of ours, and the failure mode of guessing wrong is
// deleting a binary the user depends on.
func (in *installer) installLink() error {
	dir := filepath.Dir(in.p.link)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		in.record(in.p.link, "skipped", dir+" does not exist, so `governor` is not on your PATH")
		return nil
	}
	if target, err := os.Readlink(in.p.link); err == nil {
		if target == in.p.binary {
			in.record(in.p.link, "unchanged", "")
			return nil
		}
		if !strings.Contains(filepath.Base(target), "governor") {
			in.record(in.p.link, "skipped", "already links to "+target)
			return nil
		}
		// Ours, pointing somewhere stale — an install that moved. Replace it.
		if err := os.Remove(in.p.link); err != nil {
			return fmt.Errorf("replace %s: %w", in.p.link, err)
		}
	} else if _, err := os.Lstat(in.p.link); err == nil {
		in.record(in.p.link, "skipped", "something else is already at that name")
		return nil
	}
	if err := os.Symlink(in.p.binary, in.p.link); err != nil {
		return fmt.Errorf("link %s: %w", in.p.link, err)
	}
	in.record(in.p.link, "created", "→ "+in.p.binary)
	return nil
}

// report prints one line per path, aligned, with verb prefixed for --dry-run.
func (in *installer) report(w io.Writer, verb string) {
	labels := make([]string, len(in.steps))
	width := 0
	for i, s := range in.steps {
		labels[i] = s.action
		if verb != "" {
			labels[i] = verb + " " + s.action
		}
		if n := len(labels[i]); n > width {
			width = n
		}
	}
	for i, s := range in.steps {
		fmt.Fprintf(w, "%-*s  %s", width, labels[i], short(s.path))
		if s.comment != "" {
			fmt.Fprintf(w, "  (%s)", s.comment)
		}
		fmt.Fprintln(w)
	}
}

// startDaemon starts the daemon unless one is already answering.
//
// A failure to start is reported and is NOT fatal to the install. Every hook fails open
// against an unreachable daemon (invariant 8), so the machine is ungoverned rather than
// broken, and telling someone their install failed when the files are correctly in place
// would send them to undo the wrong thing. doctor is what reports it afterwards.
func startDaemon(p paths, stdout, stderr io.Writer) {
	endpoint, err := transport.DefaultEndpoint()
	if err != nil {
		fmt.Fprintf(stderr, "governor install: resolve endpoint: %v\n", err)
		return
	}
	if daemonAnswers(endpoint) {
		fmt.Fprintf(stdout, "\ndaemon already running on %s\n", endpoint)
		return
	}
	if err := os.MkdirAll(p.state, 0o700); err != nil {
		fmt.Fprintf(stderr, "governor install: create %s: %v\n", p.state, err)
		return
	}
	logPath := filepath.Join(p.state, daemonLogFile)
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "governor install: open %s: %v\n", logPath, err)
		return
	}
	defer logFile.Close()

	cmd := exec.Command(p.binary, "daemon")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid detaches it from this process group, so that closing the terminal that ran
	// install does not signal the daemon. It is a long-lived process that outlives the
	// command that started it, which is the whole point of starting it here.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "governor install: start daemon: %v\n", err)
		return
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(daemonStartTimeout)
	for time.Now().Before(deadline) {
		if daemonAnswers(endpoint) {
			fmt.Fprintf(stdout, "\nstarted the daemon on %s (log: %s)\n", endpoint, logPath)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Not a claim that it failed — only that it has not answered yet, which is what was
	// actually observed. The log is where the reason is.
	fmt.Fprintf(stdout, "\ndaemon started but has not answered on %s within %s; see %s\n",
		endpoint, daemonStartTimeout, logPath)
}

// daemonAnswers reports whether something is listening on the endpoint.
//
// A connect and an immediate close. It does not ask for status: this is called in a poll
// loop and the question is whether the socket is bound, not whether the answer is good —
// doctor is the command that judges the answer.
func daemonAnswers(endpoint string) bool {
	path := strings.TrimPrefix(endpoint, "unix://")
	c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ---------------------------------------------------------------------------
// uninstall
// ---------------------------------------------------------------------------

func runUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("governor uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	purge := fs.Bool("purge", false, "also remove the governor's state: config, samples and the plugin data directory")
	keepBackup := fs.Bool("keep-backup", false, "leave the settings backup file in place")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintf(stderr, "governor uninstall: %v\n", err)
		return 1
	}
	un := &installer{p: p}

	// The daemon first. It holds the socket and would rewrite the pid file underneath the
	// removal that follows, and a daemon left running after uninstall is the one piece of
	// residue a user cannot see in a directory listing.
	stopDaemon(p, stdout, stderr)

	if err := un.restoreSettings(*keepBackup); err != nil {
		fmt.Fprintf(stderr, "governor uninstall: %v\n", err)
		return 1
	}
	un.removeLink()
	un.removeTree(p.plugin)
	un.removeStateFiles()
	// ~/.claude/skills exists only because install put a plugin in it, and on a machine
	// that had no settings file ~/.claude exists only because install wrote one. Both are
	// removed ONLY if they are empty — os.Remove refuses otherwise, which is exactly the
	// test and is why this cannot take a directory the user is using.
	un.pruneEmpty(filepath.Dir(p.plugin), filepath.Dir(p.settings))
	if *purge {
		un.removeTree(p.state)
		un.removeTree(p.data)
	}

	un.report(stdout, "")
	if !*purge {
		fmt.Fprintf(stdout, "\nState kept: %s and %s. Remove them too with: governor uninstall --purge\n",
			short(p.state), short(p.data))
	}
	return 0
}

// restoreSettings takes the statusLine key back out.
//
// Surgically rather than by restoring the backup wholesale, because a wholesale restore
// would destroy anything the user added to settings.json since install. It then compares
// the result against the backup and, when nothing else changed, writes the backup's
// original bytes — so the ordinary case is byte-identical, formatting and key order
// included, and not merely equivalent. Go's json encoder reorders map keys, so without this
// an install-then-uninstall would leave a file that parses the same and reads differently.
func (un *installer) restoreSettings(keepBackup bool) error {
	current, settings, err := loadSettings(un.p.settings)
	if err != nil {
		return err
	}
	// Read the backup before the deletion loop below can remove it. It is what a replaced
	// statusLine is restored FROM, so losing it here would turn "put theirs back" into
	// "delete ours and theirs".
	backupPath, backup := newestGovernorBackup(un.p.settings)

	switch {
	case current == nil:
		un.record(un.p.settings, "unchanged", "no settings file")
	case !restoreStatusLine(settings, un.p.binary, backup):
		un.record(un.p.settings, "unchanged", "no governor statusLine in it")
	case len(settings) == 0 && backup == nil:
		// Nothing left in the file, and no backup — which means install found no settings
		// file and created this one. Leaving an empty `{}` behind would be residue from a
		// command whose whole promise is that it leaves none. The absent backup is what
		// makes this safe to conclude: install writes one whenever there was a file.
		if err := os.Remove(un.p.settings); err != nil {
			return fmt.Errorf("remove %s: %w", un.p.settings, err)
		}
		un.record(un.p.settings, "removed", "install created it; nothing else was in it")
	default:
		out, err := marshalSettings(settings)
		if err != nil {
			return fmt.Errorf("marshal settings: %w", err)
		}
		comment := "statusLine restored"
		// If the result parses to exactly what the backup holds, prefer the backup's BYTES:
		// same content, but the user's own formatting and key order rather than Go's. Without
		// this an install-then-uninstall leaves a file that parses the same and reads
		// differently, which is not what "byte-identical" means.
		if backup != nil && jsonEqual(backup, out) {
			out = backup
			comment = "statusLine restored, original formatting from " + filepath.Base(backupPath)
		}
		if err := writeAtomic(un.p.settings, out); err != nil {
			return fmt.Errorf("write settings: %w", err)
		}
		un.record(un.p.settings, "updated", comment)
	}

	if keepBackup {
		return nil
	}
	matches, _ := filepath.Glob(un.p.settings + governorBackupGlob)
	sort.Strings(matches)
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			return fmt.Errorf("remove %s: %w", m, err)
		}
		un.record(m, "removed", "")
	}
	return nil
}

// restoreStatusLine puts back whatever statusLine was there before install, and reports
// whether it changed anything.
//
// Install REPLACES the user's status line — there is only one, and someone who ran install
// asked for ours — so uninstall's job is not to delete a key but to undo a replacement. The
// previous value comes from the backup, which is the only record of it. When the backup had
// none, the key goes.
//
// Only ours is touched. A statusLine pointing at some other program was configured after
// install, or install's replacement was later edited by hand, and either way removing it
// would delete something we did not write. The symptom of getting this wrong — a status line
// that silently stopped working — is one nobody would connect back to an uninstall.
func restoreStatusLine(settings map[string]any, binary string, backup []byte) bool {
	existing, ok := settings["statusLine"].(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := existing["command"].(string)
	if !isGovernorStatuslineCommand(cmd, binary) {
		return false
	}
	if backup != nil {
		var previous map[string]any
		if json.Unmarshal(backup, &previous) == nil {
			if theirs, ok := previous["statusLine"]; ok {
				settings["statusLine"] = theirs
				return true
			}
		}
	}
	delete(settings, "statusLine")
	return true
}

// removeLink removes the PATH symlink, and only when it is ours.
//
// "Ours" means it is a symlink and it points at the binary we installed. A regular file at
// that name, or a link to something else, belongs to somebody else and is left alone —
// deleting a stranger's `governor` because it shares our name is precisely the residue
// problem in reverse.
func (un *installer) removeLink() {
	target, err := os.Readlink(un.p.link)
	if err != nil {
		if _, err := os.Lstat(un.p.link); err == nil {
			un.record(un.p.link, "skipped", "not a symlink we made")
		}
		return
	}
	if target != un.p.binary {
		un.record(un.p.link, "skipped", "links to "+target+", not ours")
		return
	}
	if err := os.Remove(un.p.link); err != nil {
		un.record(un.p.link, "skipped", err.Error())
		return
	}
	un.record(un.p.link, "removed", "")
}

// pruneEmpty removes each directory, deepest first, and only while it is empty.
//
// It reports nothing when there is nothing to do: a directory that is still in use is the
// normal case and not an event, and a line saying "skipped ~/.claude" on every uninstall
// would be noise on the one report that has to be readable.
func (un *installer) pruneEmpty(dirs ...string) {
	for _, dir := range dirs {
		if err := os.Remove(dir); err == nil {
			un.record(dir, "removed", "left empty")
		}
	}
}

func (un *installer) removeTree(path string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		un.record(path, "skipped", err.Error())
		return
	}
	un.record(path, "removed", "")
}

// removeStateFiles removes the governor's runtime files but keeps the Phase 0 measurement.
//
// design.md section 13.3 is explicit that samples.jsonl goes by hand and not on uninstall —
// it is the trace the setpoint is derived from, and an uninstall that deleted it would
// throw away the phase. So the files are named and the directory goes only if it empties.
func (un *installer) removeStateFiles() {
	for _, name := range stateFiles {
		path := filepath.Join(un.p.state, name)
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			un.record(path, "skipped", err.Error())
			continue
		}
		un.record(path, "removed", "")
	}
	// Removes the directory only when it is empty; Remove refuses otherwise, which is
	// exactly the test wanted and cheaper than listing it first.
	if err := os.Remove(un.p.state); err == nil {
		un.record(un.p.state, "removed", "")
	}
}

// stopDaemon signals the running daemon and waits for it to let go of the socket.
//
// SIGTERM rather than SIGKILL: the daemon installs a signal handler that closes the
// listener and drains, and killing it outright would leave the socket file behind for the
// next Listen to have to decide about.
func stopDaemon(p paths, stdout, stderr io.Writer) {
	endpoint, err := transport.DefaultEndpoint()
	if err != nil {
		return
	}
	pid, err := readDaemonPID(p.state)
	if err != nil {
		if daemonAnswers(endpoint) {
			// Something is on the socket and nothing says what it is. Reporting it is the
			// honest answer; killing whatever holds a path is not.
			fmt.Fprintf(stderr, "governor uninstall: a daemon is answering on %s but there is no pid file; stop it by hand\n", endpoint)
		}
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// Already gone; the pid file is stale and removeStateFiles collects it.
			return
		}
		fmt.Fprintf(stderr, "governor uninstall: signal daemon %d: %v\n", pid, err)
		return
	}
	deadline := time.Now().Add(daemonStopTimeout)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			fmt.Fprintf(stdout, "stopped the daemon (pid %d)\n", pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "governor uninstall: daemon %d did not exit within %s; stop it by hand\n",
		pid, daemonStopTimeout)
}

// ---------------------------------------------------------------------------
// settings
// ---------------------------------------------------------------------------

// governorBackupGlob matches the backups this command writes.
//
// Distinct from the Phase 0 trace installer's `.governor-trace-backup-*`. Two families of
// backup, two suffixes, and neither uninstall removes the other's — a `governor uninstall`
// that swept away the trace backup would leave `trace uninstall` with nothing to restore
// the user's formatting from.
const governorBackupGlob = ".governor-backup-*"

func backupName(settings string, at time.Time) string {
	return fmt.Sprintf("%s.governor-backup-%s", settings, at.UTC().Format("20060102T150405Z"))
}

// loadSettings reads and parses settings.json. A missing file is not an error — it is a
// machine that has never been configured, and install writes one.
func loadSettings(path string) ([]byte, map[string]any, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	settings := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return nil, nil, fmt.Errorf("%s is not valid JSON (%v)", path, err)
		}
	}
	return raw, settings, nil
}

// mergeStatusLine sets the statusLine key and reports whether anything changed.
//
// A statusLine that belongs to someone else is REPLACED, and the backup is what makes that
// acceptable: uninstall puts theirs back byte-for-byte. There is only one status line, so
// there is no merge to do — either the governor renders it or something else does, and a
// user who ran install asked for the former.
func mergeStatusLine(settings map[string]any, binary string) bool {
	want := statusLineSetting(binary)
	if existing, ok := settings["statusLine"].(map[string]any); ok {
		if fmt.Sprint(existing["command"]) == fmt.Sprint(want["command"]) &&
			fmt.Sprint(existing["type"]) == fmt.Sprint(want["type"]) {
			return false
		}
	}
	settings["statusLine"] = want
	return true
}

// isGovernorStatuslineCommand reports whether a statusLine command is one install wrote.
//
// Keyed on the binary's NAME plus the subcommand rather than on an exact path, for the
// reason trace_install.go gives at isTraceCommand: the path is not stable across release
// artefacts, a relocated install or a test build, and a match on one exact string leaves a
// statusLine permanently unremovable — which is the residue a reversible install exists to
// prevent.
func isGovernorStatuslineCommand(cmd, binary string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	if cmd == strings.TrimSpace(binary)+" statusline" {
		return true
	}
	if !strings.HasSuffix(cmd, " statusline") {
		return false
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(cmd, " statusline"))
	// A compound shell command that merely mentions our subcommand is not our entry, and
	// deleting it would be deleting something that works.
	if prefix == "" || strings.ContainsAny(prefix, "#;&|") {
		return false
	}
	return isGovernorBinary(prefix)
}

// newestGovernorBackup returns the path and bytes of the most recent backup, or nil.
// The names are timestamped in a sortable format, so lexical order is chronological.
func newestGovernorBackup(settings string) (string, []byte) {
	matches, err := filepath.Glob(settings + governorBackupGlob)
	if err != nil || len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches)
	newest := matches[len(matches)-1]
	b, err := os.ReadFile(newest)
	if err != nil {
		return "", nil
	}
	return newest, b
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// selfPath is the absolute path of the running executable.
func selfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this executable: %w", err)
	}
	abs, err := filepath.Abs(self)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", self, err)
	}
	return abs, nil
}

// fileHasContent reports whether path already holds exactly these bytes.
func fileHasContent(path string, want []byte) (bool, error) {
	got, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return bytes.Equal(got, want), nil
}

// sameContent reports whether two files hold identical bytes.
//
// By digest rather than by size and mtime: a rebuild of the same source produces a binary
// of the same size at a different time, and both of the wrong answers here are bad — a
// needless copy on every install, or a stale binary left in place after an upgrade.
func sameContent(a, b string) (bool, error) {
	ha, err := digest(a)
	if err != nil {
		return false, err
	}
	hb, err := digest(b)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, nil
	}
	return ha == hb, nil
}

func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// readDaemonPID reads the daemon's pid file.
func readDaemonPID(state string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(state, daemonPIDFile))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pid file does not hold a pid")
	}
	return pid, nil
}
