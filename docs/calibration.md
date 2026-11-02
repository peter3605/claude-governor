# Calibrating the run-queue cut threshold — GOV-25

GOV-31 swept the run-queue cut threshold and found it inert: five values from 1.2 to 6.0,
measured twice under the same ten-way fan-out, produced no difference the experiment could
detect. [climb-condition.md](climb-condition.md) §7 records why — the CPU disjunct decided
every cut before the threshold was consulted — and says the sweep is worth re-running against
the loop GOV-32 left behind, because it would this time be measuring a live constant.

This is that re-run, plus the calibration it feeds.

**Headline: the threshold is load-bearing now, and moving it changes the outcome.** That is the
thing GOV-31 could not detect and it is no longer in doubt. **Second finding, which cost this
story its first answer: the headroom constant derived from GOV-31's concurrency curve is too
small, because that curve is under-converged — a fact GOV-31's own document states about
itself.**

---

## 1. Method

`tools/sweep -recovery 1`: one build held continuously, relaunched as it finishes, for a fixed
window, through the real gate binary at a real daemon. The daemon's threshold is set per arm
through `GOVERNOR_RUNQUEUE_MAX`, which still beats the calibrated value in config precisely so
that an experiment measures the number it set (`resolveRunQueueMax`, and the test that pins it).

Arms are **interleaved** — baseline, candidate, baseline, candidate — which is
[climb-condition.md](climb-condition.md) §4.4's method and the thing that caught a discarded run
there. Each arm times a **canary** build alone beforehand, as an independent check on whether
the machine itself drifted between arms.

### What is different about this host, and why the settle threshold was raised

`sweep` defaults to settling below **0.8** run queue per core before an arm starts. **This host
does not reach 0.8 while its owner is working.** A first attempt at this sweep sat in `settle`
until it timed out, at a run queue of 3.08 per core with nothing of the experiment running.

The threshold was raised to 3.2 rather than the machine being quieted into an unrepresentative
state. That is not a workaround for the experiment; **it is the finding the story is about** —
a developer machine idles far above where the Phase 0 trace's host idled, and a cut threshold
fitted to the latter floors the budget on ambient load alone.

### Confounds, stated

- **One host, one workload, one signal that cannot see short work.** Unchanged from GOV-31 and
  GOV-32, and nothing here improves on it.
- **`loadavg1` is a one-minute decaying average, so back-to-back arms contaminate each other.**
  The settle baselines below run 0.65 to 2.95 depending on what the previous arm left decaying.
  Cross-arm *marginals* are therefore unreliable. §3's result is deliberately a **within-arm**
  comparison, which this cannot touch.
- **The developer's own Claude Code session was on the box throughout.** The first sweep was
  additionally contaminated by this session running its own test suite; those numbers were
  discarded and the sweep re-run with the machine otherwise idle. The discarded run's arms are
  not in this document.

---

## 2. The threshold sweep

One build held, 75-second window, interleaved, on the machine otherwise idle. `2.0` is the
shipped constant; `3.42` is what `governor calibrate` derived on this host at the time
(ambient 1.77 + 1.65 headroom).

| rep | threshold | settled at | rq p50 | rq p90 | rq max | cut % | climb % | **budget over 75 ticks** |
|---|---|---|---|---|---|---|---|---|
| 1 | 2.00 | 0.65 | 2.59 | 2.78 | 2.92 | 75% | 19% | 6 → **1**, floored |
| 1 | 3.42 | 2.75 | 2.77 | 2.99 | 3.11 | **0%** | 73% | **6 on every tick** |
| 2 | 2.00 | 2.10 | 2.90 | 3.01 | 3.06 | 100% | 0% | **1**, floored |
| 2 | 3.42 | 2.62 | 2.96 | 3.34 | 3.46 | 7% | 48% | 6 → **1**, floored |

Canaries: 7.4 s, 7.0 s, 7.4 s, 7.8 s — the machine did not change speed across the sweep, so
the differences above belong to the threshold and not to the hour.

