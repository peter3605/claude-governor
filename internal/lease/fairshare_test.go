package lease

import (
	"fmt"
	"testing"
	"time"
)

// GOV-18: max-min fair share over build slots (design.md section 6.1).
//
// The unit under test is the ADMISSION rule, not the wait queue — the queue decides the order
// in which waiting sessions are served and lives in cmd/governor, while this decides who is
// allowed to hold what in the first place. The two are tested apart because they fail apart:
// a correct queue over an unfair admission rule still starves, and vice versa.
//
// EVERY TEST HERE ASKS ASYMMETRICALLY, AND THAT IS NOT DECORATION. The first draft of this
// file had each session take turns, which is the one access pattern under which a budget with
// no fairness rule at all produces a perfectly even split: turn-taking does the sharing and
// the rule is never consulted. Those tests passed with fair share entirely removed from the
// table. A test for fairness has to contain a session that does NOT take turns.
//
// WHAT FAIR SHARE CAN AND CANNOT DO. Admission is not preemption. A session that asks while
// the machine is idle is entitled to the whole of it — that is the redistribution sentence of
// section 6.1, not a loophole — and nothing takes those slots back when a second session
// appears. What fair share guarantees is that a session cannot GROW past its share while
// another is contending, so the incumbent drains and the newcomer is served from the freed
// slots. Starvation is therefore bounded by the length of the builds already running, which
// is what GOV-18 clause 4 had to put a number on.

// buildFor is one ordinary build's request: a Medium build weighs exactly one slot.
func buildFor(session, tag string) Request {
	return Request{Session: session, Kind: Build, Weight: 1, ToolUseID: session + "-" + tag}
}

// fixture drives a table and remembers what each session is holding, so a test can free one
// session's builds without book-keeping in line.
type fixture struct {
	t     *testing.T
	tbl   *Table
	limit int
	held  map[string][]string
}

func newFixture(t *testing.T, limit int, sessions map[string]int) *fixture {
	t.Helper()
	f := &fixture{t: t, tbl: NewTable(Options{}), limit: limit, held: map[string][]string{}}
	for id, w := range sessions {
		f.tbl.StartSession(Session{ID: id, Weight: w})
	}
	return f
}

// ask makes one request and reports whether it was admitted.
func (f *fixture) ask(id, tag string) bool {
	f.t.Helper()
	l, ok := f.tbl.Acquire(buildFor(id, tag), f.limit)
	if ok {
		f.held[id] = append(f.held[id], l.ID)
	}
	return ok
}

// askN makes n requests back to back — a session that does not take turns.
func (f *fixture) askN(id, tag string, n int) int {
	f.t.Helper()
	got := 0
	for i := 0; i < n; i++ {
		if f.ask(id, fmt.Sprintf("%s%d", tag, i)) {
			got++
		}
	}
	return got
}

// finish frees everything a session holds, as its builds completing would.
func (f *fixture) finish(id string) {
	f.t.Helper()
	for _, l := range f.held[id] {
		f.tbl.Drop(l)
	}
	f.held[id] = nil
}

func (f *fixture) count(id string) int { return len(f.held[id]) }

// TestFairShareDoesNotStarveSmallSessions is GOV-18 clause 3: three sessions, one asking for
// ten build slots and two asking for one each, and the two small ones are never starved.
//
// The greedy session does not take turns — it asks for all ten in a row, twice, which is what
// a session fanning out a test suite actually looks like. The two quiet sessions ask once
// each. Without fair share the greedy session takes every slot both times and the quiet ones
// get nothing at all.
func TestFairShareDoesNotStarveSmallSessions(t *testing.T) {
	const limit = 6
	f := newFixture(t, limit, map[string]int{"greedy": 1, "small1": 1, "small2": 1})

	// The greedy session opens on an idle machine and is entitled to all of it.
	if got := f.askN("greedy", "a", 10); got != limit {
		t.Fatalf("greedy took %d of an idle machine's %d slots, want all of them", got, limit)
	}
	// The quiet sessions now ask. There is genuinely no room, so they are refused — and that
	// refusal is what makes their demand visible to everyone else.
	for _, id := range []string{"small1", "small2"} {
		if f.ask(id, "a") {
			t.Fatalf("%s was admitted to a machine with no free slots", id)
		}
	}

	// The greedy session's builds finish, and it immediately asks for ten more while the two
	// quiet sessions ask for one each.
	f.finish("greedy")
	f.askN("greedy", "b", 10)
	for _, id := range []string{"small1", "small2"} {
		if !f.ask(id, "b") {
			t.Fatalf("%s was starved: it could not get a single slot after the machine drained, "+
				"because the greedy session recaptured everything it freed", id)
		}
	}

	if f.count("small1") != 1 || f.count("small2") != 1 {
		t.Fatalf("quiet sessions hold %d and %d slots, want 1 each", f.count("small1"), f.count("small2"))
	}
	// Three contending sessions of equal weight on a budget of six: two slots each.
	if got := f.count("greedy"); got != 2 {
		t.Fatalf("greedy holds %d of %d slots against two contending sessions, want its fair 2", got, limit)
	}
	t.Logf("greedy=%d small1=%d small2=%d of %d slots", f.count("greedy"), f.count("small1"), f.count("small2"), limit)
}

