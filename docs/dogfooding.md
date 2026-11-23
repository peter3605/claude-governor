# Did a fixed budget of four actually help?

GOV-11's last clause: *"A note in docs records whether the fixed budget actually helped."*
This is that note. It is written in two halves — the method and the reading rule first, the
result afterwards — because the question is the kind that is easy to answer by feel, and a
week of impressions is exactly the evidence this repo does not accept anywhere else.

**Status: week RESTARTED 2026-09-09 after a dead window. Originally started 2026-09-09.**
The Results section below is empty on purpose and is filled in at the end, not as it goes.

Installed 2026-09-09 on macOS arm64 (Darwin 25.4.0), Claude Code 2.1.265, through
`governor install` and not by hand. `governor doctor` reported 8/8 immediately afterwards, a
second `install` changed nothing and did not restart the daemon, and the day-zero checks
below were run before the week's ordinary use began.

### The dead window, 2026-09-09 18:57 to 2026-09-10 — recorded, not quietly skipped

**The first hours of this week measured almost nothing, and the dates are not being moved to
hide it.** GOV-29 found that hooks are bound at session start, so every Claude Code session
that was already open when `governor install` ran never loaded the gate and was ungoverned for
its whole life. At 19:40 on the 9th this machine had six live sessions and **four of them were
in that state**, having started the previous afternoon — twenty-nine hours before the hooks
file was written at 18:57:50.

Why it matters more than a lost afternoon: the log from that window is not merely thin, it is
**actively misleading**. Four of six sessions could not contribute a peak, a park or a denial
no matter what they did, so the reading it invites — "no denials all week, four was generous"
— is the exact wrong conclusion, reached from real data, with every instrument reporting
healthy. `doctor` said 8 of 8 throughout.

What was actually being measured in that window was roughly one third of the machine's
sessions, and there is no way to recover which spawns the other four made. The window is
therefore **discarded rather than adjusted**: no partial credit, and no attempt to scale the
numbers up to what six sessions would have produced.

The week restarts from a point where every live session postdates the install. What makes that
verifiable now rather than assumed is the `sessions` check GOV-29 added — `governor doctor`
FAILS while any live session predates the hooks, and naming the sessions by directory is what
turns "restart your sessions" into something a person can act on.

**Restarted and verified 2026-09-09.** All five live sessions now postdate the hooks file
(written 18:57:50), and `governor doctor` reports `sessions ok` for all five:

| PID | Started | Claude Code | Repo |
|---|---|---|---|
| 5364 | 19:54:49 | 2.1.267 | claude-governor |
| 75024 | 20:33:31 | 2.1.267 | concertFinder |
| 75484 | 20:33:42 | 2.1.267 | budgetr |
| 4006 | 21:09:31 | 2.1.267 | muncheez |
| 59012 | 22:08:26 | 2.1.267 | nightcrew |

The two that had been ungoverned since the 8th — nightcrew and muncheez — are the two doctor
named, and restarting them is what cleared the check. Every session on the machine now runs
2.1.267; the 2.1.263 sessions that were live during the dead window are gone, which is a second
piece of evidence they were the stale ones.

**The clock starts from 22:08 on 2026-09-09**, the newest session, since that is the first
instant at which the whole machine was governed. Anything in `daemon.log` before it belongs to
the dead window and is not evidence.

---

## What is being dogfooded

`governor install` puts the plugin in `~/.claude/skills/build-governor/` and registers the
gate on every subagent spawn on this machine. The policy under test is the one Phase 1
actually ships:

| | |
|---|---|
| Budget | **4** concurrent agents, machine-wide, across every session |
| Where 4 came from | `defaultBudget` in `daemon.go` — an **arbitrary constant** from GOV-06 |
| Control loop | none. GOV-14's AIMD and `governor calibrate` are not built |
| Signals | none. The sampler is GOV-12; the daemon sends `cpu: null` |
| Full-budget behaviour | park for up to 20 s, then deny with an escalating reason |

So this is a static semaphore and nothing more. That is the point: if a fixed number already
removes the pain, Phase 2 is refinement rather than the product, and it can wait.

## What is NOT being measured, and why it would be wrong to

**Thermals, CPU, and "does the laptop feel better".** There is no sampler yet, so any claim
about load would be an impression attached to a number that was never read. GOV-12 lands the
sampler; until it does, the honest scope is *what the budget did*, not *what the machine did*.

**Token or wall-clock savings.** Nothing in this repo measures either, and a before/after
comparison across a week of different work is not a measurement.

## The instrument

The daemon writes one line to `~/.claude/governor/daemon.log` whenever the budget **moves or
binds** (design.md §4.5). Nothing is written on an ordinary grant, so the log is small.

| Line | Means |
|---|---|
| `peak in_use=N/4` | a new high-water mark for concurrent leases |
| `park session=… in_use=4/4 wait=20s` | the budget bound and the spawn was held |
| `park-granted session=… waited=1.2s` | …and a slot freed. The budget bound and it cost a stall |
| `park-expired session=… waited=20s` | …and none did |
| `deny session=… reason=capacity in_use=4/4 denials=K` | the budget bound and it cost a spawn |
| `deny session=… reason=depth` | a subagent tried to fan out. Not a capacity finding |
| `cut 4->2 cause=run-queue rq=7.10 cut=6.59 ceiling=4` | the loop moved the budget, and what moved it (GOV-B29). Also `cause=thermal`, `climb … cause=climb`, `restore … cause=idle`, `ceiling … cause=ceiling` |
| `release session=… kind=build class=heavy weight=2.00 held=1m23s by=PostToolUse` | a lease came back, and what returned it: `PostToolUse`, `PostToolUseFailure`, `SubagentStop`, `SessionEnd`, `reaper:<why>`, `handback` (GOV-B29) |

Since GOV-B29 every line in the file, not only these, starts with an RFC 3339 timestamp, and
the file rotates to `daemon.log.1` at 4 MiB. A budget line is written when the number
changes, never on a tick.

Read it with:

