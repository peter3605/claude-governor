# Sweeping the run-queue cut threshold — GOV-31

**The short answer: the threshold moves 1.2 to 2.0, but the finding worth having is that under
build load this constant is very nearly inert and the CPU setpoint is what binds.**

GOV-31 was filed on GOV-15's diagnosis: `RunQueueMax = 1.2` is below what one legitimate
`go build -a` produces, so the loop halves the budget every tick and floors within three ticks
of the first build, making `B_max` unreachable. The remedy the story names is a sweep of that
threshold and a new value for it.

The sweep was run. Five values of `RunQueueMax` were measured twice under the same ten-way
fan-out, and **the value makes no difference that this experiment can detect** between 1.2 and
6.0. The reason is measured rather than argued: a build fan-out pegs host CPU, and the loop
cuts on

```go
case r.CPU > Setpoint+SetpointBand || r.RunQueue > c.runQueueMax():
```

so the **CPU disjunct fires on 72–99% of ticks whatever the run-queue threshold is**. The
run-queue test is reachable as a decision only when CPU is *not* pegged, and under the load
this governor exists for, CPU is essentially always pegged.

Two of GOV-31's own premises did not survive contact with the measurement, and both are
recorded here rather than quietly dropped.

---

## 1. Method

| | |
|---|---|
| Host | macOS 25.4.0, darwin/arm64, 8 logical cores (M1 Air, 4 P-cores) |
| Date | 2026-09-14 |
| Harness | `tools/sweep`, committed — this document is reproducible by running it |
| Workload | 8 generated Go modules, 9,000 functions each, importing a broad slice of stdlib |
| One build | `go build -a ./...`, ~36 core-seconds in ~8.5 s wall at ~420% |
| Batch | 10 build requests, retrying every 5 s when refused |
| Window | 150 s per arm, identical for every arm |
| Admission | the real `governor gate` binary against a real, isolated daemon |
| Measurement | `internal/childcpu` for core-seconds; an independent 1 Hz `loadavg1/CPUCapacity` and `cpu.Percent` watcher |
| Raw results | [`docs/data/run-queue-sweep/`](data/run-queue-sweep/), including each governed arm's daemon budget log |

To reproduce the two threshold passes and the curve:

```
make build && go build -o bin/sweep ./tools/sweep
./bin/sweep -thresholds 1.2,2.0,3.0,4.5,6.0 -out pass.json
./bin/sweep -concurrency 1,2,3,4,6,8 -window 300s -out curve.json
```

`-reverse` runs the arms in the opposite order, which is how an effect that belongs to the
threshold is told apart from one that belongs to the running order.

Every arm settles below 0.8 run queue per core before it starts, runs a **canary** — one build
alone, timed — then clears the build cache. The daemon is restarted per arm so the loop begins
at its ceiling rather than inheriting the previous arm's floored budget.

### What the harness does that GOV-15's run did not

**It does not drive a Claude session.** The load is launched by the harness and admitted
through the same gate binary Claude Code invokes, with the same PreToolUse payload shape
(§19 of [hook-surface.md](hook-surface.md)). Both of GOV-15's experiment traps therefore stop
existing rather than having to be worked around: `claude -p` cannot kill the load early
because there is no session to exit, and the model cannot stop on a refusal because there is
no model. Those two traps cost GOV-15 a discarded run; a sweep of five thresholds run twice
would have had ten chances to hit them.

**The percentiles cover only the ticks where work was outstanding.** Every arm is held open
for the same fixed window, but they do not stay busy for the same share of it, and the arm
that drains fastest would otherwise collect the most idle samples and be flattered by them.
The whole-window series is recorded beside the busy one so the difference is visible.

**A canary build runs before each arm.** One build, alone, on a machine just declared settled
— its wall time is a reading of how fast this machine is right now. It exists because the
first pass produced build CPU falling monotonically with arm order, which looked exactly like
thermal drift on a fanless laptop. The canary says it was not: 8.5, 7.1, 7.8, 7.7, 7.3, 8.1 s
across the six arms of the second pass, flat within noise.

### Confounds this experiment does NOT control

- **The developer's own Claude Code session is on the machine throughout.** Its baseline sits
  near 0.7–0.8 run queue per core, which is the same order as the settle threshold. Every
  number here includes it.
- **One machine, one workload shape.** Go compilation on an 8-core fanless laptop. A threshold
  fitted to `go build` may be wrong for an iOS UI suite or a large pytest run, which are the
  modes the Phase 0 window already under-represents.
- **`loadavg1` is a 60-second decaying average and several of these events are shorter than
  that.** A single build runs 8.1 s — the average has 8 s to respond to it, so the run queue
  attributed to one build is mostly the residue of settling. This is a real limit on the whole
  instrument and it is discussed in §4.
