package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The daemon (GOV-06, design.md sections 4.3, 6 and 7).
//
// One process owns the budget for the whole machine. Gates ask it for a lease and it
// answers; it holds no state on disk and survives nothing, which is the correct shape for a
// single host (section 12.2) — a restart loses every lease and the reaper cleans up in
// seconds.
//
// GOV-08 added the wait queue (section 6.2) and the depth limit (section 6.3), so an
// acquire with no room now has three possible answers rather than one: refuse, refuse
// because the caller is itself a subagent, or hold the connection open until a slot frees.
//
// What is deliberately NOT here: the control loop. The budget is a constant in this story
// on purpose, because a moving budget debugged against a moving lease table is two unknowns
// at once (GOV-14 lands AIMD). Max-min fair share (section 6.1) is also still absent: the
// wait queue below decides *order* and does not yet decide who may join it.

// defaultBudget is how many slots the daemon allows until the control loop
// exists.
//
// This number is arbitrary and provisional. It is NOT a measured default and must not be
// shipped as one: `governor calibrate` measures the host and GOV-14's AIMD loop moves the
// value at runtime, and both supersede this. It is a constant here so that the lease table
// can be tested against something that does not move.
const defaultBudget = 4

// reapInterval is how often expired and orphaned leases are collected (design.md 4.3).
const reapInterval = 2 * time.Second

// maxFanOutDepth is the deepest caller the governor will take a lease for (design.md
// section 6.3).
//
// Zero means only the main session may fan out. The wire field is the *caller's* depth,
// derived by the gate from the presence of a top-level agent_id in the PreToolUse payload,
// which is the only depth signal the hook surface carries (docs/hook-surface.md section 2):
// absent means the main session is spawning, present means a subagent is. So there are
// exactly two distinguishable values and this constant picks between them.
//
// The point is the nested deadlock: if a subagent could take a lease, every slot could be
// held by an agent that is itself waiting for a slot to spawn children, and the machine
// deadlocks at full budget with nothing running. Refusing outright is the simplest correct
// answer, and a nested fan-out is rarely what was wanted in the first place.
const maxFanOutDepth = 0

// maxParkedWait caps how long a client can ask to be held for, whatever it sends.
//
// The gate asks for 20 s (design.md section 4.2) and the gate hook's own timeout is 30 s
// (section 4.1). This exists so that a buggy or hostile client cannot pin a connection and
// a queue place for longer than any real gate would.
const maxParkedWait = 30 * time.Second

// connDeadline bounds one client's whole exchange.
//
// The daemon must never accumulate goroutines held open by a client that connected and then
// went away — a hook whose session was killed mid-request does exactly that. It is
// generously larger than the gate's own 100 ms budget: the gate gives up long before this,
// and this exists only to collect what it left behind.
const connDeadline = 5 * time.Second

// request is every request shape in design.md section 7, decoded into one struct.
//
// One loose struct rather than a discriminated union per op, because this is a hot path and
// because a field a given op does not use is simply zero. A harness or client that sends a
// field this binary does not know is ignored, never rejected.
type request struct {
	V         int       `json:"v"`
	Op        string    `json:"op"`
	Principal principal `json:"principal"`
	Session   string    `json:"session"`
	ToolUseID string    `json:"tool_use_id"`
	Depth     int       `json:"depth"`
	WaitMS    int       `json:"wait_ms"`
	// Tool is the tool_name off the PreToolUse payload the gate matched (GOV-30). Recorded,
	// never acted on: nothing in the daemon's decisions may depend on it.
	Tool string `json:"tool"`
	// Kind, Class and Slots are what the gate decided this acquire is worth (GOV-15). An
	// older gate sends none of them, and the zero values are read as a whole-slot agent
	// lease — the safe direction, since it over-counts rather than under-counts.
	Kind  string  `json:"kind"`
	Class string  `json:"class"`
	Slots float64 `json:"slots"`

	// confirm / release
	Lease        string `json:"lease"`
	AgentID      string `json:"agent_id"`
	PID          int    `json:"pid"`
	PIDStartedAt int64  `json:"pid_started_at"`

	// session
	Action string `json:"action"`
	Weight int    `json:"weight"`

	// control (design.md section 9.1, GOV-B01)
	//
	// THE KEYS ARE NEW ONES, AND THEY HAD TO BE. The obvious name for the pinned budget was
	// `slots` and for its verb `action` — and this struct already carries `slots` as a FLOAT
	// (the gate's lease weight) and `action` as the session verb, with a disjoint vocabulary.
	// One decode struct serves every operation on this connection, so a key is only free if no
	// other message uses it with a different type or a different meaning. Section 7 records
	// this trap twice already, for `share` against the grant's integer and for `slots` against
	// `weight`; a third instance would have made every `limit` decode as a lease weight of N
	// slots, which is not an error anything would have reported.
	Control string `json:"control"`
	// Limit is the budget to pin, in slots, for the `limit` verb.
	Limit int `json:"limit"`
	// ForMS is how long the override lasts, in milliseconds. Zero takes the default.
	ForMS int `json:"for_ms"`
}

// The response shapes of design.md section 7, one type each.
//
// One union struct with omitempty everywhere would be less code and worse: it drops a
// status reply's sessions array whenever no session holds a lease, so a consumer has to
// handle both an absent key and an empty one, and it puts fields on confirm and release
// replies that the protocol never specified. The statusline (GOV-10) and doctor (GOV-20)
// read these, so each message says exactly what section 7 says it says.
//
// cpu is a pointer in both places it appears, because 0.0 would tell a statusline the
// machine is idle — a claim — where null says "not measured", which is the truth.
//
// GOV-12 gave the daemon a sampler, so the STATUS reply now carries real numbers whenever a
// tick has landed. The REFUSE reply's cpu is still null, and deliberately so: this story is
// observation only, and a refusal that quoted a live CPU figure would be the first place the
// governor used a measurement to explain a decision it did not make from one. GOV-14 is where
// the budget starts moving with the signal, and that is where refuse should start quoting it.

// grantReply answers a granted acquire.
type grantReply struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Lease  string `json:"lease"`
	Budget int    `json:"budget"`
	InUse  int    `json:"in_use"`
	Share  int    `json:"share"`
	Source string `json:"source"`
}

// refuseReply answers a refused acquire. It carries what the deny reason in GOV-08 will
// need to name a cause rather than just a number.
type refuseReply struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	Budget int    `json:"budget"`
	InUse  int    `json:"in_use"`
	// Slots is how much of the budget is held, which since GOV-15 is NOT the same number as
	// InUse: a subagent weighs about 0.08 slots and a Heavy build 3.75, so a count of leases
	// and a quantity of budget are different things and only one of them belongs beside
	// Budget. Additive, so an older statusline simply renders the count as it always did.
	Slots   float64         `json:"slots"`
	CPU     *float64        `json:"cpu"`
	Thermal signals.Thermal `json:"thermal"`
	Source  string          `json:"source"`
	// Denials is how many times this session has been refused inside denyWindow, counting
	// this one. The gate escalates its reason text on it. It is counted here rather than in
	// the gate because each gate is a separate process that remembers nothing, and this is
	// the only place that can see a session's refusals across all of them.
	Denials int `json:"denials"`
	// Cause distinguishes WHICH half of the admission rule refused (GOV-18): "capacity" when
	// the machine itself is full, "share" when it had room that belongs to another session.
	//
	// The `reason` stays "capacity" deliberately. It is the closed set the gate honours
	// (section 4.2), and a new reason value would be one an older gate cannot interpret —
	// which it must then treat as an unreachable daemon and fail open, silently ungoverning
	// every share refusal for the length of an upgrade. This is additive instead: an older
	// gate ignores it and prints what it always printed.
	//
	// IT IS NOT CALLED `share`, AND THAT IS NOT A STYLE CHOICE. grantReply already sends
	// `"share": 2` as an integer, and the gate decodes grants and refusals into ONE
	// acquireResponse struct — so a bool named `share` makes every GRANT fail to unmarshal
	// and the gate fail open on all traffic, which is the silent ungoverning this project
	// exists to avoid. A name is only free if nothing else on the wire already uses it.
	Cause string `json:"cause,omitempty"`
	// Asking is how much budget the refused request wanted, in slots (GOV-34).
	//
	// WITHOUT IT THE REFUSAL REFUTES ITSELF. "Machine at capacity (1/4 build slots)" was a
	// real message on 2026-09-21 and one of four slots is not capacity — the missing number
	// is the 3.75 the request itself weighed, which is what turned three free slots into no
	// room. Slots says what is held and Budget says what is allowed; neither says how big
	// the thing being refused is, and a reader given two of the three cannot check the
	// arithmetic or tell a contended machine from an oversized command.
	//
	// It is the weight the admission rule USED, via lease.WeightOf, not the one on the wire:
	// a request that names no weight contends for a whole slot, and a refusal quoting the
	// literal 0 would be describing a lease that cannot exist.
	Asking float64 `json:"asking,omitempty"`
	// Ceiling is the largest budget this host would allow if nothing were cutting it, and
	// Cut names the signal that IS cutting it, or is empty when nothing is.
	//
	// THE BUDGET ON THE WIRE HAS ALWAYS BEEN THE EFFECTIVE ONE — acquire passes one `limit`
	// to the admission test and to the refusal, so they cannot disagree — and these two do
	// not change that. What they add is the thing a reader could not previously recover: on
	// a machine calibrated for six slots, a refusal quoting three looks exactly like a
	// refusal on a machine that only ever had three. Additive, and the protocol version is
	// NOT bumped: an older gate ignores them and prints the sentence it always printed.
	Ceiling int    `json:"ceiling,omitempty"`
	Cut     string `json:"cut,omitempty"`
}

// waitingReply is the interim answer to an acquire the daemon has decided to park
// (design.md section 7). It is followed by a grantReply or a refuseReply on the same
// connection.
//
// It exists for the gate's benefit, not the daemon's, and it is what makes block-and-wait
// compatible with invariant 1. A gate cannot safely wait twenty seconds for an answer it
// has no evidence is coming, because a wedged daemon is indistinguishable from a busy one
// until it replies. This line is that evidence: it says "I am alive, I have your request,
// and you will have an answer within wait_ms". Only after reading it does the gate extend
// its own deadline past the 100 ms of section 4.4.
type waitingReply struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	WaitMS int    `json:"wait_ms"`
	Budget int    `json:"budget"`
	InUse  int    `json:"in_use"`
}

