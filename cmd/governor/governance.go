package main

import "time"

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
	// govNeverLoaded is the defect: the session started before the hooks file existed, so it
	// cannot have loaded the gate and never will.
	govNeverLoaded
	// govHookNeverArrived is a session that started after both the hooks and this daemon and
	// still never registered. Its SessionStart hook should have arrived and did not.
	govHookNeverArrived
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
	default:
		return ""
	}
}

// classify decides one live session's governance from a status reply.
//
// It is deliberately conservative in one direction: anything it cannot establish comes back
// as unknown or benign rather than as a fault. A false "ungoverned" costs a person a
// needless session restart and, worse, teaches them to disbelieve the one line that would
// have told them the truth the day it mattered.
func classify(reply statusResponse, s sessionStat) governance {
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
		return govNeverLoaded
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
