# Phase 0 findings: is agent count the right thing to govern?

**It is not. The recommendation is to pivot the control variable — from agent count to build
and test concurrency — keeping the mechanism.**

The reason is not that agents are negligible. It is that **agent cost and build cost have
different distribution shapes, and a fixed agent budget caps the one that never spikes.**

| | Agent cost (root cores) | Build cost (child cores) |
|---|---:|---:|
| median | **+0.098** | **0.000** |
| p95 | +0.170 | 0.149 |
| p99 | +0.214 | **3.318** |
| busiest single second | 1.723 (absolute) | **7.822** |

At the median an agent costs **more** than a build, because builds are rare — 91% of sampled
seconds have no child CPU at all. At p99 a build costs **15.5×** what an agent does. The
busiest child second in four days is **4.5×** the busiest root second.

A budget of 4 agents caps a quantity whose p99 is a fifth of a core. The quantity that
reaches 7.8 cores on an 8-core machine is ungoverned, and it arrives through `Bash`, which
the gate does not match.

This document is written method-first, and states its own limits in the same voice as its
results, because the conclusion is strong enough to be worth distrusting.

---

## 1. The window, and what is wrong with it

`~/.claude/governor/samples.jsonl`, written by the Phase 0 hooks in `~/.claude/settings.json`.
All figures below are pinned to a cutoff of **2026-09-12T23:00Z**; the log keeps growing, so
re-running later will move the last digits.

| | |
|---|---|
| Span | 2026-09-08T17:26:58Z → 2026-09-12T22:45:57Z (**101.3 hours**) |
| Samples | 252,560 host readings at 1 Hz — **69.2% coverage** of the span |
| Agent records | 632 (39 `SubagentStart`, 88 `PreToolUse`, 36 `PostToolUse`) |
| Reconstructable agent lifetimes | **34** |
| Agent-live wall time | **1.20 h — 1.2% of the window** |

**Concurrency: the machine was never quiet and never single-tenant.** The number of live
sessions never fell below 3, and was 5 or 6 for **98.9%** of samples. There is no moment in
four days when one project had the machine to itself.

**Composition, by agent record** (scratchpad paths attributed to the project they encode):

| Project | Records | Share |
|---|---:|---:|
| claude-governor | 304 | 48.1% |
| nightcrew | 178 | 28.2% |
| edgefinder-v2 | 87 | 13.8% |
| concertFinder | 26 | 4.1% |
| muncheez | 13 | 2.1% |
| budgetr | 10 | 1.6% |
| launch-control | 8 | 1.3% |
| unattributed | 6 | 0.9% |

Nearly half the window is the machine building the governor; the two ship-priority projects
are 3.7% between them.

### Which path was taken, and why

Notes and traps offered two honest paths. **Path (a) — get a real muncheez or budgetr working
day into the window — was unavailable, not skipped.** Those projects have no agent-doable
stories left; their remaining work is human-only, so no session runs there and waiting
produces no records from them.

**Path (b) was taken.** Two measurements close off the alternative of waiting longer:

- **Waiting adds nothing.** `SubagentStart` by day: 09-08: 9, 09-09: 24, 09-10: 6,
  **09-11: 0, 09-12: 0.** The 34 reconstructable lifetimes are the same 34 counted on 09-10.
  Two further days produced none, because the backlog that would generate them is gated
  behind GOV-11's dogfooding week.
- **Even a muncheez day would not have been attributable.** The sampler is host-wide and the
  machine never ran fewer than three concurrent sessions, so no sample can be assigned to one
  project. Path (a) assumed an attributability this instrument does not have.

### The bias runs toward the conservative side

The window is weighted toward Go, whose builds are fast, and away from the iOS UI suite and
the pytest suites that would be heavier. **That understates the finding rather than
flattering it.** The conclusion is that build work dominates the tail; a window containing
heavier builds would push that tail further out, not pull it in. It is still a reason to
re-measure once the heavy mode is represented, which is named as a follow-up.

---

## 2. Method

Two instruments, deliberately different.