// adviseReply answers the advisory channel's advise operation (design.md sections 7 and 8).
//
// IT IS THE ONLY REPLY IN THIS FILE THAT IS NOT ABOUT A LEASE, and its shape says so.
// `granted` is a bool rather than the count the withdrawn request_agents returned, because
// the question is about ONE specific command: there is no fractional `cargo build`, so a
// smaller number has nothing to be the answer to. What a refused model can act on instead is
// WaitHintMS — "not now, in about twenty seconds" — which is why that field carries the
// weight here that a count carried there.
type adviseReply struct {
	V       int  `json:"v"`
	Granted bool `json:"granted"`
	// WaitHintMS is how long the caller should expect to wait, and it is the gate's real
	// parking behaviour rather than a guess: a refused build that retries goes into the same
	// queue and is held for up to this long before it is refused for capacity. Zero on a
	// grant.
	WaitHintMS int     `json:"wait_hint_ms"`
	Reason     string  `json:"reason"`
	Budget     int     `json:"budget"`
	InUse      int     `json:"in_use"`
	Slots      float64 `json:"slots"`
}

// versionReply answers a request in a protocol version this binary does not speak.
type versionReply struct {
	V         int    `json:"v"`
	OK        bool   `json:"ok"`
	Reason    string `json:"reason"`
	Supported []int  `json:"supported"`
}

// ackReply answers confirm, release and session, which section 7 shows only as requests.
// A reply is sent anyway: without one a client cannot tell a daemon that acted from a
// daemon that closed the connection, and GOV-07 needs that distinction.
type ackReply struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Lease names the lease a confirm bound, and is absent on every other ack. GOV-07
	// reads it to correlate what it asked for with what it got.
	Lease string `json:"lease,omitempty"`
	InUse int    `json:"in_use"`
}

// statusReply answers status, for the statusline, doctor and /governor.
//
// The three governance fields below were added by GOV-29 and are additive on purpose: the
// protocol version is NOT bumped for them. A gate or statusline from an older build ignores
// what it does not know and keeps working, where a bump would make every un-upgraded hook on
// the machine fail open the moment the daemon restarted — trading a display defect for an
// ungoverned machine, which is the wrong way round.
type statusReply struct {
	V      int `json:"v"`
	Budget int `json:"budget"`
	InUse  int `json:"in_use"`
	// Slots is how much of the budget those leases weigh. See refuseReply.Slots: since
	// GOV-15 a count of leases and a quantity of budget are different numbers, and only this
	// one is comparable with Budget.
	Slots   float64 `json:"slots"`
	Waiting int     `json:"waiting"`
	// CPU is the 5 s EWMA of host utilisation in [0,1] (design.md section 5.1), or null when
	// nothing has sampled yet. It is the smoothed figure rather than the instantaneous one
	// because a raw read swings hard enough to make anything reading it oscillate — the
	// statusline included.
	CPU     *float64        `json:"cpu"`
	Thermal signals.Thermal `json:"thermal"`
	// The four signals below were added by GOV-12 and are additive, like GOV-29's before
	// them: the protocol version is NOT bumped. An older statusline or doctor ignores what it
	// does not know, where a bump would make every un-upgraded hook on the machine fail open
	// the moment the daemon restarted.
	//
	// Every one is a pointer for the reason the CPU field already was: null means "not
	// measured" and 0.0 means "measured as zero", and a status line that cannot tell them
	// apart reports an idle machine when what it has is no reading at all.
	//
	// Nothing acts on any of them. GOV-12 is observation only.

	// CPUInstant is the latest unsmoothed utilisation, kept alongside the average so a reader
	// tuning the window later can see what the smoothing did. Section 5.1's principle: store
	// the raw reading, because a trace holding only the derived value cannot be re-derived
	// against a different formula.
	CPUInstant *float64 `json:"cpu_instant,omitempty"`
	// RunQueue is loadavg1 over usable cores — contention that CPU% misses.
	RunQueue *float64 `json:"run_queue,omitempty"`
	// CPUCapacity is the cores the governor may actually use, and CPUCapacitySource says
	// whether that came from the host or from a cgroup quota. A 64-core host throttled to two
	// and a two-core machine report the same capacity and want different budgets.
	CPUCapacity       *float64 `json:"cpu_capacity,omitempty"`
	CPUCapacitySource string   `json:"cpu_capacity_source,omitempty"`
	// MemAvailableBytes is memory available without swapping, and SwapInBytesPerSec is how
	// fast pages are being faulted back in. Section 5.1 pairs them deliberately: free memory
	// falling is ordinary, pages coming back from swap is contention happening now.
	MemAvailableBytes *uint64  `json:"mem_available_bytes,omitempty"`
	SwapInBytesPerSec *float64 `json:"swap_in_bytes_per_sec,omitempty"`
	// TreeCPU is cores busy across the whole claude process tree, descendants included, and
	// TreeCPURoots is the session processes alone. The gap between them is the Phase 0
	// question — whether the load is the agents or the builds they launch — so they are
	// reported as two numbers rather than collapsed into one.
	//
	// These refresh every treeInterval rather than every tick; see sampleInterval.
	TreeCPU      *float64     `json:"tree_cpu,omitempty"`
	TreeCPURoots *float64     `json:"tree_cpu_roots,omitempty"`
	Sources      []sourceStat `json:"sources"`
	// BinaryVersion is the version of the binary running this daemon, as `governor version`
	// reports it. Additive like the governance fields below, and for the same reason: an
	// older reader ignores what it does not know.
	//
	// It exists because until GOV-24 nothing could tell WHICH governor was holding the
	// socket. The protocol "v" answers a narrower question — two builds can speak v1 and
	// still be different binaries — so an upgrade that changed behaviour without changing the
	// wire format left `install` no way to see that the daemon was stale. `install` reads
	// this to decide whether the running daemon is the one it just wrote.
	BinaryVersion string `json:"binary_version,omitempty"`
	// Build is the sha256 of the executable this daemon was started from, taken at startup
	// (GOV-43; see build.go). BinaryVersion cannot tell two builds apart when both call
	// themselves 0.0.0-dev, or when a local build follows a release; this can. "unknown" when
	// the daemon could not read itself. Absent from a daemon older than the field — which is
	// itself the answer: that daemon is older than any binary able to ask. Additive, no
	// protocol bump, for the reason given above BinaryVersion.
	Build string `json:"build,omitempty"`
	// DaemonStarted is when this daemon came up, RFC3339. See daemon.started.
	DaemonStarted string `json:"daemon_started,omitempty"`
	// Config is what this daemon loaded from config.json at startup, and the run-queue
	// threshold it is cutting at now (GOV-44; see configdrift.go). The build's companion: the
	// build says which binary is holding the socket, this says which config it is running.
	// Additive, no protocol bump, for the reason given above BinaryVersion.
	Config *loadedConfig `json:"config,omitempty"`
	// HooksInstalled is the mtime of the gate's hooks file, RFC3339, empty when there is no
	// such file. A session that started before it could not have loaded the gate and is
	// ungoverned for the rest of its life, which is the whole of GOV-29's defect.
	HooksInstalled string `json:"hooks_installed,omitempty"`
	// SessionsKnown is false when the harness's session registry could not be read at all,
	// which makes every Live field below meaningless. It is a separate field rather than an
	// empty list because "no registry on this machine" and "no sessions running" are
	// different answers and only one of them is a reason to stay quiet.
	SessionsKnown bool          `json:"sessions_known"`
	Sessions      []sessionStat `json:"sessions"`
	// SpawnSurface is every (Claude Code version, spawn tool name) pair this daemon has
	// actually seen the matcher catch — GOV-30's ledger, see surface.go. Additive like the
	// fields above, and read only by doctor.
	SpawnSurface []spawnSighting `json:"spawn_surface,omitempty"`
	// SpawnSurfaceKnown says this daemon keeps that ledger at all, which an empty SpawnSurface
	// cannot say on its own.
	//
	// It exists because the field above is additive and the protocol version is deliberately
	// NOT bumped for it, so a doctor from this build talking to a daemon from an older one
	// receives no ledger and no explanation. Without this flag it cannot tell "recording, and
	// nothing has spawned yet" from "not recording" — and it accused the user of running an
	// ungoverned machine on the strength of the second. Mirrors SessionsKnown, which was added
	// for exactly this shape of mistake.
	SpawnSurfaceKnown bool `json:"spawn_surface_known"`

	// The manual overrides of design.md section 9.1 (GOV-B01). Additive, like the governance
	// fields above: a daemon from before this story sends none of them, which reads as "no
	// override in force" — the right answer for a daemon that has no way to have one.
	//
	// Pinned and Paused are sent even when false so that a reader can tell "not pinned" from
	// a daemon too old to know; the DEADLINES are omitempty because a zero time serialises as
	// a year-1 timestamp, and a surface rendering "pinned until 0001-01-01" would be stating a
	// fact nobody computed.
	Pinned bool `json:"pinned"`
	// PinnedBudget is the budget the SOURCES computed, kept beside the pinned Budget above so
	// a person can see the number the loop would have chosen. Without it a pin explains
	// nothing: the status line says 2 slots and the reason it is 2 is invisible.
	SourceBudget int    `json:"source_budget,omitempty"`
	PinnedUntil  string `json:"pinned_until,omitempty"`
	Paused       bool   `json:"paused"`
	PausedUntil  string `json:"paused_until,omitempty"`
	// Ceiling and Cut are refuseReply's fields of the same name, on the surface a person
	// actually reads (GOV-34 clause 2). A denial and `governor status` disagreeing about why
	// the machine is refusing is what made the 2026-09-21 incident unreadable from either
	// end, so both now quote the SAME controller state rather than each deriving its own.
	// Additive; a daemon from before this story sends neither, which reads as "no cut", and
	// that is the right answer for a daemon with no way to have one.
	Ceiling int    `json:"ceiling,omitempty"`
	Cut     string `json:"cut,omitempty"`
}

type sourceStat struct {
	Name   string `json:"name"`
	Budget int    `json:"budget"`
	InUse  int    `json:"in_use"`
}

