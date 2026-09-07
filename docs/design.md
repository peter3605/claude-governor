# claude-governor — design

**Status:** design agreed, nothing built. **Owner:** Peter. **Last revised:** 2026-09-07.
**Published version:** https://claude.ai/code/artifact/58fa604b-6978-49a3-b7ba-af2e49037ddd

A machine-wide admission controller for Claude Code subagents. One daemon holds the CPU
budget for the whole laptop; every session asks it before fanning out, and gets told a
number instead of guessing one.

This document is the spec a session implements from. Where it says MUST, treat it as an
invariant and write a test for it. Where it says "verify", the fact is not yet confirmed
against this machine's Claude Code version — see [Unknowns](#unknowns) — and a session that
needs it MUST confirm it first rather than assuming.

---

## 1. Problem

Several Claude Code sessions running at once on a fanless MacBook Air saturate the machine,
and every session degrades together — including the one being watched.

The obvious fix is a fixed cap, `slots = cores - 2`. Claude Code's own Workflow tool already
does exactly this internally (`min(16, cores - 2)`). It is wrong here in both directions:

- **Load is bimodal.** A subagent blocked on the API costs almost nothing. A subagent that
  has just started `npm test` costs a whole core plus disk. A constant is simultaneously too
  stingy when six agents are thinking and far too generous when three are compiling.
- **The chassis is fanless.** Sustained multicore load thermally throttles within roughly
  two to four minutes. A cap tuned on a cool machine is the wrong cap on a warm one.

**Therefore:** the allowed count is the output of a feedback controller with a setpoint, not
a configured constant. The enforcement layer reads whatever the controller currently says.

### Non-goals

- Not a scheduler. It never reorders, queues, or launches work itself.
- Not a permission system. It has an opinion about *how many*, never about *whether it is safe*.
- Not cross-machine. Scope is one host. (Team scope is the possible product; not v1.)
- Never kills a running subagent. See §5.

---

## 2. Architecture

```
  sessions                hook gate                 broker (governord)
  ┌──────────┐         ┌───────────────┐         ┌────────────────────────┐
  │ session A│────────▶│ governor gate │────────▶│  lease table           │
  │ session B│────────▶│               │◀────────│  control loop (AIMD)   │
  │ session C│────────▶│ fails OPEN    │  grant  │  fair-share allocator  │
  └──────────┘         └───────────────┘         └────────────────────────┘
                                                            ▲
                                              ┌─────────────┴─────────────┐
                                              │ sampler, 1 Hz             │
                                              │ cpu · runqueue · thermal  │
                                              │ memory · per-agent cost   │
                                              └───────────────────────────┘
```

Four components, one binary.

| Component | Runs as | Lifetime |
|---|---|---|
| `governord` | `governor daemon` | long-lived, one per machine, lazily started by the gate |
| gate | `governor gate` | one process per subagent spawn, milliseconds |
| release | `governor release` | one process per subagent stop |
| MCP server | `governor mcp` | stdio child of each Claude Code session |

**Language: Go.** The gate runs on *every* subagent spawn inside a hook Claude Code is
blocked on. A Node or Python entry point pays 30–50 ms of interpreter startup per call; a
static Go binary pays about two. Go also cross-compiles to one file with no runtime to
install, which is the whole distribution story. `gopsutil` covers process and CPU
introspection. The single native piece is a small cgo/Swift shim for
`ProcessInfo.thermalState` (§5.3).

---

## 3. Invariants

These are the properties that make the thing usable rather than a liability. Each one gets
a test.

1. **The gate fails open.** If the daemon is unreachable, wedged, or slower than the connect
   deadline, the gate exits 0 and the spawn proceeds ungoverned. A broken governor MUST
   never brick a session. Build and test this before anything else.
2. **Leases expire on their own.** Every lease carries a TTL and an owning PID, and a reaper
   drops both. Assume the stop hooks never fire; leaks are the default outcome, not the edge
   case.
3. **The grant path is silent.** On a grant the gate exits 0 with empty stdout. It MUST NOT
   emit `permissionDecision: "allow"` — that suppresses the user's own permission prompts as
   a side effect, trading a security regression for nothing.
4. **The budget never reaches zero.** `B_min = 1`. A machine under thermal duress still makes
   progress, one agent at a time.
5. **Shrinking the budget never kills a running agent.** Cutting an agent mid-task discards
   everything it has already spent. Starve new grants; let running ones finish.
6. **Every live session is guaranteed one slot** before any session gets a second.

---

## 4. The gate

### 4.1 Hook wiring

```jsonc
// hooks/hooks.json
{
  "hooks": {
    "PreToolUse":    [{ "matcher": "Agent|Task",
                        "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor gate" }] }],
    "SubagentStart": [{ "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor confirm" }] }],
    "SubagentStop":  [{ "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor release" }] }],
    "SessionStart":  [{ "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor session start" }] }],
    "SessionEnd":    [{ "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor session end" }] }]
  }
}
```

