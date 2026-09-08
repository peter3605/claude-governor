# claude-governor — design

**Status:** design agreed, nothing built. **Owner:** Peter. **Last revised:** 2026-09-07.
**Scope:** tier one (one machine) is what gets built. Tier two is designed, seamed for, and
not built — see section 12.
**Published version:** https://claude.ai/code/artifact/58fa604b-6978-49a3-b7ba-af2e49037ddd

A machine-wide admission controller for Claude Code subagents. One daemon holds the CPU
budget for the whole laptop; every session asks it before fanning out, and gets told a
number instead of guessing one.

This document is the spec a session implements from. Where it says MUST, treat it as an
invariant and write a test for it. The hook surface it builds on is **confirmed** against
Claude Code 2.1.263 — see §10 for the conclusions and
[hook-surface.md](hook-surface.md) for the raw evidence. §10.3 lists what remains unmeasured;
a session that needs one of those MUST measure it first rather than assuming.

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
- Not cross-machine *in v1*. CPU does not aggregate across hosts, so multi-host CPU
  governance is N copies of this design. What a second tier governs is a different resource
  — tokens, spend, rate limit — and section 12 specifies it.
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
7. **Install is reversible and idempotent.** Running install twice changes nothing the
   second time; uninstall leaves no file, hook, setting or process behind. Anything the
   installer writes into the user's config is backed up first and restorable exactly.
8. **A version mismatch degrades, it never deadlocks.** Every protocol message carries a
   version. A daemon that does not understand a gate's version says so, and the gate
   treats that reply exactly like an unreachable daemon — it fails open.

---

## 4. The gate

### 4.1 Hook wiring