type sessionStat struct {
	Session   string `json:"session"`
	PID       int    `json:"pid"`
	Weight    int    `json:"weight"`
	Tentative int    `json:"tentative"`
	Confirmed int    `json:"confirmed"`
	// Registered is true when this session ran the SessionStart hook against this daemon.
	// It is the only positive proof that a session's hooks are loaded — a session holding no
	// leases is indistinguishable from an ungoverned one by lease count alone, which is
	// exactly how a machine with four ungoverned sessions read as healthy.
	Registered bool `json:"registered"`
	// Live is true when a claude process for this session is running right now, from the
	// harness registry rather than from anything the governor was told.
	Live bool `json:"live,omitempty"`
	// Started, CWD and Version come from that registry and are set for live sessions only.
	// CWD is carried so a diagnostic can name the session a person has to restart in terms
	// they can act on; a bare UUID is not one.
	Started string `json:"started,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	Version string `json:"version,omitempty"`
	// BoostedUntil is when a manually raised weight reverts (GOV-B01, section 9.1). Empty
	// when the weight is not temporary, which is every session nobody has boosted.
	BoostedUntil string `json:"boosted_until,omitempty"`
	// Kind is the harness's session kind. Only an interactive session can be restarted, which
	// is the only fix an ungoverned session has.
	Kind string `json:"kind,omitempty"`
}

// maxRequest bounds one request line. Requests are small and fixed in shape; a client that
// streams without a newline is a leak, not a large request.
const maxRequest = 64 << 10

// daemon is the broker. Everything it owns is either immutable or internally synchronised,
// so a handler needs no lock of its own.
type daemon struct {
	// speaks is the protocol version this daemon serves: the "v" it stamps on every reply
	// and the only "v" it accepts on a request (design.md section 7).
	//
	// It is a field rather than a read of the protocolVersion constant so that a test can
	// stand up a daemon of ANOTHER version beside a gate of this one and drive the mismatch
	// for real. That matters more than it looks: an upgrade is precisely a live daemon of one
	// version meeting gates of another, and the only way to know that path fails open rather
	// than stalling is to run both halves at once. A canned reply line proves the gate can
	// parse a rejection; it cannot prove the daemon produces one.
	//
	// Nothing outside a test ever sets it — newDaemonWithLeases defaults it to
	// protocolVersion and no command-line flag reaches it.
	speaks  int
	table   *lease.Table
	sources []budget.Source
	thermal func() signals.Thermal
	// waits is the FIFO of parked acquires (design.md section 6.2).
	waits *waitQueue
	// denials is what the refusal text escalates on, and what the wait's rate limit reads.
	denials *denyLog
	// diag is where a handler's complaints go. It is never the reply: a client that sends
	// rubbish is told nothing, because there is nothing useful to tell it and a broker that
	// echoes attacker-controlled input is a broker that can be used to write to a log.
	diag io.Writer
	// peakMu guards peak, the highest in_use this daemon has seen. It is logged when it
	// rises and never otherwise — see event.
	peakMu sync.Mutex
	peak   int
	// started is when this daemon process came up. It is reported in status so a reader can
	// tell "this session never registered" from "this session registered with a daemon that
	// has since been replaced" — the session table is in memory and does not survive a
	// restart, and `governor install` restarts the daemon on every upgrade.
	started time.Time
	// build is this process's build digest (build.go), reported in status.
	build string
	// liveSessions enumerates the Claude Code sessions running on this machine, and
	// hooksInstalledAt reports when the gate's hooks file was written. Both are injectable
	// because both read a directory this package does not own, and a test that had to
	// fabricate a live process to exercise the merge would be a test nobody trusts.
	liveSessions     func() ([]signals.LiveSession, error)
	hooksInstalledAt func() time.Time
	// surface is the spawn-surface ledger of GOV-30: which tool names the matcher has
	// actually been seen to catch, per Claude Code version. Written off the acquire path,
	// read by doctor through the status reply. Never consulted for a decision.
	surface *surfaceLedger
	// machineMu guards the cached answer below.
	machineMu   sync.Mutex
	machineAt   time.Time
	machineSeen []signals.LiveSession
	machineErr  error
	machineHook time.Time

	// overrides are the manual pins of design.md section 9.1 (GOV-B01): a pinned budget and
	// a pause, each expiring on its own. Read on the budget path through liveBudget and
	// reported by status; see override.go for why nothing here is persisted.
	//
	// A VALUE, NOT A POINTER, so that a daemon built as a struct literal — which is how every
	// test in this package builds one — has a working override rather than a nil one. The
	// first version was a pointer initialised only in newDaemonWithLeases and it panicked on
	// the acquire path in eleven tests.
	overrides override

	// loop is the AIMD controller (design.md section 5.2). It is stepped only by sample(),
	// and its budget is read through LocalSource.Live, so nothing else touches it.
	//
	// Nil until a sampler is wired: a daemon that cannot read the host must not be inventing
	// a moving budget from nothing, and LocalSource falls back to its fixed N.
	loop *budget.Controller

	// calibratedBMax is the ceiling `governor calibrate` measured for this host, in slots.
	//
	// Zero means uncalibrated, and the ceiling is then whatever budget.MaxSlots derives from
	// CPUCapacity — byte-identical behaviour to before GOV-25, which is the inert default
	// design.md section 13.4 requires of every config key. See ceiling for why a calibrated
	// value CAPS that derivation rather than replacing it.
	calibratedBMax int

	// loaded is what this daemon read from config.json at startup, reported in status so a
	// reader can tell whether it is running what is on disk (GOV-44, configdrift.go). Nil for
	// a daemon that never loaded a config — every daemon a test builds.
	loaded *loadedConfig
	// configStamp is config.json's identity when it was last loaded, so the sampling tick can
	// see that `calibrate` rewrote it and reload (reloadConfig). Guarded by signalsMu, like
	// loaded, calibratedBMax and the loop it feeds.
	configStamp fileStamp

	// sampler reads this host's load. It is nil unless something wired one in, and a nil
	// sampler means the daemon reports every signal as null rather than as zero.
	//
	// Nil by default for the same reason liveSessions defaults to noLiveSessions, and the
	// reason is recorded there in full: a default that reaches for the real machine does not
	// fail in a test, it quietly measures the runner. A sampler is worse than the session
	// registry in that respect — it walks the entire process table — so runDaemon wires it
	// and nothing else does.
	sampler signals.Sampler
	// signalsMu guards the latest reading and the average below. The sampling goroutine is
	// the only writer; every status call is a reader.
	signalsMu sync.Mutex
	latest    signals.Sample
	latestOK  bool
	// cpuAvg is the 5 s EWMA of CPU utilisation from design.md section 5.1. It is kept here
	// rather than in the Sample because an average is a property of the series, not of any
	// one reading in it.
	cpuAvg *signals.EWMA
}

// machineTTL is how long the daemon reuses its reading of this host's live sessions.
//
// Without it the cost is quadratic in sessions: every session's status line polls roughly
// once a second, and each poll would cost a readdir plus a file read and a process lookup per
// session. Six sessions is thirty-six process lookups a second on the machine this program
// exists to keep quiet. It matches the statusline's own cache for the same reason that one is
// a second — nothing here moves faster than a person reads, and a session that started a
// second ago is covered by classify's grace window anyway.
const machineTTL = time.Second

// observe returns this host's live sessions and hook install time, at most once per
// machineTTL. The error is cached with the answer: a host with no registry must not pay a
// failed directory read on every status call either.
func (d *daemon) observe() (live []signals.LiveSession, hooksAt time.Time, err error) {
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	if !d.machineAt.IsZero() && time.Since(d.machineAt) < machineTTL {
		return d.machineSeen, d.machineHook, d.machineErr
	}
	d.machineSeen, d.machineErr = d.liveSessions()
	d.machineHook = d.hooksInstalledAt()
	d.machineAt = time.Now()
	return d.machineSeen, d.machineHook, d.machineErr
}

// noteSpawnSurface records that the matcher caught tool on the Claude Code version this
// session is running (GOV-30). It is the only writer of the ledger.
//
// OFF THE HOT PATH, TWICE OVER. The version lookup goes through observe(), which caches for a
// second and takes a mutex, and the disk write is an fsync-less rename — neither belongs in
// front of a spawn Claude Code is blocked on. So the whole thing runs on its own goroutine
// and the caller returns immediately. Nothing waits for it, nothing fails if it loses: the
// worst case is a sighting not recorded, which reads as "not confirmed" and is corrected by
// the next spawn.
//
// The version may come back empty — an unregistered session, or a host with no registry at
// all. That is recorded as an empty version rather than dropped, because a sighting still
// proves the matcher fired even when nothing can say against which version.
func (d *daemon) noteSpawnSurface(session, tool string) {
	if tool == "" {
		return
	}
	go func() {
		version := ""
		if live, _, err := d.observe(); err == nil {
			for _, s := range live {
				if s.ID == session {
					version = s.Version
					break
				}
			}
		}
		if d.surface.note(version, tool) {
			// Only a NEW (version, tool) pair reaches the disk. On a settled machine that is
			// one write per Claude Code upgrade, not one per spawn.
			d.surface.save()
		}
	}()
}

// The budget event log (GOV-11).
//
// The daemon otherwise records only its own start-up, which makes the one question GOV-11
// has to answer — did a fixed budget of four actually help? — unanswerable by anything but
// memory. These lines are the evidence. They are deliberately few: a machine's subagents
// were measured at single digits a day (docs/hook-surface.md section 8.3), and an event is
// written when the budget MOVES or BINDS. A daemon whose log shows a peak that never reached
// the budget is itself the finding — four was never touched and the semaphore did nothing —
// and that reading is only available because the peak is recorded on the way up rather than
// inferred from the absence of denials.
//
// The log is the daemon's stderr, which `governor install` and a lazy start both point at
// ~/.claude/governor/daemon.log. Timestamped because it is append-only across restarts and a
// week of it is read after the fact, not watched.

// event records one budget event.
func (d *daemon) event(format string, args ...any) {
	fmt.Fprintf(d.diag, "%s budget %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// notePeak logs a new high-water mark for concurrent leases, and only a new one.
//
// The high-water mark is the single most informative number in the log: if it never reaches
// the budget, the budget never bound, and no amount of reasoning about denials matters.
func (d *daemon) notePeak(inUse, limit int) {
	d.peakMu.Lock()
	if inUse <= d.peak {
		d.peakMu.Unlock()
		return
	}
	d.peak = inUse
	d.peakMu.Unlock()
	d.event("peak in_use=%d/%d", inUse, limit)
}

// syncWriter serialises writes to the daemon log.
//
// The daemon answers every connection in its own goroutine and they all share one log. Two
// events written at the same instant would interleave, and where the log is a bytes.Buffer —
// which is how every test in this package builds one — they are a data race that `go test
// -race` is right to fail.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func runDaemon(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("governor daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "endpoint URL to listen on (default: "+transport.EndpointEnv+", else the state directory)")
	limit := fs.Int("budget", defaultBudget, "concurrent agent budget (provisional: GOV-14 replaces this with the control loop)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ep := *endpoint
	if ep == "" {
		var err error
		if ep, err = transport.DefaultEndpoint(); err != nil {
			fmt.Fprintf(stderr, "governor daemon: resolve endpoint: %v\n", err)
			return 1
		}
	}

	// The build digest is taken FIRST, before the socket is bound and before anything can
	// rename a newer binary over this one's path. Hashed later, it would describe whatever
	// file the path names by then rather than the code this process is running. build.go.
	thisBuild()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := newDaemon(*limit, stderr)
	// The one place the daemon is allowed to look at the real machine.
	d.observeThisMachine()
	// And the one place its spawn-surface ledger is given a file. Derived from the endpoint
	// rather than from transport.StateDir for the reason daemonPIDPath is: a daemon started
	// with --endpoint elsewhere must not write over the ledger of the one serving the
	// default socket. A directory that cannot be resolved leaves the ledger in memory, which
	// costs the evidence on restart and nothing else.
	if dir, err := stateDirFor(ep); err == nil {
		d.surface = newSurfaceLedger(dir)
	}
	ln, err := transport.Listen{}.Listen(ctx, ep)
	if err != nil {
		fmt.Fprintf(stderr, "governor daemon: %v\n", err)
		return 1
	}
	// Written after the listener is bound and removed on the way out, so its presence means
	// "this pid held the socket" rather than "this pid intended to". `governor uninstall` is
	// the only reader: it is the one command that has to stop a process it did not start,
	// and a daemon still running after uninstall is the single piece of residue a user
	// cannot see in a directory listing. Failing to write it is NOT fatal — the daemon's job
	// is to answer gates, and refusing to serve because a convenience file would not write
	// would take the machine ungoverned to protect a bookkeeping detail.
	if err := writeDaemonPID(ep, stderr); err == nil {
		defer removeDaemonPID(ep)
	}
	fmt.Fprintf(stdout, "governor daemon listening on %s (budget %d, %s build %s)\n",
		ep, *limit, binaryVersion, shortBuild(d.build))

	d.serve(ctx, ln)
	return 0
}

// daemonPIDFile and daemonLogFile are the daemon's two files in the state directory
// (design.md section 13.3). The log is written by `governor install` when it starts the
// daemon detached; this process only names it.
const (
	daemonPIDFile = "daemon.pid"
	daemonLogFile = "daemon.log"
)

// daemonPIDPath is the pid file beside the socket the daemon actually bound.
//
// Derived from the endpoint rather than from transport.StateDir, so that a daemon started
// with --endpoint elsewhere writes its pid beside its own socket instead of over the pid of
// the daemon serving the default one. Two governors on one machine is a supported thing
// (GOVERNOR_ENDPOINT exists for it) and a shared pid file would have uninstall kill the
// wrong one.
func daemonPIDPath(endpoint string) (string, error) {
	dir, err := stateDirFor(endpoint)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, daemonPIDFile), nil
}

