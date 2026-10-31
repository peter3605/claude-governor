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

**A refusal must not state a conclusion its own numbers refute** (GOV-34). The rule above —
name no fact the daemon did not send — was not enough on its own. `Machine at capacity (1/4
build slots)` states only facts the daemon sent and is still unreadable, because the reader can
see that one of four is not capacity and has not been given the third number that reconciles
them: how much the refused request itself weighed. So a capacity refusal now carries `asking`,
and the head sentence depends on what is actually true:

| State | Head |
|---|---|
| held ≥ budget | `Machine at capacity` |
| `cause: share` | `This session is at its fair share` |
| held < budget | `Not enough free build slots` |

```
Not enough free build slots (1/4 build slots held, this one needs 3.8, thermal: nominal).
This command was not run. This is a large build and the slots left free are not enough for
it. Wait a few seconds and run exactly the same command again; do not look for a different
way to build, and do not treat this as a build failure.
```

The instruction half is unchanged across all three heads — wait, then run the same command
again — because the right response does not depend on which of them is true. Only the asserted
fact changes, and a refusal that misstates that is one a reader stops believing. A budget below
this host's ceiling additionally names the signal holding it down (`budget cut from 6 by run
queue`), from the same controller state `governor status` and `governor doctor` read, so the
three surfaces cannot disagree about why a machine is refusing. §6.1a is the incident that
forced all of this.

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
| CPU utilisation | `gopsutil/cpu`, 5 s EWMA | traced and reported; **not** a control input (§5.2) |
| Run-queue depth | `loadavg1 / ncpu` | **the setpoint the loop controls on**, both directions |
| Thermal pressure | `ProcessInfo.thermalState` | hard cut trigger |
| Memory pressure | free pages + swap-in rate | secondary cut trigger |
| Process-tree cost | CPU across the `claude` tree, roots and children separately | traced and reported; **not** a control input (§5.4) |

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
daemon over the 1%-of-a-core bar that story sets. Nothing the loop reads is affected: tree cost
is not a control input (§5.4), and CPU, run queue, thermal and memory — the signals §5.2 acts
on — still tick every second. Samples carry `TreeFresh` so a consumer cannot mistake five
carried-over copies for five readings. Phase 0's own sampler is unchanged and still walks
every tick, because GOV-15 tunes the setpoint against traces collected at that cadence.

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

> **`B` is a count of build slots, and the loop controls on the run queue.** Two things moved
> since this section was first written, and both are measured rather than preferred.
> **What `B` counts** changed in GOV-15: Phase 0 found agent cost small and near-constant while
> build cost is zero almost always and occasionally enormous, so the rationed quantity became
> build and test concurrency (docs/phase-0-findings.md §7, §5.5 for what takes a lease).
> **What the loop reads** changed in GOV-32: host utilisation was the primary input and is now
> not an input at all, because it saturates at 1.0 under exactly the load this governor exists
> for and cannot distinguish two builds from eight ([climb-condition.md](climb-condition.md)).
> Additive increase, multiplicative decrease, the thermal floor and the `demand_waiting` guard
> all survive both changes untouched. The constants below are derived against build slots and
> the reasoning lives at each constant in `internal/budget/aimd.go`.

Borrowed from TCP congestion control, and the right shape for the same reason: reclaim
headroom cautiously, give it back immediately.

```
# every tick, after sampling
run_queue = loadavg1 / usable_cores

