package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// Block-and-wait, the deny reason and the depth limit (GOV-08, design.md sections 4.2, 6.2
// and 6.3).
//
// The load-bearing test in this file is TestAWedgedDaemonFailsOpenEvenThoughTheGateAskedToWait.
// Everything else here is a feature; that one is the invariant. The obvious way to build
// block-and-wait — extend the read deadline whenever the gate asks for a wait — passes every
// other test in this file and silently turns a wedged daemon into a twenty-second stall on
// every subagent spawn on the machine. It is exactly the regression the GOV-05 suite says
// it exists to catch, and it would not be caught by anything that only tested the wait
// working.

// shortWait shrinks the gate's wait to something a test can sit through, and puts it back.
//
// The gate really does wait for the full 20 s otherwise: two GOV-07 tests started taking
// exactly 20 s each the moment this story landed, which is the feature working rather than
// a fault, but it is not something a suite should pay for on every run.
func shortWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := gateWaitMax
	gateWaitMax = d
	t.Cleanup(func() { gateWaitMax = prev })
}

// TestAWedgedDaemonFailsOpenEvenThoughTheGateAskedToWait is the invariant, not the feature.
//
// The daemon here does what a wedged daemon does: it accepts the connection, reads the
// request — which asks to be held for the full wait — and then says nothing at all, ever.
// A gate that extended its deadline on the strength of its own request would sit here for
// twenty seconds, and would do so on every spawn on the machine. The extension has to be
// bought with an answer, and this daemon never gives one.
func TestAWedgedDaemonFailsOpenEvenThoughTheGateAskedToWait(t *testing.T) {
	asked := make(chan int, 1)
	endpoint := listenerFunc(t, func(c net.Conn, request []byte) {
		var req acquireRequest
		json.Unmarshal(request, &req)
		asked <- req.WaitMS
		// Deliberately no reply, and the connection is held open. Closing it would let the
		// gate fail open on EOF and prove nothing about the deadline.
		time.Sleep(2 * time.Second)
	})

	// The full 20 s, not a shrunk one: the whole question is what happens when the gate has
	// asked for a long wait, so asking for a short one would test the wrong thing.
	runGateFor(t, endpoint, samplePayload, transport.Dial{})

	select {
	case ms := <-asked:
		if ms <= 0 {
			t.Fatalf("wait_ms = %d; this test is only meaningful when the gate asked to wait", ms)
		}
	case <-time.After(time.Second):
		t.Fatal("the daemon never received the request")
	}
	// runGateFor has already asserted exit 0, empty stdout, and the fail-open ceiling.
}

// A daemon that promises an answer and does not produce one is a broken daemon, and a
// broken daemon means the spawn proceeds. The gate waits for what it was promised and then
// gives up — it does not wait forever on the strength of the promise.
func TestAnUnkeptPromiseToAnswerFailsOpen(t *testing.T) {
	shortWait(t, 100*time.Millisecond)

	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
		c.Write([]byte(`{"v":1,"ok":false,"reason":"waiting","wait_ms":100}` + "\n"))
		time.Sleep(5 * time.Second)
	})
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr strings.Builder
	var code int
	took := timed(func() {
		code = gate(strings.NewReader(samplePayload), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty: an unanswered promise is a failure, and failures fail open", stdout.String())
	}
	// The floor is asserted on the raw elapsed time and not on took.own(): a busy runner
	// cannot make the gate give up sooner than it promised, so there is nothing to discount.
	if took.elapsed < 100*time.Millisecond {
		t.Errorf("gave up after %v, before the wait it was promised", took.elapsed)
	}
	if ceiling := 100*time.Millisecond + waitGrace + failOpenCeiling; took.exceeds(ceiling) {
		t.Errorf("took %v, want under %v: the wait is bounded by what the gate offered, not by the daemon",
			took, ceiling)
	}
}

// A daemon cannot extend the wait indefinitely by parking the request again and again.
func TestTheGateWillNotBeParkedTwice(t *testing.T) {
	shortWait(t, 100*time.Millisecond)

	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
		c.Write([]byte(`{"v":1,"ok":false,"reason":"waiting","wait_ms":100}` + "\n"))
		c.Write([]byte(`{"v":1,"ok":false,"reason":"waiting","wait_ms":100}` + "\n"))
		time.Sleep(2 * time.Second)
	})
	t.Setenv(transport.EndpointEnv, endpoint)

	var stdout, stderr strings.Builder
	took := timed(func() {
		gate(strings.NewReader(samplePayload), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
	})

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "parked the request twice") {
		t.Errorf("stderr = %q, want it to name the second parking", stderr.String())
	}
	if took.exceeds(failOpenCeiling + 100*time.Millisecond) {
		t.Errorf("took %v; a second parking must be rejected at once, not waited out", took)
	}
}

