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
    "PreToolUse":    [{ "matcher": "Agent|Task|SendMessage|Bash",
                        "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor gate",
                                    "timeout": 30 }] }],
    "PostToolUse":   [{ "matcher": "Bash",
                        "hooks": [{ "type": "command",
                                    "command": "${CLAUDE_PLUGIN_ROOT}/bin/governor finish" }] }],
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

The matcher covers `Agent` and `Task` because the subagent-spawning tool has been named both
across versions. On 2.1.263 the payload's `tool_name` is **`Agent`**, and a `Task` matcher
fires for it too (§10.1) — keep the regex regardless, it costs nothing and survives the next
rename, but branch on the payload when reading it.

`SendMessage` was added by GOV-28 and is a **second spawn path**, not a second spelling.
Resuming an agent through it puts that subagent back on the CPU with no `PreToolUse(Agent)`
anywhere, so before this the resumed agent ran holding no lease
([hook-surface.md](hook-surface.md) §8.3). A matcher is not enough on its own here: the same
tool addresses teammates and other sessions by name, so the gate leases only when
`tool_input.to` has the shape of an agent id — seventeen lowercase hex characters — and
treats every other recipient as an ordinary message. A resume addressed by *name* is
therefore not governed; that is a stated limit, and it fails open.

`Bash` was added by GOV-15 and is the **second gated tool**, not a third spawn path. It is
matched by name and then gated by *command*: `Bash` fires 184-2,221 times a day against the
spawn path's 0-37, so `internal/classify` decides gate-side, before any I/O, whether the
command is build-shaped (§5.5). Four calls in five are answered and dropped without a socket
ever being opened — which is why the predicate has to be a pure function and not a question
for the daemon.

**`PostToolUse` is the sixth hook, and it exists only to return build leases.** Its matcher is
`Bash` alone, deliberately: giving it the gate's matcher would run a hook on every subagent
spawn to discover there was nothing to return. It takes **no** `timeout`, because it is the
gate that can wait and this one never parks. What it can and cannot be trusted to do is
measured in [hook-surface.md](hook-surface.md) §19 and folded into §4.3 and §5.6 — the short
version is that it is absent when a command fails and arrives early when a command is
backgrounded, so the reaper does more of this job than it does anywhere else.

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

There are two, because the two gated tools give the governor completely different evidence.

**A subagent lease** — three events, and the middle one is a correlation problem:

```
PreToolUse    → acquire TENTATIVE lease   ttl 30s, keyed by session_id
SubagentStart → CONFIRM lease             bind agent_id + owning pid
SubagentStop  → release
SessionEnd    → release all for session
reaper (2s)   → drop leases past ttl, whose SESSION is gone, or whose owning pid is gone
```

**A `Bash` lease** — two events, no correlation problem, and a release signal that cannot be
trusted (§5.6, `hook-surface.md` §19):

```
PreToolUse    → acquire CONFIRMED lease   keyed by tool_use_id, ttl 300s
PostToolUse   → release, IFF tool_response.backgroundTaskId is ABSENT
SessionEnd    → release all for session
reaper (2s)   → drop leases past ttl, or whose SESSION is gone
```

Three differences from the agent path, all of them measured rather than chosen:

- **There is no tentative state, because there is nothing to confirm against.** No second
  event announces that the command started. The lease is born confirmed.
- **There is no FIFO correlation.** `PostToolUse` carries the *same* `tool_use_id` as its
  `PreToolUse` — measured in every pairing observed — so the release is keyed exactly. This is
  the one respect in which `Bash` is easier than `Agent`, whose `SubagentStart` carries no
  `tool_use_id` at all.
- **The `backgroundTaskId` guard is not defensive coding.** A `run_in_background` command and
  a command that exceeded its `timeout` both raise `PostToolUse` *while still running* — the
  first within ~50 ms of launch, the second at the timeout, after which the command is
  backgrounded rather than killed. Releasing on those would hand back a slot to a build that
  is still burning CPU, which is the precise failure this governor exists to prevent.

> **This lifecycle was briefly believed to be broken in interactive use, and is not.** GOV-09
> read a trace showing 62 `SubagentStop` records against zero `SubagentStart` across 25
> interactive sessions and concluded that `confirm` never runs in ordinary use. GOV-27
> re-measured: those sessions had made three `Agent` tool calls between them, so there was
> almost nothing to confirm. Driven directly — six `Agent` spawns from an ordinary
> interactive session — `SubagentStart` fired for all six, and a live three-agent fan-out
> held exactly three slots with zero reaps. See `hook-surface.md` §8.1, which carries the
> correction and the run.
>
> The gap that measurement *appeared* to find — 63 of 70 `SubagentStop` records with no
> `PreToolUse` the gate could see, blamed on `Skill` — was not a gap. GOV-28 measured it:
> those stops are not subagents at all, and a `Skill` spawns nothing. See §10.1a and
> `hook-surface.md` §8.2. The one real omission it turned up, the `SendMessage` resume, is
> gated as of §4.1.

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

**Re-derived for `Bash`, GOV-15, and the rate that matters is not the one the story asked
for.** GOV-15 clause 4 said to re-derive against a gate firing ~220× more often. GOV-14
corrected that to ~11×, because the classifier removes 81.6% of `Bash` calls before any I/O.
Both numbers are about **daemon round trips**, and re-deriving against either alone would have
missed the thing that actually changed. There are now two rates, not one:

| | per day, this machine | what it costs |
|---|---|---|
| gate **process** runs | 184–2,221 (every `Bash`) + 0–37 spawns | ~6 ms of process start, unavoidable |
| `finish` **process** runs | 184–2,221 (every `Bash`) | ~5 ms of process start, unavoidable |
| **daemon round trips** | 34–409 (the 18.4% that are build-shaped) | ~0.6 ms on top |

The matcher fires on the tool name, so a process starts for *every* `Bash` call whatever the
classifier then decides. Only the socket is filtered. That is the 11× figure's blind spot and
it is the dominant cost by two orders of magnitude.

