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