// TestAParkedSpawnProceedsWhenASlotFrees is the middle row of design.md section 4.2's
// decision table, and the reason the wait exists at all: the session stalls briefly and the
// spawn then proceeds, costing no tokens and requiring the model to reason about nothing.
//
// IT ASSERTS THE ORDER OF TWO EVENTS AND NOT THEIR TIMING, which is what GOV-B16 rewrote.
// The release used to be fired by a 150 ms timer and the outcome measured from a
// `time.Now()` taken afterwards on this goroutine — two different epochs. Measured 2026-09-24
// by injecting the skew directly:
//
//	skew   elapsed from start   elapsed from the timer's own epoch   grant-release
//	   0            151.0 ms                             151.9 ms        +922 µs
//	40 ms           110.6 ms                             150.8 ms        +562 µs
//	80 ms            70.7 ms                             151.7 ms        +744 µs
//
// The release was never early: `grant - release` is positive throughout and the table never
// held more than its budget of 1. What failed was the comparison, because
// `elapsed = holdFor + releaseCost - skew`, so `elapsed < holdFor` fails whenever the
// scheduler puts more than one release-cost — about a millisecond — between the `go`
// statement and the clock read. That is why the observed failure reported 111.8 ms against a
// 150 ms hold, and it is also why RAISING THE HOLD CANNOT FIX IT: a larger holdFor raises
// `elapsed` and the bound it is compared against by the same amount, leaving the margin at
// one release cost. GOV-B10 records what padding a load-sensitive guard costs this project.
//
// So the hold is gone entirely. The release is triggered by the park it is meant to follow,
// which makes the ordering structural rather than timed: the second request is observed
// parked — which is itself the proof that the budget was not over-granted while the first
// lease was held — and only then is the slot freed.
func TestAParkedSpawnProceedsWhenASlotFrees(t *testing.T) {
	shortWait(t, 5*time.Second)
	endpoint, d := startTestDaemon(t, 1)

	if !askGateBuild(t, endpoint, "s1", "toolu_01") {
		t.Fatal("the first build was refused")
	}

	// The releasing goroutine waits for the park rather than for a clock. If the second
	// request is never parked it reports that instead of releasing, because a spawn allowed
	// through without parking is the budget violation this test exists to catch — and a
	// timer-driven release cannot tell the two apart.
	asked := make(chan struct{})
	parked := make(chan bool, 1)
	go func() {
		ok := awaitParkedWaiters(d, 1, asked)
		parked <- ok
		if !ok {
			return
		}
		var stderr strings.Builder
		lifecycleHook(strings.NewReader(postToolUsePayload("s1", "toolu_01")),
			&stderr, transport.Dial{}, hookBudget, buildFinish)
	}()

	start := time.Now()
	allowed := askGateBuild(t, endpoint, "s1", "toolu_02")
	elapsed := time.Since(start)
	close(asked)

	// The three outcomes are told apart deliberately. Only the second is a governor fault,
	// and a timer-driven release reported all three as the same failure.
	switch sawPark := <-parked; {
	case !sawPark && allowed:
		t.Fatalf("the second build was granted without ever parking, on a budget of 1 with the "+
			"first lease still held: the budget was over-granted (in use = %d)", d.table.InUse())
	case !sawPark && !allowed:
		t.Fatalf("the gate gave up after %v without the park ever being observed; the machine "+
			"stalled past the whole %v wait, so this run cannot say anything about the handover",
			elapsed.Round(time.Millisecond), gateWaitMax)
	case !allowed:
		t.Fatalf("the parked spawn was refused after %v, though the slot it waited for was freed",
			elapsed.Round(time.Millisecond))
	}
	// The waiter holds the one slot the releaser gave up. Two would mean the freed slot was
	// handed out while the original holder still had it.
	if got := d.table.InUse(); got != 1 {
		t.Errorf("in use = %d, want 1: the budget of 1 was not respected across the handover", got)
	}
	if elapsed > gateWaitMax {
		t.Errorf("took %v, which is the whole wait: the release did not wake the waiter", elapsed)
	}
}

