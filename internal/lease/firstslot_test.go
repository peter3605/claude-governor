package lease

import "testing"

// GOV-34: a session asking for its FIRST slot is not refused because another session has one.
//
// These test the bound as hard as they test the escape, because an unbounded escape is not a
// fix — it is the budget removed for anybody willing to hold nothing. The rule admits one
// lease over budget at a time, once per session, and only while the machine has room. Each
// of those three clauses gets a test below, and each of them fails a different way if the
// condition is dropped.

// heavy is a full-toolchain build: budget.SlotsFor(4.5) = 3.75 slots. It is spelled out
// rather than imported so internal/lease keeps no dependency on internal/budget, and it is
// the weight the 2026-09-21 incident turned on — too big to sit beside anything at all on a
// four-slot ceiling.
const heavy = 3.75

func heavyFor(session, tag string) Request {
	return Request{Session: session, Kind: Build, Weight: heavy, ToolUseID: session + "-" + tag}
}

// The incident itself, at the level of the rule: budget 4, one confirmed lease of one slot
// held by somebody else, three slots free, and a Heavy build refused for "capacity".
func TestAFirstSlotIsAdmittedWhileTheMachineHasRoom(t *testing.T) {
	tbl := NewTable(Options{})
	if _, ok := tbl.Acquire(buildFor("holder", "1"), 4); !ok {
		t.Fatal("the holder could not take its slot; the test proves nothing")
	}

	if _, ok := tbl.Acquire(heavyFor("blocked", "1"), 4); !ok {
		t.Fatal("a session holding nothing was refused its first slot on a machine with three of four free")
	}
	if r := tbl.Refusal(heavyFor("blocked", "2"), 4); r == "" {
		t.Error("the escape is still open after it has been used; it must fire once, not become the rule")
	}
}

// The `t.weight < limit` half. A machine with nothing free is genuinely at capacity, waiting
// genuinely works, and section 6.2's FIFO is what serves the waiter — so the escape must not
// fire, or every refusal on a full machine becomes an admission.
func TestAFullMachineStillRefusesAFirstSlot(t *testing.T) {
	tbl := NewTable(Options{})
	for i := 0; i < 4; i++ {
		if _, ok := tbl.Acquire(buildFor("holder", string(rune('a'+i))), 4); !ok {
			t.Fatalf("could not fill the table to 4 of 4 (stopped at %d)", i)
		}
	}
	if tbl.Slots() != 4 {
		t.Fatalf("table holds %.2f slots, want exactly 4: this is not the full-machine case", tbl.Slots())
	}
	if _, ok := tbl.Acquire(buildFor("newcomer", "1"), 4); ok {
		t.Error("a session was admitted to a machine with nothing free; the escape needs headroom and had none")
	}
}

// The bound. At most one lease is over budget at a time, so the table's weight never exceeds
// the budget plus one lease however many sessions hold nothing. Without this clause, ten
// idle sessions each start a Heavy build and the budget governs nothing at all.
func TestOnlyOneLeaseIsEverOverBudget(t *testing.T) {
	tbl := NewTable(Options{})
	if _, ok := tbl.Acquire(buildFor("holder", "1"), 4); !ok {
		t.Fatal("the holder could not take its slot")
	}
	if _, ok := tbl.Acquire(heavyFor("first", "1"), 4); !ok {
		t.Fatal("the first escape was refused; the rest of this test is meaningless")
	}

	// The table is now over budget. Every further session holding nothing must be refused,
	// however long it has been idle and however small it asks.
	for _, s := range []string{"second", "third", "fourth"} {
		if _, ok := tbl.Acquire(buildFor(s, "1"), 4); ok {
			t.Errorf("session %q was admitted while the table was already over budget at %.2f/4", s, tbl.Slots())
		}
	}
	if want := 1 + heavy; tbl.Slots() != want {
		t.Errorf("table holds %.2f slots, want exactly %.2f: the overshoot is not bounded at one lease", tbl.Slots(), want)
	}
}

// The `heldLocked(session) == 0` half, stated as the thing it prevents: a session cannot walk
// over the budget one lease at a time by never being the one that holds anything much.
func TestASessionCannotEscapeTwice(t *testing.T) {
	tbl := NewTable(Options{})
	if _, ok := tbl.Acquire(buildFor("holder", "1"), 4); !ok {
		t.Fatal("the holder could not take its slot")
	}
	if _, ok := tbl.Acquire(heavyFor("greedy", "1"), 4); !ok {
		t.Fatal("the first escape was refused; the rest of this test is meaningless")
	}
	if _, ok := tbl.Acquire(heavyFor("greedy", "2"), 4); ok {
		t.Errorf("the same session escaped twice, reaching %.2f against a budget of 4", tbl.Slots())
	}
}

// Fits and Refusal answer from the same rule as Acquire, so the advisory channel (§8) cannot
// tell a model the gate would refuse something it would in fact admit. GOV-16 made that
// structural; widening the admission rule is exactly the kind of change that breaks it again.
func TestTheAdvisoryChannelAgreesWithTheEscape(t *testing.T) {
	tbl := NewTable(Options{})
	if _, ok := tbl.Acquire(buildFor("holder", "1"), 4); !ok {
		t.Fatal("the holder could not take its slot")
	}
	req := heavyFor("blocked", "1")
	if !tbl.Fits(req, 4) {
		t.Error("Fits says no to a request Acquire admits; the advisory channel would tell a model to give up")
	}
	if got := tbl.Refusal(req, 4); got != "" {
		t.Errorf("Refusal = %q, want \"\": it disagrees with the rule it shares", got)
	}
}
