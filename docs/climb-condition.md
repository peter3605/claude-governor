# The increase condition, and why the budget could not climb — GOV-32

**The short answer: it is over-control, the machine is measurably not full at the operating
point the loop drives to, and the fix is to stop cutting on utilisation and start climbing on
the run queue.**

GOV-31 ended by measuring the cut condition firing on 72–99% of a build fan-out's ticks and
observing that the increase condition requires `CPU < 0.65`, which a build never allows. It
filed the question it could not answer: is a budget that floors on the first fan-out and stays
floored until the load drains **correct control** or **over-control**?

This document answers it. The increase condition fires on **0%** of a fan-out's ticks — not
rarely, never — and at the operating point that produces, host CPU sits at **0.77** and the run
queue at **1.22 per core**. The machine is neither saturated nor oversubscribed. It is 23%
idle with work queued, and the loop cannot leave.

The fix is measured with the run-queue threshold held above the machine's ambient load, so that
utilisation is the only thing the two loops disagree about. Under matched conditions, replicated
and interleaved, **the shipped loop floors to 1 and the new loop holds `B_max` = 6 — while not
one tick in either arm had a run queue high enough to justify a cut** (§4.4).

**What is NOT claimed:** that this is faster. Throughput differences on this harness sit inside
its 30% run-to-run variance, and the ten-way fan-out cannot tell the two loops apart at all —
§4.1 is why, and finding that out was most of the work.

---

## 1. Method

Same harness, same host, same workload as GOV-31 — `tools/sweep`. Re-running rather than
re-reading was necessary because `pass1.json` and `pass2.json` kept summary statistics only,
and the question here is about a condition nobody had counted.

| | |
|---|---|
| Host | macOS 25.4.0, darwin/arm64, 8 logical cores (M1 Air, 4 P-cores) |
| Date | 2026-09-15 |
| Harness | `tools/sweep`, committed |
| Workload | 8 generated Go modules, 9,000 functions each |
| Fan-out arms | 10 build requests, retrying every 5 s when refused, 150 s window |
| Recovery arms | N builds held continuously, 75–120 s window, budget polled at 1 Hz |
| Binaries compared | `bin/governor-baseline` built from HEAD, `bin/governor-candidate` from this change |
| Raw results | [`docs/data/climb-condition/`](data/climb-condition/) |

**The two loops are compared as two binaries through one harness, not as two code paths in
one binary.** `tools/sweep -governor <path>` already took the binary under test, so the shipped
loop keeps exactly one implementation and the comparison costs the repository nothing. Nothing
in `internal/budget` knows there was ever another policy.

To reproduce, from a clean checkout:

```
make build && go build -o bin/sweep ./tools/sweep
git stash && make build && mv bin/governor bin/governor-baseline && git stash pop && make build

# section 2 and 4.1 — the fan-out arms
./bin/sweep -thresholds 2.0 -ungoverned -out fanout.json

# section 4.3 — one build held, at the shipped threshold
./bin/sweep -recovery 1 -window 120s -out recovery.json

# section 4.4 — the isolating pair, interleaved, run twice
for i in 1 2; do for b in baseline ""; do
  ./bin/sweep -recovery 1 -thresholds 8.0 -settle-below 4.5 -window 75s \
    -governor bin/governor${b:+-$b} -out iso-$i-${b:-new}.json
done; done
```

**The machine was NOT quiet, and on this occasion that is a first-order confound rather than a
footnote.** Throughout these runs the host was also indexing an unrelated Xcode project and
running Chrome, Docker, Messages and a Claude Code session. Ambient run queue sat near 3 per
core and swung by a factor of three between arms; §4.3 and §4.4 are where that bites, and the
canary is what makes it visible rather than invisible. GOV-31 recorded the developer's session
as a confound; this run had considerably more than that on the box and says so.

### What this run records that GOV-31's did not

