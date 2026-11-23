package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The hooks history (GOV-B52): every write of the file holding the gate, and what it held.
//
// WHY IT EXISTS. GOV-29 decides whether a live, unregistered session could have loaded the
// gate by comparing its start against gateHooksInstalledAt, the mtime of the file holding the
// gate. That mtime is the LAST rewrite of the hooks, not the first install. Every upgrade that
// changes hooks.json moves it to now, so a session that loaded the previous release's hooks —
// and has been running its gate ever since — was reported "ungoverned (started before the hooks
// were installed)". Measured 2026-11-23 after the 0.1.8 upgrade: earbrief and stepstool,
// started on 11-21 under the 0.1.6 hooks, FAILed doctor although earbrief had build release
// lines in daemon.log that same day. They were governed, only without the UserPromptSubmit
// settle hook 0.1.7 added at 06:11 UTC on 11-22.
//
// One mtime cannot tell those apart, and keeping it still across a rewrite would be worse: a
// session that predates a hooks CHANGE really is missing the new hook and should be told so.
// So install, the only writer of hooks.json, keeps a second record beside it: each instant the
// file was written, with the hooks it held from then on. A session that started after the
// first entry loaded SOME set of the governor's hooks, and the entry in force at its start
// says which — the difference against the newest entry is what it lacks.
//
// SEEDED ENTRIES ARE LOWER BOUNDS. A machine installed by a build older than this one has a
// hooks file and no history. install then records what it finds — the old file's mtime and
// hooks — marked seeded: the gate has existed AT LEAST since then, and may have existed in an
// earlier form before. A session that started before a seeded first entry is unknown, never
// ungoverned, because the history cannot say whether a gate existed when it started. Only an
// unseeded first entry — written by an install that found no gate anywhere — is a true
// "first installed" instant.
//
// It lives in the state directory (design.md section 13.3) and is removed by a plain
// uninstall: a reinstall after uninstall is a first install again, which is the truth about
// any session older than it, whose hooks pointed at a binary uninstall removed.
const (
	hookHistoryFile = "hooks-history.json"
	// Fixed, not random, for surface.json.tmp's reason: uninstall removes files it can name.
	hookHistoryTempFile = hookHistoryFile + ".tmp"
)

// hookEpoch is one write of the gate's hooks file.
type hookEpoch struct {
	// At is the file's mtime after the write, RFC3339 — the same clock and the same instant
	// gateHooksInstalledAt reports, so the newest entry and hooks_installed agree.
	At string `json:"at"`
	// Hooks are the governor hooks the file held from then on, as "<event> <subcommand>".
	Hooks []string `json:"hooks"`
	// Seeded marks an entry recorded after the fact for a file this history did not see
	// written. See the comment above.
	Seeded bool `json:"seeded,omitempty"`
}

// hookHistoryCap bounds the file. Only the first entry and the recent ones are ever
// consulted for a live session, and a machine upgrades a few times a month.
const hookHistoryCap = 32

