package budget

import "testing"

// GOV-B53. Memory pressure is a cut trigger in design.md section 5.1, and until this story
// nothing in the controller read it: the daemon reported swap-in and the loop ignored it.

func pressed() Reading { return Reading{SwapIn: SwapInMax * 4, SwapInKnown: true} }
func quiet() Reading   { return Reading{SwapIn: 0, SwapInKnown: true} }

// Clause 1: a swap-in rate over the threshold cuts the budget multiplicatively, and memory is
// named as what cut it. Against main as of 2026-11-23 this fails: Reading had no memory field
// and the budget stayed at the ceiling.
func TestSwapInCutsTheBudget(t *testing.T) {
	c := &Controller{Max: 8}
	c.Step(quiet())
	if c.Budget() != 8 {
		t.Fatalf("budget = %d on a quiet machine, want the ceiling 8", c.Budget())
	}
	c.Step(pressed())
	if c.Budget() != 4 {
		t.Errorf("budget = %d after one pressed tick, want 4: memory halves, as the run queue does", c.Budget())
	}
	if c.Cut() != "memory" {
		t.Errorf("Cut() = %q, want \"memory\"", c.Cut())
	}
	if c.Moved() != MovedMemory {
		t.Errorf("Moved() = %q, want %q", c.Moved(), MovedMemory)
	}
	c.Step(pressed())
	c.Step(pressed())
	c.Step(pressed())
	if c.Budget() != Floor {
		t.Errorf("budget = %d after sustained pressure, want the floor %d", c.Budget(), Floor)
	}
}

// Exactly at the threshold is not over it, which is the same convention as the run queue.
func TestSwapInAtTheThresholdDoesNotCut(t *testing.T) {
	c := &Controller{Max: 8}
	c.Step(Reading{SwapIn: SwapInMax, SwapInKnown: true})
	if c.Budget() != 8 || c.Cut() != "" {
		t.Errorf("budget %d cut %q at exactly SwapInMax, want 8 and no cut", c.Budget(), c.Cut())
	}
}

// Clause 1, second half: the budget does not climb until memory has been clear for
// MemoryClearTicks, however much demand is waiting, and memory stays named while it holds.
func TestAMemoryCutHoldsUntilSwapInHasCleared(t *testing.T) {
	c := &Controller{Max: 8}
	c.Step(pressed())
	cut := c.Budget()
	for i := 0; i < MemoryClearTicks; i++ {
		r := quiet()
		r.DemandWaiting, r.InUse = true, true
		c.Step(r)
		if i < MemoryClearTicks-1 && c.Budget() != cut {
			t.Fatalf("tick %d after the cut: budget %d, want it held at %d until memory has cleared",
				i+1, c.Budget(), cut)
		}
		if i < MemoryClearTicks-1 && c.Cut() != "memory" {
			t.Fatalf("tick %d: Cut() = %q, want \"memory\" while the hold lasts", i+1, c.Cut())
		}
	}
	r := quiet()
	r.DemandWaiting, r.InUse = true, true
	c.Step(r)
	if c.Budget() <= cut {
		t.Errorf("budget %d after %d clear ticks, want it climbing past %d", c.Budget(), MemoryClearTicks+1, cut)
	}
	if c.Cut() != "" {
		t.Errorf("Cut() = %q once memory cleared and the run queue is low, want \"\"", c.Cut())
	}
}

// A burst inside the hold restarts it: one quiet second is not recovery.
func TestABurstDuringTheMemoryHoldRestartsIt(t *testing.T) {
	c := &Controller{Max: 8}
	c.Step(pressed())
	for i := 0; i < MemoryClearTicks-1; i++ {
		c.Step(quiet())
	}
	c.Step(pressed())
	at := c.Budget()
	for i := 0; i < MemoryClearTicks-1; i++ {
		r := quiet()
		r.DemandWaiting, r.InUse = true, true
		c.Step(r)
	}
	if c.Budget() != at {
		t.Errorf("budget %d, want it still held at %d: the second burst restarted the hold", c.Budget(), at)
	}
}

// Clause 5: an unreadable memory signal does not cut, however large the number beside it.
// The loop is platform-neutral, so this is the darwin and the Linux case at once; the
// platform halves are signals.swapInBytes, whose readability is tested there.
func TestAnUnknownSwapInDoesNotCut(t *testing.T) {
	c := &Controller{Max: 8}
	for i := 0; i < 10; i++ {
		c.Step(Reading{SwapIn: SwapInMax * 100, SwapInKnown: false})
	}
	if c.Budget() != 8 || c.Cut() != "" {
		t.Errorf("budget %d cut %q on an unknown swap-in, want 8 and no cut: unknown fails open",
			c.Budget(), c.Cut())
	}
	if MemoryPressed(Reading{SwapIn: SwapInMax * 100}) {
		t.Error("MemoryPressed is true for an unknown reading")
	}
}

// The wake trap: pages swap back in after sleep, and that is the wake, not pressure.
func TestSwapInJustAfterAWakeDoesNotCut(t *testing.T) {
	c := &Controller{Max: 8}
	c.Step(quiet())
	r := pressed()
	r.Woke = true
	c.Step(r)
	for i := 1; i < MemoryWakeGraceTicks; i++ {
		c.Step(pressed())
	}
	if c.Budget() != 8 {
		t.Fatalf("budget %d inside the wake grace, want 8: the swap-in after a wake is the wake", c.Budget())
	}
	c.Step(pressed())
	if c.Budget() != 4 || c.Cut() != "memory" {
		t.Errorf("budget %d cut %q once the grace ended, want 4 and \"memory\": pressure that outlasts the wake is pressure",
			c.Budget(), c.Cut())
	}
}

// Idle must not reset a memory-held budget to the ceiling while the hold lasts.
func TestIdleDoesNotOutrunTheMemoryHold(t *testing.T) {
	c := &Controller{Max: 8}
	c.Step(pressed())
	for i := 0; i < MemoryClearTicks-1; i++ {
		c.Step(quiet())
	}
	if c.Budget() == 8 {
		t.Errorf("budget returned to the ceiling during the memory hold")
	}
}

// After a memory cut clears inside the run queue's hold band, the run queue is what holds the
// budget, and the name follows (the GOV-46 rule that Cut names what holds it NOW).
func TestTheHoldBandNamesTheRunQueueOnceMemoryClears(t *testing.T) {
	c := &Controller{Max: 8}
	c.Step(pressed())
	hold := (RunQueueMax + (RunQueueMax - RunQueueBand)) / 2
	for i := 0; i < MemoryClearTicks+1; i++ {
		c.Step(Reading{RunQueue: hold, SwapInKnown: true, DemandWaiting: true, InUse: true})
	}
	if c.Cut() != "run queue" {
		t.Errorf("Cut() = %q in the hold band after memory cleared, want \"run queue\"", c.Cut())
	}
}
