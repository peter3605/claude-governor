package signals

import (
	"context"
	"testing"
	"time"
)

// The tree walk is the entire cost of sampling — 23.0 ms of the ~24 ms a tick takes, measured
// over 46 hours of the Phase 0 sampler. Carrying it forward between walks is what brings the
// daemon under GOV-12's 1%-of-a-core bar, so this asserts the carry actually happens rather
// than trusting that a duration field is consulted.
func TestTreeIsCarriedOverBetweenWalks(t *testing.T) {
	s := newSamplerNamed(selfRootName(t))
	s.treeEvery = time.Hour // long enough that only one walk can happen after priming
	ctx := context.Background()

	// Prime first. The priming walk is deliberately not cached (see
	// TestThePrimingWalkIsNotCachedAndRepublished), so the walk worth carrying is the next one.
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}

	first, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("first sample: %v", err)
	}
	if !first.TreeFresh {
		t.Fatal("the first post-priming sample did not walk the tree; there was nothing to carry forward")
	}
	if first.Tree.Roots == 0 {
		t.Fatal("the first walk found no roots, so a carried-over copy proves nothing")
	}

	for i := 0; i < 3; i++ {
		got, err := s.Sample(ctx)
		if err != nil {
			t.Fatalf("sample %d: %v", i+2, err)
		}
		if got.TreeFresh {
			t.Errorf("sample %d walked the tree inside the interval; the cost saving is not being taken", i+2)
		}
		if got.Tree != first.Tree {
			t.Errorf("sample %d carried a different tree: got %+v, want %+v", i+2, got.Tree, first.Tree)
		}
	}
}

// The other half: once the interval has elapsed the walk must actually happen again. A
// sampler that carries the first reading forever would pass the test above and report a
// process tree frozen at boot.
//
// The elapsing is staged by rewinding the recorded walk time rather than by sleeping. A
// sleep would race the walk itself — one walk costs ~23 ms, so any interval short enough to
// keep the test quick is one the walk can overrun on a loaded runner, and the test would fail
// for the machine's reasons rather than the code's. That is the flake class GOV-B04, GOV-B08
// and GOV-B10 were each filed to repair; there is no reason to introduce a fourth.
func TestTreeIsRewalkedOnceTheIntervalElapses(t *testing.T) {
	s := newSamplerNamed(selfRootName(t))
	s.treeEvery = time.Hour
	ctx := context.Background()

	// Two samples: the prime, then the walk that actually gets cached.
	for i := 0; i < 2; i++ {
		if _, err := s.Sample(ctx); err != nil {
			t.Fatalf("prime %d: %v", i, err)
		}
	}
	// Inside the interval: carried.
	inside, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("inside: %v", err)
	}
	if inside.TreeFresh {
		t.Error("walked again immediately, inside the interval")
	}

	// Stage the interval having passed, with no clock in it.
	s.lastTreeAt = s.lastTreeAt.Add(-2 * time.Hour)

	after, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if !after.TreeFresh {
		t.Error("the interval elapsed and the tree was still not re-walked: the reading would be frozen forever")
	}
}

// Phase 0's trace must keep the semantics its numbers were collected with. NewSampler walks
// every tick and the setpoint will be tuned against traces that assume it; a change here
// would silently re-define the data GOV-15 reads back.
func TestTheDefaultSamplerStillWalksEveryTick(t *testing.T) {
	s := newSamplerNamed(selfRootName(t))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got, err := s.Sample(ctx)
		if err != nil {
			t.Fatalf("sample %d: %v", i+1, err)
		}
		if !got.TreeFresh {
			t.Errorf("sample %d carried a stale tree; the default sampler must walk every tick for Phase 0", i+1)
		}
	}
}

// A rate needs two readings. The first tick has one, and reporting the swap counter's
// lifetime total as a per-second rate would show a machine that had ever swapped as one
// swapping catastrophically right now.
func TestSwapRateIsZeroUntilThereAreTwoReadings(t *testing.T) {
	s := newSamplerNamed(selfRootName(t))
	ctx := context.Background()

	first, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("first sample: %v", err)
	}
	if first.SwapInBytesPerSec != 0 {
		t.Errorf("priming sample reported a swap-in rate of %v, want 0", first.SwapInBytesPerSec)
	}

	second, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("second sample: %v", err)
	}
	if second.SwapInBytesPerSec < 0 {
		t.Errorf("swap-in rate is negative (%v); the counter went backwards and was not guarded",
			second.SwapInBytesPerSec)
	}
}

// A counter that goes backwards — which happens on swapoff — must read as zero and not as
// an enormous positive rate through an unsigned subtraction.
func TestSwapRateIgnoresACounterThatWentBackwards(t *testing.T) {
	s := newSamplerNamed(selfRootName(t))
	now := time.Now()

	// Stage a previous reading far above anything the machine will report now.
	s.swapIn, s.swapAt, s.swapSeen = ^uint64(0), now.Add(-time.Second), true

	if got := s.swapRate(context.Background(), now); got != 0 {
		t.Errorf("swap rate after the counter reset = %v, want 0", got)
	}
}

// RunQueue divides by the cores the process may actually use, so a cgroup-limited host
// divides by its quota. Whatever this platform reports, the divisor must be the capacity
// figure and not the raw core count — otherwise the cgroup work above changes nothing that
// the control loop can see.
func TestRunQueueDividesByCapacity(t *testing.T) {
	s := newSamplerNamed(selfRootName(t))
	got, err := s.Sample(context.Background())
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got.CPUCapacity <= 0 {
		t.Fatalf("CPUCapacity = %v, want positive", got.CPUCapacity)
	}
	if got.LoadAvg1 == 0 {
		t.Skip("loadavg unavailable on this host; nothing to divide")
	}
	want := got.LoadAvg1 / got.CPUCapacity
	if got.RunQueue != want {
		t.Errorf("RunQueue = %v, want loadavg/capacity = %v (divisor is not CPUCapacity)", got.RunQueue, want)
	}
}

// The first walk of a sampler's life has no previous reading to difference against, so its
// CPU figures are zero by construction. Caching it would republish that manufactured zero on
// every carried tick until the interval elapsed — five consecutive samples, on the daemon's
// cadence, each claiming the claude tree is idle.
//
// Found end to end rather than by reading the code: a daemon queried three seconds after
// start reported tree_cpu 0 with agents running.
func TestThePrimingWalkIsNotCachedAndRepublished(t *testing.T) {
	s := newSamplerNamed(selfRootName(t))
	s.treeEvery = time.Hour
	ctx := context.Background()

	// The priming sample walks, but must not retain what it walked.
	first, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	if !first.TreeFresh {
		t.Fatal("the priming sample did not walk at all")
	}
	if !s.lastTreeAt.IsZero() {
		t.Error("the priming walk was cached; its zero CPU would be republished for a whole interval")
	}

	// So the next tick walks again, despite the hour-long interval, and this one can carry
	// real figures.
	second, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.TreeFresh {
		t.Error("the tick after priming carried the priming walk instead of re-walking")
	}
	if s.lastTreeAt.IsZero() {
		t.Error("the primed walk was not cached, so every tick will now pay for a walk")
	}

	// And from there the cache holds as usual.
	third, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if third.TreeFresh {
		t.Error("still walking every tick after priming; the cost saving never starts")
	}
}
