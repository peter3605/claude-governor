package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/trace"
)

// Phase 0 instrumentation (GOV-02).
//
// `governor trace` is the whole of Phase 0 and it is deliberately separate from the gating
// commands. `gate`, `confirm` and `release` are Phase 1 and belong to their own stories;
// giving them a temporary second meaning here would leave two definitions of "confirm" in
// the tree and a story later that has to guess which one it is replacing. Instead this is
// its own command family, installed by its own reversible installer, and deleted whole when
// Phase 1 lands.
//
// The absolute rule for everything in this file: no path exits non-zero, and nothing but
// `trace status` ever writes to stdout. These bodies run as hooks on every subagent start
// and stop, and a hook that fails or speaks out of turn can break a session. Measuring the
// baseline is not worth breaking the thing being measured.

// hookDeadline force-exits a hook body if it has not finished in time.
//
// A hook reads its payload from stdin, and Claude Code closes that stdin. If it ever did
// not — a harness change, a crash mid-write, a pipe inherited by something else — a plain
// read would block forever inside a hook the session is waiting on, and the session would
// hang. This is invariant 1 (the gate fails open) applied to the instrumentation: never be
// the reason a session stops.
//
// A var, not a const, so its own test can shrink it. The property being guarded — that a
// returning hook body leaves no armed exit behind — is otherwise only observable by waiting
// out the full deadline.
var hookDeadline = 3 * time.Second

// watchdogExit is what the watchdog does when it fires. Injectable so that the disarm
// behaviour can be tested without the test binary actually exiting.
var watchdogExit = func() { os.Exit(0) }

// samplerIdleExit is how long the sampler runs with no claude session present before it
// stops. Without it, a sampler started by a session that has since quit would keep waking
// once a second forever, which is exactly the kind of invisible residue that makes people
// distrust a tool.
const samplerIdleExit = 5 * time.Minute

func runTrace(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: governor trace <agent|sample|ensure|install|uninstall|status>")
		return 2
	}
	switch args[0] {
	case "agent":
		return runTraceAgent(stderr)
	case "sample":
		return runTraceSample(stderr)
	case "ensure":
		return runTraceEnsure(stderr)
	case "install":
		return runTraceInstall(args[1:], stdout, stderr)
	case "uninstall":
		return runTraceUninstall(args[1:], stdout, stderr)
	case "status":
		return runTraceStatus(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "governor trace: unknown subcommand %q\n", args[0])
		return 2
	}
}