```jsonc
// hooks/hooks.json
{
  "hooks": {
    "PreToolUse":    [{ "matcher": "Agent|Task",
                        "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor gate",
                                    "timeout": 30 }] }],
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
has been named both across versions. On 2.1.263 the payload's `tool_name` is **`Agent`**, and
a `Task` matcher fires for it too (§10.1) — keep the regex regardless, it costs nothing and
survives the next rename, but branch on `Agent` when reading the payload.

`timeout` is set explicitly rather than inherited: the default was measured at over 150 s with
no established ceiling (§10.1), which is not a number to build a wait window on. 30 s gives the
20 s `wait_max` headroom. It bounds the *wait*, not the failure path — the 100 ms connect
deadline in §4.4 is what protects the session from a wedged daemon.

### 4.2 Decision table

| Condition | Gate does | Session sees |
|---|---|---|
| Free slot in this session's share | exit 0, silent | nothing; normal permission flow continues |
| No slot, one likely within `wait_max` | block, then grant | a brief stall, then the spawn proceeds |
| Still no slot at the deadline | `deny` + reason | a refusal it can act on (below) |
| Caller is itself a subagent | `deny`, at once | nested fan-out refused (invariant, §6.3) |
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

**Wired in GOV-07 and completed in GOV-08.** A deny is emitted only for a `reason` the gate
recognises — the set is `{capacity, depth}`. A refusal whose reason this binary cannot
interpret is treated exactly like an unreachable daemon and fails open. That is the narrowest opening in invariant 1 that still enforces anything: during
an upgrade there will be a daemon of one version and gates of another, and a gate that
honoured a policy it could not explain to the user would block spawns for a reason nobody
can read. Ungoverned spawns are acceptable in that window; a wrongly blocked one is not.

Every clause of the reason after the slot count is conditional, because a clause stating a
fact the daemon did not send is a fabrication. `cpu` is null until GOV-12 lands the sampler
and `thermal` is `unknown` on Linux by design, so the live text today reads:

```
Fleet at capacity (3/3 slots, thermal: nominal). Run this task inline yourself
rather than delegating it. Do not retry the Agent call.
```

**The wait is acknowledged, and that is what makes it safe** (GOV-08). The gate cannot
simply extend its read deadline to `wait_max` when it asks for a wait: a daemon that accepts
and never answers is indistinguishable from a busy one until it replies, so that gate would
stall *every* spawn on the machine for twenty seconds — the exact hang invariant 1 forbids,
and a silent repeal of §4.4's 120 ms ceiling. So a daemon that parks a request says so
immediately, on the socket, with an interim `{"reason":"waiting","wait_ms":N}` line (§7).
The gate reads that line inside the unchanged 100 ms allowance and only then extends. The
licence to wait comes from the daemon having answered, never from this binary having asked;
a wedged daemon never sends it and fails open exactly as before. The grant path is untouched
— one write, one read — so GOV-07's measured round trip still stands.

Two consequences worth stating because they are easy to get wrong later. The gate holds
**one reader** across both lines: a `bufio.Reader` fills in blocks, so reading the first line
can pull the second in with it, and a fresh reader for the second would block on a socket
with nothing left to send. And a second `waiting` line is refused rather than honoured, or a
daemon could extend the wait indefinitely one line at a time.

**Repeat denials are rate-limited and escalate** (GOV-08). The daemon counts a session's
refusals in a 60 s sliding window and returns the count as `denials`; the gate's text gets
firmer at 2 and names the count from 3 on, because repeating the identical sentence to a
caller that has ignored it three times is not a strategy. Inside a 5 s cool-off a session
that has just been refused is refused again *immediately* rather than parked. That protects
the queue rather than the caller: otherwise a session retrying in a loop takes a place in the
FIFO on every attempt and holds it for the whole of `wait_max`, crowding out sessions that
asked once and are waiting properly.

A parked acquire that ends in a grant is **not** counted as a denial. A session that waited
patiently must not be shouted at on its next refusal.

### 4.3 Lease lifecycle

```
PreToolUse    → acquire TENTATIVE lease   ttl 30s, keyed by session_id
SubagentStart → CONFIRM lease             bind agent_id + owning pid
SubagentStop  → release
SessionEnd    → release all for session
reaper (2s)   → drop leases past ttl, whose SESSION is gone, or whose owning pid is gone
```

> **`SubagentStart` does not fire in interactive sessions** on the version measured, so the
> `confirm` step above does not happen in ordinary use and leases are reclaimed at the
> tentative TTL while their agents still run. Measured in GOV-09; see `hook-surface.md`
> §8.1. The lifecycle below is the design and remains correct; what is missing is a confirm
> trigger that fires.

**The reaper collects three things, not two.** A lease past its TTL; a lease of any state
whose *session* is registered with a PID that is gone; and a confirmed lease whose own owner
is gone. The middle one is what makes a `kill -9` survivable mid-fan-out: `OwnerPID` is bound
at `confirm` and nowhere else, so a **tentative** lease has no owner to ask about and the
third rule cannot see it. Without the session check, a session killed between its spawns
leaves its tentative leases holding slots for the full 30 s while it is already gone. A
session that registered **no** PID is left to its leases' TTLs — the owner walk reports
unknown rather than guessing, and an unknown owner must never be treated as a dead one.

The tentative state exists because `PreToolUse` fires before the spawn and the spawn may
never happen — the user declines the permission prompt, the turn is interrupted, the model
changes its mind. A tentative lease that is not confirmed within its TTL is reclaimed.

**Correlation is per-session FIFO.** This is settled, not a fallback: `SubagentStart` carries
no `tool_use_id` and shares no per-spawn key with `PreToolUse` (§10.1). Confirm binds the
oldest unconfirmed tentative lease for that `session_id`. Measured correct 4/4 against
ground truth under a four-call burst, because `PreToolUse` and `SubagentStart` strictly
interleave — but that ordering is observed, not promised, so the reaper is what makes a
mispairing survivable rather than corrupting.

Two measured hazards the implementation MUST handle:

- `SubagentStart` also fires on **resume** (`SendMessage` to a running agent), so one agent can
  produce several. **Confirm MUST be idempotent per `agent_id`**: a confirm for an `agent_id`
  that already holds a confirmed lease is a no-op, not a second lease and not a FIFO pop.
  `SubagentStop` likewise fires more than once; release MUST tolerate it.
- A resumed subagent reaches the CPU with **no `PreToolUse(Agent)` at all**. The gate cannot
  see it and v1 does not govern it. A confirm with an empty tentative queue MUST be dropped
  quietly rather than treated as an error.

**Where the owning PID comes from** (settled in GOV-07). No lifecycle payload carries one —
§10.1 and `hook-surface.md` §5 — so `confirm` and `session start` resolve it from the hook
process's own ancestry. This is a **walk**, not `getppid()`, and the difference is
load-bearing: Claude Code may run a hook through a shell, and a shell exits the moment the
hook does. Binding a lease to it has `PIDLiveness` report the owner dead on the next reaper
tick two seconds later, so every confirmed lease on the machine is collected while its agent
is still running — the budget silently over-subscribes and the governor looks like it is
doing nothing. The walk therefore climbs until it finds a process whose **argv[0]** is
`claude` (the same rule as §5.1, and for the same measured reason), stops before pid 1, and
reports **unknown** rather than guessing if it finds nothing. An unknown owner leaves the
lease to its TTL; a wrong owner gets it reaped out from under a live agent, and those two
mistakes are not the same size.

A PID is never sent without its start time. PIDs are recycled, and a bare PID would keep a
dead agent's lease alive for as long as some unrelated process held its number.

### 4.4 Performance budget

- Connect deadline: **100 ms**, then fail open.
- Grant round-trip: **p99 < 15 ms**.
- No config file parsing, no JSON schema validation, no DNS on the hot path. Read stdin,
  open the socket, write one line, read one line, exit.

The 100 ms is spent as **two nested deadlines**, settled in GOV-05:

| | |
|---|---|
| socket deadline | 90 ms, on the dial *and* on the connection, one clock for both |
| overall budget | 100 ms, enforced around the whole hook body |

**The 100 ms is conditionally extendable, and only by the daemon** (GOV-08). Reading an
interim `waiting` line (§4.2) pushes both deadlines out to cover the wait — the socket first,
the backstop behind it by the same `ioMargin`, so the ordering below is preserved. Nothing
else moves either one, and in particular the gate *asking* for a wait does not: until a reply
has been read, every figure in the table below still applies. That is what keeps the measured
rows underneath honest after block-and-wait landed.

They are not the same number on purpose. The socket deadline is the mechanism; the overall
budget is a backstop for the failures no socket deadline can see — chiefly a read of stdin
that never returns, which happens before there is a socket to put a deadline on. Setting
both to 100 ms makes them race, and a gate that had *lost* its read deadline would still
pass its tests on the backstop, which is precisely the regression the suite exists to catch.
The backstop returns an exit code; it must never be an `os.Exit` armed in a hook body.

The deadline on the connection matters as much as the one on the dial. A daemon that accepts
and then never writes leaves a plain read blocked forever, and by that point a connect-only
deadline has already been satisfied.

Measured on the real binary, macOS, 2026-09-08 — 50 runs with no daemon and 20 against a
daemon that accepts and never answers, exit code 0 and empty stdout on all 70:

| Case | p50 | p95 | max |
|---|---|---|---|
| no daemon (connect refused) | 13.3 ms | 15.7 ms | 19.7 ms |
| daemon accepts, never answers | 105.1 ms | 112.5 ms | 113.1 ms |

The second row is the 90 ms socket deadline plus process start. Process start is ~12 ms and
is outside the gate's control, so the end-to-end ceiling a hook should be held to is 120 ms,
not 100 ms — that is the number the fail-open suite asserts.

**Grant round-trip, measured in GOV-07** against a live daemon on a real socket, macOS,
2026-09-08. The two rows answer different questions and must not be conflated: the first is
the round trip this section bounds at p99 < 15 ms, which is the part the gate controls; the
second additionally carries Go process start, which no work in this repo can reduce.

| Case | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| grant round-trip, in process | 300 | 50–80 µs | 70–100 µs | **130–170 µs** | 830 µs |
| grant end to end, real binary | 50 | 6.1 ms | 7–13 ms | see below | see below |

The round-trip is roughly two orders of magnitude inside its bound, which is the expected
shape: it is one unix write and one read against an in-memory table, and the budget exists
to catch a daemon that has gone wrong rather than to constrain the happy path.

**The first execution of a newly written binary costs ~240 ms**, and every later execution
of that same file costs ~6 ms. Measured directly: six execs of an already-run binary gave
6.4/5.8/5.1/5.4/5.2/5.0 ms, while six execs of a *byte-identical copy at a new path* gave
243.5/12.3/10.4/8.9/7.8/7.4 ms. The cost is keyed to the file rather than to its contents —
macOS validates a Mach-O's signature on first exec and caches the result per file — so it is
paid once per install or upgrade and never again.

Three consequences, none of them a defect in the gate:

- It is **outside** the 100 ms budget, which is enforced inside `main` and so does not start
  until the process is already running. The gate's own accounting is untouched.
- It means the 120 ms end-to-end ceiling above is exceeded exactly once per installed
  binary, on the first subagent spawn after an install or upgrade. That is well inside the
  30 s hook timeout in §4.1, so it is a one-off stall rather than a failure.
- §13.4 requires an upgrade to install via `rename(2)` rather than overwriting in place
  (GOV-02). A renamed-in binary is a new file, so it *will* pay this cost on its first gate
  invocation. That is the correct trade: the alternative, overwriting in place, invalidates
  the signature of a running image and the kernel SIGKILLs every later execution.

One platform fact, because it shapes how this is tested rather than how it behaves: on
darwin a unix connect to a listener whose backlog is full is **refused immediately** rather
than blocking (measured: 128 queued, then `ECONNREFUSED`). A hung *connect* therefore cannot
be produced portably from a real socket, and the connect deadline is exercised through the
`transport.Dialer` seam with a dialer that blocks. That is one of the things the seam is for.

The same fact has a consequence on the *daemon* side, confirmed against the real listener in
GOV-06: a fan-out wider than the accept backlog produces `ECONNREFUSED` at some gates even
though the daemon is healthy and listening. Those gates fail open, which is the correct
outcome and needs no handling — but it bounds what the wait queue in section 6.2 can
promise. **Block-and-wait (GOV-08) can only hold gates that got a connection**, so a burst
larger than the backlog is partly ungoverned no matter how long the queue is willing to
wait. Raising the backlog is not a fix worth reaching for; a burst that wide is already past
the point where one more agent matters.

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

**Identifying a session process: match `argv[0]`, not the executable name.** Measured in
GOV-02, and it is the kind of mistake that produces a plausible-looking trace of nothing.
Claude Code installs itself as a version-numbered file — `~/.local/share/claude/versions/2.1.263`
— and launches it with `argv[0]` set to `claude`. `ps -o comm=` therefore says `claude` while
`gopsutil`'s `Process.Name()`, which is the executable's base name, says `2.1.263`. A sampler
keyed on the executable name finds **zero** sessions on a machine running six, and fails
silently: every record still writes and every field still parses, so the trace simply reads as
a permanently idle machine.

The match is exact and case-sensitive, which excludes two neighbours that would otherwise
poison the signal — the Claude **desktop app** (`Claude`, `Claude Helper`, an Electron tree
with its own substantial CPU) and the browser native host, which runs the *same* executable
but with `argv[0]` set to the full versioned path.

**Descendants are counted, and reported separately from the roots.** A build, a test run or a
language server launched by an agent is a child of the session process, not a session process
itself, so root-only accounting measures the harness and misses the work. `signals.Tree`
carries `CPURoots` and `CPU` as two numbers rather than one, because the gap between them is
the answer Phase 0 exists to produce.

**Agent records carry the payload's top-level key names.** `agent_type` is optional (§10.1),
so a record without one is ambiguous: it may be a faithful note that the harness sent no type,
or it may be the hook silently dropping a field — a defect in the instrument rather than a
finding about the system. Real traces produced both-looking records within an hour of
installation. The key list settles it per record, offline, without re-running anything. Names
only, never values: the payload carries the agent's prompt.

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

On Apple silicon this was expected to be the signal that matters most, and it is certainly the
one most likely to be done badly. It has since been measured and demoted to a backstop — see
*Measured, and it did not move* below before you design against it. `powermetrics` gives far
more detail and requires `sudo`, which kills the install
story — it MUST NOT be a dependency. `ProcessInfo.thermalState` is public API, needs no
privileges, and returns `nominal | fair | serious | critical`. A ~20-line Swift or cgo shim
exposes it to Go.

Anything other than `nominal` takes `B` to `B_min` immediately, with hysteresis on the way
back up: require `nominal` for three consecutive ticks before the additive increase resumes,
so the budget does not oscillate against the thermal governor.

**Built (GOV-13).** The shim is cgo, inline Objective-C in `internal/signals/thermal_darwin.go`
under `//go:build darwin && cgo`, linking `Foundation` and nothing else — no subprocess, no
`sudo`, no third-party dependency. `internal/signals/thermal_other.go` carries the complement
constraint and returns `unknown`, which is what makes "degrades rather than fails to build"
structural rather than aspirational: off darwin the shim is not compiled at all, so no host
needs a C toolchain to build the governor.

