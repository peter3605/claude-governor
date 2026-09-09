# Hook surface — measured evidence

**What this is.** Raw evidence for the four unknowns in [design.md §10](design.md). Nothing
here is inferred from documentation. Every payload below is the verbatim bytes a hook
received on stdin, captured by a throwaway hook that did `cat > file` and exited 0.

Read this before changing §10. If a future Claude Code version moves the surface, re-run the
method below and replace this file wholesale rather than editing conclusions into it.

---

## 0. Provenance

| | |
|---|---|
| `claude --version` | `2.1.263 (Claude Code)` |
| Binary | `/Users/peterho/.local/bin/claude` → `/Users/peterho/.local/share/claude/versions/2.1.263` |
| Host | macOS (Darwin 25.4.0), arm64 |
| Date captured | 2026-09-07 |
| Story | GOV-01 |

### Method

Six probes. Each ran a **separate headless `claude -p`** in a throwaway scratch directory
with `--settings <probe>/settings.json --setting-sources ""`, so that nothing was written to
`~/.claude/settings.json` and no ambient user/project hook could contaminate the result. The
hook body in every probe was:

```bash
#!/bin/bash
D="$(cd "$(dirname "$0")" && pwd)/out"
LABEL="$1"
TS=$(python3 -c 'import time;print("%.6f"%time.time())')
F="$D/${TS}-${LABEL}-$$.json"
cat > "$F"                 # stdin, verbatim, unmodified
echo "$TS $LABEL pid=$$ file=$(basename "$F")" >> "$D/index.log"
exit 0
```

| Probe | Question | Shape |
|---|---|---|
| 1 | tool name, `tool_input`, event coverage | one subagent; `PreToolUse` registered three times — catch-all, `matcher:"Task"`, `matcher:"Agent"` |
| 2 | ordering under fan-out | four subagents, model chose `run_in_background` |
| 3 | timeout ceiling | one subagent; three sleeping hooks with `timeout` unset / `120` / `900` |
| 4 | FIFO correlation under a real burst | four subagents forced into one assistant message, foreground |
| 5 | spawn depth | subagent instructed to spawn a nested subagent |
| 6 | is a late decision honoured | hook sleeps then emits `deny` at 20 s and at 75 s |

The **control** that makes probe 1 trustworthy: the `matcher:"Task"` and `matcher:"Agent"`
hooks fired for the subagent spawn but did **not** fire for `Bash`, `ListAgents`,
`ToolSearch` or `SendMessage` in the same session. Matchers are therefore genuinely being
applied, not ignored.

---

## 1. `PreToolUse` on a subagent spawn — depth 0

Captured by all three registrations simultaneously (catch-all, `matcher:"Task"`,
`matcher:"Agent"`), byte-identical in each. This is the `matcher:"Task"` copy:

```json
{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e","transcript_path":"/Users/peterho/.claude/projects/-private-tmp-claude-501--Users-peterho-Repos-claude-governor-223d86ac-b4c0-4829-854b-e917795e9601-scratchpad-probe-work/2ed4bf1c-470b-4e08-9b48-4f310663cb6e.jsonl","cwd":"/private/tmp/claude-501/-Users-peterho-Repos-claude-governor/223d86ac-b4c0-4829-854b-e917795e9601/scratchpad/probe/work","prompt_id":"77e4b2be-6d77-422e-9f12-a613e4c7138b","permission_mode":"bypassPermissions","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"description":"Count lines in README.md","prompt":"In the current working directory, count the number of lines in the file README.md (e.g., using `wc -l README.md`). Report back just the line count number.","subagent_type":"general-purpose","run_in_background":false},"tool_use_id":"toolu_013XAeD4MymdfZHskv22UtK3"}
```

**`tool_name` is `Agent`. A matcher of `Task` matched it anyway** — `Task` is carried as an
alias for the same tool, so both spellings fire. The two names coexist inside one version:
the same run's `permission_denials` array (probe 6) reported the *same* tool as `Task`:

```json
{"tool_name":"Task","tool_use_id":"toolu_01MkQkUncWN8A5nDcEr56U5r","tool_input":{"description":"Count lines in README.md","prompt":"Count the number of lines in the file README.md located in the current working directory. Use a shell command like `wc -l README.md` or read the file directly. Report back just the line count.","subagent_type":"general-purpose","run_in_background":false}}
```

## 2. `PreToolUse` on a subagent spawn — nested (depth 1)

