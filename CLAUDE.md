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
| `/next` | Pull the next unblocked story a session can actually do |
| `/mine` | The blockers only YOU can do, longest lead time first |
| `/start GOV-01` | Claim one, flip it to In Progress, load its Done-when and traps |
| `/done` | Verify the acceptance criteria, tick it off, unblock what was waiting |
| `/status` | Where this project stands |
| `/groom <thing>` | File something you discovered mid-session |
| `/reconcile` | Re-check open stories against the actual repo and correct the board |

### Rules

- **Bind before you build.** Substantive work should be attached to a story. If the user asks
  for something not on the backlog, `/groom` it first, then `/start` it. One session, one story.
- **Done means the criteria are met, not that the code is written.** `/done` re-reads the
  story's **Done when** field and actually runs whatever verifies it. If a clause is not
  satisfied, say which one and leave the story open.
- **Read the traps field.** **Notes and traps** holds the failure modes this design already
  anticipates. It is not decoration.
- **Code ships through a PR.** `/done` branches as `gov-01-short-slug`, commits with the story
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
