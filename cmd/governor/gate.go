package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/classify"
	"github.com/peter3605/claude-governor/internal/identity"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The gate (GOV-05 and GOV-07, design.md section 4).
//
// This file is invariant 1: the gate fails open. It runs as the PreToolUse hook on every
// subagent spawn, inside a hook Claude Code is blocked on, so every path through it where
// the daemon does not give a clear answer ends in exit 0 with empty stdout within the
// budget below — no socket, no listener, a listener that accepts and never answers, a
// listener that answers with rubbish, a daemon speaking a protocol version this binary does
// not know. A concurrency limiter that fails badly is worse than no limiter at all, because
// it fails silently and the user blames Claude Code.
//
// GOV-07 added the one path that is not silent: a daemon that clearly refuses. That is the
// whole of the enforcement, and it is deliberately the narrowest possible opening in
// invariant 1 — a refusal is honoured only when the daemon said so in a reason this binary
// recognises. Everything else, including a refusal this version cannot interpret, still
// fails open.
//
// GOV-08 added the wait and the depth limit. The wait is the part that had to be built
// carefully, because the obvious implementation breaks invariant 1: extending the read
// deadline to wait_max whenever the gate *asks* for a wait means a daemon that accepts and
// never answers stalls every spawn for twenty seconds, which is precisely the hang this
// file exists to prevent. So the extension is licensed by evidence instead. The daemon that
// parks a request says so on the socket immediately, and only that line — arriving inside
// the unchanged 100 ms allowance — buys the longer read. A wedged daemon never sends it and
// fails open on the same ceiling it always did.

// gateBudget is the whole wall-clock allowance for one gate invocation, covering reading
// stdin, connecting, writing and reading the reply. design.md section 4.4 sets the connect
// deadline at 100 ms; this applies the same number to the invocation end to end, because
// the session is blocked for the total, not for any one step of it.
//
// A var, not a const, so tests can shrink it. Nothing else may write it.
var gateBudget = 100 * time.Millisecond

// ioMargin is how far inside the budget the socket deadline sits.
//
// Without it the connection deadline and the outer backstop expire at the same instant and
// race, and which one reports the failure is a coin toss. That matters more than tidiness:
// if the read deadline were ever dropped, a suite that accepts either mechanism would still
// pass, on the backstop, and the trap this story exists to avoid would be back. So the
// socket deadline fires first and by design, and the select in withBudget is left as a true
// last resort for the failures no socket deadline can see — chiefly a read of stdin that
// never returns, which happens before there is a socket at all.
const ioMargin = 10 * time.Millisecond

// gateWaitMax is how long the gate will let the daemon hold a spawn before it insists on an
// answer (design.md section 4.2). A stall costs no tokens and the model never has to reason
// about it, so waiting is preferred over denying wherever the wait fits.
//
// 20 s is measured-safe rather than chosen: GOV-01 held a hook for 150 s uncut and had a
// deny honoured at 75 s, and design.md section 4.1 sets the gate hook's own timeout to 30 s
// explicitly so this sits inside a number that was measured rather than inherited.
//
// A var, not a const, so tests can shrink it. Nothing else may write it.
var gateWaitMax = 20 * time.Second

// waitGrace is how far past its own promised deadline the gate lets the daemon be.
//
// The daemon answers a parked acquire at wait_ms whatever happens, so this covers only the
// scheduling and the write, not any part of the wait itself. It is generous because the
// alternative to reading the answer is failing open on a spawn the daemon has already
// decided about.
const waitGrace = 500 * time.Millisecond

// maxPayload bounds the hook payload read from stdin. The payload carries the agent's
// prompt, so it is not small, but it is not unbounded either; a truncated read fails to
// parse and the gate fails open, which is the right answer to a payload this size.
const maxPayload = 1 << 20

// maxResponse bounds one reply line. A daemon that streams without ever sending a newline
// is a hang, and the read deadline alone would not stop it from filling memory first.
const maxResponse = 64 << 10

// principal identifies who is asking. It is single-user in v1 and the local budget ignores
// it, but it is no longer true that nothing reads it: it is handed to every budget source,
// because a source governing an account's tokens or spend cannot answer without it (GOV-26).
//
// It is carried on the wire because retrofitting identity into an already-shipped format is
// far more expensive than carrying a field nobody uses, and this is the cheapest moment it
// will ever cost (design.md section 7).
//
// An ALIAS, not a declaration: internal/budget names this type in the Source interface, and
// internal/identity is where it lives so that a package below cmd/governor can name it too.
// Aliasing rather than converting at the boundary means there is one definition of the wire
// shape and no conversion function to forget to update.
type principal = identity.Principal

