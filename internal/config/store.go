package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Migration is one forward step of the config schema, from To-1 to To.
//
// It rewrites the raw document rather than a typed Config, because the whole point of a
// migration is to move keys the current Config type may no longer have — a step that could
// only see today's fields could not rename or fold yesterday's.
type Migration struct {
	// To is the schema version this step produces. A step from 1 to 2 has To: 2.
	To int
	// Apply rewrites the document in place. It must not touch the "schema" key; the store
	// stamps that after the chain has run.
	Apply func(doc map[string]json.RawMessage) error
}

// builtinMigrations is the forward chain this binary knows.
//
// Schema 1 is the first schema, so there is nothing yet to migrate from and the chain is
// empty. It is not dead code: the machinery is what makes schema 2 a matter of appending one
// step rather than inventing a mechanism during the upgrade that needs it, and
// TestAHypotheticalSchema2BinaryMigratesAndPreservesAnUnknownKey exercises it end to end.
var builtinMigrations []Migration

// FileStore is the one implementation of Store: config.json on the local filesystem.
//
// Its zero value is not usable — Path is required. Schema and Migrations are the seam's
// extension point and default to this binary's; a test, or a future binary, sets them to
// stand in for a different version.
type FileStore struct {
	// Path is the config file. Callers get it from Path().
	Path string
	// Warn receives one message per unknown key found on Load, and is where "warned about
	// and preserved" is honoured. It is optional: nil is silent, which is what the hot path
	// wants — the gate has a 100 ms budget (design.md section 4.4) and nothing to say it to.
	Warn func(string)
	// Schema is the version this store writes. Zero means this binary's, Schema.
	Schema int
	// Migrations is the forward chain applied on read. Nil means this binary's.
	Migrations []Migration
}

var _ Store = (*FileStore)(nil)

// knownKeys maps each JSON key to the Config field it fills. Anything not listed here is an
// unknown key: preserved, warned about, never dropped and never fatal.
const (
	keySchema    = "schema"
	keyEndpoint  = "endpoint"
	keySetpoint  = "setpoint"
	keyBMax      = "b_max"
	keyWaitMaxMS = "wait_max_ms"
)

// document is the typed view of the known keys. Every field is a pointer so that "absent"
// and "present and zero" stay distinguishable, and every field is omitempty on the way out
// only through explicitly building the map in Save.
type document struct {
	Schema    *int     `json:"schema"`
	Endpoint  *string  `json:"endpoint"`
	Setpoint  *float64 `json:"setpoint"`
	BMax      *int     `json:"b_max"`
	WaitMaxMS *int     `json:"wait_max_ms"`
}

func (s *FileStore) targetSchema() int {
	if s.Schema != 0 {
		return s.Schema
	}
	return Schema
}

func (s *FileStore) chain() []Migration {
	if s.Migrations != nil {
		return s.Migrations
	}
	return builtinMigrations
}

func (s *FileStore) warn(format string, args ...any) {
	if s.Warn != nil {
		s.Warn(fmt.Sprintf(format, args...))
	}
}

// Load reads the config, migrates it forward, and returns it with every key this binary does
// not recognise preserved in Unknown.
//
// A MISSING FILE IS NOT AN ERROR. design.md section 13.4 has `calibrate` write the file, so a
// machine that has never been calibrated has no config and runs on built-in defaults — a
// state, not a fault, and `doctor` already reports it as a pass. Load returns the zero Config
// stamped with this binary's schema.
//
// A config from the FUTURE is an error, and it is the one case that cannot be handled: this
// binary does not know what changed, so it can neither read it nor safely rewrite it. Better
// to refuse than to save a truncated version of someone else's settings over the original.
func (s *FileStore) Load(ctx context.Context) (Config, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	target := s.targetSchema()

	raw, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		return Config{Schema: target}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", s.Path, err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Config{}, fmt.Errorf("%s is not a JSON object: %w", s.Path, err)
	}

	from, err := schemaOf(doc, s.Path)
	if err != nil {
		return Config{}, err
	}
	if from > target {
		return Config{}, fmt.Errorf(
			"%s is schema %d and this binary understands %d: it was written by a newer governor",
			s.Path, from, target)
	}
	if err := s.migrate(doc, from, target); err != nil {
		return Config{}, err
	}

	// Re-encode the migrated document so the typed decode sees what the chain produced
	// rather than what was on disk.
	merged, err := json.Marshal(doc)
	if err != nil {
		return Config{}, fmt.Errorf("re-encode %s after migration: %w", s.Path, err)
	}
	var typed document
	if err := json.Unmarshal(merged, &typed); err != nil {
		return Config{}, fmt.Errorf("%s: %w", s.Path, err)
	}

	c := Config{Schema: target}
	if typed.Endpoint != nil {
		c.Endpoint = *typed.Endpoint
	}
	if typed.Setpoint != nil {
		c.Setpoint = *typed.Setpoint
	}
	if typed.BMax != nil {
		c.BMax = *typed.BMax
	}
	if typed.WaitMaxMS != nil {
		c.WaitMaxMS = *typed.WaitMaxMS
	}
	c.Unknown = s.unknown(doc)
	return c, nil
}

