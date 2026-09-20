package main

// GOV-26: prove the tier-two seams against a stub remote budget source.
//
// design.md section 12 designs a second tier and does not build it. Every seam it needs —
// transport, identity, budget source, liveness, signals, config — exists today as one
// interface with exactly one implementation, and section 12.2 states the rule plainly: a
// change that only makes sense for tier two goes behind an interface, and if it needs a
// second implementation to compile, it is premature.
//
// The hazard that rule creates is the reason this file exists. An interface with one
// implementation has never been asked to abstract anything, so it abstracts whatever that
// one implementation happens to do. It looks like a seam and is not one, and nobody finds
// out until the day a second implementation is urgent.
//
// So: once, as a test, a real gate talks to a real daemon which talks to a stub remote
// source over tcp. NOTHING HERE SHIPS. Everything in this file is behind the _test.go
// suffix, which means the compiler — not a reviewer's memory — is what keeps it out of the
// released binary. This is deliberately not a remote broker, and must not become one: tier
// two is not justified until the local governor has been in daily use and the token and
// spend problem is actually felt.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/identity"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// ---------------------------------------------------------------------------
// The stub: a tcp dialer, a remote source, and a broker to talk to.
// ---------------------------------------------------------------------------

// tcpDial is a second implementation of transport.Dialer, and the only one that will ever
// exist beside transport.Dial until tier two does.
//
// It is what proves the transport seam is a seam. transport.Dial refuses every scheme but
// unix:// — correctly, since the gate's answer to an unknown endpoint is to fail open and
// connecting to something unintended is far worse than not connecting at all. The question
// this answers is whether anything ABOVE the Dialer interface had quietly assumed a path
// rather than a URL. Nothing had: the interface is (ctx, endpoint string) -> net.Conn and a
// tcp implementation drops straight in.
type tcpDial struct{}

func (tcpDial) Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "tcp" {
		return nil, fmt.Errorf("tcpDial speaks tcp://, not %q", u.Scheme)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", u.Host)
}

// remoteSource is a budget.Source that asks a broker over the network.
//
// This is the shape tier two would take: it governs a number that genuinely aggregates
// across machines, it is asked per principal, and it is a SOFT dependency — every failure
// path returns an error so that budget.Min skips it and the machine degrades to its local
// budget, for the same reason the gate fails open when the daemon is unreachable.
type remoteSource struct {
	name     string
	endpoint string
	dialer   transport.Dialer
}

// quotaAsk is the request this stub protocol sends. It carries v and principal because
// those are the two fields design.md insists every message carries — v so that a version
// mismatch is answered rather than ignored, and principal because a source governing an
// account's spend has no answer without one.
type quotaAsk struct {
	V         int                `json:"v"`
	Op        string             `json:"op"`
	Principal identity.Principal `json:"principal"`
}

type quotaAnswer struct {
	V      int    `json:"v"`
	Budget int    `json:"budget"`
	InUse  int    `json:"in_use"`
	Error  string `json:"error,omitempty"`
}

func (r remoteSource) Quota(ctx context.Context, who identity.Principal) (budget.Quota, error) {
	conn, err := r.dialer.Dial(ctx, r.endpoint)
	if err != nil {
		return budget.Quota{}, fmt.Errorf("dial %s: %w", r.endpoint, err)
	}
	defer conn.Close()

	// The context's deadline is the whole budget for this exchange. budget.Min caps it at
	// MaxWait even when the caller passed a context with no deadline at all, which is what
	// makes a silent broker a degradation rather than a stall.
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	line, err := json.Marshal(quotaAsk{V: protocolVersion, Op: "quota", Principal: who})
	if err != nil {
		return budget.Quota{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return budget.Quota{}, fmt.Errorf("write: %w", err)
	}

	raw, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return budget.Quota{}, fmt.Errorf("read: %w", err)
	}
	var ans quotaAnswer
	if err := json.Unmarshal([]byte(raw), &ans); err != nil {
		return budget.Quota{}, fmt.Errorf("decode %q: %w", raw, err)
	}
	if ans.Error != "" {
		return budget.Quota{}, fmt.Errorf("broker: %s", ans.Error)
	}
	if ans.V != protocolVersion {
		// Answered, not ignored (design.md section 7): an unrecognised version is treated
		// like an unreachable source, which degrades to the local budget.
		return budget.Quota{}, fmt.Errorf("broker speaks v%d, this binary speaks v%d", ans.V, protocolVersion)
	}
	return budget.Quota{Name: r.name, Budget: ans.Budget, InUse: ans.InUse}, nil
}

