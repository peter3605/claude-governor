# Which `Bash` commands are build-shaped

**A predicate over command text catches every expensive command that names a build tool, and
none of the ones that do not — which is 47% of them. That is the ceiling, and it is a property
of the problem rather than of the table.**

GOV-03 established that agent count is the wrong control variable and that the cost lives in
builds and test runs arriving through `Bash` (docs/phase-0-findings.md §7). Pointing the gate
at `Bash` is a matcher change. Deciding **which** `Bash` is the whole problem, and this
document is the evidence for the answer.

The rule the work was held to: classify from observed commands and their measured cost, never
from a hand-written list of binary names that looks plausible. A list may well be the right
*answer*; it is not an acceptable *method*. The answer here is close to a list — and the point
of measuring was to find out how far that list actually gets you, which turns out to be about
half way.

| | |
|---|---|
| Corpus | **20,105** `Bash` tool calls, 477 transcripts (20,107 collected, less 2 — see §2) |
| Cost-labelled | **1,587** ("fittable": inside sampler coverage, ≥2 attributable seconds) |
| Expensive | **115** of 1,587 — 7.2% |
| Catch rate | **53.0%** of expensive commands (35.2% held out) |
| False positives | **35.7%** of cheap commands (28.8% held out) |
| Gated share | **18.4%** of all `Bash` calls |
| Cost captured | **78.6%** of measured child core-seconds |

Everything below is reproduced by:

```sh
go build -o /tmp/bashcorpus ./tools/bashcorpus && /tmp/bashcorpus -out /tmp/corpus.jsonl
GOVERNOR_CORPUS=/tmp/corpus.jsonl go test ./internal/classify/ -run FullCorpus -v
```

---

## 1. The corpus, and how it was collected

`tools/bashcorpus` walks every transcript under `~/.claude/projects`, pairs each
`tool_use(Bash)` with its `tool_result` by id, and writes one record per call: the command
text, the window it ran in, the session, and the cost measured during it.

The pairing is what makes a cost label possible at all — the `tool_use` timestamp is when the
command was issued and the `tool_result` timestamp is when its output came back, so the two
bracket the seconds the command was on the CPU. An unpaired `tool_use` is dropped rather than
given an invented end.

**The cost label is child cores**, `claude_cpu` minus `claude_cpu_roots` from
`~/.claude/governor/samples.jsonl`. This is the same quantity phase-0-findings.md §5 turns on:
a build or test run is spawned as a *child* of a session and lands there, while a subagent
executes *inside* the session process and lands in roots.

| | |
|---|---|
| Sampler window | 2026-09-08T17:26Z → 2026-09-13T21:00Z, 306,886 samples at 1 Hz |
| Calls in that window with a usable label | 1,587 |
| Sidechain (subagent) calls in the corpus | 1,400, of which **41** are labelled and fittable |
| Backgrounded calls | 342 — never labelled, see §2 |

**The corpus is overwhelmingly main-session traffic.** Only 41 of the 1,587 cost-labelled calls
— 2.6% — were made by a subagent, which is consistent with phase-0-findings.md §1: agents were
live for 1.2% of the window and produced zero spawns on each of the last two days. **Nothing
here characterises what an agent's shell traffic costs**, and a machine whose agents shell out
heavily might distribute cost differently.

### The count, and the count this story was filed with

GOV-14 was filed citing **18,929** `Bash` calls counted on 2026-09-12. This collection finds
**20,105**, five days later and by a different route (recursive walk, tool_use blocks paired to
results). The two are the same measurement at different times and the difference is not
interesting; the figure that matters — that `Bash` outnumbers gate-matched calls by roughly
220× — is unchanged.

## 2. Attributing cost to one command, which is the hard part

The sampler is **host-wide**, and this machine ran five or six concurrent sessions for 98.9% of
the window. Child cores in the seconds a command ran are not necessarily *that* command's.

**The obvious rule was tried first and is biased in exactly the wrong direction.** Keeping only
commands that overlapped no other command sounds clean. Measured, the share of calls that ran
alone falls from **54% at 1–5 seconds to 26% at 20–60 seconds** — a long command on a
five-session machine is almost certain to overlap something, so the rule discards the expensive
tail, which is the only part of the distribution this story is about. The clean subset's median
peak was 0.009 cores against 0.298 for the rest: it selects quiet windows by construction.

**So exclusivity is enforced per second, not per command.** A 1 Hz sample is *attributable* to
a call when that call is the only `Bash` call in flight anywhere on the machine during that
second. A 200-second test run that overlapped a 3-second `grep` loses three seconds of evidence
rather than all of it.

The bias reverses, in the safe direction:

| Duration | Calls | Fittable | Median attributable fraction |
|---|---:|---:|---:|
| 1–5 s | 2,829 | 37.5% | 1.04 |
| 5–20 s | 521 | 51.6% | 0.98 |
| 20–60 s | 266 | 54.9% | 0.97 |
| 60 s+ | 186 | 60.2% | 0.87 |