// TestReleaseWakesTheLongestWaiter isolates the wake-up from the reaper's backstop.
//
// newDaemon on its own runs no reaper — serve() starts that — so nothing here can wake a
// waiter except the release path itself. That isolation is the whole point of the test.
// End to end the reaper's unconditional pump also frees the waiter, two seconds later, so a
// release that woke nobody still *looks* like it works: the spawn proceeds, just far more
// slowly than it should, on every release on the machine. That is exactly the kind of
// regression a wall-clock end-to-end test reports as a pass.
func TestReleaseWakesTheLongestWaiter(t *testing.T) {
	d := newDaemon(1, io.Discard)
	if _, ok := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1); !ok {
		t.Fatal("could not fill the budget")
	}
	if _, ok := d.table.Confirm("s0", "", "agent_a", 0, time.Time{}); !ok {
		t.Fatal("could not confirm the lease")
	}

	p, ok := d.acquire(request{V: protocolVersion, Session: "s1", WaitMS: 2000}).(*parkedAcquire)
	if !ok {
		t.Fatal("the second acquire was not parked")
	}

	done := make(chan any, 1)
	go func() { done <- p.resolve() }()

	d.release(request{V: protocolVersion, Session: "s0", AgentID: "agent_a"})

	select {
	case r := <-done:
		if _, ok := r.(grantReply); !ok {
			t.Fatalf("the waiter was refused rather than given the freed slot: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("the release woke nobody; the waiter will sit out its whole wait beside a free slot")
	}
}

// SessionEnd frees every lease a session held, and those slots belong to the queue too.
func TestSessionEndWakesTheLongestWaiter(t *testing.T) {
	d := newDaemon(1, io.Discard)
	if _, ok := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1); !ok {
		t.Fatal("could not fill the budget")
	}

	p, ok := d.acquire(request{V: protocolVersion, Session: "s1", WaitMS: 2000}).(*parkedAcquire)
	if !ok {
		t.Fatal("the second acquire was not parked")
	}

	done := make(chan any, 1)
	go func() { done <- p.resolve() }()

	d.session(request{V: protocolVersion, Session: "s0", Action: "end"})

	select {
	case r := <-done:
		if _, ok := r.(grantReply); !ok {
			t.Fatalf("the waiter was refused rather than given the freed slot: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("SessionEnd woke nobody; a crashed session's slots do not reach the queue")
	}
}

// A slot reclaimed by the reaper must reach the queue too.
//
// This is the only path by which a freed slot reaches a waiter without anyone calling
// release: a lease whose SubagentStop was lost, or whose session died, is collected by TTL
// or liveness (invariant 2), and the reaper's pump is what hands it on. Without it such a
// slot sits free while somebody waits for it and is then refused — the budget looks correct
// and the machine idles.
func TestASlotReclaimedByTheReaperWakesAWaiter(t *testing.T) {
	table := lease.NewTable(lease.Options{TentativeTTL: 10 * time.Millisecond})
	d := &daemon{
		table:   table,
		sources: []budget.Source{budget.LocalSource{N: 1, InUse: table.InUse}},
		thermal: func() signals.Thermal { return signals.Unknown },
		waits:   &waitQueue{},
		denials: newDenyLog(nil),
		diag:    io.Discard,
	}

	// A tentative lease nobody will ever confirm: the spawn PreToolUse announced never
	// happened, which is the case the tentative TTL exists for.
	if _, ok := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1); !ok {
		t.Fatal("could not fill the budget")
	}

	p, ok := d.acquire(request{V: protocolVersion, Session: "s1", WaitMS: 10000}).(*parkedAcquire)
	if !ok {
		t.Fatal("the second acquire was not parked")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.reap(ctx)

	done := make(chan any, 1)
	go func() { done <- p.resolve() }()

	select {
	case r := <-done:
		if _, ok := r.(grantReply); !ok {
			t.Fatalf("the waiter was refused rather than given the reclaimed slot: %+v", r)
		}
	case <-time.After(2*reapInterval + time.Second):
		t.Fatal("the reaper reclaimed a slot and woke nobody; it went to no one while a caller waited")
	}
}

// At the deadline the wait becomes a deny. The stall is the preferred outcome, not the only
// one, and a queue that never gives up is a session that never proceeds.
func TestAParkedSpawnIsRefusedAtItsDeadline(t *testing.T) {
	shortWait(t, 200*time.Millisecond)
	endpoint, d := startTestDaemon(t, 1)

	if !askGateBuild(t, endpoint, "s1", "toolu_01") {
		t.Fatal("the first spawn was refused")
	}

	start := time.Now()
	allowed := askGateBuild(t, endpoint, "s1", "toolu_02")
	elapsed := time.Since(start)

	if allowed {
		t.Fatal("the second spawn was allowed against a budget of 1")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("refused after %v, without waiting out the %v it offered", elapsed, 200*time.Millisecond)
	}
	if got := d.table.InUse(); got != 1 {
		t.Errorf("in use = %d, want 1: a spawn that waited and was then refused must not hold a slot", got)
	}
	if got := d.waits.len(); got != 0 {
		t.Errorf("%d waiter(s) left in the queue after the wait expired", got)
	}
}

// The next freed slot goes to whoever has waited longest, whatever session they are in
// (design.md section 6.2). The queue is walked from the head and stops at the first waiter
// it cannot satisfy, so a session asking in a tight loop cannot overtake a quiet one.
func TestTheWaitQueueIsStrictlyFIFOAcrossSessions(t *testing.T) {
	d := newDaemon(1, io.Discard)

	held, ok := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1)
	if !ok {
		t.Fatal("could not fill the budget")
	}

	order := []string{"first", "second", "third"}
	waiters := map[string]*waiter{}
	for _, name := range order {
		w := &waiter{session: name, granted: make(chan lease.Lease, 1)}
		waiters[name] = w
		d.waits.enqueue(w)
	}

	d.pump()
	for _, name := range order {
		if len(waiters[name].granted) != 0 {
			t.Fatalf("%q was granted a lease while the budget was full", name)
		}
	}

	// Free one slot at a time and check who gets it.
	last := held.ID
	for i, name := range order {
		d.table.Drop(last)
		d.pump()

		select {
		case l := <-waiters[name].granted:
			last = l.ID
		default:
			t.Fatalf("slot %d went to nobody; %q had waited longest", i+1, name)
		}
		for _, other := range order[i+1:] {
			if len(waiters[other].granted) != 0 {
				t.Fatalf("%q jumped the queue ahead of the waiters before it", other)
			}
		}
	}
	if got := d.waits.len(); got != 0 {
		t.Errorf("%d waiter(s) still queued after all were served", got)
	}
}

// A waiter granted a lease at the same instant it gave up must end up owning that lease,
// not refused beside it. The race is real on a busy machine and both wrong answers cost a
// slot: refusing wastes it for the lease's whole TTL, and dropping the queue entry with the
// lease unread leaks it outright.
func TestALeaseGrantedAsAWaiterGivesUpIsNotLost(t *testing.T) {
	q := &waitQueue{}
	w := &waiter{session: "s1", granted: make(chan lease.Lease, 1)}
	q.enqueue(w)

	w.granted <- lease.Lease{ID: "L-race"}

	l, got := q.drop(w)
	if !got {
		t.Fatal("drop reported no lease, so the granted lease has been leaked")
	}
	if l.ID != "L-race" {
		t.Errorf("lease = %q, want L-race", l.ID)
	}
	if q.len() != 0 {
		t.Error("the waiter was left in the queue")
	}
}

// A client that goes away before it can even be told it is waiting must not leave the slot
// it was about to be given held for the lease's whole TTL.
func TestACancelledParkHandsTheSlotBack(t *testing.T) {
	d := newDaemon(1, io.Discard)

	held, _ := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1)
	reply := d.acquire(request{V: protocolVersion, Op: "acquire", Session: "s1", WaitMS: 5000})
	p, ok := reply.(*parkedAcquire)
	if !ok {
		t.Fatalf("acquire returned %T, want a parked acquire", reply)
	}

	// The slot frees and the pump hands it to the parked waiter, whose client is gone.
	d.table.Drop(held.ID)
	d.pump()
	p.cancel()

	if got := d.table.InUse(); got != 0 {
		t.Errorf("in use = %d, want 0: the slot was not handed back when the client vanished", got)
	}
	if got := d.waits.len(); got != 0 {
		t.Errorf("%d waiter(s) left queued after cancel", got)
	}
}

