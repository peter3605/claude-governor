package main

import (
	"strings"
	"time"
)

// Whether a session's hooks are loaded, and how confidently that is known (GOV-29).
//
// Hooks are bound at session start (docs/hook-surface.md section 17). A Claude Code session
// that was already open when the governor was installed never loaded the gate and is
// ungoverned for the rest of its life — no restart of the daemon, no reinstall and no
// upgrade changes that. Only restarting the session does.
//
// This lives in one place because two surfaces answer the same question and must not drift:
// `governor doctor` reports it for every session on the machine, and the status line reports
// it for the one session it is being rendered in. Two copies of this reasoning would be two
// chances to disagree about whether a machine is governed, on the two surfaces a person uses
// to decide exactly that.
type governance int

const (
	// govUnknown is "this cannot be answered here" — no registry, or a daemon that predates
	// the fields. It is never rendered as either good or bad news.
	govUnknown governance = iota
	// govRegistered is the only positive proof: this session's SessionStart hook reached
	// this daemon, so its hooks are loaded.
	govRegistered
	// govPreviousDaemon is a session that is not in this daemon's table but started before
	// this daemon did, so it registered with one that has since been replaced.
	//
	// This is BENIGN and reporting it as a fault would be a serious bug. The session table is
	// in memory and `governor install` restarts the daemon on every upgrade, so this is the
	// ordinary state of every live session immediately after an upgrade. Treating it as a
	// fault would tell a person to restart every session on the machine every time they
	// upgraded — the same instrument lying, pointing the other way.
	govPreviousDaemon
	// govNeverLoaded is the defect: the session started before any gate hook existed, so it
	// cannot have loaded the gate and never will.
	//
	// Since GOV-B52 this needs the hooks history to say so. hooks_installed is the LAST write
	// of the hooks file, and a session that started before it may equally have loaded the
	// previous release's hooks — that is govOlderHooks, and the two need opposite words.
	govNeverLoaded
	// govHookNeverArrived is a session that started after both the hooks and this daemon and
	// still never registered. Its SessionStart hook should have arrived and did not.
	govHookNeverArrived
	// govOlderHooks is a session that loaded an earlier set of the governor's hooks: it
	// started after the first gate was installed and before the latest rewrite of the hooks
	// file (GOV-B52). Its gate runs; what it lacks is whatever changed since, which
	// missingHooksFor names. NOT ungoverned, and never a FAIL: telling a person a session
	// whose gate is visibly running is ungoverned is the false alarm that teaches them to
	// ignore the line. But not silent either — such a session really is missing a hook
	// (GOV-B41's settle, the measured case), and a restart is what picks it up.
	govOlderHooks
)

// ungoverned reports whether this verdict means the session's spawns are not being governed.
func (g governance) ungoverned() bool {
	return g == govNeverLoaded || g == govHookNeverArrived
}

// why states the verdict in the words a person needs, or "" where there is nothing to say.
func (g governance) why() string {
	switch g {
	case govNeverLoaded:
		return "started before the hooks were installed"
	case govHookNeverArrived:
		return "its SessionStart hook never reached the daemon"
	case govOlderHooks:
		return "loaded an older set of the governor's hooks"
	default:
		return ""
	}
}

