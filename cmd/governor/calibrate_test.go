package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/transport"
)

// TestCalibrateWritesNumbersTheDaemonActuallyReads is the test this story most needed, and it
// exists because of what the repo looked like before it.
//
// `governor calibrate` was specified to "write the defaults for this machine" into config.json
// — and NOTHING READ THAT FILE. The daemon built its controller from compiled-in constants and
// derived its ceiling from CPUCapacity on every tick; the only readers of config were `doctor`
// and `onboard`, both checking the path rather than the contents. A calibration shipped into
// that repo would have satisfied every clause of its story and changed nothing about how any
// machine behaved, which is the exact failure GOV-32 named when it struck the setpoint from
// this story's Done-when: writing a number nothing consults.
//
// So the assertion is deliberately not "the file contains 2.94". It is that the value in the
// file reaches the controller that cuts the budget.
func TestCalibrateWritesNumbersTheDaemonActuallyReads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)

	const calibrated = 4.25
	writeConfig(t, filepath.Join(dir, config.File), map[string]any{
		"schema":        config.Schema,
		"run_queue_max": calibrated,
		"b_max":         3,
	})

	var diag bytes.Buffer
	c, _ := loadCalibration(&diag)
	if got := resolveRunQueueMax(c, &diag); got != calibrated {
		t.Errorf("the daemon resolved a run-queue threshold of %v, want the calibrated %v", got, calibrated)
	}

	// And the ceiling: a calibrated B_max must bind a machine whose cores would otherwise
	// allow more.
	d := &daemon{calibratedBMax: c.BMax}
	if got, want := d.ceiling(8), 3; got != want {
		t.Errorf("ceiling %d slots on an 8-core host with a calibrated b_max of 3, want %d", got, want)
	}
}

// TestAnUncalibratedMachineBehavesExactlyAsItDidBefore.
//
// design.md section 13.4: "Every config key ships with a default that leaves its feature
// inert. An upgraded binary reading an old config must behave exactly as it did before until
// the user opts in." A machine that has never run `calibrate` has no config file at all, and
// that is a state rather than a fault — so it must get the compiled-in threshold and the
// per-tick ceiling, not a zero of either.
func TestAnUncalibratedMachineBehavesExactlyAsItDidBefore(t *testing.T) {
	t.Setenv(transport.StateDirEnv, t.TempDir()) // no config.json written

	var diag bytes.Buffer
	c, _ := loadCalibration(&diag)

	// Zero from resolveRunQueueMax means "use the package constant" — it is what
	// budget.Controller.RunQueueMax documents as its unset value.
	if got := resolveRunQueueMax(c, &diag); got != 0 {
		t.Errorf("an uncalibrated host resolved a threshold override of %v, want 0 so the compiled-in %v stands",
			got, budget.RunQueueMax)
	}
	d := &daemon{calibratedBMax: c.BMax}
	if got, want := d.ceiling(8), budget.MaxSlots(8); got != want {
		t.Errorf("ceiling %d on an uncalibrated host, want the derived %d", got, want)
	}
}

// TestACalibratedCeilingOnlyEverLowersTheDerivedOne.
//
// budget.MaxSlots is recomputed from CPUCapacity on every tick on purpose: a cgroup quota is
// bandwidth that can change under a running daemon, and a container throttled from eight cores
// to two must follow it down within the second. A calibrated ceiling that simply replaced that
// would break the one case the per-tick recomputation exists for — so it may only cap.
func TestACalibratedCeilingOnlyEverLowersTheDerivedOne(t *testing.T) {
	// A ceiling calibrated on the full machine, with the quota later cut to two cores.
	d := &daemon{calibratedBMax: 5}
	if got, want := d.ceiling(2), budget.MaxSlots(2); got != want {
		t.Errorf("ceiling %d after the quota fell to 2 cores, want %d: a calibrated value must not hold the ceiling up",
			got, want)
	}
	// And a calibrated value above the derived one must not raise it.
	high := &daemon{calibratedBMax: 99}
	if got, want := high.ceiling(8), budget.MaxSlots(8); got != want {
		t.Errorf("ceiling %d with a calibrated b_max of 99, want the derived %d", got, want)
	}
}

