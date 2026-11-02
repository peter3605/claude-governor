package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/config"
)

// GOV-44: THE DAEMON SAYS WHICH CONFIG IT IS RUNNING, AND ONE PREDICATE JUDGES IT.
//
// GOV-43 made a running daemon report its BUILD, so that a process holding the socket could be
// compared with the binary on disk. A daemon has a second input that it reads once at startup
// and never again, and nothing reported it: config.json. On 2026-10-31 that cost two separate
// failures, both invisible to every check doctor had:
//
//   - A daemon a gate started read a different file from the one `calibrate` wrote, found
//     nothing, and ran on the compiled-in constants — ceiling 6 against a calibrated 4.
//   - A daemon `install` started kept the calibration it loaded, and `install` then ran
//     `calibrate` and wrote new numbers underneath it. Disk said 5.02; the daemon cut at 4.81.
//
// Both are "the daemon is not running what is on disk", which is the shape GOV-43's build
// check already has. So this is that check's second half and not a parallel mechanism: the
// daemon reports what it loaded in the same status reply that carries its build, and
// calibrationDrift is the one predicate that both `install` (to decide whether to replace the
// daemon) and `doctor` (to fail and name the fix) apply — the same arrangement inspectDaemon
// and checkDaemonBuild already share for the build.

// loadedConfig is what a daemon read from config.json at startup, as status reports it.
//
// Additive like every field GOV-12, GOV-29 and GOV-43 added to statusReply, and for their
// reason: no protocol bump, because a bump fails every un-upgraded hook open the moment the
// daemon restarts. A daemon older than the field sends none, and a reader treats that as
// "cannot say" rather than as agreement.
type loadedConfig struct {
	// Path is the config path the daemon resolved. Comparing it with the reader's own
	// resolution is what catches the GOV-44 defect directly, rather than through its symptom.
	Path string `json:"path"`
	// Found is whether anything was at Path. False means built-in defaults.
	Found bool `json:"found"`
	// BMax, RunQueueMax and CalibratedAt are the file's values as loaded — zero when the file
	// was absent or uncalibrated.
	BMax         int     `json:"b_max"`
	RunQueueMax  float64 `json:"run_queue_max"`
	CalibratedAt string  `json:"calibrated_at,omitempty"`
	// RunQueueOverride is GOVERNOR_RUNQUEUE_MAX when tools/sweep set one, which wins over the
	// file by design (resolveRunQueueMax). Reported so that a sweep's daemon is not called
	// stale for doing exactly what it was told.
	RunQueueOverride float64 `json:"run_queue_override,omitempty"`
	// RunQueueCut is the threshold the control loop is cutting at NOW — the live value, not
	// the configured one, filled in at status time from the controller itself. Zero when the
	// daemon has no loop (every daemon a test builds).
	RunQueueCut float64 `json:"run_queue_cut,omitempty"`
}

// readDiskConfig is the reader's own view of the config: where this process resolves it, and
// what is there. found is false for a missing file; err is set for one that cannot be read.
func readDiskConfig() (path string, c config.Config, found bool, err error) {
	path, err = config.Path()
	if err != nil {
		return "", config.Config{}, false, err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return path, config.Config{}, false, nil
		}
		return path, config.Config{}, false, statErr
	}
	c, err = (&config.FileStore{Path: path}).Load(context.Background())
	return path, c, err == nil, err
}

// calibrationDrift reports why a daemon that loaded got, and is serving ceiling, is NOT
// running the config at diskPath — or "" when it is.
//
// ceiling is the live ceiling from the same status reply (0 when the daemon has no loop and
// so no ceiling to compare). got must be non-nil: a daemon too old to report what it loaded is
// the caller's to judge, because "cannot tell" means different things to install and doctor.
func calibrationDrift(got *loadedConfig, ceiling int, diskPath string, disk config.Config, diskFound bool) string {
	if got.Path != diskPath {
		// The GOV-44 defect itself: two processes resolving two files.
		return fmt.Sprintf("it read its config from %s, but calibrate writes %s", short(got.Path), short(diskPath))
	}
	if got.Found != diskFound {
		if diskFound {
			return fmt.Sprintf("it started with no config at %s and is on built-in defaults, but one has been written since", short(diskPath))
		}
		return fmt.Sprintf("it loaded a config from %s that is no longer there", short(diskPath))
	}
	if got.BMax != disk.BMax || got.RunQueueMax != disk.RunQueueMax || got.CalibratedAt != disk.CalibratedAt {
		return fmt.Sprintf("it loaded the calibration of %s (b_max %d, run-queue cut %.2f), but the config now holds %s (b_max %d, run-queue cut %.2f)",
			orUncalibrated(got.CalibratedAt), got.BMax, got.RunQueueMax,
			orUncalibrated(disk.CalibratedAt), disk.BMax, disk.RunQueueMax)
	}
	// The live values, from the controller rather than from what was loaded. These catch a
	// daemon that read the right file and then did not apply it — the class GOV-41 was, one
	// layer further down.
	if disk.BMax > 0 && ceiling > disk.BMax {
		return fmt.Sprintf("it serves a ceiling of %d slots against a calibrated b_max of %d", ceiling, disk.BMax)
	}
	if got.RunQueueCut > 0 {
		want := budget.RunQueueMax
		if disk.RunQueueMax > 0 {
			want = disk.RunQueueMax
		}
		if got.RunQueueOverride > 0 {
			want = got.RunQueueOverride
		}
		if got.RunQueueCut != want {
			return fmt.Sprintf("it cuts at run queue %.2f per core, but the config calls for %.2f", got.RunQueueCut, want)
		}
	}
	return ""
}