`Thermal.Elevated()` is the only place the cut rule is written down. It is true for `fair`,
`serious` and `critical`, and false for `nominal`, for `unknown`, and for any state a future
macOS adds that this version cannot name. Callers ask `Elevated()`; a caller that writes
`thermal != nominal` itself reintroduces exactly the bug §13.5 warns about, and pins every
Linux budget at `B_min` forever.

**Measured, and it did not move (GOV-13).** Ten minutes of saturating 8-core FP load on the
M1 Air (8 cores / 4 P-cores, fanless), loadavg peaking at 66, and `thermalState` never left
`nominal`. Corroborated three ways: an independent Swift read of the same API returned 0, the
raw notify key `com.apple.system.thermalpressurelevel` read 0 throughout, and `pmset -g therm`
recorded no thermal or performance warning level. The shim is not the problem — it agrees
exactly with the platform. The API is simply conservative on Apple silicon.

Injecting a level to force the other states needs privileges, so it was not attempted; the
`fair` / `serious` / `critical` branches are covered by testing the raw-value mapping directly
instead (`thermalFromRaw`), which is why that mapping is split out of the cgo file.

This demotes the opening claim of this section. Thermal is a **backstop, not the controller**:
it is real, it is free to read, and when it fires the budget should floor immediately — but on
this chassis it does not fire under exactly the load this project exists to manage. CPU
utilisation and run-queue depth do the actual work in §5.2, which makes the setpoint tuning in
GOV-15 the thing that determines whether the governor is any good. Do not tune the loop
expecting thermal to intervene, and do not read a steady `nominal` as evidence the machine is
comfortable. One machine, one run — but the direction is clear enough to design against.

