---
description: Show or override the machine-wide build budget (status, limit N, pause, resume, boost)
argument-hint: "[status | limit <slots> | pause | resume | boost] [for <duration>]"
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/bin/governor:*)
---

The governor's current state on this machine:

!`${CLAUDE_PLUGIN_ROOT}/bin/governor status`

The user typed: `$ARGUMENTS`

Act on it as follows. Treat an empty argument, or `status`, as a request to read the state
above — it is already here, so **do not run any command**; just report it in a sentence or two.

For anything else, run exactly one command and report what it prints:

| The user typed | Run |
|---|---|
| `limit N` or `limit N for D` | `${CLAUDE_PLUGIN_ROOT}/bin/governor limit N for D` |
| `pause` or `pause for D` | `${CLAUDE_PLUGIN_ROOT}/bin/governor pause for D` |
| `resume`, `release`, `unpin` | `${CLAUDE_PLUGIN_ROOT}/bin/governor resume` |
| `boost` or `boost for D` | `${CLAUDE_PLUGIN_ROOT}/bin/governor boost for D` |

`D` is a Go duration such as `30m` or `2h`. Omit `for D` when the user did not give one; the
command supplies its own default and says what it chose.

If the argument matches none of those, say so and list the four verbs. Do not guess, and do not
invent a fifth verb — every one of these changes how much work every session on this machine is
allowed to run at once.

Three things worth telling the user when they come up, because each is a question they will
otherwise ask next:

- **Every override expires**, and the command prints when. Nothing here is permanent and
  nothing survives a daemon restart.
- **`pause` stops the cap, not the bookkeeping.** Leases are still recorded, so `status` keeps
  telling the truth about what is running while a pause is in force.
- **`boost` changes who refills a contended budget, not who reaches it first.** On an idle
  machine it changes nothing at all, which is the expected result rather than a failure.