// socketPath is the filesystem path an endpoint URL names, or an error naming why it has
// none. v1 speaks unix:// and nothing else, exactly as transport.Dial does.
//
// It lives here rather than beside its first caller because three things now depend on
// agreeing about it: `doctor` checks the socket's permissions, the daemon writes its pid
// beside it, and a lazy start rate-limits itself in the same directory (GOV-11). Two
// functions that each strip a scheme prefix will eventually disagree about what counts as
// an endpoint, and the cost of that disagreement is a path uninstall stops covering.
func socketPath(endpoint string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(endpoint, prefix) {
		return "", fmt.Errorf("endpoint %s is not a unix:// URL: v1 speaks unix:// and nothing else", endpoint)
	}
	path := strings.TrimPrefix(endpoint, prefix)
	if path == "" {
		return "", fmt.Errorf("endpoint %s has no socket path", endpoint)
	}
	return path, nil
}

func writeDaemonPID(endpoint string, stderr io.Writer) error {
	path, err := daemonPIDPath(endpoint)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "governor daemon: write pid file: %v\n", err)
		return err
	}
	return nil
}

// removeDaemonPID removes the pid file, and only when it still holds this process's pid.
//
// The check matters on a restart: if this daemon is exiting because another has taken over,
// the file already names the new one, and removing it would leave the live daemon
// unstoppable by uninstall.
func removeDaemonPID(endpoint string) {
	path, err := daemonPIDPath(endpoint)
	if err != nil {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		return
	}
	_ = os.Remove(path)
}

// newDaemon builds a daemon with the v1 budget: a slice of sources with exactly one member,
// reduced by min().
//
// The slice is the point. One source does not need a reducer, but the plural is the whole
// of the tier-two seam (design.md section 12.1) — a remote source governing tokens and
// spend is added at this line and nothing downstream changes, because every reader already
// takes the minimum over a set.
func newDaemon(limit int, diag io.Writer) *daemon {
	return newDaemonWithLeases(limit, diag, lease.Options{})
}

// newDaemonWithLeases is newDaemon over a lease table configured by opt.
//
// It exists so that a test can shorten the TTLs. The tentative TTL is thirty seconds and the
// confirmed one is half an hour, and a test that waited for either would be a test nobody
// runs. Everything else about the daemon is identical, and the shipped defaults still come
// from lease.NewTable — this cannot change them, only override them for a caller that says
// so explicitly.
func newDaemonWithLeases(limit int, diag io.Writer, opt lease.Options) *daemon {
	if opt.Liveness == nil {
		opt.Liveness = lease.PIDLiveness{}
	}
	table := lease.NewTable(opt)
	return &daemon{
		speaks:           protocolVersion,
		table:            table,
		sources:          []budget.Source{budget.LocalSource{N: limit, InUse: table.InUse}},
		thermal:          signals.ReadThermal,
		waits:            &waitQueue{},
		denials:          newDenyLog(nil),
		diag:             &syncWriter{w: diag},
		started:          time.Now(),
		build:            thisBuild(),
		liveSessions:     noLiveSessions,
		hooksInstalledAt: func() time.Time { return time.Time{} },
		// Pathless by default, for the same reason liveSessions defaults to noLiveSessions:
		// a daemon constructed in a test must not write to the developer's real state
		// directory. runDaemon gives it a path; everything else keeps the ledger in memory.
		surface: newSurfaceLedger(""),
	}
}

// noLiveSessions is the default answer to "what is running on this machine": unknown.
//
// The real enumerator is wired in by runDaemon and nowhere else, deliberately. Both of these
// functions read directories under the developer's real home, and TestMain in this package
// exists because anything that reaches for a real home without being told does not fail — it
// quietly measures the machine it runs on. A default that read the registry made four of this
// package's tests assert against whichever sessions happened to be open, which is precisely
// the accident that file was written to stop. Unknown is also the honest default for a daemon
// on a host whose harness keeps no registry.
func noLiveSessions() ([]signals.LiveSession, error) { return nil, signals.ErrNoRegistry }

// observeThisMachine points a daemon at the real host: the harness's session registry and the
// mtime of the hook file a session starting now would load. runDaemon calls it; tests that
// want session data assign their own stubs instead.
func (d *daemon) observeThisMachine() {
	d.liveSessions = func() ([]signals.LiveSession, error) { return signals.LiveSessions("") }
	d.hooksInstalledAt = gateHooksInstalledAt
	d.sampler = signals.NewSamplerEvery(treeInterval)

	// The loop is wired here and nowhere else, beside the sampler that feeds it. A daemon
	// built by a test has neither, and so keeps the fixed budget it was constructed with —
	// which is what lets a lease test assert against a number it chose.
	cal, loaded := loadCalibration(d.diag)
	d.calibratedBMax = cal.BMax
	loaded.RunQueueOverride = runQueueMaxOverride(io.Discard)
	d.loaded = &loaded
	d.configStamp = stampOf(loaded.Path)
	// MAX IS SET HERE, NOT LEFT FOR THE FIRST TICK (GOV-41). Controller.start() begins at the
	// ceiling precisely because the daemon is started lazily by a gate, but with Max unset
	// the ceiling it begins at is zero, which reads as Floor: every daemon answered budget 1
	// from its first request until the first sample landed, and that request is always the
	// one that started it. sample() still recomputes Max every tick, so a cgroup quota that
	// changes under a running daemon is still followed; this only fills the gap before that.
	capacity, _ := hostCapacity()
	d.loop = &budget.Controller{
		Max:         d.ceiling(capacity),
		RunQueueMax: resolveRunQueueMax(cal, d.diag),
	}
	d.sources = []budget.Source{budget.LocalSource{
		Live:  d.liveBudget,
		InUse: d.table.InUse,
	}}
}

// hostCapacity reads this host's CPU capacity for the controller's starting ceiling. A
// variable rather than a direct call so the GOV-41 test can pin a capacity whose ceiling is
// above the floor on any runner — on a two-core CI host the real one would floor anyway, and
// the test could not tell the fix from the bug.
var hostCapacity = signals.CPUCapacity

// runQueueMaxEnv overrides internal/budget.RunQueueMax for one daemon.
//
// EXPERIMENT SURFACE, NOT A TUNING SURFACE. tools/sweep sets it to measure the threshold
// under load; nothing in the shipped product reads it, `governor calibrate` does not write
// it, and an unset or unparseable value leaves the compiled-in constant alone. It is here
// because a constant cannot be swept, and the alternative — rebuilding the binary per arm —
// is the kind of method a person who was not there cannot reproduce.
const runQueueMaxEnv = "GOVERNOR_RUNQUEUE_MAX"

// runQueueMaxOverride reads that variable. A malformed value is reported and ignored rather
// than defaulted to zero, because silently governing at the wrong threshold is how a sweep
// produces a confident wrong number.
func runQueueMaxOverride(diag io.Writer) float64 {
	raw := strings.TrimSpace(os.Getenv(runQueueMaxEnv))
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		fmt.Fprintf(diag, "governor daemon: ignoring %s=%q: want a positive number\n", runQueueMaxEnv, raw)
		return 0
	}
	fmt.Fprintf(diag, "governor daemon: %s=%g overriding the compiled-in run-queue threshold of %g\n", runQueueMaxEnv, v, budget.RunQueueMax)
	return v
}

// liveBudget is the controller's current answer, read under the daemon's signals lock
// because the loop is stepped from the sampling goroutine and read from every acquire.
func (d *daemon) liveBudget() int {
	d.signalsMu.Lock()
	defer d.signalsMu.Unlock()
	if d.loop == nil {
		return budget.Floor
	}
	return d.loop.Budget()
}