**The window (retrospective).** `internal/signals.Sampler` reports two numbers per tick:
`claude_cpu`, cores busy across every Claude Code session process *and its transitive
descendants*, and `claude_cpu_roots`, cores busy in the session processes alone.

**This split is what makes the comparison like-for-like.** A subagent executes *inside* the
session process, so its cost appears in **root** cores. A build or test run is spawned *as a
child*, so its cost appears in **child** cores. Agent cost is therefore a root-cores delta
between agent-live and baseline samples; build cost is child cores directly. The two are
measured by the same instrument in the same seconds.

**The decomposition (live).** `tools/childcpu` buckets the same trees the sampler sums and
reports cores by bucket and by process name. It mirrors the sampler deliberately — same
`argv[0]` root rule, same transitive walk, same `cpuDelta` physical bound that keeps a
compiler visible for one tick from scoring zero — because a decomposition computed by a
different rule would not add up to the number it decomposes. It excludes its own subtree.

It was run twice: **Phase A**, three minutes ambient with no deliberate load; **Phase B**,
150 seconds spanning a cold-cache `go build ./...` and the full `go test ./...` suite.

---

## 3. What a subagent costs (amended clause 1)

**Per-agent attribution is impossible with this instrument, and no per-agent number is
reported here.** The sampler is host-wide and the machine ran 5–6 concurrent sessions
throughout. What can be measured is a population marginal: root cores while at least one
reconstructable lifetime is open (n=2,367 samples), against root cores while none is
(n=250,193).

| | Agent live | Baseline | Marginal |
|---|---:|---:|---:|
| median | 0.189 | 0.091 | **+0.098** |
| p95 | 0.388 | 0.218 | +0.170 |
| p99 | 0.568 | 0.354 | +0.214 |
| max | 1.477 | 1.723 | **−0.246** |

**A live subagent adds about a tenth of a core at the median, and about a fifth at p99.**

**The negative figure at max is real and is reported rather than hidden:** the busiest root
seconds in the whole window occurred with *no* agent live. The agent marginal does not
survive into the extreme tail at all — there is no meaningful "peak agent cost" to quote.
That is itself part of the finding: agent cost is small and near-constant, not spiky.

This is an **upper bound** on the agent's own cost, since a session that has spawned an agent
is also working itself, inside the same measurement. No p95 or p99 of *per-agent* cost is
reported — n=34 lifetimes does not support one, and the story's notes forbade quietly
computing one off this n. (The percentiles above are over 2,367 samples of host state, which
is a different and much larger population than the 34 lifetimes.)

It agrees with GOV-02's independent result, which moved CPU from 0.18 to 0.29 cores with two
parallel `Explore` agents. Two methods, same answer: **agents are I/O-bound and cheap.**

## 4. How long a subagent holds a slot (amended clause 2)

The idle-versus-active split asked for originally is **not reported, because the trace records
no field that separates the two.** What a fixed budget rations is occupancy, and occupancy is
measurable.

Over the 34 reconstructable lifetimes: **median 89.1 s**, min 1.4 s, max 716.2 s (11.9 min),
total 1.20 h — **1.2% of the window**.

## 5. Where the CPU actually goes (clause 3 — the one the pivot turns on)

Over the whole window, of 52,138 core-seconds of claude-tree CPU:

| | Core-seconds | Share |
|---|---:|---:|
| Session processes (roots) | 27,385 | 52.5% |
| **Children** | **24,753** | **47.5%** |

Nearly half — and **bursty, not ambient**:

| Child cores | |
|---|---:|
| median | 0.000 |
| p95 | 0.149 |
| p99 | 3.318 |
| p99.9 | 7.693 |
| max | **7.822** |

Only **9.1% of ticks** show any child CPU at all. Almost half of all Claude-attributable CPU
on this machine is delivered in 9% of the seconds.

Undecomposed, that 47.5% supports two opposite products. The live measurement decomposes it.

**Phase A — ambient, 3 minutes, no deliberate load:**

