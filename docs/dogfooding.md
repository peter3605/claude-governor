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

`governor install` puts the plugin in `~/.claude/skills/claude-governor/` and registers the
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