// schemaOf reads the one key every config must carry.
//
// A file with no "schema" is refused rather than assumed to be schema 1. Assuming would mean
// migrating a document of unknown provenance and writing the result back over it, and the
// only thing worse than refusing to read a user's config is silently rewriting it wrongly.
// `doctor` reports the same file the same way.
func schemaOf(doc map[string]json.RawMessage, path string) (int, error) {
	rawSchema, ok := doc[keySchema]
	if !ok {
		return 0, fmt.Errorf("%s carries no %q key, so nothing can tell which version wrote it",
			path, keySchema)
	}
	var n int
	if err := json.Unmarshal(rawSchema, &n); err != nil {
		return 0, fmt.Errorf("%s has a non-numeric %q: %w", path, keySchema, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s is schema %d, which is not a version", path, n)
	}
	return n, nil
}

// migrate walks the chain forward one version at a time.
func (s *FileStore) migrate(doc map[string]json.RawMessage, from, to int) error {
	if from == to {
		return nil
	}
	steps := map[int]Migration{}
	for _, m := range s.chain() {
		steps[m.To] = m
	}
	for v := from + 1; v <= to; v++ {
		m, ok := steps[v]
		if !ok {
			return fmt.Errorf("%s is schema %d and this binary writes %d, but it has no migration to schema %d",
				s.Path, from, to, v)
		}
		if m.Apply == nil {
			continue
		}
		if err := m.Apply(doc); err != nil {
			return fmt.Errorf("migrating %s to schema %d: %w", s.Path, v, err)
		}
	}
	return nil
}

// unknown collects every key this binary does not recognise, warning once per key.
//
// The values are json.RawMessage holding the original bytes verbatim. Decoding them through
// `any` would round every number through float64, so a newer version's large integer would
// come back mangled — preserved in name and corrupted in value, which is worse than dropping
// it because it looks like it worked.
func (s *FileStore) unknown(doc map[string]json.RawMessage) map[string]any {
	var names []string
	for k := range doc {
		switch k {
		case keySchema, keyEndpoint, keySetpoint, keyBMax, keyWaitMaxMS:
			continue
		}
		names = append(names, k)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make(map[string]any, len(names))
	for _, k := range names {
		s.warn("%s: unrecognised key %q, kept as written — it is probably from a newer governor",
			s.Path, k)
		out[k] = doc[k]
	}
	return out
}

// Save writes the config atomically: a temp file in the same directory, then rename(2).
//
// Atomicity is not a nicety here. `calibrate` writes this file while a gate on the hot path
// may be reading it, and a reader that catches a half-written config is a fail-open event at
// best. rename(2) within a directory is atomic, so a concurrent reader sees either the whole
// old file or the whole new one. Crash durability is a different guarantee and is not
// claimed — a config lost to a power failure is one `calibrate` away.
//
// Unknown keys are written back exactly as they were read. That is what lets one synced
// config serve two machines on two versions without the older binary destroying settings it
// does not understand.
func (s *FileStore) Save(ctx context.Context, c Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := s.targetSchema()

	doc := map[string]any{
		keySchema:    target,
		keyEndpoint:  c.Endpoint,
		keySetpoint:  c.Setpoint,
		keyBMax:      c.BMax,
		keyWaitMaxMS: c.WaitMaxMS,
	}
	for k, v := range c.Unknown {
		if _, clash := doc[k]; clash {
			// A hand-built Config could carry a known key in Unknown. The typed field is
			// the authority; saying so is better than a silent, arbitrary winner.
			s.warn("%s: ignoring %q in Unknown, which is a key this binary owns", s.Path, k)
			continue
		}
		doc[k] = v
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, File+".tmp-*")
	if err != nil {
		return fmt.Errorf("create a temp file beside %s: %w", s.Path, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // No-op once the rename has moved it; cleanup on every failure path.

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, s.Path); err != nil {
		return fmt.Errorf("rename %s over %s: %w", name, s.Path, err)
	}
	return nil
}