// acquireRequest is the acquire message of design.md section 7.
type acquireRequest struct {
	V         int       `json:"v"`
	Op        string    `json:"op"`
	Principal principal `json:"principal"`
	Session   string    `json:"session"`
	ToolUseID string    `json:"tool_use_id"`
	Depth     int       `json:"depth"`
	WaitMS    int       `json:"wait_ms"`
	// Tool is the tool_name the harness actually put on this PreToolUse payload, passed
	// through so the daemon can record what the matcher really caught (GOV-30, surface.go).
	//
	// Additive, and the protocol version is NOT bumped for it, exactly as GOV-12's and
	// GOV-29's status fields were not: an older daemon ignores a key it does not know, and a
	// newer daemon reading an older gate's request simply gets "" and records nothing. Both
	// halves of a mixed-version machine keep governing spawns, which is the property that
	// matters; the ledger merely stays empty and doctor reports the surface as unconfirmed,
	// which is true. A bump would instead make every un-upgraded gate fail open.
	Tool string `json:"tool,omitempty"`
	// Kind, Class and Weight say what is being asked for and how big it is: "agent" or
	// "build", the cost class for a build, and the weight in slots.
	//
	// Additive for the same reason Tool is, and with the same consequence for a mixed-version
	// machine: an older daemon ignores them and treats every acquire as a whole-slot agent
	// lease, which over-counts a `Bash` call rather than under-counting it. That is the safe
	// direction — the machine is governed more tightly than intended for the length of an
	// upgrade, not less — and it is why this does not bump the protocol version, which would
	// instead make every un-upgraded gate fail open and govern nothing at all.
	// The wire key is "slots", not "weight": `weight` is already taken on this connection
	// by the session operation's fair-share weight (design.md section 6.1), and one struct
	// decodes every operation. Two meanings on one key is how a float quietly fails to
	// parse into an int and every build silently weighs nothing.
	Kind  string  `json:"kind,omitempty"`
	Class string  `json:"class,omitempty"`
	Slots float64 `json:"slots,omitempty"`
}

// acquireResponse is the granted and refused replies of design.md section 7, decoded into
// one struct. The refusal's fields are what let the deny name a cause rather than a number.
type acquireResponse struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Lease  string `json:"lease"`
	Reason string `json:"reason"`
	Budget int    `json:"budget"`
	InUse  int    `json:"in_use"`
	// Slots is how much of the budget is held. It is what the refusal text names, because
	// InUse counts leases and the budget counts slots, and four agents reading "4/6 slots"
	// on a machine that is 5% full is a number that invites exactly the wrong response.
	Slots float64 `json:"slots"`
	// CPU is a pointer because the daemon has no sampler until GOV-12 and sends null
	// rather than a fabricated 0.0, which would read as an idle machine. A deny must not
	// claim a CPU figure it was not given.
	CPU     *float64        `json:"cpu"`
	Thermal signals.Thermal `json:"thermal"`
	// WaitMS is set only on the interim "waiting" line, and is what the daemon has
	// committed to answering within.
	WaitMS int `json:"wait_ms"`
	// Denials is how many times this session has been refused inside the daemon's window,
	// counting this one. It is what the reason text escalates on.
	Denials int `json:"denials"`
	// Cause says which half of the admission rule refused (GOV-18): "share" when the machine
	// had room that belongs to another session, "capacity" when it was genuinely full. It is
	// deliberately NOT named `share` — a grant sends `"share": 2` as an integer into this
	// same struct, and a bool of that name would make every grant fail to decode. Absent from
	// an older daemon, which decodes as "" and prints the sentence it always printed.
	Cause string `json:"cause"`
	// Asking is how much budget the refused request wanted, in slots, and it is the number
	// whose absence let the gate print "Machine at capacity (1/4 build slots)" on
	// 2026-09-21. See refuseReply.Asking. Zero from a daemon too old to send it, and every
	// clause that quotes it is conditional on that, for the same reason CPU is.
	Asking float64 `json:"asking"`
	// Ceiling is the budget this host would allow with nothing cutting it and Cut names the
	// signal doing the cutting, or "" when nothing is. Both are what let a refusal quoting
	// three slots on a six-slot machine say WHY it is quoting three.
	Ceiling int    `json:"ceiling"`
	Cut     string `json:"cut"`
}