// TestNestedFanOutIsRefusedOutright is the depth limit (design.md section 6.3).
//
// The payload is a real nested PreToolUse from docs/hook-surface.md section 2 — the only
// depth signal the hook surface carries is the presence of a top-level agent_id, and Claude
// Code enforces nothing itself (measured max_depth 2, refused.depth_limit 0), so this is
// entirely the governor's to refuse.
func TestNestedFanOutIsRefusedOutright(t *testing.T) {
	shortWait(t, 5*time.Second)
	endpoint, d := startTestDaemon(t, 4)

	nested := `{"session_id":"s2","agent_id":"a0dca3e4fa79a2f6f","agent_type":"general-purpose",` +
		`"hook_event_name":"PreToolUse","tool_name":"Agent","tool_use_id":"toolu_nested"}`

	t.Setenv(transport.EndpointEnv, endpoint)
	var stdout, stderr strings.Builder
	var code int
	took := timed(func() {
		code = gate(strings.NewReader(nested), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
	})

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var out denyOutput
	if err := json.Unmarshal([]byte(stdout.String()), &out); err != nil {
		t.Fatalf("nested spawn was not refused; stdout = %q", stdout.String())
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("permissionDecision = %q, want deny", out.HookSpecificOutput.PermissionDecision)
	}
	reason := out.HookSpecificOutput.PermissionDecisionReason
	if !strings.Contains(reason, "Nested") {
		t.Errorf("reason = %q, want it to name nesting as the cause rather than capacity", reason)
	}
	if strings.Contains(reason, "slots") {
		t.Errorf("reason = %q; naming a slot count invites a retry once the count moves, and it never will for this caller", reason)
	}

	// The budget had three free slots. A depth refusal is not a capacity problem and must
	// not be parked in the hope of one appearing.
	if took.exceeds(failOpenCeiling) {
		t.Errorf("took %v; a nested spawn must be refused at once, not waited out", took)
	}
	if got := d.table.InUse(); got != 0 {
		t.Errorf("in use = %d, want 0: a refused nested spawn must not consume a lease", got)
	}
}

// The rate limit. A session that has just been refused is refused again immediately rather
// than parked, so a retry loop cannot take a place in the FIFO on every attempt and hold it
// for the whole of wait_max, crowding out sessions that asked once and are waiting.
func TestASessionRefusedInsideTheCooloffIsNotParkedAgain(t *testing.T) {
	shortWait(t, 500*time.Millisecond)
	endpoint, _ := startTestDaemon(t, 1)

	if !askGateBuild(t, endpoint, "s1", "toolu_01") {
		t.Fatal("the first spawn was refused")
	}
	if askGateBuild(t, endpoint, "s1", "toolu_02") {
		t.Fatal("the second spawn was allowed against a budget of 1")
	}

	var allowed bool
	took := timed(func() { allowed = askGateBuild(t, endpoint, "s1", "toolu_03") })
	if allowed {
		t.Fatal("the third spawn was allowed against a budget of 1")
	}

	if took.exceeds(failOpenCeiling) {
		t.Errorf("the retry waited %v; inside the cooloff it must be refused at once", took)
	}
}

// A session that waited patiently and was granted a slot must not be shouted at on its next
// refusal: only refusals count.
func TestAGrantAfterWaitingIsNotCountedAsADenial(t *testing.T) {
	d := newDaemon(1, io.Discard)

	held, _ := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1)
	reply := d.acquire(request{V: protocolVersion, Session: "s1", WaitMS: 5000})
	p := reply.(*parkedAcquire)

	d.table.Drop(held.ID)
	d.pump()

	if _, ok := p.resolve().(grantReply); !ok {
		t.Fatal("the parked acquire did not resolve to a grant")
	}
	if d.denials.throttled("s1") {
		t.Error("a session that waited and was granted a slot was recorded as denied")
	}
}