**Measured on the real binary, macOS/arm64, 2026-09-14**, warm (§4.4's first-exec cost paid),
`GOVERNOR_ENDPOINT` pointed at a live daemon and at an absent one:

| Case | n | p50 | p95 | p99 |
|---|---|---|---|---|
| `gate`, cheap `Bash`, **no daemon contact** | 80 | 5.96 ms | 6.97 ms | 7.30 ms |
| `gate`, build-shaped `Bash`, live daemon | 80 | 6.57 ms | 9.37 ms | 10.92 ms |
| `finish`, live daemon | 80 | 6.72 ms | 8.46 ms | 9.78 ms |
| `gate`, no daemon at all (fails open) | 60 | 5.74 ms | 6.94 ms | 7.20 ms |

Four conclusions, and only one of them is comfortable.

1. **The round trip is not the cost. Starting a process is.** A cheap command, where the
   governor parses one JSON payload and runs a ~5 µs classifier, costs 5.96 ms — within noise
   of doing nothing at all. Contacting the daemon adds ~0.6 ms on top. Every latency argument
   about this hook is really an argument about `fork`+`exec`.
2. **The p99 < 15 ms grant round-trip bound still holds, comfortably**, and is unchanged.
   GOV-07 measured the in-process round trip at 130–170 µs p99 and nothing here moves it: the
   daemon does the same work for a build lease as for a spawn.
3. **The 100 ms connect deadline is not the continuous tax the traps field feared**, because
   the commands that pay it are the 18.4% that are about to run a compiler. A `cat` never
   opens a socket, so it cannot wait on one. `TestACheapCommandNeverContactsTheDaemon`
   asserts exactly that, by failing on any dial rather than on any duration — a latency
   assertion would pass on an implementation that asked the daemon about everything and was
   merely fast about it.
4. **The honest cost is ~11 ms of added latency on every `Bash` call**, gate plus finish, of
   which the governor's own work is a rounding error. Over 2,221 calls that is ~24 s of CPU a
   day, which is nothing in aggregate and perceptible on a single fast command. It is the
   price of governing `Bash` at all with a hook-per-call harness, and it cannot be optimised
   away from inside this binary — only by not registering the hook.

**A refused build is a different event from a refused spawn**, and GOV-15 gave it its own
text. Refusing a spawn means "do not delegate, do the work here": the work still happens.
Refusing a `Bash` means the user's command **did not run**, and there is no inline alternative
— nothing else on the machine can compile that package. So the build refusal says the command
did not run, tells the model to run the *same* command again shortly, and explicitly does not
tell it to find another way. Nor does it escalate into "stop retrying" the way the spawn text
does: a retry is the correct response to a full build queue, and the gate parks a build for up
to `wait_max` before it ever refuses one, so a refusal already means the machine stayed full
for twenty seconds.

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

### 4.5 Lazy daemon start

Section 2 has always called the daemon "lazily started by the gate". GOV-11 made that true:
before it, only `governor install` started one, so a daemon that went away — a reboot, a
crash, a `pkill` — left the machine ungoverned until a human noticed the status line. That is
the project's own worst failure mode, because nothing fails: the hooks still pass, sessions
still run, and the budget has simply stopped applying.

A gate that fails to **dial** starts one. The rules are all narrowing:

| | |
|---|---|
| Trigger | the dial itself failed (`errNoDaemon`), never a daemon that answered badly |
| Ordering | the gate has already failed open; the start is for the **next** spawn |
| Waiting | none. Fire, detach, exit — nothing on the hot path waits for a bind |
| Precondition | the state directory already exists, or the governor is not installed |
| Rate limit | `daemon.start`'s mtime, 5 s, recorded **before** the fork, not after |
| Identity | `os.Executable()`'s basename must be `governor`, or it refuses |

Three of those are worth the words:

- **A daemon that accepted the connection is a daemon.** Answering late, answering rubbish,
  or answering in an unknown protocol version all fail open, and all mean there is already a
  process on that socket. Forking at it would lose the bind race and would do so on every
  spawn for as long as the first one stays wedged.
- **The rate limit records the attempt, not the success.** A daemon that cannot start fails
  the fork every time, so a marker written only on success would bound nothing — which is the
  one case the marker exists for.
- **The race between two gates is not defended against, on purpose.** A fan-out against a
  dead daemon puts several gates here at once and each forks. `transport.Listen` already
  probes an existing socket before unlinking it and refuses to bind when something answers,
  so the losers exit into the daemon log rather than stealing the socket. A lock file would
  be a second mechanism that has to agree with the one already doing the work.

The identity check is a safety property rather than a test convenience, though it is both.
The gate spawns *itself* under another subcommand, so whatever it spawns has to be the
governor; executing a program this binary cannot identify is strictly worse than leaving the
machine ungoverned. It also makes the package's own tests safe by construction — under
`go test`, `os.Executable()` is the test binary, and running that with `daemon` would run the
suite again, recursively. That is the same shape as the trap GOV-21 hit when `doctor` ran
`<hooked binary> version` under the test runner.

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

**The daemon samples the cheap signals every second and the process tree every fifth.**
GOV-12. The walk is essentially the entire cost of a tick, so paying it at 1 Hz put the
daemon over the 1%-of-a-core bar that story sets. Nothing the v1 loop reads is affected:
per-agent tree cost is the "Phase 2 refinement" row of the table above, and CPU, run queue,
thermal and memory — the signals §5.2 acts on — still tick every second. Samples carry
`TreeFresh` so a consumer cannot mistake five carried-over copies for five readings. Phase 0's
own sampler is unchanged and still walks every tick, because GOV-15 tunes the setpoint against
traces collected at that cadence.

**Measured sampler cost.** `GOVERNOR_MEASURE=1 go test ./internal/signals/ -run TestMeasureSamplerCost -v`,
which reports CPU time from `getrusage` rather than wall clock — a percentage of a core is a
claim about CPU consumed, and a sampler blocked on a syscall consumes none.

| Host | Every tick | Tree every 5th tick |
|---|---|---|
| darwin/arm64, 8 cores, ~700 processes | 8.4–17.7 ms/tick — 0.84–1.77% | 1.5–3.4 ms/tick — **0.15–0.34%** |
| linux/arm64 container, `--cpus=2`, ~5 processes | 1.6–1.8 ms/tick — 0.16–0.18% | 0.8–1.1 ms/tick — **0.08–0.11%** |

Ranges, not points, because two runs minutes apart on the same laptop differed by a factor of
two — the walk's cost tracks how many processes are on the machine and how contended it is.
A single figure here would be a snapshot presented as a property.

The two rows are not comparable to each other and should not be read as "Linux is ten times
cheaper": the walk scales with the size of the process table, and a container running one test
binary has almost nothing in it. The darwin row is the load-bearing one, and even its upper
end understates a working machine — the Phase 0 sampler, measured over 46 hours of ordinary
use at full rate, cost **2.30% of one core** (23.0 ms of CPU per tick). Scaling that by the
same ratio puts the daemon's split cadence near 0.5%, still inside the bar.

Note what the full-rate column says: at 1 Hz the walk is at or over the 1% bar on a real
machine. The split cadence is not an optimisation, it is the reason the bar is met.

This is checked by an opt-in measurement and not by a CI assertion, and the variance above is
exactly why: GOV-B04, GOV-B08 and GOV-B10 were each filed to repair a timing guard that
measured the runner instead of the code.

**On Linux the cgroup quota is preferred to the host's core count.** Inside a container, raw
host CPU is not merely imprecise but misleading in the direction that breaks the loop: a quota
of two cores on a 64-core host reads as 97% idle, so the loop would keep increasing the budget
against a container already throttled flat. The run-queue divisor has the same problem —
loadavg is the host's, and the cores to divide it by are the cgroup's. `Sample.CPUCapacity`
carries the effective figure and `CPUCapacitySource` says where it came from, so a two-core
machine and a 64-core machine throttled to two are distinguishable in a trace. Verified
against a real cgroup rather than a fixture: in a `--cpus=2` container the sampler reports
2 cores from `cgroup` while `runtime.NumCPU()` still reports 8. A quota larger than the
machine is clamped to the host, and an unrecognised cgroup layout falls back to the host
figure — the same answer a non-cgroup machine gives.

### 5.2 AIMD

> **`B` is no longer a count of agents.** Phase 0 measured agent cost to be small and
> near-constant while build cost is zero almost always and occasionally enormous, so the
> quantity this loop rations moved to build and test concurrency (docs/phase-0-findings.md §7,
> and §5.5 below for what now takes a lease). **The mechanism in this section is unchanged and
> is inherited wholesale** — additive increase, multiplicative decrease, the thermal floor and
> the `demand_waiting` guard all survive the pivot untouched; only the variable moved. Re-aiming
> the loop and re-deriving its constants is GOV-15's work, and the constants below have not yet
> been re-derived against build slots.

Borrowed from TCP congestion control, and the right shape for the same reason: reclaim
headroom cautiously, give it back immediately.

```
# every tick, after sampling
headroom = setpoint - util_ewma            # setpoint 0.75
queue_ok = loadavg1 / usable_cores < 1.2

if thermal is serious or critical:    B = B_min                # hard floor
elif util_ewma > setpoint + 0.05 or not queue_ok:
                                      B = max(B_min, B // 2)   # multiplicative decrease
elif headroom > 0.10 and queue_ok and demand_waiting:
                                      B = min(B_max, B + 1)    # additive increase

B_min = 1
B_max = floor(usable_cores / 1.2)          # slots, one slot = one Medium build
```

`demand_waiting` gates the increase: do not climb the budget when nobody is asking for a
slot, or the first fan-out after an idle period arrives with an unearned budget.

**Built and tuned in GOV-15** (`internal/budget/aimd.go`). Every constant is derived from the
312,515-sample Phase 0 trace and the reasoning is written at the constant, not here. Four
things changed against the text above, and each is a finding rather than a preference.

**The setpoint is 0.75 and the measurement's contribution was to show it barely matters.**
The instruction was to tune it against the traces, which assumed the choice was sensitive. It
is not, because host CPU on this machine is **bimodal**: p50 0.185, p90 0.425, then p95 0.728
and p99 1.000. The machine is quiet or pegged, with little in between, so the share of time
above a candidate setpoint moves by 1.5 points across the whole plausible range —
5.93% at 0.60, 4.89% at 0.75, 4.46% at 0.85. The design's 0.75 is therefore **kept**, and
kept for a reason. Retuning it to 0.71 would have been inventing a precision the distribution
does not carry.

**The run queue is doing more of the work than the setpoint, which is the opposite of what
this section implied.** Over the same trace it is p50 0.351, p90 0.820, p95 2.946, p99
**27.103**, max **49.620**. The 1.2 threshold sits just above the 90th percentile and fires on
7.68% of samples against the setpoint's 4.89%. More importantly, CPU% saturates at 1.0 and
stops carrying information, while a run queue of 27 per core does not — at the top of the
range the run queue is the **only** signal still moving, which is exactly the state the loop
exists for. 1.2 is the design's number and the measurement endorses it.

**`B_max` could not be carried over, because it counted agents.** `min(2 × performance_cores,
12)` was a statement about how many mostly-idle, I/O-bound things fit beside each other. A
build is neither. The ceiling is now however many Medium builds fit in the cores this process
may actually use — `floor(CPUCapacity / 1.2)`, which is 6 on this 8-core machine and 1 in a
`--cpus=2` container. The old expression would have given 8 slots here, which is 9.6 cores of
expected peak on a machine that has 8. `CPUCapacity` rather than the core count is
load-bearing: a container limited to two cores on a 64-core host would otherwise be handed 53.

**Thermal cuts at `serious` and above, not at any non-nominal state.** §5.3 demoted thermal to
a backstop; GOV-15 was required to decide explicitly rather than inherit, and the decision is
to narrow the trigger. Over 312,515 samples the state read `nominal` 97.15% of the time and
`fair` 2.85%, and `serious` or `critical` **never** — not once in 101 hours including the
busiest seconds on the machine. GOV-13 separately drove ten minutes of saturating 8-core load
with loadavg peaking at 66 without the state leaving `nominal`, corroborated by `pmset -g
therm` reporting no warning level. Flooring the budget on `fair` would throttle this machine
to one slot for nearly three hours in every 101, on a reading that came with no corroborating
evidence of throttling at all — and it would fire hardest during long builds, when it is most
expensive. `signals.Thermal.Severe()` is the narrowed predicate; `Elevated()` is unchanged and
still means what it always did. **It is honest to say this branch is, on this hardware, dead
code carried for a different chassis.** A fanless Air is not a Mac Pro under sustained load,
and a governor that deleted the check would have nothing to say on hardware that does throttle.

**The loop steps at the sample rate — 1 Hz — not the 2 s this section used to say.** The
sampler already ticks every second (§5.1) and a second cadence would be another timer for no
benefit. The practical difference is climb speed: one slot per second, so floor to ceiling
takes six seconds on this machine. Reactivity to noise is set by the EWMA window rather than
by the tick rate.

**The EWMA window stays at 5 s, and the trace says why.** Excursions above the setpoint are
p50 **1 s**, p75 3 s, p90 8 s — but p99 87 s and max 7,709 s. A loop reacting to a raw sample
would cut on the one-second transients, which are not a load problem and which make up most
of the excursions by count; a 5 s window damps those while still reaching a long burst inside
two ticks. The distribution is exactly the shape that justifies smoothing at all.

**The budget starts at `B_max`, not at `B_min`.** The daemon is started lazily *by a gate*, so
the moment it comes up is the moment somebody is trying to do something, and climbing one slot
per tick from the floor would throttle an idle machine for several seconds for nothing.
Starting high and cutting fast is the same asymmetry AIMD is built on.

Replayed against 3,000 contiguous real samples — the window of the trace with the most
setpoint crossings, run queue peaking at 49.6 per core — the budget took 6 distinct values,
reversed direction on 1.4% of ticks, and spent 76% of that saturated window at the floor
without becoming stuck there (`internal/budget/aimd_replay_test.go`).

**Measured against a real fan-out, the loop controls and it over-controls.**
[control-proof.md](control-proof.md) drove ten concurrent `go build -a` runs through a live
daemon and the same load with no hooks at all. Governed, the machine admitted 1 build of 10,
spent 4.8× less CPU on builds, and held the run queue to 4.22 per core against 11.93. Two
things came out of it that belong here:

- **Peak child cores cannot measure this and the acceptance criterion that named it was
  wrong.** It came out *higher* with the governor on — 6.67 against 4.84 — because one build
  alone gets the whole machine and compiles at full width, while ten at once thrash and
  deliver less. The metric tracks how fast the machine is going, not how oversubscribed it is,
  and it is bounded above by the core count. The run queue is the instrument that works, which
  is the same conclusion the trace analysis above reached from the other direction.
- **`RunQueueMax` is the binding constraint, not the setpoint.** One legitimate `go build -a`
  alone drives the run queue to ~2.9 per core, over the 1.2 threshold, so the loop reaches the
  floor within three ticks of the first build starting and `B_max` is unreachable in practice.
  1.2 is right as a description of ordinary use (p90 0.820) and too tight as a control
  threshold, and the two are different questions. Sweeping it needs measurements at several
  values rather than a line edit, so the number is unchanged here and the diagnosis is
  recorded instead.

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

### 5.5 Classification: which `Bash` is build-shaped

Phase 0 moved the control variable from agent count to build and test concurrency (§7 of
docs/phase-0-findings.md). That makes `Bash` the gated tool, and `Bash` fires 184–2,221 times a
day against the gate's 0–37. **Leasing per call is not an option, so a predicate is required,
and the predicate is the hard half — pointing the matcher at `Bash` is the easy one.**

`internal/classify` is that predicate. It is a pure function of the command text with no I/O,
called **gate-side before the daemon is contacted**, because the obvious alternative — ask the
daemon about every command and let it answer "not gated" — is a socket round trip on `cat`.

**The measured behaviour, from docs/bash-corpus.md, which is normative for anything built on
top of it:**

| | |
|---|---|
| Catch rate | 53.0% of expensive commands (35.2% held out) |
| False positives | 35.7% of cheap commands (28.8% held out) |
| Gated share | 18.4% of all `Bash` calls |
| Cost captured | 78.6% of measured child core-seconds |
| Cost of classifying | ~5 µs per call |

"Expensive" is **peak child cores ≥ 1.0** during the command. Peak, not mean: a cold
`go test ./...` measures 0.06 cores *mean* over 44 s because compilation is bursty and test
binaries then block on I/O, and a loop protecting against saturation must size work by its
peak.

**The ceiling is the number to design against.** Of 115 expensive commands, 61 name a
recognisable build tool and **all 61 are caught**; 54 name none and **none is caught**. The
misses are ad-hoc `python3` scripts, a pathological `grep` over a 100 MB binary, a `DerivedData`
sweep. No table edit reaches them, and a predicate that tried would have to gate `grep`. If a
later story needs that 47%, the mechanism is reacting to observed load after a command starts —
not a better predicate.

**Cost classes are a prior, not a constant.** `Class.ExpectedPeakCores()` returns 0.1 / 1.2 /
4.5 cores for Light / Medium / Heavy, from measured p90 peaks. They are a floor: the window
under-represents heavy modes. And they cannot be right everywhere, because **the same tool is
not the same cost in two repositories** — `uv run pytest` in one project here is expensive in
0.7% of 148 calls, while `python -m pytest` in another is among the most expensive commands on
the machine. A per-host table keyed on command shape, seeded from these classes and corrected
from what the sampler observes, is the design this points at. The seed exists so the first
decision is not random; it is not the last word.

**18.4% of calls taking a lease is a reduction, not cheapness.** It turns a 220× problem into
roughly an 11× one. §4.4's performance budget was priced for a gate firing a few dozen times a
week and must be re-derived against the 11× figure before `Bash` is actually gated.

### 5.6 How long a `Bash` lease lives, and why the reaper is the primary mechanism

A subagent lease is returned by `SubagentStop`, and the TTL is a backstop for the rare lost
event. **A `Bash` lease is not like that.** `hook-surface.md` §19 measured `PostToolUse` on
`Bash` and found it wrong in both directions: absent when the command failed, and present while
the command was still running when it was backgrounded or timed out. Invariant 2's posture —
*assume the stop hooks never fire; leaks are the default outcome* — is, on this path, no longer
a posture. It is the measured behaviour.

**What actually returns a `Bash` lease:**

| Outcome | Returned by | Slot held for |
|---|---|---|
| command succeeds | `PostToolUse` | its real runtime |
| non-zero exit the harness interprets (`grep`, no match) | `PostToolUse` | its real runtime |
| **command errors** — non-zero exit, failed compile, missing binary | **the reaper, at TTL** | the full TTL |
| **`run_in_background: true`** | **the reaper, at TTL** | the full TTL |
| **`timeout` exceeded** (backgrounded, not killed) | **the reaper, at TTL** | the full TTL |
| session dies | `SessionEnd`, or the reaper's session check | — |

Three of six rows are the reaper. A failing build is the *normal* state of a build under
development, so this is not a tail case.

**The TTL is therefore a control constant, not a safety margin, and it is set from
measurement.** Over the corpus `tools/bashcorpus` collects — 20,614 `Bash` calls from 498
transcripts, the same instrument and the same attribution GOV-14 used — the runtime of
**foreground** calls the classifier gates:

| | n | p50 | p90 | p99 |
|---|---|---|---|---|
| all foreground `Bash` | 20,261 | 1.7 s | 14.5 s | 227.7 s |
| **gated** foreground | 3,699 | **6.4 s** | **57.5 s** | **360.7 s** |
| gated, class Heavy | 858 | 18.1 s | 140.9 s | 602.0 s |

Background calls are excluded from every row above, and must be: their recorded end is the
launch receipt, not a completion, so their duration is not runtime. There were 353 of them,
87 gated.

The choice the table forces:

| TTL | gated calls that outlive it |
|---|---|
| 30 s (`DefaultTentativeTTL`) | 662 — **17.9%** |
| 60 s | 363 — 9.8% |
| 120 s | 207 — 5.6% |
| **300 s** | **51 — 1.4%** |
| 600 s | 19 — 0.5% |

**300 s.** The two errors are not symmetric and that is what picks the number. A TTL that is
too *short* reaps a lease out from under a build that is still running, so the budget
over-grants against real load — the governor's own failure mode, silent, and worst under
exactly the heavy fan-out it exists to control. A TTL that is too *long* holds a slot after a
failed build for up to five minutes, which costs throughput and is visible in `governor status`
as a slot count that does not match reality.

At 30 s the first error happens to nearly one build in five. At 300 s the second error is
bounded by the failure rate of builds, decays on its own, and covers 98.6% of gated commands
outright. Going further to 600 s buys 0.9 points of coverage for double the leak cost, which is
the wrong side of the trade.

**This does not make the reaper's session check less important — it makes it more.** A five
minute TTL on a session that has been killed would hold slots for five minutes; the session-PID
rule in §4.3 is what collects those in two seconds instead, and it applies to `Bash` leases
exactly as it does to tentative agent ones.

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
 "binary_version":"1.2.0","sources":[{"name":"local","budget":6}],"sessions":[...]}