// hookPayload is the PreToolUse payload, measured in GOV-01 and recorded in
// docs/hook-surface.md. Decoded loosely: a harness upgrade that adds or renames a field
// must degrade to a thinner request, never to a failed hook.
type hookPayload struct {
	SessionID string `json:"session_id"`
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	// CWD is the session's working directory, present on a Bash PreToolUse payload
	// (docs/hook-surface.md section 2, measured). It is read for one purpose — attributing a
	// recorded build to a project — and is never sent to the daemon.
	CWD string `json:"cwd"`
	// AgentID is the *caller*, not the agent about to be created. Absent at top level means
	// the main session is spawning (depth 0); present means a subagent is (depth >= 1).
	// It is the only depth signal the surface carries — there is no depth field
	// (docs/hook-surface.md section 2).
	AgentID string `json:"agent_id"`
	// ToolInput is decoded for two fields only. `to` is what separates a SendMessage that
	// resumes a subagent from one that messages a teammate or another session
	// (docs/hook-surface.md section 8.3). `command` is the Bash command text, which is the
	// sole input to the classifier. Nothing else here is read: tool_input also carries the
	// prompt and the message body, and this process has no business with either.
	ToolInput struct {
		To      string `json:"to"`
		Command string `json:"command"`
	} `json:"tool_input"`
}

// agentIDShape is the shape of a subagent id on 2.1.263: seventeen lowercase hex characters,
// measured in docs/hook-surface.md sections 2 and 8.3.
//
// It is used for exactly one decision — whether a SendMessage is resuming a subagent — and
// it is deliberately the strict form. SendMessage also addresses teammates, other local
// sessions and cloud sessions by NAME, and taking a lease for one of those would be a slot
// consumed by something that is not a subagent and never returned, because no SubagentStop
// follows it. An unrecognised recipient is left ungoverned, which is the status quo and the
// side to fail on.
var agentIDShape = regexp.MustCompile(`^[0-9a-f]{17}$`)

// spawnToolNames are the tool names that can put a subagent on the CPU, and therefore the
// names a PreToolUse matcher has to cover for the gate to fire at all.
//
// It is a list rather than a copy of governsSpawn's switch because the two answer different
// questions and only one of them is about matchers. governsSpawn decides what happens once
// a payload has arrived — for SendMessage that depends on the recipient — whereas a matcher
// is compared against the tool name alone, before any payload exists. `governor doctor`
// reads this list to check the registered matcher (GOV-20); TestEverySpawnToolNameIsGoverned
// binds the two together, so a name added to one and not the other is a red test rather than
// a matcher that silently stops covering a spawn path.
var spawnToolNames = []string{"Agent", "Task", "SendMessage"}

// buildToolName is the tool that runs build-shaped work, and the second thing the gate
// governs (GOV-15, design.md section 5.5).
//
// It is gated by COMMAND rather than by name: `Bash` fires 184-2,221 times a day against the
// spawn path's 0-37, so leasing every call would be absurd and the predicate is the whole
// design. internal/classify decides, gate-side, before any I/O.
const buildToolName = "Bash"

// gatedToolNames is what the PreToolUse matcher has to cover: every spawn name, plus Bash.
//
// It is a superset of spawnToolNames rather than a replacement, because the two lists answer
// different questions. This one is "what must reach this binary at all"; spawnToolNames is
// "which of those, once here, is a subagent spawn". A name that reaches the gate and turns
// out to be neither is simply dropped.
var gatedToolNames = append(append([]string{}, spawnToolNames...), buildToolName)

// spawnConfirmingTools are the names whose arrival PROVES the spawn path is still matched.
//
// It is a subset of spawnToolNames, and the difference between the two lists is the whole
// point. `Agent` and `Task` are two spellings of the tool that STARTS a subagent. SendMessage
// RESUMES one that already exists (docs/hook-surface.md section 8.3, path 2) — a real spawn
// worth governing, but not evidence about the path that creates them.
//
// GOV-30's first cut confirmed the surface on any sighting at all, and an adversarial review
// found the hole: rename the spawn tool and `Agent` stops matching, so every fresh spawn goes
// ungoverned — while `SendMessage` keeps matching, keeps reaching the gate, and keeps landing
// sightings on the new version. Doctor would print ok on the strength of a tool that never
// broke, and the silence that was supposed to be the signal would be covered. So the ledger's
// question is narrower than "did anything fire": it is "did the thing that starts subagents
// fire". TestASendMessageSightingDoesNotConfirmTheSpawnPath is that hole, held open.
var spawnConfirmingTools = []string{"Agent", "Task"}

// matchedSpawnTool reports whether a tool name is one the gate's matcher is built from.
func matchedSpawnTool(tool string) bool {
	for _, n := range spawnToolNames {
		if n == tool {
			return true
		}
	}
	return false
}

// confirmsSpawnPath reports whether an observed tool name proves the spawn path is matched.
func confirmsSpawnPath(tool string) bool {
	for _, n := range spawnConfirmingTools {
		if n == tool {
			return true
		}
	}
	return false
}