**The cgo constraint has a release consequence.** `CGO_ENABLED=0` on darwin builds cleanly and
is thermally blind — it reports `unknown` on a Mac, silently, and looks precisely like a cool
machine. `make cross` builds exactly that binary, because a local cross-compile cannot link
another platform's C toolchain; it is for checking that the four targets still compile, and its
darwin output MUST NOT be shipped. The released darwin binaries are the ones CI builds natively
on a macOS runner with `CGO_ENABLED=1` (GOV-19). `governor status` is the check: on a Mac it
must not say `unknown`.

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

**Built in GOV-08.** One queue for the whole daemon, not one per session, and the walk is
strict: it stops at the first waiter it cannot satisfy rather than skipping to one it can,
because skipping is how a session asking in a tight loop overtakes a session that has been
waiting. Waiters are woken on release, on session end, and on the reaper tick — the last
unconditionally, as a backstop, so a lost wake-up costs latency rather than a full `wait_max`
spent beside a free slot. The queue decides *order*; max-min fair share (§6.1) will decide
who may join it, and does not exist yet.

A waiter that times out at the same instant as the pump grants it a lease keeps the lease
rather than being refused beside it — the removal and the hand-over happen under one lock, so
a granted lease always has exactly one owner. A client that disconnects before it can even be
told it is waiting has its slot handed straight back, rather than left to a 30 s TTL.