// -> advise (MCP request_agents)
{"v":1,"op":"advise","session":"abc123","wanted":5}
{"v":1,"granted":3,"wait_hint_ms":0,"reason":"..."}
```

`source` and `sources` are plural because the budget is modelled as a *set* of sources
reduced by `min()`, with exactly one entry in v1. That plural is the entire cost of the
seam in section 12.

`binary_version` is the build running the daemon, and it answers a question `v` cannot: two
builds can both speak protocol v1 and still be different binaries. `governor install` reads
it to tell a current daemon from a stale one (section 13.4). It is additive — an older
reader ignores it — and it is absent from a daemon that predates it, which install reads as
"not this binary" rather than as "unknown".

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
claude-governor/                  the repo
├── install.sh                    curl-able bootstrap: fetch binary, then `governor install`
├── cmd/governor/                 one static binary
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
└── commands/governor.md          /governor status | limit N | pause | boost

~/.claude/skills/claude-governor/ the installed plugin, written by `governor install`
├── .claude-plugin/plugin.json    name, version, description
├── hooks/hooks.json              the six hooks in section 4.1
└── bin/governor                  the binary those hooks run

state  → ~/.claude/governor/      sock, daemon.pid, daemon.log, daemon.start, statusline.json,
                                  onboarded, surface.json, surface.json.tmp, samples.jsonl,
                                  sampler.lock, sampler.log
config → ${CLAUDE_PLUGIN_DATA}/config.json, falling back to the state directory
```

