# Contributing to claude-governor

This is the maintainer's guide: how to build the binary, run the tests, measure Claude Code's
hook surface, and cut a release. It is written for whoever does those things next, a
contributor or a future maintainer, and it assumes nothing that lives outside this
repository.

Read [`docs/design.md`](docs/design.md) before changing code. It is the contract: the
invariants, the wire protocol, and (in [§10](docs/design.md#10-hook-surface--confirmed)) the
hook-surface facts everything else is built on. [`docs/README.md`](docs/README.md) says which
of the other documents is a spec, a guide or a measurement record.

The traps below were each learned by getting them wrong once. They are stated as rules
because the failure they prevent is usually silent.

---

## Building

```sh
make build    # bin/governor, for this machine
make check    # go vet, go test ./..., and a gofmt check: what CI's first job runs
make cross    # compile check for darwin and linux, arm64 and amd64
```

The project is one static Go binary, and nothing on the hot path may be anything else. The
gate runs inside a hook that Claude Code is blocked on, on every subagent spawn, so an
interpreter's startup time is exactly the cost being avoided. Do not prototype the gate in
Python or shell, not even temporarily.

### `make cross` builds thermally blind darwin binaries. Never release them.

`make cross` builds all four targets with `CGO_ENABLED=0`, because a local cross-compile
cannot link another platform's C toolchain. The darwin binaries it produces build cleanly and
run cleanly, and they are **thermally blind**. Thermal state comes from
`ProcessInfo.thermalState` through a small cgo shim (`internal/signals/thermal_darwin.go`).
Without cgo the shim is not compiled, so the binary reports thermal `unknown`, and
[design.md §5.3](docs/design.md#53-thermal) treats `unknown` as nominal. A throttling Mac then
looks exactly like an idle one and the budget never drops to the floor. Nothing errors, and
nothing in `install` or `doctor` looks wrong.

`make cross` is a compile check and nothing more. Release artefacts are built by
`.github/workflows/release.yml`, which builds darwin **on a macOS runner with cgo on**. The
one-line acceptance check for any darwin binary is `governor status` on a Mac: if it prints
`thermal unknown`, it is the wrong binary.

### Never overwrite an installed binary in place

`install.sh`, `governor install` and every upgrade put the new binary in place with
rename(2), never by copying over the old file. If you `cp` onto a Mach-O binary that is
running, its code signature becomes invalid and the kernel SIGKILLs every later execution
with exit 137 and no output at all. See
[design.md §13.4](docs/design.md#134-config-and-upgrade). If you replace a binary by hand
while testing, do the same: write a new file beside it, then `mv` it over the old one.

A side effect worth knowing: the first execution of any newly written binary costs about
240 ms on macOS while the signature is validated. The first subagent spawn after an install
or upgrade pays that once. It is not a latency regression.

---

## Testing

```sh
go test ./...          # what you run while working
go test -race ./...    # what CI runs (ubuntu, in the "vet and test" job)
make leakcheck         # the suite, failing if it leaves a process behind
```

### How long it takes

Every package except `cmd/governor` finishes in about a second. `cmd/governor` is the whole
cost. Measured on 2026-11-01 on an Apple-silicon Mac, without `-race`, the package took
**about 6½ minutes** (395 s of test time, 475 s of wall clock including compilation). CI's
`-race` run on ubuntu took about 5¾ minutes for the package (344 s) on 2026-10-31. Budget for
that before you run `go test ./...` and wait on it.

Most of that time goes to the install, upgrade and uninstall tests. Each one builds the
governor binary, starts a real daemon and stops it again, at roughly 10 to 20 seconds per
test. When you are working on one area, run just that area:

```sh
go test ./cmd/governor -run 'TestGate' -v
```

A laptop sleeping on battery looks exactly like a hung test run: Go's timeout clock pauses with the machine, so no timeout fires and no goroutine dump appears.

### The tests that depend on timing

The gate's job is to answer within a 100 ms budget and to fail open past it, so some tests
have to assert on time. They are written to be honest about load rather than padded. Read
the comment above each one before you touch it. Several earlier "fixes" widened a deadline,
and that widening hid the bug the test exists to catch.

- **Latency ceilings** (`cmd/governor/latency_test.go`, and every test that goes through its
  `timed` helper: the gate, the status line, onboarding). These subtract the runner's own
  measured lateness from the elapsed time instead of allowing more time. If one starts failing
  intermittently, the fix is not a bigger number; the comment at the top of that file explains
  why. The status line's ceiling also logs, rather than fails, a run where the runner's
  lateness alone exceeded the whole ceiling, because nothing under it can be measured on that
  run. Its content is still checked.
- **The wait queue** (`cmd/governor/wait_test.go`). These assert the order of two events, not
  how long they took.
- **Lazy daemon start** (`TestGateStartsADaemonOnlyWhenNothingIsListening` in
  `cmd/governor/lazystart_test.go`). Its "accepts and never answers" case does not assert
  which of two racing deadlines prints its message, because under load either can win. It
  asserts that no daemon was started, and that the wedged listener received the gate's request,
  which proves the gate reached it. If the gate's budget ran out before it even dialled, the
  run tested nothing and is reported as a skip.
- **Doctor's exec budget** (`TestDoctorCatchesHooksRunningADifferentBuild`). Doctor gives each
  child process 2 seconds, and on a saturated machine this test can take exactly that long.
  `TestTheEffectiveBudgetIsTheMinimumOfLocalAndRemote` is load-sensitive in the same way.
  Both have failed only while a Docker build was running on the same machine at the same
  time, and passed when re-run on their own.
- **Measurements are opt-in, not asserted.** Wall-clock performance figures are measured with
  `GOVERNOR_MEASURE=1 go test ./cmd/governor/ -run TestMeasure -v` (and the same in
  `internal/signals`), recorded in design.md, and never checked on every push. A latency
  ceiling checked on every push mostly measures the CI runner.

If a test is flaky, file it. Do not retry it until it goes green, and do not widen its
budget. The gate's 100 ms budget is a real hot-path figure
([design.md §4.4](docs/design.md#44-performance-budget)), and a test that passes only when
the machine allows more time than that is not testing the gate.

### Your own installed governor gates your `go test`

If you develop this repository in Claude Code on a machine where the governor is installed,
the governor governs your work on it. `go test ./...` is a build-shaped command, so the
installed hook asks the installed daemon for a slot before it runs. On a busy machine it waits
and may be refused with `This command was not run`. That refusal is not a test failure: the
tests never started. Wait and run the same command again, or take the budget out of the way
for a while:

```sh
governor pause for 30m   # stop capping without stopping the daemon
governor resume          # when you are done
```

The tests themselves never touch the installed governor. `TestMain`
(`cmd/governor/main_isolation_test.go`) points the whole package at a private state directory
and socket before any test runs, so a test that forgets to set its own endpoint writes into a
temporary directory, not into `~/.claude/governor`. Keep it that way: any new code path that
resolves the endpoint or state directory itself must honour `GOVERNOR_ENDPOINT` and
`GOVERNOR_STATE_DIR`.

Two more effects of testing on a governed machine:

- The daemon the suite starts competes for CPU with your other sessions. Running the full
  suite next to a Docker build is how the doctor tests above came to fail.
- `make leakcheck` (`tools/leakcheck.sh`) runs the suite and fails if it leaves a process
  behind. Run it after any change to how tests start or stop daemons.

### The non-negotiable tests

- **`TestGateFailsOpen`** (`cmd/governor/gate_test.go`) is the proof that the gate never hangs,
  crashes or blocks a spawn past its deadline. Every failure shape in it must keep passing,
  and a new failure path needs its case there **before** the feature that adds the path.
- **`TestUninstallLeavesTheMachineByteIdentical`** holds uninstall to leaving no file, hook,
  setting or process behind. [design.md §13.3](docs/design.md#133-what-touches-the-users-machine)
  lists every path install touches, and that list is normative: add a path there in the same
  change that adds it to install.
- **The docs are tested.** `cmd/governor/docs_test.go` holds `docs/guide.md` and
  `docs/how-it-works.md` to the code, and `cmd/governor/contributing_test.go` holds this file
  to the repository: every path, script, release job, make target and test it names must
  exist. If one fails after you rename a file, a job or a heading, fix the doc, not the test.

### The clean-install container test

```sh
bash scripts/clean-install-test.sh                 # debian:stable-slim
bash scripts/clean-install-test.sh alpine:3        # musl and busybox
```

This needs **docker**, and `openssl` and `python3` on the host. It builds both linux binaries
from your working tree, serves them over TLS with a throwaway certificate (so `install.sh`
keeps its `--proto '=https'` restriction), and runs the real, unmodified `install.sh` inside a
clean container. The checks themselves are in `scripts/clean-install-in-container.sh`:

1. the container starts with no `~/.claude`;
2. install writes exactly the files design.md §13.3 lists, and creates `settings.json`
   because there was none;
3. `governor doctor` exits 0 or 3, with exactly the `sessions` and `spawn` checks unverified
   (no Claude Code runs in the container, so those two cannot verify);
4. a reinstall over the running install works, which is the rename(2) path;
5. uninstall leaves `HOME` byte-identical to how it started.

CI runs it on every push, on both images (the `clean-install` job in
`.github/workflows/ci.yml`). Run it locally after any change to `install.sh`, `governor install`
or `governor uninstall`: that is where an install bug is cheapest to find. A developer machine
cannot find these bugs, because it already has everything a stranger's machine lacks: a
binary on `PATH`, an existing `settings.json`, a daemon running.

---

## Measuring the hook surface

Everything the governor does depends on facts about Claude Code's hooks: which tool name a
subagent spawn arrives under, what the payload carries, when a hook fires, where its output
goes. Those facts are **measured, never inferred from documentation**. A wrong tool name does
not fail; the matcher just never matches, the gate is never called, and every other check
stays green.

[`docs/hook-surface.md`](docs/hook-surface.md) is the raw record. [design.md §10](docs/design.md#10-hook-surface--confirmed)
is the summary the code relies on, and §10.3 lists what is still unmeasured. A new fact goes
into **both** files, with the Claude Code version it was measured on.

The method, from [hook-surface.md §0](docs/hook-surface.md#0-provenance) and
[§23](docs/hook-surface.md#23-the-marketplace-route-and-the-rename-it-forced--measured-on-21310-and-21312):

- **Record provenance first.** `claude --version`, the host, the date. Claude Code updates
  itself in the middle of a session (it went from 2.1.310 to 2.1.312 during one release), so
  check the version again before you write a result down, and re-check an old fact against
  the version that is running now.
- **Work under an isolated HOME.** Run each probe as a headless `claude -p` with `HOME` set to
  a fresh scratch directory, so nothing reads or writes the real `~/.claude`. For hook probes,
  also pass `--settings <probe>/settings.json --setting-sources ""`, so no other hook on the
  machine can contaminate the result. The probe hook should write its stdin to a file
  unmodified and exit 0.
- **SessionStart fires without a login.** Under an isolated HOME, `claude -p` prints
  `Not logged in · Please run /login` and exits 1, but a plugin's SessionStart hook has
  already run by then. Anything observable from SessionStart (`$CLAUDE_PLUGIN_ROOT`,
  `$CLAUDE_PLUGIN_DATA`, which plugin copy loaded) can therefore be measured without touching
  the real account.
- **Use `--plugin-dir` for a logged-in run.** When a fact needs a real model turn (the tool
  names a session actually gets, for example), load the plugin for that one headless run with
  `claude -p … --plugin-dir <tree> --output-format stream-json --verbose`. It registers
  nothing and leaves nothing behind. It does cost money, so do one run and read the `init`
  event carefully.
- **Only a person in a terminal can accept a command source.** The marketplace entry is a
  `command` source, and inside a Claude Code session both `-y` and `--accept-command` are
  ignored. Any measurement of the marketplace install path is done by a person, in their own
  terminal, under a throwaway HOME.
- **Keep `HOME` short.** The daemon's socket lives under it, and macOS limits a socket path to
  104 bytes. A `mktemp -d` under `$TMPDIR` can exceed that; `/tmp/h.XXXXXX` does not.
- **Include a control.** A matcher result means nothing unless you also show it *not* firing
  for a tool it should not match.

---

## Pull requests and CI

Every change ships through a pull request against `main`, and merges only when every check
has run and passed. Evaluate the checks yourself:

- **Never `gh pr merge --auto`, and never `--admin`.** A merge that bypasses or pre-approves
  CI is not a merge this project accepts.
- **Zero checks having run is not a pass.** Confirm the checks actually ran, with real
  durations, before you merge.
- **A billing rejection is not a test failure.** On this account, a job that GitHub refused
  to run for billing reasons ends in 2 to 4 seconds with **zero steps**. Do not debug it as a
  test failure; it is an account problem.
- **`gh run list --commit` needs the full 40-character SHA.** A short SHA silently matches
  nothing, so a loop waiting for a run to appear never ends. Use `git rev-parse HEAD`.
- **macOS minutes cost about ten times Linux minutes here.** That is why `-race` runs on
  ubuntu only, macOS runs the native cgo test pass once per push, and the clean macOS install
  runs only on a release tag.

The maintainer tracks work in a Notion board (story IDs like `GOV-37` in commit subjects); a
contributor does not need it.

---

## Releasing

A release is a tag. Pushing `vX.Y.Z` runs `.github/workflows/release.yml`, which builds the
four binaries, publishes the GitHub release, and then tests the published release on a clean
macOS machine. The Homebrew tap is then updated by hand. The whole procedure:

### 1. Tag a green `main` with an annotated tag

```sh
git fetch origin
sha=$(git rev-parse origin/main)               # the full SHA; a short one matches nothing below
gh run list --commit "$sha"                    # every ci run on this commit must be completed and green
git tag -a vX.Y.Z -m "governor X.Y.Z" "$sha"
git cat-file -t vX.Y.Z                         # must print "tag"; "commit" means you made a lightweight tag
git push origin vX.Y.Z
```

The tag must be **annotated** (`-a`), so it records who tagged it and when. v0.1.0 and v0.1.1
are annotated; v0.1.2 was pushed as a lightweight tag by mistake. Do not repeat that.

Tag only a commit whose CI is green. The release workflow does not run the test suite; it
trusts that `main` already did.

### 2. Watch `release.yml`, all three jobs

```sh
gh run list --workflow release --limit 1
gh run watch <run-id>
```

- **`build`** builds `governor-<os>-<arch>` for darwin and linux, arm64 and amd64, with the
  version stamped from the tag (`governor version` prints `governor X.Y.Z`). darwin is built
  on `macos-latest` with cgo **on**; linux is built with cgo off, so it is static.
- **`publish`** writes `checksums.txt` (sha256 of every binary), generates `governor.rb`
  from that file with `tools/brewformula`, and attaches all of it to a GitHub release with
  generated notes. `install.sh` downloads from `releases/latest`, so from this moment the
  release is live for anyone running the one-liner.
- **`clean-macos`** runs **after** `publish`, against the release as published. On a fresh
  macOS runner with a fresh `HOME` it checks:
  1. **`install.sh`, unmodified, fetched from the tag**, installs from `releases/latest` and
     passes everything in `scripts/clean-install-in-container.sh`: the exact §13.3 file set,
     `governor doctor` exiting 0 or 3 with exactly `sessions` and `spawn` unverified, a
     reinstall over a running install, **thermal is not `unknown`** (the release's darwin
     binary really was built with cgo), and a byte-identical uninstall;
  2. **the release's own `governor.rb`**, placed in a throwaway local tap, passes
     `brew style` and `brew audit --strict`, installs, passes `brew test`, and the installed
     binary prints `governor X.Y.Z`.

It needs the repository to be public, because it downloads from
`raw.githubusercontent.com` and `releases/latest` exactly as a stranger would.

**Known issue:** on current Homebrew, the formula step in `clean-macos` fails with
`Refusing to load formula local/governor/governor from untrusted tap local/governor`, because
the job does not yet trust the throwaway tap it creates. That happened on v0.1.2. If that is
the **only** failure, the release itself is sound. Do the Homebrew check by hand in step 4
before you announce anything. Any other failure in `clean-macos` means the release is bad:
go to step 5.

### 3. Copy that release's `governor.rb` into the tap

The tap is a separate repository, `peter3605/homebrew-claude-governor`, so that
`brew install peter3605/claude-governor/governor` resolves. Copy the formula **attached to
this release**: it is generated from the release's own checksums, so its version and digests
match the binaries beside it. Never hand-edit it.

```sh
gh repo clone peter3605/homebrew-claude-governor /tmp/tap
cd /tmp/tap
gh release download vX.Y.Z --repo peter3605/claude-governor --pattern governor.rb \
  --output Formula/governor.rb --clobber
git add Formula/governor.rb
git commit -m "governor X.Y.Z"
git push
```

Copy a formula only after `clean-macos` has passed (or failed only on the known issue
above). The tap's history starts at 0.1.1 because v0.1.0's formula failed its own audit and
was never copied.

This step is manual on purpose. Automating it would mean storing a token with write access to
the tap repository as a secret in this one. That is the maintainer's decision, and it has not
been made.

### 4. Verify

```sh
# the tap holds exactly what the release attached
gh release download vX.Y.Z --repo peter3605/claude-governor --pattern governor.rb --output - \
  | cmp - /tmp/tap/Formula/governor.rb

# Homebrew installs that version
brew update
brew install peter3605/claude-governor/governor      # or: brew upgrade governor
governor version                                      # governor X.Y.Z
governor status                                       # on a Mac: thermal is anything but unknown
```

The marketplace needs no step of its own. Its entry is a command source that runs
`governor plugin-path` and loads the tree that the installed binary wrote
([design.md §13.2a](docs/design.md#132a-the-marketplace-route-and-why-the-plugin-is-build-governor)),
so it always serves whatever version is installed.

### 5. When `clean-macos` fails: a new patch tag, never a re-tag

By the time `clean-macos` runs, the release is already published. `releases/latest` points at
it, and someone may already have installed it. **Never move or re-push a tag**, and never
replace a release's assets. A tag that changes meaning under people who already fetched it is
worse than a bad release. Instead:

1. Leave the bad release where it is, and do not copy its formula into the tap.
2. Fix the problem on `main` through a normal pull request.
3. Tag the next patch version from the fixed `main`, and run this procedure again from step 1.

**The worked example is v0.1.0 to v0.1.1.** v0.1.0 published all four binaries, and then
`clean-macos` failed: the generated `governor.rb` carried a redundant `version` line, which
`brew audit --strict` rejects. The fix went through PR #62, which changed `tools/brewformula`
so that it no longer writes that line. v0.1.1 was tagged from the fixed `main` eighteen
minutes after v0.1.0, `clean-macos` passed, and v0.1.1's formula was the first one copied into
the tap. v0.1.0 was left as it was.
