// Package config reads and writes the versioned config at
// ${CLAUDE_PLUGIN_DATA}/config.json.
//
// The seam (design.md section 12.1, 13.4): the schema is versioned and migrated on read,
// so a config written by a newer binary must not brick an older one. Unknown keys warn and
// are preserved, never crash. Writes are atomic — temp file, rename — because a truncated
// config is a governor that will not start.
package config

import "context"

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
	Setpoint float64
	// BMax is the ceiling on concurrent agents for this host.
	BMax int
	// WaitMax is how long the gate will wait for a slot before refusing.
	WaitMaxMS int
	// Unknown holds keys this binary does not recognise, so a round trip through an older
	// version preserves a newer version's settings.
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