**§13.3's table is the normative list of paths; the state line above is a summary of the part
of it under `~/.claude/governor/`.** GOV-B09 found it naming five of nine — `daemon.start`
(GOV-11), `onboarded` (GOV-22) and the sampler's lock and log (GOV-02) had each been added to
§13.3 without this line following, which is the same drift as the status line a few
paragraphs down and in the same section. `TestSectionNineNamesEveryStateFile` holds the two
together now, because a summary nobody can check is how the first one survived. Adding a path
means adding it to §13.3, as §13.3 already requires; the summary then has to follow or the
suite is red.

**The installed plugin goes in `~/.claude/skills/`, not `~/.claude/plugins/`.** GOV-21
measured why ([hook-surface.md](hook-surface.md) §15): `~/.claude/plugins` holds a marketplace
cache and its bookkeeping, and a directory dropped beside them is loaded by nothing.
`~/.claude/skills/<name>/` auto-loads as `<name>@skills-dir` with no `settings.json` entry at
all, its `hooks/hooks.json` fires for a *matched* `PreToolUse`, and `${CLAUDE_PLUGIN_ROOT}`
and `${CLAUDE_PLUGIN_DATA}` both expand. Registering the gate in the other directory would
have been the silent never-fires failure §10.1 exists to prevent, arriving by a different
door.

The binary lives **inside** the plugin. One copy, so there is nothing for an upgrade to leave
skewed, and `${CLAUDE_PLUGIN_ROOT}/bin/governor` is one string on every platform — the release
artefacts are `governor-darwin-arm64` and friends and are renamed on the way in.

`.mcp.json` is not written yet: `governor mcp` is still a stub, and a plugin file registering
an MCP server that does nothing would be a broken server in the user's session rather than an
absent one. It arrives with the advisory channel (section 8).

`governor trace` is Phase 0 (GOV-02) and is deliberately not part of the surface above it.
It is `trace agent` (the `SubagentStart`/`SubagentStop` hook body), `trace sample` (the 1 Hz
sampler), `trace ensure` (the `SessionStart` hook that lazily starts the sampler), and its own
`trace install`/`trace uninstall` pair. It gates nothing and decides nothing; it only records
to `samples.jsonl`.

It is a separate command family rather than a temporary meaning for `gate`, `confirm` and
`release` on purpose: giving those a second definition now would leave the stories that
implement them properly (GOV-05, GOV-07) guessing which one they are replacing. `internal/trace`
and the whole `trace` family are deleted when Phase 1 lands, and `governor install` (GOV-21)
supersedes `trace install`.

