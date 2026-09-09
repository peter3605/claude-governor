package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// Lazy daemon start (GOV-11). design.md section 2 has always described the daemon as
// "lazily started by the gate"; until this landed, nothing started it but `governor
// install`.
//
// The gap that closes is not exotic. The daemon is an ordinary user process: it goes away
// on reboot, on a crash, on a `pkill`, and on anything that takes the machine down
// uncleanly. Before this, every one of those left the governor silently off until somebody
// noticed the status line and ran install again — and "silently off" is the exact failure
// mode this project exists to avoid, because the hooks keep passing, doctor is not being
// watched, and nothing in a session ever says the budget stopped applying.
//
// EVERYTHING HERE IS SUBORDINATE TO INVARIANT 1, and the ordering is what makes that true
// rather than aspirational. By the time any of this runs the gate has already failed open:
// the dial failed, the spawn is proceeding ungoverned, and no code below can change that.
// Nothing waits for the daemon to bind, nothing retries, and nothing reports a failure to
// start as anything but a line on stderr. The daemon being started is for the NEXT spawn.
//
// TWO GATES RACING IS EXPECTED, AND IS NOT DEFENDED AGAINST HERE. A fan-out of ten spawns
// against a dead daemon can put several gates in this file at the same instant, and each
// will fork. transport.Listen already probes an existing socket before unlinking it and
// refuses to bind when something answers, so the losers exit with an error into the daemon
// log rather than stealing the socket from the winner. A lock file would be a second
// mechanism that has to agree with that one, and the one it would duplicate is the one that
// is actually load-bearing — the same reason `governor install` does not take one either.

// lazyStartCooldown is how long one start attempt suppresses the next.
//
// It is a rate limit rather than a lock, and it is not there for the race above. What it
// bounds is the other shape: a daemon that cannot start AT ALL — a corrupt binary, a socket
// path past sockaddr_un's limit, a read-only home — where without it every subagent spawn
// on the machine forks a process that immediately dies.
//
// Short on purpose, because being too long has a cost of its own: while it is in force a
// machine whose daemon has just died stays ungoverned. Five seconds collapses a ten-agent
// fan-out to roughly one attempt and costs at most a handful of ungoverned spawns.
const lazyStartCooldown = 5 * time.Second

// daemonStartFile records when a lazy start was last attempted (design.md section 13.3).
//
// The decision reads its modification time; the timestamp inside it is for whoever is
// looking at the directory wondering why their daemon is not coming back.
const daemonStartFile = "daemon.start"

// lazilyStartDaemon starts a daemon for the endpoint this gate could not reach.
//
// It writes only to diag — never to stdout, which for the gate is the deny channel
// (design.md section 4.2) — and it reports nothing to its caller, because there is no
// decision left to make.
func lazilyStartDaemon(diag io.Writer) {
	endpoint, err := transport.DefaultEndpoint()
	if err != nil {
		fmt.Fprintf(diag, "governor gate: resolve endpoint: %v\n", err)
		return
	}
	state, err := stateDirFor(endpoint)
	if err != nil {
		fmt.Fprintf(diag, "governor gate: %v: not starting a daemon\n", err)
		return
	}
	// An absent state directory means the governor is not installed. The likeliest way to
	// reach this is an uninstall that ran while a live session still holds the hooks it
	// registered, and creating the directory here would put back a path uninstall had just
	// removed — the rule the statusline already follows for its cache (design.md section
	// 13.3). Starting a daemon for an uninstalled governor is worse than an ungoverned
	// machine: it is a process the user has no reason to expect and no obvious way to find.
	if fi, err := os.Stat(state); err != nil || !fi.IsDir() {
		fmt.Fprintf(diag, "governor gate: no state directory at %s: not starting a daemon\n", state)
		return
	}
	if !claimStartAttempt(state) {
		fmt.Fprintf(diag, "governor gate: a daemon start was attempted within the last %v: not starting another\n",
			lazyStartCooldown)
		return
	}
	if err := execDaemon(state); err != nil {
		fmt.Fprintf(diag, "governor gate: start daemon: %v\n", err)
		return
	}
	fmt.Fprintf(diag, "governor gate: started a daemon for %s; this spawn proceeds ungoverned\n", endpoint)
}

// stateDirFor is the directory beside the socket an endpoint names.
//
// Derived from the endpoint rather than from transport.StateDir, for the same reason
// daemonPIDPath is: two governors on one machine is a supported thing — GOVERNOR_ENDPOINT
// exists for it — and a gate that could not reach the socket at *this* endpoint must start
// a daemon for *that* socket and rate-limit beside it, not beside the default one.
func stateDirFor(endpoint string) (string, error) {
	sock, err := socketPath(endpoint)
	if err != nil {
		return "", err
	}
	return filepath.Dir(sock), nil
}

// claimStartAttempt reports whether this process should attempt a start, recording the
// attempt when it says yes.
//
// The record is written BEFORE the fork rather than after, and deliberately so: a fork that
// fails, and a gate killed by its own budget mid-fork, are both attempts. Recording only
// successes is what turns a daemon that cannot start into a fork on every spawn, which is
// the case this function exists for.
func claimStartAttempt(state string) bool {
	path := filepath.Join(state, daemonStartFile)
	if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) < lazyStartCooldown {
		return false
	}
	// The write's error is dropped on purpose. A state directory this process cannot write
	// to is a reason to lose the rate limit, not a reason to leave the machine permanently
	// ungoverned — failing open is the posture here as everywhere else in the gate.
	_ = os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)+"\n"), 0o600)
	return true
}

// execDaemon is the fork. It is a var so a test can watch lazilyStartDaemon reach its
// decision without a daemon actually being forked into the test runner's process group.
// Nothing in the binary writes it.
var execDaemon = forkDaemon

// forkDaemon starts `<this binary> daemon`, detached, with its output appended to the
// daemon log.
//
// It inherits this process's environment, which is what carries GOVERNOR_ENDPOINT and
// GOVERNOR_STATE_DIR through: a gate pointed at a non-default endpoint must start a daemon
// serving that same endpoint, not the default one.
func forkDaemon(state string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve this binary: %w", err)
	}
	// The gate spawns ITSELF under a different subcommand, so what it spawns has to be the
	// governor. Under `go test` os.Executable() is the TEST binary, and running that with
	// "daemon" runs the whole suite again with an argument it does not understand — the
	// same shape as the trap GOV-21 hit when doctor ran `<hooked binary> version` and got
	// the test suite killed at its own budget. Checking the name makes that safe by
	// construction rather than by every test in the package remembering to stub this out,
	// and it is the right rule regardless of tests: executing a program this binary cannot
	// identify is strictly worse than leaving the machine ungoverned.
	if base := filepath.Base(exe); base != installedBinaryName {
		return fmt.Errorf("this binary is named %q, not %q: refusing to run it as a daemon", base, installedBinaryName)
	}

	logFile, err := os.OpenFile(filepath.Join(state, daemonLogFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "daemon")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid for the reason install uses it, and more sharply here: the gate is a hook
	// process that exits in milliseconds, and a daemon left in its process group would take
	// the session's terminal signals — a Ctrl-C in the session that happened to spawn it
	// would kill the governor for every session on the machine.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Released, never waited on. Waiting for it to bind would put the daemon's start-up on
	// the hot path, which is the one thing this file must not do.
	return cmd.Process.Release()
}