// stubBroker is the tier-two service that does not exist, standing in for one long enough
// to answer a question.
type stubBroker struct {
	ln net.Listener
	// done is closed by stop. A silent connection parks on it rather than on a channel
	// nobody holds, so the goroutine ends with the test instead of outliving the run.
	done chan struct{}

	mu     sync.Mutex
	budget int
	asks   []identity.Principal
	silent bool // accept the connection and never answer
	closed bool
}

// startStubBroker serves the stub protocol on a loopback tcp port and returns its endpoint.
//
// 127.0.0.1:0 rather than a fixed port: the kernel picks a free one, so two tests running
// in parallel cannot collide, and nothing listens on an address another machine can reach.
func startStubBroker(t *testing.T, budgetSlots int) *stubBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	b := &stubBroker{ln: ln, done: make(chan struct{}), budget: budgetSlots}
	t.Cleanup(b.stop)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serveConn(c)
		}
	}()
	return b
}

func (b *stubBroker) serveConn(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return
	}
	var ask quotaAsk
	if err := json.Unmarshal([]byte(raw), &ask); err != nil {
		return
	}

	b.mu.Lock()
	b.asks = append(b.asks, ask.Principal)
	silent, n := b.silent, b.budget
	b.mu.Unlock()

	if silent {
		// The failure shape a refused connection does not cover: the broker is up, the
		// connection succeeds, and no answer ever comes. Holding the connection open is the
		// whole point — closing it would be an error the source sees immediately.
		<-b.done
		return
	}

	out, _ := json.Marshal(quotaAnswer{V: protocolVersion, Budget: n, InUse: 0})
	c.Write(append(out, '\n'))
}

func (b *stubBroker) endpoint() string { return "tcp://" + b.ln.Addr().String() }

func (b *stubBroker) source(name string) budget.Source {
	return remoteSource{name: name, endpoint: b.endpoint(), dialer: tcpDial{}}
}

// goSilent makes the broker accept and never answer, from the next connection on.
func (b *stubBroker) goSilent() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.silent = true
}

// seen reports every principal the broker has been asked on behalf of.
func (b *stubBroker) seen() []identity.Principal {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]identity.Principal(nil), b.asks...)
}

// stop kills the broker. This is "the remote dies": the listener closes, and a source that
// dials it afterwards gets a refused connection rather than a hang.
func (b *stubBroker) stop() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	close(b.done)
	b.ln.Close()
}

// ---------------------------------------------------------------------------
// The assertions.
// ---------------------------------------------------------------------------

// buildPayload is a PreToolUse for a build-shaped Bash command, which the gate classifies
// and leases against build slots. A whole-slot request is what makes the budget arithmetic
// below legible: with a budget of N, the (N+1)th is refused.
func buildPayload(session, toolUseID, cmd string) string {
	in, _ := json.Marshal(map[string]any{"command": cmd, "description": "build"})
	p, _ := json.Marshal(map[string]any{
		"session_id":      session,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      json.RawMessage(in),
		"tool_use_id":     toolUseID,
	})
	return string(p)
}