func orUncalibrated(at string) string {
	if at == "" {
		return "no calibration"
	}
	return at
}

// fileStamp is enough of a file's identity to notice that it was rewritten. calibrate saves by
// temp file and rename, so a new calibration is a new mtime (nanosecond resolution on APFS and
// ext4) and usually a new size; exists covers a config appearing or being removed.
type fileStamp struct {
	exists bool
	mod    time.Time
	size   int64
}

func stampOf(path string) fileStamp {
	if path == "" {
		return fileStamp{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, mod: fi.ModTime(), size: fi.Size()}
}

// reloadConfig re-reads config.json when it has changed since it was loaded, and applies it to
// the running controller. Called once per sampling tick, never on the hot path.
//
// WHY THE DAEMON RELOADS RATHER THAN BEING RESTARTED. The first version of GOV-44 had install
// replace a daemon whose loaded config differed from disk. But install recalibrates on every
// run, so every calibration differs — calibrated_at at least — and an identical reinstall
// restarted the daemon, which the idempotency rule forbids (TestInstallLeavesACurrentDaemonAlone).
// A stat once a second is the cheaper fix and the more complete one: a standalone `calibrate`
// takes effect within a tick too, with nothing for anyone to remember to restart, and GOV-43's
// rule that no surface but install restarts the daemon is left where it was.
//
// The stat runs outside the lock; the swap happens inside it, beside the loop it changes.
// Only the two calibrated inputs change — the ceiling cap and the run-queue threshold — and
// the ceiling itself is recomputed by the Step that follows on the same tick.
func (d *daemon) reloadConfig() {
	d.signalsMu.Lock()
	if d.loaded == nil {
		d.signalsMu.Unlock()
		return
	}
	path, was, override := d.loaded.Path, d.configStamp, d.loaded.RunQueueOverride
	d.signalsMu.Unlock()

	now := stampOf(path)
	if now == was {
		return
	}
	fmt.Fprintf(d.diag, "governor daemon: config %s changed, reloading\n", path)
	cal, loaded, err := readCalibration(d.diag)
	if err != nil {
		// A FAILED RELOAD KEEPS WHAT THE DAEMON HAD (GOV-B22). Startup falls back to the
		// compiled-in defaults because it has nothing else; a running daemon does, and the
		// defaults are usually the LOOSER limit — b_max only ever lowers the ceiling — so
		// falling back to them would answer a corrupt or newer-schema file by allowing more.
		// The stamp still advances, so this is logged once per change to the file rather
		// than once a second until someone fixes it; the next write is reloaded as normal.
		// doctor reads the same file itself and reports it unreadable, so nothing is hidden.
		fmt.Fprintf(d.diag, "governor daemon: %v; keeping the calibration it had\n", err)
		d.signalsMu.Lock()
		d.configStamp = now
		d.signalsMu.Unlock()
		return
	}
	loaded.RunQueueOverride = override
	threshold := resolveRunQueueMax(cal, d.diag)

	d.signalsMu.Lock()
	defer d.signalsMu.Unlock()
	d.loaded = &loaded
	d.configStamp = now
	d.calibratedBMax = cal.BMax
	if d.loop != nil {
		d.loop.RunQueueMax = threshold
	}
}

// statusConfig is the loadedConfig a status reply carries: what was loaded, plus the live
// run-queue threshold read from the controller under the same lock every other live read
// takes. Nil when the daemon never loaded a config — a daemon a test built.
func (d *daemon) statusConfig() *loadedConfig {
	d.signalsMu.Lock()
	defer d.signalsMu.Unlock()
	if d.loaded == nil {
		return nil
	}
	c := *d.loaded
	if d.loop != nil {
		c.RunQueueCut = d.loop.RunQueueMax
		if c.RunQueueCut <= 0 {
			c.RunQueueCut = budget.RunQueueMax
		}
	}
	return &c
}
