package config

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/peter3605/claude-governor/internal/transport"
)

func storeIn(t *testing.T) *FileStore {
	t.Helper()
	return &FileStore{Path: filepath.Join(t.TempDir(), File)}
}

func write(t *testing.T, s *FileStore, body string) {
	t.Helper()
	if err := os.WriteFile(s.Path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed %s: %v", s.Path, err)
	}
}

func load(t *testing.T, s *FileStore) Config {
	t.Helper()
	c, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func save(t *testing.T, s *FileStore, c Config) {
	t.Helper()
	if err := s.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func onDisk(t *testing.T, s *FileStore) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatalf("read back %s: %v", s.Path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", s.Path, err, raw)
	}
	return doc
}

// A machine that has never been calibrated has no config file. design.md section 13.4 makes
// that a state and not a fault, and `doctor` already reports it as a pass — so Load must
// agree rather than making every caller special-case it.
func TestAMissingConfigLoadsAsBuiltInDefaults(t *testing.T) {
	s := storeIn(t)
	c := load(t, s)
	if c.Schema != Schema {
		t.Fatalf("schema %d, want this binary's %d", c.Schema, Schema)
	}
	if !reflect.DeepEqual(c, Config{Schema: Schema}) {
		t.Fatalf("a missing config loaded as %+v, want the zero Config", c)
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatalf("Load created %s; asking what the config says must not write one", s.Path)
	}
}

// "Do not add a config key without a default that makes the feature inert." A config that
// sets nothing must load as the zero Config, so an upgraded binary reading an old file
// behaves exactly as before until the user opts in.
func TestAConfigThatSetsNothingLeavesEveryFeatureInert(t *testing.T) {
	s := storeIn(t)
	write(t, s, `{"schema": 1}`)
	if c := load(t, s); !reflect.DeepEqual(c, Config{Schema: 1}) {
		t.Fatalf("loaded %+v, want the zero Config with schema 1", c)
	}
}

func TestTheKnownKeysRoundTrip(t *testing.T) {
	s := storeIn(t)
	want := Config{Schema: Schema, Endpoint: "unix:///tmp/sock", Setpoint: 0.62, BMax: 7, WaitMaxMS: 1500}
	save(t, s, want)
	if got := load(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip gave %+v, want %+v", got, want)
	}
	doc := onDisk(t, s)
	if _, ok := doc[keySchema]; !ok {
		t.Fatalf("the file carries no %q key: %v", keySchema, doc)
	}
}

// The two-machines-one-synced-config case, and the reason unknown keys are preserved at all:
// a newer governor adds a key within the same schema, and the older binary must hand it back
// untouched rather than deleting a setting it does not understand.
func TestAnUnknownKeySurvivesARoundTripAtTheSameSchema(t *testing.T) {
	s := storeIn(t)
	write(t, s, `{"schema": 1, "b_max": 4, "tier_two_broker": "tcp://broker:9000"}`)

	c := load(t, s)
	if c.BMax != 4 {
		t.Fatalf("b_max %d, want 4", c.BMax)
	}
	if len(c.Unknown) != 1 {
		t.Fatalf("Unknown is %v, want the one unrecognised key", c.Unknown)
	}
	save(t, s, c)

	var back struct {
		Broker string `json:"tier_two_broker"`
		BMax   int    `json:"b_max"`
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.Broker != "tcp://broker:9000" {
		t.Fatalf("the unrecognised key came back as %q, want it written exactly as it was read", back.Broker)
	}
	if back.BMax != 4 {
		t.Fatalf("b_max came back as %d, want 4", back.BMax)
	}
}

// The Done-when's test, in as many words: a schema-1 config carrying an unknown key, loaded
// by a binary that writes schema 2, migrated forward, and saved — with the key still there.
func TestAHypotheticalSchema2BinaryMigratesAndPreservesAnUnknownKey(t *testing.T) {
	// A stand-in for the next binary: it writes schema 2 and knows one step to get there.
	// The step does real work — it renames a key — so that a chain that silently did nothing
	// could not pass this test.
	s := storeIn(t)
	s.Schema = 2
	s.Migrations = []Migration{{
		To: 2,
		Apply: func(doc map[string]json.RawMessage) error {
			// Schema 2 renamed wait_ms to wait_max_ms. The step does real work, so a chain
			// that silently did nothing could not pass this test: without it, wait_ms would
			// arrive as an unknown key and WaitMaxMS would be zero.
			if v, ok := doc["wait_ms"]; ok {
				doc[keyWaitMaxMS] = v
				delete(doc, "wait_ms")
			}
			return nil
		},
	}}

	write(t, s, `{"schema": 1, "wait_ms": 900, "future_key": {"nested": [1, 2, 3]}}`)

	c := load(t, s)
	if c.Schema != 2 {
		t.Fatalf("loaded schema %d, want it migrated forward to 2", c.Schema)
	}
	if c.WaitMaxMS != 900 {
		t.Fatalf("wait_max_ms %d, want 900 carried through the migration", c.WaitMaxMS)
	}
	if _, ok := c.Unknown["future_key"]; !ok {
		t.Fatalf("Unknown is %v, want future_key preserved", c.Unknown)
	}
	if _, stale := c.Unknown["wait_ms"]; stale {
		t.Fatalf("Unknown is %v, want the migrated-away key gone", c.Unknown)
	}

	save(t, s, c)

	doc := onDisk(t, s)
	var schema int
	if err := json.Unmarshal(doc[keySchema], &schema); err != nil {
		t.Fatalf("schema key: %v", err)
	}
	if schema != 2 {
		t.Fatalf("saved schema %d, want 2", schema)
	}
	got, ok := doc["future_key"]
	if !ok {
		t.Fatalf("future_key was dropped on save: %v", doc)
	}
	if compact(t, got) != `{"nested":[1,2,3]}` {
		t.Fatalf("future_key came back as %s, want it byte-faithful", got)
	}
}

// A number a newer governor wrote must come back as the same number. Decoding unknown values
// through `any` rounds every one of them through float64, which would hand back a mangled
// value that still looks preserved — worse than dropping it, because nothing reports it.
func TestAnUnknownValueSurvivesWithoutPrecisionLoss(t *testing.T) {
	s := storeIn(t)
	write(t, s, `{"schema": 1, "ledger_id": 9007199254740993, "ratio": 0.1000000000000000055511151231257827}`)

	save(t, s, load(t, s))

	doc := onDisk(t, s)
	if got := compact(t, doc["ledger_id"]); got != "9007199254740993" {
		t.Fatalf("ledger_id came back as %s, want the integer it was written with", got)
	}
	if got := compact(t, doc["ratio"]); got != "0.1000000000000000055511151231257827" {
		t.Fatalf("ratio came back as %s, want it byte-faithful", got)
	}
}

// "Warned about and preserved rather than dropped or fatal" — all three clauses.
func TestAnUnknownKeyIsWarnedAboutAndIsNotFatal(t *testing.T) {
	var warnings []string
	s := storeIn(t)
	s.Warn = func(msg string) { warnings = append(warnings, msg) }
	write(t, s, `{"schema": 1, "setpoint": 0.7, "alpha": 1, "beta": 2}`)

	c := load(t, s) // Not fatal: Load returning an error would have failed here.
	if c.Setpoint != 0.7 {
		t.Fatalf("setpoint %v, want the known key still read", c.Setpoint)
	}
	if len(c.Unknown) != 2 {
		t.Fatalf("Unknown is %v, want both unrecognised keys", c.Unknown)
	}
	if len(warnings) != 2 {
		t.Fatalf("got %d warnings %q, want one per unrecognised key", len(warnings), warnings)
	}
	for _, key := range []string{"alpha", "beta"} {
		if !strings.Contains(strings.Join(warnings, "\n"), key) {
			t.Fatalf("no warning names %q: %q", key, warnings)
		}
	}
}

// The hot path has a 100 ms budget and nowhere to print. A nil Warn must be silent, not a
// nil-pointer dereference in the gate.
func TestANilWarnIsSilentRatherThanAPanic(t *testing.T) {
	s := storeIn(t)
	write(t, s, `{"schema": 1, "unrecognised": true}`)
	if c := load(t, s); len(c.Unknown) != 1 {
		t.Fatalf("Unknown is %v, want the key preserved", c.Unknown)
	}
}

// The one case that cannot be handled: this binary does not know what changed, so it can
// neither read a future config nor safely rewrite it.
func TestAConfigFromTheFutureIsRefused(t *testing.T) {
	s := storeIn(t)
	write(t, s, `{"schema": 99, "b_max": 3}`)
	_, err := s.Load(context.Background())
	if err == nil {
		t.Fatal("a schema-99 config loaded without error; this binary cannot migrate one")
	}
	if !strings.Contains(err.Error(), "newer governor") {
		t.Fatalf("error %q does not say the file came from a newer governor", err)
	}
}

// Refused rather than assumed to be schema 1. Assuming would mean migrating a document of
// unknown provenance and writing the result back over it.
func TestAConfigWithNoSchemaKeyIsRefused(t *testing.T) {
	s := storeIn(t)
	write(t, s, `{"b_max": 3}`)
	_, err := s.Load(context.Background())
	if err == nil {
		t.Fatal("a config with no schema key loaded without error")
	}
	if !strings.Contains(err.Error(), keySchema) {
		t.Fatalf("error %q does not name the missing key", err)
	}
}

func TestUnreadableConfigsAreRefusedRatherThanTreatedAsAbsent(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not JSON at all", `this is not json`},
		{"a JSON array", `[1, 2, 3]`},
		{"a non-numeric schema", `{"schema": "one"}`},
		{"a schema below 1", `{"schema": 0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := storeIn(t)
			write(t, s, tc.body)
			if _, err := s.Load(context.Background()); err == nil {
				t.Fatalf("%s loaded without error", tc.name)
			}
		})
	}
}

// A gap in the chain must stop the load, not skip a version. Migrating 1 to 3 by way of
// nothing would hand schema-1 keys to schema-3 code, which is the corruption the version
// number exists to prevent.
func TestAMissingMigrationStepIsRefusedRatherThanSkipped(t *testing.T) {
	s := storeIn(t)
	s.Schema = 3
	s.Migrations = []Migration{{To: 3, Apply: func(map[string]json.RawMessage) error { return nil }}}
	write(t, s, `{"schema": 1}`)

	_, err := s.Load(context.Background())
	if err == nil {
		t.Fatal("a load with no migration to schema 2 succeeded")
	}
	if !strings.Contains(err.Error(), "schema 2") {
		t.Fatalf("error %q does not name the missing step", err)
	}
}

// `calibrate` writes this file while a gate on the hot path may be reading it. rename(2)
// within a directory is atomic, so a concurrent reader sees either the whole old file or the
// whole new one — never half of either.
func TestSaveIsAtomicForAConcurrentReader(t *testing.T) {
	s := storeIn(t)
	save(t, s, Config{Endpoint: strings.Repeat("x", 4096)})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	torn := make(chan string, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(s.Path)
			if os.IsNotExist(err) {
				select {
				case torn <- "the config vanished mid-write":
				default:
				}
				return
			}
			if err != nil {
				continue
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(raw, &doc); err != nil {
				select {
				case torn <- "read a partial config: " + err.Error():
				default:
				}
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		save(t, s, Config{BMax: i, Endpoint: strings.Repeat("x", 4096)})
	}
	close(stop)
	wg.Wait()

	select {
	case msg := <-torn:
		t.Fatalf("a concurrent reader %s; the write is not atomic", msg)
	default:
	}
}

func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	s := storeIn(t)
	save(t, s, Config{BMax: 2})
	save(t, s, Config{BMax: 3})

	if got := siblings(t, s); len(got) != 1 || got[0] != File {
		t.Fatalf("the directory holds %v, want only %s", got, File)
	}
}

// The failure path, which is the only one where the cleanup can be observed at all: on
// success the rename has already moved the temp file, so a Save that never cleaned up would
// still look tidy. A governor that littered its own state directory with a temp file per
// failed calibrate would fill it silently.
func TestAFailedSaveLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory standing where config.json should be. rename(2) refuses to
	// replace it, so Save fails after the temp file exists.
	s := &FileStore{Path: filepath.Join(dir, File)}
	if err := os.Mkdir(s.Path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", s.Path, err)
	}
	if err := os.WriteFile(filepath.Join(s.Path, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("occupy %s: %v", s.Path, err)
	}

	if err := s.Save(context.Background(), Config{BMax: 1}); err == nil {
		t.Fatal("Save over a non-empty directory succeeded")
	}
	if got := siblings(t, s); len(got) != 1 || got[0] != File {
		t.Fatalf("a failed Save left %v behind, want only %s", got, File)
	}
}

func siblings(t *testing.T, s *FileStore) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(s.Path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSaveCreatesTheStateDirectoryPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "governor")
	s := &FileStore{Path: filepath.Join(dir, File)}
	save(t, s, Config{BMax: 1})

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("%s is %04o, want 0700", dir, perm)
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatalf("stat %s: %v", s.Path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("%s is %04o, want 0600", s.Path, perm)
	}
}

// A hand-built Config could carry a key this binary owns inside Unknown. The typed field
// wins, and the collision is reported rather than resolved arbitrarily.
func TestAKnownKeyInsideUnknownDoesNotOverwriteTheTypedField(t *testing.T) {
	var warnings []string
	s := storeIn(t)
	s.Warn = func(msg string) { warnings = append(warnings, msg) }
	save(t, s, Config{BMax: 5, Unknown: map[string]any{keyBMax: json.RawMessage(`99`)}})

	if c := load(t, s); c.BMax != 5 {
		t.Fatalf("b_max is %d, want the typed field's 5", c.BMax)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], keyBMax) {
		t.Fatalf("warnings %q, want one naming the clash on %q", warnings, keyBMax)
	}
}

// The store writes this binary's schema whatever the file it read said. Anything else would
// leave a migrated document stamped with the version it came from.
func TestSaveStampsThisBinarysSchema(t *testing.T) {
	s := storeIn(t)
	save(t, s, Config{Schema: 0})
	var doc struct {
		Schema int `json:"schema"`
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if doc.Schema != Schema {
		t.Fatalf("saved schema %d, want %d", doc.Schema, Schema)
	}
}

func TestACancelledContextStopsBeforeTouchingTheFile(t *testing.T) {
	s := storeIn(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Load(ctx); err == nil {
		t.Fatal("Load on a cancelled context succeeded")
	}
	if err := s.Save(ctx, Config{}); err == nil {
		t.Fatal("Save on a cancelled context succeeded")
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatalf("Save wrote %s despite a cancelled context", s.Path)
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.String()
}

// GOV-44, clause 1: the writer and the reader resolve the same file whoever started them.
//
// `calibrate` runs from a terminal, where CLAUDE_PLUGIN_DATA is unset. A daemon a gate starts
// is a child of a hook, where Claude Code sets it. Path used to prefer that variable, so the
// two resolved different files and every gate-started daemon ran uncalibrated. The assertion
// is therefore not "Path is in the state directory" alone: it is that setting the plugin
// variable does not move it.
func TestPathIsTheSameWithOrWithoutThePluginDataDirectory(t *testing.T) {
	state := t.TempDir()
	t.Setenv(transport.StateDirEnv, state)

	t.Setenv(DataDirEnv, "")
	terminal, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	plugin := t.TempDir()
	t.Setenv(DataDirEnv, plugin)
	hook, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if terminal != hook {
		t.Fatalf("a terminal resolves %s and a hook-started process resolves %s: calibrate and a gate-started daemon read different files", terminal, hook)
	}
	if want := filepath.Join(state, File); hook != want {
		t.Fatalf("Path is %s, want %s", hook, want)
	}

	// And it is a name, not a side effect: a doctor or a statusline that conjured a
	// directory while merely asking where one would be is what design.md 13.3 forbids.
	for _, dir := range []string{state, plugin} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Fatalf("Path created %v inside %s", entries, dir)
		}
	}
}

// The config lives in the state directory, beside the socket. One directory holds the
// governor's state, so uninstall has one place to look.
func TestPathIsInTheStateDirectory(t *testing.T) {
	t.Setenv(DataDirEnv, "")
	state := t.TempDir()
	t.Setenv(transport.StateDirEnv, state)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(state, File); got != want {
		t.Fatalf("Path is %s, want %s", got, want)
	}
}

// The end-to-end shape, driven through Path rather than a hand-built path: a saved config is
// a config.json in the state directory carrying a schema number.
func TestASavedConfigIsAConfigJSONCarryingASchemaNumber(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(transport.StateDirEnv, dir)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	s := &FileStore{Path: path}
	save(t, s, Config{BMax: 4})

	raw, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatalf("no config.json in the state directory: %v", err)
	}
	var doc struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.json is not valid JSON: %v", err)
	}
	if doc.Schema == nil || *doc.Schema != Schema {
		t.Fatalf("config.json carries schema %v, want %d", doc.Schema, Schema)
	}
}