// Repeat denials escalate. The first text demonstrably does not always work, and repeating
// the identical sentence to a caller that has ignored it three times is not a strategy.
func TestRepeatDenialsEscalate(t *testing.T) {
	// A budget of 1, filled, rather than a budget of 0: budget.Floor is 1 and invariant 4
	// says it never reaches zero, so asking for 0 gets 1 and every acquire would be granted.
	d := newDaemon(1, io.Discard)
	if _, ok := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1); !ok {
		t.Fatal("could not fill the budget")
	}

	var texts []string
	for i := 0; i < 4; i++ {
		r, ok := d.acquire(request{V: protocolVersion, Session: "s1"}).(refuseReply)
		if !ok {
			t.Fatalf("refusal %d was not a refusal", i+1)
		}
		if r.Denials != i+1 {
			t.Errorf("denials = %d on refusal %d", r.Denials, i+1)
		}
		texts = append(texts, denyReason(acquireResponse{
			Reason: r.Reason, Budget: r.Budget, InUse: r.InUse, Denials: r.Denials,
		}, intent{kind: "agent"}))
	}

	for i, text := range texts {
		if !strings.Contains(text, "Agent") {
			t.Errorf("refusal %d gives no concrete instruction: %q", i+1, text)
		}
	}
	if texts[0] == texts[1] {
		t.Errorf("the second refusal repeats the first verbatim: %q", texts[0])
	}
	if texts[1] == texts[2] {
		t.Errorf("the third refusal repeats the second verbatim: %q", texts[1])
	}
	if !strings.Contains(texts[3], "4 times") {
		t.Errorf("the fourth refusal does not name how many times: %q", texts[3])
	}
}

