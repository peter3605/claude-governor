package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/trace"
)

// GOV-B42: doctor reports Phase 0 trace instrumentation that is not the build the governor
// hooks run, and a sampler writing to a log that is no longer on disk.

// addTraceHooks merges the five Phase 0 hooks for bin into the staged settings.json, through
// the same merge `trace install` uses, so the fixture is the shape the installer writes.
func addTraceHooks(t *testing.T, w *world, bin string) {
	t.Helper()
	raw, err := os.ReadFile(w.settings)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	// Merged by hand rather than with mergeTraceHooks, because that now replaces trace hooks
	// from another binary, and the machine being reproduced is one where nothing did.
	hooks, _ := settings["hooks"].(map[string]any)
	for _, e := range traceEvents {
		entry := map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": hookCommand(bin, e.args), "timeout": traceHookTimeout,
		}}}
		if e.matcher != "" {
			entry["matcher"] = e.matcher
		}
		list, _ := hooks[e.event].([]any)
		hooks[e.event] = append(list, entry)
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.settings, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func withSamplers(t *testing.T, s ...traceSampler) {
	t.Helper()
	prev := scanForSamplers
	scanForSamplers = func(context.Context) []traceSampler { return s }
	t.Cleanup(func() { scanForSamplers = prev })
}

// THE MACHINE AS IT WAS, half one: settings.json registers the trace hooks of a September
// binary at a path of its own, while the governor hooks run the current build.
func TestDoctorReportsTraceHooksFromAnotherBuild(t *testing.T) {
	w := stageWorkingInstall(t)
	old := markedGovernor(t, mkdir(t, filepath.Join(w.home, ".local", "share", "claude-governor", "bin")),
		"governor", "0.0.0-dev", protocolVersion, configSchemaVersion, "built 2026-09-20")
	addTraceHooks(t, w, old)

	_, found, out := doctor(t)
	f := want(t, found, "trace", "warn", out)
	if !strings.Contains(f.detail, "claude-governor/bin/governor") {
		t.Errorf("the detail does not name the stale binary: %q", f.detail)
	}
	for _, ev := range []string{"SessionStart", "SubagentStop", "PostToolUse"} {
		if !strings.Contains(f.detail, ev) {
			t.Errorf("the detail does not name the %s hook: %q", ev, f.detail)
		}
	}
	if !strings.Contains(f.fix, "trace install") || !strings.Contains(f.fix, "trace uninstall") {
		t.Errorf("the fix does not offer both ways out: %q", f.fix)
	}
}

// The controls. A trace hook that runs the hooked binary, or a byte-identical copy of it at
// another path (brew's governor beside the plugin's copy), is the same build and passes.
func TestDoctorPassesTraceHooksFromTheHookedBuild(t *testing.T) {
	for _, c := range []struct {
		name string
		bin  func(w *world) string
	}{
		{"the hooked binary itself", func(*world) string { return selfBinary(t) }},
		{"a byte-identical copy elsewhere", func(w *world) string {
			return copyOfSelf(t, mkdir(t, filepath.Join(w.home, "brew", "bin")), "governor")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := stageWorkingInstall(t)
			addTraceHooks(t, w, c.bin(w))
			_, found, out := doctor(t)
			want(t, found, "trace", "ok", out)
		})
	}
}

// No trace at all is the ordinary install, and says so.
func TestDoctorPassesWhenTraceIsNotInstalled(t *testing.T) {
	stageWorkingInstall(t)
	_, found, out := doctor(t)
	f := want(t, found, "trace", "ok", out)
	if !strings.Contains(f.detail, "not installed") {
		t.Errorf("detail = %q", f.detail)
	}
}

