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
