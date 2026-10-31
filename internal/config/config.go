// Package config reads and writes the versioned config at <state dir>/config.json — the
// governor's own state directory, beside the socket. See Path for why it is not
// ${CLAUDE_PLUGIN_DATA}.
//
// The seam (design.md section 12.1, 13.4): the schema is versioned and migrated on read,
// so a config written by a newer binary must not brick an older one. Unknown keys warn and
// are preserved, never crash. Writes are atomic — temp file, rename — because a truncated
// config is a governor that will not start.
package config

import (
	"context"
	"path/filepath"

	"github.com/peter3605/claude-governor/internal/transport"
)

// Schema is the config schema version this binary writes.
const Schema = 1

// Config is the tunable surface. Its defaults come from `governor calibrate` measuring
// this host, never from a constant tuned on one laptop.
type Config struct {
	// Schema is the version the file was written with, migrated forward on read.
	Schema int
	// Endpoint is the daemon endpoint URL passed to transport.Dialer.
	Endpoint string
	// Setpoint is the target CPU utilisation the AIMD loop steers to (section 5.2).
	//
	// IT IS NO LONGER A CONTROL INPUT AND `calibrate` DOES NOT WRITE IT. GOV-32 removed
	// utilisation from the loop, so a value here changes nothing; the key stays so that a
	// config written before that still round-trips rather than being reported as unknown.
	// The reasoning is at budget.Setpoint.
	Setpoint float64
	// BMax is the ceiling on concurrency for this host, in slots.
	//
	// Zero means uncalibrated, and the daemon then derives the ceiling from CPUCapacity on
	// every tick exactly as it did before — the inert default section 13.4 requires. A
	// non-zero value CAPS that derivation rather than replacing it; see the daemon.
	BMax int
	// RunQueueMax is the run-queue-per-core above which the budget is halved.
	//
	// The one control input the loop has, after GOV-32. Zero means uncalibrated and the
	// compiled-in budget.RunQueueMax stands.
	RunQueueMax float64
	// CalibratedAt is when `governor calibrate` last measured this host, RFC3339.
	CalibratedAt string
	// CalibratedFrom is the one-line provenance note: the readings the numbers above were
	// derived from. A number nobody can trace back to a measurement gets treated as
	// arbitrary and edited at random six months later.
	CalibratedFrom string
	// WaitMax is how long the gate will wait for a slot before refusing.
	WaitMaxMS int
	// Unknown holds keys this binary does not recognise, so a round trip through an older
	// version preserves a newer version's settings.
	//
	// Its values are json.RawMessage holding the original bytes verbatim. Decoding them
	// through `any` would round every number through float64, so a newer version's large
	// integer would come back mangled — preserved in name and corrupted in value, which is
	// worse than dropping it because it looks like it worked.
	Unknown map[string]any
}

// Store loads and saves the config.
//
// Load MUST migrate an older schema forward and MUST NOT fail on an unrecognised key.
// Save MUST be atomic.
type Store interface {
	Load(ctx context.Context) (Config, error)
	Save(ctx context.Context, c Config) error
}

// File is the config's filename inside the data directory.
const File = "config.json"

// DataDirEnv is the environment variable Claude Code sets to the plugin's own data
// directory (design.md sections 9 and 13.3).
//
// Path DOES NOT CONSULT IT, and the constant lives here only so the processes that do — the
// onboarding marker, doctor's environment listing, uninstall --purge — name one spelling of it.
const DataDirEnv = "CLAUDE_PLUGIN_DATA"

// Path is where config.json lives: the governor's own state directory, the same directory
// as the socket, resolved by the same chain (GOVERNOR_STATE_DIR, then HOME).
//
// IT DOES NOT CONSULT ${CLAUDE_PLUGIN_DATA}, AND UNTIL GOV-44 IT DID. That variable is set
// only in processes Claude Code starts for the plugin — hooks, the MCP server — and their
// children. `governor calibrate` and `governor install` run from a terminal where it is
// unset, so they wrote <state dir>/config.json. A daemon lazily started by a gate is a child
// of a hook, so it read ${CLAUDE_PLUGIN_DATA}/config.json, found nothing, and ran on the
// compiled-in constants without a word — while binding the CORRECT socket, because
// transport.DefaultEndpoint never looked at the plugin variable. On 2026-10-31 that daemon
// served ceiling 6 against a calibrated 4 and cut at run queue 2.0 against a calibrated 4.72
// (docs/dogfooding.md). Calibration is what install exists to deliver, and every daemon
// started after a reboot comes up through the gate.
//
// The state directory is the canonical location rather than the plugin's, for two reasons.
// Every writer of this file is a person at a terminal, so the reader has to be made to agree
// with the writer and not the other way round. And design.md section 13.3 promises that one
// directory holds all of the governor's state and uninstall removes it: a config in a
// plugin-scoped directory named after whichever route the plugin loaded by is a path
// uninstall can silently stop covering. The rule is the one section 13.3a states for every
// environment-resolved path: a gate-started process and a terminal-started one must resolve
// it identically, so nothing that decides behaviour may read a variable only one of them has.
//
// It resolves a name and does not touch the filesystem. A governor that has never been
// configured has no config file, and a doctor or a statusline that conjured a directory
// while merely asking where one would be is exactly what section 13.3 exists to prevent.
func Path() (string, error) {
	dir, err := transport.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, File), nil
}