// TestTheEffectiveBudgetIsTheMinimumOfLocalAndRemote is the first Done-when clause, end to
// end: a real gate, a real daemon over a real socket, and a stub source over tcp.
//
// Both directions are asserted, and the second is the one that matters. A test where the
// remote is always tighter would pass against a daemon that ignored the local source
// entirely; a test where the local is always tighter would pass against a daemon that had
// never connected to the broker at all. Only the pair distinguishes min() from either.
func TestTheEffectiveBudgetIsTheMinimumOfLocalAndRemote(t *testing.T) {
	tests := []struct {
		name       string
		local      int
		remote     int
		wantBudget int
		wantSource string
	}{
		{name: "remote is tighter", local: 6, remote: 2, wantBudget: 2, wantSource: "remote"},
		{name: "local is tighter", local: 2, remote: 6, wantBudget: 2, wantSource: "local"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortWait(t, 100*time.Millisecond)
			broker := startStubBroker(t, tc.remote)

			endpoint, d := startTestDaemonConfigured(t, tc.local, lease.Options{}, func(d *daemon) {
				// The whole of the tier-two integration, exactly as design.md section 12
				// promises: one more member in the slice. Nothing downstream changes,
				// because every reader already takes the minimum over a set.
				d.sources = append(d.sources, broker.source("remote"))
			})

			// The status reply reports every source's answer, so this reads the budget the
			// daemon actually computed rather than inferring it from a refusal.
			got := d.status(request{Principal: localPrincipal()})
			if got.Budget != tc.wantBudget {
				t.Errorf("effective budget = %d, want min(%d, %d) = %d", got.Budget, tc.local, tc.remote, tc.wantBudget)
			}
			if len(got.Sources) != 2 {
				t.Fatalf("status reported %d sources, want both local and remote: %+v", len(got.Sources), got.Sources)
			}

			byName := map[string]int{}
			for _, s := range got.Sources {
				byName[s.Name] = s.Budget
			}
			if byName["local"] != tc.local {
				t.Errorf("local source reported %d, want %d", byName["local"], tc.local)
			}
			if byName["remote"] != tc.remote {
				t.Errorf("remote source reported %d, want %d", byName["remote"], tc.remote)
			}

			// And now through the real gate: fill the budget, then assert the next build is
			// refused and the refusal names the source that was actually tightest.
			for i := 0; i < tc.wantBudget; i++ {
				out := runGateGrant(t, endpoint, buildPayload("s1", fmt.Sprintf("toolu_fill_%d", i), "go build ./..."))
				if out != "" {
					t.Fatalf("build %d was refused while the budget had room: %s", i, out)
				}
			}
			deny := runGateDeny(t, endpoint, buildPayload("s1", "toolu_over", "go build ./..."))
			if deny.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("permissionDecision = %q, want deny once min(%d, %d) is full",
					deny.HookSpecificOutput.PermissionDecision, tc.local, tc.remote)
			}
		})
	}
}

// TestThePrincipalArrivesIntactAtTheRemoteSource is the second Done-when clause, and it is
// the seam that was actually broken.
//
// The field had been on the wire since v1 and gate.go said so in as many words: "It is
// single-user in v1 and nothing reads it." That was true all the way down. The daemon
// decoded request.Principal and never referred to it again, and budget.Source.Quota took a
// context and nothing else — so there was no parameter a principal could have travelled in
// even if someone had wanted to send one. Identity was a seam in the sense that a struct
// field existed, and in no other sense.
//
// Note what this asserts: not that the daemon can be handed a principal, but that one put on
// the wire BY A REAL GATE reaches a budget source. The gate resolves it from the environment
// (localPrincipal), so the assertion is against that same value rather than a fixture.
func TestThePrincipalArrivesIntactAtTheRemoteSource(t *testing.T) {
	shortWait(t, 100*time.Millisecond)
	broker := startStubBroker(t, 4)
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.sources = append(d.sources, broker.source("remote"))
	})

	if out := runGateGrant(t, endpoint, buildPayload("s1", "toolu_p1", "go build ./...")); out != "" {
		t.Fatalf("build refused with a budget of 4: %s", out)
	}

	asks := broker.seen()
	if len(asks) == 0 {
		t.Fatal("the remote source was never asked: the daemon did not consult it at all")
	}
	want := localPrincipal()
	for i, got := range asks {
		if got.User == "" {
			t.Fatalf("ask %d arrived with an empty user; the gate sent %q", i, want.User)
		}
		if got.User != want.User {
			t.Errorf("ask %d carried user %q, want %q", i, got.User, want.User)
		}
	}
}

