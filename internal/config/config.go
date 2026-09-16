// Package config reads and writes the versioned config at
// ${CLAUDE_PLUGIN_DATA}/config.json.
//
// The seam (design.md section 12.1, 13.4): the schema is versioned and migrated on read,
// so a config written by a newer binary must not brick an older one. Unknown keys warn and
// are preserved, never crash. Writes are atomic — temp file, rename — because a truncated
// config is a governor that will not start.
package config

import (
	"context"
	"os"
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
const DataDirEnv = "CLAUDE_PLUGIN_DATA"

// Path is where config.json lives.
//
// ${CLAUDE_PLUGIN_DATA}/config.json when Claude Code has named a data directory for the
// plugin, and the governor's own state directory otherwise — the same directory as the
// socket, because design.md section 13.3's promise is that one directory holds all of the
// governor's state and uninstall removes it. A separate fallback location would be a path
// uninstall silently stops covering.
//
// It resolves a name and does not touch the filesystem. A governor that has never been
// configured has no config file, and a doctor or a statusline that conjured a directory
// while merely asking where one would be is exactly what section 13.3 exists to prevent.
func Path() (string, error) {
	if d := os.Getenv(DataDirEnv); d != "" {
		return filepath.Join(d, File), nil
	}
	dir, err := transport.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, File), nil
}