- **Run-to-run variance is large and is not eliminated by replication at n=2.** The same
  threshold measured twice gave 147.3 and 194.2 core-seconds at 3.0, and 135.0 and 99.1 at
  6.0. Differences smaller than about 30% between arms mean nothing here.
- **Arm order is not randomised**, only reversible. The `-reverse` flag exists; the passes
  reported here are both forward, with the canary standing in as the drift check.

---

## 2. The threshold sweep — raw numbers

Two full passes, same parameters. Pass 2 added the CPU and canary instrumentation.

### Pass 1

| arm | refusals | build core-sec | rq p50 | rq p90 | rq max | drain s |
|---|---|---|---|---|---|---|
| ungoverned | 0 | 323.6 | 16.96 | 28.09 | 28.51 | 85.8 |
| `RunQueueMax=1.2` | 10 | 212.5 | 10.29 | 13.51 | 14.48 | 97.5 |
| `RunQueueMax=2.0` | 14 | 193.7 | 6.02 | 7.94 | 8.20 | 83.1 |
| `RunQueueMax=3.0` | 15 | 147.3 | 3.23 | 4.11 | 4.16 | 102.7 |
| `RunQueueMax=4.5` | 11 | 193.7 | 8.56 | 9.95 | 10.14 | 88.4 |
| `RunQueueMax=6.0` | 19 | 135.0 | 3.53 | 3.83 | 3.84 | 94.8 |

### Pass 2, with host CPU and the canary

| arm | canary s | refusals | build core-sec | rq p50 | rq p90 | rq max | cpu p50 | cpu > cut | drain s |
|---|---|---|---|---|---|---|---|---|---|
| ungoverned | 8.5 | 0 | 323.9 | 17.57 | 28.80 | 28.87 | 1.00 | 97% | 85.1 |
| `RunQueueMax=1.2` | 7.1 | 10 | 205.2 | 6.12 | 8.50 | 8.63 | 1.00 | 75% | 80.4 |
| `RunQueueMax=2.0` | 7.8 | 12 | 188.7 | 8.04 | 10.58 | 10.97 | 1.00 | 75% | 79.0 |
| `RunQueueMax=3.0` | 7.7 | 9 | 194.2 | 10.20 | 13.12 | 14.02 | 1.00 | 72% | 83.1 |
| `RunQueueMax=4.5` | 7.3 | 11 | 202.6 | 9.44 | 12.73 | 13.62 | 1.00 | 76% | 82.7 |
| `RunQueueMax=6.0` | 8.1 | 12 | 99.1 | 1.89 | 2.02 | 2.11 | 0.80 | 49% | 83.2 |

`cpu > cut` is the share of busy ticks already above `Setpoint + SetpointBand` — the share on
which the CPU disjunct fires on its own.

### What these say

**Governing versus not governing is large and reproducible.** 323.6 and 323.9 core-seconds
ungoverned across two passes — the tightest agreement anywhere in this document — against
roughly 190–205 governed. Run queue p50 falls from ~17 to 6–10. That effect is real and is not
in question.

**The threshold's value between 1.2 and 4.5 is not detectable.** Build CPU across those four
arms in pass 2 is 205.2, 188.7, 194.2, 202.6 — a 9% spread, against a run-to-run variance on
the same threshold of 30%+. Drain time is 80.4, 79.0, 83.1, 82.7 — a 5% spread. Run queue p50
moves 6.12 → 8.04 → 10.20 → 9.44, which is the *wrong direction* and is noise.

**The 6.0 arm is the least stable thing here and no conclusion rests on it.** It gave 135.0
core-seconds in pass 1, 99.1 in pass 2, and 198.7 in a third, shorter run. Its low-contention
readings are not evidence that a high threshold reduces contention.

---

## 3. Why the sweep found nothing: the daemon's own log

The budget event log from a run at 1.2 and a run at 6.0, side by side, is what makes this
legible. It is quoted rather than summarised because it is the evidence.

```
GOVERNOR_RUNQUEUE_MAX=1.2 overriding the compiled-in run-queue threshold of 1.2
budget peak  in_use=1/1
budget park  slots=1.00/1 wait=20s          ... nine of these
budget park-granted waited=1.887s           ... five of these
budget peak  in_use=6/1
budget deny  reason=capacity slots=6.00/1 denials=1
```

```
GOVERNOR_RUNQUEUE_MAX=6 overriding the compiled-in run-queue threshold of 1.2
budget peak  in_use=1/1
budget park  slots=1.00/1 wait=20s          ... nine of these
budget park-granted waited=1.888s           ... five of these
budget peak  in_use=6/1
budget deny  reason=capacity slots=6.00/1 denials=1
```