From probe 5, where a subagent spawned a further subagent. Note the top-level `agent_id` /
`agent_type`, and note that `subagent_type` is **absent** from `tool_input` here:

```json
{"session_id":"1206ebc4-102c-4716-aa5f-4e7bc4371735","transcript_path":"/Users/peterho/.claude/projects/-private-tmp-claude-501--Users-peterho-Repos-claude-governor-223d86ac-b4c0-4829-854b-e917795e9601-scratchpad-probe5-nested-work/1206ebc4-102c-4716-aa5f-4e7bc4371735.jsonl","cwd":"/private/tmp/claude-501/-Users-peterho-Repos-claude-governor/223d86ac-b4c0-4829-854b-e917795e9601/scratchpad/probe5-nested/work","prompt_id":"b01c6d87-91b2-4b8d-9ac7-3f294188364e","permission_mode":"bypassPermissions","agent_id":"a0dca3e4fa79a2f6f","agent_type":"general-purpose","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"description":"Count lines in README.md","prompt":"In the working directory /private/tmp/.../probe5-nested/work there is a file named README.md. Count the exact number of lines in this file (e.g. using `wc -l` or by reading the file and counting). Report back only the exact line count number and the method you used.","run_in_background":false},"tool_use_id":"toolu_01FPehV8CCXrhtoDkPRpKfzn"}
```

A `PreToolUse` for an ordinary tool *inside* a subagent carries the same marker — here `Bash`
running inside the depth-0 subagent of probe 1:

```json
{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e","transcript_path":"...","cwd":"/private/tmp/.../probe/work","prompt_id":"77e4b2be-6d77-422e-9f12-a613e4c7138b","permission_mode":"bypassPermissions","agent_id":"aca0c17642a96d305","agent_type":"general-purpose","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"wc -l README.md"},"tool_use_id":"toolu_01FPXhU7JWNpZ5ErhMHHLM3d"}
```

**Therefore: `agent_id` absent at top level ⇒ the caller is the main session (depth 0);
`agent_id` present ⇒ the caller is itself a subagent (depth ≥ 1), and the value names the
*parent*, not the agent about to be created.** There is no `depth` field. This is the only
depth signal on the surface, and it is sufficient for §6.3.

## 3. `SubagentStart`

```json
{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e","transcript_path":"/Users/peterho/.claude/projects/-private-tmp-claude-501--Users-peterho-Repos-claude-governor-223d86ac-b4c0-4829-854b-e917795e9601-scratchpad-probe-work/2ed4bf1c-470b-4e08-9b48-4f310663cb6e.jsonl","cwd":"/private/tmp/claude-501/-Users-peterho-Repos-claude-governor/223d86ac-b4c0-4829-854b-e917795e9601/scratchpad/probe/work","prompt_id":"77e4b2be-6d77-422e-9f12-a613e4c7138b","agent_id":"aca0c17642a96d305","agent_type":"general-purpose","hook_event_name":"SubagentStart"}
```

Six fields, and **no `tool_use_id`**. Here `agent_id` is the *new* agent (contrast §2, where
on `PreToolUse` it is the parent). There is no field in common with `PreToolUse` that
identifies the specific spawn — only `session_id` and `prompt_id`, which are shared by every
spawn in the turn.

## 4. `SubagentStop`

```json
{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e","transcript_path":"...","cwd":"/private/tmp/.../probe/work","prompt_id":"77e4b2be-6d77-422e-9f12-a613e4c7138b","permission_mode":"bypassPermissions","agent_id":"aca0c17642a96d305","agent_type":"general-purpose","effort":{"level":"high"},"hook_event_name":"SubagentStop","stop_hook_active":false,"agent_transcript_path":"/Users/peterho/.claude/projects/.../subagents/agent-aca0c17642a96d305.jsonl","last_assistant_message":"3","background_tasks":[],"session_crons":[]}
```

Carries `agent_id`, so release keys off `agent_id` directly and needs no correlation.

**Addendum, GOV-02 (2026-09-08), from live traces rather than probes.** `agent_type` is
present as a *key* on every `SubagentStop` observed on 2.1.263, but its **value is sometimes
empty**. Three of eleven records captured across five concurrent real sessions carried
`"agent_type"` in the payload with nothing in it, while `agent_id`, `session_id`, `prompt_id`
and `cwd` were all populated.