```sh
grep ' budget ' ~/.claude/governor/daemon.log | awk '{print $3}' | sort | uniq -c
grep 'peak in_use' ~/.claude/governor/daemon.log | tail -1     # did it ever reach 4?
grep 'deny .*capacity' ~/.claude/governor/daemon.log | wc -l   # what it cost
```

The high-water mark is recorded on the way **up** rather than inferred from an absence of
denials, and that is load-bearing: without it, "no denials all week" is ambiguous between
*the budget was well chosen* and *nothing ever came close to it*, which are opposite findings
with opposite consequences for GOV-14.

## The reading rule, fixed in advance

| Observed | Conclusion | What follows |
|---|---|---|
| peak never reaches 4 | The budget never bound. The semaphore did nothing this week. | Say so plainly. The pain, if any, was not concurrency. GOV-14 has nothing to tune against yet |
| peak reaches 4; parks resolve; few or no capacity denies | **A fixed budget of four worked.** | Phase 2 is refinement. Let GOV-14 wait |
| peak reaches 4; capacity denies are common | 4 is too low for this machine, or the fan-outs are too wide | The number needs measuring, not guessing — `calibrate` and GOV-14 become the next work |
| repeated denies to one session for one fan-out | The deny loop GOV-16 exists for | GOV-16 becomes urgent regardless of the rest |
| any session failure attributable to the governor | The story is not done | Fix it before anything else. Invariant 1 is not negotiable |

## Known limits of this week's evidence

- **One machine, one user, one kind of work.** This is dogfooding, not a study.
- **A `SendMessage` resume addressed by NAME is ungoverned by design** and fails open
  (hook-surface.md §8.3). If fan-outs behave unaccountably, check for name-addressed resumes
  before suspecting the budget.
- **The lease attribution is not airtight** (design.md §4.3): a resume landing while another
  spawn is in flight takes that spawn's tentative lease via FIFO. The slot *count* stays
  correct, which is the invariant that matters here, but a `session=` in the log may name the
  wrong one of two simultaneous callers.
- **Denials are counted in a 60 s sliding window** per session (`denyWindow`), so `denials=K`
  is a burst count, not a session total.

---

## Day zero, 2026-09-09

Not results — the checks that establish the instrument is real before a week is spent
against it. Each was driven through the shipped binary and the actual harness, never in
process.

**Lazy start recovers a dead daemon.** The daemon was killed with `SIGTERM` the way a reboot
or a crash would, and `doctor` went to `FAIL daemon … every spawn on this machine is
ungoverned`. A fresh `claude -p` session then made one Agent call: that gate found no socket,
failed open (the subagent ran ungoverned, which is correct — the start is for the *next*
spawn), wrote `daemon.start` at 18:58:29, and forked a daemon. `doctor` was back to 8/8 with
no human involved. This is the clause GOV-21 left unbuilt, working on the real machine.

**The next fan-out was governed.** A three-agent parallel fan-out from a second session
produced exactly the log this note is meant to be read from:

```
2026-09-09T18:59:09-04:00 budget peak in_use=1/4
2026-09-09T18:59:11-04:00 budget peak in_use=2/4
2026-09-09T18:59:12-04:00 budget peak in_use=3/4
```

Three agents, three slots, no fourth line, no park, no deny — and `in use 0` afterwards, so
every lease came back on `SubagentStop`. The budget did not bind, which at three agents
against four is exactly right and is the first data point rather than a result.

**Two pre-existing conditions on this machine, recorded because they colour the week.** The
`statusLine` in `settings.json` pointed at a binary that no longer existed (a leftover from
before GOV-21 moved the plugin path); `governor install` overwrote it with the correct path
and backed up the original, so this is now fixed. The five Phase 0 `trace` hooks are still
registered in `settings.json` and still appending to a 43 MB `samples.jsonl`. They are left
alone deliberately — removing Phase 0 instrumentation is not this story's call — but they
mean `SubagentStart`, `SubagentStop` and `SessionStart` each run two binaries this week, and
that overhead is not the governor's.

---

## Results

Filled in 2026-09-12 from `daemon.log`, against the reading rule above and without revising
it. The observed window is **2026-09-09 22:08 → 2026-09-12**, about **three days** at five to
six concurrent sessions — not the seven originally planned. Why it was closed early is in the
Verdict.

### What the log contains

`daemon.log` holds **17 budget events**, and **every one of them falls before 2026-09-09
22:08** — inside the dead window recorded at the top of this document. In the observed window
there are **zero parks, zero capacity denials and zero new peaks**.

### The high-water mark is unavailable for this window, and that is an instrument defect

The reading rule leans on the peak because "no denials all week" is otherwise ambiguous
between *the budget was well chosen* and *nothing ever came close to it*. That disambiguation
**could not be made from the peak here**, for a reason worth recording:

`peak` is a **monotonic high-water mark held in daemon memory**, and it logs only on a new
maximum. It reached `in_use=4/4` at 2026-09-09T19:59:04 — inside the dead window — and 4 is
the budget, so no further peak line could ever be written. The daemon then ran continuously
from 2026-09-09 18:58:29 until it was replaced on 2026-09-12, carrying that saturated value
across the boundary between the discarded window and the real one.

So the instrument built to prevent exactly this ambiguity was **disabled by a single early
saturation in a window that was subsequently thrown away**. The verdict below therefore rests
on parks and capacity denials, which are per-event and unaffected, rather than on the peak.
Anything that reads the high-water mark across a discard boundary needs to reset it, and
nothing currently does.

### The mechanism does work end to end — from the discarded window

Not evidence for the verdict, and deliberately kept separate from it. At 20:00–20:01 on
2026-09-09 a single session (`286a8d57`) fanned out hard: the budget reached 4/4, parked five
spawns, granted two of them after 733 ms and 6.25 s, expired three at the full 20 s, and
denied three for capacity. That is the gate, the lease table, the park path, the wait queue
and the deny path all behaving as designed under real contention.

It also lands on the reading rule's fourth row — *repeated denies to one session for one
fan-out* — which would make GOV-16 urgent if it were reproduced in a valid window. It was
not, so it is recorded as a mechanism check rather than promoted to a finding.

### Session failures attributable to the governor

