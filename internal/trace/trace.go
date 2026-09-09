// Package trace writes the Phase 0 instrumentation log.
//
// Phase 0 (design.md section 5.2, "tune setpoint against the Phase 0 traces") measures what
// a real machine actually does under real fan-out, so that the control loop's constants come
// from evidence rather than from feel. Nothing in this package gates, blocks, denies or
// decides anything — it only records.
//
// The log is newline-delimited JSON at ${GOVERNOR_STATE_DIR}/samples.jsonl, defaulting to
// ~/.claude/governor/samples.jsonl. Two kinds of process append to it concurrently: the 1 Hz
// sampler, and a hook process per subagent start and stop. Each record is therefore
// marshalled whole and handed to a single Write on a file opened O_APPEND, which is the
// standard multiple-appender arrangement — the kernel makes one such write atomic, so
// records interleave but never tear.
package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StateDirEnv overrides the state directory. It exists for tests, which must never write to
// the real log, and for anyone who keeps ~/.claude on a synced volume.
const StateDirEnv = "GOVERNOR_STATE_DIR"

// Kinds of record. Every line carries one, because a single log with two shapes is only
// readable if each line says which shape it is.
const (
	KindAgent  = "agent"
	KindSample = "sample"
)

// Record is one line of the log.
//
// It is one struct with an explicit kind rather than two types, so that a reader can
// unmarshal the file without first partially parsing each line to decide what to unmarshal
// it into. Every field that does not apply to a kind is omitted.
type Record struct {
	// TS is when the record was made, RFC3339 with nanoseconds, always UTC.
	TS string `json:"ts"`
	// Kind is KindAgent or KindSample.
	Kind string `json:"kind"`
	// V is the record schema version. It is not the wire protocol version — this is a file
	// that outlives the binary that wrote it, and a trace whose shape changed silently
	// halfway through is a trace nobody can analyse.
	V int `json:"v"`

	Agent  *Agent  `json:"agent,omitempty"`
	Sample *Sample `json:"sample,omitempty"`
}

