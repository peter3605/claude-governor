# Does telling a session to ask before it builds actually help?

GOV-17 ships a standing instruction: before an expensive build or test run, call
`governor__request_build` and act on the answer. This note is how that instruction is judged.

It is written in two halves — **the method and the reading rule first, the result afterwards**
— because the question is the kind that is easy to answer by feel. `docs/dogfooding.md` is the
same shape for the same reason, and this file inherits its discipline and one of its scars.

**Status: NOT STARTED. The instrument shipped 2026-09-16; the baseline window has not opened.**
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

Read it with:

```sh
S=~/.claude/governor/samples.jsonl

# builds per outcome
jq -r 'select(.kind=="build")|.build.outcome' $S | sort | uniq -c

# TEST-SUITE RUNS — clause 3. Builds that actually ran, whose shape is a test invocation.
jq -r 'select(.kind=="build")
       |select(.build.outcome!="denied")
       |.build.shapes[]' $S | grep -E '(^|\s)(test|check|bench)$' | wc -l

# the same, split by shape, which is what makes a drop legible
jq -r 'select(.kind=="build")|select(.build.outcome!="denied")|.build.shapes[]' $S \
  | sort | uniq -c | sort -rn

# how long parking actually cost
jq -r 'select(.kind=="build")|select(.build.outcome=="parked")|.build.waited_ms' $S \
  | sort -n | tail -5
```

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

## Results

*Empty until both windows have closed. Filled in at the end, not as it goes.*
