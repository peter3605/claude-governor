package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// Tests for GOV-43: a daemon of a different build is detected, and not only by doctor.
//
// THE CASE THAT FOUND IT. On 2026-10-31 every session on Peter's machine was served by a
// daemon started on 2026-09-20 from a 0.0.0-dev build, while v0.1.1 was installed. It spoke
// protocol 1, so checkProtocol passed; doctor's binary check compared files on disk, so it
// passed; `governor install` compared version strings, so it would have left the daemon
// alone had the version matched. Every test below stages some shape of that and asserts the
// shape is now caught.

// otherBuild is a digest no real binary has.
const otherBuild = "0000000000000000000000000000000000000000000000000000000000000bad"

func TestTheDaemonReportsWhichBuildItIs(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got struct {
		Build string `json:"build"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode status %q: %v", line, err)
	}
	if got.Build == "" || got.Build == buildUnknown {
		t.Fatalf("build = %q: the daemon does not say which build it is", got.Build)
	}
	// The digest of the executable, by the same function install uses to decide whether it
	// has anything to copy — so the two cannot drift into disagreeing about "same build".
	want, err := digest(selfBinary(t))
	if err != nil {
		t.Fatalf("digest self: %v", err)
	}
	if got.Build != want {
		t.Errorf("build = %s, want the digest of this executable %s", got.Build, want)
	}
}

func TestDaemonBuildVerdictNeedsPositiveEvidence(t *testing.T) {
	mine := strings.Repeat("a", 64)
	for _, c := range []struct {
		name, daemon, mine string
		stale              bool
	}{
		{"same build", mine, mine, false},
		{"different build", otherBuild, mine, true},
		// The pid 23289 shape: no field at all, so older than any binary that can ask.
		{"daemon predates the field", "", mine, true},
		// Neither "could not tell" is evidence. A status line that cried stale whenever a
		// read failed would be noise a person learns to ignore.
		{"daemon could not read itself", buildUnknown, mine, false},
		{"reader could not read itself", otherBuild, "", false},
		{"reader reports unknown", otherBuild, buildUnknown, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := daemonBuildVerdict(c.daemon, c.mine); got != c.stale {
				t.Errorf("daemonBuildVerdict(%q, %q) = %v, want %v", c.daemon, c.mine, got, c.stale)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the status line: runtime detection, report only
// ---------------------------------------------------------------------------

func TestStatuslineSaysWhenTheDaemonIsAnotherBuild(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply map[string]any
		stale bool
	}{
		{"a different build", map[string]any{"build": otherBuild}, true},
		{"a daemon from before build reporting", map[string]any{noBuildKey: true}, true},
		{"a daemon that could not read itself", map[string]any{"build": buildUnknown}, false},
		{"this build", map[string]any{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolateState(t)
			reply := map[string]any{"v": protocolVersion, "budget": 4, "slots": 1.0, "in_use": 1,
				"thermal": "nominal"}
			for k, v := range c.reply {
				reply[k] = v
			}
			endpoint, _, _ := statusDaemon(t, reply)
			line := runStatuslineFor(t, endpoint, "", 0)
			got := strings.Contains(line, "stale daemon: governor install")
			if got != c.stale {
				t.Errorf("line = %q, stale clause present = %v, want %v", line, got, c.stale)
			}
			// Beside the slot count, never instead of it: the old daemon is still governing.
			if !strings.HasPrefix(line, "1/4 slots") {
				t.Errorf("line = %q: the slot count is gone", line)
			}
		})
	}
}

// The stale clause outranks context. On a narrow terminal cpu and thermal go first.
func TestTheStaleClauseSurvivesANarrowTerminal(t *testing.T) {
	v := statusView{OK: true, Budget: 4, Slots: 1, Thermal: "nominal", StaleDaemon: true}
	cpu := 0.5
	v.CPU = &cpu
	line := render(v, len("1/4 slots - stale daemon: governor install")+3)
	if line != "1/4 slots - stale daemon: governor install" {
		t.Errorf("line = %q", line)
	}
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

func TestDoctorCatchesADaemonOfAnotherBuild(t *testing.T) {
	for _, c := range []struct {
		name, build, mention string
	}{
		{"a different build", otherBuild, shortBuild(otherBuild)},
		{"a build from before build reporting", "", "predates build reporting"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			// A real daemon, on its own endpoint, claiming another build. Same protocol and
			// the same binary_version as the hooks — the shape every other check passes.
			endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
				d.build = c.build
			})
			t.Setenv(transport.EndpointEnv, endpoint)
			w.writeHooks(w.settings, selfBinary(t), goodHooks())
			w.writeCalibration()

			_, found, out := doctor(t)
			want(t, found, "protocol", "ok", out)
			want(t, found, "binary", "ok", out)
			got := want(t, found, "build", "FAIL", out)
			if !strings.Contains(got.detail, c.mention) {
				t.Errorf("the finding does not say what it found (%q): %q", c.mention, got.detail)
			}
			if !strings.Contains(got.detail, binaryVersion) {
				t.Errorf("the finding does not name the daemon's version: %q", got.detail)
			}
			if got.fix != "governor install" {
				t.Errorf("fix = %q, want governor install", got.fix)
			}
		})
	}
}

func TestDoctorPassesADaemonOfTheHookedBuild(t *testing.T) {
	w := newWorld(t)
	endpoint, _ := startTestDaemon(t, 4)
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())

	_, found, out := doctor(t)
	got := want(t, found, "build", "ok", out)
	if !strings.Contains(got.detail, shortBuild(thisBuild())) {
		t.Errorf("the finding does not name the build: %q", got.detail)
	}
}

// A daemon that could not read its own executable is not evidence of staleness, and doctor
// says it could not check rather than passing or failing.
func TestDoctorDoesNotGuessAnUnknownBuild(t *testing.T) {
	w := newWorld(t)
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.build = buildUnknown
	})
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())

	_, found, out := doctor(t)
	want(t, found, "build", "unknown", out)
}

// ---------------------------------------------------------------------------
// install: the fix line works
// ---------------------------------------------------------------------------

// TestInstallReplacesADaemonOfAnotherBuildWhoseBinaryIsAlreadyCurrent is the fix line's proof.
//
// The installed binary is current, so install copies nothing and binaryReplaced is false. The
// daemon on the socket is a different build reporting the SAME version — the two-dev-builds
// shape. Before GOV-43 inspectDaemon compared only the version string, found them equal, and
// left that daemon running; doctor's "governor install" would have done nothing. It must now
// replace it.
func TestInstallReplacesADaemonOfAnotherBuildWhoseBinaryIsAlreadyCurrent(t *testing.T) {
	p := stagedHome(t)

	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })
	endpoint := "unix://" + filepath.Join(p.state, "sock")

	stopTheDaemon(t, p)
	startDetachedDaemon(t, governorRebuilt(t), p)
	if v := daemonBinaryVersion(t, endpoint); v != binaryVersion {
		t.Fatalf("staging: the rebuilt daemon reports %q, want the same version %q", v, binaryVersion)
	}
	before, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid: %v", err)
	}

	code, out, errOut := runReal(t, p, "install")
	if code != 0 {
		t.Fatalf("re-install: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "replacing the running daemon") || !strings.Contains(out, "build") {
		t.Errorf("install left a daemon of another build on the socket, or did not say why:\n%s", out)
	}
	after, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid after re-install: %v", err)
	}
	if after == before {
		t.Errorf("the daemon was not replaced: pid %d before and after", before)
	}
	installed, err := digest(p.binary)
	if err != nil {
		t.Fatalf("digest installed: %v", err)
	}
	if got := daemonBuild(t, endpoint); got != installed {
		t.Errorf("the daemon reports build %s after re-install, want the installed %s",
			shortBuild(got), shortBuild(installed))
	}
}

func daemonBuild(t *testing.T, endpoint string) string {
	t.Helper()
	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got struct {
		Build string `json:"build"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return got.Build
}