// spawnToolNamesConfirmedAgainst is the Claude Code version the list above was last MEASURED
// against, not merely believed correct for.
//
// It exists because `governor doctor`'s matcher check cannot, on its own, catch the failure
// it was written for. That check reads the matcher off disk and tests it against the list
// above, which catches a hand-edited or stale matcher — but install writes the matcher FROM
// that same list, so on a stock install the two agree by construction. If Claude Code renamed
// the spawn tool tomorrow, both sides would still agree, doctor would print ok, and not one
// spawn would be governed: exactly the silent failure design.md section 10.1 was spent
// measuring the tool name to avoid.
//
// A version stamp cannot detect a rename either. What it can do is stop the claim from being
// unfalsifiable: it turns "the matcher is correct" into "the matcher was correct as of
// 2.1.270", which is a statement that expires and can be re-earned by re-running the probe in
// docs/hook-surface.md section 12. GOV-28 and GOV-29 between them changed the answer twice in
// four patch versions, so it does expire.
//
// GOV-30 TOOK THE OTHER HALF OF THE PROBLEM AWAY FROM IT. This constant is now the stamp on
// the DOCUMENTED measurement and nothing else: doctor no longer decides anything by comparing
// it to the running version, because that comparison could only ever say "the measurement is
// old", never "the matcher stopped firing". Doctor asks the ledger in surface.go instead,
// which is built from tool names the harness actually sent. What this constant still does is
// tie the repo's written record to a version, and TestRecordedSurfaceMatchesTheMatcher holds
// it to docs/hook-surface.md so the two cannot drift.
const spawnToolNamesConfirmedAgainst = "2.1.270"

// governsSpawn reports whether this payload puts a subagent on the CPU, and if not, why.
//
// The set of paths is the one GOV-28 measured rather than the one the tool names suggest
// (docs/hook-surface.md section 8.3):
//
//	Agent, Task    a spawn. The tool answers to both names within one version.
//	""             a payload shape this binary does not recognise. Governed deliberately:
//	               guessing "not an agent" after a harness rename stops governing silently.
//	SendMessage    a resume IF `to` names an agent; otherwise an ordinary message.
//	anything else  not a spawn. Skill in particular runs INLINE in the calling session and
//	               spawns nothing at all, which is what GOV-28 established.
func governsSpawn(p hookPayload) (bool, string) {
	switch p.ToolName {
	case "Agent", "Task", "":
		return true, ""
	case "SendMessage":
		if agentIDShape.MatchString(p.ToolInput.To) {
			return true, ""
		}
		return false, fmt.Sprintf("SendMessage to %q is not a subagent resume", p.ToolInput.To)
	default:
		return false, fmt.Sprintf("not a subagent spawn (tool_name %q)", p.ToolName)
	}
}

// intent is what the gate has decided to ask the daemon for, or that it will ask for nothing.
type intent struct {
	// take reports whether to contact the daemon at all. When false, why says what was
	// decided and the gate returns having performed NO I/O.
	take bool
	why  string
	kind string
	// weight is how much of the budget to ask for, in slots (internal/budget).
	weight float64
	// class is the cost class of a build, for the daemon's log. Empty for a spawn.
	class string
	// shapes names the gated invocations, for the build record. Empty for a spawn.
	//
	// Carried here rather than re-derived, because the alternative is calling the classifier
	// a second time on the hot path to recover something it already computed.
	shapes []string
}

// decide works out what this payload is worth, WITHOUT touching the network.
//
// THE ORDERING IS THE POINT AND IT IS CLAUSE 1 OF GOV-15. `Bash` fires 184-2,221 times a day
// and 81.6% of those calls are not build-shaped, so the cheap answer has to be reached
// before a socket is opened. The obvious implementation — send everything to the daemon and
// let it answer "not gated" — is a unix round trip on `cat`, and at this call rate it spends
// the entire latency budget of design.md section 4.4 arriving at "no". internal/classify is
// a pure function costing ~5 us with no I/O and no locks precisely so that this function can
// be the whole of the decision for four calls in five.
func decide(p hookPayload) intent {
	if p.ToolName == buildToolName {
		r := classify.Classify(p.ToolInput.Command)
		if !r.Gated {
			return intent{why: "not build-shaped"}
		}
		return intent{
			take:   true,
			kind:   "build",
			class:  r.Class.String(),
			shapes: r.Shapes,
			weight: budget.SlotsFor(r.Class.ExpectedPeakCores()),
		}
	}
	if ok, why := governsSpawn(p); !ok {
		return intent{why: why}
	}
	// An agent spawn takes a lease WEIGHTED at its measured cost rather than a whole slot.
	// See internal/budget/weight.go for why this was decided rather than dropping agent
	// leases outright: the short version is that +0.098 cores is small but real, and a
	// budget that can express both sizes governs both.
	return intent{take: true, kind: "agent", weight: budget.AgentWeight}
}

