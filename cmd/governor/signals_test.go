package main

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
)

// stubSampler hands out a scripted reading and counts how many times it was asked.
//
// The count is the load-bearing part: the sampler's contract says the first read is a
// priming read whose rates are all zero, so "was it called twice before the first value was
// published" is the only way to tell a discarded prime from a published one.
type stubSampler struct {
	mu    sync.Mutex
	calls int
	err   error
	make  func(call int) signals.Sample
}

func (s *stubSampler) Sample(context.Context) (signals.Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return signals.Sample{}, s.err
	}
	return s.make(s.calls), nil
}

func (s *stubSampler) Host(context.Context) (signals.Host, error) {
	return signals.Host{Cores: 8, PerformanceCores: 8}, nil
}

func (s *stubSampler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// A daemon that has measured nothing must say so. Every signal absent is the honest answer;
// a zero would be a claim that the machine is idle, and that claim is what GOV-29's dead
// window taught this repo to distrust — a plausible reading of nothing at all.
func TestSignalsAreAbsentUntilATickLands(t *testing.T) {
	d := newDaemon(4, io.Discard)
	d.sampler = &stubSampler{make: func(int) signals.Sample { return signals.Sample{} }}

	got := d.status(request{})

	if got.CPU != nil {
		t.Errorf("cpu = %v before any tick, want null", *got.CPU)
	}
	for name, p := range map[string]*float64{
		"cpu_instant":           got.CPUInstant,
		"run_queue":             got.RunQueue,
		"cpu_capacity":          got.CPUCapacity,
		"swap_in_bytes_per_sec": got.SwapInBytesPerSec,
		"tree_cpu":              got.TreeCPU,
		"tree_cpu_roots":        got.TreeCPURoots,
	} {
		if p != nil {
			t.Errorf("%s = %v before any tick, want null", name, *p)
		}
	}
	if got.MemAvailableBytes != nil {
		t.Errorf("mem_available_bytes = %v before any tick, want null", *got.MemAvailableBytes)
	}
}

// The reading must reach the status reply, and it must be the SECOND reading: the first is
// the prime, whose CPU and process-tree rates are zero by construction because they are
// differences against a tick that does not exist yet. Publishing it would put a row of false
// zeroes in front of every reader for the daemon's first second.
func TestTheFirstSampleIsPrimedAwayAndTheSecondIsPublished(t *testing.T) {
	stub := &stubSampler{make: func(call int) signals.Sample {
		return signals.Sample{
			CPU:               float64(call) / 10,
			RunQueue:          float64(call),
			CPUCapacity:       8,
			CPUCapacitySource: "host",
			MemAvailableBytes: uint64(call) * 1024,
			SwapInBytesPerSec: float64(call) * 2,
			Tree:              signals.Tree{CPU: float64(call) / 4, CPURoots: float64(call) / 8},
		}
	}}

	d := newDaemon(4, io.Discard)
	d.sampler = stub

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.sample(ctx) }()

	waitFor(t, 5*time.Second, func() bool {
		d.signalsMu.Lock()
		defer d.signalsMu.Unlock()
		return d.latestOK
	})
	cancel()
	<-done

	if n := stub.count(); n < 2 {
		t.Fatalf("sampler was called %d time(s); the prime plus one tick is two", n)
	}

	got := d.status(request{})
	if got.CPUInstant == nil {
		t.Fatal("cpu_instant is null after a tick landed")
	}
	// Call 2's scripted value. Call 1's would be 0.1.
	if *got.CPUInstant != 0.2 {
		t.Errorf("cpu_instant = %v, want 0.2 (the second reading; the first is the discarded prime)", *got.CPUInstant)
	}
	if got.RunQueue == nil || *got.RunQueue != 2 {
		t.Errorf("run_queue = %v, want 2", got.RunQueue)
	}
	if got.CPUCapacity == nil || *got.CPUCapacity != 8 {
		t.Errorf("cpu_capacity = %v, want 8", got.CPUCapacity)
	}
	if got.CPUCapacitySource != "host" {
		t.Errorf("cpu_capacity_source = %q, want %q", got.CPUCapacitySource, "host")
	}
	if got.MemAvailableBytes == nil || *got.MemAvailableBytes != 2048 {
		t.Errorf("mem_available_bytes = %v, want 2048", got.MemAvailableBytes)
	}
	if got.SwapInBytesPerSec == nil || *got.SwapInBytesPerSec != 4 {
		t.Errorf("swap_in_bytes_per_sec = %v, want 4", got.SwapInBytesPerSec)
	}
	if got.TreeCPU == nil || *got.TreeCPU != 0.5 {
		t.Errorf("tree_cpu = %v, want 0.5", got.TreeCPU)
	}
	if got.TreeCPURoots == nil || *got.TreeCPURoots != 0.25 {
		t.Errorf("tree_cpu_roots = %v, want 0.25", got.TreeCPURoots)
	}
}