### 6.3 Depth

Only depth-0 fan-out consumes leases: **a spawn requested by a subagent is denied outright.**
This is the simplest correct answer to the nested-deadlock failure (all slots held by agents
themselves waiting for slots), and nested fan-out is rarely what was wanted.

**Depth is read from the presence of a top-level `agent_id` in the `PreToolUse` payload**
(absent ⇒ depth 0, present ⇒ the caller is itself a subagent). There is no depth field, and
Claude Code does **not** refuse nested spawns on its own — measured `max_depth: 2`,
`refused.depth_limit: 0` (§10.1). This invariant is entirely the governor's to enforce.

**Corrected in GOV-08.** This section and the §4.2 table both used to say "depth > 1 is
denied", which was unenforceable as written and denied nothing. The `depth` field on the wire
is the *caller's* depth, fixed by the paragraph above at exactly two distinguishable values —
0 for the main session and 1 for a subagent — so it is never greater than 1 and the rule
never fired. The enforceable rule is the single test §10.1 already names: `agent_id` present
means nested, and nested is refused. The daemon therefore denies at depth ≥ 1.

The depth refusal is decided **before** the budget is consulted and is never parked. A nested
spawn is not short of a slot, it is not allowed one, and holding it for twenty seconds to
reach an answer that was already known would stall a session for nothing. Its reason text
deliberately names no slot count, because a count invites a retry once the count moves, and
for this caller it never will.

---

## 7. Wire protocol

The endpoint is a URL, not a path: `unix:///Users/you/.claude/governor/sock` in v1, and
nothing in the gate or the daemon assumes the scheme. Newline-delimited JSON, one request
per connection. Deliberately boring — this is a hot path.

```jsonc
// Every message carries "v" (protocol major) and, on requests, "principal".
// principal is single-user in v1 and always resolves locally - it exists so that a
// second tier never has to retrofit identity into an already-shipped wire format.

// -> acquire
{"v":1,"op":"acquire","principal":{"user":"peter","team":null},
 "session":"abc123","tool_use_id":"toolu_01...","depth":0,"wait_ms":20000}
// <- granted
{"v":1,"ok":true,"lease":"L-0d41","budget":6,"in_use":4,"share":2,"source":"local"}
// <- waiting (GOV-08: interim, sent immediately when the daemon parks the request;
//    exactly one grant or refusal then follows on the same connection)
{"v":1,"ok":false,"reason":"waiting","wait_ms":20000,"budget":6,"in_use":6}
// <- refused
{"v":1,"ok":false,"reason":"capacity","budget":6,"in_use":6,"cpu":0.94,
 "thermal":"fair","source":"local","denials":1}
// <- refused, nested fan-out (§6.3); never preceded by a "waiting" line
{"v":1,"ok":false,"reason":"depth","budget":6,"in_use":4,"cpu":null,
 "thermal":"nominal","source":"local"}
// <- version rejected (the gate treats this exactly like an unreachable daemon)
{"v":1,"ok":false,"reason":"unsupported_version","supported":[1]}

// -> confirm
{"v":1,"op":"confirm","session":"abc123","lease":"L-0d41","agent_id":"ag_77",
 "pid":48213,"pid_started_at":1757212800}
// -> release
{"v":1,"op":"release","session":"abc123","agent_id":"ag_77"}
// -> session
{"v":1,"op":"session","action":"start","session":"abc123","pid":48001,"weight":1}
// -> status (statusline, doctor, /governor)
{"v":1,"op":"status"}
{"v":1,"budget":6,"in_use":4,"waiting":1,"cpu":0.61,"thermal":"nominal",
 "sources":[{"name":"local","budget":6}],"sessions":[...]}
// -> advise (MCP request_agents)
{"v":1,"op":"advise","session":"abc123","wanted":5}
{"v":1,"granted":3,"wait_hint_ms":0,"reason":"..."}
```

`source` and `sources` are plural because the budget is modelled as a *set* of sources
reduced by `min()`, with exactly one entry in v1. That plural is the entire cost of the
seam in section 12.

The `waiting` line is the one message that is not the end of an exchange, and it is
**compatible in both directions** — which is what let it be added without a version bump. A
gate that cannot wait sends `wait_ms: 0`, is never parked, and never sees it; that is what
GOV-07's gate does and what any older binary still does. A new gate against an older daemon
is refused immediately for capacity and never waits. A daemon sends it only when it has
decided to park, and always follows it with exactly one grant or refusal. A client that reads
a *second* `waiting` line MUST treat the daemon as unreachable and fail open, or the wait can
be extended indefinitely one line at a time.