// classifyGovernance decides one live session's governance from a status reply.
//
// It is deliberately conservative in one direction: anything it cannot establish comes back
// as unknown or benign rather than as a fault. A false "ungoverned" costs a person a
// needless session restart and, worse, teaches them to disbelieve the one line that would
// have told them the truth the day it mattered.
func classifyGovernance(reply statusResponse, s sessionStat) governance {
	if !reply.SessionsKnown {
		return govUnknown
	}
	if s.Registered {
		return govRegistered
	}
	if s.Tentative+s.Confirmed > 0 {
		// It holds leases. Whatever the session table says, this session's gate hook demonstrably
		// ran and demonstrably reached this daemon — the leases are the daemon's own record of
		// it. Snapshot invents a PID-less entry for a lease whose session it never saw
		// registered (lease/table.go), so this is reachable whenever a SessionStart was lost
		// but the gate is working perfectly, and calling that "ungoverned" would contradict
		// evidence the daemon is holding in its hand.
		return govRegistered
	}
	started := parseStatusTime(s.Started)
	if started.IsZero() {
		// Live, unregistered, and no start time to place it against either marker. There is
		// no honest verdict here.
		return govUnknown
	}
	if hooks := parseStatusTime(reply.HooksInstalled); !hooks.IsZero() && started.Before(hooks) {
		return beforeLatestHooks(reply, started)
	}
	// Tested on the PARSED value, not on the raw string. Testing the string for emptiness
	// while the branch above tested the parse meant a daemon_started this binary could not
	// parse — a zone-less timestamp from another build, say — skipped the benign branch,
	// failed the emptiness guard, and fell through to a confident fault. An input that cannot
	// be read must reach unknown, which is the rule every other branch here follows.
	daemonAt := parseStatusTime(reply.DaemonStarted)
	if daemonAt.IsZero() {
		// Without knowing when the daemon came up, "not registered" cannot be told apart
		// from "registered with a daemon that has gone".
		return govUnknown
	}
	if started.Before(daemonAt) {
		return govPreviousDaemon
	}
	if time.Since(started) < registrationGrace {
		// Young enough that its SessionStart may still be in flight. The daemon reads the
		// lease table and the registry at slightly different instants, so a session that
		// registers between the two shows up live and unregistered for one status reply — and
		// the statusline caches that for a second, which is long enough for the first thing a
		// brand-new session ever shows its user to be an accusation that it is broken.
		return govUnknown
	}
	return govHookNeverArrived
}

// beforeLatestHooks decides a session that started before the latest write of the gate's
// hooks file, from the hooks history (GOV-B52, hookhistory.go). That mtime alone used to
// decide it, as "never loaded", and after every upgrade that changed hooks.json it called
// every session older than the upgrade ungoverned — including ones whose gate had been
// running for days.
//
// Three answers, and the evidence for each is the history's first entry:
//   - started before an UNSEEDED first entry, written by an install that found no gate
//     anywhere: no gate existed when it started. Ungoverned, and only a restart fixes it.
//   - started before a SEEDED first entry: the history begins after the fact, so it cannot
//     say whether a gate existed then. Unknown.
//   - started at or after the first entry: it loaded the set in force at its start. Older
//     hooks, governed.
//
// No history to read is unknown, never ungoverned: the conservative rule of this file.
func beforeLatestHooks(reply statusResponse, started time.Time) governance {
	if !reply.HookHistoryKnown || len(reply.HookHistory) == 0 {
		return govUnknown
	}
	first := reply.HookHistory[0]
	firstAt := parseStatusTime(first.At)
	if firstAt.IsZero() {
		return govUnknown
	}
	if started.Before(firstAt) {
		if first.Seeded {
			return govUnknown
		}
		return govNeverLoaded
	}
	return govOlderHooks
}

// missingHooksFor names what a govOlderHooks session lacks: the hooks the newest history
// entry holds that the entry in force at its start did not. Empty when that is not knowable
// or nothing was added — a rewrite can change a matcher without adding a hook.
func missingHooksFor(reply statusResponse, s sessionStat) []string {
	started := parseStatusTime(s.Started)
	if started.IsZero() || !reply.HookHistoryKnown {
		return nil
	}
	had, ok := hooksAt(reply.HookHistory, started)
	if !ok {
		return nil
	}
	return missingHooks(reply.HookHistory, had)
}

// olderHooksDetail is the words for one govOlderHooks session: what it lacks, when known.
func olderHooksDetail(reply statusResponse, s sessionStat) string {
	switch m := missingHooksFor(reply, s); len(m) {
	case 0:
		return "missing whatever changed since"
	case 1:
		return "missing the " + m[0] + " hook"
	default:
		return "missing the " + strings.Join(m, ", ") + " hooks"
	}
}

// registrationGrace is how long after a session starts its SessionStart is still allowed to be
// in flight before absence is read as a fault. Generously longer than the hook's own 100 ms
// budget, because the machine this runs on is by definition one under load.
const registrationGrace = 10 * time.Second

// parseStatusTime reads one of the RFC3339 instants in a status reply, or the zero time.
func parseStatusTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
