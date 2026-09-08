package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/signals"
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

func TestEverySubcommandExitsZero(t *testing.T) {
	for _, name := range required {
		if !registered(name) {
			t.Errorf("governor %s: not in the command table", name)
			continue
		}
		if servers[name] {
			continue
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{name}, &stdout, &stderr); code != 0 {
			t.Errorf("governor %s: exit %d, want 0 (stderr: %s)", name, code, stderr.String())
		}
	}
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
// version and status are exempt because they are implemented and printing is their job,
// and daemon because it does not return. None of the three is wired to a hook, so none of
// their stdout is ever read as a decision.
var implemented = map[string]bool{"version": true, "status": true}

func TestStubsWriteNothingToStdout(t *testing.T) {
	for _, name := range required {
		if implemented[name] || servers[name] {
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
// that matters most on a fanless chassis is unobservable until GOV-10 ships a statusline.
func TestStatusReportsThermalState(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("governor status: exit %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "thermal ") {
		t.Fatalf("governor status: output %q, want a \"thermal <state>\" line", out)
	}
	state := signals.Thermal(strings.TrimSpace(strings.TrimPrefix(out, "thermal ")))
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