`denials` is how many times that session has been refused inside a 60 s window, counting the
current refusal. It is counted at the daemon because every gate is a separate process that
remembers nothing.


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
├── hooks/hooks.json              the five hooks in section 4.1
├── .mcp.json                     stdio server → bin/governor mcp
├── install.sh                    curl-able bootstrap: fetch binary, then `governor install`
├── bin/governor                  one static binary
│   ├── daemon gate confirm release session status mcp statusline
│   ├── install uninstall doctor calibrate version
│   └── trace                     Phase 0 only — removed when the gate ships
├── internal/
│   ├── transport/                endpoint URL → net.Conn. unix:// in v1, tcp:// later
│   ├── budget/                   Source interface; LocalSource in v1; reduced by min()
│   ├── lease/                    table, TTL, Liveness interface (PID+starttime in v1)
│   ├── signals/                  Sampler interface; sampler_darwin.go, sampler_linux.go
│   └── config/                   versioned schema, migration, atomic write
├── skills/fleet-budget/SKILL.md  "ask before you fan out"
├── commands/governor.md          /governor status | limit N | pause | boost
└── settings.json                 defaults + statusLine wiring

state  → ${CLAUDE_PLUGIN_DATA}/   config.json, leases.db, samples.jsonl
socket → unix:///~/.claude/governor/sock
```

`governor trace` is Phase 0 (GOV-02) and is deliberately not part of the surface above it.
It is `trace agent` (the `SubagentStart`/`SubagentStop` hook body), `trace sample` (the 1 Hz
sampler), `trace ensure` (the `SessionStart` hook that lazily starts the sampler), and its own
`trace install`/`trace uninstall` pair. It gates nothing and decides nothing; it only records
to `samples.jsonl`.

It is a separate command family rather than a temporary meaning for `gate`, `confirm` and
`release` on purpose: giving those a second definition now would leave the stories that
implement them properly (GOV-05, GOV-07) guessing which one they are replacing. `internal/trace`
and the whole `trace` family are deleted when Phase 1 lands, and `governor install` (GOV-18)
supersedes `trace install`.

The five `internal/` packages are not architecture astronautics: each is one interface with
exactly one implementation in v1, and each is a seam section 12 names. If a package needs a
second implementation before tier two exists, it was premature.

The statusline earns its place: `◐ 3/5 · cpu 61% · thermal ok` in every session turns an
invisible policy into something watchable, which is most of what makes a governor
trustworthy. Ship it in Phase 1, not as polish.

---

## 10. Hook surface — confirmed

**Confirmed against `claude --version` = `2.1.263 (Claude Code)` on macOS/arm64, 2026-09-07
(GOV-01).** Raw payloads are in [hook-surface.md](hook-surface.md); this section is the
conclusion only. Nothing below is inferred from documentation. A session that needs a
detail not stated here MUST measure it and extend both files, not guess.

### 10.1 The four answers

**1. Tool name — `Agent`, and a `Task` matcher also fires.**
`tool_name` on the wire is `Agent`. A hook registered with `matcher: "Task"` fired for it
anyway; `Task` is carried as an alias. Both spellings are live *within this one version* —
the same run reported the same tool as `Task` in its `permission_denials`. So: keep the
`Agent|Task` regex in §4.1, and match on the **payload** value `Agent` in code. Registering
only `Task` would have worked by luck; relying on the payload saying `Task` would not.

**2. `tool_input` shape — `{description, prompt, subagent_type?, run_in_background}`.**
`subagent_type` carries the agent type but is **optional** — it was absent on a nested spawn.
`run_in_background` is a bool and background subagents cost the same CPU, so they take a
lease like any other. **There is no depth field.**

**Depth comes from the top-level `agent_id`, not from `tool_input`.** A `PreToolUse` whose
payload has no top-level `agent_id` was issued by the main session (depth 0). One that has it
was issued by a subagent (depth ≥ 1), and the value names the *parent*. That single test is
all §6.3 needs. It also matters that Claude Code does **not** refuse nested spawns itself —
measured `max_depth: 2`, `spawned_by_subagents: 1`, `refused.depth_limit: 0` — so the depth
invariant is entirely the governor's to enforce.

**3. `SubagentStart` — cannot be correlated by ID, and does not fire once per spawn.**

`SubagentStart` carries `{session_id, transcript_path, cwd, prompt_id, agent_id, agent_type,
hook_event_name}`. It carries **no `tool_use_id`**, and shares no other field with
`PreToolUse` that is unique to one spawn (`prompt_id` is per turn, not per spawn). The
`tool_use_id → agentId` binding does exist, but only in the `Agent` tool's **`PostToolUse`**
`tool_response`, which for a foreground spawn arrives *after the agent has finished*.

**Therefore §4.3's per-session FIFO fallback is the design, not a contingency.** It was
validated, not merely assumed: forcing four `Agent` calls into a single assistant message and
popping the oldest unconfirmed `tool_use_id` on each `SubagentStart` reproduced the
`PostToolUse` ground-truth pairing 4/4, in both foreground and background runs. `PreToolUse`
and `SubagentStart` strictly interleave — each `SubagentStart` lands 35–60 ms after its own
`PreToolUse` and before the next one, even in a four-call burst. This ordering is *observed
and unpromised*; the reaper (invariant 2) is what keeps a mispairing survivable.

Worse than the missing ID, and the thing to design against:

- **`SubagentStart` is a start-*or*-resume signal.** One spawn produced **two**
  `SubagentStart` events with the same `agent_id` (and two `SubagentStop`s) — the second when
  `SendMessage` resumed the agent. Confirm MUST be idempotent per `agent_id`, and release
  MUST tolerate firing twice.
- **`SendMessage` resumes a subagent with no `PreToolUse(Agent)` anywhere.** That is a live
  subagent burning CPU that the gate never sees. v1 cannot govern it; the daemon should at
  least not be confused by the confirm that follows. Do not claim the budget is airtight.

Release keys off `agent_id`, which `SubagentStop` does carry — no correlation needed there.

**4. Timeout ceiling — the configured `timeout` is honoured, in seconds, precisely.**
Measured on one spawn with three competing hooks: `timeout: 120` was killed at **119.1 s**;
`timeout: 900` and an unset timeout both ran a full 150 s uncut, and Claude Code *blocked on
them the whole time* (run `duration_ms: 169493`, spawn then proceeded). A `deny` emitted after
20 s was honoured, and so was one emitted after **75 s** with `timeout: 900`.

**20 s block-and-wait is safe with room to spare.** Two riders: §4.1 MUST set `timeout`
explicitly on the gate hook rather than inherit the default — the default was not cut at 60 s
and its real ceiling was not established, so it is not a number to build on. And the ceiling
bounds only the *wait*; the 100 ms connect deadline in §4.4 is unchanged and is what actually
protects the session.

### 10.2 Consequences already folded into this document

§4.1 gains an explicit `timeout`. §4.3 states FIFO as the mechanism. §6.3 states that depth is
read from `agent_id` and that the harness enforces nothing. §7 gains no new fields — the
`tool_use_id` in `acquire` is still what the daemon keys a tentative lease on; it is simply
never echoed back by `SubagentStart`.

### 10.3 Still unmeasured

Named so they are not mistaken for settled:

- The default hook timeout's true ceiling (only that it exceeds 150 s).
- Whether the observed `PreToolUse`/`SubagentStart` interleaving holds under machine load
  heavy enough to slow the harness itself. Phase 0's traces are the place to watch for it.
- Whether `SubagentStart` fires for a subagent restored by session resume.
- Linux: every measurement above is macOS. Re-run the probe on Linux before trusting §10 there.

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

---

## 12. Scale posture

**Two tiers, and only the first gets built.**

| | Tier one — the local governor | Tier two — the shared broker |
|---|---|---|
| Governs | CPU, run queue, thermal, memory | tokens, spend, API rate limit |
| Scope | one machine | an account or a team |
| Deployment | one daemon per host | one service |
| Status | this document | designed here, not built |

The reason this is two tiers rather than one bigger one: **CPU does not aggregate.** Summing
utilisation across ten laptops tells you nothing about whether any one of them can take
another agent, so "multi-host CPU governance" is just N copies of tier one — which you
already get by installing the plugin N times. The resources that genuinely *are* shared
across machines are the ones a second tier governs, and they are different resources.

The gate asks every configured budget source and takes the **minimum**. In v1 the set has
one member. That is the whole integration.

### 12.1 Seam inventory

Each seam is one interface with exactly one implementation today. The cost of each is a
type signature, not a subsystem.

| Seam | v1 | Tier two | Lives in |
|---|---|---|---|
| Transport | `unix://` endpoint URL | `tcp://` + TLS | `internal/transport` |
| Identity | `principal{user, team:null}`, carried and unused | resolved against an account | protocol field, present from v1 |
| Budget source | one `LocalSource` | `min(local, remote)` over a slice | `internal/budget` |
| Liveness | PID + process start time | heartbeat | `internal/lease.Liveness` |
| Signals | `Sampler`, darwin + linux impls | unchanged | `internal/signals` |
| Config | versioned schema, migration on read | new keys migrate forward | `internal/config` |