// TestEverySessionGetsOneSlotBeforeAnyGetsASecond is the first sentence of section 6.1: among
// sessions contending, nobody reaches a second slot while somebody else is still asking for a
// first.
func TestEverySessionGetsOneSlotBeforeAnyGetsASecond(t *testing.T) {
	const limit = 3
	f := newFixture(t, limit, map[string]int{"a": 1, "b": 1, "c": 1})

	// a fills the machine, and b and c reveal their demand by being refused.
	f.askN("a", "x", limit)
	for _, id := range []string{"b", "c"} {
		if f.ask(id, "x") {
			t.Fatalf("%s was admitted to a full machine", id)
		}
	}

	// a's builds finish and it reaches straight for all three again.
	f.finish("a")
	f.askN("a", "y", limit)

	// One slot each is the whole budget, so a must have been held to one.
	if got := f.count("a"); got != 1 {
		t.Fatalf("a took %d of %d slots while b and c were contending for their first, want 1", got, limit)
	}
	for _, id := range []string{"b", "c"} {
		if !f.ask(id, "y") {
			t.Fatalf("%s could not get a first slot while a held %d", id, f.count("a"))
		}
	}
}

// TestUnclaimedShareIsRedistributed is the second sentence of section 6.1, and it guards
// against over-restriction rather than under-restriction: a registered session that is not
// asking must not reserve capacity.
//
// This is the shape that would make the governor worse than useless — six sessions registered,
// one building, and a naive limit/n rule capping the only session doing any work at a sixth of
// the machine while five-sixths sat idle.
func TestUnclaimedShareIsRedistributed(t *testing.T) {
	const limit = 6
	sessions := map[string]int{"worker": 1}
	for i := 0; i < 6; i++ {
		sessions[fmt.Sprintf("idle%d", i)] = 1
	}
	f := newFixture(t, limit, sessions)

	if got := f.askN("worker", "x", limit); got != limit {
		t.Fatalf("the only session asking got %d of %d slots; idle registered sessions are reserving share they are not using", got, limit)
	}
}

// TestPartialDemandIsRedistributed is max-min proper: a session holding less than its share
// and asking for no more does not pin the rest of the machine idle.
//
// Two sessions of equal weight on a budget of six, one of which took a single slot and stopped
// asking. The other is entitled to the remaining five, not to three.
func TestPartialDemandIsRedistributed(t *testing.T) {
	const limit = 6
	f := newFixture(t, limit, map[string]int{"modest": 1, "busy": 1})

	if !f.ask("modest", "x") {
		t.Fatal("modest could not take its one slot")
	}
	if got := f.askN("busy", "x", 10); got != limit-1 {
		t.Fatalf("busy got %d slots beside a neighbour holding 1 of %d; the unclaimed share was not redistributed", got, limit)
	}
}