**The shipped threshold floors the budget with one build on this machine, in both replicates.**
That is the defect photographed, and it is what `calibrate` exists to fix.

**Raising the threshold changed the outcome, which is the result GOV-31 could not obtain.**
Under the old loop, five values spanning 1.2 to 6.0 were indistinguishable. Under this loop,
two values 1.42 apart give opposite budgets.

**But 3.42 held in only one replicate of two, and the failure is the more informative arm.**

---

## 3. What actually decides it: the peak, not the median

The four arms look inconsistent until they are sorted by the one quantity that is measured
*within* each arm, and is therefore immune to the baseline contamination in §1:

| arm | threshold | that arm's own rq **max** | margin | outcome |
|---|---|---|---|---|
| rep1 @ 2.00 | 2.00 | 2.92 | **−0.92** | floored |
| rep2 @ 2.00 | 2.00 | 3.06 | **−1.06** | floored |
| rep2 @ 3.42 | 3.42 | 3.46 | **−0.04** | floored |
| rep1 @ 3.42 | 3.42 | 3.11 | **+0.31** | **held at B_max** |

**One variable orders all four arms, and it is not the threshold's value.** It is whether the
threshold cleared the peak of that arm's run queue. Every arm whose threshold sat below its own
maximum floored; the single arm whose threshold sat above it held `B_max` for all 75 ticks.

### Why the peak and not the median, stated as mechanism

Because AIMD is **deliberately asymmetric**: it halves on a cut and climbs one slot per tick.
rep2 at 3.42 satisfied the cut condition on **7% of ticks** — a large minority of ticks
satisfied the *climb* condition, 48% — and still ended at the floor. A budget of 6 halves to 1
in three ticks and needs five clean ticks to climb back, so an excursion rate of one tick in
fourteen is sufficient to pin it down.

**This is the practical content of the calibration.** A threshold placed above a host's
*typical* run queue is not enough. It has to be placed above the excursions, because the
controller's response to a rare excursion is not rare.

---

## 4. The headroom constant was derived from an under-converged curve

`BuildHeadroom` was set to **1.65** by subtracting the Phase 0 trace's settled run queue
(p50 0.352) from the threshold GOV-31 measured against build load (2.0). The appeal of that
derivation is that a host idling where Phase 0's host idled calibrates back to exactly 2.0 — the
calibration explains the one value with evidence behind it rather than overturning it.

**It inherits an error, and [run-queue-sweep.md](run-queue-sweep.md) §4 Correction 2 states it
about its own numbers:**

> One build runs 8.1 seconds. `loadavg1` is a one-minute decaying average. It has 8 seconds to
> respond, so the 1.22 above is mostly the residue of the settle threshold (0.79) with a small
> rise on top […] **Any threshold on `loadavg1` is therefore a statement about sustained load,
> not about bursts.**

So the concurrency curve that 1.65 descends from measures **the leading edge of a build, not a
build**. A longer `-window` cannot fix it, because `-concurrency` runs each build **once** and
ends the window with the work — the load stops before the average arrives.

### What 1.65 does on a quiet machine, arithmetically

rep1 settled at **0.65** per core. A calibration taken at that moment writes
`0.65 + 1.65 = 2.30`. One sustained build on this host reaches **2.59 p50 and 2.92 max**.

**The cut lands below the load one build produces, so the budget floors and the climb threshold
at 1.80 is unreachable — the exact defect this story exists to remove, reintroduced by its own
constant.**

---

## 5. The concurrency curve, re-measured under sustained load

`-recovery N -thresholds 99`: N builds held continuously and relaunched for a **180-second**
window — three averaging windows, so `loadavg1` reaches its asymptote — with the cut placed far
out of reach so the governor admits everything and this measures the machine rather than the
controller.

| sustained builds | settled at | rq p50 | rq p90 | rq max | cpu p50 | canary |
|---|---|---|---|---|---|---|
| 1 | 2.73 | **3.04** | 3.81 | **4.07** | 0.84 | 7.1 s |
| 2 | 2.95 | **5.83** | 7.20 | **7.61** | 0.92 | 6.8 s |
| 3 | 2.89 | **10.06** | 11.04 | 11.98 | 0.99 | 7.0 s |