// TestABuildIsRefusedInItsOwnTerms: a refused spawn and a refused build are not the same event
// and must not be told the same thing.
//
// Refusing a spawn means "do not delegate, do the work here" — the work still happens. Refusing
// a Bash means the user's command DID NOT RUN, and there is no inline alternative: nothing else
// on the machine can compile that package. Telling a model that tried `cargo build` to "run
// this task inline yourself rather than delegating it" sends it looking for another way to
// build, which is worse than waiting.
func TestABuildIsRefusedInItsOwnTerms(t *testing.T) {
	resp := acquireResponse{Reason: "capacity", Budget: 4, InUse: 4, Denials: 1}

	build := denyReason(resp, intent{kind: "build"})
	spawn := denyReason(resp, intent{kind: "agent"})

	if build == spawn {
		t.Fatalf("a refused build is told exactly what a refused spawn is told: %q", build)
	}
	if !strings.Contains(build, "was not run") {
		t.Errorf("the build refusal does not say the command did not run: %q", build)
	}
	if !strings.Contains(build, "same command again") {
		t.Errorf("the build refusal does not say to retry the same command: %q", build)
	}
	for _, wrong := range []string{"Agent", "inline", "delegat"} {
		if strings.Contains(build, wrong) {
			t.Errorf("the build refusal contains %q, which is spawn advice a build cannot act on: %q",
				wrong, build)
		}
	}
	// And the escalation must not tell it to stop retrying at step two, the way the spawn
	// text does: retrying IS the correct response to a full build queue.
	second := denyReason(acquireResponse{Reason: "capacity", Budget: 4, InUse: 4, Denials: 2},
		intent{kind: "build"})
	if second == build {
		t.Errorf("the second build refusal repeats the first verbatim: %q", build)
	}
	if !strings.Contains(second, "retrying") && !strings.Contains(second, "Wait longer") {
		t.Errorf("the second build refusal does not say what to do differently: %q", second)
	}
}