// A team survives the whole path, including its absence.
//
// The gate is single-user and sends a nil team, so the real-gate test above cannot tell a
// preserved nil from a dropped field. This drives the socket directly to cover both: a named
// team must arrive named, and no team must arrive as no team. The wire distinguishes the two
// and a personal account read as a team account is a billing mistake, not a rounding one.
func TestTheTeamSurvivesToTheRemoteSourceIncludingItsAbsence(t *testing.T) {
	shortWait(t, 100*time.Millisecond)

	for _, tc := range []struct {
		name string
		team *string
	}{
		{name: "a named team", team: strptr("platform")},
		{name: "no team", team: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := startStubBroker(t, 4)
			endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
				d.sources = append(d.sources, broker.source("remote"))
			})

			req, err := json.Marshal(request{
				V:         protocolVersion,
				Op:        "status",
				Principal: principal{User: "someone", Team: tc.team},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := callRaw(t, endpoint, string(req)+"\n"); err != nil {
				t.Fatalf("status: %v", err)
			}

			asks := broker.seen()
			if len(asks) != 1 {
				t.Fatalf("broker was asked %d times, want exactly 1", len(asks))
			}
			got := asks[0].Team
			switch {
			case tc.team == nil && got != nil:
				t.Fatalf("no team arrived as team %q, want nil", *got)
			case tc.team != nil && got == nil:
				t.Fatalf("team %q arrived as nil", *tc.team)
			case tc.team != nil && *got != *tc.team:
				t.Fatalf("team arrived as %q, want %q", *got, *tc.team)
			}
		})
	}
}

// TestKillingTheRemoteDegradesToTheLocalBudget is the third Done-when clause and, per the
// story's traps, the important one: a remote budget source must be a soft dependency for
// exactly the same reason the local daemon is.
//
// Two deaths, because they are genuinely different failures and only one of them was
// survivable before this story. A broker that is GONE refuses the connection instantly and
// budget.Min skips it — that path worked. A broker that is UP AND SILENT accepts the
// connection and never replies, and every budget.Min call site in the daemon passed
// context.Background(), which has no deadline: the promise in Source's own doc comment that
// a slow source "is skipped, never waited on" was resting on a deadline nobody had set.
func TestKillingTheRemoteDegradesToTheLocalBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill func(*stubBroker)
	}{
		{name: "the broker is gone", kill: (*stubBroker).stop},
		{name: "the broker is up and silent", kill: (*stubBroker).goSilent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortWait(t, 100*time.Millisecond)
			broker := startStubBroker(t, 1)
			endpoint, d := startTestDaemonConfigured(t, 5, lease.Options{}, func(d *daemon) {
				d.sources = append(d.sources, broker.source("remote"))
			})

			// While it is alive it really is tighter, so the degradation below is a change
			// and not the absence of an effect.
			if got := d.status(request{Principal: localPrincipal()}); got.Budget != 1 {
				t.Fatalf("with a live remote the budget is %d, want the remote's 1", got.Budget)
			}

			tc.kill(broker)

			start := time.Now()
			got := d.status(request{Principal: localPrincipal()})
			elapsed := time.Since(start)

			if got.Budget != 5 {
				t.Errorf("after the remote died the budget is %d, want the local 5 — a dead source must degrade, not decide", got.Budget)
			}
			if len(got.Sources) != 1 || got.Sources[0].Name != "local" {
				t.Errorf("status reported sources %+v, want only the one that answered", got.Sources)
			}
			// The failure being guarded is unbounded, not slow; the bound is generous.
			if limit := 10 * budget.MaxWait; elapsed > limit {
				t.Fatalf("status took %v with a dead remote, want under %v — this is the hot path and it must not stall", elapsed, limit)
			}

			// And the machine still governs: a real gate still gets a decision, promptly.
			if out := runGateGrant(t, endpoint, buildPayload("s1", "toolu_after", "go build ./...")); out != "" {
				t.Fatalf("after the remote died a build was refused at local budget 5: %s", out)
			}
		})
	}
}