// Agent is a subagent lifecycle observation: SubagentStart, SubagentStop, Stop, or
// PostToolUse on the Agent tool.
//
// The fields are exactly what the hook payload carries (design.md section 10.1, measured in
// GOV-01), and nothing is synthesised. In particular AgentType may legitimately be empty:
// tool_input.subagent_type is optional and was absent on a nested spawn, so a record with no
// type is a real observation rather than a dropped field.
type Agent struct {
	// Event is the hook_event_name from the payload.
	Event string `json:"event"`
	// SessionID identifies the Claude Code session.
	SessionID string `json:"session_id"`
	// AgentID identifies the subagent. It does NOT correlate to the PreToolUse
	// tool_use_id — that was measured and settled in GOV-01.
	AgentID string `json:"agent_id"`
	// AgentType is tool_input.subagent_type, and may be empty.
	AgentType string `json:"agent_type,omitempty"`
	// PromptID is per turn, not per spawn. Recorded because it is the only field that
	// groups a burst of spawns together.
	PromptID string `json:"prompt_id,omitempty"`
	// CWD is the session's working directory, which is what attributes load to a project.
	CWD string `json:"cwd,omitempty"`
	// ToolName, ToolUseID and the two Tool* fields below are the PostToolUse correlation
	// fields (GOV-27). They are empty on SubagentStart, SubagentStop and Stop, which carry
	// none of them.
	//
	// PostToolUse on the Agent tool is the only event on the surface that carries BOTH the
	// tool_use_id of the spawn and the agent_id it produced (hook-surface.md section 6), so
	// it is the one place a lease taken at PreToolUse can be tied to an agent exactly rather
	// than by per-session FIFO. That matters here because SubagentStart — the event the
	// design originally used for it — does not fire in interactive sessions at all
	// (section 8.1).
	ToolName  string `json:"tool_name,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// ToolStatus is tool_response.status: "completed" for a foreground spawn, which has
	// already finished by the time this arrives, or "async_launched" for a background one,
	// which has only just begun. The two demand opposite handling and the field is the only
	// thing that separates them.
	ToolStatus string `json:"tool_status,omitempty"`
	// ToolAgentID is tool_response.agentId — the agent this spawn produced.
	ToolAgentID string `json:"tool_agent_id,omitempty"`
	// ToolDurationMS is tool_response.totalDurationMs, the agent's own measure of how long
	// it ran. It is recorded because the lease TTLs are supposed to outlive a real agent
	// and were previously calibrated against three probe runs; this is the first source of
	// that number at any scale.
	ToolDurationMS float64 `json:"tool_duration_ms,omitempty"`

	// RawKeys is the sorted list of top-level keys the payload actually carried.
	//
	// It is here because without it an absent AgentType is ambiguous, and the ambiguity is
	// not academic: GOV-01 measured subagent_type as optional, so a missing type may be a
	// faithful record of what the harness sent — or it may be this hook silently dropping a
	// field, which would be a defect in the instrument rather than a finding about the
	// system. Names only, never values: a payload carries the agent's prompt.
	//
	// Reading it, since the three cases are easy to confuse and one of them already caused a
	// false accusation against this code during GOV-02's own verification:
	//
	//	agent_type NOT in RawKeys, no AgentType  →  the harness sent no such field
	//	agent_type in RawKeys, no AgentType      →  the harness sent the field, empty or null
	//	agent_type in RawKeys, AgentType set     →  the ordinary case
	//
	// Both of the first two are real observations rather than losses. What RawKeys rules out
	// is the fourth combination — a field present and non-empty on the wire but missing from
	// the record — which is the only one that would mean this instrument is lying.
	RawKeys []string `json:"raw_keys,omitempty"`
}

// Sample is one 1 Hz reading of the host.
type Sample struct {
	// CPU is host utilisation in [0,1].
	CPU float64 `json:"cpu"`
	// LoadAvg1 is the raw 1-minute load average; RunQueue is it divided by Cores.
	LoadAvg1 float64 `json:"loadavg1"`
	RunQueue float64 `json:"runqueue"`
	Cores    int     `json:"cores"`
	// MemAvailableBytes and MemTotalBytes are raw; MemoryPressure is derived from them.
	MemAvailableBytes uint64  `json:"mem_available_bytes"`
	MemTotalBytes     uint64  `json:"mem_total_bytes"`
	MemoryPressure    float64 `json:"mem_pressure"`
	// Thermal is nominal, fair, serious, critical, or unknown on a host with no thermal
	// API. Unknown is not a hot machine and must never be read as one.
	Thermal string `json:"thermal"`

	// ClaudeRoots is the number of `claude` session processes.
	ClaudeRoots int `json:"claude_roots"`
	// ClaudeProcs is ClaudeRoots plus every transitive descendant.
	ClaudeProcs int `json:"claude_procs"`
	// ClaudeCPURoots is cores busy in the session processes alone; ClaudeCPU is cores busy
	// across the whole tree. The gap between them is the load that agents caused but did
	// not themselves burn — the build, the test run, the language server — and separating
	// the two is the finding Phase 0 is being run to get.
	ClaudeCPURoots float64 `json:"claude_cpu_roots"`
	ClaudeCPU      float64 `json:"claude_cpu"`
	ClaudeRSSBytes uint64  `json:"claude_rss_bytes"`

	// CostMS is what collecting this sample cost, in milliseconds. A sampler that becomes
	// expensive invalidates the baseline it is measuring, so its own cost is part of the
	// measurement rather than a thing asserted once in a commit message.
	CostMS float64 `json:"cost_ms"`
}

// RecordV is the current record schema version.
//
// Bumped to 2 by GOV-27, which added the PostToolUse correlation fields to Agent and began
// recording two further events (PostToolUse and Stop). The addition is backward compatible —
// every v1 field means what it always did — but the file is read as a whole by Phase 0
// analysis and "no PostToolUse records before this line" is a fact about the instrument
// rather than about the machine. That distinction is the entire reason this field exists.
const RecordV = 2

// StateDir is the directory holding the governor's state, per design.md section 13.3.
func StateDir() (string, error) {
	if d := os.Getenv(StateDirEnv); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "governor"), nil
}

// LogPath is the full path of the trace log.
func LogPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "samples.jsonl"), nil
}

// Append writes one record to the log, creating the directory and file if needed.
//
// TS is stamped here rather than by the caller so that every record in the file comes from
// one clock and one format.
func Append(r Record) error {
	path, err := LogPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return write(f, r)
}

// Writer holds the log open for a long-lived appender, so the 1 Hz sampler does not pay an
// open and a close every second.
type Writer struct{ f *os.File }

// Open returns a Writer appending to the log.
func Open() (*Writer, error) {
	path, err := LogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f}, nil
}

// Write appends one record.
func (w *Writer) Write(r Record) error { return write(w.f, r) }

// Close releases the file.
func (w *Writer) Close() error { return w.f.Close() }

// write marshals the record and emits it in exactly one Write call. The single call is the
// whole reason concurrent appenders are safe; splitting the newline into a second write
// would let another process interleave between a record and its terminator.
func write(f *os.File, r Record) error {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if r.V == 0 {
		r.V = RecordV
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
