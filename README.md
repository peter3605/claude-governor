# claude-governor

Two promises come first, because what this asks of you is to run a hook on every subagent spawn
and every build:

**It fails open.** If the daemon is down, unreachable, slow, the wrong version, or anything else
goes wrong, the spawn or build goes ahead ungoverned. A broken governor can never stall a
session. Every hook has a hard deadline inside Claude Code's, and `TestGateFailsOpen` drives the
gate through each failure shape it knows — no daemon, nobody listening on the socket, a connect
or a reply that never comes, a connection closed mid-exchange, a garbled or truncated reply, an
unknown protocol version, a malformed hook payload — and requires every one to let the work
through.

**It comes out clean.** `governor uninstall` removes the plugin, stops the daemon, and puts your
`settings.json` back byte for byte. A test installs into a scratch home, uninstalls, and fails on
any difference in a digest of the whole directory; on every push the same check runs on clean
Debian and Alpine containers, and on every release on a clean macOS machine, from the published
download.

**What it is: a jobserver for Claude Code subagents.**

Run several Claude Code sessions at once on a laptop and they will happily saturate it —
each one fans out into subagents and builds with no idea the others exist, and every session
degrades together. `claude-governor` puts one budget behind all of them: a small daemon watches
load and thermal pressure, and every session asks it before spawning an agent or starting an
expensive build.

- **Adaptive, not a constant.** The budget is an AIMD control loop with a **run-queue**
  setpoint, so it climbs while the machine still has room and halves the moment it is
  oversubscribed. It deliberately does *not* control on CPU utilisation: a build machine at
  100% with a shallow queue is working, not in trouble, and utilisation saturates at 1.0
  exactly when the interesting distinctions start ([why](docs/climb-condition.md)).
- **Thermal-aware.** On a fanless Mac, sustained load throttles the whole chassis. A
  `serious` or `critical` thermal state takes the budget to the floor — not merely a
  non-nominal one, which GOV-15 measured at 2.85% of a 101-hour window with no corroborating
  evidence of throttling behind it.
- **Tells the model first.** An MCP tool lets a session ask whether the machine has room for a
  build before it starts one — so it waits a few seconds instead of being refused into shape.
  It answers cheap commands without contacting the daemon at all, and grants everything when
  the daemon is down.

The design, its invariants and the measurements behind every constant are in
[`docs/design.md`](docs/design.md).

## Install

Any one of these. They all end in the same place: one plugin, `build-governor`, in
`~/.claude/skills/build-governor/`, and one `governor` binary.

```sh
# the one-liner
curl -fsSL https://raw.githubusercontent.com/peter3605/claude-governor/main/install.sh | sh

# or Homebrew, then install the plugin
brew install peter3605/claude-governor/governor
governor install
```

`install.sh` fetches the binary for this OS and architecture and hands off to `governor
install`, which writes the plugin, merges a `statusLine` key into `~/.claude/settings.json`
after backing it up, and starts the daemon. It prints every path it touched. **Claude Code binds
hooks when a session starts, so every session you already have open stays ungoverned until you
restart it** — install counts them and names them, and `governor doctor` fails while any of them
is still running.

**Through the Claude Code plugin marketplace**, once the binary is installed by either route
above:

```sh
claude plugin marketplace add peter3605/claude-governor
claude plugin install build-governor@claude-governor
```

The marketplace entry does not carry a second copy of the plugin. It runs `governor
plugin-path`, which points Claude Code at the tree `governor install` already wrote, so both
registrations load the same files and the same binary and cannot double up. Claude Code asks
you to approve that command once, **in your own terminal** — it refuses to accept one from inside
a session.

**It measures your machine before it checks anything.** `governor install` ends by running
`governor calibrate`, which reads your core count, how many of them are performance cores where
the platform says, your RAM, and — the number that actually decides how the governor behaves —
what your machine's run queue already sits at while you work. It then runs a short, announced
load probe, about four seconds, to find out how many cores this hardware really delivers in
parallel, which is not the number it advertises: a laptop with four performance and four
efficiency cores measures about six. The limits it writes are derived from those readings and
recorded with a note saying when they were taken and from what.

None of the shipped constants were measured on your machine, and that is the whole point of
this step. It is also why the advice is to **re-run `governor calibrate` while you are actually
working** — the baseline it places your threshold above is whatever the machine was doing in
the preceding minute, and a machine measured while idle will throttle you once it is not.

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
governor calibrate    # re-measure this machine; run it while you are working
governor uninstall    # remove all of it
```

**When you already know a heavy build is coming, say so** rather than waiting for the loop to
be hurt by it and work it out. From inside a session that is `/build-governor:governor`; from
a terminal it is the same four verbs:

```sh
governor limit 2 for 30m   # pin the budget, whatever the loop thinks
governor pause for 15m     # stop capping, without stopping the daemon
governor resume            # give it back early
governor boost             # double this session's share of a contended budget
```

**Every one of them expires, and the status line says so while it holds** — `2/2 slots -
pinned`, or `governor: gating paused` in place of the count. That is deliberate: a manual
override nobody can see is how a governor stops being trusted, because the machine feels wrong
and the person who set it an hour ago has no reason to suspect themselves. Nothing here
survives a daemon restart, and the longest any of it can be asked for is a day.

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

**Removing it.** `governor uninstall` deletes the plugin, restores whatever `statusLine` you had
before — byte for byte, formatting and key order included — stops the daemon and removes its
socket. `--purge` also removes the config, the first-run notice marker and the sample log, which
are otherwise kept. If you added the marketplace entry, run `claude plugin uninstall
build-governor@claude-governor` first: that registration lives in Claude Code's own files, so
`governor uninstall` names it rather than editing them.

Installing twice changes nothing the second time. The six hooks live in the plugin's own
`hooks/hooks.json`, so the only key touched in your `settings.json` is `statusLine`, and hooks
of your own are left exactly where they are.

**Upgrading from a version named `claude-governor`.** The plugin was renamed `build-governor`
because Claude Code reserves the `claude-` prefix for its own plugins. Run `governor install`
from the new version: it removes the old `~/.claude/skills/claude-governor/` tree, because two
differently named copies would both load and every hook would run twice. `governor doctor`
fails until it is gone.

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
