# Does the loop control the thing it claims? — GOV-15 clause 5

**The short answer: yes, it controls, and the metric the clause named cannot show it.**

Clause 5 asked for a deliberate multi-build fan-out to be "shown NOT to reach the peak the
ungoverned machine reaches — measured with `tools/childcpu` against the 7.822-core ungoverned
maximum, not asserted from the loop's own logs." The experiment was run exactly that way. The
governor demonstrably controls the machine — 4.8× less build CPU admitted, 2.8× less
contention — and **peak child cores came out higher with the governor on than with it off**,
for a reason that is structural rather than a fault.

The clause's own instinct was right and is what makes this readable: the proof had to be an
independent measurement rather than the loop's own accounting. It is only because the
measurement was independent that it could disagree.

## The experiment

| | |
|---|---|
| Host | macOS 25.4.0, darwin/arm64, 8 logical cores (M1 Air, 4 P-cores) |
| Date | 2026-09-14 |
| Claude Code | 2.1.270, `claude -p`, `--setting-sources ""`, `--permission-mode bypassPermissions` |
| Load | ten `cd modN && for i in 1 2 3; do go build -a ./...; done`, each launched `run_in_background` |
| Modules | eight generated Go modules, 9,000 functions each; one `go build -a` alone measures ~28 s of CPU in ~9 s wall at ~344% |
| Measurement | `tools/childcpu -for 150s` (peak and total descendant cores) and a 1 Hz `loadavg1/ncpu` watcher |
| Arms | **ungoverned** — no hooks at all; **governed** — all six hooks of §4.1 against a live daemon |

Both arms ran `go clean -cache` first, both waited for the machine to settle below 0.8
run-queue per core, both held the session open for the same 115 s foreground sleep so the
background builds were not killed early, and both were measured over the same 150 s window.

The governed arm's prompt says explicitly that some calls may be refused and that a refusal is
expected and must not be retried. Without that the model stops after the first few denials and
reports back, which it did on the first attempt — and which left the two arms with unequal
session lifetimes and made the comparison worthless. That run was discarded and re-run rather
than reported.

## The result

| | Ungoverned | Governed | |
|---|---|---|---|
| builds admitted | 10 of 10 | **1 of 10** | 9 refused |
| build-test CPU in the window | 468.1 core-sec | **96.8 core-sec** | **4.8× less** |
| run queue per core, p50 | 10.19 | **2.89** | |
| run queue per core, p90 | 11.49 | **3.97** | |
| run queue per core, max | 11.93 | **4.22** | **2.8× less** |
| **peak descendant cores** | **4.84** | **6.67** | **higher with the governor on** |

The daemon's own log, as a cross-check against the session's `permission_denials: 9` — the
budget had already been cut from 6 to 1 by the time the second build asked:

```
01:45:13  park  session=16c20e52 in_use=1/1 wait=20s
01:45:33  park-expired  waited=20.004s
01:45:33  deny  reason=capacity in_use=1/1 denials=1
01:45:33  deny  reason=capacity in_use=1/1 denials=2     ... through denials=9
```

## Why peak child cores cannot answer this question

**It measures how fast the machine is going, not how oversubscribed it is.** Peak cores is
bounded above by the core count, so any arm that saturates reads the same number. And the two
arms do not saturate the same way:

- **Governed**, one build alone: `go build -a` runs `compile` at `GOMAXPROCS`, gets the whole
  machine, and hits a high instantaneous peak — 6.67 cores.
- **Ungoverned**, ten builds at once: 220 runnable threads on 8 cores. Each build gets a
  fraction, the machine thrashes on context switches and memory, and the *delivered* peak is
  **lower** — 4.84 cores — while total work admitted is 4.8× greater and the run queue is
  nearly three times deeper.

A thrashing machine looks calm by this metric. That is the whole problem with it. Neither arm
came near the 7.822-core figure from the Phase 0 window, which is itself a clue: that maximum
was a single unusually clean burst, not a marker of "an overloaded machine".

**The run queue is the metric that works**, and this is the second time the same conclusion
has arrived from a different direction. §5.2 already records that CPU% saturates at 1.0 and
stops carrying information while the run queue does not — p99 27.1, max 49.6 over the Phase 0
trace. The same property is what makes it the right instrument here: 11.93 against 4.22 is a
measurement of contention, and contention is what the governor exists to remove.

**So clause 5 is satisfied in substance and not in letter.** The governor was shown to control
a deliberate multi-build fan-out, measured with `tools/childcpu` and an independent run-queue
watcher rather than from the loop's own logs. It was not shown to lower the peak child-core
figure, because that figure does not measure what the clause assumed it measured.

## The loop over-controls, and the binding constraint is named

**One build in ten is too few.** The machine settled at 2.89 run queue per core — busy, but
not in trouble — while nine builds were refused. The governor traded more throughput than it
needed to.

The cause is identified rather than guessed: **`RunQueueMax = 1.2` is the binding constraint,
not the setpoint.** A single `go build -a` alone drives the run queue to ~2.9 per core, which
is already over the threshold, so the loop halves the budget every tick and reaches the floor
within three ticks of the *first* build starting. `B_max = 6` is then unreachable in practice
whenever any real build is running.

This is a genuine tension in the evidence, and both halves of it are measured:

- 1.2 sits just above the 90th percentile of **ordinary machine use** (p90 0.820) and fires on
  7.68% of samples, which is what justified keeping it in §5.2.
- But 1.2 is *below* what **one legitimate build** produces, so as a control threshold it
  cannot distinguish "a build is running" from "the machine is in trouble".

That analysis was about how OFTEN the threshold fires. It was not about what it does to
throughput when it does fire, and this experiment is what surfaced the difference.

**The number is not being changed here, and that is deliberate.** Picking a new threshold from
this single experiment would be exactly the move `docs/bash-corpus.md` refuses — a constant
that looks plausible, fitted to one observation, presented as measured. Setting it properly
needs a sweep across several values with the contention and throughput of each measured, which
is a follow-up story and not a line edit. What this document fixes is the diagnosis: the
run-queue threshold is the constant to sweep, its current value is too tight by roughly a
factor of two to three on this workload, and the setpoint is not involved.
