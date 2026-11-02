package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// GOV-44. calibrate runs from a terminal, where CLAUDE_PLUGIN_DATA is unset; a daemon a gate
// starts is a hook's child, where Claude Code sets it. config.Path used to prefer that
// variable, so the two resolved different files and every gate-started daemon ran on the
// compiled-in constants without a word.

// TestACalibrationReachesADaemonTheGateStarted is clause 1 as it is checkable: with
// CLAUDE_PLUGIN_DATA set to a scratch directory, calibrate, then start a daemon, and assert
// they agree on b_max and run_queue_max.
//
// Both halves are the REAL binary, and the daemon is started the way the defect needs it to
// be — by a gate, inheriting the plugin variable — rather than in-process, where nothing about
// which process set which variable would be exercised. Before the fix the daemon reported
// path <scratch>/config.json, found false.
func TestACalibrationReachesADaemonTheGateStarted(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and forks a real binary")
	}
	state := sockDir(t)
	endpoint := "unix://" + filepath.Join(state, "sock")
	plugin := t.TempDir()
	t.Cleanup(func() { stopLazilyStartedDaemon(t, state) })

	// The terminal: no plugin variable. --no-probe keeps it to the ambient read.
	cal := exec.Command(realGovernor(t), "calibrate", "--no-probe", "--quiet")
	cal.Env = append(os.Environ(),
		transport.StateDirEnv+"="+state,
		transport.EndpointEnv+"="+endpoint,
		config.DataDirEnv+"=",
	)
	if out, err := cal.CombinedOutput(); err != nil {
		t.Fatalf("calibrate: %v\n%s", err, out)
	}
	disk, err := (&config.FileStore{Path: filepath.Join(state, config.File)}).Load(context.Background())
	if err != nil || disk.CalibratedAt == "" {
		t.Fatalf("calibrate wrote nothing usable to the state directory: %+v, %v", disk, err)
	}

	// The hook: the plugin variable set, exactly as Claude Code sets it for a gate.
	t.Setenv(config.DataDirEnv, plugin)
	runRealGate(t, state, endpoint)
	deadline := time.Now().Add(5 * time.Second)
	for !daemonAnswers(endpoint) {
		if time.Now().After(deadline) {
			t.Fatalf("no daemon answered within 5s; log:\n%s", readDaemonLog(state))
		}
		time.Sleep(20 * time.Millisecond)
	}

	got := daemonLoadedConfig(t, endpoint)
	if got == nil {
		t.Fatalf("the daemon reported no config at all")
	}
	if !got.Found || got.Path != filepath.Join(state, config.File) {
		t.Fatalf("the gate-started daemon read %s (found %v), but calibrate wrote %s",
			got.Path, got.Found, filepath.Join(state, config.File))
	}
	if got.BMax != disk.BMax || got.RunQueueMax != disk.RunQueueMax {
		t.Fatalf("daemon loaded b_max %d, run_queue_max %v; calibrate wrote b_max %d, run_queue_max %v",
			got.BMax, got.RunQueueMax, disk.BMax, disk.RunQueueMax)
	}
	if want := disk.RunQueueMax; got.RunQueueCut != want {
		t.Errorf("the daemon cuts at %v, want the calibrated %v", got.RunQueueCut, want)
	}
	if entries, _ := os.ReadDir(plugin); len(entries) != 0 {
		t.Errorf("the daemon wrote %v into the plugin data directory", entries)
	}
}

// TestADaemonSaysWhichConfigItResolvedAndThatItFoundNothing is clause 2: a missing config is
// not silent. The daemon's log names the path and says it is on built-in defaults.
func TestADaemonSaysWhichConfigItResolvedAndThatItFoundNothing(t *testing.T) {
	state := t.TempDir()
	t.Setenv(transport.StateDirEnv, state)
	t.Setenv(config.DataDirEnv, t.TempDir()) // empty, and must not matter

	var diag bytes.Buffer
	c, loaded := loadCalibration(&diag)
	path := filepath.Join(state, config.File)
	if !strings.Contains(diag.String(), path) || !strings.Contains(diag.String(), "using built-in defaults") {
		t.Fatalf("log = %q, want it to name %s and say it is using built-in defaults", diag.String(), path)
	}
	if c.BMax != 0 || c.RunQueueMax != 0 || loaded.Found || loaded.Path != path {
		t.Fatalf("an absent config loaded %+v / %+v", c, loaded)
	}

	// And a present one is named too, with what was in it.
	writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 3, "run_queue_max": 4.5,
		"calibrated_at": "2026-10-31T19:14:30Z"})
	diag.Reset()
	_, loaded = loadCalibration(&diag)
	if !strings.Contains(diag.String(), path+": found, calibrated 2026-10-31T19:14:30Z") || !loaded.Found {
		t.Fatalf("log = %q, want it to name %s and the calibration it found", diag.String(), path)
	}
}