### 12.2 Deliberately not built

Durable or replicated lease state, service discovery, authentication, consensus. Each is
justified only by tier two, and each is expensive. In-memory leases plus a reaper are
*correct* for one machine: a restart loses everything and the reaper cleans up in seconds.

The rule for a future session: **a change that only makes sense for tier two goes behind an
interface with one implementation. If it needs a second implementation to compile, it is
premature.** GOV-26 exists to prove the seams hold, by pointing a real gate at a stub remote
budget source — once, as a test, not as a shipped feature.

---

## 13. Install, onboarding and upgrade

Packagability is a v1 requirement, not a release chore. A stranger must be able to install
this in one command and — more importantly — remove it in one command, because what is
being asked of them is to run a hook on every subagent spawn.

### 13.1 Principles

- **One command in, one command out, and uninstall leaves nothing.** No stray hook, no
  edited setting, no running daemon, no directory.
- **Idempotent.** Running install twice is a no-op the second time, never a duplicate hook.
- **Never blind-write the user's config.** Read, back up, merge, write atomically, and keep
  the backup so uninstall can restore byte-for-byte.
- **`doctor` diagnoses, it does not describe.** It is the first thing a confused user runs,
  and every check it fails must name the fix.
- **Defaults come from the machine.** `calibrate` measures this host and writes real numbers;
  no constant compiled in from Peter's laptop ships as anyone else's default.

