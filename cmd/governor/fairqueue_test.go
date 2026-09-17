package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
)

// GOV-18: the wait queue under fair share, and what a long build costs the sessions behind it
// (design.md sections 6.1 and 6.2).

// TestAFreshAcquireDoesNotJumpTheQueue closes the other door into the starvation that the
// strict FIFO exists to prevent.
//
// The pump stops at the first waiter it cannot satisfy rather than skipping to one it can, so
// a queue that is non-empty is always one whose HEAD does not fit. Before this story a fresh
// acquire went straight to the lease table without ever looking at the queue, so a caller
// asking for less than the head — an agent at 0.08 slots behind a Heavy build at 3.75 — was
// granted immediately and the head waited again. A session spawning in a tight loop would
// overtake a session that had been waiting, which is exactly what the pump's strict walk is
// written to prevent.
func TestAFreshAcquireDoesNotJumpTheQueue(t *testing.T) {
	d := newDaemon(4, io.Discard)

	// Fill most of the budget, leaving less room than the head waiter needs.
	held, ok := d.table.Acquire(lease.Request{Session: "holder", Kind: lease.Build, Weight: 3.5, ToolUseID: "holder-1"}, 4)
	if !ok {
		t.Fatal("could not seed the table")
	}

	// A big waiter queues and cannot be satisfied: 3.5 held, 3.75 wanted, budget 4.
	big := &waiter{session: "big", req: lease.Request{Session: "big", Kind: lease.Build, Weight: 3.75, ToolUseID: "big-1"}}
	big.granted = make(chan lease.Lease, 1)
	d.waits.enqueue(big)
	d.pump()
	if len(big.granted) != 0 {
		t.Fatal("the big waiter was granted a lease it does not fit in; the test proves nothing")
	}

	// A small, cheap acquire arrives. There IS room for it — that is the whole danger — and
	// it must not be taken while somebody has been waiting longer.
	reply := d.acquire(request{
		Session: "sneaky", Op: "acquire", Kind: "build", Slots: 0.08, ToolUseID: "sneaky-1",
	})
	if _, granted := reply.(grantReply); granted {
		t.Fatal("a fresh acquire was granted a slot ahead of a waiter that had been queued longer: the FIFO is bypassable")
	}

	// And the queue still works: when the holder finishes, the head waiter is served.
	d.table.Drop(held.ID)
	d.pump()
	if len(big.granted) != 1 {
		t.Fatal("the head waiter was not served once its slot freed")
	}
}

