package main

import (
	"bytes"
	"strings"
	"testing"
)

// The command surface GOV-04 requires, from design.md section 9. A subcommand that
// disappears from the binary is a hook that silently stops firing, so the list is asserted
// rather than assumed.
var required = []string{
	"daemon", "gate", "confirm", "release", "session", "status", "mcp", "statusline",
	"install", "uninstall", "doctor", "calibrate", "version",
}

func TestEverySubcommandExitsZero(t *testing.T) {
	for _, name := range required {
		var stdout, stderr bytes.Buffer
		if code := run([]string{name}, &stdout, &stderr); code != 0 {
			t.Errorf("governor %s: exit %d, want 0 (stderr: %s)", name, code, stderr.String())
		}
	}
}

// A stub must look like a grant, not a refusal: the gate's grant path is exit 0 with an
// empty stdout, and anything on stdout is read by Claude Code as a hook decision.
func TestStubsWriteNothingToStdout(t *testing.T) {
	for _, name := range required {
		if name == "version" {
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

func TestUnknownCommandFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"nonesuch"}, &stdout, &stderr); code == 0 {
		t.Error("governor nonesuch: exit 0, want non-zero")
	}
}