// TestTheSweepEnvironmentStillBeatsACalibratedThreshold.
//
// GOVERNOR_RUNQUEUE_MAX is tools/sweep's experiment handle, and an experiment that silently
// measured the calibrated value instead of the one it set would produce a confident wrong
// number. That is not hypothetical: GOV-31's first pass had to discard a run over an effect
// that belonged to something other than the variable it thought it was changing.
func TestTheSweepEnvironmentStillBeatsACalibratedThreshold(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)
	t.Setenv(runQueueMaxEnv, "8.0")
	writeConfig(t, filepath.Join(dir, config.File), map[string]any{
		"schema": config.Schema, "run_queue_max": 4.25,
	})

	var diag bytes.Buffer
	c, _ := loadCalibration(&diag)
	if got := resolveRunQueueMax(c, &diag); got != 8.0 {
		t.Errorf("resolved %v with the sweep variable set to 8.0 and a calibrated 4.25 on disk, want 8.0", got)
	}
}

// TestAnUnreadableConfigLeavesTheDaemonRunningOnDefaults.
//
// The daemon must not refuse to start because a tuning file was unreadable. Every number in
// that file has a working default, so the choice is between a governed machine on shipped
// constants and an ungoverned one — and the second is the outcome invariant 1 exists to
// prevent. A config from the future is the sharp case: the store refuses it deliberately,
// and the daemon must degrade rather than propagate that refusal.
func TestAnUnreadableConfigLeavesTheDaemonRunningOnDefaults(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"from the future", `{"schema": 99, "run_queue_max": 4.25}`},
		{"no schema key", `{"run_queue_max": 4.25}`},
		{"not an object", `["nope"]`},
		{"truncated", `{"schema": 1, "run_q`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(transport.StateDirEnv, dir)
			if err := os.WriteFile(filepath.Join(dir, config.File), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var diag bytes.Buffer
			c, _ := loadCalibration(&diag)
			if got := resolveRunQueueMax(c, &diag); got != 0 {
				t.Errorf("resolved %v from an unreadable config, want 0 so the compiled-in default stands", got)
			}
			if diag.Len() == 0 {
				t.Error("the daemon fell back to defaults silently; an unreadable config should say so on the diagnostic stream")
			}
		})
	}
}