func TestCalibrationDrift(t *testing.T) {
	const path = "/state/config.json"
	disk := config.Config{BMax: 4, RunQueueMax: 4.72, CalibratedAt: "2026-10-31T19:14:30Z"}
	same := func() *loadedConfig {
		return &loadedConfig{Path: path, Found: true, BMax: 4, RunQueueMax: 4.72,
			CalibratedAt: "2026-10-31T19:14:30Z", RunQueueCut: 4.72}
	}
	for _, c := range []struct {
		name    string
		got     func() *loadedConfig
		ceiling int
		disk    config.Config
		found   bool
		mention string // "" means no drift
	}{
		{"agreement", same, 4, disk, true, ""},
		{"a ceiling below b_max is a cgroup, not drift", same, 2, disk, true, ""},
		{"no loop, no ceiling to compare", func() *loadedConfig { c := same(); c.RunQueueCut = 0; return c }, 0, disk, true, ""},
		// The 2026-10-31 gate-started daemon.
		{"another path", func() *loadedConfig {
			return &loadedConfig{Path: "/plugin/config.json", RunQueueCut: budget.RunQueueMax}
		}, 6, disk, true, "/plugin/config.json"},
		{"started before the config existed", func() *loadedConfig {
			return &loadedConfig{Path: path, RunQueueCut: budget.RunQueueMax}
		}, 6, disk, true, "built-in defaults"},
		{"config removed since", same, 4, config.Config{}, false, "no longer there"},
		// The 2026-10-31 install-started daemon: install calibrated after starting it.
		{"recalibrated underneath it", func() *loadedConfig {
			c := same()
			c.RunQueueMax, c.RunQueueCut, c.CalibratedAt = 4.81, 4.81, "2026-10-31T22:23:56Z"
			return c
		}, 4, config.Config{BMax: 4, RunQueueMax: 5.02, CalibratedAt: "2026-10-31T22:33:16Z"}, true, "5.02"},
		{"live ceiling above b_max", same, 6, disk, true, "ceiling of 6"},
		{"live cut not the configured one", func() *loadedConfig { c := same(); c.RunQueueCut = 2; return c }, 4, disk, true, "cuts at run queue 2.00"},
		{"a sweep's override is not drift", func() *loadedConfig {
			c := same()
			c.RunQueueOverride, c.RunQueueCut = 1.5, 1.5
			return c
		}, 4, disk, true, ""},
		{"uncalibrated both sides", func() *loadedConfig {
			return &loadedConfig{Path: path, RunQueueCut: budget.RunQueueMax}
		}, 6, config.Config{}, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			why := calibrationDrift(c.got(), c.ceiling, path, c.disk, c.found)
			if c.mention == "" && why != "" {
				t.Fatalf("drift reported where there is none: %q", why)
			}
			if c.mention != "" && !strings.Contains(why, c.mention) {
				t.Fatalf("drift = %q, want it to mention %q", why, c.mention)
			}
		})
	}
}

// TestDoctorFailsADaemonNotRunningTheConfigOnDisk is clause 3: doctor cannot report two
// ceilings and call both ok. The calibration check reads the file and passes; this one reads
// both sides and fails, with the install that clears it.
func TestDoctorFailsADaemonNotRunningTheConfigOnDisk(t *testing.T) {
	w := newWorld(t)
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.loaded = &loadedConfig{Path: "/elsewhere/config.json"}
	})
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())
	w.writeCalibration()

	_, found, out := doctor(t)
	want(t, found, "calibration", "ok", out)
	got := want(t, found, "loaded", "FAIL", out)
	if !strings.Contains(got.detail, "/elsewhere/config.json") {
		t.Errorf("the finding does not name the file the daemon read: %q", got.detail)
	}
	if want := selfBinary(t) + " install"; got.fix != want {
		t.Errorf("fix = %q, want %q", got.fix, want)
	}
}

func TestDoctorPassesADaemonRunningTheConfigOnDisk(t *testing.T) {
	w := newWorld(t)
	w.writeCalibration()
	path, _ := config.Path()
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.loaded = &loadedConfig{Path: path, Found: true, BMax: 4, RunQueueMax: 3.42,
			CalibratedAt: "2026-09-16T00:00:00Z"}
	})
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())

	_, found, out := doctor(t)
	want(t, found, "loaded", "ok", out)
}