The matcher is a regex covering both `Agent` and `Task` because the subagent-spawning tool
has been named both across versions. Confirm which one this machine uses (§10) but keep the
regex regardless — it costs nothing and survives the next rename.

### 4.2 Decision table

| Condition | Gate does | Session sees |
|---|---|---|
| Free slot in this session's share | exit 0, silent | nothing; normal permission flow continues |
| No slot, one likely within `wait_max` | block, then grant | a brief stall, then the spawn proceeds |
| Still no slot at the deadline | `deny` + reason | a refusal it can act on (below) |
| Spawn depth > 1 | `deny` | nested fan-out refused (invariant, §6.3) |
| Daemon unreachable | exit 0, silent | nothing |

Block-and-wait is preferred over deny wherever the wait fits inside the hook timeout,
because a stall costs no tokens and the model never has to reason about it. Deny is the
fallback, and its reason text matters:

```
Fleet at capacity (6/6 slots, CPU 94%, thermal: fair). Run this task inline
yourself rather than delegating it. Do not retry the Agent call.
```

Name the number, name the cause, and give one concrete instruction. A vague refusal produces
a retry loop.

### 4.3 Lease lifecycle

```
PreToolUse    → acquire TENTATIVE lease   ttl 30s, keyed by session_id
SubagentStart → CONFIRM lease             bind agent_id + owning pid
SubagentStop  → release
SessionEnd    → release all for session
reaper (2s)   → drop leases past ttl, or whose owning pid is gone
```

The tentative state exists because `PreToolUse` fires before the spawn and the spawn may
never happen — the user declines the permission prompt, the turn is interrupted, the model
changes its mind. A tentative lease that is not confirmed within its TTL is reclaimed.

Correlating the confirm to the right tentative lease needs a key that appears in both hook
payloads. `tool_use_id` (PreToolUse) and `agent_id` (SubagentStart) are the candidates; if
they cannot be correlated (§10), fall back to per-session FIFO — confirm the oldest tentative
lease for that `session_id`, which is correct as long as spawns within a session are ordered.

### 4.4 Performance budget

- Connect deadline: **100 ms**, then fail open.
- Grant round-trip: **p99 < 15 ms**.
- No config file parsing, no JSON schema validation, no DNS on the hot path. Read stdin,
  open the socket, write one line, read one line, exit.

---

## 5. The control loop

### 5.1 Signals, sampled at 1 Hz

| Signal | Source (macOS, no privileges) | Use |
|---|---|---|
| CPU utilisation | `gopsutil/cpu`, 5 s EWMA | primary setpoint input |
| Run-queue depth | `loadavg1 / ncpu` | catches contention CPU% misses |
| Thermal pressure | `ProcessInfo.thermalState` | hard cut trigger |
| Memory pressure | free pages + swap-in rate | secondary cut trigger |
| Per-agent cost | CPU across the `claude` process tree ÷ confirmed leases | Phase 2 refinement |

### 5.2 AIMD

Borrowed from TCP congestion control, and the right shape for the same reason: reclaim
headroom cautiously, give it back immediately.

```
# every 2s, after sampling
headroom = setpoint - util_ewma            # setpoint default 0.75
queue_ok = loadavg1 / ncpu < 1.2

if thermal != nominal:                B = B_min                # hard floor
elif util_ewma > setpoint + 0.05 or not queue_ok:
                                      B = max(B_min, B // 2)   # multiplicative decrease
elif headroom > 0.10 and queue_ok and demand_waiting:
                                      B = min(B_max, B + 1)    # additive increase

B_min = 1
B_max = min(2 * performance_cores, 12)
```

`demand_waiting` gates the increase: do not climb the budget when nobody is asking for a
slot, or the first fan-out after an idle period arrives with an unearned budget.

Tune `setpoint` against the Phase 0 traces, not by feel.

### 5.3 Thermal

On Apple silicon this is the signal that matters most and the one most likely to be done
badly. `powermetrics` gives far more detail and requires `sudo`, which kills the install
story — it MUST NOT be a dependency. `ProcessInfo.thermalState` is public API, needs no
privileges, and returns `nominal | fair | serious | critical`. A ~20-line Swift or cgo shim
exposes it to Go.

Anything other than `nominal` takes `B` to `B_min` immediately, with hysteresis on the way
back up: require `nominal` for three consecutive ticks before the additive increase resumes,
so the budget does not oscillate against the thermal governor.

### 5.4 Attributed cost (Phase 2 refinement, not v1)

Raw host CPU% cannot tell a build from Chrome. Sum CPU across the `claude` process tree and
its children, divide by the count of confirmed leases, and you have a live estimate of the
marginal cost of one more agent — so `B = headroom / cost_per_agent` predicts instead of
reacting. Build this only after Phase 0 shows the per-agent cost is real and measurable.

---

## 6. Allocation

### 6.1 Fair share