| | Core-sec | Share of tree |
|---|---:|---:|
| Session roots | 32.55 | 97.6% |
| Descendants | 0.80 | **2.4%** |

**Phase B — 150 seconds spanning a cold build and the full test suite:**

| | Core-sec | Share of tree |
|---|---:|---:|
| Session roots | 28.00 | 60.3% |
| Descendants | 18.46 | **39.7%** |

…and those descendants decompose as:

| Bucket | Core-sec | Share of descendants |
|---|---:|---:|
| **build-test** | 17.23 | **93.4%** |
| shell | 0.80 | 4.3% |
| other | 0.42 | 2.3% |
| **mcp** | **0.00** | **0.0%** |

By process name: `go` 7.36, `compile` 5.98, `governor.test` 1.24, `link` 1.23, `clang` 0.69,
`signals.test` 0.41, `dsymutil` 0.17, `cgo` 0.02.

**The children are compilers and test binaries.** Ambient they are 2.4% of the tree; during a
build, 39.7%, and 93.4% of that is the toolchain.

### MCP servers are not in the tree at all

The hypothesis that the 47.5% might be attached MCP servers — which would have meant the
sampler had been counting them as agent cost since GOV-02 — **is false, and structurally so
rather than incidentally.** Neither MCP server on this machine is a descendant of a session
process:

- **claude-in-chrome** runs as `--chrome-native-host`, a child of **Google Chrome** (pid 708).
  Its `argv[0]` is the version path, so it is not even counted as a root.
- **Notion** is a remote HTTP connector. It runs no local process at all.

Zero MCP core-seconds were measured inside the trees in either phase.

**This cuts both ways, and the second way is a blind spot that is now a known limit of the
instrument rather than a discovery waiting to happen.** The sampler has not been inflating
agent cost with MCP servers — but it is **blind to them**. A stdio MCP server launched by the
session *would* be a descendant and *would* be counted; one attached over HTTP or through a
native host is invisible to `claude_cpu` while still burning real CPU on the machine. Any
future per-agent cost attribution (GOV-B03) inherits this: it would count the builds an agent
launched, which is arguably correct, and silently omit whatever its MCP servers cost, which
is not.

## 6. Thermal (clause 4)

| State | Samples | Share |
|---|---:|---:|
| nominal | 243,751 | 96.51% |
| fair | 8,809 | 3.49% |
| serious / critical | 0 | 0% |

**Thermal state left nominal exactly once in 101 hours**, for 150.4 minutes, and never went
past `fair`. It never reached a level any cut rule would have acted on.

---

## 7. Recommendation: pivot the control variable, keep the mechanism

### The problem was correctly felt and wrongly attributed

This project began with an empirical complaint: sessions degrading under load on a machine
running five or six of them. **Nothing here says that was imagined.** The machine does get
hammered, almost half of Claude-attributable CPU is delivered in 9% of the seconds, and the
busiest of those seconds take 7.8 of 8 cores.

What the measurement changes is the *cause*, not the symptom. The load is builds and test
runs — many of them launched by agents, which is why limiting agents felt like it should
help. It is a re-aim of a working instrument, **not a retraction**. Almost everything built so
far is variable-agnostic and survives unchanged: the daemon, the lease table and its reaper,
the fail-open gate, the transport and budget seams, config and protocol versioning,
install/uninstall, doctor, the statusline. What changes is **what takes a lease**.

### Why agent count is the wrong variable

Not because agents are free — at the median an agent costs *more* than a build. Because the
two distributions have different shapes:

- **Agent cost is small and near-constant.** Median +0.098 cores, p99 +0.214, and no positive
  marginal at all in the extreme tail. A budget of 4 caps roughly 0.4–0.9 of a core.
- **Build cost is zero almost always and enormous occasionally.** Median 0.000, p99 3.318,
  max 7.822 — and nothing caps it.

Rationing the first while leaving the second open cannot control machine load, and no
setpoint fixes that. **GOV-15 would have been tuning a constant that governs the wrong
quantity.**