// TestInstallLeavesTheDaemonOnTheConfigItWrote: install starts the daemon and THEN calibrates,
// deliberately, so the probe measures the machine as the governor finds it. The daemon read
// its config before that calibration existed. Before GOV-44 every first install therefore
// left a daemon on built-in defaults — and the install-time doctor passed it.
func TestInstallLeavesTheDaemonOnTheConfigItWrote(t *testing.T) {
	p := stagedHome(t)
	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })

	path := filepath.Join(p.state, config.File)
	disk, err := (&config.FileStore{Path: path}).Load(context.Background())
	if err != nil || disk.CalibratedAt == "" {
		t.Fatalf("install wrote no calibration to %s: %+v, %v", path, disk, err)
	}
	got := daemonLoadedConfig(t, "unix://"+filepath.Join(p.state, "sock"))
	if got == nil || !got.Found || got.CalibratedAt != disk.CalibratedAt || got.RunQueueMax != disk.RunQueueMax {
		t.Fatalf("after install the daemon is running %+v, but install wrote %+v", got, disk)
	}
}

func daemonLoadedConfig(t *testing.T, endpoint string) *loadedConfig {
	t.Helper()
	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got struct {
		Config *loadedConfig `json:"config"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return got.Config
}

// TestARunningDaemonPicksUpANewCalibration: a calibration written under a running daemon
// takes effect on its next tick, with nothing restarted. Before GOV-44 the daemon read the
// file once at startup and a standalone `calibrate` changed nothing until something killed it.
func TestARunningDaemonPicksUpANewCalibration(t *testing.T) {
	state := t.TempDir()
	t.Setenv(transport.StateDirEnv, state)
	path := filepath.Join(state, config.File)

	var diag bytes.Buffer
	d := &daemon{diag: &diag, loop: &budget.Controller{Max: 6}}
	_, loaded := loadCalibration(&diag)
	d.loaded, d.configStamp = &loaded, stampOf(path)

	d.reloadConfig() // nothing changed: nothing reloads
	if strings.Contains(diag.String(), "reloading") {
		t.Fatalf("reloaded an unchanged config: %s", diag.String())
	}

	writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 4, "run_queue_max": 4.72,
		"calibrated_at": "2026-10-31T19:14:30Z"})
	d.reloadConfig()

	got := d.statusConfig()
	if !got.Found || got.BMax != 4 || got.RunQueueMax != 4.72 || got.RunQueueCut != 4.72 {
		t.Fatalf("after a calibration the daemon reports %+v", got)
	}
	if d.calibratedBMax != 4 || d.ceiling(8) != 4 {
		t.Fatalf("the reloaded b_max does not cap the ceiling: calibratedBMax %d, ceiling(8) %d", d.calibratedBMax, d.ceiling(8))
	}
	if why := calibrationDrift(got, 4, path, config.Config{BMax: 4, RunQueueMax: 4.72, CalibratedAt: "2026-10-31T19:14:30Z"}, true); why != "" {
		t.Fatalf("still drifting after reload: %s", why)
	}
}

// loadsItsConfig gives a test daemon what every real daemon has: the config it read at startup.
// A daemon built by newDaemon never loads one, and doctor rightly cannot vouch for it.
func loadsItsConfig(d *daemon) {
	_, loaded := loadCalibration(io.Discard)
	d.loaded = &loaded
}

// TestAFailedReloadKeepsTheCalibrationItHad is GOV-B22 clause 4: a config that goes bad under a
// running daemon — unreadable, or written by a newer schema — leaves the daemon governing on
// what it had, not on the compiled-in defaults. The defaults are the looser limit (a calibrated
// b_max only ever lowers the ceiling), so falling back to them would answer a corrupt file by
// allowing more.
func TestAFailedReloadKeepsTheCalibrationItHad(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(t *testing.T, path string)
	}{
		{"newer schema", func(t *testing.T, path string) {
			writeConfig(t, path, map[string]any{"schema": config.Schema + 1, "b_max": 9, "run_queue_max": 9.5})
		}},
		{"malformed", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			// New content first: a chmod alone changes neither mtime nor size, and a file
			// that went unreadable without changing is not a reload at all.
			writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 9, "run_queue_max": 9.5,
				"calibrated_at": "2026-11-01T00:00:00Z"})
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(path, 0o600) })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv(transport.StateDirEnv, state)
			t.Setenv(runQueueMaxEnv, "")
			path := filepath.Join(state, config.File)

			var diag bytes.Buffer
			d := &daemon{diag: &diag, loop: &budget.Controller{Max: 6}}
			_, loaded := loadCalibration(&diag)
			d.loaded, d.configStamp = &loaded, stampOf(path)
			writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 4, "run_queue_max": 4.72,
				"calibrated_at": "2026-10-31T19:14:30Z"})
			d.reloadConfig()
			if d.calibratedBMax != 4 || d.loop.RunQueueMax != 4.72 {
				t.Fatalf("setup: the good calibration did not load: b_max %d, cut %g", d.calibratedBMax, d.loop.RunQueueMax)
			}

			tc.spoil(t, path)
			diag.Reset()
			d.reloadConfig()

			if d.calibratedBMax != 4 || d.ceiling(8) != 4 || d.loop.RunQueueMax != 4.72 {
				t.Fatalf("a failed reload moved the daemon off its calibration: b_max %d, ceiling(8) %d, cut %g\n%s",
					d.calibratedBMax, d.ceiling(8), d.loop.RunQueueMax, diag.String())
			}
			if got := d.statusConfig(); !got.Found || got.CalibratedAt != "2026-10-31T19:14:30Z" || got.RunQueueCut != 4.72 {
				t.Fatalf("status no longer reports the calibration being served: %+v", got)
			}
			if !strings.Contains(diag.String(), "keeping the calibration it had") {
				t.Fatalf("a failed reload was not logged as one: %s", diag.String())
			}

			// Logged once per change, not once a tick.
			diag.Reset()
			d.reloadConfig()
			if diag.Len() != 0 {
				t.Fatalf("an unchanged bad file was re-read on the next tick: %s", diag.String())
			}

			// And the next good write is picked up as normal.
			os.Chmod(path, 0o600)
			writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 3, "run_queue_max": 5.1,
				"calibrated_at": "2026-11-01T09:00:00Z"})
			d.reloadConfig()
			if d.calibratedBMax != 3 || d.loop.RunQueueMax != 5.1 {
				t.Fatalf("the daemon stopped reloading after a bad file: b_max %d, cut %g", d.calibratedBMax, d.loop.RunQueueMax)
			}
		})
	}
}

// TestAReloadDoesNotOverrideTheRunQueueEnv: GOVERNOR_RUNQUEUE_MAX beats config.json at startup
// (resolveRunQueueMax), and a reload must not quietly reverse that. tools/sweep sets the
// variable to measure one threshold; a calibration landing mid-sweep that swapped it out would
// produce a confident wrong number.
func TestAReloadDoesNotOverrideTheRunQueueEnv(t *testing.T) {
	state := t.TempDir()
	t.Setenv(transport.StateDirEnv, state)
	t.Setenv(runQueueMaxEnv, "3.3")
	path := filepath.Join(state, config.File)

	d := &daemon{diag: io.Discard, loop: &budget.Controller{Max: 6, RunQueueMax: 3.3}}
	_, loaded := loadCalibration(io.Discard)
	loaded.RunQueueOverride = runQueueMaxOverride(io.Discard)
	d.loaded, d.configStamp = &loaded, stampOf(path)

	writeConfig(t, path, map[string]any{"schema": config.Schema, "b_max": 4, "run_queue_max": 4.72,
		"calibrated_at": "2026-10-31T19:14:30Z"})
	d.reloadConfig()

	if d.loop.RunQueueMax != 3.3 {
		t.Fatalf("a reload let config.json's cut %g override the environment's 3.3: now cutting at %g", 4.72, d.loop.RunQueueMax)
	}
	if d.calibratedBMax != 4 {
		t.Fatalf("the env var covers the run-queue cut only, but b_max did not reload: %d", d.calibratedBMax)
	}
	got := d.statusConfig()
	if got.RunQueueOverride != 3.3 {
		t.Fatalf("the reload dropped the override from status: %+v", got)
	}
	if why := calibrationDrift(got, 4, path, config.Config{BMax: 4, RunQueueMax: 4.72, CalibratedAt: "2026-10-31T19:14:30Z"}, true); why != "" {
		t.Fatalf("a sweep's daemon is called stale for obeying its override: %s", why)
	}
}

// BenchmarkConfigStamp is the per-tick cost of noticing a new calibration (design.md 5.1's
// 1%-of-a-core bar). reloadConfig does one stat a second and nothing else when the file has
// not changed.
func BenchmarkConfigStamp(b *testing.B) {
	path := filepath.Join(b.TempDir(), config.File)
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		stampOf(path)
	}
}
