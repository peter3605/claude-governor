package budget

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The replay test of GOV-15 clause 2, driven from a REAL trace rather than a synthetic one.
//
// aimd_test.go drives the loop over shapes chosen to exercise each branch, which is the right
// way to test a branch and the wrong way to find out whether a control loop is stable on the
// machine it will run on. Real load is not a square wave: it is bursty, it crosses the
// setpoint hundreds of times an hour, and its run queue has a tail that no synthetic generator
// would have thought to produce.
//
// testdata/trace.csv is 3,000 contiguous 1 Hz host samples taken from this machine's Phase 0
// trace — the same samples.jsonl the constants in aimd.go were derived from, read back
// through the same fields. The window is the one with the most crossings of the setpoint in
// the whole 312,515-sample trace, because a loop that oscillates does it while the load is
// MOVING, and a window picked for high variance alone turned out to be pegged at 1.0 from end
// to end, which tests only the cut.
//
// What the window contains, and why it is a fair test:
//
//	cpu        p10 0.229   p50 0.595   p90 1.000   max 1.000
//	runqueue   p10 0.618   p50 2.872   p90 18.109  max 49.620
//	thermal    nominal 2,999, fair 1, severe 0
//	           328 crossings of the setpoint
//
// The run queue reaching 49.6 per core matters more than the CPU figure: CPU% saturates at
// 1.0 and stops carrying information, so at the top of this window the run queue is the only
// signal still moving. That is precisely the state the loop exists for.

func loadTrace(t *testing.T) []Reading {
	t.Helper()
	f, err := os.Open("testdata/trace.csv")
	if err != nil {
		t.Fatalf("the replay trace is the evidence this test rests on: %v", err)
	}
	defer f.Close()

	var out []Reading
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			t.Fatalf("malformed trace line %q", line)
		}
		cpu, err1 := strconv.ParseFloat(parts[0], 64)
		rq, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("malformed trace line %q", line)
		}
		out = append(out, Reading{
			CPU:      cpu,
			RunQueue: rq,
			// serious/critical never occurs in the trace, which is itself the finding
			// recorded at Controller.Step. `fair` is deliberately NOT severe.
			ThermalSevere: parts[2] == "serious" || parts[2] == "critical",
			// Demand is held true throughout so that the loop is free to climb on every
			// tick it is entitled to. That is the ADVERSARIAL setting for this test: it
			// removes the thing that would otherwise damp the increase, so any oscillation
			// the loop is capable of shows up here.
			DemandWaiting: true,
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if len(out) < 1000 {
		t.Fatalf("trace has only %d readings; this test is not meaningful on a short one", len(out))
	}
	return out
}

// TestTheLoopReplayedAgainstARealTrace is clause 2's three assertions, over real load.
func TestTheLoopReplayedAgainstARealTrace(t *testing.T) {
	trace := loadTrace(t)
	c := &Controller{Max: MaxSlots(8)}

	budgets := make([]int, 0, len(trace))
	for i, r := range trace {
		b := c.Step(r)
		budgets = append(budgets, b)
		if b < Floor {
			t.Fatalf("tick %d: budget %d below the floor of %d", i, b, Floor)
		}
		if b > c.Max {
			t.Fatalf("tick %d: budget %d above B_max of %d", i, b, c.Max)
		}
	}

	// SETTLING, over a trace that never stops moving, cannot mean "stops changing" — the load
	// itself changes 328 times. What it must mean is that the loop is not THRASHING: it must
	// not be reversing direction on a large fraction of ticks, which is what an unstable loop
	// looks like and what a merely-responsive one does not.
	reversals := 0
	var prev int
	for i := 1; i < len(budgets); i++ {
		d := budgets[i] - budgets[i-1]
		if d == 0 {
			continue
		}
		dir := 1
		if d < 0 {
			dir = -1
		}
		if prev != 0 && dir != prev {
			reversals++
		}
		prev = dir
	}
	if rate := float64(reversals) / float64(len(budgets)); rate > 0.10 {
		t.Errorf("the budget reversed direction on %.1f%% of ticks (%d of %d); a loop that "+
			"hunts this hard leaves the machine alternately starved and over-subscribed",
			rate*100, reversals, len(budgets))
	}

	// And it must actually have MOVED. A loop pinned at the floor for the whole trace would
	// satisfy every assertion above and govern nothing, which is the failure mode that looks
	// most like success.
	seen := map[int]bool{}
	for _, b := range budgets {
		seen[b] = true
	}
	if len(seen) < 3 {
		t.Errorf("the budget only ever took %d distinct values over a trace that crosses the "+
			"setpoint 328 times; it is not tracking the load", len(seen))
	}

	// It must spend real time at the floor — this window is saturated for much of its length,
	// and a loop that never reached the floor under a run queue of 49 per core is not cutting.
	atFloor := 0
	for _, b := range budgets {
		if b == Floor {
			atFloor++
		}
	}
	if share := float64(atFloor) / float64(len(budgets)); share < 0.10 {
		t.Errorf("the budget sat at the floor for only %.1f%% of a window whose run queue "+
			"peaks at 49.6 per core; it is not reacting to saturation", share*100)
	}
	// ...and it must not be stuck there either.
	if share := float64(atFloor) / float64(len(budgets)); share > 0.95 {
		t.Errorf("the budget sat at the floor for %.1f%% of the trace; it is not recovering "+
			"when the machine goes quiet", share*100)
	}

	t.Logf("replay over %d real ticks: budget took %d distinct values, %d direction reversals, "+
		"%.1f%% of ticks at the floor, ceiling %d",
		len(budgets), len(seen), reversals, 100*float64(atFloor)/float64(len(budgets)), c.Max)
}

// The same trace with leases held and nothing waiting for a slot. The budget must never climb,
// because demand_waiting gates the increase on an in-use machine — and it must still cut,
// because a machine that is saturated is saturated whether or not this governor's queue has
// anyone in it.
//
// InUse is held true since GOV-46: with nothing held AND nothing waiting the governor is idle,
// and an idle governor returns a recovered budget to the ceiling on purpose
// (TestARecoveredCutReturnsToTheCeilingOnAnIdleMachine). The trace is host load with no lease
// record, so "something of ours is running throughout" is the reading that keeps this test
// about the gate it names.
func TestTheLoopDoesNotClimbOnARealTraceWithNoDemand(t *testing.T) {
	trace := loadTrace(t)
	c := &Controller{Max: MaxSlots(8)}

	prev := c.Budget()
	climbs := 0
	for _, r := range trace {
		r.DemandWaiting = false
		r.InUse = true
		b := c.Step(r)
		if b > prev {
			climbs++
		}
		prev = b
	}
	if climbs != 0 {
		t.Errorf("the budget climbed %d times with nothing waiting for a slot", climbs)
	}
	if c.Budget() != Floor {
		t.Errorf("budget = %d at the end of a saturated trace, want the floor: the cut does "+
			"not depend on anyone being in the queue", c.Budget())
	}
}
