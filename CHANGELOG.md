# Changelog

What changed in each release of `governor`, written for the person running it. Each entry
covers what you will notice: a behaviour, a command, an install or upgrade step. It does not
list commit subjects; the pull requests are linked for anyone who wants the detail.

## Format

This file follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Each release
has its own `## [X.Y.Z] - YYYY-MM-DD` heading, newest first, and its changes are grouped
under `Added`, `Changed`, `Fixed` and `Removed`. **There is no `Unreleased` section.** The
entry for a release is written in the pull request that prepares it, and that PR merges
before the tag is pushed.

**The text of an entry is the GitHub release's notes, word for word.** `release.yml` runs
`tools/changelog` against the tag being built and publishes what it prints as the release
body. A tag with no entry here, or with an empty entry, fails before anything is built or
published. Write the entry first.

## Versioning

The version number is [semver](https://semver.org/spec/v2.0.0.html), with the usual 0.x
caveat: **before 1.0.0, a minor release may break things, and a patch release never does.**

For this project, a **breaking change** is anything that makes a working install stop
working, or behave differently, until you take a manual step. Examples: a config key that is
read differently, a removed command or flag, a hook that has to be re-registered, or an
upgrade that needs anything beyond `governor install`. A breaking change is called out at the
top of its entry, together with the step you need to take.

`governor version` prints three numbers, and they move independently:

```
governor 0.1.2       the release: this file, and the tag
protocol 1           the "v" field on every message between the hooks and the daemon
config schema 1      the "schema" key in ~/.claude/governor/config.json
```

- **Protocol.** A protocol bump is never shipped as a patch release, and its entry says so.
  A protocol bump is not breaking by itself. During an upgrade, a gate that meets a daemon
  of the other protocol treats it as unreachable and lets the spawn through ungoverned until
  `governor install` replaces the daemon. A protocol change that meant anything worse than
  that would be breaking.
- **Config schema.** A schema bump migrates `config.json` forward when the file is read, and
  its entry says so. An older binary refuses a newer schema and runs on its built-in
  defaults rather than rewriting a file it does not understand. So a schema bump is
  breaking only if you downgrade afterwards, and the entry says that too.

Releases are fixed by a new patch version and never by moving a tag. [0.1.1](#011---2026-10-29)
is the first example.

## [0.1.7] - 2026-11-22

Protocol and config schema are unchanged (`protocol 1`, `config schema 1`). Upgrading takes
no extra step beyond the usual one: rerun the install one-liner, or `brew upgrade governor`
followed by `$(brew --prefix)/opt/governor/bin/governor install`. That install registers an
eighth hook (below), and until you run it, a backgrounded build keeps its slot the old way.
Restart open Claude Code sessions afterwards, because a session binds its hooks when it starts.

### Added

- **`governor doctor` has a `trace` check.** It finds Phase 0 instrumentation left behind
  by an older binary: `governor trace` hooks in `~/.claude/settings.json` that run a
  different build from your governor hooks, and a `governor trace sample` process from
  another build or one still writing to a log file that was rotated or deleted. A sampler
  writing to a deleted file grows without limit and nothing else would show it, so that
  case is a failure. The fix line names the command to run.
  ([#103](https://github.com/peter3605/claude-governor/pull/103))

### Changed

- **`governor trace install` replaces trace hooks from another binary.** Before, it added
  its own hooks next to the old ones. Now it removes any trace hooks that name a different
  binary in the same backed-up write. `governor install` and `brew upgrade` still never
  touch trace hooks, because tracing is opt-in.
  ([#103](https://github.com/peter3605/claude-governor/pull/103))

### Fixed

- **A build Claude Code moves to the background gives its slot back when it really ends.**
  This covers a build that passes its Bash timeout and a build run with
  `run_in_background`. Before, such a build held its slot for the full five-minute limit,
  however soon it finished, and the same session's next build was refused with the bare
  "Not enough free build slots". The slot is now released when Claude Code announces that
  the task ended, when the subagent that started it stops, or when the task's output file
  shows it exited. Until then, a refusal caused by it names the background task. A
  backgrounded build still running at five minutes is released at that point, as before.
  The new hook is a `UserPromptSubmit` registration.
  ([#102](https://github.com/peter3605/claude-governor/pull/102))

## [0.1.6] - 2026-11-14

Protocol and config schema are unchanged (`protocol 1`, `config schema 1`). Upgrading takes
no extra step beyond the usual one: rerun the install one-liner, or `brew upgrade governor`
followed by `$(brew --prefix)/opt/governor/bin/governor install`. That install registers a
seventh hook (below), so until you run it, a failed build keeps its slot the old way. Restart
open Claude Code sessions afterwards; a session binds its hooks when it starts.

### Added

- **`governor --version`** prints the same three lines as `governor version`. Before, it
  answered `unknown command "--version"`.
  ([#89](https://github.com/peter3605/claude-governor/pull/89))
- **The daemon log records what the budget did and why.** Every cut, climb and restore is
  a line naming its cause and the reading against its threshold, for example
  `budget cut 4->1 cause=run-queue rq=7.10 cut=6.59`. Every released slot is a line saying
  which session held it, for how long, and what released it. Every line now starts with a
  timestamp. `~/.claude/governor/daemon.log` rotates at 4 MiB to `daemon.log.1` and keeps
  that one older file. `governor uninstall` removes both.
  ([#90](https://github.com/peter3605/claude-governor/pull/90))

### Changed

- **Slot counts are whole numbers, and agents are counted beside them.** The status line,
  `governor status`, refusals and the `request_build` tool used to show figures like `3.8/4`.
  They now show `4/4 slots · 1 agent`: a build's share rounds up, and subagents are listed
  separately rather than rounded up to a slot each. Admission itself is unchanged; only the
  display rounds. ([#93](https://github.com/peter3605/claude-governor/pull/93))
- **`docker compose` is classified by its subcommand.** Before, every `docker compose`
  call was gated as a heavy build, including `docker compose exec app psql`, `logs` and `ps`.
  Now `build`, `watch` and anything with `--build` stay heavy, `up`, `run` and `create` are
  medium, and the control-plane subcommands are not gated at all. `exec` and `run` are
  classified by the command they run, so `docker compose exec app pytest` is still a test run.
  ([#91](https://github.com/peter3605/claude-governor/pull/91))

### Fixed

- **A failed build releases its slot at once.** A command that exits non-zero raises a
  different hook event from one that succeeds, and the governor was not listening for it. So
  every failed build kept its slot for five minutes, and the retry from the same session was
  refused as if another session held it. The new hook releases it immediately, and a refusal
  that is caused by the session's own earlier build now says so.
  ([#88](https://github.com/peter3605/claude-governor/pull/88))
- **A subagent can run builds and tests.** A `go test` or `make` from inside a subagent was
  refused with the message meant for a subagent spawning further subagents. A subagent's
  builds now wait for a slot like any other build. Subagents still cannot spawn subagents.
  ([#95](https://github.com/peter3605/claude-governor/pull/95))
- **A waiting build is no longer refused while there is room for it**, and no refusal has an
  empty cause any more. A build that had waited its full time was refused even when the
  budget had room by then, and the log line said `cause=` with nothing after it. It now gets
  one last look before it is refused, and every refusal names its reason.
  ([#92](https://github.com/peter3605/claude-governor/pull/92))
- **`governor limit` and `governor pause` hold for waiting requests.** A pin set with
  `governor limit` applied to new requests but not to ones already waiting, nor to the
  `request_build` tool's advice. Both now use the pinned budget, and requests already waiting
  when you `pause` are let through, the same as new ones.
  ([#94](https://github.com/peter3605/claude-governor/pull/94))

## [0.1.5] - 2026-11-02

Protocol and config schema are unchanged (`protocol 1`, `config schema 1`). From 0.1.3 or
0.1.4, upgrading takes no extra step: rerun the install one-liner, or `brew upgrade governor`.
From 0.1.2 or earlier with Homebrew, follow
[0.1.3's upgrade step](https://github.com/peter3605/claude-governor/releases/tag/v0.1.3).

### Added

- **`governor doctor` has a `tracelog` check.** It lists each trace log file with its size.
  It warns only about a file over 32 MiB, which means a log set aside from before this
  release. ([#84](https://github.com/peter3605/claude-governor/pull/84))

### Changed

- **`governor uninstall` names the trace log it keeps.** A plain uninstall still leaves the
  log, because it is your record of every gated build, but it now prints each file it kept
  and its size. `governor uninstall --purge` removes them, including any set-aside file.
  ([#84](https://github.com/peter3605/claude-governor/pull/84))

### Fixed

- **The trace log no longer grows without limit.** `~/.claude/governor/samples.jsonl` records
  every gated build, and before this release nothing ever trimmed it. On one machine it
  reached 699 MB in six weeks. It now rotates at 16 MiB to `samples.jsonl.1` and keeps that
  one older file, so it never takes more than about 32 MiB.
  ([#84](https://github.com/peter3605/claude-governor/pull/84))

  If your log is already over 32 MiB, the first write after you upgrade does not delete it.
  It renames it to `samples.pre-bound-<date>.jsonl` and leaves it alone. Nothing reads that
  file, so you can keep it, compress it or delete it.

## [0.1.4] - 2026-11-02

Protocol and config schema are unchanged (`protocol 1`, `config schema 1`).

**This is v0.1.3 for Homebrew users.** The governor itself is the same code as v0.1.3, so
everything in [0.1.3's release notes](https://github.com/peter3605/claude-governor/releases/tag/v0.1.3) applies, including its upgrade step: after
`brew upgrade governor`, run `$(brew --prefix)/opt/governor/bin/governor install`, using the
full path.

### Fixed

- **v0.1.3 never reached the Homebrew tap.** Its formula was sound and passed its own
  Homebrew check. The release's clean-macOS install check failed, so the formula was held
  back. That check required doctor to report `ok calibration`, but on a busy machine install
  skips the calibration probe and doctor correctly warns `b_max was not measured`. The check
  now accepts that one warning and still fails on anything else.
  ([#81](https://github.com/peter3605/claude-governor/pull/81))

  v0.1.3 was left published rather than re-tagged. The install one-liner served it, and it
  works.

## [0.1.3] - 2026-11-02

Protocol and config schema are unchanged (`protocol 1`, `config schema 1`).

**Upgrading from 0.1.2 or earlier with Homebrew:** after `brew upgrade governor`, run
`$(brew --prefix)/opt/governor/bin/governor install`, using the full path. On those versions
a bare `governor` can still resolve to install's own `~/.local/bin` link to the old binary,
which reinstalls the old version over itself. That full-path install removes the link, and
a bare `governor install` works from then on.

### Added

- **`governor doctor --report`** prints one block you can paste into a public issue: the
  version, install route, thermal source, Claude Code version, budget, doctor's findings,
  the daemon log tail and the last few gated builds. Paths, IDs and anything shaped like a
  credential are scrubbed out, and command text and working directories are never included.
  The new issue templates ask for it first.
  ([#78](https://github.com/peter3605/claude-governor/pull/78))
- **`governor doctor` has a `loaded` check.** It fails when the running daemon is serving a
  different calibration from the one in `config.json`.
  ([#68](https://github.com/peter3605/claude-governor/pull/68))
- [CHANGELOG.md](https://github.com/peter3605/claude-governor/blob/main/CHANGELOG.md) (this
  file) and
  [CONTRIBUTING.md](https://github.com/peter3605/claude-governor/blob/main/CONTRIBUTING.md).
  From this release on, the GitHub release notes are this file's entry, word for word.
  ([#77](https://github.com/peter3605/claude-governor/pull/77),
  [#70](https://github.com/peter3605/claude-governor/pull/70))

### Changed

- **A calibration takes effect without a restart.** The daemon re-reads `config.json` on
  the tick after it changes. That covers both `governor calibrate` and the calibration
  `governor install` writes. If the file goes bad while the daemon is running (unreadable,
  malformed, or from a newer schema), the daemon keeps the values it was serving and logs
  the problem once. It no longer falls back to the looser built-in defaults.
  ([#68](https://github.com/peter3605/claude-governor/pull/68),
  [#76](https://github.com/peter3605/claude-governor/pull/76))
- **The budget recovers by itself.** A budget that has been cut climbs back to the ceiling
  once the machine has been idle for about a minute: no slot held, nothing waiting, and the
  run queue low. Before this release it waited for demand, which a low budget suppresses, so
  a budget could stay at 1 on an idle machine. A raised ceiling now takes effect on the same
  tick. ([#71](https://github.com/peter3605/claude-governor/pull/71))
- **`governor calibrate` gives steadier answers.** It now uses a probe result only when its
  repetitions agree. It prints every repetition. When it cannot get a trustworthy answer, it
  keeps the ceiling it already had and says so, instead of writing a new one.
  ([#72](https://github.com/peter3605/claude-governor/pull/72))

### Fixed

- **A brew upgrade really upgrades.** On the Homebrew route, `governor install` no longer
  creates a `~/.local/bin/governor` link that shadows the brew binary, and it removes the
  link an earlier version left there. Wherever doctor's fix lines and the brew caveat name a
  command, they now name the binary that will actually do the upgrade.
  ([#67](https://github.com/peter3605/claude-governor/pull/67),
  [#69](https://github.com/peter3605/claude-governor/pull/69),
  [#71](https://github.com/peter3605/claude-governor/pull/71))
- **A daemon started by a hook read a different `config.json`** from the one `calibrate`
  wrote, and silently ran on the built-in ceiling. Every part of the tool now reads the
  same file. ([#68](https://github.com/peter3605/claude-governor/pull/68))
- **`governor status` called a session ungoverned while it held a slot** after a daemon
  restart. It now uses the same wording as doctor and the status line. A replaced daemon
  also learns about sessions that were already running, so fair share counts them again.
  ([#74](https://github.com/peter3605/claude-governor/pull/74))

## [0.1.2] - 2026-10-31

### Added

- The daemon now reports which build it is, and every other part of the tool checks it. A
  daemon left running from an older binary used to go on serving every session after an
  upgrade, with nothing to tell you. Now:
  - the status line reads `stale daemon: governor install`;
  - `governor doctor` fails its new `build` check;
  - `governor install` replaces that daemon, even when both report the same version number.

  `brew upgrade` and a marketplace update do not restart the daemon. Run `governor install`
  after either one. ([#66](https://github.com/peter3605/claude-governor/pull/66))
- User documentation:
  - [the guide](https://github.com/peter3605/claude-governor/blob/main/docs/guide.md) to living with the governor: the status line, refusals,
    overrides, troubleshooting by symptom, and uninstalling;
  - [how it works](https://github.com/peter3605/claude-governor/blob/main/docs/how-it-works.md);
  - an index of `docs/`.

  ([#64](https://github.com/peter3605/claude-governor/pull/64))

### Fixed

- A freshly started daemon served a budget of 1 until its first sample landed, so the
  session that started it was throttled hardest. It now starts at this machine's ceiling.
  ([#65](https://github.com/peter3605/claude-governor/pull/65))

## [0.1.1] - 2026-10-29

### Fixed

- **v0.1.0 could not be installed with Homebrew.** Its `governor.rb` carried a `version`
  line that Homebrew already reads from the download URL, and `brew audit --strict` rejects
  that. The release's own clean-macOS check failed on it, so v0.1.0's formula was never
  copied into the tap. v0.1.1 is the same code as v0.1.0 with that line removed
  ([#62](https://github.com/peter3605/claude-governor/pull/62)). It is the first version
  available from `brew install peter3605/claude-governor/governor`.

  v0.1.0 was left published rather than re-tagged, because a tag that changes under people
  who already downloaded it is worse than a bad release. The install one-liner was never
  affected: it fetches the binaries, which were sound.

## [0.1.0] - 2026-10-29

The first release.

### Added

- **One budget for every Claude Code session on the machine.** A small daemon watches the
  run queue and, on a Mac, the thermal state. Every subagent spawn and every expensive build
  asks it for a slot first. The budget climbs while the machine has room, halves when the
  machine is oversubscribed, and drops to the floor on a `serious` or `critical` thermal
  state. It never needs `sudo`.
- **It fails open.** If the daemon is down, slow, the wrong version or anything else goes
  wrong, the spawn or build goes ahead ungoverned. A broken governor never stalls a session.
- **Install by any of three routes**, all ending in the same single plugin,
  `build-governor`:
  - the `install.sh` one-liner;
  - `brew install` followed by `governor install` (from 0.1.1; see above);
  - the Claude Code plugin marketplace.

  Install backs up and merges `settings.json` rather than overwriting it, and running it
  twice changes nothing. ([#17](https://github.com/peter3605/claude-governor/pull/17),
  [#61](https://github.com/peter3605/claude-governor/pull/61))
- **`governor uninstall`** removes the plugin and stops the daemon. It puts `settings.json`
  back byte for byte.
- **A status line** showing the live slot count. It says so when a session is ungoverned
  and why: started before install, no daemon, or paused.
  ([#15](https://github.com/peter3605/claude-governor/pull/15))
- **`governor doctor`**, which checks the install end to end and names the command that
  fixes each failure. ([#16](https://github.com/peter3605/claude-governor/pull/16))
- **`governor calibrate`**, which measures this machine and writes its own setpoint and
  ceiling into `config.json`, instead of shipping numbers tuned on someone else's laptop.
  ([#37](https://github.com/peter3605/claude-governor/pull/37))
- **The `governor__request_build` tool.** It lets the model ask whether there is room
  before it starts a build, and wait a few seconds if there isn't, rather than being
  refused. ([#38](https://github.com/peter3605/claude-governor/pull/38))
- **Overrides**: `governor limit N`, `pause`, `resume` and `boost`, also available as the
  `/build-governor:governor` slash command.
  ([#44](https://github.com/peter3605/claude-governor/pull/44))
