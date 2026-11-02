package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/trace"
)

// GOV-B26 clause 4: a user can find out the trace log is there, and how big it is.

func traceLogFinding(t *testing.T) finding {
	t.Helper()
	d := &diagnosis{}
	d.checkTraceLog()
	return findingNamed(t, d, "tracelog")
}

func TestDoctorNamesTheTraceLogAndItsSize(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(trace.StateDirEnv, dir)

	if f := traceLogFinding(t); f.state != pass || !strings.Contains(f.detail, "no trace log yet") {
		t.Errorf("with no log: %s %q, want ok and 'no trace log yet'", f.state.label(), f.detail)
	}

	writeFile(t, filepath.Join(dir, "samples.jsonl"), strings.Repeat("x", 3000)+"\n")
	writeFile(t, filepath.Join(dir, "samples.jsonl.1"), strings.Repeat("x", 5000)+"\n")
	f := traceLogFinding(t)
	if f.state != pass {
		t.Fatalf("a bounded log is %s, want ok: %s", f.state.label(), f.detail)
	}
	for _, want := range []string{"samples.jsonl 3 KiB", "samples.jsonl.1 5 KiB", "rotating at 16.0 MiB"} {
		if !strings.Contains(f.detail, want) {
			t.Errorf("detail %q is missing %q", f.detail, want)
		}
	}
}

// A file no bounded binary could have written is a warn that names it, and never a failure.
func TestDoctorWarnsAboutALogFromBeforeTheBound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(trace.StateDirEnv, dir)
	old := trace.MaxBytes
	trace.MaxBytes = 1000
	t.Cleanup(func() { trace.MaxBytes = old })

	pre := filepath.Join(dir, "samples.pre-bound-20261102T000000Z.jsonl")
	writeFile(t, pre, strings.Repeat("x", 2001))
	writeFile(t, filepath.Join(dir, "samples.jsonl"), "x\n")

	f := traceLogFinding(t)
	if f.state != warn {
		t.Fatalf("a pre-bound log is %s, want warn: %s", f.state.label(), f.detail)
	}
	if !strings.Contains(f.fix, filepath.Base(pre)) || !strings.Contains(f.fix, "--purge") {
		t.Errorf("the fix should name the file and how it goes: %q", f.fix)
	}

	// Exactly at twice the bound is still something the bound can produce: no warn.
	if err := os.Truncate(pre, 2000); err != nil {
		t.Fatal(err)
	}
	if f := traceLogFinding(t); f.state != pass {
		t.Errorf("a file at exactly 2 x the bound warned: %s", f.detail)
	}
}