// denyOutput is the PreToolUse decision Claude Code reads from stdout, measured verbatim in
// GOV-01 (docs/hook-surface.md section 10.2).
//
// permissionDecision is only ever "deny" here. "allow" must never be emitted: it suppresses
// the user's own permission prompts as a side effect, which is a security regression traded
// for nothing. The grant path is exit 0 with empty stdout — silence means "no opinion".
type denyOutput struct {
	HookSpecificOutput denyBody `json:"hookSpecificOutput"`
}

type denyBody struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// refusalsThisVersionHonours are the daemon reasons that become a deny.
//
// The set is closed on purpose. A refusal whose reason this binary does not recognise is
// treated exactly like an unreachable daemon, because during an upgrade there will be a
// daemon of one version and gates of another, and the posture in that window is settled:
// ungoverned spawns are acceptable, a wrongly blocked one is not.
//
// "waiting" is deliberately absent. It is not a refusal at all — it is the daemon saying it
// has parked the request — and it is handled before this map is consulted. A daemon that
// sent it as a final answer would be treated as unreachable, which is the correct reading
// of a promise that was never kept.
var refusalsThisVersionHonours = map[string]bool{"capacity": true, "depth": true}

// waitingReason is the interim line's reason. Seeing it is the only thing that lets the
// gate spend longer than design.md section 4.4's allowance.
const waitingReason = "waiting"

func runGate(_ []string, stdout, stderr io.Writer) int {
	return gate(os.Stdin, stdout, stderr, transport.Dial{}, gateBudget, protocolVersion)
}

// gate is the whole hook body. It returns the process exit code, which is always 0 — even
// for a refusal, which is carried on stdout rather than in the exit status
// (docs/hook-surface.md section 10.2).
// speaks is the protocol version this gate talks, threaded rather than read from the
// constant so a test can run a gate of one version against a daemon of another — which is
// what every upgrade is, and the one path that must fail open rather than stall.
func gate(stdin io.Reader, stdout, stderr io.Writer, dialer transport.Dialer, budget time.Duration, speaks int) int {
	return withBudget(stderr, budget, func(diag io.Writer, clock *budgetClock) {
		gateOnce(stdin, stdout, diag, dialer, clock, speaks)
	})
}

// gateOnce asks the daemon for a lease and acts on the answer. Every error it can produce
// means the same thing to the caller — fail open — so they are reported to stderr and
// otherwise swallowed.
func gateOnce(stdin io.Reader, stdout, diag io.Writer, dialer transport.Dialer, clock *budgetClock, speaks int) {
	var p hookPayload
	if !decodePayload(stdin, diag, &p) {
		return
	}

	// The hook matcher is configuration and configuration drifts, so the payload is checked
	// here too and a lease is taken for nothing the binary does not recognise as worth one.
	//
	// Nothing above this line has touched the network, and for a cheap `Bash` command
	// nothing below it will either — the function returns here having opened no socket and
	// resolved no endpoint.
	in := decide(p)
	if !in.take {
		fmt.Fprintf(diag, "governor gate: %s\n", in.why)
		return
	}

	// The instrument for GOV-17 clauses 2 and 3. Only a gated BUILD is recorded: an agent
	// spawn is already fully described by daemon.log, and a command that was never gated
	// returned above having done nothing at all, which is the property the cheap path exists
	// to have.
	//
	// The defer is registered here, before the first I/O, so that every exit below records
	// something — and it runs last, after any deny has reached stdout. buildlog.go's write()
	// explains why that ordering is a fail-open argument rather than a stylistic one.
	var rec *buildRecorder
	if in.kind == "build" {
		rec = newBuildRecorder(p, in)
		defer func() { rec.write(diag) }()
	}

	conn, replies, err := dialAndSend(dialer, clock.Deadline(), acquireRequestFor(p, in, speaks))
	if err != nil {
		fmt.Fprintf(diag, "governor gate: %v\n", err)
		rec.failOpen("daemon unreachable")
		if errors.Is(err, errNoDaemon) {
			// Nothing is listening. This spawn is already ungoverned — the decision was
			// made the instant the dial failed — so the start is for the next one, and
			// nothing below waits for it (GOV-11).
			lazilyStartDaemon(diag)
		}
		return
	}
	defer conn.Close()

	var resp acquireResponse
	if err := readLineInto(replies, &resp); err != nil {
		fmt.Fprintf(diag, "governor gate: read response: %v\n", err)
		rec.failOpen("no readable response")
		return
	}

	if resp.V != speaks {
		// A version mismatch is answered, not ignored — but the gate treats the answer
		// exactly as it treats an unreachable daemon. During an upgrade there will be a
		// daemon of one version and gates of another, and ungoverned spawns are acceptable
		// where a stalled session is not.
		fmt.Fprintf(diag, "governor gate: daemon speaks protocol v%d, this binary speaks v%d: failing open\n",
			resp.V, speaks)
		rec.failOpen("protocol version mismatch")
		return
	}

	parkedFor := time.Duration(0)
	if !resp.OK && resp.Reason == waitingReason {
		parkStart := time.Now()
		if !awaitParked(diag, conn, replies, clock, &resp, speaks) {
			rec.failOpen("parked, then no usable answer")
			return
		}
		parkedFor = time.Since(parkStart)
	}

	if resp.OK {
		// The grant. Nothing is written anywhere: exit 0 and an empty stdout *is* the
		// grant, and the lease is released by the release hook or reaped, never by this
		// process (design.md section 4.3).
		fmt.Fprintf(diag, "governor gate: granted lease %s (%d/%d in use)\n",
			resp.Lease, resp.InUse, resp.Budget)
		if parkedFor > 0 {
			rec.parkedThenGranted(parkedFor)
		} else {
			rec.grantedNow()
		}
		return
	}

	if !refusalsThisVersionHonours[resp.Reason] {
		fmt.Fprintf(diag, "governor gate: refused for reason %q, which this binary does not "+
			"recognise: failing open\n", resp.Reason)
		rec.failOpen("unrecognised refusal reason")
		return
	}

	out, err := json.Marshal(denyOutput{denyBody{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: denyReason(resp, in),
	}})
	if err != nil {
		// Failing to encode a refusal means the spawn proceeds. That is the correct way
		// round: the alternative is a session blocked by a bug in this binary.
		fmt.Fprintf(diag, "governor gate: encode deny: %v: failing open\n", err)
		rec.failOpen("could not encode the refusal")
		return
	}
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		fmt.Fprintf(diag, "governor gate: write deny: %v\n", err)
		rec.failOpen("could not write the refusal")
		return
	}
	fmt.Fprintf(diag, "governor gate: refused (%d/%d in use)\n", resp.InUse, resp.Budget)
	rec.denied(resp.Reason)
}