**The loop's own predicates, not a copy of them.** `budget.Cutting` and `budget.Climbing` are
now exported, and the harness calls them. GOV-31's sweep reported `cpu_share_above_cut` from a
hand-copied cut condition in `tools/sweep/main.go`; the copy was evaluated against **raw** host
CPU while the loop acts on a **5 s EWMA**. Under a sustained fan-out both read 1.00, so that
discrepancy changed none of GOV-31's conclusions — but nobody could see it, because the two
expressions lived in different files. It would have mattered here: the climb needs CPU below
0.65, and a raw series dips there on brief scheduling gaps that a 5 s average absorbs.

**The per-tick series, kept.** Every arm now writes its busy-window run queue, raw CPU and
smoothed CPU to the results JSON. A fan-out arm costs four minutes of a settled machine and
cannot be reconstructed afterwards; the summary it used to collapse to costs nothing to keep
in full. This story had to re-run GOV-31's entire sweep to answer a one-line question about
the same load.

**Paired samples.** `runQueue.sample` appended to the run-queue series unconditionally and to
the CPU series only when the CPU read succeeded. Nothing read the two together, so nothing was
wrong; the cut and climb conditions are **joint** over both, so a dropped CPU read would have
shifted one series against the other from that tick onward and silently mispaired every
reading after it. Ticks are now dropped from both series or neither, and the count is reported.

### Confounds, unchanged from GOV-31 and still true

- The developer's own Claude Code session is on the machine throughout.
- One machine, one workload shape: Go compilation on an 8-core fanless laptop.
- `loadavg1` is a 60-second decaying average and a single build runs eight seconds, so the
  run-queue signal cannot see a short build at all.
- Run-to-run variance on a fixed configuration reached 30% in GOV-31. **Differences smaller
  than that between two arms mean nothing**, and the argument below is built to not need them.

---

## 2. Clause 1 — the two conditions, counted

Ten `go build -a` requests through the real gate at a real daemon, 150-second window, the
loop's own `budget.Cutting` and `budget.Climbing` evaluated against the smoothed CPU and run
queue of every busy tick:

| arm | busy ticks | **cut condition** | **climb condition** |
|---|---|---|---|
| ungoverned | 49 | **100%** | **0%** |
| governed, `RunQueueMax=2.0` | 92 | 93% | 4% |

**Under the load this governor exists for, the increase condition never fires.** Not rarely —
in 49 busy ticks of an ungoverned ten-way fan-out, zero. Host CPU read p50 1.00 raw and 1.00
smoothed, and the climb needs it below 0.65.

**The 4% on the governed arm is the more interesting number, and it is not a reprieve.** Those
are four ticks, and their run queues were 1.14, 1.31, 1.31 and 1.31 — the state of a machine
running *one* build, which is where the budget had already floored. The climb was reachable
only in the gaps the floor itself created, and each such tick could add one slot that the next
tick's pegged CPU immediately halved away. A fifth tick had smoothed CPU under 0.65 but a run
queue of 16.59, so it failed the other half.

The daemon's own event log for that arm is the shortest statement of the problem:

```
GOVERNOR_RUNQUEUE_MAX=2 overriding the compiled-in run-queue threshold of 2
budget peak  in_use=1/1
budget park  slots=1.00/1 wait=20s          ... fifteen of these
budget deny  reason=capacity                ... seventeen of these
```

Every `peak` line in the arm reads `/1`. The budget reached the floor before the first build
finished and never took another value for 150 seconds.

### The same count over the Phase 0 trace

`TestTheLoopsConditionsOverARealTrace` runs the same two predicates over the 3,000 contiguous
real samples the replay test uses — a window of ordinary developer use that happens to contain
saturation, rather than a fan-out driven on purpose:

| | share of 3,000 ticks |
|---|---|
| cut condition | 66.8% |
| climb condition | 29.9% |
| neither | 3.3% |
| **ticks at CPU ≥ 0.99** | **471 (15.7%)** |
| **of those, climb condition** | **0 (0.0%)** |