This is a third state that §10.1 did not name, and it is worth naming because it is easy to
misdiagnose: the earlier finding was that `tool_input.subagent_type` can be *absent*, and an
absent key and an empty value are not the same observation. Consumers must distinguish

    key absent      → the harness sent no such field
    key, no value   → the harness sent the field, empty
    key with value  → the ordinary case

which is why the Phase 0 agent record carries the payload's top-level key names alongside the
decoded fields. Anything keying a lease or a policy on `agent_type` must treat empty as
"unknown type" and not as a type named "".

## 5. `SessionStart` / `SessionEnd`

```json
{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e","transcript_path":"...","cwd":"/private/tmp/.../probe/work","hook_event_name":"SessionStart","source":"startup"}
```
```json
{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e","transcript_path":"...","cwd":"/private/tmp/.../probe/work","prompt_id":"defa4b42-3443-40b3-9869-9d5b8462b44b","hook_event_name":"SessionEnd","reason":"other"}
```

Neither carries a PID. `SessionStart` has no `prompt_id`. The daemon must learn the session's
owning PID some other way — from the hook process's own ancestry, not the payload.

**Refinement, GOV-07 (2026-09-08).** "The hook process's own parent" is too strong, and
building on it would have been a silent budget bug. Claude Code may run a hook through a
shell, and that shell exits the instant the hook does; a lease bound to it is reported dead
by `PIDLiveness` on the next reaper tick, two seconds later, so every confirmed lease on the
machine would be collected while its agent was still running. Nothing errors — the governor
just stops governing.

The owner is therefore found by **walking up** the ancestry to the first process whose
`argv[0]` is `claude`, with three rules: stop before pid 1 (init never exits, so a lease
owned by it can never be reaped for liveness, and on Linux an orphaned hook really is
reparented there); give up after a bounded number of hops; and report **unknown** rather
than substituting the parent, because an unknown owner merely waits out its TTL while a
wrong one is reaped out from under a live agent.

The `argv[0]` rule is the same one recorded in §12 and in `design.md` §5.1, and it is tested
the way it actually occurs: the test executes a file named `2.1.263` with `argv[0]` set to
`claude`. A test using a file *named* `claude` passes whether the code reads `argv[0]` or the
executable name, which is exactly how GOV-02 shipped a sampler that found zero sessions on a
machine running six.

## 6. `PostToolUse` on the `Agent` tool — the ground truth for correlation

`tool_response` binds `tool_use_id` to `agentId`. It arrives when the agent **finishes**
(probe 1: `SubagentStop` at t=716.234, this at t=716.300), so it is useless for confirming a
lease at spawn time — but it is exactly what is needed to *verify* a correlation scheme
offline, which is how §7 below was checked. Abridged to the correlating fields:

```json
{"hook_event_name":"PostToolUse","tool_name":"Agent","tool_use_id":"toolu_013XAeD4MymdfZHskv22UtK3","tool_response":{"status":"completed","agentId":"aca0c17642a96d305","agentType":"general-purpose","totalDurationMs":3989,"totalTokens":17932,"totalToolUseCount":1,"toolStats":{"readCount":0,"searchCount":0,"bashCount":1,"editFileCount":0,"linesAdded":0,"linesRemoved":0,"otherToolCount":0}},"duration_ms":3992}
```

For a background spawn the status is `async_launched` instead of `completed`, and it arrives
immediately rather than at completion (probe 2).

---

## 7. Ordering — is per-session FIFO correlation sound?

Probe 4 forced four `Agent` calls into **one** assistant message, foreground
(`subagent_stats`: `{"spawned":4,"requested":{"foreground":4},"max_depth":1}`). Observed
order, from `index.log` plus the payloads:

```
t=860.514  PreToolUse     toolu_01JbkAYyeNvj3GnYTk9MrDZK   (alpha)
t=860.563  SubagentStart                    agent a831043fbd5fdd318
t=861.364  PreToolUse     toolu_012rtGsac94Fa7U9ssAbQVgm   (bravo)
t=861.424  SubagentStart                    agent aabf925d6d594b811
t=862.414  PreToolUse     toolu_0182sdjXxDLABDhXry7TK9SX   (charlie)
t=862.449  SubagentStart                    agent adcc8ee2b9293f58a
t=863.090  SubagentStop                     agent a831043fbd5fdd318
t=863.147  PostToolUse    toolu_01JbkAYyeNvj3GnYTk9MrDZK → a831043fbd5fdd318
t=863.235  PreToolUse     toolu_01CT5HSwH6t7zWK5xhZaLS8k   (delta)
t=863.270  SubagentStart                    agent a7b9a4734d0f0d0a4
```