// awaitParked reads the second line of a parked acquire, having first bought the time to do
// it with.
//
// The order here is the whole safety argument, and it is worth being explicit about. The
// deadline is extended only *after* the daemon's interim line has been read, so the licence
// to wait comes from the daemon having answered, not from this binary having asked. Every
// failure that does not involve a live, answering daemon has already fallen open on the
// unchanged 100 ms allowance by the time this function is reached.
//
// The wait is bounded by what the gate is willing to give, not by what the daemon asks for:
// a daemon that answers "waiting, 10 minutes" gets gateWaitMax and no more.
func awaitParked(diag io.Writer, conn net.Conn, replies *bufio.Reader, clock *budgetClock, resp *acquireResponse, speaks int) bool {
	wait := gateWaitMax
	if promised := time.Duration(resp.WaitMS) * time.Millisecond; promised > 0 && promised < wait {
		wait = promised
	}
	fmt.Fprintf(diag, "governor gate: daemon parked this spawn, waiting up to %v\n", wait)

	socketDeadline := time.Now().Add(wait + waitGrace)
	// The backstop stays behind the socket deadline by the same margin it always has, so
	// the socket is still the mechanism and the select is still the last resort.
	clock.ExtendTo(socketDeadline.Add(ioMargin))
	if err := conn.SetDeadline(socketDeadline); err != nil {
		fmt.Fprintf(diag, "governor gate: extend deadline: %v: failing open\n", err)
		return false
	}

	*resp = acquireResponse{}
	if err := readLineInto(replies, resp); err != nil {
		// The daemon promised an answer and did not produce one. That is a broken daemon,
		// and a broken daemon means the spawn proceeds.
		fmt.Fprintf(diag, "governor gate: read parked response: %v: failing open\n", err)
		return false
	}
	if resp.V != speaks {
		fmt.Fprintf(diag, "governor gate: parked reply speaks protocol v%d, this binary speaks v%d: failing open\n",
			resp.V, speaks)
		return false
	}
	if !resp.OK && resp.Reason == waitingReason {
		// Two interim lines is not a protocol this binary speaks, and honouring a second
		// one would let a daemon extend the wait indefinitely one line at a time.
		fmt.Fprintf(diag, "governor gate: daemon parked the request twice: failing open\n")
		return false
	}
	return true
}