The two measurements agree, and that is worth more than either alone: one is 3,000 seconds of
the machine being itself, the other is a fan-out driven through the real gate.

**Zero is not a small number here, it is a structural one.** The climb requires
`Setpoint - CPU > Headroom`, i.e. CPU below 0.65. Saturation means CPU at 1.00. No setpoint and
headroom this design would accept can make 1.00 satisfy it — a setpoint above where builds put
CPU would have to exceed 1.00, and that is not a setpoint any more.
`TestTheClimbConditionIsUnreachableUnderSaturation` pins the property rather than the sample.

---

## 3. Clause 2 — over-control, and the evidence that decides it

The clause requires picking one answer in writing. **It is over-control.** One measurement
settles it, and it is not a throughput argument, because throughput differences on this
harness are inside the noise.

**At the operating point the loop drives to, the machine is not full.** From GOV-31's
committed concurrency curve — N concurrent builds, no governor, each level on a settled
machine:

| builds | rq p50 | **cpu p50** | wall s | wall per build |
|---|---|---|---|---|
| **1** | 1.22 | **0.77** | 8.1 | 8.06 |
| 2 | 1.53 | 1.00 | 14.0 | 7.01 |
| 3 | 2.55 | 1.00 | 21.4 | 7.13 |
| 4 | 2.85 | 1.00 | 29.6 | 7.40 |
| 6 | 4.83 | 1.00 | 43.9 | 7.32 |
| 8 | 24.98 | 1.00 | 95.7 | 11.96 |

At one build — which is where the loop floors and stays — **host CPU is 0.77 and the run queue
is 1.22 per core**. Saturation begins at two. So the defence of the current behaviour, "the
machine really is full and one build at a time is the right answer", is not a judgement call
that could go either way. It is false on this host: the machine is 23% idle with nine build
requests waiting.

Note where 0.77 sits. The loop's setpoint is 0.75 and its band is 0.05, so at the floor the
machine reads 0.77 — inside the dead zone between the cut at 0.80 and the climb at 0.65. **The
loop has converged on its setpoint.** It is doing exactly what an AIMD controller with a
utilisation setpoint of 0.75 is specified to do. The defect is not in the mechanism, it is in
the quantity: a utilisation setpoint of 0.75 asks for a machine that is three-quarters busy,
and for a build governor that is a request to leave a quarter of the machine unused while work
is queued.

**The throughput side is stated and then set aside.** The curve's wall-clock column puts the
optimum at 2–3 concurrent builds and makes one-at-a-time about 13% slower than two-at-a-time
over the same eight builds (64.5 s against 56.1 s). That is the right direction but it is a
single measurement per level against a 30% run-to-run variance, so **no conclusion here rests
on it**. The utilisation figure does not have that problem: 0.77 against 1.00 is a difference
in kind, it reproduces across every level of the curve, and it is measured host-wide rather
than through the process-tree sampler.

**A correction to how GOV-31's core-second columns should be read.** `build_test_core_sec`
comes from `internal/childcpu`, which walks the process tree at 1 Hz. `go build` spawns many
short-lived `compile` processes, so a 1 Hz walk systematically undercounts — and undercounts
*more* at low concurrency, where fewer of them are alive at any sampled instant. The curve's
5.6 core-seconds for one build cannot be reconciled with the same document's method table,
which records one build as ~36 core-seconds at ~420% over 8.5 s. The wall-clock and host-CPU
columns are unaffected. Every argument in this document uses those two.

---

## 4. Clause 3 — the fix, and what it is measured to do

### 4.1 The fan-out arms cannot answer this, and finding that out is most of the work

The obvious experiment is the one GOV-31 built: run the ten-way fan-out against the old loop
and the new one and compare. It was run, both ways, and **it does not discriminate between the
two controllers.**