// cutNow is the signal holding the budget below this host's ceiling, and the ceiling itself.
//
// Read under the same lock as liveBudget and from the same controller, because the whole
// point of GOV-34 clause 2 is that the refusal text and `governor status` cannot disagree.
// A daemon with no controller — which is every daemon a test builds — is not cutting
// anything, and says so rather than inventing a ceiling it never computed.
func (d *daemon) cutNow() (ceiling int, cut string) {
	d.signalsMu.Lock()
	defer d.signalsMu.Unlock()
	if d.loop == nil {
		return 0, ""
	}
	return d.loop.Max, d.loop.Cut()
}

// budgetNow is the budget every decision is made against: the sources reduced by min(), with a
// manual pin (design.md section 9.1) applied on top.
//
// THE PIN IS APPLIED HERE RATHER THAN INSIDE LocalSource, and the reason is a seam rule. The
// sources are the tier-two seam (section 12.1) and each is one interface with one
// implementation; a pin is not a budget SOURCE, it is a person overruling the answer the
// sources gave, and teaching LocalSource about it would put a local-only concept inside the
// abstraction that exists to stop local-only concepts leaking. Applying it at the point of use
// also means it works on a daemon with no controller wired — which is every daemon a test
// builds, and so the difference between a pin that is tested and one that is only shipped.
//
// The quotas are returned unmodified on purpose. They are what the SOURCES said, and status
// reports them separately from the budget in force: a person looking at a pinned daemon should
// be able to see the number the loop would have chosen, or the pin explains nothing.
//
// A PAUSE IS NOT APPLIED HERE. Pausing means "stop capping", and the obvious implementation —
// return a huge number — would make `governor status` report a budget no source ever computed,
// which is the fabricated-reading failure section 4.2 and the status line both exist to
// prevent. acquire skips the capacity test instead, and every surface reports it as a pause.
func (d *daemon) budgetNow(who principal) (int, []budget.Quota, overrideState) {
	limit, quotas := budget.Min(context.Background(), d.sources, who)
	over := d.overrides.state()
	if over.pinned() {
		limit = over.Limit
		if limit < budget.Floor {
			// Invariant 4 is not negotiable by a person either. A pin of zero is a wedged
			// machine, not a budget.
			limit = budget.Floor
		}
	}
	return limit, quotas, over
}

// serve accepts connections until ctx is cancelled, reaping leases on a ticker throughout.
func (d *daemon) serve(ctx context.Context, ln net.Listener) {
	var wg sync.WaitGroup

	// Closing the listener is what unblocks Accept; nothing else does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		ln.Close()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.reap(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.sample(ctx)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			// A single failed accept is not a reason to stop being a broker. Pause briefly
			// so a persistent error cannot spin a core.
			fmt.Fprintf(d.diag, "governor daemon: accept: %v\n", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Millisecond):
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.handle(conn)
		}()
	}
	wg.Wait()
}

// sampleInterval is the daemon's tick, and treeInterval is how often the tick includes the
// process-tree walk.
//
// One second is design.md section 5.1's rate and the rate the control loop will read. Five
// seconds for the tree is what keeps the whole thing affordable: the walk is 23.0 ms of CPU
// against 1-2 ms for everything else, measured over 46 hours of the Phase 0 sampler at 2.30%
// of one core. Refreshing it every fifth tick averages ~0.6% of a core, under GOV-12's bar,
// and costs the v1 loop nothing because section 5.1 lists per-agent tree cost as a Phase 2
// refinement — the signals the loop acts on are the cheap ones and still tick every second.
const (
	sampleInterval = time.Second
	treeInterval   = 5 * time.Second
)

// sample reads the host until ctx is cancelled and steps the control loop with what it reads.
//
// GOV-12 deliberately made this goroutine inert — it wrote d.latest and d.cpuAvg and touched
// no lease, no source and no budget — because the dogfooding week was measuring a fixed
// semaphore and a sampler that quietly fed the allocator would have changed the artefact
// under observation. That week is closed and GOV-15 connects the two: the budget is now an
// OUTPUT of this loop (design.md section 5.2).
//
// It remains the ONLY writer of the controller. Nothing on the request path steps it, so an
// acquire cannot move the budget it is being judged against.
func (d *daemon) sample(ctx context.Context) {
	if d.sampler == nil {
		return
	}
	t := time.NewTicker(sampleInterval)
	defer t.Stop()

	// The first reading is a priming read — NewSampler's contract says CPU and every
	// process-tree rate come back zero because they are differences against a tick that does
	// not exist yet. Taking it here and discarding it means the first sample a status call
	// can see is a real one, rather than a row of false zeroes that reads as an idle machine.
	if _, err := d.sampler.Sample(ctx); err != nil {
		fmt.Fprintf(d.diag, "governor daemon: priming sample: %v\n", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reloadConfig()
			// Read demand BEFORE the signals lock, not inside it: the wait queue has a lock
			// of its own and every acquire holds it, so nesting the two would put the
			// sampling goroutine in a lock order no other path uses.
			demand := d.waits.len() > 0
			s, err := d.sampler.Sample(ctx)
			if err != nil {
				// A failed reading is not a reason to stop sampling, and not a reason to
				// overwrite a good reading with a blank one. The previous sample stands and
				// the next tick tries again.
				fmt.Fprintf(d.diag, "governor daemon: sample: %v\n", err)
				continue
			}
			d.signalsMu.Lock()
			d.latest, d.latestOK = s, true
			if d.cpuAvg == nil {
				d.cpuAvg = signals.NewEWMA(signals.DefaultCPUWindow)
			}
			d.cpuAvg.Update(s.CPU, time.Now())
			if avg, ok := d.cpuAvg.Value(); ok && d.loop != nil {
				// The ceiling comes from the machine on every tick rather than once at
				// start-up, because CPUCapacity is the cgroup quota where there is one and a
				// quota can be changed under a running daemon.
				d.loop.Max = d.ceiling(s.CPUCapacity)
				d.loop.Step(budget.Reading{
					CPU:           avg,
					RunQueue:      s.RunQueue,
					ThermalSevere: s.Thermal.Severe(),
					DemandWaiting: demand,
				})
			}
			d.signalsMu.Unlock()
		}
	}
}

// reap collects expired and orphaned leases until ctx is cancelled.
func (d *daemon) reap(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := d.table.Reap(); n > 0 {
				fmt.Fprintf(d.diag, "governor daemon: reaped %d lease(s)\n", n)
			}
			// Unconditionally, not only when something was reaped. This is the backstop for
			// a wake-up that was somehow missed on the release path; a waiter should never
			// sit through its whole wait next to a free slot because of a lost signal.
			d.pump()
			d.denials.sweep()
		}
	}
}

// handle serves one request on one connection, then closes it.
//
// Every failure here is contained to this connection. That is design.md section 7's rule —
// "the daemon MUST tolerate a malformed line by closing that connection only" — and the
// recover is what makes it true of a panic as well as of a parse error. One bad client must
// never take down the broker, because every other session on the machine is behind it.
func (d *daemon) handle(conn net.Conn) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(d.diag, "governor daemon: recovered from handler panic: %v\n", r)
		}
	}()

	if err := conn.SetDeadline(time.Now().Add(connDeadline)); err != nil {
		return
	}

	line, err := bufio.NewReaderSize(io.LimitReader(conn, maxRequest), 4096).ReadString('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		fmt.Fprintf(d.diag, "governor daemon: read request: %v\n", err)
		return
	}

	var req request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		// No reply. A malformed line is not a protocol error to be reported, it is a client
		// this daemon cannot talk to; the gate's answer to a closed connection is to fail
		// open, which is the right outcome.
		fmt.Fprintf(d.diag, "governor daemon: malformed request, closing connection: %v\n", err)
		return
	}

	reply := d.dispatch(req)

	// A parked acquire is answered twice: once immediately to say it has been parked, and
	// once when it resolves. The interim line must reach the client BEFORE the wait starts,
	// because it is the client's only evidence that waiting is safe — sending it afterwards
	// would be sending it too late to be worth anything.
	if p, ok := reply.(*parkedAcquire); ok {
		if !d.write(conn, p.interim) {
			p.cancel()
			return
		}
		// The exchange is now legitimately longer than one round trip.
		if err := conn.SetDeadline(time.Now().Add(p.wait + connDeadline)); err != nil {
			p.cancel()
			return
		}
		reply = p.resolve()
	}

	d.write(conn, reply)
}

// write marshals one reply and sends it, reporting whether the client got it.
func (d *daemon) write(conn net.Conn, reply any) bool {
	out, err := json.Marshal(reply)
	if err != nil {
		fmt.Fprintf(d.diag, "governor daemon: encode reply: %v\n", err)
		return false
	}
	if _, err := conn.Write(append(out, '\n')); err != nil {
		fmt.Fprintf(d.diag, "governor daemon: write reply: %v\n", err)
		return false
	}
	return true
}

// dispatch answers one well-formed request. The return is any because each operation has
// its own shape on the wire (section 7), not a shared envelope.
func (d *daemon) dispatch(req request) any {
	// A version this binary does not speak is answered, never ignored (invariant 8). During
	// an upgrade there will be a daemon of one version and gates of another; the gate treats
	// this reply exactly as it treats an unreachable daemon and fails open, so an upgrade
	// costs ungoverned spawns rather than a stalled session.
	if req.V != d.speaks {
		return versionReply{V: d.speaks, Reason: "unsupported_version", Supported: []int{d.speaks}}
	}
	switch req.Op {
	case "acquire":
		return d.acquire(req)
	case "confirm":
		return d.confirm(req)
	case "release":
		return d.release(req)
	case "finish":
		return d.finish(req)
	case "session":
		return d.session(req)
	case "status":
		return d.status(req)
	case "advise":
		return d.advise(req)
	case "control":
		return d.control(req)
	default:
		return ackReply{V: d.speaks, Reason: "unknown_op", InUse: d.table.InUse()}
	}
}

// parkedAcquire is an acquire the daemon is holding rather than answering. handle writes
// interim, then blocks in resolve for the final answer, or calls cancel if the client went
// away before the wait could even be announced.
type parkedAcquire struct {
	interim waitingReply
	wait    time.Duration
	resolve func() any
	cancel  func()
}

// leaseRequestFor turns a wire acquire into what the lease table needs.
//
// It is where an older gate is normalised: one that sends no kind and no slots asks, in
// effect, for a whole-slot agent lease, which is exactly what every gate before GOV-15 was
// asking for. Reading the absence as "build, weight 0" would be the unsafe direction — it
// would admit an unbounded number of builds during an upgrade.
func leaseRequestFor(req request) lease.Request {
	r := lease.Request{Session: req.Session, Depth: req.Depth, Weight: req.Slots}
	if req.Kind == "build" {
		r.Kind = lease.Build
		// Only a build is keyed by tool_use_id. An agent lease carries one too on the wire,
		// but its release comes from SubagentStop with no tool_use_id at all, so indexing it
		// here would build an index nothing can ever look up.
		r.ToolUseID = req.ToolUseID
	}
	return r
}