// denyReason is the text that reaches the model verbatim (docs/hook-surface.md section
// 10.2), in the shape design.md section 4.2 specifies.
//
// Name the number, name the cause, and give one concrete instruction. A vague refusal
// produces a retry loop, which is the expensive failure here: the model reads "no" as
// "not yet" and spends the budget it was just denied on asking again.
//
// Every clause after the slot count is conditional, because a clause that states a fact the
// daemon did not send is a fabrication. CPU is null until GOV-12 lands the sampler, and
// thermal is "unknown" on Linux by design.
func denyReason(resp acquireResponse, in intent) string {
	if resp.Reason == "depth" {
		// Nothing conditional to say: this refusal does not depend on load, and naming a
		// slot count would invite a retry once the count moved. It never will for this
		// caller (design.md section 6.3).
		return "Nested subagent spawns are refused: this Agent call comes from a subagent, " +
			"and only the main session may fan out. Do this task inline in the agent you " +
			"are already in. Do not retry the Agent call."
	}

	var b strings.Builder
	// WHICH SENTENCE IS TRUE DEPENDS ON WHICH RULE REFUSED (GOV-18, GOV-34). Since fair share
	// landed, a session can be refused while the machine still has free slots — they belong to
	// another session — and saying "Machine at capacity (4/6)" there is a fabrication of
	// exactly the kind section 4.2 forbids, as well as being visibly self-contradicting to the
	// reader. An older daemon does not send the field, which decodes as "", which is the text
	// that binary has always printed.
	//
	// GOV-34 ADDED THE THIRD HEAD, and it is the one the 2026-09-21 incident needed. A
	// capacity refusal is not always a full machine: a Heavy build weighs 3.75 slots, so on a
	// four-slot ceiling it is refused beside a single one-slot lease, and "Machine at capacity
	// (1/4 build slots)" is then a sentence that contradicts itself inside its own
	// parentheses. Held strictly below the budget means the machine is NOT at capacity and the
	// only honest thing to say is that what is left is not enough for this particular command.
	head, why := "Machine at capacity", refusedFull
	switch {
	case resp.Cause == "share":
		head, why = "This session is at its fair share", refusedShare
	case resp.Slots < float64(resp.Budget):
		head, why = "Not enough free build slots", refusedTooBig
	}
	// formatSlots rather than %.1f so a machine holding four whole builds says "4/6" and not
	// "4.0/6": the decimal is there to stop a machine holding only agents rounding to a
	// reassuring zero, not to decorate the common case.
	fmt.Fprintf(&b, "%s (%s/%d build slots held", head, formatSlots(resp.Slots), resp.Budget)
	// The number whose absence made the incident's message unreadable. Conditional like every
	// other clause here: a daemon too old to send it sends zero, and a refusal claiming a
	// command needs no slots would be the fabrication section 4.2 forbids.
	if resp.Asking > 0 {
		fmt.Fprintf(&b, ", this one needs %s", formatSlots(resp.Asking))
	}
	if resp.CPU != nil {
		fmt.Fprintf(&b, ", CPU %.0f%%", *resp.CPU*100)
	}
	if resp.Thermal != "" && resp.Thermal != signals.Unknown {
		fmt.Fprintf(&b, ", thermal: %s", resp.Thermal)
	}
	// A cut budget and a small machine produce the same number, and only this tells them
	// apart. Both fields are required before it is printed: a ceiling with no cause, or a
	// cause with no ceiling, would be half a fact stated as a whole one.
	if resp.Cut != "" && resp.Ceiling > resp.Budget {
		fmt.Fprintf(&b, ", budget cut from %d by %s", resp.Ceiling, resp.Cut)
	}
	b.WriteString("). ")
	if in.kind == "build" {
		b.WriteString(buildEscalation(resp.Denials, why))
	} else {
		b.WriteString(escalation(resp.Denials))
	}
	return b.String()
}

// The three situations a capacity refusal can actually be in, which is one more than the two
// GOV-18 left behind.
//
// They are a type rather than a pair of bools because the three are mutually exclusive and the
// instruction half has to pick exactly one sentence per denial count. Two bools would make the
// fourth combination — at its share AND too big — expressible, and nothing in the admission
// rule can produce it: Refusal reports whichever half refused, not both.
type refusalWhy int

const (
	// refusedFull: every slot is taken. The original case, and the only one whose sentence
	// was ever correct before GOV-34 — it was being printed for the other two as well.
	refusedFull refusalWhy = iota
	// refusedShare: the machine has room and it belongs to other sessions (GOV-18).
	refusedShare
	// refusedTooBig: the machine has room and this command is bigger than what is left
	// (GOV-34). A Heavy build weighs 3.75 slots, so on a four-slot ceiling it does not fit
	// beside a single one-slot lease — the machine is a third full and the command still
	// cannot start.
	refusedTooBig
)