Longer commands are now *better* represented, and for those kept the attributable seconds cover
essentially the whole window.

**Sub-second commands get no label at all** (0 of 199 reach two attributable seconds). They are
cheap by construction — a command that exits in 300 ms cannot be a build — but they are absent
from the labelled set and none of the rates below are computed over them.

### What is still unattributed, stated rather than assumed

- **Backgrounded commands.** `run_in_background` returns its `tool_result` immediately and keeps
  running, so the transcript records when it started and never when it stopped. Nothing in this
  corpus polls `BashOutput`, so there is no second source to recover the end from. These 342
  calls are treated as in flight for a conservative 10 minutes: they are never labelled, and
  they suppress attribution for anything they overlap. The horizon is a guess and
  `-bg-horizon` exists to vary it.
- **Non-`Bash` child CPU.** A language server, a dev server started earlier, or a stdio MCP
  server is a descendant of a session and lands in child cores while no `Bash` call is running.
  The baseline subtraction (mean child cores over *idle* seconds in the 15 s before the call)
  removes the steady part of this and not the bursty part.
- **HTTP and native-host MCP servers are invisible**, as phase-0-findings.md §5 records. They
  burn real CPU that never appears in this measurement at all.

### This project's own stress probes are excluded, and it matters

The two most expensive commands in the entire corpus are
`for i in $(seq 1 16); do yes > /dev/null & done` — GOV-13's thermal load generators. Together
they carry **59% of all measured core-seconds**. They are instrumentation, not workload: nobody
else's machine runs them, and fitting a predicate to them teaches it `yes` and `seq`. They are
excluded from every figure in this document, and the exclusion is one line in
`eval_test.go` rather than a quiet filter.

## 3. What "expensive" means

**Peak child cores during the command, ≥ 1.0.**

Mean was tried first and is the wrong variable. `go test ./...` measured **0.06 cores mean over
44 seconds** — which is not a measurement error: phase-0-findings.md's own Phase B decomposition
puts a cold build plus the full suite at 18.46 core-seconds over 150 s, about 0.12 cores mean.
Compilation is bursty and test binaries then sit I/O-bound, so the mean dilutes a three-core
five-second compile across a long window. The control loop in GOV-15 is protecting against
*saturation*, so the peak is the quantity that matters.

At a threshold of 1.0 core, 115 of 1,587 labelled calls are expensive — **7.2%**, which lands
where phase-0-findings.md's independent measurement said it would: child CPU is non-zero on
9.1% of ticks. A small number of invocations carry nearly all the cost.

The separation at the median is stark:

| | Median peak | p90 peak | Ever ≥ 1 core |
|---|---:|---:|---:|
| Commands of only `cat`/`grep`/`ls`/`git`/`sed`/… | **0.00** | 0.07 | 0.6% |
| `go test ./...` | 0.63 | 1.56 | 32.3% |
| `xcodebuild` / `xcodegen` | 1.57 | 5.25 | 75.0% |

## 4. The predicate, and both of its error rates

`internal/classify` parses a command into the binaries it actually invokes — a quote-aware
scan that strips heredoc bodies, descends into `$(…)`, skips env-assignment and `sudo`/`time`
prefixes, unwraps runners (`uv run pytest` is pytest), and reads `python -m pytest` as pytest —
then gates if any of them is a known build driver, excluding subcommands that only ask a tool
about itself (`go version`, `npm ls`, `docker ps`).

Measured over the corpus:

| | |
|---|---|
| **Catch rate** | **53.0%** — 61 of 115 expensive commands |
| **False positives** | **35.7%** — 526 of 1,472 cheap commands |
| Precision | 10.4% |
| **Gated share of all `Bash` calls** | **18.4%** — 3,706 of 20,105 |
| Cost captured | 78.6% of measured child core-seconds |

Both directions are reported because a predicate with only one of them is trivially gamed: gate
everything and the catch rate is perfect, gate nothing and the false-positive rate is.

Two of those parsing rules earn their place by measurement rather than by looking tidy.
**Unwrapping runners** — treating `uv run pytest` as pytest rather than as uv — is worth 4.3
points of gated share (22.7% → 18.4%) and 5.4 points of false-positive rate (41.1% → 35.7%) at
*exactly* the same catch rate, 53.0% either way: gating the wrapper instead of the wrapped is
pure loss. **Stripping heredoc bodies** is not a refinement at all but a correctness fix: before
it, the Python inside `python3 - <<'PY' … PY` was scanned as shell, and the first cost ranking
this analysis produced had a "binary" called `1` above `go`.

### Held out

