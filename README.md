# claude-governor

A jobserver for Claude Code subagents.

Run several Claude Code sessions at once on a laptop and they will happily saturate it —
each one fans out into subagents with no idea the others exist, and every session degrades
together. `claude-governor` puts one CPU budget behind all of them: a small daemon watches
load and thermal pressure, and every session asks it before spawning.

- **Adaptive, not a constant.** The budget is an AIMD control loop with a **run-queue**
  setpoint, so it climbs while the machine still has room and halves the moment it is
  oversubscribed. It deliberately does *not* control on CPU utilisation: a build machine at
  100% with a shallow queue is working, not in trouble, and utilisation saturates at 1.0
  exactly when the interesting distinctions start ([why](docs/climb-condition.md)).
- **Thermal-aware.** On a fanless Mac, sustained load throttles the whole chassis. A
  `serious` or `critical` thermal state takes the budget to the floor — not merely a
  non-nominal one, which GOV-15 measured at 2.85% of a 101-hour window with no corroborating
  evidence of throttling behind it.
- **Fails open.** If the daemon isn't running, every spawn proceeds ungoverned. A broken
  governor never bricks a session.
- **Tells the model the number.** An MCP tool lets a session ask how many agents it may run,
  so it plans a right-sized fan-out instead of being refused into shape.

Status: **Phase 0 — measuring.** The control loop is designed but not built; what runs today
is instrumentation only. See [`docs/design.md`](docs/design.md).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/peter3605/claude-governor/main/install.sh | sh
```

That fetches the binary for this OS and architecture and hands off to `governor install`,
which writes a plugin to `~/.claude/skills/claude-governor/`, merges a `statusLine` key into
`~/.claude/settings.json` after backing it up, and starts the daemon. It prints every path it
touched. **Claude Code binds hooks when a session starts, so every session you already have
open stays ungoverned until you restart it** — install counts them and names them, and
`governor doctor` fails while any of them is still running.

**It then checks its own work.** `governor install` finishes by running `governor doctor` and
printing the report, and exits non-zero if any check fails. An install can write every file
correctly and still govern nothing — a socket it cannot bind, an older `governor` earlier on
your `PATH` — and without this the first evidence of that is an ungoverned fan-out hours
later, in a session where nobody is reading an installer's output.

**Your first session after installing gets one short notice**, saying what is now happening,
how to read the status line, and how to turn it off. It is shown once and never again, and it
goes to you rather than into the model's context.

```sh
governor doctor       # is it working, and if not, what exactly do I type
governor status       # budget, holders, CPU, thermal
governor uninstall    # remove all of it
```

**A check can say `unknown`, and that is not a failure.** It means the check could not answer
its own question rather than that something is broken — most often the `spawn` check on a
freshly installed machine, which confirms the hook really fires by watching a real subagent
spawn go past it, and has not seen one yet. `doctor` exits `3` for that (`1` is a genuine
failure, `0` is all clear), and on most machines it settles the first time you run a subagent.
Each `unknown` line says what it could not see and what, if anything, you can do — on a host
whose Claude Code keeps no session registry for the governor to read, some of them cannot be
settled at all, and say so rather than pretending otherwise.

The reason any of this is reported instead of rounded up to `ok`: if a future Claude Code
renamed the tool the hook matches on, nothing would error. The hook would simply stop firing,
every other check would stay green, and no subagent would ever be governed again. Silence is
the only evidence that exists, so the report is built to say when all it has is silence.

**Removing it leaves nothing.** `governor uninstall` deletes the plugin, restores whatever
`statusLine` you had before — byte for byte, formatting and key order included — stops the
daemon and removes its socket. `--purge` also removes the config, the first-run notice marker and the
sample log, which are otherwise kept. There is a test that installs into a scratch home, uninstalls, and fails on
any difference in a recursive digest of the whole directory; what is being asked of you is to
run a hook on every subagent spawn, and being able to prove it comes out clean is most of why
that should be acceptable.

Installing twice changes nothing the second time. The five hooks live in the plugin's own
`hooks/hooks.json`, so the only key touched in your `settings.json` is `statusLine`, and hooks
of your own are left exactly where they are.

To build and install from a clone instead:

```sh
make build
./bin/governor install
```

## Phase 0

Before anything is governed, the machine is measured, so that the thresholds and the ceiling
come from traces rather than from a number that felt about right on one laptop.

```sh
make build
governor trace install      # merges three hooks into ~/.claude/settings.json, after a backup
governor trace status       # where the log is, and whether the sampler is up
governor trace uninstall    # removes them again, restoring the original bytes
```

That installs a `SubagentStart`/`SubagentStop` hook that records each subagent, and a
`SessionStart` hook that lazily starts a 1 Hz sampler. Both append newline-delimited JSON to
`~/.claude/governor/samples.jsonl`. Nothing gates, blocks or denies anything, the hooks exit 0
whatever happens, and the sampler stops on its own once no session has been running for five
minutes.

Each sample carries host CPU, load average, free memory and thermal state, plus the CPU of
the `claude` process tree split two ways — the session processes alone, and those plus every
descendant. The gap between the two is the point: it separates what the agents burn from what
they launched.