The five `internal/` packages are not architecture astronautics: each is one interface with
exactly one implementation in v1, and each is a seam section 12 names. If a package needs a
second implementation before tier two exists, it was premature.

The statusline earns its place: `3/5 slots - cpu 61% - thermal ok` in every session turns an
invisible policy into something watchable, which is most of what makes a governor
trustworthy. Ship it in Phase 1, not as polish.

GOV-29 added one more line it can render, and it replaces the slot count rather than joining
it: `governor: ungoverned - restart to enable`, in a session whose hooks never loaded. Such a
session holds no leases, so it rendered `0/4 slots` — true, and identical to what a fully
governed idle session shows. A person watched that line read zero while two subagents ran and
concluded the governor was working. The count was the thing being misread, so the count is
what goes; and this is the one state where what a reader needs is not a number but an
instruction.

GOV-B07 added the other half of that. Claude Code runs a status line on events — an assistant
message, a compact, a permission-mode change, a settings change — and on nothing else unless
`statusLine.refreshInterval` is set (hook-surface.md §14.2). A session that has spawned a
fan-out and is waiting for it produces no assistant message until the fan-out ends, so the
line displayed whatever it captured before the spawns, for the entire duration of the one
thing it exists to make watchable. Install now writes `refreshInterval: 2`, in seconds.

Two seconds is argued rather than picked, and the measurement that decides it is not the one
expected. One invocation costs 4.32 ms of CPU on this host, of which the socket round trip to
the daemon is 0.6 ms: nine tenths of the cost is starting a process. So the interval cannot be
justified by what it saves the daemon, and `statusline.json` does not do what §13.3 below
originally claimed for it. What is left is affordable at any interval — S sessions at N
seconds cost S/N × 4.32 ms of CPU per second, so twenty sessions at two seconds is 0.5% of
this eight-core machine — which means the number has to come from what the line is for. Two
things fix it: it must be strictly longer than the one-second cache, or a lone session's timer
fires exactly at expiry and whether it reads the daemon or a stale entry is a race; and the
worst case from a lease changing to the line showing it is the interval plus a 300 ms debounce
plus up to a second of cache, which at two seconds is 3.3 s — inside the tens of seconds a
fan-out worth watching lasts.

GOV-10 shipped it, and the line above is what it actually renders. Two departures from what
this section originally illustrated, both deliberate. There is no leading spinner glyph,
because an animated one would have to come from somewhere and the statusline is stateless,
and the separator is the plain hyphen above rather than a middle dot. And every clause after
the slot count is conditional: `cpu` is absent until GOV-12 lands the sampler, and `thermal`
is absent on any host reporting `unknown`, which is every Linux host by design. A clause
naming a fact the daemon did not send would be a fabricated reading on the one surface whose
value is being trusted at a glance.

**GOV-B09 deleted the superseded illustration and pinned the ones that remain.** It outlived
its own correction because prose is not compiled: GOV-10 fixed the line this section leads
with and left the old one standing a paragraph below as history, GOV-22 then corrected §13's
account of the same line, and a reader arriving cold met two halves of one document
disagreeing about the single surface they would look at every day. The illustration stays
prose — moving it into the code would file it where nobody reading the design would find it —
so a test reaches into the prose instead.
`TestTheDocsIllustrateTheStatusLineThatActuallyRenders` extracts every status-line-shaped
span from this file and from README.md and requires that set to equal, exactly, the set of
lines `render` produces for the views it enumerates. An illustration that stops rendering is
red, an undocumented new one is red, and so is deleting them all — which is the failure a
one-directional check would have passed.

---

## 10. Hook surface — confirmed

**Confirmed against `claude --version` = `2.1.263 (Claude Code)` on macOS/arm64, 2026-09-07
(GOV-01), and RE-CONFIRMED against `2.1.270` on macOS/arm64, 2026-09-13 (GOV-30,
[hook-surface.md](hook-surface.md) section 18).** All four facts below were re-measured on
2.1.270 by GOV-01's own method and none of them moved; section 18 records what did change,
none of it load-bearing for the gate. **The `Bash` half of the surface — which GOV-15 gates —
was measured separately on `2.1.270`, 2026-09-14, in [hook-surface.md](hook-surface.md)
section 19**, and its conclusions are in §4.3 and §5.6 rather than below, because they are
about release rather than about spawning. Raw payloads are in
[hook-surface.md](hook-surface.md); this section is the conclusion only. Nothing below is
inferred from documentation. A session that needs a detail not stated here MUST measure it
and extend both files, not guess.

**That version stamp is no longer what `governor doctor` judges by.** GOV-30 found that
comparing the measured-against version to the running one can only ever report that a
measurement is old — it cannot tell a stale-but-still-correct matcher from one that has
stopped firing altogether, and those two are exactly what had to be told apart. Doctor now
asks a ledger of the tool names the gate has actually been seen to catch, per Claude Code
version (§10.4). This section stays the written record, and a test binds the two: the matcher
may not name a tool that no measured payload in hook-surface.md carries.

### 10.1 The four answers

**1. Tool name — `Agent`, and a `Task` matcher also fires.**
`tool_name` on the wire is `Agent`. A hook registered with `matcher: "Task"` fired for it
anyway; `Task` is carried as an alias. Both spellings are live *within this one version* —
the same run reported the same tool as `Task` in its `permission_denials`. So: keep the
both spellings in §4.1's regex, and match on the **payload** value in code. Registering only
`Task` would have worked by luck; relying on the payload saying `Task` would not. §4.1's
matcher also carries `SendMessage`, for the separate reason given in 1a.

**1a. What the gate is gated on — the decision GOV-28 was filed to make.**

The premise GOV-28 carried was that the gate saw about 4% of subagent spawns, because 63 of
70 `SubagentStop` records had no matching `PreToolUse` and correlated with `Skill`. Measured,
both halves are wrong ([hook-surface.md](hook-surface.md) §8.2). A `Skill` runs **inline** and
spawns nothing. The unmatched stops are not subagents: they are `Stop` events that the harness
relabels `SubagentStop` when the execution context that finished is not the main conversation
— overwhelmingly its own away-summary generation, which fires ~183 s after a turn ends. They
have no start, no tools, no transcript, and **no `PreToolUse` under any tool name**, confirmed
with a catch-all matcher watching every tool in every live session.

**So the budget stays driven by agent count**, and GOV-14 and GOV-15 need no correction: over
a 25 h window the number of agents the gate leased and the number of subagents that actually
ran were the same nine. What changed instead is one genuine omission — the `SendMessage`
resume path of §4.1, now gated.

Two rules follow, and both are load-bearing:

- **`SubagentStop` is not a spawn counter.** Anything deriving a count of agents from it is
  measuring the harness's bookkeeping as well as its subagents. `release` is safe because it
  keys off `byAgent`, which only `confirm` populates, so a non-agent stop is a `no_lease`
  no-op; nothing else may assume more.
- **`agent_type` present-but-empty on a `SubagentStop` marks a non-agent context**, cleanly
  separating the two populations across every record in the trace. It is a discriminator, not
  a missing field — which is what §4's GOV-02 addendum saw and could not explain.

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
- Whether a user **declining the permission prompt** suppresses `PostToolUse` — another `Bash`
  lease leak path if it does. Every probe in `hook-surface.md` §19 ran `bypassPermissions`.
- Whether the `backgroundTaskId` discriminator of §19.3 holds for tools other than `Bash`.
- Linux: every measurement above is macOS. Re-run the probe on Linux before trusting §10 there.

