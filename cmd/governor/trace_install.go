package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Installing and removing the Phase 0 hooks.
//
// This is NOT `governor install` (GOV-18). That command installs the plugin, the gate, the
// daemon and the statusline, and it is a different story. This one installs three
// instrumentation hooks and nothing else, and exists mostly so that the thing being asked of
// a machine — run our binary on every subagent start and stop — is undoable by running one
// command rather than by hand-editing JSON and hoping.
//
// The rule from CLAUDE.md that shapes all of it: never blind-write the user's settings.
// Read, back up, merge, write atomically, keep the backup.

// traceHookSuffixes are the command tails this installer writes. Uninstall removes hook
// entries ending in one of them and leaves everything else untouched, so a settings file
// that has grown other hooks since install still comes out correct.
var traceHookSuffixes = []string{" trace agent", " trace ensure", " trace sample"}

// traceEvents are the three hooks Phase 0 installs, in the order they are reported.
//
// SubagentStart and SubagentStop are the observations. SessionStart is not an observation at
// all — it is what starts the sampler, so that Phase 0 has no launchd job, no systemd unit
// and nothing that outlives Claude Code.
var traceEvents = []struct{ event, args string }{
	{"SubagentStart", "trace agent"},
	{"SubagentStop", "trace agent"},
	{"SessionStart", "trace ensure"},
}

// traceHookTimeout bounds each hook in seconds. It is set explicitly rather than inherited,
// for the reason design.md section 10.1 gives: the default timeout was measured at over
// 150 s with no established ceiling, which is not a number to build on. These bodies have
// their own internal watchdog at 3 s; this is the belt to that pair of braces.
const traceHookTimeout = 10

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func backupGlob() (string, error) {
	p, err := settingsPath()
	if err != nil {
		return "", err
	}
	return p + ".governor-trace-backup-*", nil
}

// hookCommand is the command string for one installed hook.
func hookCommand(self, args string) string { return self + " " + args }

// runTraceInstall merges the Phase 0 hooks into ~/.claude/settings.json.
//
// Idempotent: an entry whose command already matches is left alone rather than added again.
// Running it twice is a no-op the second time, which is invariant 7 applied to the part of
// the machine this story actually touches.
//
// --dry-run prints the merged file to stdout and writes nothing, which is how the change
// gets reviewed before it is made.
func runTraceInstall(args []string, stdout, stderr io.Writer) int {
	dryRun := false
	for _, a := range args {
		if a == "--dry-run" || a == "-n" {
			dryRun = true
		}
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace install: executable: %v\n", err)
		return 1
	}
	self, err = filepath.Abs(self)
	if err != nil {
		fmt.Fprintf(stderr, "governor trace install: abs path: %v\n", err)
		return 1
	}

	path, err := settingsPath()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace install: settings path: %v\n", err)
		return 1
	}

	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "governor trace install: read %s: %v\n", path, err)
		return 1
	}

	settings := map[string]any{}
	if len(bytes.TrimSpace(original)) > 0 {
		if err := json.Unmarshal(original, &settings); err != nil {
			// Refusing here is the whole point of not blind-writing. A settings file we
			// cannot parse is one we cannot safely merge into, and overwriting it with our
			// own idea of its contents would destroy configuration we never read.
			fmt.Fprintf(stderr, "governor trace install: %s is not valid JSON (%v).\n", path, err)
			fmt.Fprintln(stderr, "Refusing to write. Fix or move the file and run this again.")
			return 1
		}
	}

	added, merged := mergeTraceHooks(settings, self)

	out, err := marshalSettings(merged)
	if err != nil {
		fmt.Fprintf(stderr, "governor trace install: marshal: %v\n", err)
		return 1
	}

	if dryRun {
		fmt.Fprintf(stdout, "would write %s\n", path)
		fmt.Fprintf(stdout, "hooks to add: %d\n", added)
		fmt.Fprintln(stdout, "---")
		stdout.Write(out)
		return 0
	}

	if added == 0 {
		fmt.Fprintf(stdout, "already installed, nothing to do (%s)\n", path)
		return 0
	}

	// Back up before touching anything, and only when there is something to back up. The
	// backup is timestamped rather than fixed so that a second install after a manual edit
	// cannot silently overwrite the record of what was there first.
	if len(original) > 0 {
		backup := fmt.Sprintf("%s.governor-trace-backup-%s", path, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(backup, original, 0o600); err != nil {
			fmt.Fprintf(stderr, "governor trace install: write backup: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "backed up %s\n", backup)
	}

	if err := writeAtomic(path, out); err != nil {
		fmt.Fprintf(stderr, "governor trace install: write: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "installed %d hook(s) into %s\n", added, path)
	for _, e := range traceEvents {
		fmt.Fprintf(stdout, "  %-14s %s\n", e.event, hookCommand(self, e.args))
	}
	fmt.Fprintln(stdout, "remove with: governor trace uninstall")
	return 0
}

// mergeTraceHooks adds any missing Phase 0 hook to settings and reports how many it added.
//
// It navigates the settings structure defensively: any node that is not the shape it
// expects is replaced rather than reached into, because a panic here happens while holding
// the user's only copy of their configuration.
func mergeTraceHooks(settings map[string]any, self string) (int, map[string]any) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	added := 0
	for _, e := range traceEvents {
		want := hookCommand(self, e.args)

		entries, _ := hooks[e.event].([]any)
		if hasHookCommand(entries, want) {
			continue
		}
		entries = append(entries, map[string]any{
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": want,
				"timeout": traceHookTimeout,
			}},
		})
		hooks[e.event] = entries
		added++
	}

	settings["hooks"] = hooks
	return added, settings
}

// hasHookCommand reports whether any hook under these matcher entries runs cmd.
func hasHookCommand(entries []any, cmd string) bool {
	for _, entry := range entries {
		m, _ := entry.(map[string]any)
		if m == nil {
			continue
		}
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			if s, _ := hm["command"].(string); s == cmd {
				return true
			}
		}
	}
	return false
}