// TestFairShareRespectsSessionWeight checks the share is per unit of registered weight, which
// is what GOV-B02's focus boost will move — and the only thing that should.
//
// Both sessions establish demand by being refused, the machine drains, and then each asks for
// everything. The weighted caps are what decide the refill: weight 3 against weight 1 on a
// budget of 8 is 6 and 2. Without the rule the first to ask takes all eight.
func TestFairShareRespectsSessionWeight(t *testing.T) {
	const limit = 8
	f := newFixture(t, limit, map[string]int{"boosted": 3, "plain": 1})

	// Establish that both are contending.
	f.askN("boosted", "a", limit)
	if f.ask("plain", "a") {
		t.Fatal("plain was admitted to a full machine")
	}
	f.finish("boosted")

	// Both now reach for the whole machine, the boosted session first.
	f.askN("boosted", "b", limit)
	f.askN("plain", "b", limit)

	if got, want := f.count("boosted"), 6; got != want {
		t.Fatalf("boosted (weight 3) took %d slots of %d, want %d", got, limit, want)
	}
	if got, want := f.count("plain"), 2; got != want {
		t.Fatalf("plain (weight 1) took %d slots of %d, want %d", got, limit, want)
	}
}

// TestFairShareStaysWorkConserving is invariant 4 surviving the new rule. A Heavy build weighs
// 3.75 slots and the budget is floored at 1, so it can never fit; refusing it would idle the
// machine rather than govern it.
func TestFairShareStaysWorkConserving(t *testing.T) {
	tbl := NewTable(Options{})
	tbl.StartSession(Session{ID: "a", Weight: 1})
	tbl.StartSession(Session{ID: "b", Weight: 1})

	heavy := Request{Session: "a", Kind: Build, Weight: 3.75, ToolUseID: "a-heavy"}
	if _, ok := tbl.Acquire(heavy, 1); !ok {
		t.Fatal("a Heavy build was refused an empty table on a budget of 1: the machine would sit idle refusing the only work it had")
	}
	if _, ok := tbl.Acquire(Request{Session: "b", Kind: Build, Weight: 3.75, ToolUseID: "b-heavy"}, 1); ok {
		t.Fatal("a second Heavy build was admitted over budget; the work-conserving escape admits one at a time, not many")
	}
}

// TestFairShareReleasesToTheContendingSession is the bound on starvation at the admission
// layer: once a second session is contending, the incumbent cannot re-take the slots it frees.
//
// This is what makes the wait queue's FIFO effective rather than decorative. Without it a
// session that released a slot and immediately asked again would recapture it before the
// waiter it was freed for could be woken, and no queue discipline could help.
func TestFairShareReleasesToTheContendingSession(t *testing.T) {
	const limit = 4
	f := newFixture(t, limit, map[string]int{"incumbent": 1, "newcomer": 1})

	f.askN("incumbent", "x", limit)
	if f.ask("newcomer", "x") {
		t.Fatal("newcomer was admitted to a full machine")
	}

	// One build finishes.
	f.tbl.Drop(f.held["incumbent"][0])
	f.held["incumbent"] = f.held["incumbent"][1:]

	if f.ask("incumbent", "retake") {
		t.Fatal("the incumbent recaptured the slot it just freed while the newcomer was contending: starvation is unbounded")
	}
	if !f.ask("newcomer", "y") {
		t.Fatal("the newcomer could not take the freed slot")
	}
}

// TestDemandExpiresSoAGoneSessionStopsShrinkingEveryoneElse is the other half of the demand
// window. A session that asked once, was refused and went away must not depress the machine's
// fair share forever.
func TestDemandExpiresSoAGoneSessionStopsShrinkingEveryoneElse(t *testing.T) {
	const limit = 4
	c := newClock()
	tbl := NewTable(Options{Now: c.now})
	tbl.StartSession(Session{ID: "worker", Weight: 1})
	tbl.StartSession(Session{ID: "passerby", Weight: 1})

	for i := 0; i < limit; i++ {
		tbl.Acquire(buildFor("worker", fmt.Sprint(i)), limit)
	}
	// The passer-by asks once, is refused, and is never heard from again.
	if _, ok := tbl.Acquire(buildFor("passerby", "x"), limit); ok {
		t.Fatal("passerby was admitted to a full machine")
	}
	// The worker's builds finish.
	for _, l := range tbl.Leases() {
		tbl.Drop(l.ID)
	}
	// While the passer-by still counts as contending, the worker is held to half the machine.
	if got := acquireN(tbl, "worker", "y", limit, limit); got != limit/2 {
		t.Fatalf("worker took %d slots while a refused session was still contending, want %d", got, limit/2)
	}

	// Once the window passes and the reaper sweeps, the worker has the machine back.
	for _, l := range tbl.Leases() {
		tbl.Drop(l.ID)
	}
	c.advance(demandWindow + time.Second)
	tbl.Reap()
	if got := acquireN(tbl, "worker", "z", limit, limit); got != limit {
		t.Fatalf("worker took %d of %d slots long after the passer-by stopped asking; stale demand is shrinking the machine", got, limit)
	}
}

