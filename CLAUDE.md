# claude-governor

A Claude Code plugin that budgets subagent concurrency across every session on this machine
from live CPU and thermal load. One daemon owns the budget; the hooks enforce it; an MCP tool
tells the model how many agents it may run instead of letting it guess.

**The spec is `docs/design.md`.** Read it before writing code. It carries the invariants, the
wire protocol, and — in §10 — four facts about the Claude Code hook surface that are NOT yet
confirmed on this machine. Do not build on an unconfirmed one; confirm it first, then correct
the doc.

---

## Launch Control — work is tracked in Notion

This repo's remaining work lives in the **Launch Control** workspace in Notion. Story IDs for
this project are prefixed **`GOV-`**. Connection details are in `.claude/launch-control.json`.

### The loop

| | |
|---|---|
| `/lc:next` | Pull the next unblocked story a session can actually do |
| `/lc:mine` | The blockers only YOU can do, longest lead time first |
| `/lc:start GOV-01` | Claim one, flip it to In Progress, load its Done-when and traps |
| `/lc:done` | Verify the acceptance criteria, tick it off, unblock what was waiting |
| `/lc:status` | Where this project stands |
| `/lc:groom <thing>` | File something you discovered mid-session |
| `/lc:reconcile` | Re-check open stories against the actual repo and correct the board |

### Rules

- **Bind before you build.** Substantive work should be attached to a story. If the user asks
  for something not on the backlog, `/lc:groom` it first, then `/lc:start` it. One session, one story.
- **Done means the criteria are met, not that the code is written.** `/lc:done` re-reads the
  story's **Done when** field and actually runs whatever verifies it. If a clause is not
  satisfied, say which one and leave the story open.
- **Read the traps field.** **Notes and traps** holds the failure modes this design already
  anticipates. It is not decoration.
- **Code ships through a PR.** `/lc:done` branches as `gov-01-short-slug`, commits with the story
  ID leading the subject, pushes, opens a PR, waits for CI, and merges once every check is
  green. A story stays `In Review` while its PR is open and unblocks nothing.
- **CI is evaluated, not delegated.** Never `gh pr merge --auto`, never `--admin`. Zero checks
  having run is not a pass.
- **Dependencies are a relation, not text.** `Blocked by` holds page links; `Blocked by IDs`
  and `Blocker status` are rollups that render in Notion but return opaque handles to the API.
  `Status = Ready` already means unblocked.
- Prefer Notion `query_data_sources` in **view mode** — unquota'd on this plan. SQL is metered.

---

## Project rules specific to this repo

- **Go, one static binary.** The gate runs on every subagent spawn inside a hook Claude Code
  is blocked on. Interpreter startup is the thing being avoided, so nothing on the hot path
  may be Node, Python, or a shell script. Do not "temporarily" prototype the gate in Python.
- **Fail open is not negotiable.** Any code path where the gate could hang, crash, or block
  past its deadline without the spawn proceeding is a bug, regardless of what else it does.
  Its test comes before its feature.
- **No `sudo`, ever.** `powermetrics` is off limits; thermal comes from
  `ProcessInfo.thermalState`. A dependency on elevated privileges kills the install story.
- **Never emit `permissionDecision: "allow"`.** It suppresses the user's own permission
  prompts. The grant path is exit 0 with empty stdout.
- **Measure before you control.** Phase 0 (GOV-01/02) is instrumentation only, and its traces
  are what tune the setpoint. Do not skip ahead to the control loop and guess at constants.
- **Install is reversible and idempotent.** Running install twice is a no-op the second time;
  uninstall leaves no file, hook, setting or process behind. Never blind-write the user's
  `settings.json` - read, back up, merge, write atomically, and keep the backup so uninstall
  can restore it exactly. What is being asked of a stranger here is to run a hook on every
  subagent spawn, and the only thing that makes that reasonable is being able to undo it.
- **Seams are one interface with one implementation.** `internal/transport`, `internal/budget`,
  `internal/lease.Liveness`, `internal/signals` and `internal/config` exist so tier two
  (design.md section 12) never has to be retrofitted. Do NOT write a second implementation of
  any of them before tier two actually exists - if a change needs one to compile, it is
  premature. Equally, do not collapse the interface away because there is only one impl.
- **Defaults come from the machine, not from a constant.** `governor calibrate` measures this
  host and writes the numbers. Nothing tuned on Peter's laptop ships as anyone else's default.
- **macOS and Linux, both, from the start.** Anything platform-specific goes behind the
  `Sampler` interface with a `_darwin.go` / `_linux.go` pair. Thermal has no Linux equivalent
  and reports `unknown`, which is treated as nominal - never as a cut trigger.
- **Every protocol message carries `v`.** An unrecognised version is answered, not ignored,
  and the gate treats that answer like an unreachable daemon. During an upgrade there will be
  a daemon of one version and gates of another; ungoverned spawns are acceptable, a stalled
  session is not.
