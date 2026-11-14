package main

import (
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
)

// GOV-B34. A pin set with `governor limit` is the budget every reader compares against:
// acquire, the wait-queue pump, advise, and the deny line. Before this story the pump and
// advise read budget.Min directly and never saw the pin, so a waiter parked behind the old
// budget was neither admitted when the pin went up nor held back when it came down.

// parkOne fills nothing itself: it asks for one build slot with a long wait and requires the
// daemon to park it. The caller arranges for the budget to be full first.
func parkOne(t *testing.T, d *daemon, session string) *parkedAcquire {
	t.Helper()
	got := d.acquire(request{V: protocolVersion, Session: session, Kind: "build", Slots: 1, WaitMS: 60_000})
	p, ok := got.(*parkedAcquire)
	if !ok {
		t.Fatalf("the acquire was not parked: %+v", got)
	}
	if n := d.waits.len(); n != 1 {
		t.Fatalf("%d waiter(s) in the queue, want 1", n)
	}
	return p
}

func holdBuild(t *testing.T, d *daemon, session string) {
	t.Helper()
	if _, ok := d.acquire(request{V: protocolVersion, Session: session, Kind: "build", Slots: 1}).(grantReply); !ok {
		t.Fatal("filling the budget was refused")
	}
}

// Pinning UP admits a parked waiter at once. The source says 1 and the one slot is held; the
// pin raises the budget to 3, and the pump the control handler runs must hand the waiter a
// slot against the 3 — not leave it to time out against the source's 1.
func TestPinningUpAdmitsAParkedWaiter(t *testing.T) {
	d := controlDaemon(t, 1)
	d.table.StartSession(lease.Session{ID: "s1", Weight: 1})
	holdBuild(t, d, "s1")
	p := parkOne(t, d, "s1")
	defer p.cancel()

	ctl(d, "limit", 3, time.Hour)

	if n := d.waits.len(); n != 0 {
		t.Fatalf("%d waiter(s) still parked after the pin raised the budget to 3 with 1 slot "+
			"held: the pump compared against the unpinned budget, not the pin", n)
	}
	if r, ok := p.resolve().(grantReply); !ok {
		t.Fatalf("the waiter was not granted: %+v", r)
	} else if r.Budget != 3 {
		t.Errorf("granted against budget %d, want the pinned 3", r.Budget)
	}
}

// Pinning DOWN holds a waiter back that the source's budget would have admitted. The source
// says 8, the pin says 1 and the one slot is held, so the acquire parks. A pump must not then
// admit it against the source's 8.
func TestPinningDownHoldsAParkedWaiterBack(t *testing.T) {
	d := controlDaemon(t, 8)
	d.table.StartSession(lease.Session{ID: "s1", Weight: 1})
	ctl(d, "limit", 1, time.Hour)
	holdBuild(t, d, "s1")
	p := parkOne(t, d, "s1")
	defer p.cancel()

	d.pump()

	if n := d.waits.len(); n != 1 {
		t.Fatalf("the pump admitted a waiter against the source's 8 while a pin of 1 was in force "+
			"and its one slot was held (%d left parked, want 1)", n)
	}
	if got := d.table.InUse(); got != 1 {
		t.Errorf("in use = %d, want the 1 the pin allows", got)
	}
}

// advise answers what acquire would do, pinned above and below the source's budget. Each
// case is asked of advise first (which changes nothing) and then of acquire with no wait, so
// both are answering about the same table.
func TestAdviseAgreesWithAcquireUnderAPin(t *testing.T) {
	cases := []struct {
		name        string
		source, pin int
		held        int
		wantGranted bool
	}{
		{"pinned above the source, room under the pin", 1, 3, 1, true},
		{"pinned above the source, pin full", 1, 3, 3, false},
		{"pinned below the source, pin full", 8, 2, 2, false},
		{"pinned below the source, room under the pin", 8, 2, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := controlDaemon(t, c.source)
			d.table.StartSession(lease.Session{ID: "s1", Weight: 1})
			ctl(d, "limit", c.pin, time.Hour)
			for i := 0; i < c.held; i++ {
				holdBuild(t, d, "s1")
			}
			req := request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1}

			adv := d.advise(req)
			if adv.Budget != c.pin {
				t.Errorf("advise reported budget %d, want the pinned %d", adv.Budget, c.pin)
			}
			_, acquired := d.acquire(req).(grantReply)
			if adv.Granted != acquired {
				t.Errorf("advise said granted=%t but acquire granted=%t for the same request",
					adv.Granted, acquired)
			}
			if adv.Granted != c.wantGranted {
				t.Errorf("advise said granted=%t, want %t", adv.Granted, c.wantGranted)
			}
		})
	}
}

// PAUSE, DECIDED (GOV-B34 clause 3): a waiter parked before a pause is admitted by the pump
// the pause handler runs. Pause means "stop capping" (design.md section 9.1) and acquire
// already takes a lease through Table.Take while one is in force; a session that happened to
// ask a moment earlier and is still parked is asking the same question, and making it wait out
// a cap that no longer exists would answer it differently from the caller who asked a moment
// later. FIFO order is kept: the walk hands out leases from the head.
func TestPausingAdmitsEveryParkedWaiter(t *testing.T) {
	d := controlDaemon(t, 1)
	d.table.StartSession(lease.Session{ID: "s1", Weight: 1})
	holdBuild(t, d, "s1")
	p := parkOne(t, d, "s1")
	defer p.cancel()

	ctl(d, "pause", 0, time.Hour)

	if n := d.waits.len(); n != 0 {
		t.Fatalf("%d waiter(s) still parked after the cap was paused", n)
	}
	if r, ok := p.resolve().(grantReply); !ok {
		t.Fatalf("the waiter was not granted under a pause: %+v", r)
	}
	if got := d.table.InUse(); got != 2 {
		t.Errorf("in use = %d, want 2: a paused grant is still recorded", got)
	}
}

// And advise agrees with acquire under a pause, too: acquire grants, so advise says so.
func TestAdviseAgreesWithAcquireUnderAPause(t *testing.T) {
	d := controlDaemon(t, 1)
	d.table.StartSession(lease.Session{ID: "s1", Weight: 1})
	holdBuild(t, d, "s1")
	ctl(d, "pause", 0, time.Hour)
	req := request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1}

	before := d.table.InUse()
	adv := d.advise(req)
	if d.table.InUse() != before {
		t.Fatal("advise took a lease")
	}
	_, acquired := d.acquire(req).(grantReply)
	if !acquired {
		t.Fatal("acquire refused under a pause")
	}
	if !adv.Granted {
		t.Error("advise said no under a pause, where acquire grants")
	}
}