| arm | canary s | refusals | build core-s | rq p50 | rq p90 | drain s | **budget** |
|---|---|---|---|---|---|---|---|
| ungoverned (baseline run) | 7.7 | 0 | 213.9 | 15.46 | 26.60 | 70.4 | — |
| shipped loop, `RunQueueMax=2.0` | 7.3 | 17 | 144.6 | 13.58 | 18.93 | 92.9 | **1, always** |
| ungoverned (candidate run) | 6.3 | 0 | 233.7 | 17.75 | 31.81 | 74.1 | — |
| new loop, `RunQueueMax=2.0` | 9.0 | 12 | 169.3 | 8.87 | 12.56 | 102.2 | **1, always** |

The two governed arms differ in every column, and **none of those differences can be
attributed to the change**, because the last column is the same. Every budget value the daemon
logged in either arm was the floor. Two controllers that never produced a different budget
cannot have produced a different outcome; what the other columns are showing is a different
machine. The canaries say so directly — 7.3 s against 9.0 s, a 23% slower host during the
candidate arm — and the run-to-run variance GOV-31 established on a *fixed* configuration was
30%, which swallows all of it.

**Why the arm is structurally blind to this.** The batch is admitted at `B_max`, so six builds
start at once before any tick lands. `loadavg1` is a one-minute decaying average, those six
take tens of seconds to drain under contention, and the run queue therefore sits between 8 and
18 per core for essentially the whole 150-second window. That is genuine oversubscription, and
**both loops are correct to floor there** — the old one because CPU is pegged, the new one
because the queue is deep. The arm is not comparing controllers; it is confirming that ten
concurrent builds oversubscribe an eight-core laptop.

This is the same shape of trap GOV-31 named and then fell into from the other side. There, the
CPU disjunct decided before the run-queue threshold could be consulted, so sweeping the
threshold measured nothing. Here, the initial burst decides before the loop can be consulted,
so swapping the loop measures nothing. **In both cases the quantity under test was downstream
of something that had already fixed the answer.**

### 4.2 The experiment that does discriminate

The regime the story is about is not an oversubscribed machine. It is a machine with builds
running on it and a queue that is *not* deep — where the old loop floors on utilisation alone
and cannot climb back. `tools/sweep -recovery N` measures exactly that, and it is a mode of the
same harness rather than a second one: same settle, same canary, same cache clear, same
isolated daemon, same `governor` binary under test.

It holds N builds running continuously for the window, relaunching each as it finishes, and
reads the budget out of `governor status` once a second. **The load is open-loop and does not
go through the gate** — the question is what the controller does *given* a load, and routing
the load through the budget would make the load a function of the answer.

It also reads the budget by polling rather than from the daemon's event log, because the log
prints a budget only when something happens, and §4.1 is what that costs: two arms whose event
logs were identical.

### 4.3 At the shipped threshold, on a real developer machine, both loops still floor

The first recovery pair was run at `RunQueueMax = 2.0`, one build held, 120 seconds.

| | budget trajectory | rq p50 | smoothed cpu p50 | ticks with rq > 2.0 |
|---|---|---|---|---|
| shipped loop | 6, 6, 3, **1** — then 1 for 117 more ticks | 2.91 | 0.82 | 73% |

That is the defect photographed: **the budget reaches the floor in four seconds and stays there
for the remaining 116**, with one build on the machine.

But it does not discriminate either, and the reason is worth more than the arm. **Holding one
build produced a run queue of 2.91 per core, not the 1.22 the concurrency curve records for a
build alone.** The difference is the rest of the machine — during these runs this host was also
indexing an unrelated Xcode project, running Chrome, Docker and a Claude Code session. At 2.91
the run queue is over the 2.0 cut on 73% of ticks, so **the new loop floors here too, and is
right to.**