// runTraceUninstall removes the Phase 0 hooks.
//
// It removes our entries surgically rather than restoring the backup wholesale, because
// anything the user added to settings.json after install would be destroyed by a wholesale
// restore. It then compares the result against the backup, and if nothing else changed it
// writes the backup's original bytes verbatim — so the common case really does leave the
// file byte-identical, formatting and key order included, rather than merely equivalent.
func runTraceUninstall(args []string, stdout, stderr io.Writer) int {
	keepBackup := false
	for _, a := range args {
		if a == "--keep-backup" {
			keepBackup = true
		}
	}

	path, err := settingsPath()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace uninstall: settings path: %v\n", err)
		return 1
	}
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stdout, "no settings file at %s, nothing to do\n", path)
			return 0
		}
		fmt.Fprintf(stderr, "governor trace uninstall: read: %v\n", err)
		return 1
	}

	settings := map[string]any{}
	if err := json.Unmarshal(current, &settings); err != nil {
		fmt.Fprintf(stderr, "governor trace uninstall: %s is not valid JSON (%v). Refusing to write.\n", path, err)
		return 1
	}

	removed := removeTraceHooks(settings)
	if removed == 0 {
		fmt.Fprintf(stdout, "no governor trace hooks in %s, nothing to do\n", path)
	} else {
		out, err := marshalSettings(settings)
		if err != nil {
			fmt.Fprintf(stderr, "governor trace uninstall: marshal: %v\n", err)
			return 1
		}

		// If the backup parses to exactly what we are about to write, prefer the backup's
		// bytes: same content, but the user's original formatting rather than ours.
		backup, restored := newestBackup(path)
		if restored != nil && jsonEqual(restored, out) {
			out = restored
			fmt.Fprintf(stdout, "restored original formatting from %s\n", backup)
		}

		if err := writeAtomic(path, out); err != nil {
			fmt.Fprintf(stderr, "governor trace uninstall: write: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %d hook(s) from %s\n", removed, path)
	}

	if !keepBackup {
		pattern, err := backupGlob()
		if err == nil {
			matches, _ := filepath.Glob(pattern)
			for _, m := range matches {
				if err := os.Remove(m); err != nil {
					fmt.Fprintf(stderr, "governor trace uninstall: remove %s: %v\n", m, err)
					continue
				}
				fmt.Fprintf(stdout, "removed backup %s\n", m)
			}
		}
	}
	return 0
}

