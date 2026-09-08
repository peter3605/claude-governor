package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(StateDirEnv, dir)
	return dir
}

func TestAppendWritesNDJSON(t *testing.T) {
	dir := stateDir(t)

	if err := Append(Record{Kind: KindAgent, Agent: &Agent{
		Event: "SubagentStart", SessionID: "s1", AgentID: "a1", AgentType: "Explore",
	}}); err != nil {
		t.Fatalf("append agent: %v", err)
	}
	if err := Append(Record{Kind: KindSample, Sample: &Sample{CPU: 0.42, Cores: 8, Thermal: "nominal"}}); err != nil {
		t.Fatalf("append sample: %v", err)
	}

	lines := readLines(t, filepath.Join(dir, "samples.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}

	var a Record
	if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if a.Kind != KindAgent || a.Agent == nil {
		t.Fatalf("line 1 is not an agent record: %+v", a)
	}
	if a.Agent.AgentType != "Explore" || a.Agent.SessionID != "s1" || a.Agent.AgentID != "a1" {
		t.Errorf("agent fields lost: %+v", a.Agent)
	}
	if a.TS == "" {
		t.Error("no timestamp stamped")
	}
	if a.V != RecordV {
		t.Errorf("record version = %d, want %d", a.V, RecordV)
	}
	if a.Sample != nil {
		t.Error("agent record carries a sample")
	}

	var s Record
	if err := json.Unmarshal([]byte(lines[1]), &s); err != nil {
		t.Fatalf("line 2 is not JSON: %v", err)
	}
	if s.Sample == nil || s.Sample.CPU != 0.42 {
		t.Errorf("sample fields lost: %+v", s.Sample)
	}
	if s.Agent != nil {
		t.Error("sample record carries an agent")
	}
}

// TestAgentTypeIsOptional guards a measured fact rather than a hypothetical.
//
// GOV-01 found tool_input.subagent_type absent on a nested spawn, so an empty agent type is
// a real observation. It must round-trip as an absent field, not become the string "" that a
// later analysis mistakes for a type.
func TestAgentTypeIsOptional(t *testing.T) {
	dir := stateDir(t)
	if err := Append(Record{Kind: KindAgent, Agent: &Agent{Event: "SubagentStart", AgentID: "a1"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	line := readLines(t, filepath.Join(dir, "samples.jsonl"))[0]
	if strings.Contains(line, "agent_type") {
		t.Errorf("empty agent_type was written instead of omitted: %s", line)
	}
	var r Record
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Agent.AgentID != "a1" {
		t.Errorf("agent id lost: %+v", r.Agent)
	}
}

// TestConcurrentAppendersDoNotTear is the property the whole file format rests on. The 1 Hz
// sampler holds the log open while a hook process appends to it on every subagent start and
// stop, so several writers are the normal case, not an edge case. If a record could
// interleave with another mid-line, every line after the first tear would be unparseable and
// the trace would be worthless exactly when the machine was busiest.
func TestConcurrentAppendersDoNotTear(t *testing.T) {
	dir := stateDir(t)

	const writers = 8
	const each = 60

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Half hold the file open like the sampler; half open per record like a hook.
			if w%2 == 0 {
				ww, err := Open()
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				defer ww.Close()
				for i := 0; i < each; i++ {
					if err := ww.Write(sampleRec(w, i)); err != nil {
						t.Errorf("write: %v", err)
						return
					}
				}
				return
			}
			for i := 0; i < each; i++ {
				if err := Append(sampleRec(w, i)); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	lines := readLines(t, filepath.Join(dir, "samples.jsonl"))
	if len(lines) != writers*each {
		t.Fatalf("want %d lines, got %d", writers*each, len(lines))
	}
	seen := make(map[string]bool, len(lines))
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %d is torn or interleaved: %v\n%q", i+1, err, line)
		}
		if r.Agent == nil {
			t.Fatalf("line %d lost its payload: %q", i+1, line)
		}
		if seen[r.Agent.AgentID] {
			t.Errorf("duplicate record %s", r.Agent.AgentID)
		}
		seen[r.Agent.AgentID] = true
	}
}

func sampleRec(w, i int) Record {
	// Padded so records are long enough that a non-atomic write would reliably interleave.
	return Record{Kind: KindAgent, Agent: &Agent{
		Event:     "SubagentStart",
		SessionID: strings.Repeat("s", 200),
		AgentID:   itoa(w) + "-" + itoa(i),
		CWD:       strings.Repeat("/padding", 30),
	}}
}

func TestLogPathHonoursStateDirOverride(t *testing.T) {
	dir := stateDir(t)
	got, err := LogPath()
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if want := filepath.Join(dir, "samples.jsonl"); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}

func TestAppendCreatesTheStateDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "governor")
	t.Setenv(StateDirEnv, dir)

	if err := Append(Record{Kind: KindSample, Sample: &Sample{}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "samples.jsonl")); err != nil {
		t.Fatalf("log not created: %v", err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