### The hard part is the predicate, not the plumbing

The gate already runs on `PreToolUse`; pointing it at `Bash` is a matcher change and is the
easy half. **Deciding which `Bash` is build-shaped is the whole problem.** GOV-28 recorded
15,000+ `Bash` calls across those transcripts. Leasing per call is obviously wrong — most are
`cat`, `grep`, `git status` — and a wrong predicate is worse than none: it either gates
everything, making the tool unusable, or gates nothing, making it decorative.

Whatever replaces GOV-14 and GOV-15 must carry **classification as its central problem**, held
to the same measure-don't-assume discipline that produced this finding: classify from observed
commands and their measured cost, not from a hand-written list of binary names that looks
plausible. This is not an implementation detail of a matcher.

### Gating `Bash` makes the gate a real hot path, and that re-prices the fail-open work

Counted from this machine's transcripts: **18,929 `Bash` calls against 86 gate-matched calls**
(`Agent`, `Task`, `SendMessage`) — a ratio of about **220×**. Per day the gate fires between 0
and 37 times; `Bash` fires between 184 and 2,221 times.

> **Correction, 2026-09-12.** This paragraph first read "the gate runs 88 times in 101 hours,
> measured from the Phase 0 `PreToolUse` records". That was wrong. `PreToolUse` is **not** one
> of the registered trace hooks — the five in `settings.json` are `SubagentStart`,
> `SubagentStop`, `Stop`, `PostToolUse` and `SessionStart` — so those 88 records came from a
> temporary registration on 2026-09-09 alone, not from continuous instrumentation. The figure
> both misdescribed its source and understated the rate. The numbers above are counted from
> the transcripts instead, which is where the call actually appears.

Every latency assumption in the current design was set for a rare event and must be
re-derived:

- the **p99 under 15 ms** budget was chosen for something that happens a few dozen times a
  week, not a few thousand times a day;
- the **100 ms connect deadline** stops being a rare worst case and becomes a tax paid
  continuously;
- a wedged or slow daemon goes from an occasional stall to a **per-command tax on every shell
  invocation the user makes**.

**GOV-05's fail-open invariant becomes more load-bearing, not less.** It is currently the
cheapest insurance in the repo; under this pivot it becomes the thing standing between a
daemon hiccup and an unusable terminal. This is stated here so it is designed for rather than
discovered.

### What this means for the backlog

Re-scoped (see the stories themselves): **GOV-14** (AIMD on agent count), **GOV-15**
(setpoint), **GOV-16** and **GOV-17** (`request_agents` and teaching sessions to ask),
**GOV-18** (fair share over agent slots), **GOV-B03** (per-agent cost).

Untouched, because they are variable-agnostic: the daemon and lease work, fail-open,
transport and budget seams, config and protocol versioning, install/uninstall, doctor,
statusline.

### Confidence, and what would change the answer

The comparison rests on 252,560 samples for the distribution shapes and on a direct live
decomposition for what the children are. The weakest input is n=34 agent lifetimes, which is
why no per-agent percentile is quoted and why the agent side is reported as a population
marginal with its own tail anomaly shown rather than smoothed.

What would genuinely change the conclusion: a workload where agents are CPU-bound rather than
I/O-bound, or one where builds are rare and agent fan-out is wide and sustained. Neither
describes this machine. **The named follow-up is to re-measure once the heavy-build mode (iOS
UI suite, pytest) is represented** — which, on the reasoning in §1, should push the build tail
further out.

---

## 8. Reproducing this

```sh
go build -o /tmp/childcpu ./tools/childcpu

# ambient
/tmp/childcpu -for 3m -tick 1s -label "ambient"

# during a build
/tmp/childcpu -for 150s -tick 1s -label "burst" &
go clean -cache && go build ./... && go test ./...
```

The window figures come from `~/.claude/governor/samples.jsonl` directly, pinned to a cutoff
of 2026-09-12T23:00Z. Every number in §§1–6 is computed from that file or from
`tools/childcpu` output, and none is carried over from an earlier document.