func acquireN(tbl *Table, session, tag string, n, limit int) int {
	got := 0
	for i := 0; i < n; i++ {
		if _, ok := tbl.Acquire(buildFor(session, fmt.Sprintf("%s%d", tag, i)), limit); ok {
			got++
		}
	}
	return got
}

// TestFitsAgreesWithAcquireUnderFairShare is GOV-16's fourth clause, re-checked now that the
// admission rule has a second half. The advisory channel must not tell a model it will fit
// when fair share is about to refuse it.
func TestFitsAgreesWithAcquireUnderFairShare(t *testing.T) {
	tbl := NewTable(Options{})
	const limit = 4
	tbl.StartSession(Session{ID: "a", Weight: 1})
	tbl.StartSession(Session{ID: "b", Weight: 2})

	for i := 0; i < 12; i++ {
		for _, id := range []string{"a", "b"} {
			r := buildFor(id, fmt.Sprintf("check-%d", i))
			want := tbl.Fits(r, limit)
			_, got := tbl.Acquire(r, limit)
			if want != got {
				t.Fatalf("Fits said %v and Acquire did %v for session %s: the advisory channel and the gate have drifted", want, got, id)
			}
		}
	}
}

// TestFitsRecordsNoDemand is why Fits is not simply Acquire with the write removed. The
// advisory channel is called speculatively; if asking the question counted as demand, a model
// could inflate its own fair share just by asking, and every other session's would shrink.
func TestFitsRecordsNoDemand(t *testing.T) {
	const limit = 4
	f := newFixture(t, limit, map[string]int{"worker": 1, "asker": 1})
	f.askN("worker", "x", limit)

	// The asker only ever asks the advisory channel, never the gate.
	for i := 0; i < 20; i++ {
		f.tbl.Fits(buildFor("asker", fmt.Sprint(i)), limit)
	}
	f.finish("worker")

	if got := f.askN("worker", "y", limit); got != limit {
		t.Fatalf("worker got %d of %d slots after a session merely ASKED whether it would fit %d times; advice is consuming share", got, limit, 20)
	}
}

// TestFairShareIsCheapOnTheHotPath is GOV-18's third trap, made falsifiable.
//
// The gate now fires on every build-shaped Bash call rather than 88 times in 101 hours, so a
// fairness computation that was fine at spawn frequency has to be re-measured at shell
// frequency. docs/bash-corpus.md puts that at 184-2,221 Bash calls a day of which 18.4% are
// gated; design.md section 4.4 budgets the whole gate at 10 ms. An admission decision costing
// microseconds is invisible against that, and this asserts a bound two orders of magnitude
// inside it rather than a number that merely looks small.
func TestFairShareIsCheapOnTheHotPath(t *testing.T) {
	tbl := NewTable(Options{})
	// Six sessions is the machine this project was built for: Phase 0 recorded 5-6
	// concurrent sessions for 98.9% of its window.
	const sessions = 6
	const limit = 6
	for i := 0; i < sessions; i++ {
		id := fmt.Sprintf("s%d", i)
		tbl.StartSession(Session{ID: id, Weight: 1})
		tbl.Acquire(buildFor(id, "held"), limit)
		// Every session contending, which is the most expensive shape the filling loop has.
		tbl.Acquire(buildFor(id, "refused"), 0)
	}

	const n = 20000
	r := Request{Session: "s0", Kind: Build, Weight: 1}
	start := time.Now()
	for i := 0; i < n; i++ {
		tbl.Fits(r, limit)
	}
	per := time.Since(start) / n

	// 100 microseconds is a hundredth of the gate's whole 10 ms budget and is a ceiling for a
	// slow shared CI runner, not a target. The real figure on this machine is hundreds of
	// nanoseconds; this fails when the rule acquires a walk over something unbounded.
	if per > 100*time.Microsecond {
		t.Fatalf("admission decision costs %v with %d contending sessions, a real fraction of the gate's 10 ms budget", per, sessions)
	}
	t.Logf("admission decision: %v per call with %d contending sessions", per, sessions)
}
