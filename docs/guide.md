# Using the governor

This is the guide for someone who has installed the governor and wants to know what it is
doing to their sessions, how to read it, how to overrule it, and how to remove it. It explains
the design rather than restating it. Every number lives in [design.md](design.md), the
contract, and this page links to the section that holds it, because numbers there have moved
more than once and a copy here would eventually be wrong.

For how the pieces fit together, read [how-it-works.md](how-it-works.md). To install it, see
the [README](../README.md#install).

The plugin is called **`build-governor`**, and the binary is `governor`. An install from
before the rename is covered under [Upgrading from `claude-governor`](#upgrading-from-claude-governor).

---

## What changes in a session once it is governed

From the next session you open, two kinds of tool call ask the machine-wide budget before
they run:

- **A subagent spawn**: an `Agent` call, or a `SendMessage` that resumes a subagent. It
  weighs a small fraction of a slot ([design.md §5.2](design.md#52-aimd)).
- **A build-shaped `Bash` command**, such as `go test ./...`, `cargo build`, `npm test` or
  `docker compose`. It weighs one slot or more, depending on its cost class
  ([§5.5](design.md#55-classification-which-bash-is-build-shaped)).

Everything else is untouched. `ls`, `git status`, `cat` and most other `Bash` calls are
classified as cheap inside the hook and never contact the daemon at all. Reads, edits and
other tools are not hooked.

When the machine has room, a gated call simply runs, and you see nothing. The hook prints
nothing and does not touch your permission prompts. It never answers `allow`, so every
prompt you would normally get still appears ([§3](design.md#3-invariants), invariant 3).
When the machine is full, the call waits briefly and then runs. If it is still full after
that wait, the call is refused with a sentence that says why
([below](#when-a-spawn-or-a-build-is-refused)).

Four things do not change:

- **Nothing is ever killed.** Lowering the budget only holds back new work, and anything
  already running finishes ([§3](design.md#3-invariants), invariant 5).
- **It fails open.** If the daemon is down, slow, the wrong version, or anything else goes
  wrong, the call goes ahead ungoverned. A broken governor costs you governance, never a
  stalled session ([§3](design.md#3-invariants), invariant 1).
- **Nested fan-out is refused.** A subagent that tries to spawn its own subagents is told to
  do the work itself ([§6.3](design.md#63-depth)).
- **Sessions that were already open stay ungoverned.** Claude Code binds hooks when a
  session starts, so a window opened before install never loads them until you restart it.
  That was measured on Claude Code 2.1.267
  ([hook-surface.md §17](hook-surface.md#17-hooks-are-bound-at-session-start-and-how-to-see-which-sessions-loaded-them)).
  The status line says so in that window.

Each session also gets a standing instruction at start-up: before an expensive build, ask the
governor first ([below](#the-governor__request_build-tool)). Your first governed session gets
a one-time notice explaining the status line and how to turn the governor off. It goes to you,
not into the model's context, and it is never shown again.

---

## Reading the status line

The status line is one line at the bottom of each session window. Install writes it into the
`statusLine` key of your `~/.claude/settings.json` and backs up whatever was there before. It
always shows exactly one of four states.

### Governed: the slot count

```
3/5 slots - cpu 61% - thermal ok
```

The parts, in order:

| Part | Shown | Means |
|---|---|---|
| 3/5 slots | always | slots in use across **every** session on the machine, out of the current budget |
| (1 yours) | only when this session holds something | how many of those leases are this window's |
| boosted | while a boost is in force | this session's share was raised by hand ([overrides](#overriding-it-limit-pause-resume-boost)) |
| pinned | while a limit is in force | the budget is a number a person set, not one the control loop chose |
| 2 waiting | only when something is queued | spawns or builds parked, waiting for a slot |
| cpu 61% | once the daemon has taken a sample | host CPU, for context. The budget does **not** move on it ([§5.2](design.md#52-aimd)) |
| thermal ok | when the platform reports a state | `ok`, `fair`, `serious` or `critical`. Absent on Linux, which has no equivalent ([§13.5](design.md#135-platforms)) |

A slot is roughly one ordinary build. A subagent is a small fraction of one, so a machine
running only agents reads `0.2/6 slots - cpu 12% - thermal ok`, not zero.

Some lines you may see:

| Line | What is going on |
|---|---|
| `0/4 slots` | governed and idle: nothing held, nothing waiting |
| `3/5 slots (1 yours) - cpu 61% - thermal ok` | one of the three in use belongs to this window |
| `4/4 slots - 2 waiting - cpu 97% - thermal fair` | full, with two calls queued behind it |
| `1/1 slots - cpu 99% - thermal serious` | thermal pressure holds the budget at its floor ([§5.2](design.md#52-aimd), [§5.3](design.md#53-thermal)) |
| `0/1 slots - thermal critical` | the same, at the most severe level |
| `4/4 slots` | full, possibly with leases that outlived the work they were for ([troubleshooting](#a-full-budget-on-a-quiet-machine)) |
| `2/2 slots - pinned` | someone ran `limit 2`. The budget is theirs, not the loop's |
| `2/2 slots - pinned 42m left` | the same, with the time until it lapses |
| `1/6 slots - boosted` | this session's share is doubled |
| `1/6 slots - boosted 55m left` | the same, with the time until it lapses |

On a narrow terminal the line drops clauses from the right, so the slot count survives and the
context goes first.

### Paused

```
governor: gating paused
```

Someone ran `pause`. Nothing is being capped. The slot count is hidden on purpose: during a
pause it would read like a limit that is not being enforced. With a deadline it reads
`governor: gating paused 14m left`.

### Governed by an older daemon

```
1/4 slots - stale daemon: governor install
```

The daemon answering is a different build from the one installed, usually because an upgrade
put a new binary on disk while the old daemon kept running. It is still capping spawns, so the
count is real, but fixes in the new version are not running. Run `governor install`: it
replaces that daemon and leaves everything else alone. Nothing restarts it automatically
([§9](design.md#9-package) says why).

### Not governed: this window started too early

```
governor: ungoverned - restart to enable
```

This window opened before the governor was installed, or before its hooks loaded, so nothing
it spawns is capped. Restart the session. The other windows are unaffected.

### Not governed: no daemon

```
governor: daemon not running - spawns ungoverned
```

The status line could not get an answer from the daemon, so nothing is being capped. The
next gated call starts a daemon on its own ([§4.5](design.md#45-lazy-daemon-start)), so this
usually clears by itself. On a heavily loaded machine this line can appear even though the
daemon is fine, because the status line itself did not get scheduled in time
([§9](design.md#9-package)). If it persists, run `governor doctor`.

The status line refreshes on a timer as well as on each message, so it moves during a fan-out
that produces no output ([§9](design.md#9-package)).

---

## When a spawn or a build is refused

A refusal arrives as a hook denial that the model reads. It always states the numbers, the
cause and one instruction. The model is told different things for the two kinds of call,
because they are different situations.

### A refused spawn

```
Machine at capacity (4/4 build slots held, this one needs 0.1, CPU 93%, thermal: nominal). Run this task inline yourself rather than delegating it. Do not retry the Agent call.
```

The work still happens. The session does it itself instead of delegating it. If the session
keeps retrying, the wording gets firmer on the second and third refusal
([§4.2](design.md#42-decision-table)).

A nested spawn, meaning a subagent trying to start its own subagents, is refused at once
whatever the load:

```
Nested subagent spawns are refused: this Agent call comes from a subagent, and only the main session may fan out. Do this task inline in the agent you are already in. Do not retry the Agent call.
```

**What to do:** nothing, usually. If you want more parallelism right now and know the
machine can take it, raise the budget for a while with `limit`, or `pause` gating
([overrides](#overriding-it-limit-pause-resume-boost)).

### A refused build

```
Not enough free build slots (1/4 build slots held, this one needs 3.8, thermal: nominal). This command was not run. This is a large build and the slots left free are not enough for it. Wait a few seconds and run exactly the same command again; do not look for a different way to build, and do not treat this as a build failure.
```

**The command did not run.** It did not fail, and its output is not a compiler error. The
right response is to wait and run the same command again, and that is what the model is
told. The first words say which situation it is
([§4.2](design.md#42-decision-table)):

| Head | Means |
|---|---|
| `Machine at capacity` | every slot is taken |
| `This session is at its fair share` | the machine has room, but it belongs to other sessions that are asking ([§6.1](design.md#61-fair-share)) |
| `Not enough free build slots` | the machine has room, but less than this command needs |

When the budget is below what this machine allows, the refusal also names the cause, for
example `budget cut from 6 by run queue`.

**What to do:** let the model retry, which is what it is told to do. If the same session is
refused three times, it is told to stop starting builds and tell you that the machine is
contended. At that point you decide: wait, check `governor status` for what is holding the
slots, or override the budget for a while.

---

## The `governor__request_build` tool

The plugin ships an MCP server with one tool, which a session sees as
`mcp__plugin_build-governor_governor__governor__request_build`. That name was measured on
Claude Code 2.1.312, not composed from the parts
([hook-surface.md §23.6](hook-surface.md#236-the-renamed-plugins-real-names-and-the-double-load-seen-directly--21312)).
It takes the exact command about to run, and a short reason, and answers whether to run it
now:

```
governor__request_build(command: string, why: string)
  → { granted: bool, wait_hint_ms: int, reason: string }
```

([design.md §8](design.md#8-advisory-channel).)

**When a session should call it:** before an expensive build or test run, meaning anything
that compiles a project, runs a suite, or starts containers. **Not** before ordinary shell
work. It uses the same classifier as the hook, so for a cheap command it answers `granted`
immediately without asking the daemon, and asking about `ls` is merely wasted effort. When
unsure, do not ask, because the hook enforces the budget underneath either way.

**How to read the answer:** `granted: true` means run it now. `granted: false` means do not
run it yet. Wait about `wait_hint_ms`, then ask again. The answer decides **when** a build
runs, never **whether** it does. If the daemon is unreachable, the tool answers
`granted: true`. Advice that failed closed would stall a session that was trying to
cooperate.

Every session is told this at start-up. The bundled skill `build-governor:heavy-build` carries
the detail ([§8](design.md#8-advisory-channel)). Calling the tool is a convenience: the
measurement that would show it reduces load could not be scored on the development machine
([advice.md](advice.md)), so it is shipped because it saves a refusal round trip, not because
it is proven to save CPU.

---

## Calibrating: `governor calibrate`

The limits this ships with were measured on one laptop. `governor calibrate` measures **your**
machine and writes two numbers into its config: the run-queue level at which the budget is
cut, and the most slots this machine may ever hand out
([§5.2a](design.md#52a-calibration-where-those-constants-come-from-on-a-machine-that-is-not-this-one)).

```sh
governor calibrate
```

It reads your core count and memory, reads how loaded the machine already is, and runs a
short load probe. The probe is announced before it starts, and it is skipped if the machine is
already busy. The probe measures how many cores this hardware actually delivers in parallel,
which is often fewer than it advertises. The output, and the config it writes, record when the
measurement was taken and from what.

`governor install` runs it once at the end. **Re-run it while you are doing real work**, when:

- **You calibrated on an idle machine.** The cut level is placed above what the machine was
  doing in the minute before calibrate ran. Measured idle, the governor will throttle you as
  soon as you are not.
- **Your normal workload changes**, for example a new project with heavier builds, or a
  language server that is always running.
- **The hardware changes.** `governor doctor`'s `calibration` check warns when the config was
  measured on a machine with a different core count, and when the machine has never been
  measured at all.

**Calibration takes effect when the daemon next starts.** The daemon reads its config once, at
start-up ([§13.4](design.md#134-config-and-upgrade)), and `governor install` leaves a healthy
daemon alone. So after re-running calibrate, restart the daemon yourself, the same way
`governor doctor` names it:

```sh
kill $(cat ~/.claude/governor/daemon.pid) && governor install
```

`governor install` runs calibrate after it has started the daemon, so this applies to a fresh
install too. Until the daemon restarts, it runs on the shipped defaults.

---

## Overriding it: limit, pause, resume, boost

When you already know a heavy build is coming, say so instead of waiting for the control loop
to work it out. From inside a session, use the slash command `/build-governor:governor`. From a
terminal, use the binary directly ([§9.1](design.md#91-the-slash-command)):

```sh
governor limit 2 for 30m   # pin the budget at 2 slots, whatever the loop thinks
governor pause for 15m     # stop capping, without stopping the daemon
governor resume            # end a pin or a pause early
governor boost             # double this session's share of a contended budget
governor status            # what is in force right now, and everything else
```

- **Every override expires.** Without `for <duration>` it lasts a default period. Ask for a
  very long one and it is capped at the maximum
  ([§9.1](design.md#91-the-slash-command)). The command prints what it chose. Nothing here
  survives a daemon restart, and an upgrade restarts the daemon.
- **The status line says so while one holds.** It shows the word pinned or boosted beside the
  slot count, and `governor: gating paused` in place of it. An override you cannot see is one you
  forget you set.
- **`pause` stops the cap, not the bookkeeping.** Leases are still recorded, so
  `governor status` stays accurate during a pause.
- **`boost` changes who gets the next free slot when sessions compete, not who gets in
  first.** On an idle machine it changes nothing, and that is expected. It needs to know which
  session you are in. Run it through `/build-governor:governor boost` inside the session you
  want boosted. From a plain terminal it has no session to apply to, says
  `boost not applied`, and changes nothing. Whether the slash command's subprocess receives
  the session id has not been measured
  ([hook-surface.md §22.7](hook-surface.md#227-still-unmeasured)), so if it reports
  `boost not applied` there too, that is why.

`limit` sets the budget for the whole machine, not just for this session: every session
shares the one budget.

---

## Troubleshooting by symptom

Start with `governor doctor`. Every check it fails names the command to fix it. Its exit code
is `0` for all clear, `1` for a real failure, and `3` when a check could not answer its own
question ([§13.2](design.md#132-command-surface)). The symptoms below each point at the check
or command that settles them.

### Builds feel slow, or keep being parked

1. `governor status` shows the budget, what is in use, and what is waiting.
2. `governor doctor`, **`budget` check.** It warns when the budget is below this machine's
   ceiling and names the signal cutting it, such as the run queue or thermal pressure. That is
   the loop working, and the budget climbs back as the machine quietens
   ([§5.2](design.md#52-aimd)). It also warns when someone pinned the budget and names the
   `resume` that lifts it.
3. **`calibration` check.** "Never been measured", or "calibrated on a different machine",
   means the limits are somebody else's. Run `governor calibrate` while working, then restart
   the daemon so it uses the new numbers ([see calibrating](#calibrating-governor-calibrate)).
4. If the budget is fine and the machine really is busy, it is doing its job. Use
   `governor limit N for D` if you want more concurrency anyway.

### A full budget on a quiet machine

A build that **fails** returns its slot only when the lease expires, because Claude Code does
not send the event that would return it sooner. The same is true of a command run in the
background or one that exceeded its timeout
([§5.6](design.md#56-how-long-a-bash-lease-lives-and-why-the-reaper-is-the-primary-mechanism)).
A burst of failing builds can therefore hold slots for a few minutes after the work has
stopped. The slots come back on their own. `governor doctor`'s **`leases`** check fails when a
lease cannot be reclaimed at all, and names the restart that clears it.

### The status line says `stale daemon: governor install`

Run `governor install`. In `governor doctor` this is the **`build`** check: it compares the
running daemon's build with the binary the hooks run.

**After a `brew upgrade`, use the full path:** `$(brew --prefix)/bin/governor install`. Brew
changes neither the hooked binary nor the daemon, and a bare `governor` usually resolves to
`~/.local/bin/governor`, a link to the old hooked copy. Running that reinstalls the old version
over itself and reports "unchanged". `$(brew --prefix)/bin/governor doctor` shows the right
command in its fix lines.

### The status line says `governor: ungoverned - restart to enable`

Restart that session. **`sessions`** in `governor doctor` lists every running session that is
not governed, by its working directory.

### The status line says `governor: daemon not running - spawns ungoverned`

`governor doctor`, **`daemon`** and **`socket`** checks. The usual fix is `governor install`,
which starts the daemon. Starting a new subagent or build also starts one by itself
([§4.5](design.md#45-lazy-daemon-start)). If the line comes and goes while the machine is
heavily loaded, see [the note above](#not-governed-no-daemon).

### `governor doctor` says `unknown`

`unknown` means "could not look", not "broken", and the exit code is `3`.

- **`spawn`:** the hook has not yet been seen to catch a real subagent spawn on the Claude
  Code version running now. That is normal on a fresh install and after every Claude Code
  update. Run any subagent in a governed session, then run `governor doctor` again. If it
  still says `unknown` after a spawn, the hook is not firing, and that is worth reporting.
- **`sessions`:** this machine's Claude Code keeps no session registry the governor can read,
  so it cannot check whether open windows are governed. Some hosts cannot settle this at all,
  and the line says so.

### Two of everything after an upgrade

Two `request_build` tools, two `/…:governor` commands, or slots filling twice as fast as they
should. Two copies of the plugin are loaded, so every hook runs twice. `governor doctor`'s
**`hooks`** check fails with `the gate is registered in N places`.

- **An old `claude-governor` tree** is left from before the rename. `governor install` from
  the new version removes it ([§13.2a](design.md#132a-the-marketplace-route-and-why-the-plugin-is-build-governor)).
- **A marketplace copy that is not linked to your install.** `doctor` names the
  `claude plugin uninstall` that removes it.

Restart your sessions afterwards. Hooks are bound at session start.

### `thermal unknown` on a Mac

`governor status` should never say `thermal unknown` on macOS. If it does, the binary was
built without cgo and cannot read thermal state ([§5.3](design.md#53-thermal)). Reinstall from
a release, through `install.sh` or Homebrew, rather than from a cross-compiled build. On Linux,
`unknown` is expected and treated as normal ([§13.5](design.md#135-platforms)).

### `governor doctor` warns about `strays`

Another governor daemon has been running on its own socket for a while. This is usually a
leftover from development or testing. `doctor` names the pids, and the fix is to `kill` them
if you did not start them on purpose.

### The marketplace install says the command `is ignored inside a Claude Code session`

The marketplace entry runs `governor plugin-path`, and Claude Code accepts a command like that
only from a person in their own terminal, never from inside a session. That was measured on
2.1.310 and 2.1.312
([hook-surface.md §23.3](hook-surface.md#233-a-command-source-re-run-every-session-refused-inside-a-session)).
Open a terminal outside Claude Code and run
`claude plugin install build-governor@claude-governor` there. It shows the exact command and
asks `[y/N]`.

---

## Uninstalling

Whichever route you installed by, `governor uninstall` removes what `governor install` wrote:
the plugin tree, the `statusLine` it merged into `~/.claude/settings.json` (your previous value
is restored, byte for byte), the daemon and its socket. `--purge` also removes the calibrated
config, the first-run marker and the sample log, which are otherwise kept. The full list of
paths is normative in [§13.3](design.md#133-what-touches-the-users-machine).

Restart open sessions afterwards. They loaded the hooks when they started and keep trying to
run them until they exit.

### Installed with `install.sh`

```sh
governor uninstall
```

If `governor` is not on your `PATH` (install links it into `~/.local/bin` only when that
directory already exists), run it from the plugin:

```sh
~/.claude/skills/build-governor/bin/governor uninstall
```

This route is the one the byte-identical guarantee is stated for
([§13.2a](design.md#132a-the-marketplace-route-and-why-the-plugin-is-build-governor)).

### Installed with Homebrew

Remove the plugin first, while the binary is still there to do it, then the formula:

```sh
governor uninstall
brew uninstall governor
```

### Installed through the plugin marketplace

The marketplace registration lives in Claude Code's own files, so the governor names it but
does not edit it ([§13.2a](design.md#132a-the-marketplace-route-and-why-the-plugin-is-build-governor)).
Remove it with Claude Code first, then remove the rest, then the binary if Homebrew supplied
it:

```sh
claude plugin uninstall build-governor@claude-governor
claude plugin marketplace remove claude-governor   # optional: forget the marketplace too
governor uninstall
brew uninstall governor        # if you installed the binary with Homebrew
```

If you skip the first step, the leftover registration is harmless: with the tree gone,
`governor plugin-path` finds nothing and the plugin does not load.

### Upgrading from `claude-governor`

The plugin used to be called `claude-governor`. Claude Code reserves that prefix for its own
plugins, so it was renamed `build-governor`
([hook-surface.md §23.5](hook-surface.md#235-the-claude--prefix-is-reserved)). Run
`governor install` from the new version. It removes the old
`~/.claude/skills/claude-governor/` tree, because two differently named copies would both load
and every hook would run twice. `governor doctor` fails until the old tree is gone. The slash
command, the MCP tool and the skill all moved to `build-governor` names. The repository, the
binary and the marketplace kept theirs.
