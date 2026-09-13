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

**Re-measured on 2.1.270 — see [section 18](#18-re-measured-on-21270--the-gov-01-facts-checked-again).** This
provenance block covers sections 1 through 14 only. Section 15 was measured on 2.1.265,
section 17 on 2.1.267, and GOV-30 re-ran GOV-01's own probe against 2.1.270 on 2026-09-13 and
recorded the result in section 18. Read section 18 alongside any conclusion drawn from the
sections below it: where the two disagree, section 18 is the later measurement and says so
explicitly.

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
`agent_type`, and note that `subagent_type` is **absent** from `tool_input` here — a
version-specific observation, n=1, and **contradicted on 2.1.270 in section 18.5**, where a
nested spawn carried it. Nothing depends on it either way: depth comes from the top-level
`agent_id`, never from `tool_input`.

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

## 8. `SubagentStart` is a start-*or*-resume signal, so it can fire twice for one spawn

> An earlier version of this section also claimed `SubagentStart` never fires in
> interactive sessions. That claim was wrong; §8.1 carries the correction and is kept
> rather than rewritten. Everything below it in this section still holds.

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
path that emits `SubagentStop` but no `PreToolUse` on `Agent` — strongly correlated with
`Skill` invocations, of which there were 48 in the same window. Those never pass the gate,
so they never take a lease, and the governor does not see them at all. On this machine the
gated path accounted for 3 of 70 observed subagent stops. It is filed as its own story
rather than fixed here.

> **The `Skill` half of that paragraph is wrong.** GOV-28 measured it and the correlation
> does not survive: those stops are not subagents at all. §8.2 carries the correction. The
> sentence is left standing because a fix was designed against it, which is the second time
> on this surface that a per-session correlation has been read as a mechanism — see the
> banner at the head of §8.1.

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

### 8.2 `SubagentStop` is not a count of subagents

**GOV-28, 2026-09-09.** §8.1 closed by saying the 63 stops the gate could not see were
"strongly correlated with `Skill` invocations", and GOV-28 was filed to widen the matcher and
catch them. Measured, that is wrong twice over: they are not `Skill`, and they are not
subagents.

**The fresh trace.** `~/.claude/governor/samples.jsonl`, 2026-09-08T17:27:00Z →
2026-09-09T18:29:10Z (25 h 02 m), `claude 2.1.263`, 31 sessions:

| | count |
|---|---|
| `SubagentStop` | 91 |
| `SubagentStart` | 9 |
| `PostToolUse` on `Agent` | 6 |
| per-agent transcripts written under `projects/*/subagents/` | 9 of the 91 |

The 9 `SubagentStart` records, the 9 stops with a transcript, and the 9 agents the gate
leased are **the same nine agents**. The other 82 stops have no start, no `PreToolUse`, no
transcript, and — see below — nothing that ever ran.

**Four independent lines of evidence, because a fifth correlation was not going to be enough.**

1. **They are `away_summary`, to the millisecond.** Indexing every transcript entry on the
   machine by `(session_id, timestamp)`, **57 of the 82** land on a `system` entry of
   subtype `away_summary` in the same session at the same millisecond. The remaining 21 fire
   1.5–3 s after a `turn_duration` entry and write nothing to the transcript at all. Fixed
   offsets, not a distribution: the away-summary cluster sits at **182.6–183.4 s** after the
   session's `Stop`.

2. **`Skill` is refuted from the same window.** There were 62 `Skill` tool calls in it.
   Only **3 of the 82** ungated stops had a same-session `Skill` call within the preceding
   120 s. GOV-27 counted *sessions* that used `Skill` against *sessions* that emitted
   unexplained stops, and both numbers were large; the per-event join is not.

3. **A `Skill` invocation spawns nothing, measured directly.** A probe registering all seven
   events, driving a local skill (`--setting-sources project`):

   ```
   SessionStart
   PreToolUse    tool_name=Skill          ← the skill IS matchable
   PostToolUse   tool_name=Skill
   PreToolUse    tool_name=Bash           ← its work, inline, agent_id absent
   PostToolUse   tool_name=Bash
   Stop
   SessionEnd
   ```

   `subagent_stats: {"spawned": 0, …}`. No `SubagentStart`, no `SubagentStop`. A skill of
   this kind runs **in the calling session**, and there is no subagent to govern.

4. **There is no `PreToolUse` for them, under any tool name.** A catch-all `PreToolUse`
   registration ran for 40 minutes across every live session on the machine and captured 85
   calls (`Bash` 73, `Write` 4, `Edit` 3, the Notion MCP tools, `ToolSearch`). Exactly one
   `SubagentStop` occurred in that window — session `24b54357`, `Stop` at 18:48:09.366,
   `SubagentStop` at 18:51:12.084 (+182.7 s), against `away_summary` at 18:51:12.086 in that
   session's transcript — and **no `PreToolUse` fired for that session in between**. Not one
   under an unexpected name; none at all.

**The mechanism.** The 2.1.263 binary carries the string

```
Converting Stop hook to SubagentStop for … (subagents trigger SubagentStop)
```

adjacent to `execution_context` and `agent-preload`. So `SubagentStop` is what a **`Stop`**
event becomes when the execution context that finished is not the main conversation — and
the harness's own away-summary generation is such a context. A probe registering **only**
`Stop` received one plain `Stop` and never saw the subagent's completion, which is that
conversion in the observable direction: converted events go to `SubagentStop` handlers and a
`Stop` registration does not see them.

**A discriminator that separates the two populations cleanly.** Across every v2 record in the
trace, with no exceptions:

| `agent_type` on `SubagentStop` | gated | ungated |
|---|---|---|
| key present, value **set** | 7 | 0 |
| key present, value **empty** | 0 | 81 |

This also explains the "third state" §4's GOV-02 addendum recorded and could not account for.
A `SubagentStop` carrying `agent_type` as an empty string is the signature of a non-agent
execution context, not of an agent whose type went missing.

**What this costs the budget: nothing.** The count the governor leases against and the count
of subagents that actually ran are the same number — 9 and 9. The gate was not governing 4%
of spawns; it was governing all of them, and `SubagentStop` was being read as a spawn counter
when it is not one.

### 8.3 Every spawn path on 2.1.263

The enumeration GOV-28 was filed to produce. "Gated" means a `PreToolUse` the gate can match
*and* a lease that is returned, verified end to end against a running daemon in §8.4.

| # | Path | `PreToolUse`? | `tool_name` | Puts a subagent on the CPU | Gated |
|---|---|---|---|---|---|
| 1 | `Agent` tool spawn — foreground, background, or nested | yes | `Agent` (a `Task` matcher also fires, §1) | yes | **yes**, since GOV-07 |
| 2 | Resuming an agent through `SendMessage` | yes | `SendMessage`, with `tool_input.to` = the agent id | yes | **yes**, since GOV-28 |
| 3 | `Skill` invocation | yes | `Skill` | **no** — runs inline, `spawned: 0` | n/a, nothing to govern |
| 4 | Away-summary and other non-main execution contexts | **no, none at all** | — | **no** — no start, no tools, no transcript | **cannot be**, see below |

**Path 2 is a real gap and it is closed.** Probe C, all events registered, one spawn then one
resume:

```
t= 0.000  PreToolUse     tool=Agent          ← the spawn: gated
t= 0.045  SubagentStart  agent=ac05ddd6e5bdf6c51
t= 1.782  PreToolUse     tool=Bash           agent=ac05ddd6e5bdf6c51
t= 3.050  SubagentStop   agent=ac05ddd6e5bdf6c51
t= 3.088  PostToolUse    tool=Agent
t= 6.676  PreToolUse     tool=SendMessage    ← the resume: NO PreToolUse(Agent) anywhere
t= 6.718  PostToolUse    tool=SendMessage    resumedAgentId=ac05ddd6e5bdf6c51
t= 6.718  SubagentStart  agent=ac05ddd6e5bdf6c51    ← same agent_id, second start
t= 8.370  PreToolUse     tool=Bash           agent=ac05ddd6e5bdf6c51
t= 9.649  SubagentStop   agent=ac05ddd6e5bdf6c51
```

The agent runs real work after the resume. Before GOV-28 it held no lease while doing it.
`confirm` and `release` needed no change — `SubagentStart` fires on a resume and
`SubagentStop` still carries the `agent_id` — so the missing acquire was the whole of it.

The `PreToolUse(SendMessage)` payload carries the recipient:

```json
{"to":"ac05ddd6e5bdf6c51","summary":"ask for first line of README.md",
 "message":"Also report the FIRST line of README.md…","recipient":"ac05ddd6e5bdf6c51"}
```

`to` is the discriminator, and it has to be, because `SendMessage` also addresses teammates,
other local sessions and cloud sessions **by name**. The gate takes a lease only when `to`
has the shape of an agent id — seventeen lowercase hex characters, §2. A resume addressed by
name is therefore still ungoverned; that is a stated limit rather than an oversight, and it
fails open, which is the side to fail on.

**Path 4 cannot be gated, and this is the honest answer rather than a deferral.** Gating
requires an event to decide on. There is no `PreToolUse` for these — measured, not assumed,
with a catch-all matcher watching every tool in every session. The only event they emit is
the completion. A governor cannot refuse something it first hears about when it is over. It
also should not want to: nothing was spawned, no tool ran, and the work is the harness's own
bookkeeping. The correct handling is what the daemon already does — `release` keys off
`byAgent`, which only `confirm` populates, so one of these arrives as `no_lease` and changes
nothing (§8.4).

### 8.4 End-to-end, against a running daemon

Real daemon (`--budget 4`), real `gate` / `confirm` / `release` / `session` hooks, `in_use`
polled over the daemon's own `status` op at 20 Hz.

**Path 1 — three `Agent` spawns in one message.** `subagent_stats.spawned: 3`.

```
in_use=0                      in_use=3  (0 tentative, 3 confirmed)
in_use=1  tentative 1         in_use=2
in_use=1  confirmed  1        in_use=1
in_use=2  tentative 1         in_use=0
```

**Path 2 — one spawn, then a `SendMessage` resume.** Two full lease cycles for one agent:

```
matcher Agent|Task|SendMessage      matcher Agent|Task   (the control, pre-GOV-28)
  in_use 0 → 1 → 0    the spawn       in_use 0 → 1 → 0     the spawn
  in_use 0 → 1 → 0    the resume      (nothing)            the resume, ungoverned
```

Both runs completed both steps — line count and first line — so both did resume; only the
widened matcher leased for it.

**Path 3 — a `Skill` invocation.** `spawned: 0`, the skill returned its answer, and `in_use`
never left 0.

**Path 4 — an away-summary `SubagentStop` cannot take a live agent's slot.** Fed the real
`agent_id` captured at 18:51:12, while a genuine agent held a confirmed lease:

```
after Agent gate        in_use=1  (tentative 1)
after confirm           in_use=1  (confirmed 1)
away_summary stop  →    governor release: ok=false reason="no_lease"  in_use=1
after the real stop     in_use=0
```

**The catch-all was removed.** It ran for 40 minutes, and `~/.claude/settings.json` was
restored byte-identically from its backup afterwards — verified with `cmp`, not asserted.

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

## 13. Not the hook surface, but adjacent: never overwrite the binary in place

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

---

## 14. Not the hook surface, but adjacent: the `statusLine` command's stdin

Measured for GOV-10. The statusline is not a hook — it is the `statusLine` key in
`settings.json` — but it is the same shape of contract: Claude Code runs a command, writes
JSON to its stdin, and reads its stdout. It is recorded here because it is the only other
place this binary is executed by the harness, and because §10 of design.md says facts about
that harness get measured rather than assumed.

**Method differs from §0, and the difference matters.** The statusline is a TUI surface and
does not run under `claude -p`, so the `cat > file` probe used for the hooks cannot reach it.
These facts were instead read out of the shipped binary
(`/Users/peterho/.local/share/claude/versions/2.1.265`, `claude --version` → `2.1.265`,
2026-09-09) — the payload construction, the invocation, and the embedded documentation string
the `statusline-setup` agent is given. That is weaker evidence than a captured payload: it is
this version's source of truth rather than an observation of it, and it should be re-checked
against a real capture if any of it ever appears to be wrong.

### 14.1 The payload

Every field below is present in the object the harness builds for the statusline. The ones
GOV-10 relies on are `session_id` and nothing else.

```jsonc
{
  "session_id": "string",        // always present
  "session_name": "string",      // optional, set via /rename
  "prompt_id": "string",         // optional
  "transcript_path": "string",
  "cwd": "string",
  "scratchpad_dir": "string",    // optional
  "permission_mode": "string",
  "agent_id": "string",          // optional
  "agent_type": "string",
  "model":       {"id": "string", "display_name": "string"},
  "workspace":   {"current_dir": "string", "project_dir": "string", "added_dirs": ["string"],
                  "git_worktree": "string", "repo": {"host": "…", "owner": "…", "name": "…"}},
  "version": "string",
  "output_style": {"name": "string"},
  "context_window": {"total_input_tokens": 0, "total_output_tokens": 0,
                     "context_window_size": 0, "current_usage": {…} | null,
                     "used_percentage": 0 | null, "remaining_percentage": 0 | null},
  "cost": {"total_cost_usd": 0, "total_duration_ms": 0, "total_api_duration_ms": 0,
           "total_lines_added": 0, "total_lines_removed": 0},
  "exceeds_200k_tokens": false,
  "fast_mode": false,
  "effort": {"level": "…"},      // optional
  "thinking": {"enabled": true},
  "rate_limits": {…},            // optional
  "vim": {"mode": "…"},          // optional
  "agent": {"name": "…"},        // optional
  "remote": {"session_id": "…"}, // optional
  "pr": {…}, "worktree": {…}     // optional
}
```

### 14.2 Five facts about the invocation

1. **There is no `hook_event_name`.** Every hook payload in §1–§5 carries one; this does not.
   A binary that dispatches on that field cannot dispatch a statusline, which is why
   `governor statusline` takes its identity from its subcommand argument alone.

2. **A non-zero exit discards the output entirely.** The harness checks the status first and
   only then reads stdout. So a statusline that fails must still exit 0 and say what went
   wrong *in the line* — exiting non-zero does not show the user an error, it shows them
   nothing. This is the opposite of the gate's contract in §10.2 and the reason
   `governor statusline` never propagates a failure into its exit code.

3. **stdout is trimmed, split on newlines, blank lines dropped, and rejoined.** Multi-line
   output is therefore supported. `governor statusline` emits one line regardless.

4. **stderr is captured to the debug log and never displayed.** Diagnostics are safe to write
   there, on the same bargain the hooks make.

5. **It is invoked on events only, unless `statusLine.refreshInterval` is set.** Measured for
   GOV-B07 against `2.1.267` by the same read-the-binary method as the rest of §14. The
   settings schema is

   ```jsonc
   statusLine: {
     type: "command",
     command: string,
     padding?: number,
     refreshInterval?: number,      // .min(1), "Re-run the status line command every N
                                    // seconds in addition to event-driven updates"
     hideVimModeIndicator?: boolean
   }
   ```

   and it resolves to milliseconds as `refreshInterval === undefined ? null : Math.max(1, n) * 1000`.
   Four consequences, all load-bearing for GOV-B07:

   - **Absent means no timer at all**, and the events are: a new assistant message, a compact,
     a permission-mode change, a settings change. A session waiting on a fan-out produces
     none of them, so the line freezes at its pre-fan-out value for the whole fan-out. That
     is the defect GOV-B07 fixes and it is a property of the harness, not of our command.
   - **The unit is seconds and the floor is 1.** A sub-second interval is not an error, it is
     silently clamped. The zod field also carries `.catch(undefined)`, so a malformed value
     drops the key rather than rejecting the whole settings file — which is also why writing
     this key is safe against an older Claude Code that does not know it.
   - **The timer is a plain repeating `setTimeout`, not reset by event-driven renders.** An
     active session pays both, and an idle session pays the timer forever. The cost of the
     interval therefore scales with sessions open, not with sessions working.
   - **Each fire goes through the same 300 ms debounce as an event refresh**, which coalesces
     a timer fire with a concurrent event rather than running the command twice.

Two settings suppress the statusline entirely and are worth knowing when one appears not to
run: `disableAllHooks` (which covers statusLine execution as well as hooks, despite the name)
and an unaccepted workspace trust prompt.

**`COLUMNS` was not observed to be set** for the child process. A statusline that treated an
absent `COLUMNS` as a width of zero would render as the empty string on every such machine,
so it is read as "unknown, do not truncate" rather than as a width.

---

## 15. Where a plugin has to live for its hooks to fire

**Measured on `claude --version` = `2.1.265 (Claude Code)`, macOS/arm64, 2026-09-09 (GOV-21).**

design.md §9 and §13.3 named `~/.claude/plugins/claude-governor/` as the plugin directory
`governor install` would write. **That path is inert.** Nothing loads a directory dropped
there, so a gate registered in it would never fire — the silent failure §10.1 was spent
measuring the tool name to avoid, arriving by a different door: every file present, the
install apparently complete, and not one spawn governed.

### 15.1 What `~/.claude/plugins` actually holds

Not plugins. On this machine it holds a marketplace cache and bookkeeping:

```
~/.claude/plugins/
├── installed_plugins.json        installPath per plugin, per scope
├── known_marketplaces.json       marketplace name → source and install location
├── marketplaces/<marketplace>/   the marketplace repo, cloned
├── cache/<marketplace>/<plugin>/<version>/    the plugin itself
└── data/<plugin>-<marketplace>/  ${CLAUDE_PLUGIN_DATA}
```

A plugin installed this way is also listed in `enabledPlugins` in `settings.json`. So the
supported route in is a marketplace — which is one command more than "curl this" and requires
somewhere to publish a marketplace to.

### 15.2 `~/.claude/skills/<name>/` auto-loads, and is the way in

`claude plugin init --help` names it: *"Scaffold a new plugin at `~/.claude/skills/<name>/`
(auto-loads next session as `<name>@skills-dir`)"*. Verified directly rather than taken from
the help text.

A directory containing only `.claude-plugin/plugin.json` and `hooks/hooks.json`, with **no**
`settings.json` entry and **no** `installed_plugins.json` entry, was reported by
`claude plugin list` as:

```
❯ gov-probe@skills-dir
  Version: 0.0.1
  Scope: user
  Path: ~/.claude/skills/gov-probe
  Status: ✔ loaded
```

Three facts follow, each verified by running `claude -p` against a probe hook that appended
its arguments and environment to a file:

1. **`SessionStart` from a plugin's `hooks/hooks.json` fires.**
2. **`PreToolUse` *with a matcher* fires.** The probe registered
   `{"matcher": "Bash", "hooks": [...]}` and it ran on a `Bash` call — so §4.1's registration
   shape works from a plugin file, matcher included. This is the one that mattered: the whole
   gate is a matched `PreToolUse`.
3. **`${CLAUDE_PLUGIN_ROOT}` and `${CLAUDE_PLUGIN_DATA}` both expand** inside the hook's
   command string, to `~/.claude/skills/gov-probe` and
   `~/.claude/plugins/data/gov-probe-skills-dir` respectively. A skills-dir plugin's
   marketplace component is the literal string `skills-dir`.

Probe hook output, verbatim:

```
sessionstart root=/Users/peterho/.claude/skills/gov-probe
data=/Users/peterho/.claude/plugins/data/gov-probe-skills-dir
pretooluse-bash root=/Users/peterho/.claude/skills/gov-probe
data=/Users/peterho/.claude/plugins/data/gov-probe-skills-dir
```

### 15.3 Plugin hooks and `settings.json` hooks coexist

During the probe, `settings.json` carried its own `SessionStart` hook and the plugin carried
another. **Both ran.** They merge; the plugin's registration does not replace the user's.

This is *not* a general answer to whether plugin hooks merge with project-level ones in every
configuration — it is one observation on one event with two registrations, and it is recorded
because it is the reason `governor install` can leave the user's own hooks alone rather than
having to reason about which file wins.

### 15.4 Consequences folded into design.md

§9's package tree and §13.3's path table now say `~/.claude/skills/claude-governor/`. The five
hooks live in that plugin's own `hooks/hooks.json`, written entirely by us, so the only thing
`governor install` merges into the user's `settings.json` is the `statusLine` key — `statusLine`
is not a plugin component (`claude plugin init --with` offers skills, agents, hooks, mcp, lsp,
output-style and channel, and no statusline), so it has nowhere else to go.

`governor doctor` reads the old path as well as the new one. A machine installed by an earlier
build has hooks sitting in the inert directory, and a diagnostic that could not see them would
report "not registered" for a file it is looking straight at.

### 15.5 Still unmeasured

- Whether `~/.claude/skills/` auto-loading survives a Claude Code upgrade. It is documented in
  `claude plugin init`'s own help, so it is a supported route rather than an accident, but this
  is one version.
- Whether a plugin there is picked up by a session **already running**. `install` tells the user
  it is not, which is the safe claim; `SessionStart` is where hooks are read.
- Linux. Every measurement in this section is macOS.

---

## 16. Where a hook's output goes — the user, the model, or nowhere

Measured 2026-09-09 on Claude Code **2.1.265**, macOS arm64, against a throwaway skills-dir
plugin (`gov22-probe`) registering a single `SessionStart` hook — §15.2's route, no
`settings.json` entry. GOV-22 needed one fact this document did not have: a first-run notice
is written for a *person*, and nothing here recorded which channel a person actually reads.

Two drivers, because they answer different halves of it. `claude -p … --output-format
stream-json` shows what reaches the **model** (the prompt asked it to quote back any marker in
its own context) and carries a `system`/`hook_response` event per hook with `stdout`, `stderr`,
`exit_code` and `outcome`. A real TUI under a pty shows what reaches the **person**, which
`claude -p` cannot: two of the results below are invisible in headless mode entirely.

### 16.1 The three channels

| Hook writes | Reaches the model | Reaches the person |
|---|---|---|
| plain text on stdout, exit 0 | yes, as `SessionStart:startup hook success: <text>` | **no** |
| `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"<text>"}}` | yes, wrapped in `<system-reminder>` as `SessionStart hook additional context: <text>` | **no** |
| `{"systemMessage":"<text>"}` | **no — nothing at all** | yes, as `SessionStart:startup says: <text>` |
| stderr, exit 0 | no | no (captured, not displayed) |
| stderr, non-zero exit | no | yes, as `[<command path>]: <stderr>` under a `hook error` line |

The two JSON fields compose: sent in one object, `systemMessage` goes to the person and
`additionalContext` to the model, each unaware of the other.

**`systemMessage` is therefore the only channel that addresses the user**, and it is the one
`governor session start` uses for the first-run notice. `additionalContext` is the channel
§8's advisory instruction (GOV-17) will want, and the two are not interchangeable in either
direction — a notice sent as `additionalContext` is read by nobody but the model, and a
standing instruction sent as `systemMessage` is read by nobody but the person.

**It is invisible in headless mode.** `systemMessage` did not appear in `claude -p` output in
either text or `stream-json` form. A test of it has to drive a pty or read the code, which is
why GOV-22's tests assert the JSON this binary *emits* rather than what a session displays.

### 16.2 Three traps

1. **A non-zero exit discards plain-text stdout, but not `additionalContext`.** Same rule as
   the statusline in §14.2 for the plain-text route — it is gated on `outcome == "success"` —
   and the JSON route is not gated at all: a hook exiting 1 still injected its
   `additionalContext`, twice out of two.

2. **Printing parseable hook JSON suppresses the user-visible error, even at exit 1.** Three
   runs: the TUI showed no `hook error` line and no stderr, while the transcript still recorded
   `"exit_code": 1, "outcome": "error"`. A hook that fails while printing valid JSON fails
   *silently to the user*. This is the reason `sessionNotice` carries one field and nothing
   else — the less a hook says on stdout, the less it can hide.

3. **Unknown top-level keys are ignored, not fatal**, with a hint naming
   `hookSpecificOutput.additionalContext`. A typo'd field is silence, not an error.

### 16.3 Caps, read from the binary rather than probed

`systemMessage` 4000 chars / 20 lines; `additionalContext` 8000 / 200; `reason`,
`stopReason` and `permissionDecisionReason` 2000 chars / 20 lines. Flagged as a binary read:
over-length truncation was **not** exercised. The first-run notice is six lines and well
under 1000 characters, so it is not close to either bound.

### 16.4 Still unmeasured

- Whether `systemMessage` surfaces in any headless mode. It does not in `-p` text or
  `stream-json`; the SDK path was not tested.
- Truncation behaviour at the caps above.
- Linux. Every measurement in this section is macOS.

---

## 17. Hooks are bound at session start, and how to see which sessions loaded them

**Measured on `claude --version` = `2.1.267 (Claude Code)`, macOS/arm64, 2026-09-09 (GOV-29).**

A Claude Code session loads its hooks when it starts. A session that was already open when a
plugin was installed does not pick them up later — not on the next turn, not on the next
spawn, not ever. It is ungoverned for the rest of its life, and only restarting it changes
that.

Nothing in sections 1–5 can observe this, and that is the point: the hooks of an ungoverned
session never run, so the evidence has to come from outside the hook surface entirely.

### 17.1 The evidence

Three facts settled it, none of them inference:

1. Session `b99c1ee8` (edgefinder-v2) had existed since at least 2026-09-08 17:29, the mtime
   of its tool-results directory.
2. `~/.claude/skills/claude-governor/hooks/hooks.json` was written 2026-09-09 18:57 and its
   binary 19:09. The session predates the gate by about twenty-six hours.
3. The Phase 0 trace hooks live in `~/.claude/settings.json` and were installed 2026-09-08,
   **before** that session started — which is exactly why trace captured every spawn in the
   window while the gate captured none. Two load paths, two install times, one old session.

Two runs twelve minutes apart on the same machine, same version, same repo, same background
path: in the old session, three subagents demonstrably alive and `governor status` polled at
1 Hz read `in_use=0` at all forty-seven ticks. In a session started after the install,
`in_use` climbed 0-1-2-3 and held.

Three hypotheses were eliminated by measurement and must not be re-investigated: tentative
TTL reaping (the zero was read 17 s after launch, before the 30 s TTL could fire), a tool
rename on 2.1.267 (`PostToolUse` carries `tool_name":"Agent"` verbatim), and background
spawns emitting no `PreToolUse` (the working run was also a background spawn).

### 17.2 `~/.claude/sessions/<pid>.json` — the harness's own live-session registry

This is what makes the condition visible, and it is the only enumeration method that works.
One file per **live** session, named by PID, alongside a `<pid>.<hash>.key` that is an auth
token and must never be opened.

```jsonc
{
  "pid": 5364,
  "sessionId": "2bcbd3bd-a469-4be6-add4-e528a09b4e34",  // the join key
  "cwd": "/Users/peterho/Repos/claude-governor",
  "startedAt": 1788998089326,                 // epoch ms, UTC
  "procStart": "Wed Sep  9 23:54:48 2026",    // UTC, "Mon Jan _2 15:04:05 2006"
  "version": "2.1.267",
  "status": "busy",                           // busy | idle
  "kind": "interactive", "entrypoint": "cli",
  "messagingSocketPath": "/tmp/cc-socks/5364.sock",
  "name": …, "nameSource": …, "nameSince": …, "peerProtocol": …, "peerFeatures": …,
  "pidDomain": …, "updatedAt": …, "statusUpdatedAt": …, "bridgeSessionId": …
}
```

`sessionId` is the same value that arrives on every hook payload (section 5) and on the
statusline payload (section 14.1), so "running on this machine" and "registered with the
daemon" can be joined directly. The governor reads seven of those fields and ignores the
rest: the file is the harness's, not ours, and parsing fields we have no use for is how a
version bump breaks something that did not need to care.

`kind` is read as well as the six fields above: only an `interactive` session can be
restarted, and restarting is the only fix an ungoverned session has. A headless `claude -p`
run is left out of that advice rather than told to do something it cannot.

The `procStart` comparison that guards against a recycled PID is a **two-second tolerance,
not an equality**. The two sides do not derive the number the same way — on Linux gopsutil
computes a start time from the jiffy counter plus `/proc/stat`'s `btime`, which can disagree
with whatever the harness wrote by about a second — so an exact match would reject every entry
on that platform and report a busy machine as empty.

**It is cleaned up on a clean exit.** Watched across two sessions ending and one restarting,
the file count tracked `ps` exactly. Stale files after an unclean exit are the exception, not
the rule — but they are why `procStart` is compared against the process's real start time
before an entry is believed, since a PID the kernel has recycled would otherwise present an
unrelated process as a live session.

**Measured cost: 1.2 ms** for four sessions — `readdir`, a JSON parse each, and one liveness
check per candidate. Re-measured against `ps` at the same instant and it agreed exactly, four
PIDs for four processes.

The alternatives were measured and are worse:

| Method | Verdict |
|---|---|
| `~/.claude/sessions/*.json` + liveness | **1.2 ms, exact.** The only one that yields a session id at all |
| `ps -Ao pid,command` | ~28 ms, and a session shows as bare `claude` with no recoverable session id |
| `/tmp/cc-socks/*.sock` | **20 sockets for 6 live sessions.** Stale sockets are not cleaned up |
| `lsof -p <pid>` | ~140 ms *per pid*, and it does not hold the transcript open, so it adds nothing |
| `.jsonl` mtime under `projects/` | Every session that ever ran, dead or alive. Not a liveness signal |

**This surface is undocumented**, so anything built on it must degrade to *unknown* when it
is absent — never to "no sessions running", which reads as a clean machine and is precisely
the false negative GOV-29 exists to remove. `signals.LiveSessions` returns `ErrNoRegistry`
for exactly this, and `governor doctor` reports "not verified" rather than a pass or a
failure.

The same rule covers a registry that is *present but unintelligible*, which is the more likely
future: a version that renames `sessionId` or reformats `procStart` would leave every file in
place and every one unparseable. Files skipped because they could not be **read** are counted
and, where none could, reported as unknown; files skipped because their **process is gone** are
not, because a directory of stale entries genuinely does mean nothing is running. Collapsing
those two into "zero sessions" is the same false all-clear arriving by a different door.

### 17.3 `scratchpad_dir`, and other fields that arrived without notice

`scratchpad_dir` is present on **365 of the last 400** hook payloads on this machine and
appears in none of the payloads documented in sections 1–5. It is on `PreToolUse`,
`PostToolUse`, `SubagentStart`, `SubagentStop` and `Stop`.

Nothing depends on it. It is recorded because the drift is only visible at all thanks to
GOV-02's decision to store `raw_keys` — key names, never values — on every traced payload.
That one choice is what let a four-version change be read off an existing trace instead of
costing another probe, and it is worth keeping for that reason alone.

### 17.4 The foreground `Agent` spawn still exists — a claim in GOV-29 corrected

GOV-29 was filed asserting that "on 2.1.267 the Agent tool has no foreground option at all —
every `PostToolUse` in the trace reads `tool_status=async_launched`". **The trace does not
support that**, and it is corrected here rather than propagated.

Of 33 `PostToolUse`/`Agent` records in `samples.jsonl`, 28 read `async_launched` and **five
read `completed`**, with real durations:

```
2026-09-09T22:13:25Z  completed  dur=1390 ms
2026-09-09T22:58:38Z  completed  dur=8943 ms
2026-09-09T22:59:15Z  completed  dur=5843 ms
2026-09-09T22:59:16Z  completed  dur=5636 ms
2026-09-09T22:59:18Z  completed  dur=5461 ms
```

A foreground spawn returns its result inline and reports how long it took; a background one
returns immediately with no duration. Both shapes are present.

The trace carries no per-record Claude Code version, so those five **cannot** be attributed
to 2.1.267 specifically — the sessions that produced them were not among those live when the
version was read. What can be said is that the strong form of the claim is unsupported by the
evidence cited for it, and no measurement here establishes that the foreground path has been
removed.

**It changes nothing about the gate**, which is why this is a correction and not a defect:
both paths raise `PreToolUse` before the spawn, and that is the only event the gate acts on.
`tool_status` is read at `PostToolUse`, after the decision. The reason to get it right is that
this file is the record other stories build on, and section 8.3's spawn-path table would be
wrong if this one were written down as filed.

### 17.5 Still unmeasured

- Whether `~/.claude/sessions/` exists on Linux, and whether it has the same shape. Every
  measurement in this section is macOS.
- Whether a session whose hooks file changes *mid-session* picks up the change on some later
  event. The binding is asserted here only for the start of a session; nothing was tested
  that rewrote hooks under a running session and then spawned.
- Whether `disableAllHooks` or an unaccepted trust prompt leaves a session in the registry
  looking identical to a governed one. It would, on everything measured here, and doctor
  would then report it as ungoverned with the wrong fix.

---

## 18. Re-measured on 2.1.270 — the GOV-01 facts, checked again

**Why this section exists.** Every fact in sections 1–10 was measured once, on 2.1.263, and
four versions have shipped since. A changed `tool_name` is the exact failure this project
exists to catch, so GOV-30 clause 3 re-ran the §0 method against the version now on this
machine rather than assuming the surface held. It held. The two things that moved are called
out in §18.5 and §18.7, and neither touches the gate.

### 18.0 Provenance

| | |
|---|---|
| `claude --version` | `2.1.270 (Claude Code)` |
| Binary | `/Users/peterho/.local/bin/claude` → `/Users/peterho/.local/share/claude/versions/2.1.270` |
| Host | macOS (Darwin 25.4.0, xnu-12377.101.15 RELEASE_ARM64_T8103), arm64 |
| Date captured | 2026-09-13, 13:43–13:45 UTC |
| Story | GOV-30 |
| Cost | three headless `claude -p` runs, `--model sonnet`, `$0.3324` total |

**`~/.claude/settings.json` was untouched, and this is evidence rather than an assertion.**
SHA-256 taken before the first probe and again after the last:

```
before  6425790c4df381e0b152c4298b949361bce1d8322e67593d969cdd1eebf09b9e
after   6425790c4df381e0b152c4298b949361bce1d8322e67593d969cdd1eebf09b9e
```

Identical, and its mtime was still `Sep 9 22:35` afterwards. Everything the probes wrote lived
under a session scratchpad. The two things under `~/.claude` that did change are neither the
probe's doing nor avoidable: the probe sessions' own transcripts land in
`~/.claude/projects/`, and the already-installed governor daemon went on sampling the host
throughout, because `--setting-sources ""` excludes the user's hooks from the *probe* without
stopping anything else on the machine.

### 18.1 Method

The §0 method, three probes instead of six. The capture hook is the same `cat > file` body:

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

| Probe | Question | Registrations |
|---|---|---|
| A | tool name, `tool_input`, `SubagentStart`/`Stop`, the `SendMessage` resume, and the matcher control | `PreToolUse` × {`*`, `Task`, `Agent`}, `PostToolUse` `*`, `SubagentStart`, `SubagentStop`, `SessionStart`, `SessionEnd` |
| B | is a configured `timeout` still honoured | one `PreToolUse` on `Bash`, `"timeout": 5`, body sleeps 30 s ticking once a second |
| C | depth — a subagent spawning a subagent | `PreToolUse` × {`*`, `Agent`}, `PostToolUse` `Agent`, `SubagentStart`, `SubagentStop` |

```
# Probe A — one spawn, then a SendMessage resume of the same agent, then a Bash in the main session
cd <scratch>/A/work && claude -p "Do these three steps in order, no extra steps.
STEP 1: Use the Agent tool to spawn ONE subagent of subagent_type general-purpose with the
prompt: 'Run wc -l < README.md in the current working directory and report only the number.'
Wait for its answer.
STEP 2: Use the SendMessage tool to send that SAME agent a follow-up message asking it to
report the FIRST line of README.md. Wait for its answer.
STEP 3: Run the Bash command: echo control-bash-in-main-session
Then reply with the line count and the first line." \
  --settings <scratch>/A/settings.json \
  --setting-sources "" \
  --permission-mode bypassPermissions \
  --model sonnet \
  --output-format json

# Probe B — timeout. settings.json is the whole of it:
#   {"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[
#     {"type":"command","command":"<scratch>/B/slow.sh t5","timeout":5}]}]}}
cd <scratch>/B/work && claude -p "Run exactly one Bash command: echo hello-timeout-probe .
Then reply with its output and nothing else." \
  --settings <scratch>/B/settings.json --setting-sources "" \
  --permission-mode bypassPermissions --model sonnet --output-format json

# Probe C — nested spawn
cd <scratch>/C/work && claude -p "Use the Agent tool to spawn ONE subagent of subagent_type
general-purpose. Give that subagent this prompt verbatim: 'You must delegate. Immediately use
your own Agent tool to spawn a nested general-purpose subagent with the prompt: run
wc -l < README.md in the current working directory and report only the number. Do not run wc
yourself; report what the nested subagent tells you.' Then report the number it returns." \
  --settings <scratch>/C/settings.json --setting-sources "" \
  --permission-mode bypassPermissions --model sonnet --output-format json
```

Each work directory held a three-line `README.md` so the subagent had something to count.

`subagent_stats` from the runs, verbatim, as the independent check on what the hooks saw:

```json
A: {"spawned":1,"requested":{"background":0,"foreground":1,"unset":0},"started_in_background":0,"max_depth":1,"spawned_by_subagents":0,"completed":1,"failed":0,"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},"by_type":{"general-purpose":1}}
C: {"spawned":2,"requested":{"background":0,"foreground":2,"unset":0},"started_in_background":0,"max_depth":2,"spawned_by_subagents":1,"completed":2,"failed":0,"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},"by_type":{"general-purpose":2}}
```

**Trimming.** In the payloads below, `transcript_path` is abbreviated to `…` — it is the long
absolute path into `~/.claude/projects/` and carries nothing this file needs. Nothing else is
altered: every other byte is as the hook received it.

### 18.2 The four GOV-01 facts, stated plainly

| Fact, as sections 1–10 record it | 2.1.270 |
|---|---|
| §1 `tool_name` on an `Agent`-tool spawn is `Agent`, and a `matcher:"Task"` fires for it too | **CONFIRMS** |
| §2 `tool_input` has no depth field; depth is carried as a top-level `agent_id`, absent at depth 0 and naming the *parent* at depth ≥ 1 | **CONFIRMS** |
| §3 `SubagentStart` fires and carries **no** `tool_use_id` — it cannot be correlated to its `PreToolUse` except positionally | **CONFIRMS** |
| §4 `SubagentStop` fires and carries `agent_id`, so release needs no correlation | **CONFIRMS** |
| §10.1 a configured `timeout` is honoured, in seconds | **CONFIRMS** |
| §8/§8.3 path 2 — a `SendMessage` resume raises `PreToolUse` with `tool_input.to` = the agent id, and a second `SubagentStart`, with no `PreToolUse(Agent)` anywhere | **CONFIRMS** |
| §2's sentence that `subagent_type` is **absent** from a nested spawn's `tool_input` | **CONTRADICTS** — see §18.5 |

### 18.3 `PreToolUse` on a subagent spawn — depth 0

Probe A. Captured by all three registrations simultaneously and byte-identical in each; this
is the `matcher:"Agent"` copy.

```json
{"session_id":"749698d7-325b-4454-9d09-e0b4d32e404b","transcript_path":"…","cwd":"/private/tmp/claude-501/-Users-peterho-Repos-claude-governor/8a1d8b20-3634-4420-8c91-f08a631bde9b/scratchpad/surface-2.1.270/A/work","prompt_id":"92c8316d-a1ca-4208-84b5-eb71cadf92e8","permission_mode":"bypassPermissions","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"description":"Count README.md lines","prompt":"Run wc -l < README.md in the current working directory and report only the number.","subagent_type":"general-purpose","run_in_background":false},"tool_use_id":"toolu_017q5Pt13qKFFGqPLe3pQgEf"}
```

`tool_name` is still `Agent`, `tool_input` still has exactly `description`, `prompt`,
`subagent_type`, `run_in_background`, there is still no depth field, and `agent_id` is still
absent at the top level.

**The control that makes this trustworthy, re-run.** In the same session `Bash` (twice),
`Read`, `ToolSearch`, `SendMessage` and `ScheduleWakeup` fired the catch-all and **neither**
matched registration. Each matched registration fired exactly once, at the spawn. From
`index.log`, t relative to `SessionStart`:

```
t=  0.000  sessstart        SessionStart
t=  5.723  pre-AGENTMATCH   PreToolUse   Agent
t=  5.723  pre-any          PreToolUse   Agent
t=  5.723  pre-TASKMATCH    PreToolUse   Agent
t=  5.767  substart         SubagentStart              agent a62fad8018117fb5d
t=  8.392  pre-any          PreToolUse   Bash          agent a62fad8018117fb5d
t=  8.492  post-any         PostToolUse  Bash          agent a62fad8018117fb5d
t=  9.711  substop          SubagentStop               agent a62fad8018117fb5d
t=  9.753  post-any         PostToolUse  Agent
t= 12.264  pre-any          PreToolUse   ToolSearch
t= 14.887  pre-any          PreToolUse   SendMessage   to=a62fad8018117fb5d
t= 14.938  post-any         PostToolUse  SendMessage   resumedAgentId=a62fad8018117fb5d
t= 14.939  substart         SubagentStart              agent a62fad8018117fb5d   ← the resume
t= 18.045  pre-any          PreToolUse   Read          agent a62fad8018117fb5d
t= 19.877  substop          SubagentStop               agent a62fad8018117fb5d
t= 21.424  pre-any          PreToolUse   ScheduleWakeup
t= 23.860  pre-any          PreToolUse   ScheduleWakeup
t= 24.026  pre-any          PreToolUse   Bash                                    ← the control
t= 25.643  sessend          SessionEnd
```

Matchers are therefore still genuinely applied on 2.1.270, and `Task` is still carried as an
alias for the same tool.

### 18.4 `PreToolUse` on a subagent spawn — nested (depth 1)

Probe C. `subagent_stats` reported `max_depth: 2`, `spawned_by_subagents: 1`,
`refused.depth_limit: 0`, so §9 also still holds: the harness does not refuse a nested spawn.

```json
{"session_id":"2a845b48-efec-4a2d-bdd8-6f8842d59af7","transcript_path":"…","cwd":"/private/tmp/claude-501/-Users-peterho-Repos-claude-governor/8a1d8b20-3634-4420-8c91-f08a631bde9b/scratchpad/surface-2.1.270/C/work","prompt_id":"6353352e-35e0-4d7e-a8ec-aab082979832","permission_mode":"bypassPermissions","agent_id":"a2d87c696ee916652","agent_type":"general-purpose","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"description":"Count lines in README.md","prompt":"Run the following shell command in the current working directory: wc -l < README.md\n\nReport back only the resulting number (the line count).","subagent_type":"general-purpose","run_in_background":false},"tool_use_id":"toolu_01MWf5bfYPVWQmcZQpGnsm7H"}
```

The `agent_id` here, `a2d87c696ee916652`, is the agent from probe C's **first**
`SubagentStart`; the agent this call creates is `a75fb1c1107d80b68`. So §2's rule is intact
verbatim: **`agent_id` absent ⇒ the caller is the main session; `agent_id` present ⇒ the caller
is itself a subagent, and the value names the parent.** There is still no `depth` field.

An ordinary tool inside a subagent carries the same marker — here the `Bash` at t=8.392 in
probe A:

```json
{"session_id":"749698d7-325b-4454-9d09-e0b4d32e404b","transcript_path":"…","cwd":"…/A/work","prompt_id":"92c8316d-a1ca-4208-84b5-eb71cadf92e8","permission_mode":"bypassPermissions","agent_id":"a62fad8018117fb5d","agent_type":"general-purpose","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"wc -l < README.md","description":"Count lines in README.md"},"tool_use_id":"toolu_01NLa3C52U42tbYCS9sJHjHq"}
```

### 18.5 CONTRADICTION — `subagent_type` is present on a nested spawn

Section 2 says, of the nested `PreToolUse`, "note that `subagent_type` is **absent** from
`tool_input` here". On 2.1.270 it is **present**: `"subagent_type":"general-purpose"`, in the
payload above.

Both observations are **n = 1** — one nested spawn on 2.1.263, one on 2.1.270 — and the field
is written by the model, not the harness, so this may be prompt variation rather than a
version change. The honest reading is that a nested spawn's `tool_input` **may or may not**
carry `subagent_type`, and that neither run establishes which.

**It is not load-bearing.** Depth comes from the top-level `agent_id`, which §18.4 re-confirms,
and nothing in the gate reads `subagent_type` to decide depth. The reason to record it is that
§2's sentence as written is false for this sample, and a future reader must not build on it.

### 18.6 `SubagentStart` and `SubagentStop`

`SubagentStart`, probe A, the spawn:

```json
{"session_id":"749698d7-325b-4454-9d09-e0b4d32e404b","transcript_path":"…","cwd":"…/A/work","prompt_id":"92c8316d-a1ca-4208-84b5-eb71cadf92e8","agent_id":"a62fad8018117fb5d","agent_type":"general-purpose","hook_event_name":"SubagentStart"}
```

The same fields as §3, no more and no fewer, and **still no `tool_use_id`** — there remains
nothing on the wire that binds a `SubagentStart` to the `PreToolUse` that caused it, only
`session_id` and `prompt_id`, which every spawn in the turn shares. Still no `permission_mode`
and no `effort`, per §11.

The `SubagentStart` raised by the resume at t=14.939 was identical in shape and carried the
same `agent_id`, so §8 holds on 2.1.270: **`SubagentStart` is a start-or-resume signal and
confirm must stay idempotent per `agent_id`.**

`SubagentStop`, probe A, the first one:

```json
{"session_id":"749698d7-325b-4454-9d09-e0b4d32e404b","transcript_path":"…","cwd":"…/A/work","prompt_id":"92c8316d-a1ca-4208-84b5-eb71cadf92e8","permission_mode":"bypassPermissions","agent_id":"a62fad8018117fb5d","agent_type":"general-purpose","effort":{"level":"high"},"hook_event_name":"SubagentStop","stop_hook_active":false,"agent_transcript_path":"/Users/peterho/.claude/projects/…/subagents/agent-a62fad8018117fb5d.jsonl","last_assistant_message":"3","background_tasks":[],"session_crons":[]}
```

The second one, after the resume, again reported the agent as still running in its own
payload, exactly as §8 recorded:

```json
"background_tasks":[{"id":"a62fad8018117fb5d","type":"subagent","status":"running","description":"Count README.md lines","agent_type":"general-purpose"}]
```

`agent_type` was populated on every `SubagentStop` here. The empty-value third state of §4's
GOV-02 addendum and §8.2 did not arise, because no away-summary context fired in these runs —
that is absence of the occasion, not evidence against it.

### 18.7 The `SendMessage` resume — `to` is unchanged, `tool_input` has grown

```json
{"session_id":"749698d7-325b-4454-9d09-e0b4d32e404b","transcript_path":"…","cwd":"…/A/work","prompt_id":"92c8316d-a1ca-4208-84b5-eb71cadf92e8","permission_mode":"bypassPermissions","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"SendMessage","tool_input":{"to":"a62fad8018117fb5d","summary":"Ask for first line of README.md","message":"Please report the FIRST line of README.md in the current working directory.","type":"message","recipient":"a62fad8018117fb5d","content":"Please report the FIRST line of README.md in the …"},"tool_use_id":"toolu_01TzKBtScduZAudoa7rBKvP1"}
```

`to` is `a62fad8018117fb5d` — seventeen characters, all lowercase hex, the shape §8.3 makes
the gate's discriminator. **Unchanged.** The resume is real: `PostToolUse` reported
`resumedAgentId`, a `SubagentStart` followed 1 ms later, and the resumed agent then ran a
`Read` under its own `agent_id`.

**Two additions, and one of them is a trap.** Relative to §8.3's payload, `tool_input` has
gained:

| Key | Value here | Note |
|---|---|---|
| `type` | `"message"` | new |
| `content` | `"Please report the FIRST line of README.md in the …"` | new, and **truncated** — a clipped copy of `message` ending in a literal `…` |

The `…` above is not this file abbreviating. It is in the bytes the hook received.
`recipient` still duplicates `to`, as in §8.3.

**The gate must keep reading `to`, and must never read `content`.** `content` is lossy by
construction, so anything parsed out of it is parsed out of a string the harness has already
cut. Nothing in the governor reads it today; this is written down so nothing starts to.

`PostToolUse` on `SendMessage` has likewise gained a `pin` object:

```json
"tool_response":{"success":true,"message":"Resuming agent a62fad8","resumedAgentId":"a62fad8018117fb5d","pin":{"id":"a62fad8018117fb5d","name":"a62fad8018117fb5d","ref":"8b5d23"}}
```

`resumedAgentId` — the field §8 and §8.3 use as ground truth — is unchanged.

### 18.8 Timeout — a configured value is still honoured, in seconds

Probe B. One `PreToolUse` hook on `Bash` with `"timeout": 5`, ticking a log once a second and
sleeping 30 s:

```
t5 hook-start 1789307093.343032
t5 tick 1 1789307094.395980
t5 tick 2 1789307095.440873
t5 tick 3 1789307096.510382
t5 tick 4 1789307097.554150        ← last tick; tick 5 would have been ~1789307098.6
```

Killed between 4.2 s and 5.2 s after start. The unit is seconds and the configured value is
enforced to about a second, as §10.1 found for `timeout: 120`. The tool then proceeded
normally — the run returned `hello-timeout-probe` with `duration_ms: 7238` — so a hook killed
at its timeout **fails open**.

The unset-`timeout` ceiling was deliberately not re-derived; §10.1's 150 s finding stands
untested on 2.1.270.

### 18.9 `ScheduleWakeup` — a tool name this file has not recorded

Observation only. Probe A's main session called a tool named `ScheduleWakeup` twice:

```json
{"session_id":"749698d7-325b-4454-9d09-e0b4d32e404b","transcript_path":"…","cwd":"…/A/work","prompt_id":"92c8316d-a1ca-4208-84b5-eb71cadf92e8","permission_mode":"bypassPermissions","effort":{"level":"high"},"hook_event_name":"PreToolUse","tool_name":"ScheduleWakeup","tool_input":{"delaySeconds":60,"reason":"Waiting for subagent's reply about README.md first line before proceeding to step 3","prompt":"Check if the subagent has replied with the first line of README.md; if so proceed to step 3 (run echo control-bash-in-main-session) and reply with both results.","noop":true},"tool_use_id":"toolu_01FunwXBzRcSPnnKaCxWMv4E"}
```

It matched the catch-all and matched neither `Agent` nor `Task`, it carried no `agent_id`, and
`subagent_stats.spawned` stayed at 1 across the whole run. **It spawns nothing and it is not a
gate concern.** It is recorded because a catch-all registration will now see it, and because
§8.3's table enumerates spawn paths by what raises a `PreToolUse` — this raises one and belongs
to none of them.

### 18.10 Still unmeasured

Named rather than inferred, because a gap filled by inference is how §8.1 and §8.2 went wrong:

- The **unset-`timeout` ceiling**. §10.1 probed 150 s on 2.1.263 and found no cut; nothing was
  probed here.
- **§10.2 — a decision emitted after a long wait**, and the `permissionDecision: "deny"`
  output contract with it. No deny was emitted in any probe.
- **FIFO correlation under fan-out** (§7). Every spawn here was singular; `PreToolUse` and
  `SubagentStart` interleaved at +44 ms and +45 ms, which is consistent with §7 but is not a
  burst.
- **Background spawns.** Every spawn measured was foreground (`requested.background: 0`), so
  the `async_launched` shape of §6 and §17.4 was not re-observed.
- **The `Skill` path** (§8.3 path 3).
- **The away-summary path** (§8.3 path 4) and the empty-`agent_type` discriminator of §8.2.
- **Whether `permission_denials` still spells the tool `Task`** (§1's second payload). No probe
  produced a denial.
- **Interactive, non-headless sessions.** All three probes were `claude -p`.
- **End to end against a running daemon** (§8.4). These probes captured payloads; no lease was
  taken.
- **Linux.** Every measurement in this section is macOS.
