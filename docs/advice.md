# Does telling a session to ask before it builds actually help?

GOV-17 ships a standing instruction: before an expensive build or test run, call
`governor__request_build` and act on the answer. This note is how that instruction is judged.

It is written in two halves — **the method and the reading rule first, the result afterwards**
— because the question is the kind that is easy to answer by feel. `docs/dogfooding.md` is the
same shape for the same reason, and this file inherits its discipline and one of its scars.

**Status: INSTALLED 2026-09-16. WINDOW STILL NOT OPEN.** The two day-zero preconditions are now
met — sessions are governed and the build rate is real — but the re-check found two new blockers
and one broken query. See [The rate re-check, 2026-09-20](#the-rate-re-check-2026-09-20).
The Results section is empty on purpose and is filled in at the end, not as it goes.

---

## The claim being tested

Three clauses, from the story:

1. The plugin ships a skill and a `SessionStart` hook carrying a short standing instruction,
   scoped explicitly to expensive builds and explicitly **not** to ordinary shell work.
2. Parks and denials per hour, and peak child cores, are **materially better** after, with the
   budget unchanged.
3. **Sessions do not get worse at their job.** A session told to check before building must not
   respond by not building. Test-suite runs are counted so that a drop is visible.

Clause 3 is the one that matters and it is the one this instrument was built for. The original
story could not go wrong this way: telling a session to ask before fanning out produced, at
worst, fewer agents. Telling a session to ask before *building* can produce a session that
stops running its tests, which is a direct hit to the quality of the work — and which would
show up in clauses 1 and 2 as a **success**.

---

## The instrument

Two files, and they record different things on purpose.

### `~/.claude/governor/daemon.log` — when the budget moved or bound

Unchanged, and described in full in `dogfooding.md`. One line whenever the budget **moves or
binds**: `peak`, `park`, `park-granted`, `park-expired`, `deny`. Nothing on an ordinary grant.

That rule is right for a log about the budget and **wrong for clause 3**, which is a question
about an absence. A session that quietly stops running its tests writes nothing here, because
nothing bound.

### `~/.claude/governor/samples.jsonl` — every gated build, and what became of it

New in GOV-17 (`trace.Build`, record schema **v3**). The gate appends one line per gated build:

```jsonc
{"ts":"…","kind":"build","v":3,"build":{
  "session_id":"…","shapes":["go test"],"class":"medium","weight":1,
  "outcome":"granted","cwd":"/Users/peterho/Repos/claude-governor"}}
```

Every query below was run against real records before this file was committed, not composed
from the schema. A chained line records one shape per invocation, so `make && go test ./...`
contributes to both `make` and `go test`.

| Field | Means |
|---|---|
| `shapes` | the gated invocations, as `bin` or `bin sub` — `go test`, `xcodebuild`, `npm run` |
| `class` | `medium` or `heavy`. Never `light`: a light command is not gated and is never recorded |
| `outcome` | `granted`, `parked`, `denied`, `failed-open` |
| `waited_ms` | how long a `parked` build waited before it was admitted |

**`shapes` carries the invocation and never the arguments.** `sub` is not a subcommand in
general — it is the first non-flag word after the binary, which for `xcodebuild -scheme Foo` is
somebody's product name. A closed vocabulary in `internal/classify` filters it, and
`TestShapesNameTheInvocationAndNeverTheArguments` keeps it closed. Nothing on this record
carries command text.

**The outcome split is the whole point.** `granted`, `parked` and `failed-open` are builds that
**ran**; `denied` is a build that did **not**. Merging `failed-open` into `granted` would hide
the ungoverned ones; merging it into `denied` would count a build that ran as one that was
stopped. Counting test-suite runs means counting the first three.

Read it with `tools/buildstats`, which reports every figure in this document's reading rule —
per-day and total builds by outcome, test-suite runs, rates per awake hour, the shape breakdown,
park waits, and the halves test:

```sh
go run ./tools/buildstats                      # whole log from the first build record
go run ./tools/buildstats --since 2026-09-22   # one window
```

**THE ONE-LINERS THIS SECTION USED TO CARRY ARE RETIRED, AND THE REASON IS A CORRECTION RATHER
THAN A TIDY-UP.** The clause-3 count shipped here as:

```sh
... | grep -E '(^|\s)(test|check|bench)$' | wc -l      # DO NOT USE
```

On the first four days of real records it returned **93** where the window held **144**. It
matched `go test` and silently missed **all 51 `pytest` invocations**, because `pytest` does not
end in a separate `test` word. Clause 3 is the *first* gate in the reading rule below, so that
query would have scored a go-test-only result as the whole answer — and it would have failed in
the flattering direction, reporting test runs as stable while a third of them were invisible.

The categorisation now lives in `internal/classify.ShapeRunKind`, next to the vocabulary it has
to agree with, with `TestShapeRunKind` going from a real command line through `Classify` to a
category. Three readings that looked obvious were wrong when probed and are now pinned by tests:
`npx jest` does **not** unwrap to `jest`, `vitest run` and `coverage run` keep a subcommand, and
`python -m unittest` is not gated at all.

Shapes are reported in three buckets, never merged:

| Bucket | Means | This window |
|---|---|---|
| `test-run` | can only be a test suite: `go test`, `pytest`, `cargo test`, `npm test`, `xcodebuild test`, `vitest run`, `tox` | `go test`, `pytest` |
| `ambiguous` | may be one, and the shape cannot say | `npx` |
| `not-test-run` | gated work that is not a suite: `go build`, `go` (i.e. `go vet`), `make`, `mypy` | `go build`, `go`, `mypy`, `go run` |

Folding `ambiguous` into `test-run` inflates the denominator clause 3 is judged against; folding
it into `not-test-run` hides a real drop. It stays its own column.

### `tools/childcpu` — peak child cores

Unchanged, and the source of clause 2's peak. `docs/phase-0-findings.md` §8 has the invocation.

---

## Known defects of the instrument, stated before the window rather than after

- **A record can be lost.** The write is deferred to the end of the gate, after any refusal has
  reached stdout, so that an unwritable disk cannot silently convert a deny into a grant
  (`TestRecordingABuildNeverSuppressesADeny`). The cost of that ordering is that a gate whose
  budget expires mid-write exits without the line. **A window's build count is therefore a
  floor, not an exact figure.** This biases against finding a drop in test runs, which is the
  safe direction for clause 3 but must be stated before the numbers are read, not after.
- **Only gated commands are recorded.** A test suite the classifier rates light is invisible
  here. The classifier's error rate is measured in `internal/classify/eval_test.go`; this note
  inherits it whole.
- **`shapes` is a list.** `make && go test ./...` records both. Counting shapes counts
  invocations, not commands, and the two differ for chained lines.
- **An ungoverned session records nothing.** GOV-29's finding applies unchanged: hooks are
  bound at session start, so a session open before the install contributes nothing and is not
  distinguishable from a quiet one. `governor doctor` fails while any live session predates the
  hooks, which is what makes this checkable rather than assumed.
- **`python -m unittest` is not gated at all**, so a unittest suite is invisible here — not
  undercounted, absent. `TestUnittestIsInvisibleToClauseThree` pins it, and if it ever starts
  being gated that test fails and this list has to change in the same commit.
- **`npx` cannot be resolved, and it is a classifier quirk rather than a privacy choice.** `npx`
  is registered as a `runner`, which unwraps `<runner> run <cmd>`, but npx takes its command as a
  bare operand with no `run` keyword — the `dispatcher` shape it is *not* registered as. So
  unwrapping never fires and `npx jest` and `npx some-local-bin` record identically as `npx`.
  That is 14 of this window's 190 records. Fixing it would move the classifier's measured gated
  share and error rates, so it belongs to a story of its own, not to this window.
- **Wall-clock time is not the denominator.** The sampler writes one record per second
  unconditionally, so a period's sample count *is* the seconds the machine was awake in it. On a
  laptop that is shut most of the day the two differ by an order of magnitude, and reporting per
  calendar day makes a closed lid look like a session that stopped building. `tools/buildstats`
  reports per awake hour for this reason, and splits only the days that actually carry builds.

---

## The reading rule, fixed in advance

Clause 3 is evaluated **first**. If it fails, the other two are not a success regardless of
what they say.

| Observed | Conclusion | What follows |
|---|---|---|
| test-suite runs per hour drop materially | **The instruction made sessions worse at their job.** | The story has failed. Withdraw or re-word the instruction; a lower peak bought this way is not a saving |
| test-suite runs hold; parks and denials per hour fall; peak falls | **The advice is landing.** | Clause 2 and 3 are met. Record the numbers |
| test-suite runs hold; nothing else moves | The instruction is being ignored, or the budget never bound in either window | Check the peak first. If the budget never bound, this window cannot answer the question and must be discarded rather than reported |
| `failed-open` is a large share of either window | The daemon was down for part of it | The window is measuring the wrong thing. Discard it and say so |
| the two windows differ in build volume by more than ~2x | The workloads are not comparable | Rates do not fix a difference in *kind* of work. Say so rather than normalising it away |

**Materially better is defined before the window, not chosen after**: a change smaller than the
difference between the two baseline halves is noise. Split the before-window in two and compare
those to each other first. If the halves differ as much as the windows do, there is no finding.

---

## The window, and the trap it has already sprung twice

**Check the event rate is real before committing to a week.** This project has now lost one
window to emptiness (GOV-29, discarded) and had one come back too thin to carry a percentile
(GOV-03, n=34 agent lifetimes in 101 hours, and zero on two consecutive days).

Measured on 2026-09-16, before any of this shipped, on the daemon actually running here:

```
2026-09-09 → 2026-09-15, whole daemon.log:
  9 peak   5 park   2 park-granted   3 park-expired   3 deny
```

**Three denials in six days is ~0.02/hour, and no improvement on that is detectable.** Clause 2
cannot be scored against it. Two things have to be true before the baseline window opens:

1. **The machine must be running a build-gating governor.** The daemon that produced the log
   above writes the pre-GOV-15 `in_use=4/4` park format and predates build gating entirely, so
   it was governing *agent slots*. Builds are far more frequent than spawns, which is the whole
   reason clause 2 might be measurable at all — but only once the binary that gates them is
   installed.
2. **The rate must be re-checked after that install and before the clock starts**, from
   `samples.jsonl` rather than from `daemon.log`, because the build count is the denominator
   for everything here. If a few days of ordinary use do not produce enough gated builds to
   make a rate meaningful, **say so and do not start the week.** An empty window reported as a
   null result is worse than no window, because it reads as evidence that advice does not help.

---

## Day zero, 2026-09-16

Recorded now so that the window's provenance is a fact rather than a memory.

| | |
|---|---|
| Installed | `governor install` from `455df4a`, not by hand |
| Claude Code | 2.1.273 |
| Host | macOS 25.4.0, darwin/arm64, 8 cores (4P + 4E), 16.0 GiB |
| Budget | `b_max` 4, unchanged — clause 2 requires it to stay unchanged across both windows |
| Run-queue cut | 5.11 per core, from `governor calibrate` at 21:27:22Z: ambient 0.81 + 4.30 headroom |
| Build records so far | **0** — the instrument shipped today and has recorded nothing |
| `samples.jsonl` | 1,477 `agent` and 537,159 `sample` records from Phase 0, and no `build` records |

**`governor doctor`: 11 ok, 1 unknown, 1 FAIL.** The failure is the point, not a defect:

```
FAIL  sessions  6 of 6 live session(s) are ungoverned: launch-control, budgetr,
                muncheez, nightcrew, claude-governor, concertFinder
                (each started before the hooks were installed)
```

**The window does not open until that check passes.** This is GOV-29's finding, and ignoring it
is precisely how this project lost a window before: hooks are bound at session start, so a
session open when `install` ran never loads the gate and contributes nothing — while every
instrument reports healthy. A window measured across those six would not merely be thin, it
would be *actively misleading*, because "few builds gated" would be indistinguishable from
"sessions are behaving well".

The `unknown` on the `spawn` check is expected and is not a blocker: no subagent has been
spawned yet on 2.1.273 since the install, so nothing has yet re-confirmed the matcher on this
version (design.md §10.4). It resolves itself with ordinary use.

### What has to happen before the clock starts

1. ~~**Restart the six sessions**, then `governor doctor` until `sessions` is ok.~~ **DONE** —
   `ok sessions  6 of 6 live session(s) registered with the daemon`, 2026-09-20.
2. ~~**Let a few days of ordinary use accumulate**, then re-check the rate from
   `samples.jsonl`~~ — **DONE 2026-09-20, and the rate is real.** See the next section.
3. **Reinstall from current `main` first.** Added by the re-check. The installed binary is from
   `455df4a` and is four merged PRs behind, one of which is GOV-18 — max-min fair build slots
   and the queue's back door, i.e. *park and deny behaviour*, which is clause 2's metric.
   **GOV-B13** exists so that the next stale install is caught by `doctor` rather than by a
   window: today nothing reports it, because every dev build calls itself `0.0.0-dev`.
4. **Give clause 2 a metric with a stable baseline**, or accept the peak as the scored quantity.
   The halves test says denials per hour cannot carry the comparison on this machine.
5. **Only then** open the baseline week, and record its start date in this file.

If the rate does not make a meaningful denominator, **say so and do not start the week.** An empty
window reported as a null result is worse than no window at all.

---

## The rate re-check, 2026-09-20

Day zero left two conditions. Both are now met, and the re-check that cleared them found three
reasons not to start the clock anyway. **Figures are pinned to a `--until 2026-09-20` cutoff**,
which excludes the session that ran the check: its own `go test` runs are in the log from
2026-09-21 and would be the instrument measuring itself.

```
day           awake_h  builds     b/h  granted  parked  denied  f-open  testrun   ambig   other
2026-09-17       4.87      88    18.1       73       4      11       0       70       1      29
2026-09-18       0.88       0     0.0        0       0       0       0        0       0       0
2026-09-19       0.80       0     0.0        0       0       0       0        0       0       0
2026-09-20       2.55     102    40.0       97       5       0       0       74      13      37
TOTAL            9.10     190    20.9      170       9      11       0      144      14      66
```

### The two day-zero conditions are met

**Sessions are governed.** `governor doctor` reports `ok sessions — 6 of 6 live session(s)
registered with the daemon`; 13 checks, 12 passing, and the one `unknown` is the expected `spawn`
check. The day-zero FAIL is cleared.

**The build rate is real, and this is the question day zero could not answer.** 190 gated builds
in 9.10 awake hours: **20.9 builds and 1.21 denials per awake hour**, against the 0.021
denials/hour from `daemon.log` that this document rejected as unscoreable. That is ~58x. Builds
are indeed far more frequent than spawns, exactly as the story's traps predicted. `failed-open` is
**0**, so the daemon was up for the whole period and the window is not measuring an absent gate.

**Two of the four days recorded zero builds, and it is not the instrument.** That pattern is the
GOV-03 scar, so it was chased before it was trusted. The sampler (pid 15117) had been alive **12
days 8 hours** and never exited, writing records every hour throughout. The collapse is in
`pmset`: **566 sleep/wake events on 09-18 and 430 on 09-19**. The lid was shut. Four calendar days
bought 9.10 awake hours, which is the finding that changed the denominator for this whole document.

### Three reasons the clock does not start

**1. The halves test fails, and it was pre-registered.** The rule above says a change smaller than
the gap between the two baseline halves is noise. Splitting the active days:

| half | builds/h | denials/h | parks/h | test runs |
|---|---|---|---|---|
| 2026-09-17 | 18.1 | **2.26** | 0.82 | 70 |
| 2026-09-20 | 40.0 | **0.00** | 1.96 | 74 |

Build volume differs by **2.21x**, past this document's own ~2x incomparability threshold. Worse,
**all 11 denials fall in one half and none in the other**. Clause 2's headline metric has no
stable baseline at all, so there is nothing for an after-window to be materially better than.
Test runs, by contrast, are steady at 70 against 74 — clause 3's quantity looks measurable even
though clause 2's does not.

**2. The clause-3 query was wrong, in the flattering direction.** The retired `grep` counted 93
where the window holds 144, missing every `pytest` run. That defect is fixed above; it is recorded
here because it would have produced a confident, published, wrong answer on the clause the traps
field calls the one that matters.

**3. The installed binary is four merged PRs behind `main`, and one of them is GOV-18.** The hook
binary is dated 2026-09-16 17:27, built from `455df4a`. Merged since: GOV-33, GOV-18, GOV-26,
GOV-B01. **GOV-18 is max-min fair build slots and closing the queue's back door — park and deny
behaviour, which is precisely clause 2's metric.** Measure the baseline on this binary, upgrade
for part 2, and the after-window differs by GOV-18 *as well as* by the advice, with no way to
separate them afterwards. Corroborating evidence that the install is genuinely stale rather than
merely older: the statusline temp-file leak GOV-33 fixed is still producing files — 67 orphans,
newest 2026-09-20 19:33.

### What this says about clause 2

Clause 2 asks for parks and denials per hour *and* peak child cores, and requires the after figures
to be materially better **on at least the peak**. The halves test says the first half of that is
not scoreable on this machine: denials are a bursty count that swings from 2.26/h to 0.00/h
between two adjacent active days, and no amount of waiting makes a count of eleven events stable.
The peak is a distribution statistic over every sampled second and is the quantity with a real
chance of carrying a comparison. The recommendation, recorded here before the window rather than
after it, is that **parks and denials are reported as descriptive and the peak is what is scored**.

---

## Results

*Empty until both windows have closed. Filled in at the end, not as it goes.*