// acquire takes a tentative lease if the budget has room, and decides what to do when it
// does not: refuse the caller outright, refuse it for capacity, or park it.
func (d *daemon) acquire(req request) any {
	limit, quotas, over := d.budgetNow(req.Principal)
	src := sourceName(quotas)

	// GOV-30. Recorded for every acquire, including the ones about to be denied: the ledger's
	// question is "did the matcher fire", and a spawn refused for capacity answers it just as
	// well as one that was granted. Off the reply path entirely — see noteSpawnSurface.
	d.noteSpawnSurface(req.Session, req.Tool)

	// The depth limit comes first, and deliberately never waits. A nested spawn is not
	// short of a slot, it is not allowed one, and parking it for twenty seconds before
	// saying so would stall a session to reach an answer that was already known.
	if req.Depth > maxFanOutDepth {
		d.event("deny session=%s reason=depth", req.Session)
		return refuseReply{
			V: d.speaks, Reason: "depth",
			Budget: limit, InUse: d.table.InUse(), Slots: d.table.Slots(), Source: src,
			CPU: nil, Thermal: d.thermal(),
		}
	}

	// THE FAST PATH IS CLOSED WHILE ANYONE IS WAITING (GOV-18). Taking a slot here when the
	// queue is non-empty is the same starvation design.md section 6.2 forbids the pump's walk
	// from causing, arriving by the other door: the pump stops at the first waiter it cannot
	// satisfy, so a queue that is non-empty is one whose head does NOT fit, and a fresh caller
	// asking for less — an agent at 0.08 slots behind a Heavy build at 3.75 — would sail past
	// it every time. A session spawning in a tight loop would then overtake a session that has
	// been waiting, which is exactly what the strict FIFO exists to prevent.
	//
	// The cost is one mutex-guarded length read on the hot path, and only the CONTENDED path
	// pays anything more: an empty queue is the uncontended case and falls straight through.
	// PAUSED MEANS THE CAP IS OFF, NOT THAT THE BOOKS ARE (section 9.1). The lease is still
	// taken, so `governor status` during a pause reports what is genuinely running and the
	// reaper still collects it; only the admission test is skipped. A pause that stopped
	// recording would leave the person who set it with no way to judge when to end it, which
	// is the one thing they need it for.
	//
	// It is checked after the depth limit, not before: a nested fan-out is not short of a slot
	// and never was, so "stop capping" has nothing to say about it.
	if over.Paused {
		l := d.table.Take(leaseRequestFor(req))
		return d.granted(l, limit, src)
	}

	if d.waits.len() == 0 {
		if l, ok := d.table.Acquire(leaseRequestFor(req), limit); ok {
			return d.granted(l, limit, src)
		}
	}

	wait := time.Duration(req.WaitMS) * time.Millisecond
	if wait > maxParkedWait {
		wait = maxParkedWait
	}
	// A client that cannot wait, and a client that has just been refused, are both answered
	// now. The second is the rate limit: without it a session retrying in a loop would take
	// a place in the FIFO on every attempt and hold it for the whole of wait_max, crowding
	// out sessions that asked once and are waiting properly.
	if wait <= 0 || d.denials.throttled(req.Session) {
		return d.refuseForCapacity(req, limit, src)
	}

	d.event("park session=%s slots=%.2f/%d wait=%v", req.Session, d.table.Slots(), limit, wait)
	parked := time.Now()

	w := &waiter{session: req.Session, depth: req.Depth, who: req.Principal, req: leaseRequestFor(req), granted: make(chan lease.Lease, 1)}
	d.waits.enqueue(w)
	// A slot may have freed between the failed Acquire above and the enqueue. Pumping here
	// closes that window without anything having to poll for it.
	d.pump()
	select {
	case l := <-w.granted:
		// It was never really a wait. Answer as an ordinary grant rather than sending an
		// interim line the client would have to read for nothing.
		d.event("park-granted session=%s waited=%v", req.Session, time.Since(parked).Round(time.Millisecond))
		return d.granted(l, limit, src)
	default:
	}

	return &parkedAcquire{
		interim: waitingReply{
			V: d.speaks, Reason: "waiting",
			WaitMS: int(wait / time.Millisecond),
			Budget: limit, InUse: d.table.InUse(),
		},
		wait: wait,
		resolve: func() any {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case l := <-w.granted:
				// The pump removed it from the queue before handing over the lease.
				d.event("park-granted session=%s waited=%v", req.Session, time.Since(parked).Round(time.Millisecond))
				return d.granted(l, limit, src)
			case <-timer.C:
				d.event("park-expired session=%s waited=%v", req.Session, time.Since(parked).Round(time.Millisecond))
				if l, got := d.waits.drop(w); got {
					// Granted at the very moment the wait expired. The lease exists and
					// this caller owns it, so honour it rather than refusing a spawn that
					// already holds a slot.
					return d.granted(l, limit, src)
				}
				return d.refuseForCapacity(req, limit, src)
			}
		},
		cancel: func() {
			if l, got := d.waits.drop(w); got {
				// The client is gone and this lease will never be confirmed. Hand the slot
				// back now instead of letting it wait out its TTL.
				d.table.Drop(l.ID)
			}
		},
	}
}

// advise answers "would the gate grant this command right now" WITHOUT taking a lease,
// counting a denial, parking anything, or touching the surface ledger (design.md section 8).
//
// EVERY ONE OF THOSE OMISSIONS IS DELIBERATE, because this operation is called speculatively
// and an advisory question that changed state would be a second, invisible way to consume the
// budget. In particular it must not record a denial: the gate escalates its refusal text on
// that count, and a model that asked politely three times would find itself being shouted at
// by a gate it had not yet reached.
//
// The answer comes from lease.Table.Fits, which IS Acquire's admission rule rather than a
// copy of it. That is what makes GOV-16's fourth clause structural: the tool cannot say yes
// to something the gate would refuse, because both questions are resolved by the same
// arithmetic over the same table under the same lock.
//
// The depth rule is checked first and for the same reason acquire checks it first — a nested
// caller is not short of a slot, it is not allowed one, and no amount of waiting changes that.
func (d *daemon) advise(req request) adviseReply {
	limit, _ := budget.Min(context.Background(), d.sources, req.Principal)
	reply := adviseReply{V: d.speaks, Budget: limit, InUse: d.table.InUse(), Slots: d.table.Slots()}

	if req.Depth > maxFanOutDepth {
		reply.Reason = "depth"
		return reply
	}
	if d.table.Fits(leaseRequestFor(req), limit) {
		reply.Granted = true
		reply.Reason = "capacity"
		return reply
	}
	// Not "you will wait exactly this long": it is the ceiling the gate would park a retry
	// for before refusing it, which is the honest thing to tell a caller deciding whether to
	// wait or to do something else.
	reply.WaitHintMS = int(maxParkedWait / time.Millisecond)
	reply.Reason = "capacity"
	return reply
}

// granted builds the reply for a lease that has been taken, however it was taken.
func (d *daemon) granted(l lease.Lease, limit int, src string) grantReply {
	inUse := d.table.InUse()
	d.notePeak(inUse, limit)
	return grantReply{
		V: d.speaks, OK: true, Lease: l.ID,
		Budget: limit, InUse: inUse,
		// v1's allocation policy is "no division": there is no max-min fair share and no
		// wait queue yet (design.md section 6.1, 6.2), so a session's share is the whole
		// budget and the first to ask is the first served. This is the accurate description
		// of the policy, not a placeholder — when allocation lands it changes this number
		// rather than adding the field.
		Share:  limit,
		Source: src,
	}
}

// refuseForCapacity builds a capacity refusal and counts it against the session.
//
// The count is what the gate's reason text escalates on. It is taken here, at the moment of
// refusal, rather than anywhere the request merely passed through — a parked acquire that
// ends in a grant must not count as a denial, or a session that waited patiently would be
// shouted at on its next refusal.
func (d *daemon) refuseForCapacity(req request, limit int, src string) refuseReply {
	ceiling, cut := d.cutNow()
	r := refuseReply{
		V: d.speaks, Reason: "capacity",
		Budget: limit, InUse: d.table.InUse(), Slots: d.table.Slots(), Source: src,
		CPU: nil, Thermal: d.thermal(),
		Denials: d.denials.record(req.Session),
		Cause:   d.table.Refusal(leaseRequestFor(req), limit),
		Asking:  lease.WeightOf(leaseRequestFor(req)),
		Ceiling: ceiling, Cut: cut,
	}
	// Logged here rather than at each call site so that a refusal added later cannot be one
	// the log does not know about. This is the event that costs the model something, and it
	// is the one a week of dogfooding is read for.
	d.event("deny session=%s reason=capacity cause=%s slots=%.2f/%d denials=%d", req.Session, r.Cause, r.Slots, r.Budget, r.Denials)
	return r
}

// pump hands freed slots to the longest-waiting callers, in order.
//
// The FIFO is strict: the walk stops at the first waiter that cannot be satisfied rather
// than skipping over it to one that can. Skipping would let a session asking for slots in a
// tight loop overtake a session that has been waiting, which is the starvation design.md
// section 6.2 exists to prevent.
//
// It holds the queue lock across the acquire so that a slot cannot be taken by a fresh
// acquire in between deciding a waiter can have it and handing it over.
//
// THE BUDGET IS READ ONCE, AND OUTSIDE THAT LOCK (GOV-26). It used to be read inside the
// loop, so a source that went over the network was dialled once per waiter while the queue
// was locked — and every fresh acquire consults that same queue (section 6.2), so a slow
// remote source would have stalled admission for the whole machine, N times over, in the one
// place that must not stall. With a LocalSource reading memory the cost was invisible, which
// is exactly why the seam had to be exercised to find it.
//
// Reading it once also means a pump cycle sizes every waiter against one budget rather than a
// number that may move between waiters. That is no loss: the controller steps at 1 Hz and a
// pump is a handful of in-memory admissions, so the two readings were near enough always the
// same. It is a gain in explicability — a pump either had room for a waiter or it did not.
func (d *daemon) pump() {
	// On behalf of the waiter at the head, because that is the one whose budget decides
	// whether the walk moves at all. In v1 every principal on this machine is the same user,
	// so asking per waiter would be asking one question N times; when that stops being true,
	// the FIFO is still strict and the head is still what gates the queue.
	who, ok := d.headPrincipal()
	if !ok {
		return
	}
	limit, _ := budget.Min(context.Background(), d.sources, who)

	d.waits.mu.Lock()
	defer d.waits.mu.Unlock()

	for len(d.waits.q) > 0 {
		w := d.waits.q[0]
		l, ok := d.table.Acquire(w.req, limit)
		if !ok {
			return
		}
		d.waits.q = d.waits.q[1:]
		// Never blocks: granted is buffered, and a waiter is removed from the queue at the
		// same time as it is written to, so it is written to at most once.
		w.granted <- l
	}
}