**They are the same trace.** In both, the budget is at the floor (`/1`) within two seconds,
having admitted an identical initial burst of six while it was still at the ceiling, and in
neither does it ever climb back.

The mechanism is in the loop's two branches:

- The cut fires on `CPU > 0.80` **or** `RunQueue > RunQueueMax`. Under a fan-out, CPU is 1.00,
  so the left disjunct is true on 72–76% of busy ticks and the right one never gets to matter.
- The climb requires `Setpoint - CPU > Headroom`, i.e. **`CPU < 0.65`**. That never happens
  while builds are running. So once cut, the budget cannot recover until the load drains,
  whatever the run-queue threshold is.

`RunQueueMax` is not a live decision point under build load. It is shadowed by the CPU test on
both the way down and the way up.

---

## 4. The concurrency curve, and two corrections to GOV-31's premise

To reach the regime where the run-queue test *is* the deciding one, the load has to be small
enough that CPU is not pegged. This is that measurement: N concurrent builds, no governor at
all, each level on a settled machine.

| builds | rq p50 | rq p90 | rq max | cpu p50 | cpu > cut | build core-sec | wall s |
|---|---|---|---|---|---|---|---|
| 1 | 1.22 | 1.22 | 1.22 | 0.77 | **50%** | 5.6 | 8.1 |
| 2 | 1.53 | 2.72 | 2.72 | 1.00 | **85%** | 22.0 | 14.0 |
| 3 | 2.55 | 3.57 | 3.57 | 1.00 | 95% | 64.9 | 21.4 |
| 4 | 2.85 | 4.78 | 4.78 | 1.00 | 97% | 89.2 | 29.6 |
| 6 | 4.83 | 10.91 | 10.91 | 1.00 | 95% | 167.6 | 43.9 |
| 8 | 24.98 | 42.43 | 42.55 | 1.00 | 99% | 285.7 | 95.7 |

### Correction 1 — one build produces 1.22, not ~2.9

GOV-31 states, as the fact the whole story rests on, that "a single legitimate `go build -a` on
an 8-core M1 Air drives the run queue to ~2.9 per core ON ITS OWN", and concludes the threshold
is "too tight by roughly a factor of two to three".

Measured on a settled machine, one build gives **run queue p50 1.22, p90 1.22, max 1.22**. That
is over 1.2, but by 2%, not by a factor of two to three. GOV-15's 2.9 was read during its
governed fan-out arm — a machine with nine other build requests arriving and a loadavg still
carrying the previous arm — not from one build in isolation.

The defect GOV-31 names is therefore real in direction and roughly twenty times smaller in
magnitude than stated.

### Correction 2 — `loadavg1` cannot measure a single build at all

One build runs 8.1 seconds. `loadavg1` is a one-minute decaying average. It has 8 seconds to
respond, so the 1.22 above is mostly the residue of the settle threshold (0.79) with a small
rise on top, and the curve's lower rows are measuring the *duration* of the load as much as its
depth. This is visible in the curve's own shape: the jump from 6 builds (4.83) to 8 builds
(24.98) is far steeper than the work ratio, because 8 builds ran 95.7 s — long enough for the
average to converge — while 6 builds ran 43.9 s and did not.

**Any threshold on `loadavg1` is therefore a statement about sustained load, not about bursts.**
A short build is invisible to this signal almost by construction. That is a property of the
instrument the design chose, and it is not fixable by picking a different number for it.

### What the curve says about the threshold's job

§5.1 gives the run queue a specific job: catch contention CPU% cannot see, because CPU
saturates at 1.0 and a run queue does not. The curve shows CPU saturating from **two** builds
onward and staying there. So among the states the governor actually needs to tell apart —
2 builds, 4 builds, 8 builds — CPU carries no information at all and the run queue carries all
of it, moving 1.53 → 2.85 → 24.98.

At 1.2 the run-queue test fires on every one of those states, which makes it redundant with the
CPU test rather than complementary to it. That is a genuine design problem, and it is the one
this sweep found. But it is a problem with **both** constants considered together, not one that
a new value for `RunQueueMax` alone fixes — because the CPU test would still fire first on
every tick of every one of those states.

---

## 5. What follows for the constant

**`RunQueueMax` goes from 1.2 to 2.0.** The change is small because the defect turned out to be
small, and it is made for a reason the sweep can defend rather than for the reason the story
predicted.

### What it costs, in the regime where this test actually decides things

Re-counted over the Phase 0 trace — 379,915 host samples, p50 0.352, p90 0.813, p95 2.664,
p99 24.584, max 49.620, materially the same distribution GOV-15 read off 312,515 of them —
the share of ordinary samples each candidate fires on:

