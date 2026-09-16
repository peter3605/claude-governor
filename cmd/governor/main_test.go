package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The command surface GOV-04 requires, from design.md section 9. A subcommand that
// disappears from the binary is a hook that silently stops firing, so the list is asserted
// rather than assumed.
var required = []string{
	"daemon", "gate", "confirm", "release", "session", "status", "mcp", "statusline",
	"install", "uninstall", "doctor", "calibrate", "version",
}

// servers are the subcommands that do not return: they run until the process is signalled.
//
// They are still asserted to exist — a subcommand that vanishes from the binary is a hook
// that silently stops firing, which is what this file is for — but they cannot be *run*
// here. Running one hangs the test binary forever, and because it binds the real endpoint
// on the way it also leaves a daemon holding the user's socket long after the test is
// killed. That is not hypothetical: it happened once while GOV-06 was being written.
var servers = map[string]bool{"daemon": true}

// diagnostics are the subcommands whose exit code REPORTS something rather than merely
// reflecting whether they ran.
//
// `governor doctor` is the only one (GOV-20). Its contract is to exit non-zero when a check
// fails, and this file deliberately runs it on a machine with no daemon and no hooks
// registered — so a failure here is the correct answer, not a broken command. It is still
// asserted to be in the command table, which is what this file is actually for.
//
// It is exempted rather than isolated into a passing state on purpose: making doctor pass
// would mean staging a complete working install just to prove a subcommand exists, and the
// checks themselves are tested in doctor_test.go against exactly that.
var diagnostics = map[string]bool{"doctor": true}

// mutating are the subcommands that change the machine.
//
// `install` writes a plugin directory, merges the user's settings.json and starts a detached
// daemon; `uninstall` removes all of it. Running either from this sweep would do that to the
// DEVELOPER'S OWN ~/.claude — isolateRealState redirects the state directory and the
// endpoint, but not HOME, and a test that installed over someone's real settings while
// merely asserting a subcommand exists would be the same class of accident as GOV-06's
// daemon holding the real socket, with a blast radius of one settings file rather than one
// socket.
//
// They are exempted here rather than isolated into passing, because HOME is process-wide and
// this file runs the whole table: pointing it at a temp directory for these two would point
// it there for every other subcommand too, quietly changing what the rest of the sweep
// tests. install_test.go runs both for real against an isolated HOME, which is where the
// behaviour belongs.
var mutating = map[string]bool{"install": true, "uninstall": true}

func TestEverySubcommandExitsZero(t *testing.T) {
	isolateRealState(t)
	for _, name := range required {
		if !registered(name) {
			t.Errorf("governor %s: not in the command table", name)
			continue
		}
		if servers[name] || diagnostics[name] || mutating[name] {
			continue
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{name}, &stdout, &stderr); code != 0 {
			t.Errorf("governor %s: exit %d, want 0 (stderr: %s)", name, code, stderr.String())
		}
	}
}

// isolateRealState points the state directory and the endpoint somewhere disposable.
//
// This file runs every subcommand in the table, and as of GOV-10 one of them writes a cache
// file to the state directory and another dials the socket. Without this they do it to the
// developer's own ~/.claude/governor: the run would then depend on whether a daemon happened
// to be up, and would leave a file behind in a real install. That is the same class of
// accident as GOV-06's, where running `governor daemon` from this file left a daemon holding
// the user's real socket — so the isolation goes at the top of the test rather than being
// remembered subcommand by subcommand as more of them are implemented.
func isolateRealState(t *testing.T) {
	t.Helper()
	t.Setenv(transport.StateDirEnv, t.TempDir())
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))
}

func registered(name string) bool {
	for _, c := range commands {
		if c.name == name {
			return true
		}
	}
	return false
}

// A stub must look like a grant, not a refusal: the gate's grant path is exit 0 with an
// empty stdout, and anything on stdout is read by Claude Code as a hook decision.
//
// version, status, statusline, doctor and calibrate are exempt because they are implemented
// and printing is their job, and daemon because it does not return. None of the six is wired
// to a hook, so none of their stdout is ever read as a decision.
//
// statusline is the one worth being explicit about: its stdout IS read by Claude Code, but
// as the text of the status line rather than as a permission decision, and it is required
// to produce a line even when everything has gone wrong. The rule this test enforces —
// silence on stdout — is the opposite of that command's contract, not a weaker version of it.
var implemented = map[string]bool{
	"version": true, "status": true, "statusline": true, "doctor": true,
	// calibrate reports what it measured and what it derived, which is the whole point of
	// running it by hand. It is NOT in `mutating`, because the file it writes is
	// config.json in the state directory and isolateRealState already redirects that —
	// unlike install, whose blast radius is HOME and cannot be redirected process-wide.
	"calibrate": true,
}

func TestStubsWriteNothingToStdout(t *testing.T) {
	isolateRealState(t)
	for _, name := range required {
		if implemented[name] || servers[name] || mutating[name] {
			continue
		}
		var stdout, stderr bytes.Buffer
		run([]string{name}, &stdout, &stderr)
		if stdout.Len() != 0 {
			t.Errorf("governor %s: wrote %q to stdout, want empty", name, stdout.String())
		}
	}
}

func TestVersionReportsAllThreeVersions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("governor version: exit %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{"governor " + binaryVersion, "protocol ", "config schema "} {
		if !strings.Contains(out, want) {
			t.Errorf("governor version: output %q missing %q", out, want)
		}
	}
}

// The thermal state has to reach a human through the status operation, or the one signal
// that matters most on a fanless chassis is unobservable.
//
// GOV-10 gave this command the client half of the status operation, so the thermal line is
// no longer the whole output and no longer the first line. It is still required to be
// there, and required to be one of the five named states — that is the property, and it
// holds whether the reading came over the wire or was read locally because no daemon
// answered. The endpoint is pointed at nothing so the test is about the command rather than
// about whether the developer happens to have a daemon running.
func TestStatusReportsThermalState(t *testing.T) {
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(sockDir(t), "absent"))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("governor status: exit %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "thermal ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("governor status: output %q, want a \"thermal <state>\" line", out)
	}
	state := signals.Thermal(strings.TrimSpace(strings.TrimPrefix(line, "thermal ")))
	switch state {
	case signals.Nominal, signals.Fair, signals.Serious, signals.Critical, signals.Unknown:
	default:
		t.Fatalf("governor status: reported thermal %q, want one of the five named states", state)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"nonesuch"}, &stdout, &stderr); code == 0 {
		t.Error("governor nonesuch: exit 0, want non-zero")
	}
}