// status.cpu is the 5 s average, not the latest read. design.md section 5.1 specifies the
// EWMA, and the reason is in GOV-12's notes: a raw sample swings hard enough that anything
// acting on it oscillates. A single spike must not move the reported figure to the spike.
func TestReportedCPUIsTheAverageNotTheLatestSpike(t *testing.T) {
	d := newDaemon(4, io.Discard)
	d.cpuAvg = signals.NewEWMA(signals.DefaultCPUWindow)

	now := time.Now()
	// A steady, quiet machine for a minute.
	for i := 0; i < 60; i++ {
		d.cpuAvg.Update(0.1, now)
		now = now.Add(time.Second)
	}
	// Then one loud second.
	d.cpuAvg.Update(1.0, now)
	d.latest = signals.Sample{CPU: 1.0}
	d.latestOK = true

	got := d.status(request{})
	if got.CPU == nil || got.CPUInstant == nil {
		t.Fatal("cpu or cpu_instant is null after a reading")
	}
	if *got.CPUInstant != 1.0 {
		t.Errorf("cpu_instant = %v, want the raw 1.0", *got.CPUInstant)
	}
	if *got.CPU >= 1.0 {
		t.Errorf("cpu = %v after one spike on a quiet minute: that is the raw read, not a 5 s average", *got.CPU)
	}
	if *got.CPU <= 0.1 {
		t.Errorf("cpu = %v, want the spike to move the average somewhat", *got.CPU)
	}
}

// GOV-12's first trap, as a test. The budget is a fixed semaphore for as long as the
// dogfooding week is measuring it, and this story is observation only. A sample that would
// obviously justify cutting — CPU pinned, thermal critical, run queue deep — must change
// nothing the allocator reports.
//
// If this test ever needs changing, the story it belongs to is GOV-14, not this one.
func TestSamplingDoesNotMoveTheBudget(t *testing.T) {
	d := newDaemon(4, io.Discard)
	before := d.status(request{})

	d.signalsMu.Lock()
	d.latest = signals.Sample{
		CPU: 1.0, RunQueue: 12, Thermal: signals.Critical,
		CPUCapacity: 8, MemAvailableBytes: 0,
		Tree: signals.Tree{CPU: 8},
	}
	d.latestOK = true
	d.cpuAvg = signals.NewEWMA(signals.DefaultCPUWindow)
	d.cpuAvg.Update(1.0, time.Now())
	d.signalsMu.Unlock()

	after := d.status(request{})

	if after.Budget != before.Budget {
		t.Errorf("budget moved from %d to %d on a pinned, critically hot machine; GOV-12 is observation only",
			before.Budget, after.Budget)
	}
	if after.InUse != before.InUse {
		t.Errorf("in_use moved from %d to %d because of a sample", before.InUse, after.InUse)
	}
	if after.CPU == nil {
		t.Error("the sample was not exposed at all, so this test proves nothing")
	}
}

// A failed reading must not blank a good one. The sampler runs unattended for days and a
// transient failure to read one signal is not a reason to tell every status caller that the
// machine is unmeasured.
func TestAFailedTickKeepsThePreviousReading(t *testing.T) {
	d := newDaemon(4, io.Discard)
	d.signalsMu.Lock()
	d.latest = signals.Sample{CPU: 0.4, CPUCapacity: 8}
	d.latestOK = true
	d.signalsMu.Unlock()

	stub := &stubSampler{err: context.DeadlineExceeded, make: func(int) signals.Sample { return signals.Sample{} }}
	d.sampler = stub

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.sample(ctx) }()
	waitFor(t, 5*time.Second, func() bool { return stub.count() >= 2 })
	cancel()
	<-done

	got := d.status(request{})
	if got.CPUInstant == nil || *got.CPUInstant != 0.4 {
		t.Errorf("cpu_instant = %v after a failed tick, want the previous 0.4 to stand", got.CPUInstant)
	}
}

