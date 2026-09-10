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

// defaultBudget is how many concurrent agents the daemon allows until the control loop
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

	// confirm / release
	Lease        string `json:"lease"`
	AgentID      string `json:"agent_id"`
	PID          int    `json:"pid"`
	PIDStartedAt int64  `json:"pid_started_at"`

	// session
	Action string `json:"action"`
	Weight int    `json:"weight"`
}

// The response shapes of design.md section 7, one type each.
//
// One union struct with omitempty everywhere would be less code and worse: it drops a
// status reply's sessions array whenever no session holds a lease, so a consumer has to
// handle both an absent key and an empty one, and it puts fields on confirm and release
// replies that the protocol never specified. The statusline (GOV-10) and doctor (GOV-20)
// read these, so each message says exactly what section 7 says it says.
//
// cpu is a pointer in both places it appears. The daemon has no sampler until GOV-12, and
// reporting 0.0 would tell a statusline the machine is idle — a claim. Null says "not
// measured", which is the truth.

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
	V       int             `json:"v"`
	OK      bool            `json:"ok"`
	Reason  string          `json:"reason"`
	Budget  int             `json:"budget"`
	InUse   int             `json:"in_use"`
	CPU     *float64        `json:"cpu"`
	Thermal signals.Thermal `json:"thermal"`
	Source  string          `json:"source"`
	// Denials is how many times this session has been refused inside denyWindow, counting
	// this one. The gate escalates its reason text on it. It is counted here rather than in
	// the gate because each gate is a separate process that remembers nothing, and this is
	// the only place that can see a session's refusals across all of them.
	Denials int `json:"denials"`
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
	V       int             `json:"v"`
	Budget  int             `json:"budget"`
	InUse   int             `json:"in_use"`
	Waiting int             `json:"waiting"`
	CPU     *float64        `json:"cpu"`
	Thermal signals.Thermal `json:"thermal"`
	Sources []sourceStat    `json:"sources"`
	// DaemonStarted is when this daemon came up, RFC3339. See daemon.started.
	DaemonStarted string `json:"daemon_started,omitempty"`
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
	// liveSessions enumerates the Claude Code sessions running on this machine, and
	// hooksInstalledAt reports when the gate's hooks file was written. Both are injectable
	// because both read a directory this package does not own, and a test that had to
	// fabricate a live process to exercise the merge would be a test nobody trusts.
	liveSessions     func() ([]signals.LiveSession, error)
	hooksInstalledAt func() time.Time
	// machineMu guards the cached answer below.
	machineMu   sync.Mutex
	machineAt   time.Time
	machineSeen []signals.LiveSession
	machineErr  error
	machineHook time.Time
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := newDaemon(*limit, stderr)
	// The one place the daemon is allowed to look at the real machine.
	d.observeThisMachine()
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
	fmt.Fprintf(stdout, "governor daemon listening on %s (budget %d)\n", ep, *limit)

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
		table:            table,
		sources:          []budget.Source{budget.LocalSource{N: limit, InUse: table.InUse}},
		thermal:          signals.ReadThermal,
		waits:            &waitQueue{},
		denials:          newDenyLog(nil),
		diag:             &syncWriter{w: diag},
		started:          time.Now(),
		liveSessions:     noLiveSessions,
		hooksInstalledAt: func() time.Time { return time.Time{} },
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
	if req.V != protocolVersion {
		return versionReply{V: protocolVersion, Reason: "unsupported_version", Supported: []int{protocolVersion}}
	}
	switch req.Op {
	case "acquire":
		return d.acquire(req)
	case "confirm":
		return d.confirm(req)
	case "release":
		return d.release(req)
	case "session":
		return d.session(req)
	case "status":
		return d.status()
	default:
		return ackReply{V: protocolVersion, Reason: "unknown_op", InUse: d.table.InUse()}
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

// acquire takes a tentative lease if the budget has room, and decides what to do when it
// does not: refuse the caller outright, refuse it for capacity, or park it.
func (d *daemon) acquire(req request) any {
	limit, quotas := budget.Min(context.Background(), d.sources)
	src := sourceName(quotas)

	// The depth limit comes first, and deliberately never waits. A nested spawn is not
	// short of a slot, it is not allowed one, and parking it for twenty seconds before
	// saying so would stall a session to reach an answer that was already known.
	if req.Depth > maxFanOutDepth {
		d.event("deny session=%s reason=depth", req.Session)
		return refuseReply{
			V: protocolVersion, Reason: "depth",
			Budget: limit, InUse: d.table.InUse(), Source: src,
			CPU: nil, Thermal: d.thermal(),
		}
	}

	if l, ok := d.table.Acquire(req.Session, req.Depth, limit); ok {
		return d.granted(l, limit, src)
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
		return d.refuseForCapacity(req.Session, limit, src)
	}

	d.event("park session=%s in_use=%d/%d wait=%v", req.Session, d.table.InUse(), limit, wait)
	parked := time.Now()

	w := &waiter{session: req.Session, depth: req.Depth, granted: make(chan lease.Lease, 1)}
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
			V: protocolVersion, Reason: "waiting",
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
				return d.refuseForCapacity(req.Session, limit, src)
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

// granted builds the reply for a lease that has been taken, however it was taken.
func (d *daemon) granted(l lease.Lease, limit int, src string) grantReply {
	inUse := d.table.InUse()
	d.notePeak(inUse, limit)
	return grantReply{
		V: protocolVersion, OK: true, Lease: l.ID,
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
func (d *daemon) refuseForCapacity(session string, limit int, src string) refuseReply {
	r := refuseReply{
		V: protocolVersion, Reason: "capacity",
		Budget: limit, InUse: d.table.InUse(), Source: src,
		CPU: nil, Thermal: d.thermal(),
		Denials: d.denials.record(session),
	}
	// Logged here rather than at each call site so that a refusal added later cannot be one
	// the log does not know about. This is the event that costs the model something, and it
	// is the one a week of dogfooding is read for.
	d.event("deny session=%s reason=capacity in_use=%d/%d denials=%d", session, r.InUse, r.Budget, r.Denials)
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
func (d *daemon) pump() {
	d.waits.mu.Lock()
	defer d.waits.mu.Unlock()

	for len(d.waits.q) > 0 {
		w := d.waits.q[0]
		limit, _ := budget.Min(context.Background(), d.sources)
		l, ok := d.table.Acquire(w.session, w.depth, limit)
		if !ok {
			return
		}
		d.waits.q = d.waits.q[1:]
		// Never blocks: granted is buffered, and a waiter is removed from the queue at the
		// same time as it is written to, so it is written to at most once.
		w.granted <- l
	}
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
		return ackReply{V: protocolVersion, Reason: "no_pending_lease", InUse: d.table.InUse()}
	}
	return ackReply{V: protocolVersion, OK: true, Lease: l.ID, InUse: d.table.InUse()}
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
	r := ackReply{V: protocolVersion, OK: ok, InUse: d.table.InUse()}
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
		return ackReply{V: protocolVersion, OK: true, InUse: d.table.InUse()}
	case "end":
		// SessionEnd is what stops a dead session's leases from waiting out their TTL.
		n := d.table.ReleaseSession(req.Session)
		fmt.Fprintf(d.diag, "governor daemon: session %s ended, released %d lease(s)\n", req.Session, n)
		if n > 0 {
			d.pump()
		}
		return ackReply{V: protocolVersion, OK: true, InUse: d.table.InUse()}
	default:
		return ackReply{V: protocolVersion, Reason: "unknown_action", InUse: d.table.InUse()}
	}
}

// status reports the budget and its holders, for the statusline, doctor and /governor.
func (d *daemon) status() statusReply {
	limit, quotas := budget.Min(context.Background(), d.sources)
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
	return statusReply{
		V: protocolVersion, Budget: limit, InUse: snap.InUse,
		// Parked acquires, from the FIFO of design.md section 6.2.
		Waiting: d.waits.len(),
		// Null until the daemon has a sampler (GOV-12). Zero would read as an idle machine.
		CPU:            nil,
		Thermal:        d.thermal(),
		Sources:        sources,
		DaemonStarted:  d.started.UTC().Format(time.RFC3339),
		HooksInstalled: hooks,
		SessionsKnown:  known,
		Sessions:       sessions,
	}
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