Canaries within 0.3 s of each other, so the machine held its speed across all three arms.

**The curve is superlinear** — marginals of +2.79 then +4.23 — which is queueing blowup as the
machine saturates, and CPU p50 tracks it at 0.84 → 0.92 → 0.99. **One sustained build reaches
3.04 at the median and 4.07 at the peak, against the 1.22 the under-converged curve recorded for
the same thing on the same host.**

A useful property of the 180-second window: the starting baseline decays out of `loadavg1`
entirely within it, so these figures are the asymptote of the new load rather than a sum of it
and whatever the previous arm left behind. That is what makes them comparable despite the
settle baselines differing.

---

## 6. What follows for the constant

**The interval is bounded on both sides, which is what makes the new value a derivation rather
than a preference:**

```
cut >  4.07    the PEAK at one sustained build  — §3: peaks decide, not medians
cut <= 5.83    the median at two builds         — so a second build still trips the cut
```

The midpoint of that interval is 4.95, and the host it was measured on idled at 0.65, so
**`BuildHeadroom` becomes 4.30**, replacing 1.65.

### Verified, at a threshold a live calibration actually produced

`governor calibrate` on this host read ambient **0.92** and wrote a cut of **5.22**. Both bounds
were then tested against it directly, 120-second windows:

| builds held | settled at | rq p50 | rq max | cut % | climb % | **budget over 120 ticks** |
|---|---|---|---|---|---|---|
| 1 | 0.74 | 1.48 | 1.94 | **0%** | 100% | **6 on every tick** |
| 2 | 1.80 | 7.85 | 9.45 | **70%** | 30% | 6 → **1**, floored |

**One build holds `B_max`; two builds trip the cut and the budget comes down.** That is the
lower bound (do not floor on a single build) and the upper bound (still govern a second one)
confirmed against a threshold nobody typed — the one the calibration derived from this machine.

### How precise this is, honestly

**Not to two decimal places.** The run queue produced by a *fixed* one-build load, measured four
times on this host with the same workload:

| arm | settled at | rq p50 | rq max | marginal p50 |
|---|---|---|---|---|
| 75 s sweep, rep 1 | 0.65 | 2.59 | 2.92 | +1.94 |
| 75 s sweep, rep 2 | 2.10 | 2.90 | 3.06 | +0.80 |
| 180 s curve | 2.73 | 3.04 | 4.07 | +0.31 |
| 120 s verification | 0.74 | 1.48 | 1.94 | +0.74 |

**p50 spans 1.48 to 3.04 and the peak spans 1.94 to 4.07 — a factor of two, for the same load on
the same machine.** So 4.30 should be read as "roughly twice the largest marginal peak yet
observed for one build", which is the safety factor that variance demands, and not as a number
known to a hundredth. What is robust across every arm is the *ordering* in §3, because it is a
within-arm comparison: the threshold either cleared that arm's own peak or it did not.

---

## 7. Limits, and what this hands to the next story

**`RunQueueBand` is narrower than the signal's own noise, and that is filed rather than fixed.**
The band is 0.5, while the spread between p50 and peak at one sustained build is about 1.0 —
twice the whole dead band. Combined with the asymmetry in §3, that is enough for the loop to hunt.
It is **GOV-B12**, deliberately not changed here: the band is GOV-32's constant, and retuning a
constant while changing the loop's shape in the same story is the move
[run-queue-sweep.md](run-queue-sweep.md) refuses. Raising the headroom so the cut clears one
build's peak by ~1.15 rather than sitting in the noise is what makes it a backlog item instead of
a blocker.