| threshold | fires on | |
|---|---|---|
| 1.2 | 7.54% | was here |
| 1.5 | 6.51% | |
| **2.0** | **5.62%** | **now here** |
| 2.5 | 5.13% | |
| 3.0 | 4.76% | |
| 4.5 | 4.09% | |
| 6.0 | 3.75% | |

against **4.27%** for CPU over the setpoint.

So the cost is **1.92 points of coverage in ordinary use**. That is the same order as the 1.5
points which span the setpoint's entire plausible range and which GOV-15 judged too small to
tune on. At 2.0 the run-queue test still fires on more of the trace than the CPU test does,
which is the property that makes it worth carrying.

### What it buys

A single Medium build stops flooring the machine's whole build concurrency. One `go build -a`
alone measures 1.22 run queue per core, so at 1.2 it cut the budget by 2% of margin; at 2.0 it
does not. `TestOneMediumBuildsRunQueueDoesNotFloorTheBudget` pins that.

### Why 2.0 and not its neighbours

- **1.5** clears one build's 1.22 by only 23%. A developer machine with a slightly busier
  baseline re-creates the defect, and most of the 1.22 is baseline rather than build (§4).
- **2.5 and above** converge on the CPU test's own 4.27% firing rate, so the run-queue test
  stops carrying information independent of the test that already fires — in the one regime
  where it is currently doing the work.
- **3.0** is above what *three* concurrent builds produce at p50 (2.55 measured), so three
  builds on an eight-core machine would never trip it at all.
- **2.0** sits above one build (1.22) and above two builds' typical state (p50 1.53), and below
  two builds' peaks (p90 2.72) and three builds' p50 (2.55). It tolerates the states the slot
  ceiling already governs and begins reacting as the machine goes past them.

### The trade in the terms GOV-31 clause 3 names

The clause asks for the chosen threshold's cost in contention and its gain in throughput,
against its neighbours. Stated directly, from pass 2:

| | 1.2 | **2.0** | 3.0 | 4.5 |
|---|---|---|---|---|
| contention, run queue p50 | 6.12 | **8.04** | 10.20 | 9.44 |
| throughput, builds completed | 10/10 | **10/10** | 10/10 | 10/10 |
| throughput, drain time | 80.4 s | **79.0 s** | 83.1 s | 82.7 s |
| build CPU admitted, core-sec | 205.2 | **188.7** | 194.2 | 202.6 |

**Every one of those differences is smaller than the variance of the same threshold measured
twice** — 147.3 against 194.2 core-seconds at 3.0, 135.0 against 99.1 at 6.0. So the answer
clause 3 asks for is that the cost is zero and the gain is zero, to the resolution this
experiment can reach, and that answer is the same for every candidate in the range.

This is worth stating plainly rather than dressing up: **the trade the clause was written to
elicit does not exist in the fan-out regime**, because the threshold is not what decides
anything there (§3). The argument above for 2.0 is therefore made from the ordinary-use
distribution and the concurrency curve. That is a weaker footing than the clause intended, and
it is the reason this document spends more words on why the sweep found nothing than on the
value it settled.

What the choice is emphatically *not* justified by is "it sits above the p90 of ordinary use",
which the clause rules out by name — and rightly, since that is the reasoning that produced
1.2. The binding reason for 2.0 is a measured fact about builds: one Medium build produces
1.22, and the threshold must sit above it.

### The honest caveat on all of that

**Under build load this constant is nearly inert at any of those values**, and the sweep is
what established it. The argument above is therefore made almost entirely from the Phase 0
distribution and the concurrency curve, not from the fan-out sweep — because the fan-out sweep's
finding is that it has nothing to say about which value is better. A reader who wants to know
what the governor does to a fan-out should read §3, not this section.

### The finding that deserves a follow-up

Under build load the CPU setpoint binds on 72–99% of ticks, the run-queue threshold binds on
none, and the budget cannot climb back while any build runs because the increase requires
`CPU < 0.65`. A fan-out therefore floors the budget and pins it there until the load drains.
Whether that is correct control or over-control is a question about `Setpoint` and `Headroom`
together — and about whether an AIMD loop whose increase condition is unreachable under the
load it governs is the right shape at all. It is not settled here, and it is not a tuning
question about `RunQueueMax`.

## 6. One honest limit on everything above

This is one machine, one workload shape, and the developer's own session was on the box the
whole time. The workload is Go compilation, which pegs CPU quickly and runs in bursts shorter
than the averaging window of the signal being measured. An iOS UI suite, a large pytest run, or
an I/O-bound test suite would each sit differently against both thresholds, and the Phase 0
window under-represents all three. Nothing here should be read as a general statement about
what a run-queue threshold is worth — only about what it is worth to `go build` on this host.
