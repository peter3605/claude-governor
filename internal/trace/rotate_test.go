package trace

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// boundAt shrinks the bound for one test, so rotation can be exercised in kilobytes.
func boundAt(t *testing.T, n int64) {
	t.Helper()
	old := MaxBytes
	MaxBytes = n
	t.Cleanup(func() { MaxBytes = old })
}

// fixedTS pins the timestamp, so every record a test writes has a known size: RFC3339Nano
// drops trailing zeros, and a stamped clock would make record lengths vary by a byte or two.
const fixedTS = "2026-11-02T00:00:00.123456789Z"

// rec is sampleRec with the timestamp pinned.
func rec(w, i int) Record {
	r := sampleRec(w, i)
	r.TS = fixedTS
	return r
}

// recSize is the on-disk size of rec with a single-digit index, newline included.
func recSize(t *testing.T) int64 {
	t.Helper()
	r := rec(0, 0)
	r.V = RecordV
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(b) + 1)
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func TestAppendRotatesAtTheBound(t *testing.T) {
	dir := stateDir(t)
	rs := recSize(t)
	boundAt(t, 10*rs)

	for i := 0; i < 15; i++ {
		if err := Append(rec(0, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	live := filepath.Join(dir, "samples.jsonl")
	old := live + RotatedSuffix
	// The tenth record reached the bound and rotated, so .1 holds 0-9 and the live file 10-14.
	if got := readLines(t, old); len(got) != 10 {
		t.Fatalf(".1 has %d records, want 10", len(got))
	}
	if got := readLines(t, live); len(got) != 5 {
		t.Fatalf("live file has %d records, want 5", len(got))
	}
	if s := size(t, live); s >= MaxBytes {
		t.Errorf("live file is %d bytes, at or past the %d-byte bound", s, MaxBytes)
	}
}

func TestTheLogNeverHoldsMoreThanTwoGenerations(t *testing.T) {
	dir := stateDir(t)
	rs := recSize(t)
	boundAt(t, 10*rs)

	for i := 0; i < 200; i++ {
		if err := Append(rec(0, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if len(files) == 0 || len(files) > 2 {
		t.Errorf("want the live file and .1 at most, got %+v", files)
	}
	if total > 2*MaxBytes {
		t.Errorf("log occupies %d bytes, over the %d worst case", total, 2*MaxBytes)
	}
	// The newest record ends the live file — or, when the record that filled it was the last
	// one written, ends .1 with no live file yet. Rotation runs after the write, so both are
	// correct, and a reader of recent records has to look at both (report.go does).
	newest := filepath.Join(dir, "samples.jsonl")
	if _, err := os.Stat(newest); err != nil {
		newest += RotatedSuffix
	}
	lines := readLines(t, newest)
	if !strings.Contains(lines[len(lines)-1], `"agent_id":"0-199"`) {
		t.Errorf("newest record is not the last line written: %q", lines[len(lines)-1])
	}
}

// A log written before the bound existed is set aside under its own name and never deleted.
//
// This is the trap GOV-B26 recorded: bounding what is written from now on is a different
// decision from deleting six weeks of measurement, and rotating the old file to .1 would make
// the second rotation take that decision silently.
func TestAPreBoundLogIsSetAsideNotRotatedAway(t *testing.T) {
	dir := stateDir(t)
	rs := recSize(t)
	boundAt(t, 10*rs)

	live := filepath.Join(dir, "samples.jsonl")
	legacy := bytes.Repeat([]byte(`{"kind":"sample","v":2}`+"\n"), int(3*MaxBytes/24)+1)
	if err := os.WriteFile(live, legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ { // several further rotations
		if err := Append(rec(0, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	pre, _ := filepath.Glob(filepath.Join(dir, PreBoundGlob))
	if len(pre) != 1 {
		t.Fatalf("want one set-aside file, got %v", pre)
	}
	got, err := os.ReadFile(pre[0])
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical, plus only the one record whose write noticed the file was full.
	if !bytes.HasPrefix(got, legacy) {
		t.Fatal("the set-aside file does not begin with the pre-bound log's bytes")
	}
	if extra := len(got) - len(legacy); int64(extra) != rs {
		t.Errorf("set-aside file gained %d bytes, want exactly one record (%d)", extra, rs)
	}
	if s := size(t, live+RotatedSuffix); s > preBoundFactor*MaxBytes {
		t.Errorf(".1 is %d bytes; it should only ever hold a bounded generation", s)
	}
	files, _ := Files()
	if len(files) != 3 || files[2].Path != pre[0] {
		t.Errorf("Files() should list live, .1 and the set-aside file in that order: %+v", files)
	}
}

// Rotation under concurrent appenders loses nothing and never rotates twice.
//
// The bound is set so the run crosses it exactly once. If two appenders both renamed the full
// file, the second would move the fresh one onto .1 and discard the first generation, and the
// record count across both files would come up short.
func TestConcurrentAppendersRotateOnceAndLoseNothing(t *testing.T) {
	dir := stateDir(t)
	rs := recSize(t)

	const writers = 8
	const each = 60
	boundAt(t, writers*each*rs*6/10)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if w%2 == 0 {
				ww, err := Open()
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				defer ww.Close()
				for i := 0; i < each; i++ {
					if err := ww.Write(rec(w, i)); err != nil {
						t.Errorf("write: %v", err)
						return
					}
				}
				return
			}
			for i := 0; i < each; i++ {
				if err := Append(rec(w, i)); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	live := filepath.Join(dir, "samples.jsonl")
	lines := append(readLines(t, live+RotatedSuffix), readLines(t, live)...)
	if len(lines) != writers*each {
		t.Fatalf("want %d records across both files, got %d", writers*each, len(lines))
	}
	seen := map[string]bool{}
	for i, l := range lines {
		var r Record
		if err := json.Unmarshal([]byte(l), &r); err != nil || r.Agent == nil {
			t.Fatalf("record %d is torn: %v %q", i, err, l)
		}
		if seen[r.Agent.AgentID] {
			t.Errorf("duplicate record %s", r.Agent.AgentID)
		}
		seen[r.Agent.AgentID] = true
	}
	if pre, _ := filepath.Glob(filepath.Join(dir, PreBoundGlob)); len(pre) != 0 {
		t.Errorf("a bounded rotation was mistaken for a pre-bound log: %v", pre)
	}
}

// A long-lived writer moves to the new file when another process rotates the log.
func TestWriterFollowsARotationItDidNotMake(t *testing.T) {
	dir := stateDir(t)
	rs := recSize(t)
	boundAt(t, 10*rs)

	w, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Write(rec(1, 0)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ { // the ninth fills it and the appender rotates
		if err := Append(rec(2, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Write(rec(1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec(1, 2)); err != nil {
		t.Fatal(err)
	}

	live := readLines(t, filepath.Join(dir, "samples.jsonl"))
	if len(live) == 0 || !strings.Contains(live[len(live)-1], `"agent_id":"1-2"`) {
		t.Fatalf("the writer did not follow the rotation into the live file: %q", live)
	}
	if s := size(t, filepath.Join(dir, "samples.jsonl"+RotatedSuffix)); s > MaxBytes+rs {
		t.Errorf(".1 kept growing after rotation (%d bytes): the writer stayed on the old file", s)
	}
}

// A long-lived writer whose file is removed by hand starts a new one at the path, rather than
// appending for the rest of its life to a file nothing can read or delete.
func TestWriterReopensWhenTheLogIsRemovedUnderIt(t *testing.T) {
	dir := stateDir(t)
	live := filepath.Join(dir, "samples.jsonl")

	w, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Write(rec(1, 0)); err != nil {
		t.Fatal(err)
	}
	// What `gzip samples.jsonl` does: the name goes, the inode lives on in the writer.
	if err := os.Rename(live, filepath.Join(dir, "moved-away")); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec(1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec(1, 2)); err != nil {
		t.Fatal(err)
	}
	got := readLines(t, live)
	if len(got) != 1 || !strings.Contains(got[0], `"agent_id":"1-2"`) {
		t.Fatalf("after the log was removed the writer should have started a new one: %q", got)
	}
}

func TestFilesIsEmptyWithNoLog(t *testing.T) {
	stateDir(t)
	files, err := Files()
	if err != nil || len(files) != 0 {
		t.Fatalf("Files() = %+v, %v; want nothing", files, err)
	}
}

// BenchmarkAppendBuild is the gate's per-record cost, which buildlog.go quotes. The bound adds
// one fstat on the open descriptor to it; re-measure when either changes.
func BenchmarkAppendBuild(b *testing.B) {
	dir := b.TempDir()
	b.Setenv(StateDirEnv, dir)
	rec := Record{Kind: KindBuild, Build: &Build{
		SessionID: "06e332e4-ad3e-4f9b-8665-5a203fb48de1", Shapes: []string{"go test"},
		Class: "heavy", Weight: 2, Outcome: "granted", CWD: "/Users/someone/Repos/project",
	}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Append(rec); err != nil {
			b.Fatal(err)
		}
	}
}