**The two-build operating point was not re-decided, because no throughput was measured.**
`budget.RunQueueBand`'s derivation assumes the loop settles at two concurrent builds, on the
grounds that the old curve put this machine's throughput optimum there. Holding two builds would
need the cut above their 7.61 peak — a headroom near 8.2. That was not done: the evidence for two
builds being optimal comes from the same under-converged curve, and `sweep -recovery` records no
completion counts, so **this story measured run queue and CPU but not work done**. Fixing the
measured defect is in scope; re-deciding the operating point on data nobody has collected is not.
CPU p50 at one, two and three builds is 0.84, 0.92, 0.99, which says the machine is not full at
one build — but "not full" is not "throughput is higher", and the difference is the whole
question.

**The compiled-in `budget.RunQueueMax` is left at 2.0.** Every host that runs `install` now
calibrates, so the constant is the fallback for a host where calibration could not run, and
leaving a fallback conservative is the right direction. A host that *is* calibrated never sees
it: with a headroom of 4.30 the floor in `deriveRunQueueMax` is unreachable, and it is kept as a
guard on the constant rather than on the reading.

**One host, one workload, one signal that cannot see short work.** `go build -a ./...` over
generated 9,000-function modules on an 8-core fanless laptop, with the developer's own Claude
Code session on the box throughout. An iOS UI suite, a large pytest run or an I/O-bound suite
would each sit differently, and the Phase 0 window under-represents all three. This is the same
limit GOV-31 and GOV-32 recorded and nothing here improves on it — what this story adds is that
the numbers are now at least *converged* for the workload they do describe.

**What would settle the remaining question:** a `-recovery` mode that records build completions,
so the concurrency curve carries throughput beside run queue. Then the operating point is a
measurement rather than an inheritance, and both this constant and `RunQueueBand` can be derived
from the same curve.

---

## 8. The probe (GOV-47)

Sections 1–7 calibrate the run-queue cut. This section is about the other number `calibrate`
writes, b_max, which comes from the load probe: effective cores, divided by 1.2 cores per slot
and floored. On this host, 4.8 to 6.0 effective cores is b_max 4. Below 4.8 is 3, and 6.0 or
more is 5.

### 8.1 The seven runs that opened the story

Seven `governor calibrate` runs on this MacBook Air (8 cores, 4 performance) on 2026-10-31 and
2026-11-01, same binary. The figures come from the story as filed, from config.json, and from
the calibrate output kept in two session transcripts of 2026-10-31. **"Not recorded" means none
of those sources holds the figure.** Nothing here was reconstructed. Run-queue cut is ambient + 4.30.

| run | loadavg1 | ambient rq / core | effective cores (max) | spread | reps usable | b_max | run-queue cut | source |
|---|---|---|---|---|---|---|---|---|
| A | not recorded | not recorded | probe failed | — | not recorded | **6** (core-count fallback) | not recorded | story |
| B | 5.79 | 0.72 | 5.37 | 0.84 | **3 of 5** | 4 | 5.02 | 2026-10-31 transcript |
| C | 16.65 | 2.08 | 5.74 | 0.37 | not recorded | 4 | 6.38 | story |
| D | 3.38 | 0.42 | 5.78 | 1.02 | 5 of 5 | 4 | 4.72 | config.json, 2026-11-01T19:34:07Z |
| E | 2.75 | 0.34 | 4.75 | 1.45 | not recorded | **3** | 4.64 | story |
| F | not recorded | 1.04 or 1.78 | 5.70 | 0.60 | not recorded | 4 | 5.34 or 6.08 | story |
| G | not recorded | 1.78 or 1.04 | 5.01 | 1.10 | not recorded | 4 | 6.08 or 5.34 | story |

The story lists the six successful runs' cuts (4.64, 4.72, 5.02, 5.34, 6.08, 6.38) and their
spreads, but it does not pair F and G with a cut. One correction to the story: it says every run
reported "5 of 5 repetitions usable". Run B's own output says 3 of 5. A further run on 2026-10-31
(loadavg1 4.11, ambient 0.51, max 5.39, spread 0.74, 4 of 5 usable, cut 4.81) is in the same
transcript but is not one of the seven.

These are summaries. Each run printed only its max and its range, and the repetitions behind
them were not kept. So the bound could not be set from these runs, and §8.2 re-measured instead.