**This is a finding about the shipped threshold, not about the change.** `RunQueueMax = 2.0` was
chosen against a Phase 0 trace whose run queue is p50 0.352, and against a concurrency curve
measured on a *settled* machine. A working developer machine idles closer to 3 per core. At 2.0
the budget will floor on ambient load alone, whichever loop is running. §7 is where that goes.

### 4.4 With the cut moved above the ambient load, the two loops separate completely

To see the change rather than the machine, the run-queue threshold is raised to **8.0 on both
binaries** — above this host's ambient load — so that it cannot be what decides. What is left is
the only thing the two binaries disagree about: whether utilisation alone can cut, and whether
the climb needs CPU headroom.

One build held, 75-second window, **arms interleaved baseline → candidate → baseline →
candidate** so that drift in the machine cannot line up with the binary:

| rep | binary | canary s | rq p50 | smoothed cpu p50 | ticks CPU > 0.80 | ticks rq > 8.0 | **budget over 75 ticks** |
|---|---|---|---|---|---|---|---|
| 1 | shipped | 14.2 | 4.68 | 0.94 | 99% | **0%** | **1 on every tick** |
| 1 | new | 13.8 | 4.40 | 0.95 | 95% | **0%** | **6 on every tick** |
| 2 | shipped | 12.3 | 3.82 | 0.89 | 95% | **0%** | 6, 6, 6, 3, then **1** for 71 ticks |
| 2 | new | 11.1 | 3.97 | 0.88 | 95% | **0%** | **6 on every tick** |

**The conditions are matched and the outcomes are opposite.** Run queue p50 lands between 3.82
and 4.68 in all four arms; smoothed CPU between 0.88 and 0.95; the canaries span 11.1 to 14.2 s
and do not line up with the binary. **In not one of the four arms did the run queue exceed the
cut threshold — 0% of every arm's ticks.** Nothing in the load justified a cut anywhere in this
table.

The shipped loop floored anyway, both times. The new loop held `B_max`, both times. **The only
input that differs between the two is host utilisation, which sat above the old cut of 0.80 on
95–99% of ticks in every arm.** That is the CPU disjunct doing the flooring, isolated, with the
run queue held out of the way and the machine held constant.

Replicate 2's shipped-loop arm is the clearest single trace in this document: **6, 6, 6, 3, 1**
— multiplicative decrease from the ceiling to the floor in five seconds — and then 71 consecutive
ticks at 1, with one build on the machine, a run queue under half the cut threshold, and nine
tenths of the host's capacity in use. It cannot climb, and there is nothing in the load telling
it not to.

**A first attempt at this pair was discarded and is worth recording**, because it is the trap
this whole story is about. Run without interleaving, the shipped arm drew a moment when the host
was busy with unrelated work: run queue p50 13.30 against the candidate arm's 4.13, and a canary
of 32.7 s against 13.4 s — a machine two and a half times slower. It produced the same headline
(budget 1 against 6) for the wrong reason: at 13.30 the run queue is over the threshold on 61%
of ticks and **the new loop would have cut too.** A table that looked like a result was a
measurement of two different machines. Interleaving and the canary are what caught it.

---

## 5. The loop's new shape

```
if thermal is serious or critical:    B = B_min
elif run_queue > 2.0:                 B = max(B_min, B // 2)   # oversubscribed
elif run_queue <= 1.5 and demand:     B = min(B_max, B + 1)    # room
else:                                 B = B                    # busy and coping
```

Three things changed and each is forced by a measurement rather than chosen.

**Utilisation left the cut, and it had to.** The branches are evaluated in order, so any
unconditional CPU cut fires first and pins the budget at the floor for as long as the machine
is saturated — whatever the climb condition says. There is no version of this fix that keeps
`CPU > setpoint + band` as a cut on its own; the only choices were to remove it or to conjoin
it with a queue condition, and conjoining it reintroduces the hunt described below. This is
worth stating plainly because "just make the climb reachable" is the obvious reading of the
story title and it does not work.