**No longer on this list, measured 2026-09-14 on 2.1.270 for GOV-15** (`hook-surface.md` §19):
`PostToolUse` on `Bash` carries the same `tool_use_id` as its `PreToolUse`; it does **not** fire
when the command errors; it fires as a launch receipt within ~50 ms for `run_in_background`; a
command that exceeds its `timeout` is backgrounded rather than killed and its `PostToolUse`
carries `timedOutAfterMs` beside `backgroundTaskId`; and a background task's completion raises
no event of any kind.

### 10.4 The surface is re-confirmed continuously, not re-asserted

The gate runs **only because** the PreToolUse matcher fired. Every acquire the daemon receives
is therefore a real payload the matcher caught, and the `tool_name` on it is what the harness
actually emitted — not what this repo believes it emits. GOV-30 turned that into the check:

- The gate forwards the payload's `tool_name` on the acquire (`tool`, additive, **no protocol
  bump** — an older daemon ignores it and an older gate omits it, and in both cases the
  machine keeps governing spawns while the ledger simply stays empty).
- The daemon records `(Claude Code version, tool name)` in `~/.claude/governor/surface.json`,
  off the acquire path, and only when the pair is new. On a settled machine that is one write
  per Claude Code upgrade.
- `governor doctor` passes the `spawn` check only when every Claude Code version running here
  has a recorded sighting **of a tool that starts a subagent**. Otherwise it is `unknown` —
  never `ok`, and never a failure.

**Only `Agent` and `Task` confirm; `SendMessage` does not.** The matcher covers three names but
only two of them start a subagent — `SendMessage` *resumes* one (§8.3 path 2). A rename of the
spawn tool would stop `Agent` matching while `SendMessage` kept matching, kept reaching the
gate and kept landing sightings on the new version; confirming on any sighting at all would let
the tool that still works cover for the one that broke, and print `ok` over an ungoverned
machine. The confirming set is therefore deliberately narrower than the matched set.

**Silence is the signal.** A Claude Code that renames the spawn tool does not announce it: the
matcher stops matching, the gate is never invoked, and every other check stays green because
the daemon is healthy and the matcher on disk is exactly the one this binary asks for. The
absence of a sighting on the running version is the only evidence available, so the check is
built to speak on it.

**What this must never become is a wider matcher.** Nothing in the ledger feeds
`spawnToolNames`. A matcher extended to catch every plausible name is not safer — it is
untested in a new way, and it would hide the very drift this exists to detect.

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
| `governor install` | write the plugin (manifest, hooks, binary), merge the statusline into settings (after backup), start the daemon, print exactly what it touched, then run `doctor` and exit non-zero if it fails |
| `governor uninstall` | reverse all of it, restore the settings backup, stop the daemon, remove state on `--purge` |
| `governor doctor` | daemon reachable? hooks registered? matcher matches the real tool name? **are the sessions running right now actually governed?** binary and plugin versions agree? config schema current? each failure names its fix |
| `governor calibrate` | measure cores, performance cores, RAM and a short load probe; write setpoint and B_max for *this* machine |
| `governor version` | binary version, protocol version, config schema version |

GOV-20 shipped `doctor`, and it reports eleven findings rather than the six checks this table
lists — `binary`, `protocol` and `config` are the three halves of "versions agree", split
because they have three different fixes and a line carrying three fixes is a line nobody acts
on; `strays` is GOV-B11's, below. Five details are worth stating because they are decisions
rather than implementation:

- **A finding can be `skip`.** With the daemon down there is no protocol version to compare
  and no lease table to read, and reporting those as further *failures* would print four
  problems and four fixes for one cause. A skip is only ever reachable downstream of a check
  that already failed and already named its fix, and it does not move the exit code.
- **A finding can be `warn`,** and the summary line may not round one off. GOV-B11 added a
  fourth state for a finding that is real, worth printing, and not a defect in this
  installation. It does not move the exit code — a warning that fails the command is a
  failure wearing a different label — but a run containing one says "N checks, M passing and
  K to look at" rather than "all passing". The claim "all passing" may only be made when it
  is true, which is the whole of what GOV-B11 was filed about.
- **A finding can be `unknown`,** printed when a check could not answer its own question —
  not "looked and it is fine", not "looked and it is broken", but "could not look". GOV-30
  added it after finding the spawn check reporting `ok` on a line that said, in its own
  detail, that it had not re-checked anything; the summary underneath then said "10 checks,
  all passing". **It is not a failure** — nothing is known to be broken — and it does not
  print as one. **It does move the exit code**, to `3`, because a report that cannot be
  trusted must not script as success, and because `1` already means "broken, here are the
  fixes". Two checks reach it today: `spawn`, until the gate has been seen to catch a real
  spawn on the Claude Code version running here, and `sessions`, when the harness keeps no
  session registry this daemon can read.

  **`governor install` treats exit 3 as success** and says why. A freshly installed machine
  has not spawned anything yet, so `spawn` is legitimately unconfirmed at the moment install
  finishes; failing there would put a red line in front of every first run, which is how a red
  line comes to mean nothing.
- **It reads every file that can hold one.** `~/.claude/settings.json`,
  `~/.claude/skills/claude-governor/hooks/hooks.json` — where GOV-21's installer actually
  writes them — and `~/.claude/plugins/claude-governor/hooks/hooks.json`, the directory 13.3
  named before that story measured it to be inert. The third is still read because a machine
  installed by an earlier build has hooks sitting in it, and a diagnostic that could not see
  them would report "not registered" for a file it is looking straight at, which is worse than
  the original mistake. It expands `${CLAUDE_PLUGIN_ROOT}` for the same reason — §4.1
  registers the hooks through that variable, and stat'ing the command verbatim would report
  "not an executable file" for every correctly installed plugin there is.
- **A matcher of exactly `Task` fails.** It fires on 2.1.263 by way of the alias in §10.1,
  which is precisely why: that section says registering only `Task` "would have worked by
  luck", and a diagnostic that passes something for working by luck will pass it on the
  version where the luck runs out. The matcher must cover `Agent`, `Task` and `SendMessage`.

GOV-29 added the two findings that make the rest of them mean anything, because every check
above reads a file, a socket or a version and **all of them pass on a machine where not one
spawn is being governed**. That is not hypothetical: on 2026-09-09 this machine ran six
sessions, four of them ungoverned for twenty-nine hours, and doctor reported 8 of 8.

- **`sessions` compares what is running against what has registered.** Hooks bind at session
  start (hook-surface.md §17), so a session already open at install time never loads the gate
  and never will. The live set comes from the harness's own registry and is joined to the
  daemon's session table on `session_id`. It FAILS naming each session by its directory,
  because the fix is a person restarting a particular window and a bare uuid is not something
  anyone can act on.
  - It must not blame a **daemon restart**. The session table is in memory and `install`
    restarts the daemon, so straight after any upgrade every live session is missing from it,
    including governed ones. A session that started before this daemon did is treated as
    having registered with a previous one, which is the ordinary state and not a fault. The
    check has three inputs for this reason — the registered set, the daemon's start time, and
    the hook file's mtime — and only the last of those is decisive on its own.
  - Where the registry cannot be read it reports **"not verified"**, as a pass that states its
    own limit rather than a skip. A skip in this command means "downstream of a check that
    already failed and already named its fix", and nothing has failed here. The wording leads
    with "not verified" because this is the one line where `ok` would otherwise be read as
    "checked and fine", which is the exact misreading being removed.
- **`spawn` dates the tool names against the version actually running.** `matcher` reads the
  matcher off disk and tests it against `spawnToolNames`, which catches a hand-edited or stale
  matcher — but `install` writes that matcher *from* the same list, so on a stock install the
  two agree by construction and a rename in Claude Code would leave both agreeing while
  nothing was governed. This finding cannot detect a rename either, and does not claim to. It
  states the version the names were last measured on, so that "the matcher is correct" becomes
  "the matcher was correct as of 2.1.267" — a claim that expires. It is deliberately **not** a
  failure when a newer Claude Code is running: that is routine, is not fixable by anyone who
  is not maintaining this program, and a red line after every upgrade teaches people that a
  red line means nothing.