### 8.2 Thirty probes with every repetition kept

`go run ./tools/probetrace -trials 30 -gap 20s`: the same ambient read and `Probe` call
calibrate makes, 20 s apart, recording every repetition. The machine was in ordinary use, with
loadavg1 between 2.1 and 27.4. Raw data: [`data/calibration/gov-47-probe-trials-max.json`](data/calibration/gov-47-probe-trials-max.json).

Every one of the 30 reported OK. These are the estimators that could be computed from the same
repetitions:

| estimator | range | sd | b_max over 30 probes |
|---|---|---|---|
| **maximum** (shipped) | 5.01 – **7.05** | 0.345 | 4 ×19, **5 ×11** |
| second-highest | 4.99 – 5.80 | **0.184** | **4 ×30** |
| median | 4.41 – 5.70 | 0.247 | 3 ×1, 4 ×29 |
| mean | 4.52 – 5.83 | 0.266 | 3 ×1, 4 ×29 |

**The maximum's premise is wrong.** probe.go argued that noise is one-sided: contention steals
from the parallel phase, so contaminated repetitions read low and none read high. The data says
otherwise. **The repetition that won the maximum had a median drift of 0.123. Every other
repetition had 0.011.** The winning repetition is systematically the one whose single-worker
baseline moved. A slow baseline shrinks the denominator and inflates the ratio, which is exactly
the case `oneRep`'s own comment warned about. The 25% drift gate is far too loose to stop it.

**Tightening the drift gate does not fix it.** At gates of 0.10, 0.05 and 0.03 the maximum still
reached 7.05 (trial 22). Both baselines of that repetition were slow together, so they agreed with
each other and the drift check, which compares them, could not see it. This is the story's
trap stated as a measurement: the bound has to be a different quantity from drift.

**What predicts a bad answer.** Error here means how far the second-highest landed from its
median across the 30 probes, 5.64. Correlation of each candidate with that error:

| candidate | correlation with error |
|---|---|
| full spread (max − min) / max | +0.12 |
| ambient run queue | −0.03 |
| top-two gap (max − second) / max | −0.30 |
| **gap between 2nd and 3rd, / 2nd** | **+0.61** |

The spread was the obvious candidate and it predicts almost nothing. It is dominated by the
lowest repetition, which the estimator already ignores. The gap between the two repetitions an
answer rests on does predict it. Over the 30 probes that gap was 5.2% or less on 29. The 30th was
11.6%, on trial 26, the probe whose answer was furthest off (4.99, the only one more than half a
slot from 5.64).

### 8.3 What was changed

- **Estimator: the second-highest usable repetition.** It keeps the upper envelope, which is
  what made the maximum resist contention. A single inflated repetition can no longer win.
- **Trust bound: the 2nd and 3rd highest must agree within 6%** (`ProbeMaxDisagreement`). The
  bound sits just above the body of §8.2's distribution and rejects the one answer in it that was wrong.
- **On a disagreement: measure once more, then decline** (`ProbeAttempts` = 2). Each attempt is
  announced.
- **A declined, skipped or failed probe derives nothing.** The stored b_max is kept, and the
  output and provenance say so. With nothing stored, b_max is written as 0 (uncalibrated), and
  the daemon derives its own ceiling. Before this, the probe fell back to the raw core count:
  run A's b_max 6, written as if measured.
- **The run-queue cut is resolved the same way, independently.** An unreadable load average
  used to write 0 over a good cut.

### 8.4 Validation, on probes the bound was not set from

The same tool after the change: 25 trials, 15 s apart. Raw data:
[`data/calibration/gov-47-probe-trials-validation.json`](data/calibration/gov-47-probe-trials-validation.json).

