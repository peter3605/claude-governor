---
name: heavy-build
description: How to act on claude-governor's answer before an expensive build or test run on this machine (go test, cargo, pytest, make, xcodebuild, npm run build). Not for ordinary shell work.
---

# Running a heavy build on a governed machine

claude-governor shares one CPU budget between every Claude Code session on this machine. A build
or test suite takes a slot while it runs; ordinary commands take nothing and are never delayed.

## Before an expensive build or test run

Call `mcp__plugin_claude-governor_governor__governor__request_build` with `command` set to the
exact command line and `why` set to a few words on what it is for.

- **`granted: true`** — run the command now.
- **`granted: false`** — other sessions are using every slot. Wait about `wait_hint_ms`, then ask
  again. It is not a fault in your command. Running it anyway is refused by the hook, so asking
  first only saves you that round trip.
- **`granted: false` saying nested subagent spawns get no build slots** — retrying will not
  change it. Report the command back to the main session and let it run the build.
- **`granted: true` with a reason beginning `governor unavailable`** — the governor is not
  running. Go ahead; nothing is capped.

## What the answer never changes

The governor decides **when** a build or test run happens, never **whether**. Do not skip, shrink,
or drop a test run because of a refusal, and do not swap the suite for a narrower one to get in
sooner — wait and run what you were going to run. A session that stops testing to stay under the
budget is the failure this tool must not cause.

## What not to ask about

Anything that is not a build or a test suite: reading or searching files, `git`, `ls`, `cat`,
`grep`, editing, starting a dev server, one quick script. If you are unsure whether a command is
expensive, do not ask — the hook classifies every command itself and gates the ones that matter,
so a forgotten question costs nothing but a wait.