// holdLog starts a real process holding path open for append on fd 3, as the sampler holds
// samples.jsonl, and returns it as the scan would see it.
func holdLog(t *testing.T, path string) traceSampler {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `exec 3>>"$1"; exec sleep 60`, "sh", path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	// Until the descriptor is open there is nothing to inspect.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, err := openFilesOf(context.Background(), cmd.Process.Pid)
		if err == nil {
			for _, f := range files {
				if filepath.Base(f.name) == filepath.Base(path) {
					return traceSampler{pid: cmd.Process.Pid, path: selfBinary(t), files: files}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d never showed %s open", cmd.Process.Pid, path)
	return traceSampler{}
}

// reinspect re-reads a sampler's descriptors, after the test has changed the files under it.
func reinspect(t *testing.T, s traceSampler) traceSampler {
	t.Helper()
	files, err := openFilesOf(context.Background(), s.pid)
	if err != nil {
		t.Fatal(err)
	}
	s.files = files
	return s
}

// THE MACHINE AS IT WAS, half two: the gate rotated samples.jsonl and the old sampler kept
// writing to the file that had been renamed away, then to one that had been deleted. Real
// descriptors, through the real openFilesOf: on darwin through lsof, on Linux through /proc.
func TestDoctorFailsOnASamplerWritingToAReplacedLog(t *testing.T) {
	stageWorkingInstall(t)
	live, err := trace.LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"kind":"sample"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := holdLog(t, live)

	// The control: holding the live file is what a healthy sampler does.
	withSamplers(t, s)
	_, found, out := doctor(t)
	want(t, found, "trace", "ok", out)

	// Rotation, as rotate.go does it: rename to .1, start a fresh live file.
	rotated := live + trace.RotatedSuffix
	if err := os.Rename(live, rotated); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	withSamplers(t, reinspect(t, s))
	code, found, out := doctor(t)
	f := want(t, found, "trace", "FAIL", out)
	if !strings.Contains(f.detail, "replaced") {
		t.Errorf("detail does not say the log was replaced: %q", f.detail)
	}
	if !strings.Contains(f.fix, "kill") {
		t.Errorf("fix does not stop the sampler: %q", f.fix)
	}
	if code == 0 {
		t.Errorf("doctor exited 0 with a sampler losing its samples")
	}

	// The next rotation deletes .1, and the descriptor is now on an unlinked file.
	if err := os.Remove(rotated); err != nil {
		t.Fatal(err)
	}
	withSamplers(t, reinspect(t, s))
	_, found, out = doctor(t)
	f = want(t, found, "trace", "FAIL", out)
	if !strings.Contains(f.detail, "deleted") {
		t.Errorf("detail does not say the log was deleted: %q", f.detail)
	}
}

// A sampler from another build, still writing the live file, is the warning before the
// failure: it will stop logging at the next rotation.
func TestDoctorWarnsOnASamplerFromAnotherBuild(t *testing.T) {
	w := stageWorkingInstall(t)
	live, _ := trace.LogPath()
	os.MkdirAll(filepath.Dir(live), 0o755)
	os.WriteFile(live, nil, 0o644)
	s := holdLog(t, live)
	s.path = markedGovernor(t, mkdir(t, filepath.Join(w.home, "old")), "governor", "0.0.0-dev",
		protocolVersion, configSchemaVersion, "september")
	withSamplers(t, s)

	_, found, out := doctor(t)
	f := want(t, found, "trace", "warn", out)
	if !strings.Contains(f.fix, "kill") {
		t.Errorf("fix does not stop the old sampler: %q", f.fix)
	}
}

func TestIsTraceSamplerArgv(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"/Users/x/.local/share/claude-governor/bin/governor", "trace", "sample"}, true},
		{[]string{"governor-darwin-arm64", "trace", "sample"}, true},
		{[]string{"governor", "trace", "agent"}, false},
		{[]string{"governor", "daemon"}, false},
		{[]string{"governor", "doctor", "trace", "sample"}, false},
		{[]string{"python3", "trace", "sample"}, false},
	} {
		if got := isTraceSamplerArgv(c.argv); got != c.want {
			t.Errorf("%q: got %v, want %v", c.argv, got, c.want)
		}
	}
}