// A daemon with no sampler wired must not start one. Every test in this package that does
// not ask for a sampler would otherwise walk the runner's whole process table once a second.
func TestSampleIsANoOpWithoutASampler(t *testing.T) {
	d := newDaemon(4, io.Discard)
	if d.sampler != nil {
		t.Fatal("newDaemon wired a real sampler; tests would measure the runner")
	}

	done := make(chan struct{})
	go func() { defer close(done); d.sample(context.Background()) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sample() with no sampler did not return; it is ticking against nothing")
	}
}

// waitFor polls until cond holds or the deadline passes.
//
// Polling rather than a channel because the condition is a mutex-guarded field written by a
// goroutine this test does not own, and because a poll that times out reports which condition
// failed rather than deadlocking the suite.
func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition did not hold within %v", within)
}

// silentSampler never returns a reading until the daemon shuts down, so no tick can land.
//
// It is what makes "before the first sample" a state the test holds rather than a race it
// hopes to win. A stub that answered would be read within a second, and an acquire that beat
// it would pass for the wrong reason on a fast machine and fail on a slow one.
type silentSampler struct{}

func (silentSampler) Sample(ctx context.Context) (signals.Sample, error) {
	<-ctx.Done()
	return signals.Sample{}, ctx.Err()
}

func (silentSampler) Host(context.Context) (signals.Host, error) { return signals.Host{}, nil }

// A daemon answers its ceiling before its first sample, not the floor (GOV-41).
//
// THIS IS HELD AT THE ADMISSION PATH ON PURPOSE. The controller always meant to start at the
// ceiling — start() says so — and a test of Controller alone passed throughout the defect,
// because the defect was observeThisMachine never giving it one. So this runs the real
// wiring, then replaces only what reads the developer's machine: the session registry, the
// hook mtime, and the sampler, which is swapped for one that never answers.
//
// The capacity is pinned so the ceiling is well above the floor on every runner. Read for
// real on a two-core CI host it would floor anyway, and the test could not tell the fix from
// the bug.
func TestAColdDaemonServesItsCeilingNotTheFloor(t *testing.T) {
	// An empty data directory, so a config left by a calibration on this machine — or a
	// plugin context the tests happen to run inside — cannot cap the ceiling under the test.
	t.Setenv(config.DataDirEnv, t.TempDir())

	const capacity = 12
	saved := hostCapacity
	hostCapacity = func() (float64, string) { return capacity, "host" }
	t.Cleanup(func() { hostCapacity = saved })

	want := budget.MaxSlots(capacity)
	if want <= budget.Floor {
		t.Fatalf("MaxSlots(%v) = %d, which is the floor: this test could not tell a cold daemon from a warm one", float64(capacity), want)
	}

	endpoint, _ := startTestDaemonConfigured(t, budget.Floor, lease.Options{}, func(d *daemon) {
		d.observeThisMachine()
		d.liveSessions = noLiveSessions
		d.hooksInstalledAt = func() time.Time { return time.Time{} }
		d.sampler = silentSampler{}
	})

	got := call(t, endpoint, acquireReq("s1"))
	if got.OK == nil || !*got.OK {
		t.Fatalf("acquire refused against an empty table: %+v", got)
	}
	if got.Budget != want {
		t.Fatalf("a cold daemon answered budget %d, want its ceiling %d — Max was not set before the first tick", got.Budget, want)
	}

	// And it says why, consistently: at its ceiling, cutting nothing, having measured nothing.
	st := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "status"})
	if st.Ceiling != want || st.Cut != "" {
		t.Errorf("status reports ceiling %d cut %q, want %d and none", st.Ceiling, st.Cut, want)
	}
	if st.CPU != nil {
		t.Errorf("status reports cpu %v before any tick, want null", *st.CPU)
	}
}