| | |
|---|---|
| probes | 25. Two were skipped by `ProbeSkipAbove`: something else on the machine pushed loadavg1 to 48.3 and 38.7, ambient 6.0 and 4.8 per core |
| accepted | **23 of 23** that ran. 21 passed on the first attempt and 2 on the retry. None declined |
| effective cores | **5.18 – 5.78**, sd 0.135, median 5.63 (§8.2's centre: 5.64) |
| b_max | **4 on all 23** |
| the maximum, over the same accepted attempts | 5.58 – 6.20. It would have written **b_max 5 on 4 of 23** |
| correlation with ambient run queue | −0.02, loadavg1 2.4 to 30.7 where the probe ran |

**The retry rate came in higher than §8.2 predicted**: 2 in 23 first attempts went over 6%, not
about 1 in 30. At that rate two disagreements in a row, and so a decline, would be roughly one
calibration in 130. A decline costs nothing but the update, because the stored value stands.

**One live escape, after validation.** The first real `governor calibrate --dry-run` with the
change in place reported **6.30** effective cores, b_max 5, with the two repetitions it rests on
3.0% apart. That is outside every one of the 53 recorded probes, whose highest was 5.80, and the
agreement check passed it. At least two repetitions read high together, and a check that
compares repetitions with each other cannot see that. calibrate printed only its summary, so the
repetitions are lost and the cause is unknown. Six probes recorded straight afterwards
([`data/calibration/gov-47-probe-trials-after-escape.json`](data/calibration/gov-47-probe-trials-after-escape.json))
read 5.51–5.69, normal. So the honest count is **one escape in 59 probes run**. The maximum
would have reported at least 6.30 on the same run, so this is a failure the change does not fix,
not one it causes. The third-highest repetition would be less exposed to it, and it was checked
on all the recorded data. It is noisier (sd 0.25 against 0.18 in §8.2) and gave b_max 3 once,
so it was not adopted. **`calibrate` now prints every repetition**, so the next escape can be
diagnosed rather than counted.

### 8.5 The story's remaining questions, answered from this data

**Does ambient load predict the probe? No.** Correlation is −0.05 in §8.2 and −0.02 in §8.4,
with loadavg1 up to 30.7 per probe. Of the seven original runs, the best (C) was taken at
loadavg1 16.65 and the worst (E) at 2.75. So `calibrate` gives no advice about load for the
probe's sake. Its closing advice to re-run mid-session is about the run-queue cut, whose baseline
*is* the ambient load. The output now says the advice applies to the cut only, and that an
untrusted probe cannot make b_max worse.

**Should the two outputs be measured under the same conditions?** They want different things,
and it turns out not to matter. The cut needs representative working load. The probe, up to
`ProbeSkipAbove`, is indifferent to load. So taking both mid-work in one command is right for
both, and the command stays one command. What it does about the separation is resolve each
output on its own measurement. A failed probe no longer touches the cut, an unreadable load
average no longer touches b_max, and `--no-probe` now re-takes the cut alone without disturbing
a measured b_max. The cut tracking ambient load across the seven runs (4.64 → 6.38) is the cut
working as designed, not a defect.

**Thermal history.** The story offered that run E, minutes after loadavg1 16.65 on a fanless
chassis, read low because the machine was warm. Under the new estimator this is moot. §8.2's
trials 12–14 and §8.4's trials 20–24 were taken as the machine came down from loadavg1 27–31,
and they answered 5.40–5.73, inside the normal range. Whatever depressed run E, the second-highest
with the agreement check is not affected by it here.

**"About 6.1 of 8."** probe.go documented this for this hardware. It came from the maximum, and
the second-highest centres on 5.6. probe.go and design.md §5.2a now say 5.6.

### 8.6 Limits

One host, 59 probes, one afternoon and evening. The 6% bound comes from this machine's
distribution. A machine whose repetitions are noisier will retry and decline more, which keeps
whatever an earlier calibration measured. That is the intended failure direction, but nobody has
measured how often it happens elsewhere.

**A run whose repetitions are wrong together still gets through**, as §8.4's escape did. The
agreement check bounds how much the probe can disagree with itself, not how wrong it can be.
Re-running calibrate gives a fresh draw. Nothing yet compares a new answer against the stored
one, so a single run can still move b_max by a slot. Whether calibrate should refuse a move
that a second probe does not confirm is the obvious next question, and it is not answered here.