**The climb moved onto the run queue, because that is the signal that still moves.** CPU% reads
1.00 at two builds, four builds and eight builds; the run queue reads 1.53, 2.85 and 24.98
across the same three states. §5.1 of the design gives the run queue exactly this job — "catches
contention CPU% misses" — and until now it was applied only to the cut.

**A dead band appeared, and it is what makes the loop settle.** Once both directions read one
signal, a single threshold means the loop cuts at a level and then immediately climbs back to
it. So the climb threshold sits one `RunQueueBand` below the cut threshold, and between them
the loop holds. The arithmetic against this host's measured curve:

| band | climbs at | what happens |
|---|---|---|
| 0.25 | 1.75 | above two builds' 1.53, so it climbs to three, finds 2.55 over the cut, halves to one — hunts |
| **0.50** | **1.50** | just under two builds' 1.53, well over one build's 1.22 — climbs off the floor, holds at two |
| 1.00 | 1.00 | below even one build's 1.22 — the budget can never leave the floor, the old defect in a new signal |

**The gap is narrow and the signal is noisy, and that is a real limit rather than a detail.**
1.53 and 1.50 differ by 2%, and `loadavg1` is a one-minute decaying average. The operating
point is a region the loop hunts within, not a fixed point. What bounds the hunting is the
replay test's 10% reversal budget (§6), and what the loop does against this machine's own
measured response is `TestTheLoopSettlesWhereTheMachineSaturates`, which closes the curve above
back onto the controller and reports where it lands.

**What this gives up: reaction speed on a rising edge.** A 5 s EWMA of CPU sees a fan-out in
about two seconds; `loadavg1` crossing 2.0 from a settled 0.79 takes about thirteen. The reason
that is affordable is invariant 5 — **shrinking the budget never kills a running agent** — so a
slower cut cannot claw back work already admitted, and only ever affects the *next* admission.
The initial burst is admitted at `B_max` under either loop. What it costs is at most an extra
admission or two in an eleven-second window, and §4 measures the total rather than arguing it.

---

## 6. Clause 4 — what the replay test now claims

`internal/budget/aimd_replay_test.go` replays 3,000 contiguous real 1 Hz samples — the window
of the Phase 0 trace with the most setpoint crossings, run queue peaking at 49.6 per core.

**No assertion threshold in it was changed.** That is the direct answer to the clause: the
figures below moved, every bound they are checked against stayed where it was, and all of them
still pass with margin.

| | before GOV-31 | at 2.0 (GOV-31) | now | the test's bound |
|---|---|---|---|---|
| distinct values | 6 | 6 | 6 | ≥ 3 |
| direction reversals | 1.4% | **2.7%** (81) | **0.3%** (9) | < 10% |
| share at the floor | 76% | 66.8% | 67.7% | ≥ 10% and ≤ 95% |

Two of those columns had never been re-derived. **GOV-31 raised `RunQueueMax` from 1.2 to 2.0
and did not re-run this test**, so design.md carried 1.4% and 76% while the code produced 2.7%
and 66.8%. Measuring HEAD before changing anything is what surfaced it, and both intermediate
values are recorded above rather than quietly skipped over.

**What each number now means, re-derived rather than carried over.**

- **Reversals rose at 2.0 and then fell by an order of magnitude here, and both movements have
  one cause.** At 1.2 the run-queue test fired on nearly everything the CPU test did, so the
  two agreed and the budget moved once per real change in load. Raising the threshold to 2.0
  opened a band of ticks where CPU cut and the run queue did not, and the budget alternated
  across it — that band is what 2.7% was measuring. Removing utilisation closes it. 0.3% is
  the loop responding to the load and to nothing else.
- **Time at the floor is unchanged, and that is the result to want.** 66.8% → 67.7% over a
  window whose run queue peaks at 49.6 per core. This change was not meant to make the loop cut
  less on a genuinely oversubscribed trace, and the measurement says it did not. A drop here
  would have been the thing to worry about.