// buildEscalation is the instruction half of a capacity deny for a BUILD, and it has to say
// something completely different from the spawn text below.
//
// THE TWO REFUSALS ARE NOT THE SAME EVENT. Refusing a spawn means "do not delegate, do the
// work here" — the work still happens, inline, and the instruction is to stop asking. Refusing
// a `Bash` means the user's command DID NOT RUN. There is no inline alternative: nothing else
// on the machine can compile that package. So the only correct instruction is to run it again
// shortly, and telling a model to "do this task inline yourself rather than delegating it"
// when it tried to run `cargo build` is advice it cannot act on — it would go looking for
// another way to build, which is worse than waiting.
//
// It also does not escalate into "stop retrying", which the spawn text does. A retry is the
// CORRECT response here and the queue is what makes it cheap: the gate parks a build for up to
// wait_max before ever refusing it, so a refusal already means the machine stayed full for
// twenty seconds. What escalation adds is honesty about how long that has been going on.
// THE INSTRUCTION IS THE SAME FOR ALL THREE CAUSES; ONLY THE FACT DIFFERS (GOV-18, GOV-34).
// "Every slot is taken", "this session has reached its share of them" and "what is left is too
// small for this build" call for the identical response — wait, then run the same command
// again — but only one of them is true at a time, and a refusal that misstates why is one a
// reader stops believing. So the cause swaps the clause that asserts something about the
// machine and leaves the instruction untouched. GOV-34 added the third because the first was
// being printed for it: a session told "other builds are using every slot" against a visible
// count of one in four learns that the gate's reasons are not worth reading.
func buildEscalation(denials int, why refusalWhy) string {
	pick := func(full, share, tooBig string) string {
		switch why {
		case refusedShare:
			return share
		case refusedTooBig:
			return tooBig
		default:
			return full
		}
	}
	switch {
	case denials <= 1:
		return "This command was not run. " + pick(
			"Other builds on this machine are using every slot.",
			"This session is already using its fair share of the build slots, and other "+
				"sessions are using theirs.",
			// NOT "the machine is full", which is the sentence that made the 2026-09-21
			// refusal unbelievable: it is demonstrably not full and the reader can see the
			// count. What is true is that this particular command is larger than the room
			// left, and the instruction — wait, retry the same command — is unchanged.
			"This is a large build and the slots left free are not enough for it.",
		) + " Wait a few seconds and run exactly the same command again; do not look for a " +
			"different way to build, and do not treat this as a build failure."
	case denials == 2:
		return "This command was not run, for the 2nd time. " + pick(
			"The machine has been saturated for a while.",
			"This session has been at its share for a while.",
			"The slots this build needs have not come free for a while.",
		) + " Wait longer before retrying the same command, and do not start any further " +
			"builds or test runs in the meantime."
	default:
		return fmt.Sprintf("This command was not run, for the %dth time. %s Stop starting "+
			"builds, tell the user the machine is contended, and let them decide — do not "+
			"keep retrying.", denials, pick(
			"Something on this machine is holding every build slot.",
			"Other sessions have been holding their share of the build slots.",
			"Something on this machine has been holding the slots this build needs.",
		))
	}
}

// escalation is the instruction half of a capacity deny, firmer each time the same session
// is refused inside the daemon's window.
//
// It escalates because the first text demonstrably does not always work: the failure this
// guards against is a model reading "no" as "not yet" and spending the budget it was just
// denied on asking again. Repeating the identical sentence to a caller that has ignored it
// three times is not a strategy. The count comes from the daemon, which is the only place
// that can see a session's refusals across separate gate processes.
func escalation(denials int) string {
	switch {
	case denials <= 1:
		return "Run this task inline yourself rather than delegating it. " +
			"Do not retry the Agent call."
	case denials == 2:
		return "This is the 2nd refusal for this session. The queue is full, not slow — " +
			"waiting will not help. Run this task inline yourself, now, and do not call " +
			"the Agent tool again this turn."
	default:
		return fmt.Sprintf("This session has been refused %d times. Every retry has cost "+
			"tokens and changed nothing. Stop delegating: do all of the remaining work "+
			"inline in this turn and do not call the Agent tool again.", denials)
	}
}

// acquireRequestFor builds the wire request from the hook payload and the gate's decision.
func acquireRequestFor(p hookPayload, in intent, speaks int) acquireRequest {
	depth := 0
	if p.AgentID != "" {
		depth = 1
	}
	return acquireRequest{
		V:         speaks,
		Op:        "acquire",
		Principal: localPrincipal(),
		Session:   p.SessionID,
		ToolUseID: p.ToolUseID,
		Tool:      p.ToolName,
		Kind:      in.kind,
		Class:     in.class,
		Slots:     in.weight,
		Depth:     depth,
		// What the gate is willing to be held for. The daemon parks the request for at most
		// this long and answers either way; a gate that cannot perform a wait sends zero,
		// which is what GOV-07 did and what an older binary still does.
		WaitMS: int(gateWaitMax / time.Millisecond),
	}
}