// headPrincipal reports who is at the front of the wait queue, and whether anyone is.
//
// Separated out so that pump can read it under a lock it holds for nanoseconds, ask the
// budget sources without holding anything, and only then take the lock for the walk.
func (d *daemon) headPrincipal() (principal, bool) {
	d.waits.mu.Lock()
	defer d.waits.mu.Unlock()
	if len(d.waits.q) == 0 {
		return principal{}, false
	}
	return d.waits.q[0].who, true
}

// confirm binds a tentative lease to a live agent.
func (d *daemon) confirm(req request) any {
	var started time.Time
	if req.PIDStartedAt > 0 {
		started = time.Unix(req.PIDStartedAt, 0)
	}
	l, ok := d.table.Confirm(req.Session, req.Lease, req.AgentID, req.PID, started)
	if !ok {
		// Not an error. A resumed subagent reaches the CPU with no PreToolUse at all
		// (design.md section 4.3), so there is nothing to bind and v1 does not govern it.
		return ackReply{V: d.speaks, Reason: "no_pending_lease", InUse: d.table.InUse()}
	}
	return ackReply{V: d.speaks, OK: true, Lease: l.ID, InUse: d.table.InUse()}
}

// release returns a lease to the pool.
func (d *daemon) release(req request) any {
	// SubagentStop fires more than once for the same agent, so "held nothing" is a normal
	// answer rather than a failure.
	ok := d.table.Release(req.Session, req.AgentID)
	if ok {
		// The slot that just came free belongs to whoever has waited longest for it.
		d.pump()
	}
	r := ackReply{V: d.speaks, OK: ok, InUse: d.table.InUse()}
	if !ok {
		r.Reason = "no_lease"
	}
	return r
}

// finish returns a Build lease at PostToolUse (GOV-15, hook-surface.md section 19).
//
// It is a separate operation from release rather than a flag on it, because the two key off
// different things and neither key exists on the other's event: release takes an agent_id
// from SubagentStop, and this takes the tool_use_id that PreToolUse and PostToolUse share.
//
// "Held nothing" is the ordinary answer, not a failure, and for a reason peculiar to this
// path. The gate leases only build-shaped commands, so every `cat` and `git status` in every
// session reaches this hook with no lease to return. It must be as quiet about that as
// release is about a repeated SubagentStop.
func (d *daemon) finish(req request) any {
	ok := d.table.ReleaseBuild(req.Session, req.ToolUseID)
	if ok {
		d.pump()
	}
	r := ackReply{V: d.speaks, OK: ok, InUse: d.table.InUse()}
	if !ok {
		r.Reason = "no_lease"
	}
	return r
}

// session registers or drops a session.
func (d *daemon) session(req request) any {
	switch req.Action {
	case "start":
		var started time.Time
		if req.PIDStartedAt > 0 {
			started = time.Unix(req.PIDStartedAt, 0)
		}
		d.table.StartSession(lease.Session{
			ID: req.Session, PID: req.PID, PIDStarted: started, Weight: req.Weight,
		})
		return ackReply{V: d.speaks, OK: true, InUse: d.table.InUse()}
	case "end":
		// SessionEnd is what stops a dead session's leases from waiting out their TTL.
		n := d.table.ReleaseSession(req.Session)
		fmt.Fprintf(d.diag, "governor daemon: session %s ended, released %d lease(s)\n", req.Session, n)
		if n > 0 {
			d.pump()
		}
		return ackReply{V: d.speaks, OK: true, InUse: d.table.InUse()}
	default:
		return ackReply{V: d.speaks, Reason: "unknown_action", InUse: d.table.InUse()}
	}
}

// status reports the budget and its holders, for the statusline, doctor and /governor.
func (d *daemon) status(req request) statusReply {
	limit, quotas, over := d.budgetNow(req.Principal)
	snap := d.table.Snapshot()

	sources := make([]sourceStat, 0, len(quotas))
	for _, q := range quotas {
		sources = append(sources, sourceStat{Name: q.Name, Budget: q.Budget, InUse: q.InUse})
	}
	// Each stat is allocated on its own and collected at the end. Holding pointers INTO a
	// slice that is still being appended to is a bug, not a style: one reallocation and every
	// pointer taken before it addresses the abandoned array, so the updates land on memory
	// that is then discarded. It presented as two live sessions silently missing from the
	// merge, which is the direction that under-reports a problem.
	// Read before the snapshot below, not after. A session that registers between the two
	// reads appears live-and-unregistered for one reply, and the statusline caches that for a
	// second — long enough for the first thing a brand-new session ever shows its user to be
	// an accusation that it is broken. Taking the registry first makes the race point the
	// harmless way: a session can only be missing from the registry and present in the table,
	// which classify already treats as registered.
	live, hookAt, liveErr := d.observe()

	byID := map[string]*sessionStat{}
	order := make([]string, 0, len(snap.Sessions))
	for _, s := range snap.Sessions {
		byID[s.ID] = &sessionStat{
			Session: s.ID, PID: s.PID, Weight: s.Weight,
			Tentative: s.Tentative, Confirmed: s.Confirmed,
			BoostedUntil: rfc3339OrEmpty(s.BoostedUntil),
			// A session in the table's snapshot registered unless it is one of the synthetic
			// entries Snapshot invents for a lease whose session it never saw, which carry
			// no PID and no start time. Those are the ones worth telling apart.
			Registered: s.PID != 0 || !s.StartedAt.IsZero(),
		}
		order = append(order, s.ID)
	}

	// Merge in what is actually running. A live session absent from the table above is one
	// the daemon has never heard from, and it is the reason this whole field exists.
	known := false
	if liveErr == nil {
		known = true
		for _, ls := range live {
			st, ok := byID[ls.ID]
			if !ok {
				st = &sessionStat{Session: ls.ID}
				byID[ls.ID] = st
				order = append(order, ls.ID)
			}
			st.Live = true
			st.CWD = ls.CWD
			st.Version = ls.Version
			st.Kind = ls.Kind
			if st.PID == 0 {
				st.PID = ls.PID
			}
			if !ls.Started.IsZero() {
				st.Started = ls.Started.UTC().Format(time.RFC3339)
			}
		}
	}
	sessions := make([]sessionStat, 0, len(order))
	for _, id := range order {
		sessions = append(sessions, *byID[id])
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Session < sessions[j].Session })

	hooks := ""
	if !hookAt.IsZero() {
		hooks = hookAt.UTC().Format(time.RFC3339)
	}
	out := statusReply{
		V: d.speaks, Budget: limit, InUse: snap.InUse, Slots: snap.Slots,
		// Parked acquires, from the FIFO of design.md section 6.2.
		Waiting:           d.waits.len(),
		BinaryVersion:     binaryVersion,
		Build:             d.build,
		Config:            d.statusConfig(),
		Thermal:           d.thermal(),
		Sources:           sources,
		DaemonStarted:     d.started.UTC().Format(time.RFC3339),
		HooksInstalled:    hooks,
		SessionsKnown:     known,
		Sessions:          sessions,
		SpawnSurface:      d.surface.sightings(),
		SpawnSurfaceKnown: true,
		Pinned:            over.pinned(),
		Paused:            over.Paused,
	}
	out.Ceiling, out.Cut = d.cutNow()
	if over.pinned() {
		out.PinnedUntil = over.LimitUntil.UTC().Format(time.RFC3339)
		// What the sources said, which is not what Budget now reports. sourceBudget reads the
		// quotas rather than calling Min a second time: a second call is a second answer, and
		// on a moving budget the two would disagree by a tick for no reason a reader could see.
		out.SourceBudget = sourceBudget(quotas)
	}
	if over.Paused {
		out.PausedUntil = over.PausedUntil.UTC().Format(time.RFC3339)
	}
	d.fillSignals(&out)
	return out
}

// sourceBudget is the tightest budget the sources themselves reported, before any manual pin.
//
// Floor rather than zero for a daemon with no usable source, matching what budget.Min would
// have returned, so that the two numbers a pinned status reply carries are always comparable.
func sourceBudget(quotas []budget.Quota) int {
	best := 0
	for _, q := range quotas {
		if best == 0 || q.Budget < best {
			best = q.Budget
		}
	}
	if best < budget.Floor {
		return budget.Floor
	}
	return best
}

// fillSignals copies the latest reading into a status reply, or leaves every signal null.
//
// Null is the whole point of the separation. A daemon with no sampler wired, or one whose
// first tick has not landed, has measured nothing — and every field here stays absent rather
// than defaulting to a zero that reads as a quiet, idle machine. That distinction is why
// these are pointers and why this is a method rather than four lines inline.
func (d *daemon) fillSignals(out *statusReply) {
	d.signalsMu.Lock()
	defer d.signalsMu.Unlock()
	if !d.latestOK {
		return
	}
	s := d.latest

	if d.cpuAvg != nil {
		if avg, primed := d.cpuAvg.Value(); primed {
			out.CPU = &avg
		}
	}
	instant := s.CPU
	runQueue := s.RunQueue
	capacity := s.CPUCapacity
	memAvail := s.MemAvailableBytes
	swapIn := s.SwapInBytesPerSec
	treeCPU := s.Tree.CPU
	treeRoots := s.Tree.CPURoots

	out.CPUInstant = &instant
	out.RunQueue = &runQueue
	out.CPUCapacity = &capacity
	out.CPUCapacitySource = s.CPUCapacitySource
	out.MemAvailableBytes = &memAvail
	out.SwapInBytesPerSec = &swapIn
	out.TreeCPU = &treeCPU
	out.TreeCPURoots = &treeRoots
}

// sourceName names the binding source for the "source" wire field. With one source it is
// that source; with several it is the one that set the minimum.
func sourceName(quotas []budget.Quota) string {
	name, best := "", 0
	for _, q := range quotas {
		if name == "" || q.Budget < best {
			name, best = q.Name, q.Budget
		}
	}
	return name
}