Split chronologically at 70%, because a *random* split puts commands from the same session on
both sides and sessions repeat commands nearly verbatim — a random hold-out scores its own
training data. The shipped predicate is a table rather than a fitted model, so nothing was
estimated from the earlier half, but the table *was* informed by this corpus (dropping `ruff`,
unwrapping runners) and a figure quoted from the data that shaped it is not evidence.

| | n | Catch rate | False positives |
|---|---:|---:|---:|
| Earlier 70% | 1,110 | 68.9% (42/61) | 38.5% |
| **Held out, last 30%** | 477 | **35.2% (19/54)** | **28.8%** |

**The catch rate halves out of sample, and the reason is the ceiling rather than overfitting.**
The later period is a different mix of projects, and a larger share of its expensive commands
are ad-hoc scripts naming no build tool — the row below that no table reaches. The
false-positive rate *falls* across the same boundary, which is what rules out the usual
explanation: an overfitted predicate degrades in both directions at once, and this one does not.

### The ceiling

| | Count | Caught |
|---|---:|---:|
| Expensive commands that **name a build tool** | 61 | **61 (100%)** |
| Expensive commands that **name none** | 54 | **0 (0%)** |

**Every expensive command containing a recognisable build tool is caught. Not one that does not
contain one is.** The 53% catch rate is entirely the second row, and no edit to the table can
move it. Those 54 commands are ad-hoc `python3 - <<'PY'` scripts doing real work, a
`grep -a -o -E` over a 100 MB binary running 121 s at 1.01 cores, an Xcode `DerivedData` sweep.
Catching them from command text would mean gating `grep` and `python3`, and gating `grep` is
what clause 3 of this story forbids.

### Precision is capped by the base rate, and the false-positive rate is high

Only 7.2% of labelled calls are expensive, so even a perfect build-tool detector cannot have
high precision. The 35.7% false-positive rate is **real build tools invoked cheaply**: `go` is
gated 303 times in the labelled set and only 13.5% of those were expensive; `pytest` 189 times
at 2.6%.

**18.4% of all `Bash` calls taking a lease is not small in the ordinary sense of the word, and
clause 3 asked for small.** Put in traffic rather than percentages: `Bash` fires 184–2,221 times
a day on this machine, so the predicate leases 34–409 times a day against the 0–37 the gate
fires today. It converts a 220× problem into roughly an 11× one. That is a real reduction and it
is not the same as cheap; GOV-15's re-derivation of the latency budget (its clause 4) has to be
done against the 11× figure, not against today's.

**But the composition says the number is a fact about the workload, not a defect in the
predicate.** Every gated call, counted by the tool it names:

| | Calls | Share of all `Bash` calls |
|---|---:|---:|
| `go` | 1,186 | 5.9% |
| `pytest` | 1,185 | 5.9% |
| `xcodebuild` | 625 | 3.1% |
| `xcodegen` | 175 | 0.9% |
| `mypy` | 171 | 0.9% |
| `make` | 145 | 0.7% |
| `npm` / `npx` | 181 | 0.9% |

There is no `grep` or `cat` inflating it and nothing left to trim without deleting a real build
tool. Roughly one in five shell commands on this machine genuinely invokes build tooling —
which is what a machine whose job is building software looks like. The failure clause 3 names
by example (`cat`, `grep`, `git status`, `ls`) is at **zero**, asserted over 900 real commands
from the corpus.

**Three rounds of trimming got it there, each one measured.** Dropping `ruff`, unwrapping
runners, and making dispatchers subcommand-aware — `docker ps`, `terraform output` and
`xcrun simctl list` are control-plane, not compute — took the gated share from 24.4% to 18.4%
**with the catch rate unchanged at 53.0% throughout**. Every point removed was a lease that
caught nothing.

### The same tool is not the same cost in two repositories

This is the finding that most constrains what a static table can ever do.

- `uv run pytest`, one project: **148 calls, 1 expensive (0.7%)**, median peak 0.52 cores.
- `python -m pytest`, another project: among the most expensive commands on the machine —
  307 s at 1.37 cores, 186 s at 2.59 cores.

Same tool, opposite cost, one machine. **The binary name is a prior, not an answer.** A table
tuned until this laptop's pytest stopped gating would be tuned to this laptop, which the
project's own rules forbid; a table that gates it accepts the false positives above. Neither is
resolvable statically, which is the argument for §6.

## 5. Cost classes

GOV-15's loop needs to know that `go test ./...` and `xcodebuild` are not the same size. Classes
are the measured **p90 peak child cores** by command shape — p90 rather than median because the
loop is protecting against saturation, and sizing a build by its median under-provisions for
exactly the invocations that cause the problem.

Measured peak child cores, by the gated binaries a command names (shapes with n ≥ 5):