// A denial from long ago is not a retry loop. The window slides, so a session that comes
// back to a busy machine an hour later starts again at the calm text.
func TestTheDenialWindowSlides(t *testing.T) {
	now := time.Now()
	log := newDenyLog(func() time.Time { return now })

	if got := log.record("s1"); got != 1 {
		t.Fatalf("first denial counted as %d", got)
	}
	if got := log.record("s1"); got != 2 {
		t.Fatalf("second denial counted as %d", got)
	}

	now = now.Add(denyWindow + time.Second)
	if got := log.record("s1"); got != 1 {
		t.Errorf("a denial %v after the last counted as %d, want 1", denyWindow+time.Second, got)
	}
	if log.throttled("s2") {
		t.Error("a session that has never been refused is throttled")
	}

	now = now.Add(denyWindow + time.Second)
	log.sweep()
	if len(log.m) != 0 {
		t.Errorf("sweep left %d stale session(s); this map grows with every session ever seen", len(log.m))
	}
}

// The interim line has to reach the client before the wait starts, because it is the
// client's only evidence that waiting is safe. Sent afterwards it would be worthless.
func TestAParkedAcquireIsAnnouncedBeforeItWaits(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 1)

	if r := call(t, endpoint, acquireReq("s0")); r.OK == nil || !*r.OK {
		t.Fatal("the first acquire was refused")
	}

	conn, err := transport.Dial{}.Dial(t.Context(), endpoint)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	req := acquireReq("s1")
	req["wait_ms"] = 3000
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := time.Now()
	var interim reply
	if err := readLineInto(bufio.NewReader(conn), &interim); err != nil {
		t.Fatalf("read interim line: %v", err)
	}
	elapsed := time.Since(start)

	if interim.Reason != "waiting" {
		t.Fatalf("interim reason = %q, want %q", interim.Reason, "waiting")
	}
	if interim.V != protocolVersion {
		t.Errorf("interim v = %d, want %d: every message carries the version", interim.V, protocolVersion)
	}
	if interim.OK == nil || *interim.OK {
		t.Error("the interim line must not read as a grant")
	}
	if interim.WaitMS != 3000 {
		t.Errorf("interim wait_ms = %d, want 3000: the client is told what it is being held for", interim.WaitMS)
	}
	if elapsed > time.Second {
		t.Errorf("the interim line took %v; it must be sent before the wait, not after it", elapsed.Round(time.Millisecond))
	}
}

// status reports the queue, which is what the statusline (GOV-10) and doctor (GOV-20) read.
func TestStatusReportsParkedAcquires(t *testing.T) {
	endpoint, d := startTestDaemon(t, 1)

	if r := call(t, endpoint, acquireReq("s0")); r.OK == nil || !*r.OK {
		t.Fatal("the first acquire was refused")
	}
	if got := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "status"}).Waiting; got != 0 {
		t.Errorf("waiting = %d with nobody parked", got)
	}

	parked, err := transport.Dial{}.Dial(t.Context(), endpoint)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer parked.Close()
	req := acquireReq("s1")
	req["wait_ms"] = 2000
	body, _ := json.Marshal(req)
	if _, err := parked.Write(append(body, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for d.waits.len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "status"}).Waiting; got != 1 {
		t.Errorf("waiting = %d with one acquire parked, want 1", got)
	}
}

// The daemon caps what a client may ask to be held for, whatever it sends.
func TestAnAbsurdWaitIsCapped(t *testing.T) {
	d := newDaemon(1, io.Discard)
	if _, ok := d.table.Acquire(lease.Request{Session: "s0", Depth: 0}, 1); !ok {
		t.Fatal("could not fill the budget")
	}

	reply := d.acquire(request{V: protocolVersion, Session: "s1", WaitMS: 1 << 30})
	p, ok := reply.(*parkedAcquire)
	if !ok {
		t.Fatalf("acquire returned %T, want a parked acquire", reply)
	}
	defer p.cancel()

	if p.wait != maxParkedWait {
		t.Errorf("wait = %v, want it capped at %v", p.wait, maxParkedWait)
	}
	if want := int(maxParkedWait / time.Millisecond); p.interim.WaitMS != want {
		t.Errorf("interim wait_ms = %d, want the capped %d — the client must be told what it will actually get",
			p.interim.WaitMS, want)
	}
}
