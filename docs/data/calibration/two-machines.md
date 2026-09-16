# The same binary, five machines — GOV-25's last Done-when clause

Raw output of `governor calibrate --dry-run`. The clause is that two different machines produce
materially different values; this is that, measured rather than argued, across one macOS host
and four Linux containers on it at different CPU quotas.

The container arms are `docker run --cpus=N debian:bookworm-slim` running the `linux/arm64`
binary from `make cross`, with `CLAUDE_PLUGIN_DATA=/tmp/gov`. Taken 2026-09-16, after
`BuildHeadroom` was corrected to 4.30.

| machine | cores seen | capacity (source) | perf cores | ambient rq/core | probe: effective cores (usable reps, spread) | **run-queue cut** | **b_max** |
|---|---|---|---|---|---|---|---|
| macOS host | 8 | 8.00 (host) | **4 of 8** | 0.71 | 5.57 (5 of 5, spread 3.92) | **5.01** | **4** |
| container `--cpus=1` | 8 | 1.00 (cgroup) | not distinguishable | 0.22 | **1.00** (4 of 5, spread **0.00**) | 4.52 | **1** |
| container `--cpus=2` | 8 | 2.00 (cgroup) | not distinguishable | 0.18 | **1.96** (4 of 5, spread 0.47) | 4.48 | **1** |
| container `--cpus=4` | 8 | 4.00 (cgroup) | not distinguishable | 0.12 | **3.84** (5 of 5, spread 0.54) | 4.42 | **3** |
| container `--cpus=8` | 8 | 8.00 (cgroup) | not distinguishable | 0.08 | **5.91** (5 of 5, spread 0.98) | 4.38 | **4** |

## What this shows beyond "the numbers differ"

**The probe measures the CPU quota without being told it.** Every container sees eight cores
through `runtime.NumCPU()`. From throughput alone the probe recovers **1.00, 1.96 and 3.84** for
quotas of 1, 2 and 4 — a quantity it has no access to except through how the machine behaves,
and at `--cpus=1` with a spread of exactly zero across four usable repetitions. That is the
strongest available evidence that it is measuring a machine rather than reciting a constant.

**The unrestricted container independently reproduces the efficiency-core finding.** At
`--cpus=8` there is no quota to find, and the probe reports **5.91 of 8** — not 8. That is the
same answer the native macOS runs give (5.57 here, and 5.73, 5.80, 6.1 across the day), from a
different operating system, a different binary, and a code path that cannot read
`hw.perflevel0.*`. Two instruments sharing no mechanism agree that this hardware delivers about
six cores of parallel work from eight advertised ones.

**`b_max` differs materially across the five: 4, 1, 1, 3, 4.** That is the value the ceiling is
actually set from, and it tracks the machine rather than the core count every arm reports.

**The cut threshold differs materially where the story says it should, and not where it should
not.** Between these five it spans 4.38 to 5.01, because all four containers are idle and idle
machines *ought* to calibrate alike — that is the constant doing its job, not a failure to
discriminate. The comparison the story is about is a working machine against a quiet one, and
there it moves well beyond the loop's own 0.5 dead band:

| host state | ambient rq/core | cut |
|---|---|---|
| idle container | 0.08 | 4.38 |
| this laptop, quiet | 0.71 | 5.01 |
| this laptop, working | 1.77 | 6.07 |
| this laptop mid-work (climb-condition.md §4.3) | 2.91 | 7.21 |

**Performance cores are reported as not distinguishable on Linux rather than as 8 of 8.** There
is no `hw.perflevel` equivalent, and the honest answer is that the platform does not draw the
distinction — a different fact from a homogeneous machine, even though both print the same core
count.

## One reading worth not glossing

The macOS arm's spread is **3.92**, far wider than any container's, because that host was doing
other work while it ran. The maximum-over-repetitions estimator still landed at 5.57, within the
range the quieter native runs give, which is the property it was chosen for — but a wide spread
is the signal that the number was taken on a busy machine, and it is reported for that reason
rather than hidden. `docs/calibration.md` §6 has the reproducibility limits in full.