**Nil.** No session failure attributable to the governor was observed in the window.

What was actually checked, rather than merely not noticed: `daemon.log` carries nine error
lines, all `write reply: broken pipe` or `read request: EOF`. Those are the gate hanging up
before the daemon answered — the fail-open path doing its job, with the spawn proceeding
ungoverned rather than stalling. They are not session failures. `governor doctor` reports 10
checks passing after a reinstall from `main`, including `sessions`, which now verifies rather
than reporting that it could not.

### Verdict

**The budget never bound. The semaphore did nothing in the observed window.** That is row one
of the reading rule, reached on its own terms and stated plainly: *the pain, if any, was not
concurrency.*

It is corroborated from a second instrument rather than from this one alone. `docs/phase-0-findings.md`
measures agents live for **1.2% of a 101-hour window**, 34 reconstructable lifetimes, and
**zero spawns on each of the last two days**. A budget of four could not bind against that,
and no additional days of the same work would have changed it.

**The week was closed at three days rather than run to seven, deliberately.** The reading
rule's first row sends the question to GOV-14; GOV-03 then answered the underlying question —
*did a fixed budget of four help?* — by measurement rather than by impression, and answered it
decisively: the budget rations roughly 0.4–0.9 cores of an eight-core machine, capping a
quantity whose p99 is a fifth of a core, while the quantity that reaches 7.8 cores is
ungoverned. Four more days could not have improved on that, and the project has since pivoted
its control variable away from agent count entirely.

The honest summary is that **this note's question has been superseded by a better-instrumented
one**, and the four days were spent on the answer instead of on more of the same evidence.

---

## Budget 1 in the daemon log, 2026-09-23 to 2026-10-31 (GOV-41)

GOV-41 was filed on the reading that a freshly started daemon serves budget 1 until its first
sample lands, and that this window is where Peter's one-slot status line came from. The first
half is true and is fixed. The second half is refuted by the same log it was drawn from, and
this section records both, because the misattribution is the more expensive finding.

### What the log contains

From `~/.claude/governor/daemon.log`, every `park` and `deny` from 2026-09-23 on (the number
after the slash is the budget the request was judged against):

| Date | Events at budget 1 | Notes |
|---|---|---|
| 09-23 | 9 parks, 5 denies | Heavy builds parked at `slots=3.75/1`; the limit then reads **4 five seconds later** (00:41:38 → 00:41:43) |
| 09-24 | 5 parks, 5 denies | two sessions (`6d417582`, `c22fc656`) denied `reason=capacity` at `slots=1.00/1` |
| 10-04, 10-24, 10-31 | 1 park each | |
| 10-27 | 2 parks | |
| 10-29 | 3 parks | a Heavy build at `slots=3.75/1` at 09:13; the limit reads **3** at 09:38 |

### Why none of it is the cold window

**One daemon produced every line.** pid 23289, started 2026-09-20 23:26 from
`~/.claude/skills/claude-governor/bin/governor` (`governor 0.0.0-dev`), still running on
2026-10-31 with 40 days of uptime. The last `listening on` line in the log is line 456, on
09-20. A cold window lasts one priming sample plus one 1-second tick after a start, and there
were no starts after 09-20. The first event in the table above is more than two days into the
daemon's life.

**The limit moves while the daemon is warm.** 1 → 4 in five seconds on 09-23 is the AIMD climb
(one slot per tick after a cut), and 1 → 3 in 25 minutes on 10-29 is the same loop under
sustained load. A controller that had never been given a ceiling cannot climb to one.

**"No cut line" was the binary, not the state.** The status reading that prompted GOV-41
(budget 2, ceiling 4, no cut) came from that same daemon. Its build predates GOV-34 (merged
2026-09-21 00:45), which is the change that added the `cut` field. That binary has no way to
name a cut, so a missing cut line says nothing about whether one happened. Thermal has never
read `serious` on this machine (312,515 samples, GOV-15), so the cuts in the table were
almost certainly the run queue crossing the calibrated 5.11, which a running Heavy build
causes by itself.