// TestCalibrateRefusesToOverwriteAConfigItCannotRead.
//
// The store refuses a schema from the future and a file with no schema key, and both refusals
// exist so nothing saves a truncated version of someone else's settings over the original
// (design.md section 13.4). `calibrate` is the one command that writes this file, so it is the
// one command that can violate that — honouring the refusal means not proceeding to Save.
func TestCalibrateRefusesToOverwriteAConfigItCannotRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)
	path := filepath.Join(dir, config.File)
	const original = `{"schema": 99, "run_queue_max": 4.25}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runCalibrate([]string{"--no-probe"}, &stdout, &stderr); code == 0 {
		t.Error("calibrate exited 0 over a config it could not read")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("calibrate rewrote a config from the future:\n got %s\nwant %s", got, original)
	}
}

// TestCalibratePreservesEverythingItDoesNotOwn.
//
// It owns three fields. A hand-edited endpoint, a wait timeout and a key from a newer binary
// are none of its business, and a calibration that reset them would make re-running it — which
// the story explicitly wants people to do, mid-session — a destructive act.
func TestCalibratePreservesEverythingItDoesNotOwn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)
	path := filepath.Join(dir, config.File)
	writeConfig(t, path, map[string]any{
		"schema":           config.Schema,
		"endpoint":         "unix:///somewhere/else.sock",
		"wait_max_ms":      1234,
		"a_key_from_v2":    []any{1, 2, 3},
		"run_queue_max":    1.0,
		"calibrated_from":  "an older run",
		"some_big_integer": 9007199254740993,
	})

	var stdout, stderr bytes.Buffer
	if code := runCalibrate([]string{"--no-probe", "--quiet"}, &stdout, &stderr); code != 0 {
		t.Fatalf("calibrate exit %d: %s", code, stderr.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("calibrate wrote something that is not a JSON object: %v", err)
	}
	for key, want := range map[string]string{
		"endpoint":         `"unix:///somewhere/else.sock"`,
		"wait_max_ms":      `1234`,
		"a_key_from_v2":    `[1,2,3]`,
		"some_big_integer": `9007199254740993`,
	} {
		got := strings.ReplaceAll(strings.ReplaceAll(string(doc[key]), " ", ""), "\n", "")
		if got != want {
			t.Errorf("%s came back as %s, want %s", key, got, want)
		}
	}
	// And the fields it does own must have moved.
	if string(doc["run_queue_max"]) == "1" {
		t.Error("run_queue_max was not rewritten by a calibration run")
	}
	if !strings.Contains(string(doc["calibrated_from"]), "cores") {
		t.Errorf("calibrated_from was not replaced with this run's provenance: %s", doc["calibrated_from"])
	}
}

// TestCalibrateDryRunWritesNothing. An install runs this command, and `install --dry-run`
// promises to write nothing at all.
func TestCalibrateDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)

	var stdout, stderr bytes.Buffer
	if code := runCalibrate([]string{"--dry-run", "--no-probe"}, &stdout, &stderr); code != 0 {
		t.Fatalf("calibrate --dry-run exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, config.File)); !os.IsNotExist(err) {
		t.Errorf("calibrate --dry-run created a config file (stat err: %v)", err)
	}
	if !strings.Contains(stdout.String(), "measured") {
		t.Error("calibrate --dry-run printed no measurement, so there is nothing to review before writing")
	}
}

// TestCalibrateIsIdempotentInTheWayInstallNeeds.
//
// `install` runs this, and install must be idempotent. Two runs on an unchanged machine must
// not accumulate keys or leave the config growing; only the readings and the timestamp may
// differ, which they will because the machine moves.
func TestCalibrateIsIdempotentInTheWayInstallNeeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)
	path := filepath.Join(dir, config.File)

	var stdout, stderr bytes.Buffer
	for i := 0; i < 2; i++ {
		if code := runCalibrate([]string{"--no-probe", "--quiet"}, &stdout, &stderr); code != 0 {
			t.Fatalf("run %d: exit %d: %s", i, code, stderr.String())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"schema", "run_queue_max", "b_max", "calibrated_at", "calibrated_from"} {
		if _, ok := doc[want]; !ok {
			t.Errorf("config has no %q key after two calibration runs", want)
		}
	}
	// The store owns eight keys. Anything beyond that after two runs means a run added one.
	if len(doc) > 8 {
		t.Errorf("config grew to %d keys after two runs: %v", len(doc), doc)
	}
}

// TestCalibrationIsReadableByTheStoreItWasWrittenBy is the round trip, which matters because
// the config is the interface between two commands that never run in the same process.
func TestCalibrationIsReadableByTheStoreItWasWrittenBy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)

	var stdout, stderr bytes.Buffer
	if code := runCalibrate([]string{"--no-probe", "--quiet"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	var warned []string
	store := &config.FileStore{Path: filepath.Join(dir, config.File), Warn: func(s string) { warned = append(warned, s) }}
	c, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("the config calibrate just wrote cannot be read back: %v", err)
	}
	if len(warned) != 0 {
		t.Errorf("reading back a freshly written config warned about unrecognised keys: %v", warned)
	}
	if c.RunQueueMax <= 0 {
		t.Errorf("run_queue_max came back as %v", c.RunQueueMax)
	}
	// --no-probe on a fresh host measures no ceiling, so none is written: zero, uncalibrated
	// (GOV-47). TestACalibrationWithoutAProbeKeepsTheStoredCeiling covers a stored one.
	if c.BMax != 0 {
		t.Errorf("b_max came back as %v from a run that took no probe, want 0", c.BMax)
	}
	if c.CalibratedAt == "" || c.CalibratedFrom == "" {
		t.Error("the provenance did not survive the round trip, so the numbers cannot be traced to a measurement")
	}
}

func writeConfig(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestACalibrationWithoutAProbeKeepsTheStoredCeiling is GOV-47 clause 2 at the command: the
// config already holds a measured b_max, this run takes no probe, and the measured value must
// survive — with the output saying it was kept rather than re-measured.
func TestACalibrationWithoutAProbeKeepsTheStoredCeiling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)
	path := filepath.Join(dir, config.File)
	writeConfig(t, path, map[string]any{
		"schema":        config.Schema,
		"b_max":         4,
		"run_queue_max": 4.72,
		"calibrated_at": "2026-11-01T19:34:07Z",
	})

	var stdout, stderr bytes.Buffer
	if code := runCalibrate([]string{"--no-probe"}, &stdout, &stderr); code != 0 {
		t.Fatalf("calibrate exit %d: %s", code, stderr.String())
	}
	var doc struct {
		BMax           int    `json:"b_max"`
		CalibratedFrom string `json:"calibrated_from"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.BMax != 4 {
		t.Errorf("b_max %d after a run with no probe, want the stored 4", doc.BMax)
	}
	if !strings.Contains(doc.CalibratedFrom, "b_max 4 kept from the calibration of 2026-11-01T19:34:07Z") {
		t.Errorf("provenance does not record that b_max was kept: %s", doc.CalibratedFrom)
	}
	if !strings.Contains(stdout.String(), "kept from 2026-11-01T19:34:07Z") {
		t.Errorf("the report does not tell the person the ceiling was kept:\n%s", stdout.String())
	}
}