Two facts, both load-bearing:

1. **`PreToolUse` and `SubagentStart` strictly interleave.** Even with four tool calls in a
   single message, the spawns are serialised ~0.9 s apart and each `SubagentStart` lands
   ~35–60 ms after its own `PreToolUse`, always before the next `PreToolUse`. At no point
   were two spawns in flight simultaneously.
2. **FIFO confirmation was correct 4/4.** Popping the oldest unconfirmed `tool_use_id` for
   the session on each `SubagentStart` reproduced the `PostToolUse` ground-truth pairing
   exactly:

```
FIFO paired toolu_01JbkAYyeNvj3GnYTk9MrDZK -> a831043fbd5fdd318   truth a831043fbd5fdd318   MATCH
FIFO paired toolu_012rtGsac94Fa7U9ssAbQVgm -> aabf925d6d594b811   truth aabf925d6d594b811   MATCH
FIFO paired toolu_0182sdjXxDLABDhXry7TK9SX -> adcc8ee2b9293f58a   truth adcc8ee2b9293f58a   MATCH
FIFO paired toolu_01CT5HSwH6t7zWK5xhZaLS8k -> a7b9a4734d0f0d0a4   truth a7b9a4734d0f0d0a4   MATCH
```

Probe 2 (four background spawns) gave the same result, 4/4.

**Caveat, recorded deliberately:** this is ordering *observed*, not ordering *guaranteed*.
Nothing in the surface promises it. The reaper (invariant 2) is what makes a mispairing
survivable rather than corrupting.

## 8. `SubagentStart` does **not** fire once per spawn

Probe 1 spawned exactly one subagent (`subagent_stats: {"spawned":1,...,"completed":1}`) and
`SubagentStart` fired **twice** for the same `agent_id`, as did `SubagentStop`:

```
t=712.313  SubagentStart  agent_id=aca0c17642a96d305     ← the spawn, after PreToolUse(Agent)
t=716.234  SubagentStop   agent_id=aca0c17642a96d305
t=729.606  PreToolUse     tool_name=SendMessage  to=aca0c17642a96d305
t=729.656  SubagentStart  agent_id=aca0c17642a96d305     ← a RESUME, no PreToolUse(Agent)
t=731.354  SubagentStop   agent_id=aca0c17642a96d305
```

The second `SubagentStart` payload is byte-identical in shape to the first. It was triggered
by `SendMessage` resuming the agent — `tool_response` was
`{"success":true,"message":"Resuming agent aca0c17","resumedAgentId":"aca0c17642a96d305",...}`.

Two consequences:

- **`SubagentStart` is a start-*or*-resume signal.** A confirm keyed naively on it
  double-counts. Confirm MUST be idempotent per `agent_id`.
- **There is a spawn path that never passes the `Agent` gate.** Resuming an agent via
  `SendMessage` puts a subagent back on the CPU with no `PreToolUse(Agent)` anywhere. The gate
  cannot see it, so the budget cannot enforce against it.

The second `SubagentStop` also reported the agent as still running in its own payload —
`"background_tasks":[{"id":"aca0c17642a96d305","type":"subagent","status":"running",...}]`.

### 8.1 `SubagentStart` fires per `Agent`-tool spawn, interactive or not

**This section previously said the opposite, and it was wrong.** It is left here as a
correction rather than quietly rewritten, because the wrong version was believed for long
enough to be filed as a launch blocker (GOV-27) and to have a fix designed against it.

**What GOV-09 measured, which was correct.** Over a 5 h 20 m window in
`~/.claude/governor/samples.jsonl` (`claude 2.1.263`, hooks installed 17:26:39Z):

| | `SubagentStart` | `SubagentStop` |
|---|---|---|
| 2 headless `claude -p` probe sessions | 3 | 3 |
| 25 ordinary interactive sessions | **0** | **62** |

**What GOV-09 concluded, which was not.** That the discriminator was headless versus
interactive — so `confirm` never runs in ordinary use, no lease is ever bound, and every
lease is reaped at its 30 s tentative TTL while its agent is still running.