Neither finding writes anything. `~/.claude/sessions/` is read-only to the governor and adds
no row to §13.3.

GOV-B11 added `strays`, which is the only finding that reports on the **machine** rather than
on this installation, and the only one that is a `warn`.

- **`strays` names every long-lived governor daemon that is not this one**, with its pid,
  path, age and the socket it holds. It exists because four governor-shaped processes — two
  daemons on scratch sockets, aged three days each, and two abandoned `go run` binaries aged
  twenty and twenty-eight days — were found running on the dev machine while doctor reported
  ten checks all passing, with the daemon check showing nothing in use and nothing waiting.
  Each held a socket of its own, so nothing was double-leased and no existing check could have
  seen them. Harmless and invisible, and
  the invisibility is the half worth fixing: the blind spot that hid four idle daemons would
  equally hide one that was busy, and a stray daemon's CPU is invisible to the Phase 0
  sampler too, because it is not a descendant of any `claude` process.
- **It is scoped on two axes, and widening either is a mistake.** The process must have a
  `daemon` argv — which is what structurally excludes the Phase 0 sampler (`governor trace
  sample`), rather than a special case that can go stale — and must be older than
  `strayMinimumAge` (ten minutes). Matching "any process named governor" would fire on a
  developer's own `go run ./cmd/governor` during ordinary work on this repo, and a check that
  fires during normal work is a check that gets silenced.
- **The socket comes from `lsof`,** bounded like every other subprocess doctor runs. gopsutil
  cannot supply it on darwin: `OpenFiles` and `Environ` both return "not implemented yet" and
  `Connections` returns nothing for a process holding a unix socket. The path comes from
  `argv[0]` resolved against the process's working directory, **not** from `Exe()`, which
  fails outright for a process whose binary has been deleted — the state both scratch daemons
  on the dev machine were in, and exactly the case worth reporting.
- It writes nothing, and adds no row to §13.3.

"Stranded" is also a precise thing rather than a feeling. The reaper collects a lease three
ways — its TTL expires, its session's process is gone, or `Liveness` says its owner is gone —
and the last two need an owning PID, bound at `session start` and at `confirm`. A lease held
under a session with **no** PID has neither available to it and holds its slot for a full
TTL, half an hour for a confirmed one. That is what a full budget on an idle machine actually
looks like, and the status line reports it as `4/4 slots` either way.

### 13.3 What touches the user's machine

Uninstall is only verifiable if this list is exhaustive, so it is normative.

| Path | Written by | Removed by |
|---|---|---|
| `~/.claude/skills/claude-governor/.claude-plugin/plugin.json` | install | uninstall |
| `~/.claude/skills/claude-governor/hooks/hooks.json` (the six hooks) | install | uninstall |
| `~/.claude/skills/claude-governor/bin/governor` | install, copied from the running binary | uninstall |
| `~/.claude/skills/` | install, only if absent | uninstall, only if it empties |
| `~/.claude/settings.json` (`statusLine` key only) | install, merged | uninstall, previous value restored from backup |
| `~/.claude/settings.json.governor-backup-<ts>` | install, only when a settings file existed | uninstall, unless `--keep-backup` |
| `~/.local/bin/governor` | install, **only if `~/.local/bin` already exists** and nothing else is at that name | uninstall, only if it still links to our binary |
| `~/.claude/governor/sock` | daemon | uninstall |
| `~/.claude/governor/daemon.pid` | daemon | uninstall |
| `~/.claude/governor/daemon.log` | install, when it starts the daemon | uninstall |
| `~/.claude/governor/statusline.json` | `statusline`, when the directory already exists | uninstall |
| `~/.claude/governor/daemon.start` | the gate, when it lazily starts a daemon and the directory already exists | uninstall |
| `~/.claude/governor/onboarded` | `session start`, only in the fallback below, and only when the directory already exists | uninstall |
| `~/.claude/governor/surface.json` | daemon, when it first sees a spawn tool name it has not recorded for that Claude Code version (GOV-30) | uninstall |
| `~/.claude/governor/surface.json.tmp` | daemon, staging the file above before the rename | uninstall |
| `~/.claude/governor/` | daemon (`transport.Listen`) | uninstall, only once it is empty; `--purge` regardless |
| `~/.claude/plugins/data/claude-governor-skills-dir/` (`${CLAUDE_PLUGIN_DATA}`: config, onboarding marker) | Claude Code creates it; the governor writes config.json and `onboarded` | uninstall `--purge` only |

Four rows carry a condition, and each condition is the difference between "removes everything
it added" and "removes things it did not":

- **`~/.local/bin/governor` is a convenience, never a requirement.** install does not create
  `~/.local/bin`; conjuring a standard PATH directory into someone's home would leave uninstall
  deciding whether to remove it. When it is absent, install says so and names the full path,
  which works. Something else already at that name is left strictly alone in both directions —
  far more likely another tool than a stale link of ours, and the cost of guessing wrong is
  deleting a binary somebody depends on.
- **`~/.claude/skills/` and `~/.claude/governor/` are removed only if empty.** `os.Remove`
  refuses a non-empty directory, which is exactly the test: a machine with other skills-dir
  plugins, or with `samples.jsonl` still in it, keeps the directory and loses only our files.
- **`surface.json.tmp` has a FIXED name, not a random one.** It is written by a goroutine
  nothing waits for, so a `SIGTERM` between staging and rename can leave it behind — and
  uninstall removes files it can name. A `CreateTemp`-style random suffix would be exactly the
  residue that keeps `~/.claude/governor/` from emptying, which is the one difference between
  "removes everything it added" and "removes most of it". Safe only because the ledger
  serialises its writers; see `surfaceLedger.writeMu`.
- **`samples.jsonl` survives a plain uninstall**, per the Phase 0 table below. It is the
  measurement the setpoint is derived from and it is not reconstructable.
- **The `statusLine` value is restored, not deleted.** There is only one status line, so
  install *replaces* whatever was there; uninstall's job is to undo a replacement, and the
  backup is the only record of the previous value. When install found no settings file at all
  it writes no backup — and uninstall then removes the file it created rather than leaving an
  empty `{}` behind.

`governor uninstall` removes our `statusLine` **surgically** rather than restoring the backup
wholesale, for the same reason `trace uninstall` does: a wholesale restore would destroy
anything the user added to `settings.json` since install. It then compares the result against
the backup and, when nothing else changed, writes the backup's original bytes — so the
ordinary case is byte-identical, formatting and key order included, not merely equivalent.
There is a test that asserts this over a recursive digest of the whole home directory rather
than over this table, because this table being exhaustive is the thing under test.

`${CLAUDE_PLUGIN_DATA}` is set by Claude Code for a plugin. When it is unset — a binary run
by hand, or a `doctor` run outside a plugin context — `config.json` resolves to the state
directory above, beside the socket, rather than to a location of its own. One directory holds
all of the governor's state, and a second home for the config would be a path uninstall
silently stops covering.

`onboarded` is the record that the first-run notice has been shown (GOV-22), and it lives
beside `config.json` for `config.json`'s reason: one directory holds the governor's state, and
a second home for a file is a path uninstall silently stops covering. In a plugin context that
is `${CLAUDE_PLUGIN_DATA}`, so a plain uninstall **keeps** it and only `--purge` removes it —
which is the behaviour the notice wants. Reinstalling after an ordinary uninstall is the same
machine and the same person, and re-showing a notice they have already read and acted on is
nagging; `--purge` says "forget everything about me", and a genuine fresh start should be
greeted like one. Outside a plugin context it falls back to the state directory and is removed
by a plain uninstall along with everything else there.

It follows the same never-create-the-directory rule as the two files below, and one
consequence is worth stating: where the directory is absent the notice is not shown at all,
rather than shown and unrecorded. A notice that cannot record itself is a notice that repeats
every session forever, which is the failure this file exists to prevent.