**Two things follow, and both were filed rather than fixed here.** v0.1.1 is installed and
Peter's sessions still talk to a 40-day-old pre-release daemon, because nothing on upgrade
replaces a running daemon and both speak protocol 1. And whether the run-queue cuts are
correct or over-eager (GOV-B12's dead band) is open again, and has to be measured on a
current daemon that can name its cuts.

### The defect that was real

`observeThisMachine` built the controller without `Max`, and `sample()` was the only writer
of it, behind the first EWMA reading. Until then `Controller.start()` returned `Floor`. GOV-41
sets `Max` at construction from `signals.CPUCapacity()` capped by the calibrated B_MAX, through
the same `ceiling()` the sampler uses. `TestAColdDaemonServesItsCeilingNotTheFloor` holds it at
the admission path with a sampler that never answers. With the assignment removed, the test
fails with "answered budget 1, want its ceiling 10".

Its practical cost was small, and this is stated so that nobody re-derives the story's
original impact claim. The request that starts a daemon lands on an empty lease table, and
`Table.Acquire` admits anything into an empty table, so the gate that started the daemon
always got its slot. Only a second concurrent request inside the first second paid.

### Does a daemon that has not sampled say so? (clause 3)

Yes, already, and no new field was added. Every signal in a status reply (`cpu`,
`cpu_instant`, `run_queue`, `cpu_capacity`, and the rest) is null until the first tick
publishes, and `TestSignalsAreAbsentUntilATickLands` holds that. What made the cold state
misleading was that it paired those nulls with a budget at the floor and no cut, which reads
like a machine floored for no reason. After the fix, the cold state answers the ceiling with
no cut, which is exactly what `start()` documents a never-stepped controller should answer.
The new test asserts all three together: budget = ceiling, no cut, and `cpu` null. A separate
"has not stepped" flag would duplicate what the null signals already say.

### Zero values filled by the first sample (clause 4)

Every field the daemon or its controller leaves at a zero value until a sample lands:

| Field | Zero value means | Verdict |
|---|---|---|
| `Controller.Max` | Floor (budget 1) | **Wrong. Fixed by GOV-41.** |
| `Controller.b` / `started` | `started=false` makes `Budget()` call `start()` | Safe: the flag guards it |
| `Controller.clean` | 0 clean ticks, so the hold-after-thermal branch fires for three ticks | Safe: it holds `b`, which starts at `Max`, so it delays nothing |
| `Controller.cut` | no cut | Correct: nothing has cut a controller that has not stepped |
| `daemon.latest` | an all-zero `Sample`, which would read as an idle machine | Safe: `latestOK` guards every reader and status reports null |
| `daemon.cpuAvg` | nil until the first tick | Safe: nil-checked, and `EWMA.Value` carries a `primed` flag |

Not filled by a sample, and checked anyway: `calibratedBMax` (0 means uncalibrated, set at
construction from config), `peak` (a count that is correctly 0 at start), and the
`machine*` cache (a zero `machineAt` forces a refresh). `thermal` is read live on every
status call, not from a sample.

One adjacent case is outside this class but has the same shape. `hostSampler.Sample` is
documented to leave a field at zero when that one reading fails. A failed load-average read
therefore reports `RunQueue` 0, which looks like an idle machine and feeds the loop. That
zero comes from a failed read, not from construction, so it is noted here rather than fixed.

### Work-conserving admission once the ceiling is set (clause 5)

**The rule is unchanged and still right.** `fitsLocked` admits anything into an empty table,
ahead of fair share, because otherwise a request heavier than the whole budget could never
run. A Heavy build weighs 4.5 / 1.2 = 3.75 slots, and on this host the ceiling is 4, so after
the fix a Heavy build on a cold daemon fits without needing the escape. The 3.75/1 grants in
the table were the escape doing its job against a budget the loop had cut.

On a host whose real ceiling is below 3.75 (anything with fewer than 4.5 cores after
calibration), a Heavy build runs only alone, or as a session's first slot. Two paths admit
it. On an empty table, the rule above applies. On a non-empty one, `fitsLocked`'s first-slot
branch (invariant 6) admits it if the table is under budget and the session holds nothing.
That branch does not compare the request's weight to the room left, so the table can reach
roughly budget + 3.75 for as long as the build runs. While it runs, `t.weight` is at or above
the limit, so every other request is refused until it finishes. That is the intended trade:
a build too large for the machine serializes the machine rather than never running, and the
overshoot is bounded by one build.

## Three daemons, one config: calibration that never loaded (GOV-44)

On 2026-10-31 one `governor doctor` run printed `calibration ... b_max 4 slots`, read from
`~/.claude/governor/config.json`, and `budget 6 slots, the most this host allows, with nothing
cutting it`, read from the daemon. Both were green. They cannot both be true, and the daemon log
shows which one was wrong and why.

### What the log contains

`~/.claude/governor/daemon.log`, lines 1252–1259. The log has no timestamp on start lines, so the
times below come from the `calibrated_at` each daemon quotes and from the first `budget peak`
line after each start. Same binary, same machine, a few minutes apart:

| # | Started by | Log | Ceiling served |
|---|---|---|---|
| 1 | `governor install`, ~19:06Z | `run-queue threshold 6.08, calibrated for this host (2026-10-31T19:03:33Z)` | 4 |
| 2 | the gate's lazy start, 19:24Z | **no calibration line at all**, then `budget peak in_use=1/6` at 15:24:40-04:00 | **6** |
| 3 | by hand, after `calibrate` at 19:14:30Z | `run-queue threshold 4.72, calibrated for this host (2026-10-31T19:14:30Z)` | 4 |

Daemon 2 is the defect. It read no calibration, so it served the ceiling derived from 8 cores
(8 / 1.2, with no efficiency-core discount) and cut at the compiled-in run-queue threshold of
2.0. The calibrated values were `b_max` 4 and 4.72. So it was wrong in both directions at once:
it admitted too much, and it throttled far too early on run queue. The story text says the
config held 4.72 "throughout". The log disagrees for daemon 1: it loaded a calibration from
19:03:33Z with threshold 6.08, and 4.72 was written at 19:14:30Z. That does not change the
finding, because daemons 1 and 3 each loaded whatever calibration was on disk when they
started, and daemon 2 loaded nothing.

### Why

`config.Path()` returned `$CLAUDE_PLUGIN_DATA/config.json` when that variable was set.
`transport.DefaultEndpoint()` never looked at it. `calibrate` and `install` run from a terminal,
where the variable is unset, so they wrote the state directory's `config.json`. A daemon a gate
starts is a child of a hook, which Claude Code gives the variable. So daemon 2 bound the correct
socket and read `~/.claude/plugins/data/build-governor-skills-dir/config.json`, which did not
exist. A missing config returned an empty `Config` with no error and no log line, which is why
the log has nothing where daemon 2's calibration line should be.

The hook environment is confirmed by what the hooks wrote, not just by the code:
`~/.claude/plugins/data/build-governor-skills-dir/onboarded` is stamped `2026-10-31T19:17:16Z`,
and only the SessionStart hook resolving `${CLAUDE_PLUGIN_DATA}` to that directory writes there.
No `config.json` exists under any `~/.claude/plugins/data/*/` on this machine, so nothing was
ever written to the path daemon 2 read.

### And a fourth, still running when GOV-44 started

The daemon that was running when GOV-44 started had logged `run-queue threshold 4.81,
calibrated for this host (2026-10-31T22:23:56Z)` when it started (log line 1264). But
`config.json` held 5.02, calibrated at 22:33:16Z. That is install
again. It starts the daemon and then calibrates, deliberately, so the probe measures a machine
with the governor running. The daemon read the file only at startup, so every install left it
on the previous calibration. This one is not about the environment. It is the same symptom,
though, and the same doctor check that was missing would have caught it.

### What changed

- `config.Path()` resolves the state directory and nothing else (design.md §13.3a has the
  rule and the audit of every environment-resolved path).
- Each daemon logs the config path it resolved and either what it found or `not found, using
  built-in defaults`. It also reports what it loaded in `status`, re-reads the file when the
  file changes, and doctor's new `loaded` check fails when the daemon and the file disagree. That
  check compares the path, the calibration, and the live ceiling and run-queue threshold.

## A budget below its ceiling that nothing was holding there (GOV-46)

Found on 2026-11-01, twice within one hour, on a current daemon. That daemon can name its cut,
which is the only reason the state was visible. The pre-GOV-34 daemon that served this machine
for six weeks (previous section) had no `cut` field, so on it the same state looked exactly like
a healthy one.

### What was observed

These are the readings recorded when the story was filed. Raw log lines were not attached to it,
so none are quoted here.

| | Event | What the daemon reported afterwards |
|---|---|---|
| (a) | GOV-44's config reload raised the ceiling from 3 to 4 | budget 3; doctor warned "3 of a possible 4 slots, and the daemon reports no signal cutting it" |
| (b) | the repo's own test suite drove run-queue cuts 4 → 2 → 1, then finished | idle machine: in use 0, waiting 0, cpu 0.42, thermal nominal. Budget **1**, ceiling 4. `status` printed `cut run queue (ceiling 4)`, minutes after the run queue had recovered |

### Why the budget stayed down

There were two causes, one for each event, plus one reporting defect.

- **(a) Nothing raised the budget when the ceiling went up.** `Step` lowered the budget when
  the ceiling dropped below it, but nothing moved it up when the ceiling rose. A daemon
  restarted under the new config would have begun at 4, through `start()`. The running one
  stayed at 3.
- **(b) The only way up was gated on demand, and a low budget suppresses demand.** The additive
  increase fired only when `DemandWaiting` was true. A budget of 1 admits fewer concurrent
  builds, so less work queues, so the signal the climb needed never arrives. An idle machine
  has no waiters at all, so the budget stayed at 1 until the next time something actually
  parked.
- **The `cut` field outlived its signal.** It was cleared when the budget reached the ceiling,
  not when the run queue recovered. So `status` blamed a signal that had already been fine for
  minutes.

### The six weeks before this, explained

GOV-41 (previous section) established that the `slots=1.00/1` parks and denies from 2026-09-23
to 10-31 were real run-queue cuts, and stopped there. Mechanism (b) explains why they lasted:
after a cut, the budget could not climb back until enough demand arrived, and the low budget
was itself holding demand down. The log's only escapes show the mechanism in reverse. On 09-23
the limit went 1 → 4 in five seconds, immediately after nine Heavy builds parked at
`slots=3.75/1`. On 10-29 it went 1 → 3 over 25 minutes, after a Heavy build parked at 09:13.
The budget rose only when something parked, which is the one condition the climb accepted.

This answers the question GOV-B23 was filed to measure ("what floors the budget at parks, on a
daemon that can name its cut").

### What changed

- **A raised ceiling is followed straight away.** If the budget was at the old ceiling, it moves
  to the new one on the same tick, without waiting for demand. A budget that a signal had cut
  stays where it is.
- **An idle governor returns to the ceiling.** After `IdleTicksBeforeReset` (60 one-second
  ticks, one loadavg1 window) with nothing held, nothing waiting and the run queue at or below
  the climb level, a cut budget goes back to the ceiling. This is the state a fresh daemon is
  in, and `start()` already handles it by beginning at the ceiling and cutting fast.
- **The one-slot climb is still demand-gated while leases are held.** In that case a budget
  below the ceiling is unused, not stranded, and raising it would give the next arrival room the
  loop never saw the machine cope with.
- **`cut` now names what holds the budget at this moment.** It is cleared when its signal
  recovers. It still names the run queue while the loop is holding between the climb and cut
  levels, because there the run queue really is what keeps the budget where it is.
- **Doctor's budget fix no longer restarts the daemon.** The restart was the workaround for
  case (a). The loop now handles that case, so the line says there is nothing to fix.
- **Every doctor fix line names a binary that works.** That restart line read
  `kill … && governor install`. Since GOV-45, bare `governor` can resolve to an older brew
  release, so running the line would have downgraded the install. Every governor command doctor
  prints now goes through `governorCommand`, which uses the bare name only when it resolves to
  the running binary. `TestEveryDoctorFixLineNamesABinaryThatWorks` reads every string literal
  in doctor.go to keep it that way.

Tests: `TestARaisedCeilingIsReachedWithoutDemand` and
`TestARecoveredCutReturnsToTheCeilingOnAnIdleMachine` replay the two observations. Against the
controller from before this change, both fail, along with the four tests of the new `cut` rule.

## A week on the released build (GOV-38)

Every earlier section of this file ran a build from the working tree. This one runs what a
stranger gets: governor 0.1.6 from the Homebrew tap, installed on the real HOME. The hooks run
`~/.claude/skills/build-governor/bin/governor`, byte-identical to `/opt/homebrew/bin/governor`.
One entry per day. The subjective fields (did the budget feel right, refusals disagreed with,
overrides used) are the owner's report. Everything else comes from `daemon.log` and `doctor`.

**Overrides are in the log.** Every `limit`, `pause`, `resume` and `boost` writes a
`budget control …` line to `daemon.log` (since GOV-B01), so the override row in each entry is
checked against the log, not taken on memory. The *reason* for an override is the owner's
word.

### Day zero, 2026-11-14 (released build from 22:11 only)

The released daemon (PID 74409, `0.1.6 build 78ae6a98f019`) started at **22:11:49**. Until then
the machine ran dev daemons (`0.0.0-dev` builds 7673c843 and 67c63bc8), so nothing logged on
11-14 before 22:11 counts as evidence about the shipped artifact. That covers 35 `reason=depth`
denies between 00:07 and 16:33, a run-queue cut 4→2→1 at 21:50 followed by a park that was
granted in 5.6s and an idle restore to 4 at 21:52, and one `write: broken pipe` at 21:50:54. They
are listed here so it is clear they were looked at and left out of the week, not missed.

- **On the released build:** about two minutes of governed use. One Medium build lease, held
  5.4s (peak `in_use=1/4` at 22:12:22, released by PostToolUse at 22:12:27). No cuts, parks or
  denies. Budget 4 of ceiling 4 throughout.
- **Did the budget feel right:** yes.
- **Refusals disagreed with:** none. There were no refusals on the released build.
- **Overrides:** none (no `budget control` lines in `daemon.log`).
- **Doctor:** clean after the upgrade (18/18, as recorded on GOV-B40).
- **Upgrade (clause 4: recorded, not counted).** The 22:11 install replaced a running dev-build
  daemon with brew's 0.1.6. Session `f0da448c` stayed attached across it: it held leases on the
  dev daemon at 21:33 and 21:40, then on the 0.1.6 daemon at 22:12, with no hang. Install said to
  restart sessions, and seven sessions ended between 22:04 and 22:11. This does not count as
  clause 4 because it was not one published release replacing another over a running daemon.
  Clause 4 stays open for the next `brew upgrade`.
- **Calibration as the week starts.** `governor calibrate` was run three times on 11-14 (02:13Z,
  02:31Z and 03:11Z) and produced run-queue cuts of 4.61, 4.76 and 4.70. The run before those, on
  11-03, gave 6.59. The week runs at **4.70**, measured at an ambient loadavg1 of 3.18 (0.40 per
  core) and a probe of 5.57 effective cores of 8. This is not a surprise from the week. It is the
  starting condition, and the verdict (clause 5) has to say whether 4.70 or 6.59 is the right cut
  for this machine.

### Day 1, 2026-11-15 (Sunday)

- **Governed activity to 12:00:** none. Nothing was written to `daemon.log` between 22:13 on
  11-14 and 12:01. Six live sessions were registered and idle (`status`: budget 4, 0 in use,
  cpu 0.19, thermal nominal).
- **From 12:00 (written up on 11-20 from the log): the busiest day of the week.** 157 build
  leases (149 Medium, 8 Heavy) and 96 agent leases. 38 builds refused on `capacity`. 53 parked:
  22 granted after waiting and **31 that waited and gave up**, which is a refusal by another
  name. 22 run-queue cut episodes at rq 4.90-8.62 per core. Only 4 of them reached 6.59, the
  cut this machine ran at before 11-14's re-calibration. 24 build leases were released by the
  reaper rather than by their build (surprise 3).
- **Did the budget feel right:** yes, as reported at about 12:00. The afternoon and evening,
  which held nearly all of the day's contention, were not asked about on the day.
- **Refusals disagreed with:** none.
- **Overrides:** none (no `budget control` lines in `daemon.log`).
- **Doctor:** 17 of 18 passing, 1 unknown. `spawn: no spawn seen yet on Claude Code 2.1.325`.
  Claude Code updated overnight, and the hook surface was last seen firing on Agent at 2.1.316,
  2.1.319 and 2.1.322. The check is behaving as designed: it reports that nothing has confirmed
  the matcher on the new version yet. It does not report a fault. (Surprise 1.)

### Days 2-6, 2026-11-16 to 11-20 (written up on 11-20 from the log)

**No session asked the owner about these days while they happened.** The subjective fields
(did the budget feel right, refusals disagreed with) are recorded as *not captured*, not
filled in afterwards. A judgement made at the end of the week about a Tuesday is not evidence
about that Tuesday. Everything else comes from `daemon.log`. No day has a `budget control`
line, so no override was used all week. `doctor` was run on 11-15 and 11-20 only, and passed
18 of 18 on 11-20.

| Day | Builds (Heavy) | Agents | Refused | Cut episodes | Budget below 4 for | `broken pipe` |
|---|---:|---:|---:|---:|---|---:|
| Mon 11-16 | 7 (1) | 97 | 2 (`depth`) | 0 | - | 1 |
| Tue 11-17 | 0 | 7 | 0 | 2 | 22:56 → 06:04 next day | 33 |
| Wed 11-18 | 0 | 21 | 0 | 9 | most of the day (see below) | 31 |
| Thu 11-19 | 0 | 1 | 0 | 4 | 02:08 → 10:42, then minutes | 10 |
| Fri 11-20 | 0 | 3 | 0 | 2 | minutes | 4 |

- **Mon 11-16:** ordinary use, mostly subagents. Four build leases were reaper-expired
  (surprise 3). No cuts.
- **Tue 11-17 to Thu 11-19: the budget was held at 1 for long stretches with almost nothing
  asking for it.** A cut at 22:56 on 11-17 (rq 5.76 per core, a load average near 46 on 8
  cores) held the budget at 1 for **7.1 hours**, until an idle restore at 06:04. On 11-18 it
  was below 4 from 06:57 to 08:28, 08:46 to 10:37, 12:08 to 14:49, 15:39 to 16:46, 19:31 to
  23:23 and 23:40 to 01:38, with rq peaking at 9.23 per core. A cut at 02:08 on 11-19 held for
  **8.6 hours**. In each case the controller behaved as designed: an idle restore needs the run
  queue below the climb level of 4.20 per core, and it wasn't. No build was refused because of
  it, since none was asked for. What is unexplained is the load (surprise 4).
- **Fri 11-20:** two short cuts (rq 5.13, 5.77), each recovered within three minutes.
- **Did the budget feel right / refusals disagreed with:** not captured for any of the five
  days.

### Day 7, 2026-11-21 (Saturday; written up on 11-22 from the log)

The owner's answers were given on 11-22, the day after, and are labelled that way. That is
closer than days 2-6 got, but it is still not on the day.

| Day | Builds (Heavy) | Agents | Refused | Cut episodes | Budget below 4 for | `broken pipe` |
|---|---:|---:|---:|---:|---|---:|
| Sat 11-21 | 237 (52) | 43 | 15 (`capacity`) | 6 | 01:51 → 14:03, then minutes | 20 |

- **The second-busiest day of the week, and the most Heavy builds by far.** 52 Heavy leases,
  against at most 8 on any earlier day. Nearly all came from stepstool: one session
  (`1becacfd`) from 16:00 to 22:00, then a second session (`356dc884`) with 19 Heavy builds
  between 22:00 and 23:00. 15 builds parked: 4 were granted after waiting and 10 waited 20 s
  and gave up. All 15 refusals were stepstool, all `capacity`: around 00:07, from 13:53 to
  13:56, at 16:34, and from 22:23 to 22:29.
- **Surprise 4 happened again, for longer, and this time it cost refusals.** A cut at 01:51
  (rq 6.73 per core against the 4.70 cut) held the budget at 1 for **12.2 hours**, until a
  climb at 14:03 (rq 0.80). Nothing asked for a slot until 13:53. Then stepstool was refused
  4 times in three minutes at `slots=1.08/1`, with the budget still pinned from overnight. This
  is the first time the overnight hold refused real work, not just an idle machine. The 1 Hz
  samples that could name the load are already gone. The trace log keeps about 23 hours, and
  its oldest file starts at 21:51 on 11-21.
- **Other cuts were short or explained by the work itself.** Cuts at 14:13 (21 s), 20:51
  (1.6 min) and 21:54 (1.6 min) recovered quickly. Cuts at 22:00 (rq 8.05) and 22:33 (rq 4.94)
  held until 22:29 and 23:49, during the second stepstool session's Heavy burst. That is the
  governor doing its job.
- **Reaper releases: 14.** 7 build leases at about 300 s (2 of them Heavy), which is GOV-B41's
  pattern; the hooks that fix it were installed on 11-22. And **7 agent leases at 30 min**, all
  stepstool, which is new (surprise 7).
- **Broken pipes: 20.** 9 of them fell between 22:00 and 22:18, during the Heavy burst, and
  one came at 01:51:36 alongside the overnight cut. Unlike 11-17 to 11-19, this was a busy day,
  so a gate giving up is a plausible client (surprise 6).
- **Did the budget feel right:** yes (owner, 11-22).
- **Refusals disagreed with:** none. The owner did not notice them, including the 13:53 ones
  (owner, 11-22).
- **Overrides:** none (no `budget control` lines in `daemon.log`).
- **Doctor:** not run on 11-21. On 11-22, around the 0.1.7 upgrade (GOV-B46), it passed every
  check except `sessions`, which named the sessions to restart.

### Surprises and how each is resolved (clause 3)

1. **Doctor `spawn: unknown` after Claude Code 2.1.325 arrived (11-15).** *Understood and
   correct; resolved 11-15 13:04.* One trivial subagent was spawned on 2.1.325 to settle it. The
   gate leased it and `SubagentStop` released it (`kind=agent weight=0.08 held=2.209s
   by=SubagentStop`), and `doctor` then read `spawn: matcher confirmed firing by real spawns
   (Agent on 2.1.325)`, 18 of 18 passing. The unknown only meant no spawn had happened yet on the
   new version, which is what the check is for. Expect it again after each Claude Code update.
2. **"Overrides never reach `daemon.log`" (raised 11-15, withdrawn the same day).**
   *Understood and correct; not a defect.* It came from a search of the code for logging near
   the word "override", which missed that `control()` in cmd/governor/daemon.go logs every
   verb through `d.event("control …")`, written as `budget control …` (GOV-B01). The log has
   zero such lines since it began on 11-01 because no override was used, which matches the
   owner's report. Recorded rather than deleted, so the week's record shows the claim was made,
   checked, and withdrawn.
3. **A build Claude Code moves to the background holds its slot for the full five-minute TTL
   (11-15/16).** *Filed as GOV-B41.* 25 build leases were released by the reaper at about
   301 s, 24 of them from one earbrief session: 15 `pytest` runs that hit the Bash timeout and
   were moved to the background, and 8 `run_in_background` builds. They account for 66% of the
   window's build slot-seconds, and that session took 29 capacity refusals. The hold itself is
   deliberate (`buildFinish`). What's missing is any signal for when a background task ends.
4. **Load average near 40-70 overnight, holding the budget at 1 for 7-8.6 hours (11-17 to
   11-19).** *Explained 11-22: the machine was asleep. Filed as GOV-B48 for what it costs at
   wake.* The run queue stayed above 4.20 per core all night with no
   governed work running. The 1 Hz samples that could name the process are lost (surprise 5).
   On 11-20 at 23:00 the load was normal (0.9 per core), with one `xcodebuild` in the
   uninterruptible state. On macOS, processes in that state count toward the load average
   without using CPU, which is a lead and not a finding. It resolves when it either recurs and
   is caught with `top`, or the week's verdict explains it.
   **Recurred 11-21:** held at 1 from 01:51 to 14:03 (12.2 h), and refused 4 stepstool builds
   at 13:53-13:56 before it lifted (Day 7). The samples were again lost to rotation before anyone
   looked, so catching it needs a deliberate capture the next time it happens.
   **Cause, from `pmset -g log` (11-22).** Every long hold starts at a sleep. On 11-17 the cut
   at 22:56 is the second a `Clamshell Sleep` began (22:56:18). On 11-21 the display went off at
   01:51:35, the cut came at 01:51:36, and the machine slept on low battery at 01:51:40. It stayed
   in hibernate until 13:53:30, and the first refusal came at 13:53:46. The daemon does not tick
   while the machine sleeps, so a "12-hour hold" is 12 hours of nothing running. The 40-70 load
   average is the burst as the machine goes to sleep, carried across the sleep. It was never a
   hidden process, and the `xcodebuild` lead is withdrawn. What it costs is real but narrow: for
   a few minutes after wake, the controller judges a just-woken machine on pre-sleep numbers and
   refuses work, as it did 4 times on 11-21. That is GOV-B48.
5. **A Phase 0 sampler has been writing to a deleted file since 11-01 (found 11-20).** *Filed
   as GOV-B42. Process stopped on the owner's instruction at 23:34 on 11-20, freeing 1.1 GiB.
   The stop is temporary:* the five Phase 0 hooks in `~/.claude/settings.json` still name the
   September binary, and its `trace ensure` restarts the sampler at the next SessionStart. `governor trace sample` (PID 23958, from the pre-Homebrew
   `~/.local/share/claude-governor` install, running since 2026-09-21) holds file descriptor 4
   on a `samples.jsonl` with link count 0. That file is 1.17 GB and growing, and nothing can
   read it. The rotation on 11-01 replaced the file under it. `doctor`'s `strays` check looks
   only for daemons, so it reports nothing. Left running: the process is the owner's to stop.
   **Resolved by GOV-B42 (#103, shipped in 0.1.7).** `doctor` now has a `trace` check that
   catches both the old-build hooks and a sampler writing to a replaced or deleted log. The owner
   chose to re-install trace from the current build, and on 11-22 `doctor` reports `ok trace`:
   the hooks and the sampler run the hooked build and write the live log.
6. **About 80 `write reply: broken pipe` lines over the week, most of them on days with no
   build requests (33 on 11-17, with 0 builds and 7 agents).** *Partly explained; filed as GOV-B49.* A client
   hung up before the daemon replied. That is how a gate that gave up looks, but on days with
   no gate requests the client is something else, perhaps a status reader. On 11-22, 28 of
   the 156 lines since 11-14 were found within 2 s of a sleep or wake transition in `pmset -g log`,
   about 28 times what chance would give. The rest cannot be traced to a client, because the log
   line does not name the op it was replying to. GOV-B49 adds that and classifies them.
7. **Agent leases released by the 30-minute reaper, more often (11-21).** *Filed as GOV-B50.* 7 on
   11-21 and 8 on 11-22, all or mostly stepstool, against 1-2 a day earlier in the week. Each
   one is a subagent whose lease was not released by `SubagentStop` within 30 minutes. That
   means either a subagent that really ran longer than 30 minutes, released while still running
   (the too-early direction), or a `SubagentStop` that never arrived. GOV-B50 matches each one to
   its transcript before deciding which.

### The upgrade, 0.1.6 → 0.1.7 over a running daemon (clause 4), 2026-11-22

The first time one published release replaced another under live sessions (GOV-B46). At
01:11 EST, `brew upgrade governor` installed 0.1.7 while the 0.1.6 daemon (PID 74409) was
serving 6 live sessions, this one among them.

- **Hangs:** none. The 0.1.6 daemon kept answering between the upgrade and the install.
  Upgrading the binary does not touch the running daemon or the hooks, which is the intended
  shape.
- **Between upgrade and install, doctor said so.** It reported two FAILs, each with the fix
  `governor install`. `hooks` said 1 of 8 hooks was not registered (the new `UserPromptSubmit →
  governor settle`). `binary` said the hooks run 0.1.6 and this is 0.1.7. It also warned on
  `trace` (the sampler was from the old build). The `hooks` line pointed at `settings.json`
  rather than the plugin's `hooks.json`, which is GOV-B47, fixed and merged that day.
- **After `$(brew --prefix)/opt/governor/bin/governor install`:** the daemon became PID 26861,
  `0.1.7 build fc38885daf57`, with eight hooks registered, and `binary`, `hooks` and `trace`
  all ok. The one remaining FAIL was `sessions`: 6 of 6 live sessions started before the hooks
  were installed, and the fix line named every project to restart.
- **Was a restart needed, and did anything say so:** yes, and yes. Hooks bind when a session
  starts, so open sessions keep the old hook set until restarted. Both install and doctor said
  that, by name. Nothing hung while they waited.
- **Something the upgrade did that nobody asked for:** install re-calibrates every time, by
  design. That moved the run-queue cut from 4.70 to **5.52** per core and the climb level to
  5.02, with b_max 4 unchanged. The verdict below takes that into account.

### Closing the week (clause 2)

The week is closed at seven full days, 11-15 to 11-21, with day zero (11-14 from 22:11)
recorded but not counted. The subjective fields are thin: the owner was asked on 11-15 and,
the day after, about 11-21. Days 2-6 are recorded as *not captured*. Asking further days
would not change the answer, for two reasons. First, the log records every override, and
there were none in eight days. Second, on the three days the owner was asked, the answer was
the same each time: the budget felt right, and no refusal was noticed or disagreed with. What
the week did find (surprises 3, 4, 6 and 7) came from the log and `pmset`, not from how the
budget felt, and more days would only add instances of the same four.

### Verdict (clause 5)

**The shipped default stays as it is: calibrate on install, and use what it measures.** On
this machine that is now a run-queue cut of 5.52 per core with b_max 4. `governor calibrate`
does not need re-running, and its defaults do not change.

The evidence: 45 cut episodes from 11-14 22:11 to 11-22, grouped by the reading that caused
them. "Refusals while cut" counts builds refused while the budget was below 4.

| Reading at the cut (rq per core) | Episodes | Refusals while cut | Within 10 s of a sleep or wake |
|---|---:|---:|---:|
| below 5.52 (cut only at 4.70) | 24 | 25 | 10 |
| 5.52 to 6.59 | 13 | 8 | 11 |
| above 6.59 (cut at any of the three) | 8 | 8 | 4 |

- **4.70 was too low for this machine.** Over half the week's cuts (24 of 45) and 25 of the
  41 refusals made while cut came from readings that 5.52 would not have cut on. The owner
  felt nothing wrong, so the cost was invisible to them, but those 25 builds were still refused.
- **6.59 is not supported, and choosing it would be tuning on this laptop.** Going from 5.52
  to 6.59 would remove 13 more episodes, but 11 of those 13 are within 10 s of a sleep or wake.
  Their real cause is the sleep transition (GOV-B48), not where the threshold sits. Once those
  are set aside, 2 mid-band cuts remain, which is no basis for raising the threshold. The cuts
  above 6.59 were real saturation: on 11-22 from 09:13 to 12:18, CPU was at 77-91% and the run
  queue at 8-27 per core, mostly from work Claude did not start. Cutting to 1 there is the
  governor doing its job.
- **The number came from the machine, which is the rule.** 5.52 is what `calibrate` measured
  on 11-22. Picking 6.59 by hand because the week looked calm would be a constant tuned on one
  laptop, which CLAUDE.md rules out.
- **One thing to watch, not act on.** Calibrations on this machine have read 6.59 (11-03),
  4.61, 4.76 and 4.70 (11-14) and 5.52 (11-22). The spread across days is wider than within a
  day, which suggests ambient load at calibration time matters. If GOV-B48's sleep fix still
  leaves false cuts at 5.52, calibration stability is the next thing to look at.