| Shape | n | median | **p90** | max | expensive |
|---|---:|---:|---:|---:|---:|
| `xcodebuild` | 9 | 1.57 | **4.55** | 4.55 | 100.0% |
| `go`+`make` | 6 | 0.94 | **3.59** | 3.59 | 33.3% |
| `make` | 9 | 0.34 | **3.53** | 3.53 | 11.1% |
| `go` | 303 | 0.48 | **1.18** | 7.41 | 13.5% |
| `mypy`+`pytest` | 8 | 0.64 | **0.91** | 0.91 | 0.0% |
| `pytest` | 189 | 0.60 | **0.89** | 2.79 | 2.6% |
| `uv` | 8 | 0.00 | **0.68** | 0.68 | 0.0% |
| `npx` | 16 | 0.34 | **0.67** | 0.74 | 0.0% |
| `mypy` | 22 | 0.24 | **0.67** | 0.77 | 0.0% |
| *(no toolchain)* | 1,000 | 0.01 | **0.43** | 7.26 | 5.4% |

Which gives three classes, and the boundaries fall where the data separates rather than where
round numbers are:

| Class | Shapes | p90 peak |
|---|---|---:|
| **Heavy** | `xcodebuild`, `make` | 3.5–4.6 cores |
| **Medium** | `go`, `pytest` | 0.9–1.2 cores |
| **Light** | everything ungated | 0.43 cores |

**These classes predict cost, not time.** GOV-18 asked whether they could order a wait queue by
how long a slot would be held, and measured that they cannot do it well: Heavy's p90 *hold time*
is 4.1x Medium's, but the spread within each class is 33–54x and 18.5% of Medium commands outlast
the median Heavy. The full table is in design.md §5.6a, and `TestClassDurations` in
`internal/classify` reproduces it from a corpus. A class is a size, and a size is not a duration.

**`ruff` and `black` were removed from the gated set on this evidence.** Where `ruff` ran as
the only gated tool in a command — 20 calls, the only ones whose cost is attributable to it
alone — its median peak was 0.02 cores, p90 0.05, and none was expensive. It is a linter
written to be instant, and gating it bought false positives and no captured cost: removing it
took 1.1 points off the gated share with the catch rate unchanged at 53.0%. `black` goes
with it by analogy and was not separately measured here. `mypy` measured slower (p90 0.67,
n=22) and stays gated.

`Class.ExpectedPeakCores()` reports 0.1 / 1.2 / 4.5 for Light / Medium / Heavy.

**These are a floor, not a calibration.** The window is weighted toward Go, whose builds are
fast, and away from the iOS UI suite and the large pytest runs that would be heavier — the same
bias phase-0-findings.md §1 records, pushing in the same conservative direction. Ecosystems with
no measured shape here (Rust, JVM, .NET, containers) are classed by analogy to the four that
were measured, and that is marked in the table in `classify.go` rather than presented as
evidence.

## 6. What this means for GOV-15

1. **Gate on the predicate, not on `Bash`.** 18.4% of calls, not 100%.
2. **Re-derive the latency budget against 12× today's gate traffic** — clause 4 of GOV-15, and
   the clause its traps flag as most likely to be skipped. Classification is gate-side and
   costs ~5 µs per call (`TestClassifyIsCheapEnoughToRunOnEveryCall`), so a cheap command never
   touches the socket. That property is load-bearing and there is a test on it.
3. **Treat the class as a prior to be corrected, not as a constant.** The governor already has
   the instrument that settles it: the sampler measures child cores, and `tools/bashcorpus`
   demonstrates the join from a command window to its cost. A per-host table keyed on command
   shape, seeded from §5 and updated from what the machine actually does, is the design this
   evidence points at — it is the only thing that can learn that this laptop's `uv run pytest`
   is cheap without being told.
4. **Do not expect the missing 47%.** Half the expensive commands name no build tool. If GOV-15
   needs them, the mechanism is not a better predicate — it is reacting to *observed* load
   after a command starts, which is a different design and a different story.

## 7. Limits

- **n=115 expensive commands.** Several shapes in §5 rest on fewer than ten calls; `xcodebuild`
  is nine. They are reported with their n rather than smoothed.
- **One machine, one user, five days.** Every rate here is this host's workload mix.
- **Sub-second commands are unlabelled**, so the rates describe commands that ran at least two
  attributable seconds.
- **The threshold is a choice.** At ≥0.5 cores, 11.6% of calls are expensive; at ≥2.0, 2.3%.
  1.0 was chosen because it is one full core of sustained demand and because it reproduces the
  ~9% shape found independently in Phase 0.
- **The held-out split is weak evidence.** Fitted feature selection scored 61% recall in
  training and 15% on a chronological hold-out, because the two halves are different projects
  with different command vocabularies. That collapse is *why* the shipped predicate is the
  toolchain table rather than a fitted feature set: the table's 100%-of-recognisable figure does
  not depend on which project runs next, and a fitted one demonstrably does.