// loadCalibration reads what `governor calibrate` measured for this host.
//
// IT IS READ ONCE, IN THE DAEMON, AND NEVER ON THE HOT PATH. The gate's whole cost is process
// start (~6 ms against ~0.6 ms of round trip, measured in GOV-15), so adding a file read to
// every Bash call to look up a constant would be spending the one budget that matters on
// information the daemon already has. The daemon holds it and the gate asks the daemon, which
// is the arrangement every other signal already uses.
//
// EVERY FAILURE PATH RETURNS THE ZERO CONFIG, WHICH MEANS THE COMPILED-IN DEFAULTS. A config
// that is missing, unreadable, from the future or malformed all resolve to "this host was
// never calibrated" — a state and not a fault (design.md section 13.4). The alternative is a
// daemon that refuses to start because a tuning file was unreadable, which takes the machine
// ungoverned in order to protect a number that has a working default.
//
// BUT NONE OF THEM IS SILENT (GOV-44). A missing file used to yield an empty Config with no
// error and no log line, so a daemon reading the wrong path ran on shipped defaults for as
// long as it lived and its log looked exactly like a healthy uncalibrated one. Every start now
// names the path it resolved and says what it found there, so the log of any daemon answers
// "which config is this running" without reproducing anything.
func loadCalibration(diag io.Writer) (config.Config, loadedConfig) {
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(diag, "governor daemon: cannot locate config, using built-in defaults: %v\n", err)
		return config.Config{}, loadedConfig{}
	}
	loaded := loadedConfig{Path: path}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Fprintf(diag, "governor daemon: config %s: not found, using built-in defaults\n", path)
		return config.Config{}, loaded
	}
	store := &config.FileStore{Path: path, Warn: func(s string) { fmt.Fprintln(diag, s) }}
	c, err := store.Load(context.Background())
	if err != nil {
		fmt.Fprintf(diag, "governor daemon: config %s: cannot read it, using built-in defaults: %v\n", path, err)
		return config.Config{}, loaded
	}
	loaded.Found = true
	loaded.BMax, loaded.RunQueueMax, loaded.CalibratedAt = c.BMax, c.RunQueueMax, c.CalibratedAt
	if c.CalibratedAt == "" {
		fmt.Fprintf(diag, "governor daemon: config %s: found, never calibrated, using built-in defaults\n", path)
	} else {
		fmt.Fprintf(diag, "governor daemon: config %s: found, calibrated %s (b_max %d, run-queue cut %g)\n",
			path, c.CalibratedAt, c.BMax, c.RunQueueMax)
	}
	return c, loaded
}

// resolveRunQueueMax picks the threshold this daemon cuts at, from three possible sources.
//
// THE ORDER IS ENVIRONMENT, THEN CONFIG, THEN THE COMPILED-IN CONSTANT, and each step has a
// different owner. The environment variable is tools/sweep's experiment handle and has to win,
// because a sweep that silently measured the calibrated value instead of the one it set would
// produce a confident wrong number — which is exactly the failure GOV-31's first pass made and
// had to discard a run over. The config is what `governor calibrate` wrote for this machine.
// The constant is what ships.
//
// GOV-25 IS WHY THE MIDDLE ENTRY EXISTS. budget.Controller.RunQueueMax carried a note that it
// was deliberately NOT part of internal/config, because putting a knob there asserts that the
// right value is host-dependent and that was a claim the sweep had to earn. GOV-31 could not
// earn it: its sweep found the threshold nearly inert, and docs/climb-condition.md §7 records
// why — the CPU disjunct decided every cut before the threshold was consulted. GOV-32 removed
// the disjunct. The claim is now earned by measurement: docs/calibration.md.
func resolveRunQueueMax(c config.Config, diag io.Writer) float64 {
	if v := runQueueMaxOverride(diag); v > 0 {
		return v
	}
	if c.RunQueueMax > 0 {
		fmt.Fprintf(diag, "governor daemon: run-queue threshold %g, calibrated for this host (%s)\n",
			c.RunQueueMax, c.CalibratedAt)
		return c.RunQueueMax
	}
	return 0
}

// ceiling is the most build concurrency this daemon will allow, in slots.
//
// A CALIBRATED B_MAX CAPS THE DERIVED ONE RATHER THAN REPLACING IT, and the asymmetry is
// deliberate. budget.MaxSlots is recomputed from CPUCapacity on every tick, on purpose: a
// cgroup quota is bandwidth that can be changed under a running daemon, and a container
// throttled from eight cores to two must have its ceiling follow it down within the second.
// A calibrated value that simply replaced that would break the one case the per-tick
// recomputation exists for.
//
// So calibration may only lower the ceiling. That is also the only direction it has evidence
// for: the probe measures that this host delivers FEWER effective cores than it advertises —
// six of eight, where four are efficiency cores — and a measurement that says "you have less
// than you think" cannot justify handing out more than the quota allows.
func (d *daemon) ceiling(capacity float64) int {
	max := budget.MaxSlots(capacity)
	if d.calibratedBMax > 0 && d.calibratedBMax < max {
		return d.calibratedBMax
	}
	return max
}

// controlReply answers a `control` request (design.md section 9.1).
//
// It reports the state that RESULTED rather than acknowledging the request, because every one
// of these commands is typed by a person who wants to know what is now true — and because two
// of them can be no-ops. `resume` with nothing pinned and `limit` against a daemon already
// pinned to that number both succeed, and only the resulting state tells them apart.
type controlReply struct {
	V  int  `json:"v"`
	OK bool `json:"ok"`
	// Reason is set only on a refusal, from the same closed-set discipline as the gate's.
	Reason string `json:"reason,omitempty"`
	// Budget is what the gate will admit against from now on.
	Budget int `json:"budget"`
	// SourceBudget is what the control loop would have chosen, present only while pinned, so
	// that the reply can say what the pin is overruling rather than only what it set.
	SourceBudget int    `json:"source_budget,omitempty"`
	Pinned       bool   `json:"pinned"`
	PinnedUntil  string `json:"pinned_until,omitempty"`
	Paused       bool   `json:"paused"`
	PausedUntil  string `json:"paused_until,omitempty"`
	// Weight is this session's share weight after a `boost`, and Boosted says whether the
	// boost actually landed — a boost naming a session the daemon has never seen does not.
	Weight       int    `json:"weight,omitempty"`
	Boosted      bool   `json:"boosted,omitempty"`
	BoostedUntil string `json:"boosted_until,omitempty"`
	// Released says whether `resume` actually had something to release, so the client can tell
	// "I have released your pin" from "there was nothing set".
	Released bool `json:"released,omitempty"`
	InUse    int  `json:"in_use"`
}

// boostWeight is what `governor boost` sets a session's weight to.
//
// Two, because the story says "double this session's weight" and every session starts at 1.
// It is deliberately not a knob: fair share is max-min over integer weights, and a surface
// that let a person type any number would be one where the right move is always to type a
// bigger number than the other sessions. One step, expiring, is an override; an open-ended
// dial is a bidding war with yourself.
const boostWeight = 2

// control applies a manual override (design.md section 9.1).
//
// Every verb here is idempotent and every one of them answers with the resulting state. None
// of them can fail in a way that leaves the daemon partly changed: each is a single guarded
// write to one field, so a malformed request is refused before anything moves.
func (d *daemon) control(req request) any {
	for_ := time.Duration(req.ForMS) * time.Millisecond

	switch req.Control {
	case "limit":
		if req.Limit < 1 {
			// Refused rather than floored silently. A person who typed `limit 0` meant
			// something, and invariant 4 says it cannot be had; answering "1 slot, as you
			// asked" would be the surface agreeing with a request it did not honour.
			return controlReply{V: d.speaks, Reason: "bad_limit", Budget: d.budgetOnly(req), InUse: d.table.InUse()}
		}
		st := d.overrides.setLimit(req.Limit, for_)
		d.event("control limit=%d until=%s", st.Limit, st.LimitUntil.Format(time.RFC3339))
		// Pinning can free capacity as well as remove it — pinning UP is the common case, and
		// a session parked behind the old budget should not wait out its timeout to discover
		// that. The pump is the same one a release calls.
		d.pump()
		return d.controlState(req, st)
	case "pause":
		st := d.overrides.setPaused(for_)
		d.event("control paused until=%s", st.PausedUntil.Format(time.RFC3339))
		d.pump()
		return d.controlState(req, st)
	case "resume":
		had := d.overrides.release()
		d.event("control resume released=%t", had)
		d.pump()
		r := d.controlState(req, d.overrides.state())
		r.Released = had
		return r
	case "boost":
		if for_ <= 0 {
			for_ = defaultOverrideTTL
		} else if for_ > maxOverrideTTL {
			for_ = maxOverrideTTL
		}
		until := time.Now().Add(for_)
		ok := d.table.Boost(req.Session, boostWeight, until)
		d.event("control boost session=%s weight=%d landed=%t", req.Session, boostWeight, ok)
		r := d.controlState(req, d.overrides.state())
		r.Boosted = ok
		if ok {
			r.Weight = boostWeight
			r.BoostedUntil = until.UTC().Format(time.RFC3339)
		} else {
			// Not an error — the daemon simply has no such session, which is what a boost
			// from an unregistered session looks like and is worth saying rather than
			// silently reporting success.
			r.Reason = "unknown_session"
		}
		return r
	default:
		return controlReply{V: d.speaks, Reason: "unknown_control", Budget: d.budgetOnly(req), InUse: d.table.InUse()}
	}
}

// controlState builds the successful reply for whatever is now in force.
func (d *daemon) controlState(req request, st overrideState) controlReply {
	limit, quotas, _ := d.budgetNow(req.Principal)
	r := controlReply{
		V: d.speaks, OK: true, Budget: limit, InUse: d.table.InUse(),
		Pinned: st.pinned(), Paused: st.Paused,
	}
	if st.pinned() {
		r.PinnedUntil = st.LimitUntil.UTC().Format(time.RFC3339)
		r.SourceBudget = sourceBudget(quotas)
	}
	if st.Paused {
		r.PausedUntil = st.PausedUntil.UTC().Format(time.RFC3339)
	}
	return r
}

// budgetOnly is the budget in force, for the refusal replies that still want to report it.
func (d *daemon) budgetOnly(req request) int {
	limit, _, _ := d.budgetNow(req.Principal)
	return limit
}

// rfc3339OrEmpty renders a deadline for the wire, or "" for a zero time.
//
// Empty rather than the year-1 timestamp a zero time.Time formats to: a consumer that saw
// "0001-01-01T00:00:00Z" would have to know to special-case it, and the one that forgot would
// render a boost that expired two thousand years ago as a boost.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
