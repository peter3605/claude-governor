# claude-governor

A jobserver for Claude Code subagents.

Run several Claude Code sessions at once on a laptop and they will happily saturate it —
each one fans out into subagents with no idea the others exist, and every session degrades
together. `claude-governor` puts one CPU budget behind all of them: a small daemon watches
load and thermal pressure, and every session asks it before spawning.

- **Adaptive, not a constant.** The budget is an AIMD control loop with a utilisation
  setpoint, so it climbs when the machine is idle and halves the moment it isn't.
- **Thermal-aware.** On a fanless Mac, sustained load throttles the whole chassis. Anything
  other than a nominal thermal state takes the budget to the floor.
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
touched. Sessions already running pick the hooks up when they next start.

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

Before anything is governed, the machine is measured, so that the setpoint and the ceiling
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