// readHookHistory reads the history from the state directory. A missing file is (nil, nil),
// an unreadable or malformed one is an error; both mean "no evidence" to the classifier.
func readHookHistory(state string) ([]hookEpoch, error) {
	raw, err := os.ReadFile(filepath.Join(state, hookHistoryFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h []hookEpoch
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	for _, e := range h {
		if parseStatusTime(e.At).IsZero() {
			return nil, errors.New("hooks history: unreadable instant " + e.At)
		}
	}
	sort.SliceStable(h, func(i, j int) bool {
		return parseStatusTime(h[i].At).Before(parseStatusTime(h[j].At))
	})
	return h, nil
}

// writeHookHistory replaces the history, staging through a fixed name.
func writeHookHistory(state string, h []hookEpoch) error {
	if len(h) > hookHistoryCap {
		// Keep the first entry — it is the one that says whether a gate existed at all — and
		// the newest the rest of the cap allows.
		h = append([]hookEpoch{h[0]}, h[len(h)-hookHistoryCap+1:]...)
	}
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(state, hookHistoryTempFile)
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(state, hookHistoryFile)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// gateHookNames is the governor hooks a hooks file holds, sorted, and whether one of them is
// the gate. Matched on event and subcommand, the pair gateHooksInstalledAt uses.
func gateHookNames(src hookSource) (names []string, holdsGate bool) {
	refs, err := scanHooks(src)
	if err != nil {
		return nil, false
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if r.event == "" || r.args == "" {
			continue
		}
		if r.event == "PreToolUse" && r.args == "gate" {
			holdsGate = true
		}
		n := r.event + " " + r.args
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, holdsGate
}

// hookFileBefore is what install saw of the gate's hooks file before writing it.
type hookFileBefore struct {
	// anyGate is whether ANY hook source held the gate — settings.json, a legacy tree. False
	// only on a machine that has never had a governor gate registered, which is what makes
	// the entry written after it a true first install.
	anyGate bool
	// at and hooks describe the plugin's own hooks file, when it held the gate.
	at    time.Time
	hooks []string
}

// observeHookFileBefore reads the machine as install finds it, before it writes anything.
func observeHookFileBefore(hooksPath, pluginRoot string) hookFileBefore {
	var b hookFileBefore
	b.anyGate = !gateHooksInstalledAt().IsZero()
	names, gate := gateHookNames(hookSource{path: hooksPath, pluginRoot: pluginRoot})
	if gate {
		if fi, err := os.Stat(hooksPath); err == nil {
			b.at, b.hooks = fi.ModTime(), names
		}
	}
	return b
}

// recordHookHistory brings the history up to date after install has written the hooks file.
//
// It never fails the install. The history is evidence for a diagnostic; a governor without it
// gates every spawn exactly as well, and doctor reads a missing history as unknown rather than
// as a fault. An install refused over it would be the bookkeeping taking the machine
// ungoverned.
func recordHookHistory(state, hooksPath, pluginRoot string, before hookFileBefore) (bool, error) {
	names, gate := gateHookNames(hookSource{path: hooksPath, pluginRoot: pluginRoot})
	if !gate {
		return false, nil
	}
	fi, err := os.Stat(hooksPath)
	if err != nil {
		return false, err
	}
	now := fi.ModTime().UTC().Format(time.RFC3339)

	h, err := readHookHistory(state)
	if err != nil {
		// Malformed: start again from what can be seen, seeded, rather than keep a file that
		// answers nothing. Seeded is the conservative reading of a history we lost.
		h = nil
		before.anyGate = true
	}
	seeded := false
	if len(h) == 0 {
		switch {
		case !before.at.IsZero():
			// A hooks file this history never saw written. It has held the gate at least
			// since its mtime.
			h = append(h, hookEpoch{At: before.at.UTC().Format(time.RFC3339), Hooks: before.hooks, Seeded: true})
			seeded = true
		case before.anyGate:
			// A gate somewhere else — settings.json, a pre-rename tree. A gate existed before
			// this write, at an instant this cannot name, so the entry below is seeded too.
			return true, writeHookHistory(state, []hookEpoch{{At: now, Hooks: names, Seeded: true}})
		default:
			// No gate anywhere before this write: this is the first install, and the one
			// entry that can say a session predates every gate.
			return true, writeHookHistory(state, []hookEpoch{{At: now, Hooks: names}})
		}
	}
	last := h[len(h)-1]
	switch {
	case last.At == now && equalStrings(last.Hooks, names):
		if !seeded {
			return false, nil // nothing moved: install twice changes nothing the second time
		}
	case last.At == now:
		h[len(h)-1].Hooks = names
	default:
		h = append(h, hookEpoch{At: now, Hooks: names})
	}
	return true, writeHookHistory(state, h)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hooksAt returns the epoch in force at t — the newest entry not after it — or false when t
// predates the whole history.
func hooksAt(h []hookEpoch, t time.Time) (hookEpoch, bool) {
	var in hookEpoch
	found := false
	for _, e := range h {
		if parseStatusTime(e.At).After(t) {
			break
		}
		in, found = e, true
	}
	return in, found
}

// missingHooks is what the newest epoch has and the given one does not.
func missingHooks(h []hookEpoch, had hookEpoch) []string {
	if len(h) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, n := range had.Hooks {
		have[n] = true
	}
	var out []string
	for _, n := range h[len(h)-1].Hooks {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}
