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

Status: **designed, not built.** See [`docs/design.md`](docs/design.md).
