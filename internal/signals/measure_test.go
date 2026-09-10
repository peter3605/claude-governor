package signals

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// The sampler cost measurement (GOV-12: "sampling costs under 1 percent of one core").
//
// A measurement, not an assertion, and opt-in — the same treatment cmd/governor's
// measure_test.go gives the latency figures, for the same reason recorded there. GOV-B04
// existed because a test asserting a wall-clock figure was measuring the CI runner's spare
// capacity rather than the code; GOV-B08 and GOV-B10 were both repairs to timing guards that
// went on flaking afterwards. A per-tick CPU ceiling checked on every push is that test a
// fourth time. What CI enforces instead is TestSamplerStaysCheap's coarse 100 ms wall-clock
// budget, which catches a sampler that has become catastrophically slow without pretending
// to measure a percentage on a shared runner.
//
//	GOVERNOR_MEASURE=1 go test ./internal/signals/ -run TestMeasureSampler -v
const measureEnv = "GOVERNOR_MEASURE"

// TestMeasureSamplerCost reports what the sampler costs, in CPU rather than wall clock.
//
// CPU TIME, NOT ELAPSED TIME, and the distinction is the whole point. Sample.CostMS is a
// wall-clock duration, which is the right thing to record in a trace — a tick that took
// 200 ms of wall time delayed the next one whatever it was doing — but it is the wrong
// thing to compare against "1 percent of one core". A percentage of a core is a claim about
// CPU consumed, and a sampler blocked on a syscall consumes none. Measuring the wrong one
// would either flatter or defame the number depending on how loaded the machine happened to
// be, which is precisely the property that makes wall-clock guards flake.
//
// The two rates it prints are the two the daemon actually runs at: a full walk every tick,
// which is what Phase 0 collects at, and the daemon's split cadence from GOV-12, where the
// process tree refreshes every treeEvery instead.
func TestMeasureSamplerCost(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skipf("set %s=1 to run the sampler cost measurement", measureEnv)
	}

	// The daemon ticks at 1 s and walks the tree at 5 s. Both are scaled down by the same
	// factor here so the walks-per-tick ratio is the one that actually ships; scaling only
	// the tick would report a tree cost amortised over five times too many samples.
	const treeEveryFactor = 5

	for _, tc := range []struct {
		name      string
		treeEvery time.Duration
	}{
		{"every tick (Phase 0)", 0},
		{"tree every 5th tick (daemon, GOV-12)", treeEveryFactor * sampleTickForMeasurement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSamplerNamed(ClaudeProcessName)
			s.treeEvery = tc.treeEvery
			ctx := context.Background()

			// Prime, then warm the root-name cache, so what is measured is the steady state
			// the daemon actually pays rather than the first two ticks of its life.
			for i := 0; i < 2; i++ {
				if _, err := s.Sample(ctx); err != nil {
					t.Fatalf("warm: %v", err)
				}
			}

			const ticks = 30
			startCPU := processCPU(t)
			start := time.Now()
			fresh := 0
			for i := 0; i < ticks; i++ {
				got, err := s.Sample(ctx)
				if err != nil {
					t.Fatalf("sample %d: %v", i, err)
				}
				if got.TreeFresh {
					fresh++
				}
				// Tick at the real rate. Without this the tree-interval case never elapses
				// its interval and would report a cost it will never actually pay.
				time.Sleep(sampleTickForMeasurement)
			}
			cpu := processCPU(t) - startCPU
			elapsed := time.Since(start)

			perTick := cpu / ticks
			// Against the SHIPPED 1 Hz rate, not the compressed rate this test ticks at.
			// Dividing by the test's own elapsed time would report a cost ten times too high
			// purely because the measurement hurried — the sampler's per-tick cost is what
			// was measured, and the rate it is paid at is a constant, not an observation.
			shareOfOneCore := float64(perTick) / float64(shippedSampleInterval)

			t.Logf("%s on %s/%s, %d cores", tc.name, runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
			t.Logf("  %d ticks over %v, %d of them walked the tree", ticks, elapsed.Round(time.Millisecond), fresh)
			t.Logf("  CPU per tick:                 %v", perTick.Round(time.Microsecond))
			t.Logf("  share of one core at 1 Hz:    %.2f%%", 100*shareOfOneCore)
		})
	}
}

// shippedSampleInterval is the rate the daemon actually ticks at (design.md section 5.1),
// and sampleTickForMeasurement is the compressed rate this test ticks at so a case does not
// take half a minute. Costs are reported against the former; only the pacing uses the latter.
const (
	shippedSampleInterval    = time.Second
	sampleTickForMeasurement = 100 * time.Millisecond
)

// processCPU returns the CPU time this process has consumed, user plus system.
//
// getrusage rather than a clock: this is the quantity "1 percent of one core" is a
// percentage of, and it is unaffected by how busy the rest of the machine is.
func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return user + sys
}