// TestADeadRemoteIsAskedOncePerPumpAndNotOncePerWaiter is the same soft-dependency rule
// applied to the one place a stall would have been worst.
//
// pump() holds the wait-queue lock across its whole walk, and every fresh acquire consults
// that same queue (design.md section 6.2, and the back door GOV-18 closed). The budget used
// to be read INSIDE that walk, once per waiter — so a remote source that went quiet would be
// dialled under the lock once for every waiter, and the cost of a silent remote would scale
// with the length of the queue, in the one code path whose job is to drain it. With a
// LocalSource reading memory that cost was zero, which is precisely why it took a second
// implementation to see it.
//
// THE QUEUE IS DELIBERATELY LONG. The first version of this test pumped an EMPTY queue and
// passed against every version of the code, including the one with the defect, because pump
// returns before reading the budget at all when nobody is waiting. A timing assertion over
// one waiter is barely better: it cannot tell one MaxWait from another. Only a queue of
// several separates "asked once" from "asked once per waiter", and that separation is the
// entire claim.
func TestADeadRemoteIsAskedOncePerPumpAndNotOncePerWaiter(t *testing.T) {
	const waiters = 5

	broker := startStubBroker(t, 4)
	_, d := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.sources = append(d.sources, broker.source("remote"))
	})

	parked := make([]*waiter, 0, waiters)
	for i := 0; i < waiters; i++ {
		name := fmt.Sprintf("s%d", i)
		w := &waiter{
			session: name,
			who:     principal{User: "peter"},
			req:     lease.Request{Session: name, Kind: lease.Build, Weight: 1, ToolUseID: name},
			granted: make(chan lease.Lease, 1),
		}
		parked = append(parked, w)
		d.waits.enqueue(w)
	}

	// Silent only now, so the queue was built against a live broker and the timing below is
	// the pump's alone.
	broker.goSilent()

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		d.pump()
		done <- time.Since(start)
	}()

	var took time.Duration
	select {
	case took = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not return against a silent remote: the wait queue is wedged, and every acquire on this machine is queued behind it")
	}

	// One MaxWait, not five. The bound is generous against a loaded CI box but still far
	// below the waiters x MaxWait the per-waiter lookup would cost.
	if limit := 3 * budget.MaxWait; took > limit {
		t.Fatalf("pump took %v for %d waiters, want under %v: the budget is being read once per waiter rather than once per pump",
			took, waiters, limit)
	}

	// It degraded rather than stopping: the local budget of 4 still served four of the five.
	served := 0
	for _, w := range parked {
		if len(w.granted) > 0 {
			served++
		}
	}
	if served != 4 {
		t.Errorf("pump served %d waiters against a local budget of 4, want 4 — a silent remote must degrade, not decide", served)
	}
}

// TestTheStubShipsInNothing is the last Done-when clause.
//
// The real enforcement is structural and stronger than any assertion here: every type in
// this file is declared in a _test.go file, so the released binary cannot reference one and
// still compile. What this adds is the case that would not be caught that way — a future
// session moving a helper out of a test file to "reuse" it, which is the first step of the
// stub becoming the remote broker section 12.2 says not to build.
func TestTheStubShipsInNothing(t *testing.T) {
	// transport.Dial is still the only Dialer the shipped code has, and it still refuses
	// every scheme but unix://. If this ever stops being true without tier two existing,
	// v1 has grown a network client nobody asked for.
	if _, err := (transport.Dial{}).Dial(context.Background(), "tcp://127.0.0.1:1"); err == nil {
		t.Fatal("transport.Dial accepted a tcp:// endpoint: v1 speaks unix:// and nothing else")
	} else if !strings.Contains(err.Error(), "unsupported endpoint scheme") {
		t.Errorf("transport.Dial refused tcp:// with %v, want the scheme error", err)
	}

	// And the shipped daemon is still configured with exactly one source. The stub is added
	// by a test hook and never by newDaemon.
	d := newDaemon(4, io.Discard)
	if len(d.sources) != 1 {
		t.Fatalf("newDaemon built %d budget sources, want exactly the one local source: tier two is not built (design.md section 12.2)", len(d.sources))
	}
}

func strptr(s string) *string { return &s }

// runGateGrant runs the real gate and returns what it wrote to stdout: empty on a grant,
// the refusal object otherwise.
//
// The grant path is exit 0 AND SILENCE — never permissionDecision "allow", which would
// suppress the user's own permission prompts as a side effect. So "was it granted" is
// exactly "was stdout empty", and that is what this returns rather than a bool, so a caller
// that expected a grant can print what it got instead.
func runGateGrant(t *testing.T, endpoint, payload string) string {
	t.Helper()
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr bytes.Buffer
	var code int
	took := timed(func() {
		code = gate(strings.NewReader(payload), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0: the gate must never fail a spawn (stderr: %s)", code, stderr.String())
	}
	if took.exceeds(failOpenCeiling) {
		t.Errorf("took %v, want under %v", took, failOpenCeiling)
	}
	return strings.TrimSpace(stdout.String())
}