// removeTraceHooks strips every hook entry whose command is one of ours, and reports how
// many it removed. An event left with no entries is deleted entirely, and a hooks object
// left empty is deleted too, so uninstall does not leave an empty scaffold behind.
func removeTraceHooks(settings map[string]any) int {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return 0
	}

	removed := 0
	for _, e := range traceEvents {
		entries, _ := hooks[e.event].([]any)
		if entries == nil {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, entry := range entries {
			if n := stripOurHooks(entry); n > 0 {
				removed += n
				if entryIsEmpty(entry) {
					continue
				}
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(hooks, e.event)
			continue
		}
		hooks[e.event] = kept
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return removed
}

// stripOurHooks removes our commands from one matcher entry, in place, and reports how many
// it removed.
func stripOurHooks(entry any) int {
	m, _ := entry.(map[string]any)
	if m == nil {
		return 0
	}
	inner, _ := m["hooks"].([]any)
	if inner == nil {
		return 0
	}
	kept := make([]any, 0, len(inner))
	removed := 0
	for _, h := range inner {
		hm, _ := h.(map[string]any)
		if hm != nil {
			if s, _ := hm["command"].(string); isTraceCommand(s) {
				removed++
				continue
			}
		}
		kept = append(kept, h)
	}
	if removed > 0 {
		m["hooks"] = kept
	}
	return removed
}

// isTraceCommand reports whether a hook command string is one this installer wrote.
//
// Ownership is decided by the argument tail plus the binary's name, and NOT by an exact
// match against our own path, because the path is not stable: the release binaries are named
// governor-darwin-arm64 and friends, install.sh may place one anywhere, and `go test` builds
// it as governor.test. A matcher keyed on the exact name "governor" leaves hooks installed
// from any of those permanently unremovable — which is precisely the residue the reversible
// install exists to prevent.
//
// It stays narrow in the other direction too. GOV-18's `governor gate` hook has no trace
// tail and is not ours to remove, and a command from an unrelated tool does not have
// "governor" in its binary name.
func isTraceCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	prefix := ""
	matched := false
	for _, suffix := range traceHookSuffixes {
		if strings.HasSuffix(cmd, suffix) {
			prefix, matched = strings.TrimSpace(strings.TrimSuffix(cmd, suffix)), true
			break
		}
	}
	if !matched || prefix == "" {
		return false
	}
	// A compound shell command that merely mentions our subcommand — in a comment, or after
	// a separator — is not our hook entry, and deleting it would be deleting someone's
	// working configuration.
	if strings.ContainsAny(prefix, "#;&|") {
		return false
	}
	return strings.Contains(strings.ToLower(filepath.Base(prefix)), "governor")
}

func entryIsEmpty(entry any) bool {
	m, _ := entry.(map[string]any)
	if m == nil {
		return false
	}
	inner, _ := m["hooks"].([]any)
	return len(inner) == 0
}

// newestBackup returns the path and contents of the most recent backup, or nil if there is
// none. The names are timestamped in a sortable format, so lexical order is chronological.
func newestBackup(settings string) (string, []byte) {
	matches, err := filepath.Glob(settings + ".governor-trace-backup-*")
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

// jsonEqual reports whether two JSON documents carry the same content, ignoring formatting.
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// marshalSettings renders the settings file.
//
// HTML escaping is off. encoding/json turns &, < and > into \u0026 and friends by default,
// which is right for a value being interpolated into a web page and wrong for a config file:
// it would silently rewrite the user's own prose the first time we merged a hook into a file
// that happened to contain an ampersand. Nothing here is ever served as HTML.
func marshalSettings(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode already terminates with a newline.
	return buf.Bytes(), nil
}

// writeAtomic writes to a temp file in the same directory and renames it over the target.
// Same directory matters: rename is only atomic within a filesystem, and a temp directory
// may well be on another one.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// Sync before rename: a rename that lands before the data does leaves a valid directory
	// entry pointing at an empty file, which is the one outcome worse than a failed write.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Preserve the original mode if there was an original.
	if fi, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmp, fi.Mode().Perm())
	} else {
		_ = os.Chmod(tmp, 0o644)
	}
	return os.Rename(tmp, path)
}