Sessions register on `SessionStart` with a weight, default 1. Allocation is max-min fair:
every live session is guaranteed one slot before any gets a second, and unclaimed share is
redistributed to sessions actually asking.

### 6.2 Wait queue

FIFO across sessions. The next freed slot goes to whoever has waited longest, so a noisy
session cannot starve a quiet one.

### 6.3 Depth

Only depth-0 fan-out consumes leases. Depth > 1 is denied outright. This is the simplest
correct answer to the nested-deadlock failure (all slots held by agents themselves waiting
for slots), and nested fan-out is rarely what was wanted.

---

## 7. Wire protocol

Unix domain socket at `~/.claude/governor/sock`, newline-delimited JSON, one request per
connection. Deliberately boring — this is a hot path.

```jsonc
// → acquire
{"op":"acquire","session":"abc123","tool_use_id":"toolu_01...","depth":0,"wait_ms":20000}
// ← granted
{"ok":true,"lease":"L-0d41","budget":6,"in_use":4,"share":2}
// ← refused
{"ok":false,"reason":"capacity","budget":6,"in_use":6,"cpu":0.94,"thermal":"fair"}

// → confirm
{"op":"confirm","session":"abc123","lease":"L-0d41","agent_id":"ag_77","pid":48213}
// → release
{"op":"release","session":"abc123","agent_id":"ag_77"}
// → session
{"op":"session","action":"start","session":"abc123","pid":48001,"weight":1}
// → status  (for statusline and /governor)
{"op":"status"}
{"budget":6,"in_use":4,"waiting":1,"cpu":0.61,"thermal":"nominal","sessions":[…]}
// → advise  (MCP request_agents)
{"op":"advise","session":"abc123","wanted":5}
{"granted":3,"wait_hint_ms":0,"reason":"…"}
```

The daemon MUST tolerate a malformed line by closing that connection only. One bad client
never takes down the broker.

---

## 8. Advisory channel

The enforcement hook alone produces a bad experience: the model launches eight agents, gets
five refusals, and burns tokens learning what it could have been told. So the plugin also
ships a stdio MCP server with one tool:

```
governor__request_agents(wanted: int, why: string)
  → { granted: int, wait_hint_ms: int, reason: string }
```

plus a `SessionStart` `additionalContext` line and a bundled skill telling the session to
call it before any fan-out.

**Advice steers; the hook enforces.** The advisory channel exists to make the common case
pleasant, not to be trusted — a model that *can* ask will sometimes forget, so the budget is
enforced underneath it regardless.

Success metric: denials per hour, before and after Phase 3. If advice is landing, denials
approach zero without the budget changing.

---

## 9. Package

```
claude-governor/
├── .claude-plugin/plugin.json    name, version, userConfig (setpoint, B_max, wait_max)
├── hooks/hooks.json              the five hooks in §4.1
├── .mcp.json                     stdio server → bin/governor mcp
├── bin/governor                  one static binary
│                                 daemon · gate · confirm · release · session · status · mcp · statusline
├── skills/fleet-budget/SKILL.md  "ask before you fan out"
├── commands/governor.md          /governor status | limit N | pause | boost
└── settings.json                 defaults + statusLine wiring

state  → ${CLAUDE_PLUGIN_DATA}/   config.json, leases.db, samples.jsonl
socket → ~/.claude/governor/sock
```

The statusline earns its place: `◐ 3/5 · cpu 61% · thermal ok` in every session turns an
invisible policy into something watchable, which is most of what makes a governor
trustworthy. Ship it in Phase 1, not as polish.

---

## 10. Unknowns — verify before Phase 1

The hook surface has moved across Claude Code versions. Each of these is a five-minute check
and getting one wrong costs a day. Register a throwaway `PreToolUse` hook that dumps stdin to
a file, spawn one subagent, and read the result.

1. **Tool name.** Is the subagent-spawning tool matched as `Task` or `Agent` on this version?
2. **`tool_input` shape.** Not publicly documented. Which fields carry agent type and spawn depth?
3. **`SubagentStart` reliability.** Does it fire for every granted spawn, and can `agent_id`
   be correlated back to `tool_use_id`? If not, §4.3's FIFO fallback is the design.
4. **Hook timeout ceiling.** Confirm it before committing to a 20 s block-and-wait. A shorter
   ceiling shrinks the wait window and pushes load onto the deny path.

---

## 11. Prior art

None of this is novel, which is the good news.

- **GNU Make's POSIX jobserver** (1994) is this exact design — a token pipe shared across
  cooperating processes, with the same leak problem and the same fail-open answer. The
  README line is "a jobserver for Claude Code subagents".
- **Ninja `-l`** limits by load average. **Bazel `--local_cpu_resources`** budgets by core.
- **Kubernetes admission controllers** are the same shape at cluster scale.
- **TCP congestion control** is where AIMD comes from.

Read the jobserver before writing the lease table.
