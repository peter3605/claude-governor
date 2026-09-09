# Did a fixed budget of four actually help?

GOV-11's last clause: *"A note in docs records whether the fixed budget actually helped."*
This is that note. It is written in two halves — the method and the reading rule first, the
result afterwards — because the question is the kind that is easy to answer by feel, and a
week of impressions is exactly the evidence this repo does not accept anywhere else.

**Status: week in progress. Started 2026-09-09.** The Results section below is empty on
purpose and is filled in at the end, not as it goes.

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

## Results

*Empty until the week is up. Filled in from `daemon.log`, against the table above, without
revising the table.*

### Session failures attributable to the governor

*To be recorded. Nil is a result and gets stated.*

### Verdict

*To be recorded.*
