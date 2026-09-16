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