if thermal is serious or critical:    B = B_min                # hard floor
elif run_queue > 2.0:                 B = max(B_min, B // 2)   # multiplicative decrease
elif run_queue <= 1.5 and demand_waiting:
                                      B = min(B_max, B + 1)    # additive increase
else:                                 B = B                    # busy and coping: hold

B_min = 1
B_max = floor(usable_cores / 1.2)          # slots, one slot = one Medium build
```

**The loop controls on saturation, not on utilisation, and `util_ewma` is not an input to it.**
That is the one structural change since GOV-15 and it was made in GOV-32 against measurement —
[climb-condition.md](climb-condition.md). The reasoning is below under "the setpoint is a
run-queue setpoint"; the short form is that CPU% saturates at 1.0 from two concurrent builds
onward and therefore cannot tell the states this loop exists to distinguish apart, while a run
queue moving 1.53 → 2.85 → 24.98 across the same states can.

**There are four outcomes and the fourth is a hold.** `2.0` is where the budget is cut and
`1.5` is where it climbs, so between them is a band in which the loop does nothing. That is
where it is meant to sit under build load: the machine busy, the queue shallow, the budget
steady. The two thresholds are one constant and a band — `RunQueueMax` and `RunQueueBand` — so
that moving the cut moves the climb with it and the gap keeps its width.

`demand_waiting` gates the increase: do not climb the budget when nobody is asking for a
slot, or the first fan-out after an idle period arrives with an unearned budget.

**Built and tuned in GOV-15** (`internal/budget/aimd.go`). Every constant is derived from the
312,515-sample Phase 0 trace and the reasoning is written at the constant, not here. Four
things changed against the text above, and each is a finding rather than a preference.

**The setpoint is a run-queue setpoint. Utilisation is not a control input.** This is the
loop's shape, so it is stated normatively rather than as a change log: **`B` is cut when
`loadavg1 / usable_cores` exceeds 2.0, climbed when it is at or under 1.5, and held between
them. Host CPU is sampled, traced and reported, and the controller does not read it.**

Three measurements took utilisation out, in the order they were made.

- **It decided everything under the load that matters.** Over the Phase 0 trace, ordinary use
  is above a 0.75 setpoint on 4.27% of samples and above a 2.0 run-queue threshold on 5.62% —
  the run queue does more of the work. Under a build fan-out it inverts completely: host CPU
  sits at p50 1.00 and is over `setpoint + 0.05` on 72–99% of ticks, so the CPU disjunct
  decided every cut and the run-queue threshold decided none of them. Five values of the
  threshold from 1.2 to 6.0, measured twice under the same ten-way fan-out, produced no
  difference the experiment could detect, because none of them were ever consulted
  ([run-queue-sweep.md](run-queue-sweep.md)).
- **The increase branch could not fire at all.** The climb required CPU below
  `setpoint − 0.10` = 0.65. Counted rather than inferred: **0%** of an ungoverned ten-way
  fan-out's busy ticks, 4% of a governed arm's, and 0 of the replay trace's 471 ticks at
  CPU ≥ 0.99. `B_max` was unreachable whenever any real build was running.
- **The operating point it drove to was not a full machine.** At one build — where the loop
  floored and stayed — this host measures host CPU **0.77** and run queue **1.22 per core**.
  Saturation begins at two. The machine sat 23% idle with nine build requests waiting, which
  makes "the machine is full and one at a time is correct" false here rather than arguable.

Note where 0.77 landed: between the climb at 0.65 and the cut at 0.80. **The loop had converged
on its setpoint** and was behaving exactly as an AIMD controller with a utilisation setpoint of
0.75 is specified to. The defect was never in the mechanism — it was that a utilisation
setpoint of 0.75 asks for a machine three-quarters busy, and asking that of a *build* governor
is asking it to leave a quarter of the machine unused while work is queued.

**Why the run queue can carry both directions when CPU cannot.** CPU% saturates at 1.0 and
stops carrying information; a run queue does not. Across the states this loop must tell apart —
two builds, four builds, eight builds — CPU reads 1.00, 1.00, 1.00 while the run queue reads
1.53, 2.85, 24.98. That is §5.1's argument for the signal, applied to the increase branch and
not only to the cut. The cost is that `loadavg1` is a one-minute decaying average, so the loop
is slower to react to a rising edge than a 5 s EWMA of CPU would be; §5.2's own invariant 5 is
what makes that affordable, since a cut never revokes running work and therefore only ever
affects the *next* admission. [climb-condition.md](climb-condition.md) carries the experiment.

**The CPU constants that survive are reporting thresholds, not control.** `Setpoint` and
`SetpointBand` remain in `internal/budget` because `tools/sweep` draws its CPU column against
them and because the bimodal distribution they were measured from — p50 0.185, p90 0.425, p95
0.728, p99 1.000 — is the reason the loop can afford to ignore utilisation at all. `Headroom`
was read only by the removed climb condition and was deleted rather than left to look live.

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
setpoint crossings, run queue peaking at 49.6 per core — the budget takes 6 distinct values,
reverses direction on **0.3%** of ticks, and spends **67.7%** of that saturated window at the
floor without becoming stuck there (`internal/budget/aimd_replay_test.go`).

Both figures are re-derived rather than adjusted, and the two changes that moved them are kept
apart — GOV-31 raised the run-queue threshold from 1.2 to 2.0 without re-running this test, so
the figures this section used to carry were two changes out of date, not one.

| | before GOV-31 | at 2.0 (GOV-31) | now |
|---|---|---|---|
| distinct values | 6 | 6 | 6 |
| direction reversals | 1.4% | **2.7%** (81 ticks) | **0.3%** (9 ticks) |
| share at the floor | 76% | 66.8% | 67.7% |

**Reversals rose when the threshold rose and then fell by an order of magnitude here**, and both
movements have the same cause. At 1.2 the run-queue test fired on nearly everything the CPU
test did, so the two agreed; at 2.0 a band of ticks opened up where CPU cut and the run queue
did not, and the budget alternated across it. Removing utilisation closes that band: the loop is
no longer cut by a signal crossing its threshold on one-second transients while the run queue
has not moved at all.

The floor figure holding still is the result to want. **This change was not meant to make the
loop cut less on a genuinely oversubscribed trace, and it did not.** What it changed is that
the budget can leave the floor while builds are still running.

**`in_use` and the budget stopped being the same unit, and the status line says so.** A lease
count and a quantity of budget are different numbers once a subagent weighs 0.08 slots and a
Heavy build 3.75, so the wire carries both — `in_use` is still the count, and an additive
`slots` field is what the status line and the deny text render beside `budget`. Four agents
reading "4/6 slots" on a machine that is 5% full is a number that invites exactly the wrong
response. Whole values print whole; a fraction prints to one place, so a machine holding only
agents reads "0.2/6 slots" rather than rounding to a reassuring zero.

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
- **`RunQueueMax` was diagnosed as the binding constraint. GOV-31 swept it and the diagnosis
  was wrong.** The threshold is now **2.0**, raised from 1.2, but the change is small and the
  reason for it is not the one predicted. One `go build -a` alone measures **1.22** run queue
  per core on a settled machine, not the ~2.9 recorded here — that figure was read off a
  ten-way fan-out arm. So 1.2 was too tight by 2%, not by a factor of two to three. The sweep
  itself found the threshold's value undetectable between 1.2 and 6.0 under build load, because
  the setpoint binds there instead; 2.0 is chosen from the ordinary-use distribution and the
  concurrency curve, and it costs 1.92 points of trace coverage to buy a single Medium build
  that no longer floors the budget. `loadavg1` is also a one-minute average and a build runs
  eight seconds, so this signal cannot see a short build at all — a limit of the instrument
  that no choice of number fixes. [run-queue-sweep.md](run-queue-sweep.md) carries the
  experiment, the daemon logs and the curve.
- **That threshold is now load-bearing for the first time, and it was chosen while it was
  not.** GOV-31 settled on 2.0 from the ordinary-use distribution and the concurrency curve,
  explicitly because its sweep could detect no difference between 1.2 and 6.0 under build
  load — and the reason it could detect none is that the CPU test decided every cut before the
  threshold was consulted. Removing the CPU test removes that shadow. `RunQueueMax` and
  `RunQueueBand` together now set the operating point under exactly the load the sweep could
  not see them under, so the sweep that found nothing is worth re-running against this loop,
  and it would this time be measuring a live constant. **GOV-25 re-ran it, and 2.0 stays the
  compiled-in value — what changed is that it is no longer the only value a host can have.**
  `governor calibrate` raises it above whatever this machine's own ambient run queue is, which
  is the quantity that differs between a developer's laptop and a CI box and the one the
  original constant could not express. §5.2a is the derivation and
  [calibration.md](calibration.md) is the re-sweep.
- **GOV-31 left the loop's shape open, and GOV-32 closed it: it was over-control.** Under
  build load the budget could not climb back at all, because the increase branch required CPU
  below `setpoint − 0.10` = 0.65 and a build never leaves CPU that low, so a fan-out floored
  the budget and pinned it there until the load drained. What settles it is not a throughput
  argument — those are inside this harness's run-to-run variance — but a utilisation one: at
  the floor this host reads CPU **0.77** and run queue **1.22 per core**, so the machine the
  loop had decided was full was 23% idle with work queued. The answer was to control on
  saturation instead, which is what the pseudocode above now specifies.
  [climb-condition.md](climb-condition.md) is the experiment, and it corrects one thing in the
  table above on its way past: `build_test_core_sec` comes from a 1 Hz walk of the process
  tree, and `go build` spawns many short-lived `compile` processes, so it undercounts — and
  undercounts *more* at low concurrency. The wall-clock and host-CPU columns are unaffected,
  and every conclusion drawn here rests on those.

### 5.2a Calibration: where those constants come from on a machine that is not this one

Everything in §5.2 was measured on one 8-core fanless laptop. The project rule is that nothing
tuned there ships as anyone else's default, so `governor calibrate` (GOV-25) measures the host
it is running on and writes two of those constants into `config.json`. Absent config means the
compiled-in values stand, exactly as before — the inert default §13.4 requires of every key.

**The cut threshold is placed a fixed headroom above what the host is *already doing*.**
`RunQueueMax = ambient + 4.30`. The split between the two terms is the design: what differs
between machines is not how much run queue a build produces — that is a property of the build
and the core count, which the existing arithmetic already handles — but what the machine was
doing before the governor saw it.

**The headroom is bounded on both sides by a measured curve, and the first value this story
derived was wrong.** 1.65 came from subtracting the Phase 0 trace's settled run queue (p50
0.352) from GOV-31's 2.0, which has the pleasing property that a host idling where that trace's
host idled calibrates back to 2.0 exactly. It is wrong because GOV-31's concurrency curve is
**under-converged**, as [run-queue-sweep.md](run-queue-sweep.md) §4 Correction 2 says of its own
numbers: one build runs 8.1 s against a one-minute decaying average, so its 1.22 per core "is
mostly the residue of the settle threshold". Re-measured under *sustained* load — builds
relaunched for 180 s, three averaging windows — one build reaches **3.04 p50 and 4.07 peak**,
two reach 5.83, three reach 10.06. So 1.65 put the cut at 2.30 on a host measured idling at
0.65, below what one build produces, and the budget floored with a single build running. The
interval the measurement bounds is `cut > 4.07` (the peak at one build) and `cut ≤ 5.83` (the
median at two, so a second build still trips it); 4.30 is its midpoint less that host's ambient.

**The bound is the peak, not the median, and that is the sweep's real finding.** AIMD halves on
a cut and climbs one slot per tick, so a rare excursion above the threshold is not rare for the
budget: an arm whose threshold sat 0.04 *below* its own peak run queue satisfied the cut
condition on 7% of ticks, the climb condition on 48%, and still ended at the floor. Sorting the
four sweep arms by whether the threshold cleared that arm's own peak orders all four; sorting
them by the threshold's value does not.

A floor at the shipped 2.0 remains, but **no longer binds on any real host** — every
non-negative ambient reading now lands above it. It is kept as a guard on the constant, not on
the reading. **A host whose load average could not be read is not calibrated at all**, rather
than treated as idle: the threshold is placed relative to a baseline, and without one there is
nothing to place it relative to.

**The measurement that matters is the ambient baseline, and it needs no load probe.** This is
where the probe-length trap is resolved rather than fought. `loadavg1` is a one-minute decaying
average, so a bounded probe cannot move it — but *reading it once is already a sixty-second
measurement*, of the preceding minute of the developer's real work, decayed and waiting in the
kernel. The window has elapsed before `calibrate` starts. It costs nothing and perturbs
nothing. Its one limitation is that a moment is not a day: a machine measured at 07:00 idle and
the same machine at 15:00 with Xcode indexing are three run-queue units apart
([climb-condition.md](climb-condition.md) §4.3 measured **2.91 per core** on this host
mid-work, against a settled 0.35). `calibrate` prints the reading and the time, records both in
the provenance, and says to re-run it mid-session.

**The probe measures something else entirely: effective cores, for `B_max`.** Throughput is
instantaneous and therefore *is* measurable in seconds, which is what separates it from the
run queue. `runtime.NumCPU()` reports 8 on a machine with four performance and four efficiency
cores; the probe measures parallel throughput over single-worker throughput and reports about
**6.1**, which is what that hardware physically predicts. It also catches, without enumerating
them, every other reason a core is not a core — SMT siblings, a noisy cloud neighbour, a quota
the cgroup reader missed. A calibrated `B_max` may only **lower** the per-tick
`floor(capacity / 1.2)`, never raise it, because that recomputation exists so a cgroup quota
changed under a running daemon is followed within the second.

**The probe's estimator is the maximum over repetitions, and that is a measured choice.**
Background work can only ever depress the ratio, so the noise is one-sided and the hardware's
true scaling is the upper envelope. Measured: a single three-phase reading, taken twelve times
as this host settled, ranged **1.89 to 6.05** — a 3.2× spread on a fixed property, with four of
twelve failing their drift check. The maximum over five, taken six times across background load
from 7.9 to 12.1, gave **5.70, 5.71, 6.31, 5.69, 6.53, 6.81**. The estimator that survives a
50% change in background load is the one measuring the machine. The probe is announced before
it runs, is about 3.8 s of load, and declines outright on a machine already loaded past 4.0 per
core, where it would be measuring the other workload.

**Not calibrated: the setpoint, and the EWMA window.** Both would be numbers no code reads. The
setpoint stopped being a control input when GOV-32 removed utilisation from the loop. The EWMA
window smooths CPU, which after the same change survives only as the diagnostic column `status`
and `tools/sweep` report — and the one alternative, smoothing the run queue instead, is worse
than leaving it alone, because `loadavg1` is already a one-minute average and the loop is
already the slower of the two on a rising edge. Writing either would be the exact failure GOV-32
named when it struck the setpoint from GOV-25's own acceptance criteria.

[calibration.md](calibration.md) carries the re-sweep of the threshold against this loop.

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

> **Superseded as to the trigger by §5.2 (GOV-15):** the floor fires at `serious` and above,
> not at `fair` — `signals.Thermal.Severe()`, which is what the daemon passes the loop. The
> paragraph above is the original rule, kept as written; §5.2 has the measurement that
> narrowed it.

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

### 5.4 Attributed cost — measured, and NOT built

**This section used to propose a predictive budget. It was measured in GOV-B03 and the
proposal was dropped; what follows is the negative result, kept because a design that was
tried and rejected is worth more written down than deleted.** The loop is AIMD over build
slots, §5.2, and the cost per slot is the constant `budget.SlotCores`.

The original text, withdrawn rather than edited, so the claim can be read as it was made:

> **5.4 Attributed cost (Phase 2 refinement, not v1).** Raw host CPU% cannot tell a build from
> Chrome. Sum CPU across the `claude` process tree and its children, divide by the count of
> confirmed leases, and you have a live estimate of the marginal cost of one more agent — so
> `B = headroom / cost_per_agent` predicts instead of reacting. Build this only after Phase 0
> shows the per-agent cost is real and measurable.

Two things happened to it. **Phase 0 withdrew its unit**: per-*agent* cost is not measurable on
this instrument at all — the sampler is host-wide and the machine never ran fewer than three
concurrent sessions (phase-0-findings.md §1). GOV-B03 re-scoped the section to cost per *build*,
per cost class, which is measurable. **Then the measurement refused it.** The evidence is
docs/bash-corpus.md §8; the four findings, shortest first:

- **The cost class explains 9% of the variance** in gated build cost (eta-squared 0.090). 91%
  lies within the class.
- **Within-class spread swamps between-class separation**: Heavy's p90 is 4.3x Medium's, but
  Medium's own p99/p50 is 8.0x and its cv is 1.22. The same shape §5.6a found for hold time,
  now confirmed for the quantity the classes exist to predict.
- **Per-class cost is non-stationary and it is not a mix shift.** Medium's mean moved 5.1 sigma
  between halves of the corpus, and the `go` family each moved 1.7–2.6x *on its own*. A finer
  grain does not escape it.
- **Sized correctly it reproduces the constant.** A saturation-protecting loop must size by the
  p90 (§5.5). Medium's measured p90 is 1.086 against `SlotCores` = 1.2 — B=7 against B=6 on
  this host. Sizing by the *mean* instead gives B=11, which is ~12 cores of expected peak on
  8 cores: the overshoot the proposal's own acceptance criteria forbade.

**The general lesson, which outlives this section.** The statistic that makes a predictive
budget look valuable is the mean, and a loop whose job is to avoid saturation cannot size by
the mean. Once it sizes by the tail instead, the live estimate and a well-chosen constant are
the same number, and the constant cannot oscillate. That is an argument against predictive
sizing *for this controller*, not against attribution as an instrument — attribution is what
produced these numbers.

**What survives as a real, smaller question.** Estimation *per command shape* is tractable for
`pytest` (cv 0.53) and, on spread alone, `xcodebuild` (0.40–0.55) — and intractable for
`go test`, which is the most common expensive shape, carries the corpus's 7.4-core maximum, has
a cv of 1.30 and drifted 3.4 sigma. So it is tractable where it is least needed. That is a
per-host correction of `SlotCores`, not a change to the control law, and it is backlog.

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
from what the sampler observes, was the design this pointed at.

**That table was measured and NOT built either. GOV-B14 was dropped on 2026-09-23.** It was the
residue of GOV-B03 (§5.4) and it failed for the same reason one grain finer down.
docs/bash-corpus.md §9 carries the measurement; the short form is that the table would have had
to be sized by the **p90**, and a shape's p90 is *less* stable than its mean, not more:
`go test` moved 3.90x between the corpus's halves with non-overlapping confidence intervals,
against an in-scope correction to `SlotCores` worth **9%** inside a noise band of 156%.

`Class.ExpectedPeakCores()` therefore remains a constant, and the classes remain a prior that is
never corrected from observation. **The one finding worth carrying forward is that `pytest`'s p90
is genuinely stable (1.06x across halves, a 9%-wide interval) and sits 28% below `SlotCores`** —
which cannot be collected by correcting a constant, because `B` is global and one `go test` burst
at ~4 cores a build would saturate a machine sized for pytest. Collecting it needs a lease that
costs what its shape costs, which is a change to this section's arithmetic rather than to its
numbers, and is filed as its own story rather than assumed here.

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

### 5.6a Does the cost class predict how long a slot is held?

**Only weakly, and that is why §6.2's queue does not schedule on it.** GOV-18 clause 4 required
this question to be answered from measurement rather than assumed either way. Re-collected over
22,820 `Bash` calls from 561 transcripts, classified by the shipped `internal/classify` (never
by a script — the reason is in `bash-corpus.md`), hold time by class:

| | n | p50 | p90 | p99 | max |
|---|---|---|---|---|---|
| ungated | 18,732 | 1.38 s | 5.05 s | 148.74 s | 1,898.95 s |
| **gated** | **4,081** | **5.58 s** | **55.85 s** | **326.75 s** | 1,240.38 s |
| gated, Medium | 3,243 | 4.48 s | 35.68 s | 239.88 s | 1,124.57 s |
| gated, Heavy | 838 | 17.82 s | 145.43 s | 602.22 s | 1,240.38 s |

`Light` is by definition everything *not* gated, so it has no row among gated calls.

The class does separate: Heavy's p90 hold is **4.1x** Medium's, in the same order as their
expected peak cores. But the separation is small against the spread *within* each class —
**53.6x** for Medium and **33.8x** for Heavy, p99 over p50 — and **18.5%** of Medium commands
outlast the *median* Heavy. A queue ordered on this would be ordering on a 4x signal buried in
30–50x noise, and would be wrong on nearly a fifth of the simplest comparison it could be asked
to make.

**The operational number, and the one §6.2 quotes as the cost:** **16.8%** of gated commands run
longer than a full 30 s parked wait — 11.4% of Medium, **37.6%** of Heavy. A session queued
behind one of those is refused rather than served, whatever order the queue is in.

Reproduced by `TestClassDurations` in `internal/classify`, which is skipped unless
`GOVERNOR_CORPUS` points at a corpus; the checked-in fixture carries no runtimes, so these rows
cannot be re-derived in CI and are recorded here instead.

---

## 6. Allocation

### 6.1 Fair share

Sessions register on `SessionStart` with a weight, default 1. Allocation is max-min fair:
every live session is guaranteed one slot before any gets a second, and unclaimed share is
redistributed to sessions actually asking.

**Built in GOV-18**, in `lease.Table.shareLocked`, as the second half of the admission rule.
Global capacity says whether the *machine* has room; fair share says whether *this session*
may have it. Both must pass, capacity first — capacity is one comparison and share is a walk,
so the walk is skipped whenever the machine is already full.

**Demand is revealed, not declared.** A session's demand is what it holds, and a session that
is *asking* demands more than it holds — unbounded. That second clause is load-bearing rather
than a nicety: reading the asker's demand as "what it holds plus this one request" puts total
demand at `sum(held) + w`, which reaches the budget at exactly the moment the capacity check
already refuses, so the share cap sits permanently in capacity's shadow and never decides
anything. The first implementation did that and its clause-3 test passed with fair share
entirely removed from the table.

A session **refused** within `demandWindow` (30 s, matching `maxParkedWait`) still counts as
contending once it is no longer the caller. Without that, the table only ever sees the demand
of whoever happens to be calling, and a session that frees a slot and immediately asks again
recaptures it before the session it was freed for is ever woken — starvation with a fair-share
rule bolted to the side. The mark decays rather than being cleared on success, because a
session taking slots in a build loop still wants the next one. `Fits` must **never** record
demand: the advisory channel (§8) is called speculatively, and if asking the question counted
as demand a model could inflate its own share just by asking.

**Admission is not preemption, and the guarantee is bounded accordingly.** A session asking
while the machine is idle is entitled to all of it — that *is* the redistribution sentence
above, not a loophole — and nothing claws those slots back when a second session appears. What
is guaranteed is that a session cannot **grow** past its share while another contends, so the
incumbent drains and the newcomer is served from freed slots. Starvation is therefore bounded
by the length of the builds already running, which is what §6.2's clause on duration has to put
a number on.

**Weight governs who refills, not who arrives first.** Two sessions ramping in lockstep from an
idle machine simply fill it first-come until capacity binds; neither has been refused, so
neither has revealed demand beyond what it holds, and there is nothing for a weighted cap to
act on. Weight starts deciding once somebody has been refused.

**One weighting mechanism, more than one trigger.** This sentence used to read "GOV-B02's focus
boost is the only thing that should ever move a session's weight", which GOV-B01 had to
contradict to ship `boost` at all. What it was guarding is worth keeping and is narrower than
what it said: there must be exactly one *mechanism*, `Session.Weight` as read by
`shareLocked`, and no second scheme layered beside it. Triggers may be plural.
`/build-governor:governor boost` (GOV-B01) sets a weight because a person said so; GOV-B02's
focus boost would have set the same field because the window came forward. Neither is a second
mechanism.

**The automatic trigger was measured, and NOT built. GOV-B02 was dropped on 2026-09-23.** The
mechanism keeps exactly one trigger, `governor boost`, and GOV-B01's decision not to defer the
manual trigger to GOV-B02 is retrospectively the right one — it would have deferred it to
something that never shipped.

The story's own bar was a claim about habit: the automatic trigger is worth building only if a
person who knows the watched session is starved fails to type `boost`. That is observable as
`deny ... cause=share` against the foreground session, and across the whole of `daemon.log`
there are **17 denials, every one `cause=capacity`, and zero `cause=share`**. No session's
weight has ever moved from 1.

So **fair share has never bound on this machine.** Every refusal observed so far was the machine
being genuinely full, which is capacity's job — and no weighting could have improved any of
them, because reweighting a full machine moves a refusal from one session to another rather than
removing it. The story was not merely unproven; it was aimed at a condition that has not
occurred. Dropping it also avoids the accessibility permission its own traps flagged as close to
the `sudo` constraint this design otherwise refuses (§5.3).

This is a statement about one 8-core laptop running one person's sessions, not about the idea. If
`cause=share` refusals do start appearing against a foreground session, the mechanism is already
built and only the trigger is missing: refile it then, with the log lines as the evidence.

**A session's FIRST slot is admitted over budget, and that is invariant 6 (GOV-34).** Global
capacity used to be checked before fair share with no knowledge of the guarantee above, so a
session holding *nothing* could be refused while another session held something — which is the
guarantee stated backwards. `lease.fitsLocked` now admits a request that does not fit when the
asking session holds nothing **and** the machine still has free room, bounded by two conditions
that each do real work: `t.weight < limit` means the escape cannot fire while an escape is
already outstanding, so at most one lease is over budget at a time and total weight never
exceeds `limit` plus one lease; `heldLocked(session) == 0` means a session gets one such
admission and its second request faces the full rule. Together they deliver invariant 6
*sequentially* — every session eventually gets its first slot, in the order §6.2's FIFO decides
— rather than seating every session at once, which on a four-slot ceiling is a promise the
machine cannot keep. A machine with nothing free is genuinely at capacity and still refuses:
there, waiting is what works, and the FIFO is what serves the waiter.

#### 6.1a The 2026-09-21 false-deny incident (GOV-34)

**Symptom.** From an unrelated session, a `PreToolUse` hook refused a `Bash` command five times
over roughly fifteen minutes with `Machine at capacity (1/4 build slots, thermal: nominal).
This command was not run.` One of four slots is not capacity. `governor status`, run between
two refusals, agreed with the message and not with the decision: budget 4, in use 1, waiting 0,
CPU 0.54, thermal nominal. The blocked session held no lease of its own. The session finished
its work on CI evidence alone because it could not run `docker compose`, `alembic` or `pytest`
locally at all.

**Cause — none of the three that were suspected.** GOV-34 listed an over-aggressive cut, a
lease-accounting error, and ungoverned sessions consuming invisible load. The evidence rules
out all three and identifies a fourth:

- *Not a cut printing the wrong number.* `daemon.acquire` reads one `limit` from `budgetNow`
  and passes that same variable to both `lease.Table.Acquire` and `refuseForCapacity`. There is
  no configured-versus-effective split on that path to get wrong; the budget in the message was
  already the effective one.
- *Not a small budget from a cut.* `~/.claude/governor/config.json` held `b_max: 4`, written by
  `governor calibrate` ("probe measured 5.37 effective cores of 8"), and `daemon.ceiling` takes
  `min(MaxSlots(8) = 6, b_max = 4)`. Four was this host's ceiling, not a throttle.
- *Not lease accounting.* The daemon's own log has the incident intact, and the arithmetic in
  it is correct throughout:

  ```
  23:45:34 budget park session=37045485-… slots=1.00/4 wait=20s
  23:45:54 budget park-expired waited=20.001s
  23:45:54 budget deny session=37045485-… reason=capacity cause=capacity slots=1.00/4 denials=1
  ```

- *The actual cause: the request's own weight, which the refusal never named.* `docker compose`
  classifies `Heavy`; `Heavy.ExpectedPeakCores()` is 4.5; `budget.SlotsFor(4.5)` is 3.75 slots.
  The admission rule is `t.weight + weight > limit`, so `1.00 + 3.75 = 4.75 > 4` refused —
  correctly, by its own arithmetic. **On a four-slot ceiling a Heavy build cannot be admitted
  beside any other lease at all, however small**, and the work-conserving escape only fired on a
  completely *empty* table. The other session held its one slot for ten minutes, so the blocked
  session was not waiting for a queue, it was waiting for a condition that `maxParkedWait` could
  not reach. Every retry restarted the same twenty-second park and hit the same table.

**Two defects, and they are separate.** The scheduling one is fixed by the escape above: a
session holding nothing is admitted while the machine has room. The reporting one is that
`Slots` and `Budget` were on the wire and `Asking` was not, so the renderer had two of the three
numbers the arithmetic needs and printed a sentence that contradicts itself inside its own
parentheses. A capacity refusal now names the weight the request asked for, uses a third head
(`Not enough free build slots`) whenever held is strictly below the budget, and — when the
budget is genuinely below this host's ceiling — names the signal cutting it, from the same
controller state `governor status` and `governor doctor` read. §4.2's rule that a refusal must
not state a fact the daemon did not send needed a companion: a refusal must not state a
*conclusion* the facts it sends do not support.

**Why the default budget was not raised.** Four was the calibrated ceiling for this host and the
binding constraint was never the budget's size — it was that a single request could exceed the
free room and had no path to progress. Raising it would have moved the same cliff to a
different pair of numbers.

### 6.2 Wait queue

FIFO across sessions. The next freed slot goes to whoever has waited longest, so a noisy
session cannot starve a quiet one.

**Built in GOV-08.** One queue for the whole daemon, not one per session, and the walk is
strict: it stops at the first waiter it cannot satisfy rather than skipping to one it can,
because skipping is how a session asking in a tight loop overtakes a session that has been
waiting. Waiters are woken on release, on session end, and on the reaper tick — the last
unconditionally, as a backstop, so a lost wake-up costs latency rather than a full `wait_max`
spent beside a free slot. The queue decides *order*; max-min fair share (§6.1) decides who may
join it.

**A fresh acquire may not jump the queue (GOV-18).** Because the pump stops at the first waiter
it cannot satisfy, a non-empty queue is always one whose *head does not fit* — so a caller
asking for less than the head would be granted immediately every time. An agent at 0.08 slots
would sail past a Heavy build at 3.75 on every attempt. That is the same starvation the strict
walk exists to prevent, arriving by the other door, so the fast path is closed whenever the
queue is non-empty. The uncontended case — an empty queue — falls straight through and pays
one mutex-guarded length read.

**The queue does not model duration, deliberately.** GOV-18 clause 4 offered the choice of
scheduling on GOV-14's cost classes or saying plainly that it does not. It does not, because
the classes predict *cost*, not *time*, and measuring them against the corpus says they predict
time only weakly — see §5.6a. Ordering a queue on a 4.1x signal buried in 33–54x within-class
noise would trade away the strict FIFO, which provably prevents starvation, for a guess that is
wrong on 18.5% of the simplest comparison.

**What that costs a starved session, measured:** 16.8% of gated commands run longer than a full
30 s parked wait (37.6% of Heavy ones). A session queued behind one of those is refused for
capacity rather than served, and retries under the deny cooloff. The exposure is bounded by
`maxParkedWait` and by nothing else. That is acceptable only because §6.1 caps what the
long-building session can hold in the first place: a long build occupies its own session's
share, not the machine.

**That bound did not hold for a session's first slot, and §6.1a is what it cost** (GOV-34).
"Starvation is bounded by the length of the builds already running" is true of the table and
false of what the session experiences: the gate refuses at `maxParkedWait` and tells the model
not to start further builds, so a session whose request is larger than the free room retries
into the same table until it gives up. Admitting a session's first slot over budget is what
puts a bound back on the case the queue could not reach.

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
// It is no longer merely carried: GOV-26 plumbed it from the gate through the daemon
// into every budget source, because a source governing an account rather than a host
// has no answer without it. The local source ignores it, as it should.

// -> acquire
{"v":1,"op":"acquire","principal":{"user":"peter","team":null},
 "session":"abc123","tool_use_id":"toolu_01...","depth":0,"wait_ms":20000}
// <- granted
{"v":1,"ok":true,"lease":"L-0d41","budget":6,"in_use":4,"share":2,"source":"local"}
// <- waiting (GOV-08: interim, sent immediately when the daemon parks the request;
//    exactly one grant or refusal then follows on the same connection)
{"v":1,"ok":false,"reason":"waiting","wait_ms":20000,"budget":6,"in_use":6}
// <- refused, machine genuinely full
{"v":1,"ok":false,"reason":"capacity","budget":6,"in_use":6,"cpu":0.94,
 "thermal":"fair","source":"local","denials":1,"cause":"capacity"}
// <- refused by FAIR SHARE (§6.1): the machine had room, it belongs to another session
{"v":1,"ok":false,"reason":"capacity","budget":6,"in_use":3,"slots":3,"cpu":0.51,
 "thermal":"nominal","source":"local","denials":1,"cause":"share"}
// <- refused, the machine had room and this request is bigger than what is left (§6.1a).
//    `asking` is the weight the admission rule used, and without it the message contradicts
//    itself; `ceiling` and `cut` say why the budget is below what this host could allow, and
//    are omitted when it is not. All three are additive — an older gate ignores them.
{"v":1,"ok":false,"reason":"capacity","budget":4,"in_use":1,"slots":1,"asking":3.75,
 "cpu":null,"thermal":"nominal","source":"local","denials":1,"cause":"capacity",
 "ceiling":6,"cut":"run queue"}
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
 "binary_version":"1.2.0","build":"9f2c…","sources":[{"name":"local","budget":6}],"sessions":[...],
 "pinned":false,"paused":false}
// -> control (the manual overrides of section 9.1). `control` and `limit` are NEW KEYS and
//    had to be: this connection's decode struct already carries `action` (the session verb)
//    and `slots` (the gate's lease weight, a FLOAT), so reusing either would have made every
//    `limit` decode as something else entirely. Third instance of the trap below.
{"v":1,"op":"control","principal":{"user":"peter","team":null},
 "session":"abc123","control":"limit","limit":2,"for_ms":1800000}
// <- the resulting state, not an acknowledgement: `resume` with nothing pinned and `limit`
//    against a daemon already at that number both succeed, and only the state tells them apart.
{"v":1,"ok":true,"budget":2,"source_budget":6,"pinned":true,
 "pinned_until":"2026-09-20T23:53:17Z","in_use":0}
// <- from a daemon that PREDATES this operation: the ordinary unknown-op ack. The client MUST
//    read it as "no answer" and say so, never as a refusal — during an upgrade there is a
//    daemon of one version and a command of another, and a person told "refused" goes looking
//    for the policy that refused them instead of restarting the daemon.
{"v":1,"ok":false,"reason":"unknown_op","in_use":4}

// -> advise (MCP request_build, section 8). READ-ONLY: it takes no lease, holds nothing
//    and changes no state, so a session may ask as often as it likes. The gate's own
//    acquire is the only thing that consumes budget.
{"v":1,"op":"advise","session":"abc123","kind":"build","class":"heavy","slots":2.5}
// <- the answer. `granted` is a BOOLEAN, not a count: the question is about one specific
//    command, so "3 of the 5 you wanted" has nothing to be the answer to.
{"v":1,"granted":false,"wait_hint_ms":20000,"reason":"capacity","budget":6,"in_use":6}
// <- from a daemon that PREDATES this operation. It is the ordinary unknown-op ack, in a
//    version the client speaks, and the client MUST read it as "no answer" rather than as
//    "no" — see the closed set of honoured reasons in section 8.
{"v":1,"ok":false,"reason":"unknown_op","in_use":4}
```

`source` and `sources` are plural because the budget is modelled as a *set* of sources
reduced by `min()`, with exactly one entry in v1. That plural is the entire cost of the
seam in section 12.

`binary_version` is the build running the daemon, and it answers a question `v` cannot: two
builds can both speak protocol v1 and still be different binaries. `governor install` reads
it to tell a current daemon from a stale one (section 13.4). It is additive — an older
reader ignores it — and it is absent from a daemon that predates it, which install reads as
"not this binary" rather than as "unknown".

`build` is the sha256 of the executable the daemon was started from, taken once at start-up
before the socket is bound (GOV-43). `binary_version` cannot tell builds apart when both call
themselves `0.0.0-dev`, or when a local build follows a release; `build` can. It is the same
digest `install` uses to decide whether it has anything to copy, so "same build" means one thing
everywhere. It is taken at start-up and never later on purpose: §13.4 installs by `rename(2)`,
which leaves the daemon on the old inode while its path names the new file, so a digest taken
lazily after an upgrade would describe the new binary and report the stale daemon as current.
`"unknown"` means the daemon could not read itself, and every reader treats that as no evidence.
**Absent** means a daemon older than the field, which is older than any binary able to ask, and
every reader treats that as stale. Additive; no protocol bump.

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

`cause` (GOV-18) says which half of the admission rule refused: `capacity` when the machine is
full, `share` when it had room belonging to another session. The `reason` stays `capacity` in
both cases **on purpose** — it is the closed set the gate honours (§4.2), and a new `reason`
value would be one an older gate must treat as an unreachable daemon and fail open, silently
ungoverning every share refusal for the length of an upgrade. Additive, so an older gate
ignores it and prints the sentence it always printed; a current one stops saying "Machine at
capacity" on a machine that is half empty.

**It is deliberately not called `share`.** The grant reply already sends `"share": 2` as an
*integer*, and the gate decodes grants and refusals into one struct — a boolean of the same
name makes every **grant** fail to decode, and a gate that cannot decode a reply fails open.
This is the same trap the acquire's `slots` key documents against `weight`. A key is only free
if no other message on this connection already uses it with a different type.

**GOV-B01 is the third instance, and it is why `control` does not reuse `action` or `slots`.**
The obvious names for "pin the budget to N" were `action: "limit"` and `slots: 2` — and the
daemon decodes every operation on this connection into ONE struct, where `action` is already
the session verb with a disjoint vocabulary and `slots` is already the gate's lease weight as a
**float**. A `limit` sent that way decodes as a lease request for 2 slots, which is not an error
anything reports. Three times now, so it is a rule rather than an anecdote: **a key is free only
after checking every other message on this connection, and a new field is guilty until then.**


The daemon MUST tolerate a malformed line by closing that connection only. One bad client
never takes down the broker.

---

## 8. Advisory channel

The enforcement hook alone produces a bad experience: the model starts a build, gets refused,
and burns tokens learning what it could have been told. So the plugin also ships a stdio MCP
server with one tool:

```
governor__request_build(command: string, why: string)
  → { granted: bool, wait_hint_ms: int, reason: string }
```

**The ask is per COMMAND, not per batch, and that is not a cosmetic change from the original
`request_agents(wanted)`.** Phase 0 moved the controlled variable from agent count to build
concurrency (§5.5), and the two questions have different answers in shape, not just in value.
"How many agents may I run" is answered by a smaller number. "May I run `cargo build
--release`" cannot be: there is no fractional build, so the useful answer is usually *not
now, in about twenty seconds*. `wait_hint_ms` carries that and is the field a model can
actually act on.

**One classifier, one answer.** The tool classifies with `internal/classify` — the same pure
function the gate calls, in the same process image — so a command the tool calls cheap is a
command the gate will not lease, by construction rather than by agreement. A session told
"go ahead" and then parked by the gate would be worse than no advice at all, because it
would teach the model to stop asking.

**A cheap command costs no round trip.** 81.6% of `Bash` calls are not build-shaped (§5.5),
and the classifier answers those with no socket, exactly as the gate does. A tool that is
slow on `ls` is a tool that gets worked around.

**It degrades open, for the gate's reason.** With the daemon unreachable — no socket, no
listener, a listener that never answers, a reply this binary cannot read, a protocol version
it does not speak — the tool answers `granted: true`. Invariant 1 is not weaker here than on
the hot path: advice that fails closed would stall a session that was trying to cooperate.

**The set of refusals it honours is closed**, exactly as the gate's is (§4.2), and for a
failure that is not hypothetical: a daemon from before this operation existed answers
`{"ok":false,"reason":"unknown_op"}` — healthy, in protocol v1, and indistinguishable from a
refusal to anything that does not check the reason. Read as a "no" it refuses *every* build
for the length of an upgrade, silently, while the machine is in fact idle. So only `capacity`
and `depth` become a refusal; every other reason is treated as an unreachable daemon. This
was caught by pointing the finished server at the daemon actually running on the development
machine, which is why it is written here rather than discovered by a user mid-upgrade.

**Advice steers; the hook enforces.** The advisory channel exists to make the common case
pleasant, not to be trusted — a model that *can* ask will sometimes forget, so the budget is
enforced underneath it regardless. Note that the server is a stdio child of the session and
is therefore inside `claude_cpu`, unlike the servers measured in phase-0-findings.md §5, and
that it is started at session start whether or not it is ever called
(hook-surface.md §20.4). It stays cheap: no sampler, no polling, no goroutine that outlives
a call.

The name a session actually sees is
`mcp__plugin_build-governor_governor__governor__request_build` — Claude Code prefixes
`mcp__plugin_<plugin>_<server>__` onto the tool's own name rather than replacing it, so
`governor` appears three times. That is measured, not composed (hook-surface.md §20.5); the
obvious guess is wrong and produces a permission denial that reads like a broken server.

**Three channels carry the advice, and they are not interchangeable.** The tool above is
useless to a session that does not know it exists, and what a session knows *before* it decides
to run a build is not the same as what it can go and read afterwards. Measured in
[hook-surface.md](hook-surface.md) §16.1 and §21.2:

| Channel | When the model sees it | What belongs there |
|---|---|---|
| `SessionStart` `additionalContext` | every session, unconditionally | the standing instruction: call the tool before an expensive build, and **not** before ordinary shell work |
| the bundled skill's `description` | resident, from session start | one line saying the skill exists and when to reach for it |
| the bundled skill's **body** | only after the skill is invoked | the detail — how to read `wait_hint_ms`, what to do on a refusal |

**A standing instruction cannot live in the skill body.** §21.2 measured progressive
disclosure directly: a marker in the frontmatter description was in context with no tools
available, and a marker in the body was invisible until the model invoked the skill. A model
that has to invoke a skill to learn it should have asked has already run the build.

The skill's name as a session sees it is `build-governor:heavy-build` — colon-separated, and
prefixed with the **manifest `name`** rather than the directory name, which is a different rule
from the MCP tool's underscores above (§21.2, answer 4). Neither name can be composed from the
other; both are measured.

**All three shipped with GOV-17** — `sessionStartAdvice` in `cmd/governor/advice.go`, and
`skills/heavy-build/SKILL.md` written by install from `pluginSkill`. The instruction says what
*not* to ask about as plainly as what to ask about, makes "unsure" resolve to not asking (the
gate is underneath either way), and says the answer decides **when** a build or test runs and
never **whether** — the counter-metric below, written into the advice itself.

**They ship as a convenience, not as a proven saving.** The success metric this section
proposed — denials per hour falling with the budget unchanged — was measured and could not be
scored on this machine: denials, the peak and finally `p99` child cores each failed the
pre-registered halves test, the last by twice its bar over a 32-day baseline. GOV-17 closed
clause 2 as unscoreable on 2026-10-24 rather than run an after-window that could only say
"cannot tell"; [advice.md](advice.md) carries the figures. Nothing here is evidence that the
advice lowers load, and nothing here is evidence that it does not.

**And a counter-metric, which is not optional.** Denials per hour falls to zero if sessions
stop building at all, and that is a worse machine, not a better one — the metric above would
report it as a complete success. So the gate records every gated build and what became of it
(`trace.Build`, record schema v3), including the ones that RAN, which `daemon.log` deliberately
does not: it writes only when the budget moves or binds, and a session that quietly stops
running its tests moves nothing. Test-suite runs are counted from the build records and a drop
in them falsifies the story regardless of what denials do.

The instrument, its known defects and the reading rule fixed in advance are in
[advice.md](advice.md). The rule is written before the window opens, for the reason
[dogfooding.md](dogfooding.md) gives: a week of impressions is not the evidence this repo
accepts anywhere else.

**The instrument is strictly downstream of enforcement.** The build record is written after any
refusal has reached stdout, never before, because `trace.Append` opens a file and an open can
block — and a write ordered ahead of the deny would turn a wedged disk into a governor that
silently grants everything. The accepted cost of that ordering is that a gate whose budget
expires mid-write loses the line, so a window's build count is a floor rather than an exact
figure. Measured cost of the write: 29.5 us, against a 100 ms budget, on the 18.4% of `Bash`
calls that are gated and already pay a round trip.

---

## 9. Package

```
claude-governor/                  the repo
├── install.sh                    curl-able bootstrap: fetch binary, then `governor install`
├── cmd/governor/                 one static binary
│   ├── daemon gate confirm release session status mcp statusline
│   ├── install uninstall doctor calibrate version
│   └── trace                     the measurement log: Phase 0 traces, and GOV-17's builds
├── internal/
│   ├── transport/                endpoint URL → net.Conn. unix:// in v1, tcp:// later
│   ├── budget/                   Source interface; LocalSource in v1; reduced by min()
│   ├── lease/                    table, TTL, Liveness interface (PID+starttime in v1)
│   ├── signals/                  Sampler interface; sampler_darwin.go, sampler_linux.go
│   └── config/                   versioned schema, migration, atomic write
├── skills/heavy-build/SKILL.md   the advice's detail (sections 8, 21.2) — GOV-17 clause 1
└── commands/governor.md          status | limit N | pause | resume | boost (section 9.1)

~/.claude/skills/build-governor/ the installed plugin, written by `governor install`
├── .claude-plugin/plugin.json    name, version, description
├── hooks/hooks.json              the six hooks in section 4.1
├── .mcp.json                     the advisory server in section 8
├── skills/heavy-build/SKILL.md   the bundled skill (section 8)
├── commands/governor.md          the slash command (section 9.1)
└── bin/governor                  the binary those hooks run

state  → ~/.claude/governor/      sock, daemon.pid, daemon.log, daemon.start, statusline.json,
                                  statusline.json.*, onboarded, surface.json, surface.json.tmp,
                                  config.json, samples.jsonl, sampler.lock, sampler.log
config → ${CLAUDE_PLUGIN_DATA}/config.json, falling back to the state directory
```

**The installed tree is SIX files**: `.claude-plugin/plugin.json`, `hooks/hooks.json`, `.mcp.json`,
`commands/governor.md`, `skills/heavy-build/SKILL.md` and `bin/governor`. It was five until GOV-17
shipped the skill on 2026-10-24, and until then this section carried a `NOT YET SHIPPED` marker
on that line, because a layout diagram had already once drifted ahead of the code (§13.3,
2026-09-23). GOV-35 asserts this list against a real install in a clean container,
which is what stops a layout diagram from being the only thing that knows.

**That assertion is `scripts/clean-install-test.sh`, run by CI on every push** against
`debian:stable-slim` (glibc, coreutils) and `alpine:3` (musl, busybox). It builds both linux
artefacts from the working tree and serves them over TLS through `install.sh`'s own
`GOVERNOR_BASE_URL`, so what is installed is the commit under test rather than the last release,
and `curl --proto '=https'` is never relaxed to make a test pass. In the container it asserts
the six installed paths, that `skills/` holds nothing but the one skill, that `doctor` reports a named set
of `ok` checks and **exactly** `sessions` and `spawn` as unverified, that a reinstall over a
running daemon does not kill it (§13.4), and that `uninstall` leaves `$HOME` byte-identical.

Three things it found that no test on the developer's machine could:

- **A fresh machine has no `settings.json`, so install CREATES one** — the mirror image of the
  merge-and-back-up path §13.3 describes. Uninstall must then *remove* that file rather than
  restore a backup of something that never existed. It does, and now that is proven rather than
  assumed.
- **`doctor` has TWO unverifiable checks where there is no Claude Code, not one.** `spawn` was
  expected; `sessions` is the second, because the session registry is filled by `SessionStart`
  hooks firing in real sessions and a container has none. The set is asserted by name rather
  than by count, because GOV-30's hole was a check that could not verify being counted as fine.
- **A digest that cannot see the disk reports success.** The first version listed directories
  with `find -printf`, which busybox rejects, with the error swallowed — so on alpine the
  directory half was empty and a leftover directory would have compared equal. The script now
  uses only POSIX `find`, and checks its own instrument: the digest taken after install MUST
  differ from the one before it, or the byte-identical comparison at the end is meaningless.

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

`.mcp.json` sits at the plugin root and registers `governor mcp` as a stdio server (section
8). That a **skills-dir** plugin loads it at all was measured for GOV-16 rather than assumed
— hook-surface.md §20 — because a plugin's MCP registration is a different component from its
hooks, and §15 had only ever measured hooks. The server it names is
`${CLAUDE_PLUGIN_ROOT}/bin/governor mcp`: the same one binary, which is what makes a server
started at every session start (§20.4) affordable.

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

GOV-43 added a clause that joins the count rather than replacing it: `1/4 slots - stale
daemon: governor install`, when the daemon's `build` (§7) differs from the binary rendering the
line, or is absent. The old daemon is still governing, so the count is true. What is false is
the belief that the installed version is the one doing it, and on 2026-10-31 that belief had
been false for 40 days. The clause comes second so a narrow terminal drops cpu and thermal
first. The status line hashes its own executable on a cache miss and keeps the digest in
`statusline.json`, keyed by path, size and mtime, so the hash is paid once per binary rather
than once per second.

**It reports and never restarts, and that was a decision.** A restart-on-mismatch rule would
interact with the lazy-start cooldown, and with two installs coexisting (a dev build in
`~/.local/bin` and the plugin's) it would have each session's line kill the other's daemon in
turn, dropping every live lease each time. A rule that flips between two builds is worse than
saying so.

**And a restart from the gate is, today, a restart without calibration (GOV-44).** The only
way a hook can start a daemon is `lazystart.go`, and a daemon started that way inherits
`CLAUDE_PLUGIN_DATA` from the hook. `config.Path()` honours that variable and
`transport.DefaultEndpoint()` does not, so the daemon binds the right socket but reads
`$CLAUDE_PLUGIN_DATA/config.json` rather than the file `governor calibrate` wrote. Measured
2026-10-31: install's daemon logged `run-queue threshold 4.72, calibrated for this host`; a
lazy-started one logged no calibration line and served ceiling 6 against a calibrated `b_max`
of 4. A restart-on-mismatch rule would swap a stale but calibrated daemon for a current but
uncalibrated one. The fix line is therefore `governor install`, which starts the daemon in the
install's environment and does load calibration. Revisit this decision once GOV-44 lands; until
then a hook restarting the daemon is a regression. The gate is not involved either: hashing a binary on every spawn is hot-path cost,
and the gate's only channel to a person is a deny.

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

**The 50 ms budget is adequate for the round trip, and GOV-B17 measured it under load rather
than assuming it.** The 0.6 ms above is an idle figure, and the question the budget actually
has to survive is a busy machine, because that is when a person looks at the line. 300 real
dial-plus-reply round trips to a live daemon over a unix socket, on this eight-core host:

| | p50 | p90 | p99 | max | over the 50 ms budget |
|---|---|---|---|---|---|
| idle | 177 µs | 208 µs | 297 µs | 2.14 ms | 0 of 300 |
| under a concurrent `-race` suite | 434 µs | 1.14 ms | 3.69 ms | 11.78 ms | 0 of 300 |

Loaded, the p99 is still **13.5x inside** the budget and the worst single observation is 4x
inside it, so the wire is not what spends the 50 ms. What does, when it is spent, is the
statusline process not being scheduled at all — the same starvation GOV-B10 measured from the
other side, where under `GOMAXPROCS=1` with four spinners a 50 ms sleep took 68 ms. That
distinction matters because the two have opposite fixes: a slow daemon would be a reason to
raise the budget, whereas a starved process is a reason not to, since a machine that cannot
give this command 50 ms of wall clock will not reliably give it 500 either.

The consequence is honest and worth stating: on a machine that starved, the line reads
`governor: daemon not running - spawns ungoverned` while the daemon is in fact running and
governing. That is the fail-open direction and it is the right one — the alternative is
printing a slot count nobody measured — but it is a misleading sentence, and if it is ever
seen in practice rather than in a test, the fix is the wording, not the budget.

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

### 9.1 The slash command

`/build-governor:governor` is the manual override, and design.md carried it for a long time
as `/governor status | limit N | pause | boost` without anything having been measured. GOV-B01
measured it ([hook-surface.md](hook-surface.md) §22) and three of those words were wrong.

**The name is `/build-governor:governor`.** A plugin command is advertised as `<manifest
name>:<file basename>`, so the bare `/governor` this document illustrated for months is not a
name that exists. It is not a choice — shortening it means renaming the plugin, which moves the
installed path, `${CLAUDE_PLUGIN_DATA}` and every §13.3 row for a cosmetic win.

**It is a prompt, not an entry point.** A command file is expanded into the model's context; it
cannot *be* the implementation. So every verb below is a real subcommand of the binary, and the
command file's job is to run it and show the result. That division is what keeps the override
usable from a script and from `governor` directly, not only from inside a session.

| Verb | Subcommand | Does |
|---|---|---|
| `status` | `governor status` | budget, in use, waiting, CPU, thermal, per-session shares |
| `limit N` | `governor limit N [for D]` | pin the budget at N slots until it expires or is released |
| `pause` | `governor pause [for D]` | stop gating without stopping the daemon |
| `resume` | `governor resume` | release a pin or a pause early |
| `boost` | `governor boost [for D]` | double this session's weight (§6.1) |

**Every override expires.** A pinned limit and a pause both carry a deadline, default one hour,
and the status line says which is in force and how long is left. This is the whole of GOV-B01's
trap and it is not a nicety: a manual override that silently outlives the reason it was set is
how a governor stops being trusted, and the person who set it is by then in a different
context and has no way to know it is still on. Nothing persists across a daemon restart either
— the override lives in memory beside the lease table, for the reason §12.2 gives for the
leases themselves.

**The status line says so while an override holds,** and that is the half of this story that
makes the other half safe. A pinned budget is otherwise indistinguishable from one the control
loop chose — the same number in the same place — so the line gains a clause:

`2/2 slots - pinned`

a boosted session says so too, because it is the override that changes nothing anyone can see
— only who wins when two sessions contend, which is invisible from inside either of them:

`1/6 slots - boosted`

and a pause replaces the slot count entirely, for GOV-29's reason and not a new one:

`governor: gating paused`

The count is what would be misread during a pause — it reads `2/4` on a machine that will admit
a fifth and a hundredth — so the count is what goes. Both clauses carry the time remaining when
there is one — the word pinned followed by how long is left — which is deliberately not quoted
here, because a countdown is not a thing a document can illustrate without going stale on the
next tick. `TestTheOverrideClausesNameWhenTheyEnd` holds that part instead.

**The command body uses `$ARGUMENTS`, never `$N`, and its `!`-lines start at column zero.**
Both are §22 findings rather than style: `$N` resolves to the (N+1)th word and leaks the
literal text `$N` when out of range, and a `` !`…` `` that does not begin its line is passed to
the model uninterpreted — which in one probe produced a model reporting the text of a command
as that command's output. On a surface whose entire value is that its numbers are real, that is
the failure worth engineering against, and it is why the command declares `allowed-tools`
covering its own binary: an uncovered pre-execution is refused visibly and substitutes nothing,
leaving the model with an error rather than with something shaped like an answer.

---

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

**No longer on this list, measured 2026-09-16 on 2.1.273 for GOV-17** (`hook-surface.md` §21):
a skills-dir plugin's bundled `SKILL.md` **is** surfaced to the model; its frontmatter
`description` is resident in context from session start while its **body** loads only on
invocation; and the name a session sees is `<manifest name>:<skill name>`, colon-separated and
taken from `.claude-plugin/plugin.json`'s `name` rather than from the directory. §8's channel
table is the consequence.

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
| Identity | `principal{user, team:null}`, carried and handed to every budget source | resolved against an account | `internal/identity`, protocol field from v1 |
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

### 12.3 What proving the seams found

GOV-26 ran the experiment: `cmd/governor/tierseam_test.go` stands up a stub broker on
`tcp://`, adds it to the daemon's source slice, and drives the whole path with a real gate.
Everything in that file is behind the `_test.go` suffix, so the compiler is what keeps it
out of the released binary rather than a reviewer's memory.

**Two of the six seams did not hold.** Both were invisible while every source answered from
memory, which is the case the rule in 12.2 is least able to protect against: an interface
with one implementation abstracts whatever that implementation happens to do.

1. **Identity was a field, not a path.** `principal` had been on the wire since v1 and
   `gate.go` said outright that nothing read it. That was true the whole way down — the
   daemon decoded `request.Principal` and never referred to it again, and `budget.Source`'s
   only method took a context and nothing else, so there was no parameter a principal could
   have travelled in. A remote source governing an account's tokens or spend cannot answer
   "how much budget" without being told "for whom", so the seam was one type signature short
   of useless. `Source.Quota` now takes the asking principal; the local source ignores it,
   because this host's CPU is not divided by who wants it. The type moved to
   `internal/identity` so a package below `cmd/governor` can name it, and `cmd/governor`
   aliases it rather than converting — one definition of the wire shape, no conversion to
   forget.

2. **"A slow source is skipped, never waited on" rested on a deadline nobody set.**
   `budget.Source`'s doc comment promised it; all four `budget.Min` call sites in the daemon
   passed `context.Background()`, which carries no deadline at all. A broker that refuses a
   connection was always survivable — `Min` skips an erroring source, and that path worked.
   A broker that *accepts and then goes quiet* would have hung the daemon forever, on the
   hot path, with no timeout anywhere between the gate's 100 ms budget and the heat death of
   the universe. `Min` now imposes `budget.MaxWait` (50 ms) as a ceiling when the caller has
   named no tighter deadline of its own. Defending inside `Min` rather than at four call
   sites means a fifth call site added later cannot reintroduce it.

   The worst instance was `pump`, which read the budget **inside** its walk over the wait
   queue while holding that queue's lock — and every fresh acquire consults the same queue
   (6.2). A silent remote would therefore have been dialled once per waiter, under the lock,
   blocking admission for the whole machine in the one path whose job is to unblock it. The
   budget is now read once per pump and outside the lock.

**The other four held.** `transport.Dialer` is `(ctx, endpoint string) -> net.Conn` and a
`tcp://` implementation drops straight in with nothing above it changing, which is the
question that seam existed to answer. The budget slice, the `Liveness` interface, `Sampler`
and the config schema all took the second source without alteration. Notably, adding the
remote source really is the one line 12.1 promises: `d.sources = append(d.sources, ...)`.

**What was NOT built, and must not be:** no config key for a remote endpoint, no `tcp://` in
the shipped `transport.Dial`, no second `Dialer` outside a test. `TestTheStubShipsInNothing`
asserts the first two directly. Tier two is not justified until the local governor has been
in daily use and the token and spend problem is actually felt.

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
  no constant compiled in from Peter's laptop ships as anyone else's default. It runs at the
  end of `install` and can be re-run by hand — and re-running it *while actually working* is
  the documented advice, because the quantity it measures is what the machine is already
  doing. See §5.2a.

### 13.2 Command surface

| Command | Does |
|---|---|
| `install.sh` | curl-able bootstrap: detect OS/arch, fetch the matching binary, hand off to `governor install` |
| `governor install` | write the plugin (manifest, hooks, binary), merge the statusline into settings (after backup), start the daemon, print exactly what it touched, then run `doctor` and exit non-zero if it fails |
| `governor uninstall` | reverse all of it, restore the settings backup, stop the daemon, remove state on `--purge` |
| `governor plugin-path` | print the installed plugin tree and exit 0, or exit non-zero with nothing on stdout when there is none. It is what the marketplace entry runs (below); it stats two files and does nothing else, because Claude Code runs it on every session start |
| `governor doctor` | daemon reachable? hooks registered? matcher matches the real tool name? **are the sessions running right now actually governed?** binary and plugin versions agree? config schema current? each failure names its fix |
| `governor calibrate` | measure cores, performance cores, RAM, this host's ambient run queue and a short load probe; write the run-queue cut threshold and B_max for *this* machine, with a provenance note |
| `governor version` | binary version, protocol version, config schema version |
| `governor limit N` / `pause` / `resume` / `boost` | the manual overrides of §9.1, each expiring on its own |

GOV-20 shipped `doctor`, and it reports eleven findings rather than the six checks this table
lists — `binary`, `protocol` and `config` are the three halves of "versions agree", split
because they have three different fixes and a line carrying three fixes is a line nobody acts
on; `strays` is GOV-B11's, below. Six details are worth stating because they are decisions
rather than implementation:

- **`binary` compares bytes, not just version numbers**, because on a development build the
  numbers are the same for every build ever made. GOV-B13; §13.4 has the reasoning, what the
  omission cost, and the one case the check cannot answer.
- **`build` compares the RUNNING DAEMON with the binary the hooks run**, which neither `binary`
  (two files on disk) nor `protocol` (the wire version) does. GOV-43: a daemon 40 days older
  than the install passed every other check. It fails on a different digest and on an absent
  one, since a daemon with no `build` field is older than the field, and its fix is `governor
  install`, which replaces exactly that daemon (§13.4). A daemon reporting `"unknown"` is
  `unknown`, not a failure.

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

### 13.2a The marketplace route, and why the plugin is `build-governor`

Measured in [hook-surface.md](hook-surface.md) §23; this is what follows from it.

**One tree, two registrations.** `governor install` is the only writer of the plugin. The
marketplace entry in `.claude-plugin/marketplace.json` is a `command` source running `governor
plugin-path` in link mode, so a marketplace install loads the very tree install wrote, through a
cache directory of symlinks into it, and runs the one binary. A copy source could not work: a
marketplace plugin's files come from git and `bin/governor` is in no repository. Homebrew
supplies the binary on `PATH` for that command; `install.sh` does the same through
`~/.local/bin`. The marketplace route is therefore `brew install` then `governor install` then
`claude plugin install build-governor@claude-governor`, with the last step accepted by a person
in their own terminal — Claude Code refuses to accept a command source from inside a session.

**Both registrations cannot double-load, and that rests on one name.** Claude Code loads one
plugin per manifest name: the marketplace copy takes precedence and the skills-dir copy is
listed "Not loaded". Because both point at the same files, the shadowed row is cosmetic. A
marketplace copy that is NOT a link to this tree would take the hooks away from it, so `doctor`
fails the hooks check on any `build-governor@<marketplace>` whose `sourceProducerPath` is not the
installed tree, and names the `claude plugin uninstall` that removes it.

**The rename.** Claude Code reserves the `claude-` prefix for plugin names (§23.5), so the plugin
is `build-governor`. The repository, the binary, the project and the marketplace keep their
names. Because precedence is keyed on the name, a machine still carrying
`~/.claude/skills/claude-governor/` would load both plugins — §23.6 saw it happen — so install
removes that tree, uninstall removes it, and `doctor` fails while the gate is registered in more
than one file. The old tree is removed only when it is provably ours: its manifest says
`claude-governor` and it holds a governor binary.

**The marketplace registration is not ours to remove.** It lives in Claude Code's own files
(`installed_plugins.json`, `settings.json`'s `enabledPlugins` and `extraKnownMarketplaces`, the
plugin cache). `governor uninstall` names it and the command that removes it rather than
editing those files, and the byte-identical uninstall guarantee is stated for the `install.sh`
route. Left behind, the registration is inert: `plugin-path` finds no tree and exits non-zero.

### 13.3 What touches the user's machine

Uninstall is only verifiable if this list is exhaustive, so it is normative.

| Path | Written by | Removed by |
|---|---|---|
| `~/.claude/skills/build-governor/.claude-plugin/plugin.json` | install | uninstall |
| `~/.claude/skills/build-governor/hooks/hooks.json` (the six hooks) | install | uninstall |
| `~/.claude/skills/build-governor/.mcp.json` (the advisory server, section 8) | install | uninstall |
| `~/.claude/skills/build-governor/commands/governor.md` (the slash command, section 9.1) | install | uninstall |
| `~/.claude/skills/build-governor/skills/heavy-build/SKILL.md` (the advice's detail, section 8) | install | uninstall |
| `~/.claude/skills/build-governor/bin/governor` | install, copied from the running binary | uninstall |
| `~/.claude/skills/` | install, only if absent | uninstall, only if it empties |
| `~/.claude/settings.json` (`statusLine` key only) | install, merged | uninstall, previous value restored from backup |
| `~/.claude/settings.json.governor-backup-<ts>` | install, only when a settings file existed | uninstall, unless `--keep-backup` |
| `~/.local/bin/governor` | install, **only if `~/.local/bin` already exists** and nothing else is at that name | uninstall, only if it still links to our binary |
| `~/.claude/governor/sock` | daemon | uninstall |
| `~/.claude/governor/daemon.pid` | daemon | uninstall |
| `~/.claude/governor/daemon.log` | install, when it starts the daemon | uninstall |
| `~/.claude/governor/statusline.json` | `statusline`, when the directory already exists | uninstall |
| `~/.claude/governor/statusline.json.*` | `statusline`, staging the file above before the rename | uninstall, **by pattern** — see below |
| `~/.claude/governor/daemon.start` | the gate, when it lazily starts a daemon and the directory already exists | uninstall |
| `~/.claude/governor/onboarded` | `session start`, only in the fallback below, and only when the directory already exists | uninstall |
| `~/.claude/governor/surface.json` | daemon, when it first sees a spawn tool name it has not recorded for that Claude Code version (GOV-30) | uninstall |
| `~/.claude/governor/surface.json.tmp` | daemon, staging the file above before the rename | uninstall |
| `~/.claude/governor/config.json` | `calibrate`, **only** when `${CLAUDE_PLUGIN_DATA}` is unset — see below | uninstall |
| `~/.claude/governor/` | daemon (`transport.Listen`) | uninstall, only once it is empty; `--purge` regardless |
| `~/.claude/plugins/data/build-governor-skills-dir/` (`${CLAUDE_PLUGIN_DATA}`: config, onboarding marker) | Claude Code creates it; the governor writes config.json and `onboarded` | uninstall `--purge` only |
| `~/.claude/plugins/data/build-governor-claude-governor/` (`${CLAUDE_PLUGIN_DATA}` when the plugin loaded through the marketplace entry) | Claude Code creates it; the governor writes the same two files | uninstall `--purge` only |
| `~/.claude/plugins/data/claude-governor-skills-dir/` (`${CLAUDE_PLUGIN_DATA}` under the plugin's name before the rename) | an install from before GOV-19 | uninstall `--purge` only |
| `~/.claude/skills/claude-governor/` (the whole tree an install from before the rename wrote) | an install from before GOV-19 | **install**, and uninstall — only when its manifest is named `claude-governor` and it holds a governor binary |

**The skill's row was removed on 2026-09-23 and restored on 2026-10-24, and both halves are
worth keeping.** It was removed because nothing wrote it — left over from planning GOV-17's
clause 1 — and a phantom row makes this table unusable as the oracle a clean-machine uninstall
proof (GOV-35) checks against, exactly as an omission does. It came back in the same change
that made install write the file, which is this document's standing rule for every path.
`TestSectionThirteenThreeNamesTheSkill` now holds the row to the installer, and
`scripts/clean-install-in-container.sh` asserts it against a real install.

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
- **`statusline.json.*` IS that residue, and it happened.** The bullet above describes the
  hazard in the abstract; the statusline walked into it. `writeCache` staged through
  `os.CreateTemp`, and because the statusline enforces its budget by *abandoning* its worker
  goroutine, a process could exit between `CreateTemp` and `Rename` — at which point the
  `defer os.Remove` in that goroutine never runs. One machine accumulated **133** of them over
  six days, and `~/.claude/governor/` could not empty for as long as they sat there. GOV-33
  fixed the cause by moving the write off the abandonable path (§9), and added `stateGlobs`
  so uninstall sweeps by pattern as well as by name. **Both halves ship, deliberately.**
  Uninstall is the promise that makes running a hook on every subagent spawn a reasonable ask,
  and it must not rest on every writer having been correct for the life of the install — a
  machine that accrued residue under the old binary still has to come clean under the new one.
  A new random-suffixed path therefore goes in `stateGlobs` and in this table, not in
  `stateFiles`.
- **`samples.jsonl` survives a plain uninstall**, per the Phase 0 table below. It is the
  measurement the setpoint is derived from and it is not reconstructable.
- **GOV-43's build check adds no row.** The daemon digests its own executable in memory and
  reports it in `status`. `doctor` digests the hooked binary and records nothing. The status
  line keeps its own digest in a field of `statusline.json`, a file already in the table.
- **GOV-B13's staleness check adds no row.** `doctor` now digests
  `~/.claude/skills/build-governor/bin/governor` and the binary running the check, and
  writes neither — no cache, no stamp, no recorded digest. The row above is unchanged and
  uninstall has nothing further to remove. It is stated rather than left silent because this
  table is what makes uninstall verifiable: "a check was added and this list did not change"
  is a fact a reader has to be able to confirm without re-reading the check.
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

**So `config.json` has two rows in this table, and a plain uninstall removes exactly one of
them** — the state-directory copy, on the same rule `onboarded` follows: in a plugin context
your calibration survives an ordinary uninstall and only `--purge` discards it, because
reinstalling is the same machine and re-measuring it is work you already did; outside one there
is no such directory and it goes with everything else.

GOV-25 is why both rows exist. Before it, `install` wrote no config at all, so the state-
directory path was reachable only by running `calibrate` by hand and its absence from
`stateFiles` cost nothing. The moment install started calibrating, an uninstall left
`config.json` behind and `~/.claude/governor/` could never empty — and every existing test
stayed green, because `TestUninstallLeavesTheMachineByteIdentical` digests the home directory
and that fixture keeps the state directory outside it. `TestUninstallEmptiesTheStateDirectory`
is the one that now covers it, asserted against a listing rather than against `stateFiles`, so
a file missing from both cannot pass.

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

**It is written after the line is printed, not while it is being rendered, and that ordering
is load-bearing (GOV-33).** The budget in this section is enforced by returning while the
worker goroutine is still running; the process then exits, and a goroutine abandoned that way
never runs its deferred cleanup. Staging a temp file inside it therefore leaked one file per
cut-off invocation — 133 on one machine in six days, each of them a reason
`~/.claude/governor/` could not empty on uninstall. So the renderer now touches no file at
all: it returns the reply to cache, and the command writes it once the line is out. The
invariant to preserve is not "clean up the temp file" but **nothing on the abandonable path
opens a file**, because only that holds no matter which line the process is killed on. It
also puts the disk after the reader, the same ordering `trace.Append` takes on the gate's hot
path and for the same reason: a wedged disk must not delay the thing the user is waiting for.
The cost is that the cache write is outside the budget, which is acceptable precisely because
the render is already finished when it starts.

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
| `~/.claude/governor/samples.jsonl` | `trace agent`, `trace sample`, and the gate's build records (section 8) | by hand — it is the measurement, and deleting it on uninstall would throw away the phase |
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

The keys, and who owns each:

| Key | Written by | Read by | Absent means |
|---|---|---|---|
| `schema` | every write | `Load`, `doctor` | refused — nothing can tell which version wrote it |
| `endpoint` | the user, by hand | — | the default endpoint |
| `run_queue_max` | `calibrate` | the daemon, into `budget.Controller` | the compiled-in `RunQueueMax` |
| `b_max` | `calibrate` | the daemon, as a **cap** on `floor(capacity / 1.2)` | the per-tick derivation alone |
| `calibrated_at` | `calibrate` | `doctor` | this host has never been measured |
| `calibrated_from` | `calibrate` | `doctor`, and a person reading the file | as above |
| `setpoint` | nothing | nothing | — |
| `wait_max_ms` | the user, by hand | — | the compiled-in wait budget |

`setpoint` is in that table with two dashes on purpose. It is no longer written and no longer
read, and the key is kept only so a config written before GOV-32 still round-trips instead of
being reported as unrecognised. §5.2a has the reasoning.

**The daemon reads this file once, at start-up, and the gate never reads it at all.** The
gate's whole cost is process start — ~6 ms against ~0.6 ms of round trip — so a file read on
every `Bash` call to look up a constant the daemon already holds would spend the one budget
that matters. Every failure to read resolves to "this host was never calibrated" and the
compiled-in defaults: a daemon that refused to start over an unreadable tuning file would take
the machine ungoverned to protect a number that has a working default.

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

**That happened, and the check now compares the bytes as well as the numbers.** The paragraph
above stood here as a hypothetical and was not one: on 2026-09-20 the installed hook binary
was four merged PRs behind main — GOV-33, GOV-18, GOV-26, GOV-B01 — and `doctor` reported
`ok binary — the hooks run this binary (0.0.0-dev)` throughout, twelve of thirteen checks
passing. GOV-18 changes max-min fair build slots and the queue's back door, which is park and
deny behaviour, which was the very metric the measurement window about to open would score.
A baseline taken on that binary against an after-window on a newer one would have differed by
GOV-18 as well as by the thing under test, with no way to separate them afterwards.

So `checkBinary` digests both files (GOV-B13). The comparison is deliberately the **same
predicate `install` uses to decide whether it has anything to copy**, read from the other end:
if `install` would replace the binary, `doctor` says the install is stale, and if it would
not, `doctor` says it is current. They cannot drift apart, because there is one rule. Identical
bytes also settle the question before the `version` subprocess runs — two files holding the
same bytes are the same build whatever they call themselves, so there is nothing to ask.

**The check cannot see staleness when it IS the installed binary, and says so.** Any check of
this shape compares the binary running the check against the binary the hooks run; invoked
through the install — the ordinary way — those are one file and there is no second build to
compare against. That case reports `ok` with `staleness unchecked` in its own detail line and
names the fresh build as how to get the answer. It stays a pass rather than `unknown` because
nothing is known to be wrong and `unknown` is exit `3`, which would land on every ordinary
`doctor` run and on `install`'s own verification — the "red line in front of every user" cost
that state was careful to avoid when GOV-30 introduced it. **A check whose blind spot is
undocumented is how the hole above survived being written down, so it is documented in both
places a reader might look: here, and in the report itself.**

Release builds were never affected, which is why this was a chore and not a launch blocker —
a published version carries a real tag and the version comparison alone is enough. The broken
case was exclusively the dogfooding path, which is also the path every finding in this
project's backlog is produced on.

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

**The second signal compared a version string, and that was not enough (GOV-43).** Two builds
that both report `0.0.0-dev`, or a local build made after a release, answered "current" to the
interrogation. Where the binary on disk was already current, so the digest signal did not fire
either, the old daemon stayed. The interrogation now also compares the daemon's `build` (§7)
with the digest of the installed binary. An absent `build` means a daemon older than the field
and is not current. `"unknown"` is not current either, by the same "ambiguous resolves to
restart" rule. `TestInstallReplacesADaemonOfAnotherBuildWhoseBinaryIsAlreadyCurrent` stages
exactly this, with a rebuilt same-version daemon on the socket and a correct binary on disk,
and fails with the build comparison removed.

**Only `governor install` replaces a daemon. No other route does.** Recorded per route for
GOV-43 clause 1:

| Route | What an upgrade changes | Replaces the running daemon? |
|---|---|---|
| `governor install` (and `install.sh`, which ends in it) | renames the new binary into `bin/`, rewrites the plugin tree | **yes**: on a new digest, or on a daemon whose version or build differs |
| `brew upgrade governor` | replaces `/opt/homebrew/bin/governor` only. The formula is `bin.install` with no `post_install`, and its caveat tells the user to run `governor install` | **no**. Neither the hooked binary in `~/.claude/skills/build-governor/bin/` nor the daemon changes until the user runs `governor install` |
| marketplace (`claude plugin update build-governor@claude-governor`) | re-runs the command source, `governor plugin-path`, which prints the tree install wrote. That source is in `.claude-plugin/marketplace.json`; `runPluginPath` stats two files and writes nothing | **no**. It copies no binary and touches no daemon. The update is only as new as the last `governor install` |

So after a `brew upgrade`, the brew binary is new while the hooks and the daemon are still the
old build, and they agree with each other. Nothing is stale *relative to the hooks*. The machine
simply has not been upgraded yet, and `governor doctor` resolves to `~/.local/bin/governor`,
which is the hooked binary, so its binary check reports `staleness unchecked`. The fix for both
routes is the same command the caveat already names. `/opt/homebrew/bin/governor doctor` names
it explicitly, because there the binary check compares two different files.

Measured on this machine on 2026-10-31 from the formula, the marketplace manifest, and the
installed state. Brew 0.1.1 is installed, byte-identical to the hooked binary, and no
`build-governor` marketplace registration exists. **Not yet observed as an actual upgrade:**
v0.1.1 is the newest release, so there is nothing to `brew upgrade` to, and a marketplace
command source can only be accepted in a person's own terminal (§13.2a). Both rows are to be
re-confirmed against the release that carries GOV-43.

**Why pid 23289 survived 40 days.** It was started on 2026-09-20 from the pre-rename tree
`~/.claude/skills/claude-governor/bin/governor`, a 0.0.0-dev build. On 2026-10-31, v0.1.1 arrived
by `brew install` at 13:37 (Cellar timestamp), which by the table above touches no daemon. Every
protocol-1 hook found that daemon healthy until `governor install` rewrote the plugin tree at
15:06 and the daemon log shows a new `listening` line. No version-string check had flagged
it before then, because nothing was comparing the running daemon with anything. With GOV-43 the first status line rendered by an installed v0.1.1 hook binary reads
`stale daemon: governor install`, because that daemon sends no `build`. `doctor`'s `build`
check fails with the same fix, and that `governor install` replaces it, whatever version
either side reports.

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