### 13.2 Command surface

| Command | Does |
|---|---|
| `install.sh` | curl-able bootstrap: detect OS/arch, fetch the matching binary, hand off to `governor install` |
| `governor install` | write plugin files, merge hooks and statusline into settings (after backup), start the daemon, print exactly what it touched |
| `governor uninstall` | reverse all of it, restore the settings backup, stop the daemon, remove state on `--purge` |
| `governor doctor` | daemon reachable? hooks registered? matcher matches the real tool name? binary and plugin versions agree? config schema current? each failure names its fix |
| `governor calibrate` | measure cores, performance cores, RAM and a short load probe; write setpoint and B_max for *this* machine |
| `governor version` | binary version, protocol version, config schema version |

### 13.3 What touches the user's machine

Uninstall is only verifiable if this list is exhaustive, so it is normative.

| Path | Written by | Removed by |
|---|---|---|
| `~/.local/share/claude-governor/bin/governor` | install.sh | uninstall |
| `~/.claude/plugins/claude-governor/` | install | uninstall |
| `~/.claude/settings.json` (hooks, statusLine keys) | install, merged | uninstall, restored from backup |
| `~/.claude/settings.json.governor-backup-<ts>` | install | uninstall |
| `~/.claude/governor/sock` | daemon | uninstall |
| `${CLAUDE_PLUGIN_DATA}/` (config, leases, samples) | daemon | uninstall `--purge` only |

Phase 0 (GOV-02) adds the rows below. They are listed here rather than in a story note
because this table is what makes uninstall verifiable, and a path that is touched but not
listed is a path uninstall silently stops covering. All of them go when Phase 1 replaces the
`trace` family.

| Path | Written by | Removed by |
|---|---|---|
| `~/.claude/settings.json` (three `trace` hooks) | `trace install`, merged after backup | `trace uninstall`, surgically |
| `~/.claude/settings.json.governor-trace-backup-<ts>` | `trace install` | `trace uninstall` |
| `~/.claude/governor/samples.jsonl` | `trace agent`, `trace sample` | by hand — it is the measurement, and deleting it on uninstall would throw away the phase |
| `~/.claude/governor/sampler.lock` | `trace sample` | by hand |
| `~/.claude/governor/sampler.log` | `trace sample` | by hand |
| `~/.local/share/claude-governor/bin/governor` | staged by hand in Phase 0 | by hand, until `install.sh` owns it |

`trace uninstall` removes our hook entries **surgically** rather than restoring the backup
wholesale, because a wholesale restore would destroy anything the user added to
`settings.json` in the meantime. It then compares the result against the backup and, when
nothing else changed, writes the backup's original bytes — so the ordinary case is
byte-identical, formatting and key order included, not merely equivalent. There is a test for
exactly that.

One consequence worth stating: while the hooks are installed, `settings.json` carries Go's
map key ordering rather than the user's. No value is altered, and uninstall restores the
original bytes.

### 13.4 Config and upgrade

Config is `${CLAUDE_PLUGIN_DATA}/config.json` carrying `"schema": 1`. Unknown keys warn and
are preserved, never crash — a config written by a newer version must not brick an older
binary. Migration happens on read; the write is atomic (temp file, rename).

**Never overwrite the binary in place.** Measured in GOV-02, on macOS: `cp` onto the existing
`bin/governor` invalidates its code signature, and the kernel then **SIGKILLs** every
subsequent execution — `exit=137`, no output, no error message anywhere. That defeats the one
guarantee the hooks make, because a process killed by a signal cannot exit 0 no matter how
carefully it is written; the failure is external to the program. During an in-place upgrade
this would land on every hook of every live session at once. Upgrade MUST write the new
binary to a temporary name in the same directory and `rename(2)` it over the old one, exactly
as the config write does — a rename swaps the directory entry and leaves running processes on
the old inode, so nothing in flight is signed wrong.

Every protocol message carries `"v"`. A daemon that does not support a gate's version replies
`unsupported_version`, and the gate treats that reply exactly like an unreachable daemon: it
fails open. This is what makes upgrade safe — during an upgrade there will be a live daemon
of one version and gates of another, and the only acceptable outcome is ungoverned spawns,
never a stalled session.

### 13.5 Platforms

macOS and Linux, arm64 and amd64. Linux is not padding: it is where a tier-two broker would
run, and where cgroup limits are a better signal than raw CPU sampling. Thermal state has no
Linux equivalent and reports `unknown`, which section 5.3 already requires be treated as
nominal. Windows is out of scope — named pipes and different process introspection roughly
double the surface for no current user.
