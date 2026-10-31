# How it works

A ten-minute tour of the governor's design. [design.md](design.md) is the contract. This page
summarises it, and every paragraph links to the section it summarises. Numbers such as
thresholds, timeouts and weights are deliberately left out, because they live in design.md and
have moved more than once. Where this page and design.md seem to disagree, design.md is right
and this page has a bug.

For day-to-day use (the status line, refusals, overrides, troubleshooting), read
[guide.md](guide.md).

---

## The problem

Several Claude Code sessions on one laptop each fan out into subagents and builds, with no idea
the others exist, and every session slows down together. A fixed cap such as
`cores - 2` is wrong in both directions. A subagent waiting on the API costs almost nothing,
while a subagent that has just started a test suite costs a whole core. On a fanless machine,
the right cap also changes as the chassis warms up. So the number of things allowed to run at
once is the output of a feedback controller, not a configured constant
([§1](design.md#1-problem)).

It is not a scheduler or a permission system. It never reorders or launches work, never
decides whether something is safe, and never kills anything
([§1](design.md#non-goals)).

Measurement also changed **what** is counted. Subagents turned out to be cheap and steady.
Builds and test runs are usually nothing and occasionally enormous. So the budget is counted in
**build slots**. A build-shaped command takes one slot or more, and a subagent takes a small
fraction of one ([§5.2](design.md#52-aimd), [phase-0-findings.md](phase-0-findings.md)).

---

## The pieces

One Go binary, `governor`, plays every part ([§2](design.md#2-architecture)):

- **The daemon** (`governor daemon`): one per machine and long-lived. It holds the lease table,
  samples the machine about once a second, and runs the control loop that sets the budget.
- **The gate** (`governor gate`): a `PreToolUse` hook. One short-lived process per gated tool
  call, which asks the daemon for a lease and exits.
- **The lifecycle hooks** (`confirm`, `release`, `finish`, `session`): small processes that tell
  the daemon when a subagent actually started or stopped, when a build finished, and when a
  session opened or closed ([§4.1](design.md#41-hook-wiring)).
- **The MCP server** (`governor mcp`): a stdio child of each session, offering one advisory
  tool ([§8](design.md#8-advisory-channel)).
- **The status line** (`governor statusline`): renders the one-line summary in every session
  ([§9](design.md#9-package)).

The gate is Go, and only Go, because it runs inside a hook that Claude Code blocks on, on every
gated call. An interpreter's start-up time would be a large part of the cost
([§2](design.md#2-architecture)).

Six hooks are registered in the plugin's own `hooks/hooks.json`. The gate matches the
subagent-spawning tool under both of its historical names, `SendMessage` (which can resume a
subagent), and `Bash` ([§4.1](design.md#41-hook-wiring)). Those tool names were measured, not
read from documentation, and each Claude Code version they were confirmed on is recorded
([§10](design.md#10-hook-surface--confirmed), [hook-surface.md](hook-surface.md)).

---

## A gated call, from start to finish

**The cheap case costs nothing.** Most `Bash` calls are not build-shaped. The gate classifies
the command text with a pure function, inside the hook process, before any I/O. A command that
names no build tool is answered and dropped without the daemon ever being contacted
([§5.5](design.md#55-classification-which-bash-is-build-shaped)). The classifier catches every
expensive command that names a build tool, and none of the expensive commands that do not.
That is a limit of predicting cost from command text, and
[bash-corpus.md](bash-corpus.md) measures it.

**Otherwise the gate asks the daemon** over a unix socket. The protocol is newline-delimited
JSON, one request per connection, and every message carries a version
([§7](design.md#7-wire-protocol)). Four outcomes follow ([§4.2](design.md#42-decision-table)):

| Situation | What the gate does |
|---|---|
| room in this session's share | exits 0 with empty stdout, and the call proceeds |
| no room, but likely soon | the daemon says it is parking the request, the gate waits, then the call proceeds |
| still no room at the deadline | denies, with a reason naming the numbers, the cause and one instruction |
| the caller is itself a subagent | denies at once, because nested fan-out is not allowed ([§6.3](design.md#63-depth)) |

The grant path is **silent**. The gate never answers `allow`, because that would suppress the
user's own permission prompts ([§3](design.md#3-invariants), invariant 3). A refusal must not
state a fact the daemon did not send, or a conclusion its own numbers contradict. That second
rule came out of a real incident ([§4.2](design.md#42-decision-table),
[§6.1a](design.md#61a-the-2026-09-21-false-deny-incident-gov-34)). A spawn and a build are
refused with different instructions. A refused spawn means "do it yourself". A refused build
means "this did not run, wait and run it again" ([§4.2](design.md#42-decision-table)).

---

## Leases

A grant creates a **lease**: a reservation of some slots, owned by a session. There are two
lifecycles, because the two kinds of tool call give the governor different evidence
([§4.3](design.md#43-lease-lifecycle)).

- **A subagent lease** starts **tentative** at `PreToolUse`, because the spawn might never
  happen: the permission prompt can be declined, or the turn interrupted. `SubagentStart`
  **confirms** it and binds it to the owning process. `SubagentStop` releases it. The start
  event carries no key linking it to its request, so confirmation pairs them in order, oldest
  first, within the session. That ordering was measured, not promised, which is why the reaper
  exists to make a wrong pairing harmless.
- **A `Bash` lease** is born confirmed and is released by `PostToolUse`, which carries the
  same key as the request. However, `PostToolUse` does not arrive when a command fails, and
  arrives early when a command is backgrounded or times out, so the release is ignored in those
  cases ([§4.3](design.md#43-lease-lifecycle),
  [§5.6](design.md#56-how-long-a-bash-lease-lives-and-why-the-reaper-is-the-primary-mechanism)).

**The reaper is the primary mechanism, not a safety net.** The design assumes the stop events
never fire ([§3](design.md#3-invariants), invariant 2). Every few seconds the reaper drops
leases that have passed their time-to-live, leases whose session process is gone, and confirmed
leases whose owner is gone. For a `Bash` lease, the time-to-live is set from the measured
runtimes of real builds. Too short, and slots are handed back while a build is still burning
CPU. Too long, and a failed build holds its slot for a while
([§5.6](design.md#56-how-long-a-bash-lease-lives-and-why-the-reaper-is-the-primary-mechanism)).
Owners are identified by process ID plus start time, so a recycled process ID cannot keep a
dead lease alive ([§4.3](design.md#43-lease-lifecycle)).

---

## The control loop: run-queue AIMD

The budget is set by **additive increase, multiplicative decrease**, the same shape TCP uses
for congestion: reclaim headroom cautiously, give it back at once
([§5.2](design.md#52-aimd)). Every tick:

- **Thermal pressure at `serious` or above** takes the budget to its floor.
- **A run queue above the cut threshold** halves it.
- **A run queue at or below the climb level, with someone waiting,** raises it by one.
- **Anything in between** holds it steady. This busy-and-coping band is where it is meant to
  sit under build load.

The signal is the **run queue**: load average divided by usable cores. It is **not** CPU
utilisation. Utilisation saturates at 100% from two concurrent builds onward, so it cannot tell
two builds from eight, while the run queue keeps rising. Controlling on utilisation also left
part of the machine idle with work queued, and kept the budget from ever climbing under real
builds. That was measured, not argued: [climb-condition.md](climb-condition.md),
[§5.2](design.md#52-aimd). CPU is still sampled and shown, but the loop does not read it.

A few properties worth knowing ([§5.2](design.md#52-aimd)):

- **The budget never reaches zero.** A machine under duress still makes progress, one build at
  a time ([§3](design.md#3-invariants), invariant 4).
- **Cutting the budget never revokes running work.** It only affects the next admission
  ([§3](design.md#3-invariants), invariant 5). That is what makes a slow-moving signal like
  load average affordable.
- **It starts at the ceiling, not the floor.** The daemon is started by the first gated call,
  which means someone is trying to do something right then.
- **The ceiling comes from usable cores**, using the cgroup quota inside a container rather
  than the host's core count ([§5.1](design.md#51-signals-sampled-at-1-hz)).

**Calibration** makes the two constants that differ between machines local. `governor
calibrate` places the cut threshold above what **this** machine's run queue already is, and
measures how many cores this hardware really delivers in parallel, which can only lower the
ceiling. Nothing tuned on the development laptop is anyone else's default
([§5.2a](design.md#52a-calibration-where-those-constants-come-from-on-a-machine-that-is-not-this-one),
[calibration.md](calibration.md)).

---

## Thermal

On macOS, thermal state comes from `ProcessInfo.thermalState`, a public API that needs no
privileges, read through a small cgo shim. `powermetrics` would give more detail and needs
`sudo`, which rules it out ([§5.3](design.md#53-thermal)).

It is a **backstop, not the controller**. Under ten minutes of saturating load on the
development machine, the state never left `nominal`. The loop therefore floors the budget only
at `serious` or `critical`, not at `fair` ([§5.2](design.md#52-aimd) is normative on this.
[§5.3](design.md#53-thermal) opens with the original, broader rule and notes that it was
superseded). On Linux there is no equivalent, so it reports `unknown`, which
is treated as nominal and never as a cut ([§13.5](design.md#135-platforms)).

A darwin binary built without cgo is silently thermally blind. That is why released Mac
binaries are built natively, and why `governor status` on a Mac must never say `unknown`
([§5.3](design.md#53-thermal)).

---

## Sharing the budget between sessions

Two rules decide who gets a slot ([§6](design.md#6-allocation)):

- **Fair share.** Every live session is guaranteed a slot before any session gets a second.
  Unused share goes to the sessions actually asking. A session's demand is revealed by what it
  holds and whether it has recently been refused, never by what it says
  ([§6.1](design.md#61-fair-share)). A session's first slot is admitted even when it would go
  over budget, as long as the machine has some room. Without that, a large build could never
  start beside even one small lease
  ([§6.1a](design.md#61a-the-2026-09-21-false-deny-incident-gov-34)).
- **One FIFO queue.** Waiters are served strictly in arrival order, and a new request cannot
  jump a non-empty queue, so a session asking in a tight loop cannot starve one that is waiting
  patiently ([§6.2](design.md#62-wait-queue)).

`governor boost` raises one session's weight in that share. It changes who refills a contended
budget, not who arrives first ([§6.1](design.md#61-fair-share),
[§9.1](design.md#91-the-slash-command)).

---

## Fail-open

This is the first invariant, and the one everything else gives way to
([§3](design.md#3-invariants)). If the daemon is missing, refuses connections, accepts and
never answers, answers garbage, or speaks another protocol version, **the call proceeds
ungoverned**. The gate has a hard deadline well inside Claude Code's own hook timeout
([§4.4](design.md#44-performance-budget)). Several details exist to protect this:

- **Waiting is licensed by the daemon, never assumed by the gate.** The gate extends its
  deadline only after the daemon has replied that it parked the request. A wedged daemon
  never sends that reply, so the gate fails open on the original deadline
  ([§4.2](design.md#42-decision-table)).
- **Unrecognised refusals are not honoured.** A refusal for a reason this binary cannot explain
  is treated like an unreachable daemon. During an upgrade, a mix of versions produces
  ungoverned calls, never stalled ones ([§4.2](design.md#42-decision-table),
  [§13.4](design.md#134-config-and-upgrade)).
- **A dead daemon restarts itself.** A gate that cannot connect has already let its own call
  through, and starts a daemon for the next one, rate-limited and without waiting for it
  ([§4.5](design.md#45-lazy-daemon-start)).
- **The binary is replaced by rename, never overwritten.** On macOS, copying over a running
  binary gets every later run of it killed without a word, and a killed process cannot fail
  open ([§13.4](design.md#134-config-and-upgrade)).

`TestGateFailsOpen` drives the gate through every failure shape it knows.

---

## Advice before enforcement

A hook alone makes for a bad experience: the model starts a build, is refused, and spends
tokens learning what it could have been told. So each session also gets an MCP tool,
`governor__request_build`, that answers "may I run this command now, and if not, roughly when?"
It uses the same classifier as the gate, so its answers agree with the gate by construction.
It answers cheap commands without contacting the daemon, and grants everything when the daemon
is unreachable ([§8](design.md#8-advisory-channel)).

A session learns that the tool exists from a standing instruction injected at session start. A
skill's description is resident from the start, but its body is read only after the skill is
invoked, which is too late to change a decision already made
([§8](design.md#8-advisory-channel)). The advice steers. The hook enforces either way.

---

## Install routes

There are three ways in, and all of them end with one plugin, `build-governor`, under
`~/.claude/skills/build-governor/`, and one binary inside it
([§9](design.md#9-package), [§13](design.md#13-install-onboarding-and-upgrade)):

1. **`install.sh`.** Downloads the binary for this OS and architecture and hands off to
   `governor install`.
2. **Homebrew.** `brew install` supplies the binary on `PATH`, and you then run
   `governor install`.
3. **The Claude Code plugin marketplace.** After either of the above, the marketplace entry
   runs `governor plugin-path` and links to the tree `governor install` already wrote. It
   carries no second copy, so the two registrations cannot double up. A person accepts that
   command once, in their own terminal
   ([§13.2a](design.md#132a-the-marketplace-route-and-why-the-plugin-is-build-governor)).

Whichever route you take, `governor install` writes the plugin, merges a `statusLine` into
`settings.json` after backing it up, starts the daemon, calibrates, and then runs
`governor doctor` to check its own work ([§13.2](design.md#132-command-surface)). Running it
twice changes nothing the second time. `governor uninstall` reverses all of it, restoring your
previous `statusLine` byte for byte. Every path it touches is listed, normatively, in
[§13.3](design.md#133-what-touches-the-users-machine), and tests hold uninstall to that list
([§13.1](design.md#131-principles)). Hooks are bound when a session starts, so sessions already
open stay ungoverned until restarted
([hook-surface.md §17](hook-surface.md#17-hooks-are-bound-at-session-start-and-how-to-see-which-sessions-loaded-them),
measured on Claude Code 2.1.267).

---

## Where the design goes next

Everything above governs one machine. CPU does not add up across hosts, so a fleet-wide CPU
budget is not a thing this design will build. A second tier would govern genuinely shared
resources such as tokens, spend and rate limits. The interfaces it would plug into already
exist, each with exactly one implementation today ([§12](design.md#12-scale-posture)).