// runTraceAgent is the hook body for every lifecycle event Phase 0 observes:
// SubagentStart, SubagentStop, Stop, and PostToolUse on the Agent tool.
//
// One command serves all of them because the payload names itself in hook_event_name, and
// commands that differ only in a string they could have read is several things to keep in
// step.
//
// PostToolUse and Stop were added by GOV-27. SubagentStart does not fire in interactive
// sessions (hook-surface.md section 8.1), so the event the lease lifecycle was built on is
// absent exactly where it matters; these two are the candidate replacements and this hook is
// how the claim that they DO fire there gets measured rather than assumed.
//
// It returns 0 unconditionally. Every failure — unparseable payload, unwritable log, no
// home directory — is written to stderr and swallowed. Phase 0 losing a record is a gap in
// a trace; Phase 0 failing a hook is a broken session.
func runTraceAgent(stderr io.Writer) int {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "governor trace agent: recovered: %v\n", r)
		}
	}()

	defer watchdog(hookDeadline)()

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(stderr, "governor trace agent: read stdin: %v\n", err)
		return 0
	}

	// Decoded loosely on purpose. The payload shape was measured in GOV-01, but a harness
	// upgrade that adds or renames a field must degrade to a thinner record, never to a
	// failed hook.
	var p struct {
		HookEventName string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
		AgentID       string `json:"agent_id"`
		AgentType     string `json:"agent_type"`
		PromptID      string `json:"prompt_id"`
		CWD           string `json:"cwd"`
		// PostToolUse only. tool_input is deliberately NOT decoded: it carries the agent's
		// prompt, and this log is not the place for it.
		ToolName  string `json:"tool_name"`
		ToolUseID string `json:"tool_use_id"`
		// Named fields only, for the same reason — tool_response also carries
		// last_assistant_message.
		ToolResponse struct {
			Status        string  `json:"status"`
			AgentID       string  `json:"agentId"`
			TotalDuration float64 `json:"totalDurationMs"`
		} `json:"tool_response"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		fmt.Fprintf(stderr, "governor trace agent: parse payload: %v\n", err)
		return 0
	}

	rec := trace.Record{
		Kind: trace.KindAgent,
		Agent: &trace.Agent{
			RawKeys:        payloadKeys(payload),
			Event:          p.HookEventName,
			SessionID:      p.SessionID,
			AgentID:        p.AgentID,
			AgentType:      p.AgentType,
			PromptID:       p.PromptID,
			CWD:            p.CWD,
			ToolName:       p.ToolName,
			ToolUseID:      p.ToolUseID,
			ToolStatus:     p.ToolResponse.Status,
			ToolAgentID:    p.ToolResponse.AgentID,
			ToolDurationMS: p.ToolResponse.TotalDuration,
		},
	}
	if err := trace.Append(rec); err != nil {
		fmt.Fprintf(stderr, "governor trace agent: append: %v\n", err)
	}
	return 0
}

// payloadKeys returns the sorted top-level key names of a JSON object payload, or nil if the
// payload is not an object. Key names only — the values include the agent's prompt.
func payloadKeys(payload []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// watchdog exits the process with status 0 after d, and returns a func that disarms it.
//
// It is a goroutine and a timer rather than a context deadline because the thing being
// guarded against is a read that cannot be cancelled: os.Stdin has no deadline on a pipe, so
// an interruptible wait is not available and the only reliable escape is to leave.
//
// Disarming is not tidiness. These bodies are called in-process by their own tests, and an
// undisarmed watchdog would call os.Exit(0) three seconds into the test binary's run — which
// terminates the suite mid-flight with a success status, reporting a pass for every test that
// never got to run. Callers use `defer watchdog(d)()`.
func watchdog(d time.Duration) (disarm func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(d):
			watchdogExit()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// runTraceEnsure is the SessionStart hook body: start the sampler unless it is running.
//
// Lazy start keeps Phase 0 to one installed thing. There is no launchd job, no systemd
// unit and no login item to leave behind — the sampler exists only while Claude Code does,
// which is also the only time it has anything to measure.
func runTraceEnsure(stderr io.Writer) int {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "governor trace ensure: recovered: %v\n", r)
		}
	}()

	defer watchdog(hookDeadline)()

	held, lock, err := takeLock()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace ensure: lock: %v\n", err)
		return 0
	}
	if !held {
		// Someone else holds it: a sampler is already running. This is the common case,
		// once per session start after the first.
		return 0
	}
	// Release immediately — the child re-takes it. Holding it here would make the child
	// fail to start, and the window between is harmless: the worst case is two ensure
	// calls racing into two spawns, and the loser exits when its own lock attempt fails.
	lock.Close()

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace ensure: executable: %v\n", err)
		return 0
	}

	logPath, err := samplerLogPath()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace ensure: state dir: %v\n", err)
		return 0
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "governor trace ensure: open sampler log: %v\n", err)
		return 0
	}
	defer logFile.Close()

	cmd := exec.Command(self, "trace", "sample")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid detaches the sampler from the session's process group, so that the terminal
	// closing, or Claude Code being interrupted, does not signal it. It has its own idle
	// exit and does not need to be killed by a hangup.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "governor trace ensure: start sampler: %v\n", err)
		return 0
	}
	// Release rather than Wait: this hook must return now, and the sampler outlives it.
	_ = cmd.Process.Release()
	return 0
}

// runTraceSample is the 1 Hz sampler loop.
func runTraceSample(stderr io.Writer) int {
	held, lock, err := takeLock()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace sample: lock: %v\n", err)
		return 0
	}
	if !held {
		// Another sampler is already running. Two would double the cost and interleave two
		// series into one file, so the loser simply leaves.
		fmt.Fprintln(stderr, "governor trace sample: another sampler holds the lock, exiting")
		return 0
	}
	defer lock.Close()

	if err := lock.Truncate(0); err == nil {
		if _, err := lock.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0); err != nil {
			fmt.Fprintf(stderr, "governor trace sample: write pid: %v\n", err)
		}
	}

	w, err := trace.Open()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace sample: open log: %v\n", err)
		return 0
	}
	defer w.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sampler := signals.NewSampler()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	fmt.Fprintf(stderr, "%s governor trace sample: started, pid %d\n",
		time.Now().UTC().Format(time.RFC3339), os.Getpid())

	// primed guards the first reading. CPU utilisation and every process-tree rate are
	// differences against a previous tick, so the first sample is structurally zero rather
	// than genuinely zero. Writing it would put a row of false idle into the trace at the
	// exact moment a session is starting up, which is the least idle second there is.
	primed := false
	var idleSince time.Time

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(stderr, "%s governor trace sample: signalled, exiting\n",
				time.Now().UTC().Format(time.RFC3339))
			return 0
		case <-ticker.C:
		}

		s, err := sampler.Sample(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "governor trace sample: %v\n", err)
			continue
		}
		if !primed {
			primed = true
			continue
		}

		if s.Tree.Roots == 0 {
			if idleSince.IsZero() {
				idleSince = time.Now()
			} else if time.Since(idleSince) > samplerIdleExit {
				fmt.Fprintf(stderr, "%s governor trace sample: no claude session for %s, exiting\n",
					time.Now().UTC().Format(time.RFC3339), samplerIdleExit)
				return 0
			}
		} else {
			idleSince = time.Time{}
		}

		rec := trace.Record{
			Kind: trace.KindSample,
			Sample: &trace.Sample{
				CPU:               s.CPU,
				LoadAvg1:          s.LoadAvg1,
				RunQueue:          s.RunQueue,
				Cores:             s.Cores,
				MemAvailableBytes: s.MemAvailableBytes,
				MemTotalBytes:     s.MemTotalBytes,
				MemoryPressure:    s.MemoryPressure,
				Thermal:           string(s.Thermal),
				ClaudeRoots:       s.Tree.Roots,
				ClaudeProcs:       s.Tree.Procs,
				ClaudeCPURoots:    s.Tree.CPURoots,
				ClaudeCPU:         s.Tree.CPU,
				ClaudeRSSBytes:    s.Tree.RSSBytes,
				CostMS:            s.CostMS,
			},
		}
		if err := w.Write(rec); err != nil {
			fmt.Fprintf(stderr, "governor trace sample: write: %v\n", err)
		}
	}
}

// runTraceStatus reports whether the sampler is running and what the log holds. It is the
// one subcommand here that speaks on stdout, because it is the one a person runs by hand.
func runTraceStatus(stdout, stderr io.Writer) int {
	path, err := trace.LogPath()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace status: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "log %s\n", path)
	if fi, err := os.Stat(path); err == nil {
		fmt.Fprintf(stdout, "size %d\n", fi.Size())
		fmt.Fprintf(stdout, "modified %s\n", fi.ModTime().UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintln(stdout, "size 0")
	}

	held, lock, err := takeLock()
	if err != nil {
		fmt.Fprintf(stderr, "governor trace status: lock: %v\n", err)
		return 1
	}
	if held {
		lock.Close()
		fmt.Fprintln(stdout, "sampler stopped")
	} else {
		fmt.Fprintln(stdout, "sampler running")
	}
	return 0
}

// lockPath is the sampler's single-instance lock.
func lockPath() (string, error) {
	dir, err := trace.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sampler.lock"), nil
}

func samplerLogPath() (string, error) {
	dir, err := trace.StateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "sampler.log"), nil
}

// takeLock tries to take the sampler's exclusive lock without blocking.
//
// It reports whether the lock was acquired, and returns the open file either way — the
// caller closes it to release. flock is used rather than a pid file because the kernel drops
// it when the holder dies however it dies, so a sampler killed with SIGKILL leaves nothing
// stale behind for the next one to have to reason about.
func takeLock() (bool, *os.File, error) {
	path, err := lockPath()
	if err != nil {
		return false, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, nil, err
	}
	// Not O_TRUNC: the file carries the running sampler's pid for anyone debugging by hand,
	// and truncating on a failed acquisition would erase the holder's own record of itself.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Held by someone else. Not an error condition — it is the answer.
		return false, f, nil
	}
	return true, f, nil
}
