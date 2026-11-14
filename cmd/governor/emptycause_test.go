package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/lease"
)

// GOV-B31: a Heavy build denied with room to spare and an empty cause.
//
// The daemon.log lines from 2026-11-08, found by dogfooding (GOV-38, ledger #7):
//
//	20:01:47 budget park session=239686b0 slots=3.75/1 wait=20s
//	20:02:02 budget park-granted session=239686b0 waited=14.699s
//	20:02:10 budget park session=6d8ea009 slots=0.08/4 wait=20s
//	20:02:30 budget park-expired session=6d8ea009 waited=20.001s
//	20:02:30 budget deny session=6d8ea009 reason=capacity cause= slots=0.08/4 denials=1
//
// and the retry at 20:02:35 was granted.

// incidentClock is the lease table's clock, moved by hand. The park timer is real time and is
// kept short; everything the admission rule reads — the demand window above all — is this.
type incidentClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *incidentClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *incidentClock) at(hms string) {
	t, err := time.Parse("2006-01-02 15:04:05.000", "2026-11-08 "+hms)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// liveBudget stands in for the controller: a budget that moves while a request is parked.
type liveBudget struct {
	mu sync.Mutex
	n  int
}

func (b *liveBudget) get() int  { b.mu.Lock(); defer b.mu.Unlock(); return b.n }
func (b *liveBudget) set(n int) { b.mu.Lock(); b.n = n; b.mu.Unlock() }
func (b *liveBudget) source(d *daemon) budget.Source {
	return budget.LocalSource{Live: b.get, InUse: d.table.InUse}
}

// incidentDaemon is a daemon on the incident's clock and budget, logging to buf. Nothing runs
// the reaper, so the only pumps are the ones the test makes — which is how the 2 s reaper
// tick is replayed at the moments it actually fired.
func incidentDaemon(t *testing.T, buf *bytes.Buffer) (*daemon, *incidentClock, *liveBudget) {
	t.Helper()
	clk := &incidentClock{}
	clk.at("20:01:14.000")
	live := &liveBudget{n: 1}
	d := newDaemonWithLeases(1, buf, lease.Options{Now: clk.Now})
	d.sources = []budget.Source{live.source(d)}
	return d, clk, live
}

const (
	sessA = "6d8ea009" // holds a Heavy, releases it, asks for another
	sessB = "239686b0" // spawns an agent while A's Heavy holds the machine
)

// replayToTheSecondPark runs the incident up to the moment A's second Heavy parks, and
// returns that parked acquire.
func replayToTheSecondPark(t *testing.T, d *daemon, clk *incidentClock, live *liveBudget) *parkedAcquire {
	t.Helper()

	// 20:01:14 — A's first Heavy. The budget is at its floor of 1; the table is empty, so it
	// is admitted alone (work-conserving, internal/lease.fitsLocked).
	if _, ok := d.acquire(request{V: protocolVersion, Session: sessA, Op: "acquire", Kind: "build", Class: "heavy", Slots: 3.75, ToolUseID: "a-1"}).(grantReply); !ok {
		t.Fatal("A's first Heavy was not granted on an empty table")
	}

	// 20:01:47 — B spawns an agent and parks: 3.75 held against a budget of 1.
	clk.at("20:01:47.000")
	pB, ok := d.acquire(request{V: protocolVersion, Session: sessB, Op: "acquire", Kind: "agent", Slots: 0.08, WaitMS: 20000}).(*parkedAcquire)
	if !ok {
		t.Fatal("B's spawn was not parked behind A's Heavy at budget 1")
	}
	// The reaper pumps every 2 s while B waits, and every one of those is a refusal that
	// re-marks B as demanding. The last one before the budget rose is the one that matters.
	for _, at := range []string{"20:01:49.000", "20:01:51.000", "20:01:53.000", "20:01:55.000", "20:01:57.000", "20:01:59.000", "20:02:01.000"} {
		clk.at(at)
		d.pump()
	}

	// 20:02:02 — the budget rises to 4 and the next pump hands B its slot.
	clk.at("20:02:01.699")
	live.set(4)
	d.pump()
	if _, ok := pB.resolve().(grantReply); !ok {
		t.Fatal("B's spawn was not granted once the budget rose to 4")
	}

	// A's first Heavy finishes.
	clk.at("20:02:05.000")
	if r, _ := d.finish(request{V: protocolVersion, Session: sessA, ToolUseID: "a-1"}); !r.OK {
		t.Fatal("A's first Heavy did not release")
	}

	// 20:02:10 — A asks for another Heavy. Only B's 0.08 is held: 0.08 + 3.75 = 3.83 <= 4.
	clk.at("20:02:10.000")
	pA, ok := d.acquire(request{V: protocolVersion, Session: sessA, Op: "acquire", Kind: "build", Class: "heavy", Slots: 3.75, ToolUseID: "a-2", WaitMS: 50}).(*parkedAcquire)
	if !ok {
		t.Fatal("A's second Heavy was not parked; the incident did not reproduce")
	}
	// The reaper's pumps through the park, every one of them before B's demand mark lapses.
	for _, at := range []string{"20:02:12.000", "20:02:14.000", "20:02:16.000", "20:02:18.000", "20:02:20.000", "20:02:22.000", "20:02:24.000", "20:02:26.000", "20:02:28.000"} {
		clk.at(at)
		d.pump()
	}
	return pA
}

// TestGOVB31TheIncidentReplays is clause 1, and it answers clause 2's question with the
// arithmetic rather than a guess.
//
// WHAT REFUSED THE HEAVY WAS FAIR SHARE, NOT CAPACITY. B was refused by every pump while it
// waited, and a refusal marks a session as demanding for demandWindow (30 s), deliberately
// NOT cleared when the session is later granted. So from 20:02:10 to 20:02:31 the table sees
// two unbounded contenders on a budget of four: A's share is 2, and a 3.75 Heavy is over it.
// That refusal was correct by the rule as written — but nothing said so. The cause was read
// AGAIN at the moment the park expired, by which time B's mark had lapsed, the request fitted,
// and Refusal answered "" — "this would be admitted" — on the very line that refused it.
func TestGOVB31TheIncidentReplays(t *testing.T) {
	var buf bytes.Buffer
	d, clk, live := incidentDaemon(t, &buf)
	pA := replayToTheSecondPark(t, d, clk, live)

	// While B's mark is live the refusal is by share, and the machine is not full.
	req := leaseRequestFor(request{Session: sessA, Kind: "build", Slots: 3.75})
	if got := d.table.Refusal(req, 4); got != "share" {
		t.Fatalf("at 20:02:28 the Heavy is refused for %q; the incident's mechanism is fair share", got)
	}

	// 20:02:31.001 — the park expires, a hair after B's last refusal (20:02:01) left the
	// window. The incident's clock is a second later than the log's throughout, because the
	// replay's last pump on B lands on a whole second.
	clk.at("20:02:31.001")
	// THIS IS WHAT THE CODE BEFORE GOV-B31 LOGGED AS "cause=". The request now fits, so
	// Refusal answers "", and the old expiry path refused it anyway and printed that answer.
	if got := d.table.Refusal(req, 4); got != "" {
		t.Fatalf("at expiry Refusal = %q; the replay no longer reaches the incident's state", got)
	}
	reply := pA.resolve()

	log := buf.String()
	if strings.Contains(log, "cause= ") {
		t.Errorf("a deny was logged with an empty cause:\n%s", log)
	}
	switch r := reply.(type) {
	case grantReply:
		// The request fits at the moment the park ends. Granting it is the fix: a pump at
		// expiry hands it the room the timer would otherwise have refused it beside.
	case refuseReply:
		if r.Cause == "" {
			t.Fatalf("the Heavy was refused with an empty cause at %.2f/%d slots", r.Slots, r.Budget)
		}
	default:
		t.Fatalf("park resolved to %T", reply)
	}
	if _, ok := reply.(grantReply); !ok {
		t.Fatalf("at 20:02:31 the Heavy fits (0.08 + 3.75 <= 4, B's demand mark has lapsed) and was refused anyway: %+v\n%s", reply, log)
	}
}

// TestNoCapacityDenyHasAnEmptyCause is clause 3: every way the daemon can refuse for capacity
// names a cause, the deny line logs it, and the text the session reads is built from the same
// cause the line logged.
//
// One case per call site of refuseForCapacity and per thing Refusal can answer there. The two
// that used to print "" are the queued ones: a request that fits, kept back by the FIFO.
func TestNoCapacityDenyHasAnEmptyCause(t *testing.T) {
	agent := func(session string, waitMS int) request {
		return request{V: protocolVersion, Session: session, Op: "acquire", Kind: "agent", Slots: 0.08, WaitMS: waitMS}
	}
	build := func(session, id string, slots float64, waitMS int) request {
		return request{V: protocolVersion, Session: session, Op: "acquire", Kind: "build", Slots: slots, ToolUseID: id, WaitMS: waitMS}
	}
	// fill takes the whole of a one-slot budget. A smaller holder would not do: a session
	// holding nothing is admitted beside anything less than the budget (invariant 6, GOV-34).
	fill := func(d *daemon) {
		if _, ok := d.acquire(request{V: protocolVersion, Session: "holder", Op: "acquire"}).(grantReply); !ok {
			t.Fatal("could not fill the machine")
		}
	}
	// queueBehindAHead leaves a waiter at the head of the queue that cannot be satisfied, on a
	// machine with room for anything small: the shape TestAFreshAcquireDoesNotJumpTheQueue
	// builds, for the same reason.
	queueBehindAHead := func(d *daemon) {
		d.table.Acquire(lease.Request{Session: "holder", Kind: lease.Build, Weight: 3.5, ToolUseID: "holder-1"}, 4)
		d.table.Acquire(lease.Request{Session: "big", Kind: lease.Agent, Weight: 0.08}, 4)
		big := &waiter{session: "big", req: lease.Request{Session: "big", Kind: lease.Build, Weight: 3.75, ToolUseID: "big-1"}, granted: make(chan lease.Lease, 1)}
		d.waits.enqueue(big)
		d.pump()
		if len(big.granted) != 0 {
			t.Fatal("the head waiter fitted; there is no queue to be kept back by")
		}
	}

	cases := []struct {
		name  string
		limit int
		setup func(d *daemon)
		ask   request
		want  string
	}{{
		name: "full machine, caller cannot wait", limit: 1,
		setup: func(d *daemon) { fill(d) },
		ask:   agent("asker", 0), want: "capacity",
	}, {
		name: "full machine, park expires at the head", limit: 1,
		setup: func(d *daemon) { fill(d) },
		ask:   agent("asker", 50), want: "capacity",
	}, {
		name: "refused moments ago, so not parked again", limit: 1,
		setup: func(d *daemon) {
			fill(d)
			d.acquire(agent("asker", 0))
		},
		ask: agent("asker", 50), want: "capacity",
	}, {
		name: "room on the machine belongs to another session", limit: 4,
		setup: func(d *daemon) {
			for _, id := range []string{"h0", "h1", "h2", "h3"} {
				d.table.Acquire(lease.Request{Session: "holder", Kind: lease.Build, Weight: 1, ToolUseID: id}, 4)
			}
			d.acquire(build("rival", "r0", 1, 0))
			for _, l := range d.table.Leases() {
				d.table.Drop(l.ID)
			}
			d.acquire(build("asker", "a0", 1, 0))
			d.acquire(build("asker", "a1", 1, 0))
		},
		ask: build("asker", "a2", 1, 0), want: "share",
	}, {
		name: "fits, but the queue is not empty and the caller cannot wait", limit: 4,
		setup: queueBehindAHead,
		ask:   agent("asker", 0), want: causeQueued,
	}, {
		name: "fits, park expires behind a head that does not", limit: 4,
		setup: queueBehindAHead,
		ask:   agent("asker", 50), want: causeQueued,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			d := newDaemon(c.limit, &buf)
			c.setup(d)
			reply := d.acquire(c.ask)
			if p, parked := reply.(*parkedAcquire); parked {
				reply = p.resolve()
			}
			r, ok := reply.(refuseReply)
			if !ok {
				t.Fatalf("not refused: %T %+v", reply, reply)
			}
			if r.Cause == "" {
				t.Fatalf("refused with an empty cause at %.2f/%d", r.Slots, r.Budget)
			}
			if r.Cause != c.want {
				t.Errorf("cause = %q, want %q", r.Cause, c.want)
			}

			lines := logLines(&buf)
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, "deny ") || !strings.Contains(last, " cause="+r.Cause+" ") {
				t.Errorf("the deny line does not log the cause the session was sent (%q): %q", r.Cause, last)
			}

			// What the session reads is the gate's decode of this reply, not the struct.
			wire, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var resp acquireResponse
			if err := json.Unmarshal(wire, &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Cause != r.Cause {
				t.Fatalf("the gate decoded cause %q from a reply carrying %q", resp.Cause, r.Cause)
			}
			text := denyReason(resp, intent{kind: "build"})
			if r.Cause == causeQueued {
				if !strings.Contains(text, "queued") {
					t.Errorf("a queued refusal does not say so: %q", text)
				}
				for _, false_ := range []string{"Machine at capacity", "every slot", "not enough for it"} {
					if strings.Contains(text, false_) {
						t.Errorf("a refusal of a request that fits claims %q: %q", false_, text)
					}
				}
			}
		})
	}
}