- **The settling claim still means what it says.** "Settled" over a trace that crosses the
  setpoint 328 times cannot mean "stopped moving"; it means not thrashing, and 9 reversals in
  3,000 ticks is a loop that is tracking rather than hunting.

Two tests were replaced rather than edited, because they asserted the behaviour this story
removed and editing them would have erased the finding they recorded:

- `TestUnderBuildLoadTheSetpointBindsAndTheRunQueueDoesNot` pinned GOV-31's real result — that
  at build-load CPU the budget floors identically at every run-queue threshold from 1.2 to 6.0.
  Its successor asserts the mirror image: that the threshold now decides, so a different
  threshold gives a different answer. The old test's claim is quoted in the new one's comment.
- `TestTheClimbConditionIsUnreachableUnderSaturation` was written *by this story*, in its first
  hour, to characterise the loop before changing it. It is now
  `TestASaturatedMachineWithAShallowQueueIsNotTrouble` and asserts the opposite. Both versions
  are correct about their own loop, which is the point of writing the first one down.

Added rather than replaced: `TestADeepQueueStillCutsWhateverTheUtilisation`, because the change
*removed a cut trigger* and the regression that matters is the governor quietly ceasing to
govern. It drives the run queue produced by ten concurrent builds — 24.98 per core — at four
different utilisations and requires the floor at every one.

---

## 7. Limits, and what this hands to the next story

**`RunQueueMax = 2.0` is now load-bearing, and it was chosen while it was not.** GOV-31 settled
on 2.0 from the ordinary-use distribution and the concurrency curve, *explicitly because* its
sweep could detect no difference between 1.2 and 6.0 under build load — and the reason it could
detect none is that the CPU test decided every cut before the threshold was consulted. Removing
the CPU test removes that shadow. `RunQueueMax` and `RunQueueBand` together now set the
operating point under exactly the load the sweep was unable to see them under. **The sweep that
found nothing is worth re-running against this loop, and it would this time be measuring a live
constant.** The value is unchanged pending that, which is a deliberate choice not to retune a
constant and change the loop's shape in the same story.

**The dead band is 0.5 wide and sits where the noise is.** Two builds measure 1.53 per core and
the climb threshold is 1.50 — a 2% margin, on a signal that is a one-minute decaying average.
The operating point is a region, not a fixed point.

**One machine, one workload shape, one signal that cannot see short work.** Go compilation on
an 8-core fanless laptop, with the developer's own Claude Code session on the box throughout.
`loadavg1` cannot see a build that runs for eight seconds, so every threshold here is a
statement about *sustained* load. An iOS UI suite, a large pytest run, or an I/O-bound suite
would each sit differently, and the Phase 0 window under-represents all three. This is the same
limit GOV-31 recorded and nothing here improves on it.

**The loop is slower on a rising edge than it was.** A 5 s EWMA of CPU sees a fan-out in about
two seconds; `loadavg1` crossing 2.0 from a settled 0.79 takes about thirteen. §5 argues that
invariant 5 makes this affordable — a cut never revokes running work, so a slower cut only ever
affects the next admission — and §4.1's fan-out arms are consistent with that, since both loops
floored and neither admitted more than the initial burst. **It is not separately measured**, and
a workload of many short build requests arriving steadily is the shape that would test it.

**What `build_test_core_sec` is worth.** It comes from a 1 Hz walk of the process tree, and
`go build` spawns many short-lived `compile` processes, so it undercounts — and undercounts
more at low concurrency, where fewer of them are alive at any sampled instant. The concurrency
curve's 5.6 core-seconds for one build cannot be reconciled with the same document's method
table recording ~36. Nothing in this document rests on that column; the wall-clock and host-CPU
columns are unaffected and are what the arguments use. Correcting the instrument is not this
story's work, but reading GOV-31's core-second columns as a measure of *work* is a mistake, and
it is recorded here so the next reader does not make it.