// TestAShareRefusalDoesNotClaimTheMachineIsFull keeps the refusal text honest once fair share
// can refuse a session on a machine that has room.
//
// design.md section 4.2 is explicit that a reason clause stating a fact the daemon did not
// send is a fabrication. "Machine at capacity (1/4 build slots)" is worse than a fabrication:
// it contradicts itself in the same sentence, and it tells a model to stop delegating for a
// reason that is not true.
func TestAShareRefusalDoesNotClaimTheMachineIsFull(t *testing.T) {
	d := newDaemon(4, io.Discard)

	// Two sessions contending on a budget of four. The rival reveals its demand by being
	// refused while the holder has the machine.
	for i := 0; i < 4; i++ {
		d.table.Acquire(lease.Request{Session: "holder", Kind: lease.Build, Weight: 1, ToolUseID: fmt.Sprintf("h%d", i)}, 4)
	}
	d.acquire(request{Session: "rival", Op: "acquire", Kind: "build", Slots: 1, ToolUseID: "r0"})
	// The holder's builds finish, leaving the machine mostly empty.
	for _, l := range d.table.Leases() {
		d.table.Drop(l.ID)
	}

	// The holder reaches for everything again. It gets its half and is then refused — by
	// share, on a machine that is half free.
	var refusal refuseReply
	for i := 0; i < 4; i++ {
		got := d.acquire(request{Session: "holder", Op: "acquire", Kind: "build", Slots: 1, ToolUseID: fmt.Sprintf("h2-%d", i)})
		if r, ok := got.(refuseReply); ok {
			refusal = r
			break
		}
	}
	if refusal.Reason != "capacity" {
		t.Fatalf("the holder was never refused; reason=%q", refusal.Reason)
	}
	if refusal.Cause != "share" {
		t.Fatal("a refusal issued while the machine had free slots did not report itself as a share refusal")
	}
	if refusal.Slots >= float64(refusal.Budget) {
		t.Fatalf("the machine holds %.1f of %d slots, so this test is not exercising a share refusal", refusal.Slots, refusal.Budget)
	}

	text := denyReason(acquireResponse{
		Reason: refusal.Reason, Budget: refusal.Budget, Slots: refusal.Slots,
		Denials: refusal.Denials, Cause: refusal.Cause,
	}, intent{kind: "build"})
	if strings.Contains(text, "Machine at capacity") {
		t.Fatalf("a share refusal on a machine with %.1f/%d slots held says %q",
			refusal.Slots, refusal.Budget, text)
	}
	if !strings.Contains(text, "fair share") {
		t.Fatalf("a share refusal does not say why it was refused: %q", text)
	}
	// The instruction half must not assert it either. "Using every slot" is the same false
	// claim one sentence later, and it is the half the model actually acts on.
	if strings.Contains(text, "every slot") {
		t.Fatalf("the instruction claims every slot is taken on a machine holding %.1f/%d: %q",
			refusal.Slots, refusal.Budget, text)
	}

	// The capacity case must keep saying the thing that IS true of it, or this has traded one
	// wrong sentence for another.
	full := denyReason(acquireResponse{
		Reason: "capacity", Budget: 6, Slots: 6, Denials: 1, Cause: "capacity",
	}, intent{kind: "build"})
	if !strings.Contains(full, "Machine at capacity") || !strings.Contains(full, "every slot") {
		t.Fatalf("a genuine capacity refusal no longer names the machine being full: %q", full)
	}
}

// TestAGrantStillDecodesBesideTheRefusalCause guards a silent failure this story very nearly
// shipped.
//
// The gate decodes GRANTS and REFUSALS into one acquireResponse. A grant already sends
// `"share": 2` as an integer, so the obvious name for GOV-18's new refusal field — `share`,
// as a bool — makes every grant fail to unmarshal, and a gate that cannot decode a reply
// fails open. The result is a governor that grants nothing, governs nothing, and says
// nothing, which is the exact silent failure invariant 1 is written to avoid. The field is
// called `cause` instead, and this keeps it that way.
//
// gate.go's acquireRequest carries the same warning about `weight` versus `slots`; this is
// the second time the one-struct-per-connection decode has nearly cost a silent regression.
func TestAGrantStillDecodesBesideTheRefusalCause(t *testing.T) {
	grant, err := json.Marshal(grantReply{V: 1, OK: true, Lease: "L-1", Budget: 6, InUse: 4, Share: 2})
	if err != nil {
		t.Fatal(err)
	}
	var resp acquireResponse
	if err := json.Unmarshal(grant, &resp); err != nil {
		t.Fatalf("a grant carrying its integer `share` no longer decodes into acquireResponse: %v\n"+
			"the refusal field must not reuse a key another reply sends with a different type", err)
	}
	if !resp.OK || resp.Lease != "L-1" {
		t.Fatalf("grant decoded to ok=%v lease=%q", resp.OK, resp.Lease)
	}

	refusal, err := json.Marshal(refuseReply{V: 1, Reason: "capacity", Budget: 6, Slots: 3, Cause: "share"})
	if err != nil {
		t.Fatal(err)
	}
	resp = acquireResponse{}
	if err := json.Unmarshal(refusal, &resp); err != nil {
		t.Fatalf("a refusal no longer decodes into acquireResponse: %v", err)
	}
	if resp.Cause != "share" {
		t.Fatalf("cause = %q, want share", resp.Cause)
	}
}

