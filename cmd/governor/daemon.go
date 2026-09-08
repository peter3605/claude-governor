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
// What is deliberately NOT here: the control loop. The budget is a constant in this story
// on purpose, because a moving budget debugged against a moving lease table is two unknowns
// at once (GOV-14 lands AIMD). Also absent are the wait queue and max-min fair share
// (section 6.1, 6.2) and the depth limit (section 6.3) — this story serves the five
// operations and keeps the lease table honest under concurrency.

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
type statusReply struct {
	V        int             `json:"v"`
	Budget   int             `json:"budget"`
	InUse    int             `json:"in_use"`
	Waiting  int             `json:"waiting"`
	CPU      *float64        `json:"cpu"`
	Thermal  signals.Thermal `json:"thermal"`
	Sources  []sourceStat    `json:"sources"`
	Sessions []sessionStat   `json:"sessions"`
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
	// diag is where a handler's complaints go. It is never the reply: a client that sends
	// rubbish is told nothing, because there is nothing useful to tell it and a broker that
	// echoes attacker-controlled input is a broker that can be used to write to a log.
	diag io.Writer
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
	ln, err := transport.Listen{}.Listen(ctx, ep)
	if err != nil {
		fmt.Fprintf(stderr, "governor daemon: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "governor daemon listening on %s (budget %d)\n", ep, *limit)

	d.serve(ctx, ln)
	return 0
}

// newDaemon builds a daemon with the v1 budget: a slice of sources with exactly one member,
// reduced by min().
//
// The slice is the point. One source does not need a reducer, but the plural is the whole
// of the tier-two seam (design.md section 12.1) — a remote source governing tokens and
// spend is added at this line and nothing downstream changes, because every reader already
// takes the minimum over a set.
func newDaemon(limit int, diag io.Writer) *daemon {
	table := lease.NewTable(lease.Options{Liveness: lease.PIDLiveness{}})
	return &daemon{
		table:   table,
		sources: []budget.Source{budget.LocalSource{N: limit, InUse: table.InUse}},
		thermal: signals.ReadThermal,
		diag:    diag,
	}
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

	out, err := json.Marshal(d.dispatch(req))
	if err != nil {
		fmt.Fprintf(d.diag, "governor daemon: encode reply: %v\n", err)
		return
	}
	if _, err := conn.Write(append(out, '\n')); err != nil {
		fmt.Fprintf(d.diag, "governor daemon: write reply: %v\n", err)
	}
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

// acquire takes a tentative lease if the budget has room.
func (d *daemon) acquire(req request) any {
	limit, quotas := budget.Min(context.Background(), d.sources)

	l, ok := d.table.Acquire(req.Session, req.Depth, limit)
	if !ok {
		return refuseReply{
			V: protocolVersion, Reason: "capacity",
			Budget: limit, InUse: d.table.InUse(), Source: sourceName(quotas),
			CPU: nil, Thermal: d.thermal(),
		}
	}
	return grantReply{
		V: protocolVersion, OK: true, Lease: l.ID,
		Budget: limit, InUse: d.table.InUse(),
		// v1's allocation policy is "no division": there is no max-min fair share and no
		// wait queue yet (design.md section 6.1, 6.2), so a session's share is the whole
		// budget and the first to ask is the first served. This is the accurate description
		// of the policy, not a placeholder — when allocation lands it changes this number
		// rather than adding the field.
		Share:  limit,
		Source: sourceName(quotas),
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
	sessions := make([]sessionStat, 0, len(snap.Sessions))
	for _, s := range snap.Sessions {
		sessions = append(sessions, sessionStat{
			Session: s.ID, PID: s.PID, Weight: s.Weight,
			Tentative: s.Tentative, Confirmed: s.Confirmed,
		})
	}
	return statusReply{
		V: protocolVersion, Budget: limit, InUse: snap.InUse,
		// There is no wait queue in v1 (design.md section 6.2), so nobody is ever waiting.
		Waiting: 0,
		// Null until the daemon has a sampler (GOV-12). Zero would read as an idle machine.
		CPU:      nil,
		Thermal:  d.thermal(),
		Sources:  sources,
		Sessions: sessions,
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