`daemon.start` is the rate limit on lazy daemon starts (GOV-11), and it follows the same
rule as `statusline.json` for the same reason: it is written only when the state directory
already exists, because a gate that recreated the directory would put a file back moments
after uninstall removed it. Its modification time is the whole of its meaning — the timestamp
inside it is for a person reading the directory, not for the code. Both files are named
relative to the socket the gate actually failed to reach rather than to the default state
directory, so a second governor on `GOVERNOR_ENDPOINT` rate-limits beside its own socket
instead of over the first one's.

`statusline.json` is a one-second cache of the last status reply (GOV-10). The statusline is
run on every assistant message in every session, and since GOV-B07 on a two-second timer as
well, so without it the busiest thing the governor does is answering its own status line. It
is written only when the state directory already exists — the statusline never creates it —
because a command that ran on every message and recreated the directory would put a file back
moments after uninstall removed it.

What the cache actually buys was measured for GOV-B07 and is narrower than the paragraph above
assumed. A cache hit costs 5.1 ms at p50 and a miss 5.7 ms, so it saves 0.6 ms of a 5.7 ms
call — the daemon round trip is a tenth of the cost and process startup is the rest. It is
still worth keeping, but for a different reason than per-call cost: it bounds *daemon* round
trips at one per second machine-wide however many sessions are open, which is what stops the
daemon from being the thing that scales with session count. Process spawns it cannot bound,
and those are what §9's refresh interval is chosen against.

Phase 0 (GOV-02, extended by GOV-27) adds the rows below. They are listed here rather than in a story note
because this table is what makes uninstall verifiable, and a path that is touched but not
listed is a path uninstall silently stops covering. All of them go when Phase 1 replaces the
`trace` family.

| Path | Written by | Removed by |
|---|---|---|
| `~/.claude/settings.json` (five `trace` hooks) | `trace install`, merged after backup | `trace uninstall`, surgically |
| `~/.claude/settings.json.governor-trace-backup-<ts>` | `trace install` | `trace uninstall` |
| `~/.claude/governor/samples.jsonl` | `trace agent`, `trace sample` | by hand — it is the measurement, and deleting it on uninstall would throw away the phase |
| `~/.claude/governor/sampler.lock` | `trace sample` | by hand |
| `~/.claude/governor/sampler.log` | `trace sample` | by hand |
| `bin/governor` in the repo, staged by hand in Phase 0 | by hand | by hand — GOV-21's `governor install` supersedes it, and installs into the plugin instead |

The five hooks are `SubagentStart`, `SubagentStop`, `SessionStart`, and — added by GOV-27 to
settle whether either fires interactively — `PostToolUse` and `Stop`. `PostToolUse` is
registered with an `Agent|Task` matcher and that matcher is load-bearing rather than tidy:
unmatched, it would run this binary on every tool call in every session.

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

Unknown keys are preserved **byte for byte**. Decoding them through a generic value would
round every number through `float64`, so a newer version's large integer would come back
mangled — preserved in name and corrupted in value, which is worse than dropping it because
nothing reports it. `config.Config.Unknown` therefore holds raw JSON.

Three states are refused rather than guessed at, and `doctor` reports each the same way the
store treats it. **A missing file is not one of them**: a machine that has never run
`calibrate` has no config and runs on built-in defaults, which is a state and not a fault, so
`Load` returns the zero config and writes nothing.

- **A schema from the future.** This binary does not know what changed, so it can neither
  read the file nor safely rewrite it. Refusing beats saving a truncated version of someone
  else's settings over the original.
- **No `"schema"` key at all.** Assuming schema 1 would mean migrating a document of unknown
  provenance and writing the result back over it.
- **A gap in the migration chain.** Migrating 1 to 3 by way of nothing would hand schema-1
  keys to schema-3 code, which is the corruption the version number exists to prevent.

Every config key ships with a default that leaves its feature **inert**. An upgraded binary
reading an old config must behave exactly as it did before until the user opts in, which is
what makes an upgrade a non-event rather than a change of behaviour nobody asked for.

**Never overwrite the binary in place.** Measured in GOV-02, on macOS: `cp` onto the existing
`bin/governor` invalidates its code signature, and the kernel then **SIGKILLs** every
subsequent execution — `exit=137`, no output, no error message anywhere. That defeats the one
guarantee the hooks make, because a process killed by a signal cannot exit 0 no matter how
carefully it is written; the failure is external to the program. During an in-place upgrade
this would land on every hook of every live session at once. Upgrade MUST write the new
binary to a temporary name in the same directory and `rename(2)` it over the old one, exactly
as the config write does — a rename swaps the directory entry and leaves running processes on
the old inode, so nothing in flight is signed wrong.

GOV-21's `governor install` does exactly that: it writes the binary to a sibling temp name in
the plugin's `bin/` and renames it over the old one, and it skips the copy entirely when the
digests already match. That skip is what makes a second install a no-op rather than a
needless rewrite of a file every live hook may be executing.

**The version is stamped at release, not left at `0.0.0-dev`.** The release workflow builds
with `-ldflags -X main.binaryVersion=<tag>`, which is why `binaryVersion` is a var. `doctor`
compares the version the hooks run against the version running the check, and two different
builds that both call themselves `0.0.0-dev` compare equal — so an upgrade that left stale
hooks behind would pass the one check that exists to catch it.

Every protocol message carries `"v"`. A daemon that does not support a gate's version replies
`unsupported_version`, and the gate treats that reply exactly like an unreachable daemon: it
fails open. A gate that cannot parse a reply at all does the same. The rejection is answered
rather than ignored because silence costs every spawn its full budget before failing open,
and it is never retried, because a version mismatch is permanent for the life of the daemon
and a retry only moves the stall. This is what makes upgrade safe — during an upgrade there
will be a live daemon of one version and gates of another, and the only acceptable outcome is
ungoverned spawns, never a stalled session.

**`install` replaces a daemon that is not this binary.** The rename above deliberately leaves
the running daemon on the old inode, so immediately after an upgrade the process holding the
socket is still the old one. If it were left there, a protocol change would make every gate on
the machine fail open for as long as that process lived — silently, behind an install that
looked complete. So install brings up a daemon of the binary it just wrote.

It decides on two independent signals, and needs both:

- **The digest.** Install already knows whether it renamed new bytes in, because it skips the
  copy when the digests match. This is the only signal that catches an upgrade between two
  builds that call themselves the same version — which is every development build, since they
  all report `0.0.0-dev`.
- **What the daemon says it is.** Install asks the running daemon for `status` and compares
  `binary_version` and `v`. This is the only signal that catches a daemon left stale by an
  *earlier* install, where the binary on disk is already correct and unchanged, so no number
  of re-installs would ever copy anything again.

A daemon matching on both is left alone: restarting a healthy one drops every live lease for
nothing, and a second install of the same binary must remain a no-op. Every ambiguous answer
— unparseable, silent, or from a daemon too old to carry `binary_version` — counts as *not*
current, because a needless restart costs reservations while a missed one costs governance for
the life of the process.

In-flight leases are lost across the replacement, and that is correct rather than merely
tolerated: a lease is a reservation, not something a running agent holds, so no agent is
harmed. The reaper collects what the restart orphans. Gates firing in the window between the
old daemon releasing the socket and the new one binding it find nothing listening and fail
open on their own 100 ms budget — a spawn loses its governance, never a session its
responsiveness.

### 13.5 Platforms

macOS and Linux, arm64 and amd64. Linux is not padding: it is where a tier-two broker would
run, and where cgroup limits are a better signal than raw CPU sampling. Thermal state has no
Linux equivalent and reports `unknown`, which section 5.3 already requires be treated as
nominal. Windows is out of scope — named pipes and different process introspection roughly
double the surface for no current user.