// TestLongBuildIsWhatAStarvedSessionWaitsFor is GOV-18 clause 4, and it asserts the behaviour
// the story CHOSE rather than the one it rejected: the wait queue does NOT account for
// expected duration, and this is the cost.
//
// WHY NOT. The cost classes of GOV-14 predict cost, not time, and measuring them against the
// corpus (internal/classify/duration_test.go, 22,820 calls) says they predict time only
// weakly: Heavy's p90 hold is 4.1x Medium's, but the spread WITHIN each class is 53.6x for
// Medium and 33.8x for Heavy, and 18.5% of Medium commands outlast the median Heavy. Ordering
// the queue on a 4x signal buried in 30-50x noise would trade away clause 2's strict FIFO —
// the one thing that provably prevents starvation — for a guess that is wrong on nearly a
// fifth of the simplest comparison. So the queue stays strictly FIFO and this test pins that
// down: a Heavy build queued first is served first, however long it is expected to hold.
//
// WHAT IT COSTS A STARVED SESSION, measured rather than asserted: 16.8% of gated commands run
// longer than a full 30 s parked wait (37.6% of Heavy ones). A session queued behind one of
// those is refused for capacity rather than served, and retries under the deny cooloff. The
// exposure is bounded by maxParkedWait and by nothing else — which is acceptable only because
// fair share (section 6.1) caps what the long-building session can hold in the first place,
// so a long build occupies its own session's share rather than the machine.
func TestLongBuildIsWhatAStarvedSessionWaitsFor(t *testing.T) {
	d := newDaemon(1, io.Discard)

	// One long-running build takes the only slot. Nothing in the table records that it is
	// long; that is the point.
	long, ok := d.table.Acquire(lease.Request{Session: "slow", Kind: lease.Build, Weight: 1, ToolUseID: "slow-1"}, 1)
	if !ok {
		t.Fatal("could not seed the long build")
	}

	// A queue of short builds forms behind it, the first of which is itself Heavy — the one
	// a duration-aware queue would have demoted.
	order := []string{"heavy-first", "short1", "short2", "short3"}
	waiters := map[string]*waiter{}
	for _, name := range order {
		w := &waiter{
			session: name,
			req:     lease.Request{Session: name, Kind: lease.Build, Weight: 1, ToolUseID: name},
			granted: make(chan lease.Lease, 1),
		}
		waiters[name] = w
		d.waits.enqueue(w)
	}

	d.pump()
	for _, name := range order {
		if len(waiters[name].granted) != 0 {
			t.Fatalf("%q was served while the long build held the only slot", name)
		}
	}

	// The long build finishes. Service is strictly in arrival order — the Heavy build that
	// arrived first goes first, and no short build overtakes it.
	last := long.ID
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
				t.Fatalf("%q was served ahead of %q: the queue is reordering by something, "+
					"and clause 4 says it orders by arrival alone", other, name)
			}
		}
	}
}

// TestAWaiterBehindALongBuildIsRefusedRatherThanHeldForever is the bound that makes clause 4's
// answer survivable: the cost of not modelling duration is paid once, as a refusal at
// maxParkedWait, not as an unbounded stall.
//
// This is the difference between a governor that degrades and one that hangs. 16.8% of gated
// commands outlast a 30 s wait, so this path is taken often enough to matter.
func TestAWaiterBehindALongBuildIsRefusedRatherThanHeldForever(t *testing.T) {
	d := newDaemon(1, io.Discard)
	if _, ok := d.table.Acquire(lease.Request{Session: "slow", Kind: lease.Build, Weight: 1, ToolUseID: "slow-1"}, 1); !ok {
		t.Fatal("could not seed the long build")
	}

	start := time.Now()
	reply := d.acquire(request{Session: "waiter", Op: "acquire", Kind: "build", Slots: 1, ToolUseID: "w-1", WaitMS: 120})
	parked, ok := reply.(*parkedAcquire)
	if !ok {
		t.Fatalf("a caller asking to wait behind a long build got %T, want a parked acquire", reply)
	}
	final := parked.resolve()
	if _, refused := final.(refuseReply); !refused {
		t.Fatalf("the waiter resolved to %T; behind a build that never finishes it must be refused, not held", final)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the waiter was held for %v; the exposure to a long build must be bounded by the parked wait", elapsed)
	}
}