**The confound.** The table counts sessions, not spawns. Cross-referencing the same window
against the transcripts in `~/.claude/projects` shows the 25 interactive sessions made
**three `Agent` tool calls between them** — and all three belong to the two probe sessions.
There were 26 `Agent` calls in *all* 267 transcripts on the machine, ever. The interactive
sessions were not failing to emit `SubagentStart` for their spawns; they were almost never
using the tool that this governor gates. Headless-versus-interactive and
uses-the-`Agent`-tool were perfectly correlated in that sample because the probe sessions
existed precisely to exercise it.

**The direct measurement, GOV-27, 2026-09-08.** Six `Agent` spawns driven from one
**ordinary interactive session** (`545d79a1`, this repo, not a probe), in two fan-outs of
three:

```
23:31:29.609  SubagentStart  acfd08ff14c716632
23:31:29.609  PostToolUse    toolu_01Q9er… → acfd08ff14c716632  async_launched
23:31:32.146  SubagentStop   acfd08ff14c716632
…
23:33:55.434  SubagentStart  a0d77362854958368
23:33:55.434  PostToolUse    toolu_01EAYK… → a0d77362854958368  async_launched
23:34:18.490  SubagentStop   a0d77362854958368
```

Six spawns, **six `SubagentStart`, six `PostToolUse`, six `SubagentStop`** — 1:1:1, in an
interactive session. `SubagentStart` fires for an `Agent`-tool spawn whether the session is
headless or interactive. The lifecycle of §4.3 is intact and `confirm` runs in ordinary use.

**Verified end to end, not just from the log.** With the daemon running and the real
`gate` / `confirm` / `release` hooks wired, a three-agent interactive fan-out produced:

```
in_use=1 tentative=1              ← three PreToolUse acquires
in_use=2 tentative=2
in_use=3 tentative=3
in_use=3 tentative=2 confirmed=1  ← three SubagentStart confirms
in_use=3 tentative=1 confirmed=2
in_use=3 tentative=0 confirmed=3  ← all three agents running, all three slots held
in_use=2 confirmed=2              ← three SubagentStop releases
in_use=1 confirmed=1
in_use=0
```

`in_use` equalled the number of agents actually running for the whole fan-out, and the
daemon logged **zero reaps**: every lease was returned by its own `SubagentStop`, none by a
TTL or by liveness. The over-subscription this section used to predict does not occur.

**What the other 63 stops were, and why it matters more than this did.** They came from a
spawn path that emits `SubagentStop` but no `PreToolUse` on `Agent` — strongly correlated
with `Skill` invocations, of which there were 48 in the same window. Those subagents never
pass the gate, so they never take a lease, and the governor does not see them at all. That
is a real governance gap and a much larger one than the bug this section used to describe:
on this machine the gated path accounted for 3 of 70 observed subagent stops. It is filed as
its own story rather than fixed here.

**Two incidental facts settled by the same run.**

- **Hooks are re-read during a session, not only at its start.** The `PostToolUse` and
  `Stop` hooks were installed at 23:31:12Z into a session that began at 23:03Z, and fired
  17 seconds later. Any argument that rules out an instrument fault by pointing at when a
  session began (as the previous version of this section did, defensibly) is unsound.
- **`Stop` fires.** Recorded from a separate live session, carrying `stop_hook_active` and
  `background_tasks`.

**The limit of this, stated plainly.** All six spawns reported
`tool_response.status: "async_launched"` — this Claude Code version launches an interactively
requested subagent in the background. A **foreground** `Agent` spawn from an interactive
session was not reachable and so is not covered; the foreground case is evidenced only by
probe 1 (§7), which was headless. Both halves of the surface fired there too.

## 9. Nested spawns are not refused by Claude Code

Probe 5, verbatim from the run's `subagent_stats`:

```json
{"spawned":2,"requested":{"background":0,"foreground":2,"unset":0},"started_in_background":0,"max_depth":2,"spawned_by_subagents":1,"completed":2,"failed":0,"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},"by_type":{"general-purpose":2}}
```

`max_depth: 2`, `spawned_by_subagents: 1`, `refused.depth_limit: 0`. A subagent spawned a
further subagent and it ran. §6.3's depth limit is therefore the governor's job entirely;
there is no harness behaviour to lean on. The `refused` counters (`depth_limit`,
`concurrency_limit`, `budget`) exist but stayed zero.

## 10. Timeout

### 10.1 A configured `timeout` is honoured, in seconds, precisely

Probe 3 registered three hooks on the same spawn, each ticking a log once a second while
sleeping up to 150 s:

| Hook `timeout` | Survived | Outcome |
|---|---|---|
| unset | 150 s (all ticks) | ran to completion, never killed |
| `120` | **119.1 s** | killed mid-sleep, log stops at tick 114 |
| `900` | 150 s (all ticks) | ran to completion, never killed |

`timeout: 120` cut at 119.1 s. The unit is seconds and the value is enforced to about a
second. The whole `claude -p` run took `duration_ms: 169493` and the spawn then succeeded —
so Claude Code **blocked on the slowest hook for its full ~150 s** rather than abandoning it.

The default (unset) was **not** cut at 60 s, and was not cut at all within the 150 s probed.
Its true ceiling was not established; the design should not depend on it.

### 10.2 A decision emitted after a long wait is still honoured

Probe 6 slept, then wrote a `deny` to stdout. At 20 s (no `timeout` configured) the deny was
honoured: the run's `result` was `REFUSED — "GOV-01 probe: denied after 20s to measure the
wait ceiling."`, `subagent_stats.spawned: 0`, and the tool appeared in `permission_denials`.

The deny payload the gate should emit is exactly:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"..."}}
```

`permissionDecisionReason` is what reaches the model, verbatim.

At 75 s, with `"timeout": 900` configured on the hook, the deny was **also** honoured:
`result` was `REFUSED "GOV-01 probe: denied after 75s to measure the wait ceiling."`,
`subagent_stats.spawned: 0`, `permission_denials: ["Task"]`, total run `duration_ms: 81839`.
The hook log shows `hook-start 1788809039.015` / `emitting-deny-after-75s 1788809114.084`.

So the wait window is bounded by the hook's configured `timeout`, not by a 60 s floor, and a
decision arriving well past 60 s is still applied. **20 s block-and-wait is safe with room to
spare** — but §4.1 should set `timeout` explicitly rather than inherit an unmeasured default.

---

## 11. Fields the design did not know about

Worth having, none of them load-bearing yet:

| Field | Where | Note |
|---|---|---|
| `prompt_id` | every event except `SessionStart` | stable across one turn; shared by every spawn in that turn, so it is not a spawn key |
| `permission_mode` | `PreToolUse`, `PostToolUse`, `SubagentStop` | absent from `SubagentStart` |
| `effort.level` | most events | absent from `SubagentStart` |
| `cwd`, `transcript_path` | every event | `cwd` is the session's, useful for the statusline |
| `run_in_background` | `tool_input` | background subagents consume CPU identically and must consume a lease |
| `agent_transcript_path` | `SubagentStop` | per-agent transcript |
| `background_tasks`, `session_crons` | `SubagentStop` | other live work in the session |

## 12. Reproducing this

The probe harness was throwaway and is not committed — it lived in a session scratchpad and
touched nothing under `~/.claude`. To rebuild it: write the `cat > file` hook above, point a
`--settings` JSON at it for `PreToolUse` / `PostToolUse` / `SubagentStart` / `SubagentStop` /
`SessionStart` / `SessionEnd`, and run

```
cd <scratch>/work && claude -p "<prompt that forces a spawn>" \
  --settings <scratch>/settings.json \
  --setting-sources "" \
  --permission-mode bypassPermissions \
  --model sonnet \
  --output-format json
```

`--setting-sources ""` is what keeps the user's real configuration out of the result, and
`--output-format json` is what yields `subagent_stats` and `permission_denials` as an
independent check on what the hooks saw.


---

## 9. Not the hook surface, but adjacent: never overwrite the binary in place

Found in GOV-02 while redeploying the Phase 0 binary, and recorded here because it presents as
a hook failure and would otherwise be debugged as one.

`cp` onto a running Mach-O executable on macOS invalidates its code signature. Every
subsequent execution is **SIGKILLed by the kernel** — the process exits `137`, writes nothing
to stdout or stderr, and leaves no message in any log:

```
$ cp bin/governor ~/.local/share/claude-governor/bin/governor
$ echo '{...}' | ~/.local/share/claude-governor/bin/governor trace agent
$ echo $?
137
```

The hook is correct, its tests pass, and it still cannot exit 0, because the failure is
external to the program. Replacing the file by `rename(2)` instead — write a temp file in the
same directory, then rename over it — leaves running processes on the old inode and the new
one correctly signed. `design.md` §13.4 now requires it of upgrade, which is the moment this
would otherwise hit every hook of every live session at once.
