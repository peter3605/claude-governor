package calibrate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
)

// TestTwoDifferentMachinesGetMateriallyDifferentValues is GOV-25's last Done-when clause,
// and it is the one that says whether any of this was worth doing.
//
// The whole story exists so that nothing tuned on one 8-core laptop ships as anyone else's
// default. A calibration that produced the same numbers everywhere would satisfy every other
// clause — it would measure the host, write a file, record provenance — and still be a
// decorative reimplementation of a constant.
//
// It is a test over Derive rather than over Measure because Derive is pure, which is the
// reason the two are separate functions. Asserting this against real hosts would need two
// machines; asserting it against readings needs only that the readings be real, and each one
// below is.
func TestTwoDifferentMachinesGetMateriallyDifferentValues(t *testing.T) {
	// A quiet CI box: the Phase 0 trace's own settled run queue, and no efficiency cores to
	// cost it anything under parallel load.
	quiet := Measurement{
		Cores: 8, CPUCapacity: 8, AmbientRunQueue: 0.35, AmbientOK: true,
		Probe: ProbeResult{OK: true, EffectiveCores: 7.8, Workers: 8, UsableReps: 5, Reps: 5},
	}
	// This laptop mid-work: the run queue docs/climb-condition.md §4.3 measured with one
	// build held and Xcode indexing an unrelated project, and the effective core count this
	// package measured on the same machine.
	working := Measurement{
		Cores: 8, PerformanceCores: 4, PerformanceCoresKnown: true, CPUCapacity: 8,
		AmbientRunQueue: 2.91, AmbientOK: true,
		Probe: ProbeResult{OK: true, EffectiveCores: 6.1, Workers: 8, UsableReps: 5, Reps: 5},
	}
	// A container with a two-core quota on a large host: the case signals.cpuCapacity exists
	// for, and the one where a host-wide core count is not merely imprecise but wrong.
	container := Measurement{
		Cores: 64, CPUCapacity: 2, CPUCapacitySource: "cgroup", AmbientRunQueue: 0.20, AmbientOK: true,
		Probe: ProbeResult{OK: true, EffectiveCores: 1.9, Workers: 64, UsableReps: 5, Reps: 5},
	}

	q, w, c := Derive(quiet), Derive(working), Derive(container)
	t.Logf("quiet     run-queue cut %.2f  b_max %d", q.RunQueueMax, q.BMax)
	t.Logf("working   run-queue cut %.2f  b_max %d", w.RunQueueMax, w.BMax)
	t.Logf("container run-queue cut %.2f  b_max %d", c.RunQueueMax, c.BMax)

	if w.RunQueueMax <= q.RunQueueMax {
		t.Errorf("a machine idling at %.2f per core got cut threshold %.2f, no higher than a machine idling at %.2f (%.2f)",
			working.AmbientRunQueue, w.RunQueueMax, quiet.AmbientRunQueue, q.RunQueueMax)
	}
	// The gap has to be big enough to matter to the loop, not merely non-zero. A
	// RunQueueBand is the width of the loop's own dead band, so two thresholds closer
	// together than that are the same operating point.
	if got := w.RunQueueMax - q.RunQueueMax; got < budget.RunQueueBand {
		t.Errorf("cut thresholds differ by %.2f, which is inside the loop's own dead band of %.2f: not a material difference",
			got, budget.RunQueueBand)
	}
	if c.BMax >= q.BMax {
		t.Errorf("a 2-core container got b_max %d, no smaller than an 8-core host's %d", c.BMax, q.BMax)
	}
}

// TestTheCutClearsOneSustainedBuildAndStillStopsTwo is the derivation, asserted.
//
// IT REPLACES A TEST THAT ASSERTED THE OPPOSITE, and the replacement is the point rather than
// an embarrassment. The first version of this package derived BuildHeadroom as 1.65 by
// subtracting the Phase 0 trace's settled run queue from GOV-31's 2.0, and this test asserted
// the pleasing consequence: a host idling where Phase 0's host idled calibrates back to exactly
// 2.0. The premise was falsified by re-running the sweep. GOV-31's concurrency curve is
// under-converged — its own document says so — so 1.65 put the cut BELOW what one sustained
// build produces, and the budget floored with one build running.
//
// So the property worth pinning is not "reproduces the old constant". It is the interval the
// measurement actually bounds, in docs/calibration.md §5:
//
//	cut >  4.07   the PEAK at one sustained build, so one build does not floor the budget
//	cut <= 5.83   the median at two, so a second build still trips the cut
//
// A calibration that violated the lower bound would reintroduce the defect; one that violated
// the upper bound would stop governing the case the governor exists for.
func TestTheCutClearsOneSustainedBuildAndStillStopsTwo(t *testing.T) {
	// The host both bounds were measured on, at the ambient it was measured at.
	const (
		ambient  = 0.65 // docs/calibration.md §2, rep1's settle
		oneBuild = 4.07 // §5, peak run queue per core under one sustained build
		twoBuild = 5.83 // §5, median under two
	)
	got := Derive(Measurement{Cores: 8, CPUCapacity: 8, AmbientRunQueue: ambient, AmbientOK: true}).RunQueueMax

	if got <= oneBuild {
		t.Errorf("cut %.2f does not clear one sustained build's peak of %.2f: the budget floors with a single build running, which is the defect this story exists to remove",
			got, oneBuild)
	}
	if got > twoBuild {
		t.Errorf("cut %.2f is above the median at two builds (%.2f): a second build would not trip it, so the loop has stopped governing build concurrency",
			got, twoBuild)
	}
}

// TestABusierHostGetsAHigherCutThanAQuietOne is the direction the whole calibration exists for,
// held separately from the absolute bounds above so that a change to BuildHeadroom cannot
// satisfy one and silently break the other.
func TestABusierHostGetsAHigherCutThanAQuietOne(t *testing.T) {
	at := func(ambient float64) float64 {
		return Derive(Measurement{Cores: 8, CPUCapacity: 8, AmbientRunQueue: ambient, AmbientOK: true}).RunQueueMax
	}
	quiet, working := at(0.35), at(2.91) // Phase 0's settled host; this one mid-work, §4.3
	if working-quiet < budget.RunQueueBand {
		t.Errorf("a host idling at 2.91 got cut %.2f against %.2f for one idling at 0.35 — a difference of %.2f, inside the loop's own dead band of %.2f",
			working, quiet, working-quiet, budget.RunQueueBand)
	}
}

// TestCalibrationNeverTightensTheThresholdBelowTheShippedConstant guards the one direction
// this calibration has no evidence for.
//
// 2.0 is the only value that has been measured against real build load. A machine quieter
// than Phase 0's would otherwise calibrate to something TIGHTER than that — trading a measured
// constant for an extrapolation, in the direction that throttles. The floor is what stops a
// calibration run on an idle machine at 3am from shipping that machine a worse threshold than
// it had before.
func TestCalibrationNeverTightensTheThresholdBelowTheShippedConstant(t *testing.T) {
	for _, ambient := range []float64{0, 0.01, 0.1, 0.2, 0.35} {
		if got := Derive(Measurement{Cores: 8, CPUCapacity: 8, AmbientRunQueue: ambient, AmbientOK: true}).RunQueueMax; got < budget.RunQueueMax {
			t.Errorf("ambient %.2f calibrated to %.2f, below the shipped floor of %.2f", ambient, got, budget.RunQueueMax)
		}
	}
}

// TestAnUnreadableHostFallsBackToTheShippedDefaults is the fail-safe direction.
//
// A host whose load average could not be read is NOT an idle host, and the difference began to
// matter when BuildHeadroom grew. At 1.65 an unreadable reading of zero landed on the shipped
// constant through the floor, so conflating "could not read" with "read zero" cost nothing. At
// 4.30 the floor no longer binds, and the same conflation would calibrate an unmeasured machine
// as though it were quiet — inferring a threshold from silence, which is the one thing Ambient's
// own comment says not to do.
//
// So a measurement with AmbientOK false produces NO threshold, and the daemon keeps the
// compiled-in one: the inert default design.md section 13.4 requires of every config key.
func TestAnUnreadableHostFallsBackToTheShippedDefaults(t *testing.T) {
	m := Measurement{Cores: 8, CPUCapacity: 8} // no ambient reading, no probe result
	d := Derive(m)
	if d.RunQueueMax != 0 {
		t.Errorf("run-queue cut %.2f from a host whose load could not be read; want 0, so the compiled-in %.2f stands",
			d.RunQueueMax, budget.RunQueueMax)
	}
	if want := budget.MaxSlots(8); d.BMax != want {
		t.Errorf("b_max %d from an unreadable host, want %d — the same ceiling it had before calibration", d.BMax, want)
	}
}

// TestAGenuinelyIdleHostIsNotMistakenForAnUnreadableOne is the other half of the bit above.
// A machine that really is doing nothing reads a real zero and must still be calibrated.
func TestAGenuinelyIdleHostIsNotMistakenForAnUnreadableOne(t *testing.T) {
	d := Derive(Measurement{Cores: 8, CPUCapacity: 8, AmbientRunQueue: 0, AmbientOK: true})
	if d.RunQueueMax <= 0 {
		t.Errorf("an idle host that WAS measured got no threshold (%.2f); AmbientOK is what separates it from one that could not be read", d.RunQueueMax)
	}
}

// TestTheProbeNeverClaimsMoreCapacityThanTheQuotaAllows.
//
// A cgroup quota is a hard ceiling. A throughput measurement above it is measuring something
// other than this container's capacity — the host's idle cores leaking into the reading, most
// likely — and acting on it would hand out a budget the kernel will then throttle flat, which
// is the exact failure signals.cpuCapacity's cgroup branch exists to prevent.
func TestTheProbeNeverClaimsMoreCapacityThanTheQuotaAllows(t *testing.T) {
	m := Measurement{
		Cores: 64, CPUCapacity: 2, CPUCapacitySource: "cgroup",
		Probe: ProbeResult{OK: true, EffectiveCores: 40, Workers: 64},
	}
	if got := m.EffectiveCapacity(); got > 2 {
		t.Errorf("effective capacity %.2f inside a 2-core quota", got)
	}
}

// TestTheProbeIsSkippedOnALoadedMachineRatherThanReportingContention.
//
// The probe's ratio is depressed by whatever else is running, and past the point of
// oversubscription it is measuring the other workload rather than this machine. Skipping is
// reported as a skip and not as a failure, because a busy machine is not a broken one and the
// two have different fixes.
func TestTheProbeIsSkippedOnALoadedMachineRatherThanReportingContention(t *testing.T) {
	r := Probe(context.Background(), 8, ProbeSkipAbove+1, Options{})
	if r.OK {
		t.Fatal("the probe ran on a machine above ProbeSkipAbove")
	}
	if !r.Skipped {
		t.Errorf("a loaded machine reported a probe FAILURE (%q); it is a skip, and a busy machine is not a broken one", r.Reason)
	}
	if r.Reason == "" {
		t.Error("skipped with no reason, so the provenance note cannot say why")
	}
}

// TestAProbeThatCouldNotRunLeavesTheCeilingWhereItWas.
//
// The fallback has to be exactly today's behaviour, or a skipped probe silently becomes a
// different governor rather than an uncalibrated one.
func TestAProbeThatCouldNotRunLeavesTheCeilingWhereItWas(t *testing.T) {
	skipped := Measurement{Cores: 8, CPUCapacity: 8, Probe: ProbeResult{Skipped: true, Reason: "busy"}}
	if got, want := Derive(skipped).BMax, budget.MaxSlots(8); got != want {
		t.Errorf("b_max %d after a skipped probe, want the uncalibrated %d", got, want)
	}
}

// TestProvenanceNamesTheReadingsAndNotJustTheTool.
//
// "A number nobody can trace back to a measurement gets treated as arbitrary and edited
// randomly six months later" is the story's own trap. The guard is that the note carries the
// READINGS — a line saying only that `governor calibrate` wrote this tells a reader nothing
// they could act on, and would pass a test that merely checked the field was non-empty.
func TestProvenanceNamesTheReadingsAndNotJustTheTool(t *testing.T) {
	m := Measurement{
		At:    time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		Cores: 8, PerformanceCores: 4, PerformanceCoresKnown: true,
		MemoryBytes: 16 << 30, CPUCapacity: 8, CPUCapacitySource: "host",
		AmbientRunQueue: 2.91, LoadAvg1: 23.28,
		Probe: ProbeResult{OK: true, EffectiveCores: 6.1, Workers: 8, UsableReps: 5, Reps: 5, Spread: 0.4},
	}
	got := Provenance(m)
	t.Log(got)
	for _, want := range []string{
		"2026-09-16T12:00:00Z", // when
		"8 cores",              // the shape of the machine
		"4 performance",        // the split, where it was measurable
		"2.91",                 // the ambient reading the threshold was placed above
		"6.1",                  // what the probe found
		"headroom",             // how the reading became the number
	} {
		if !strings.Contains(got, want) {
			t.Errorf("provenance does not mention %q, so the number cannot be traced back to it:\n%s", want, got)
		}
	}
}

// TestProvenanceSaysWhenTheSplitWasNotMeasured.
//
// "All eight cores are performance cores" and "this platform does not publish a split" are
// different facts, and reporting the second as the first is what signals.Host did for every
// machine until GOV-25. A provenance note that could not tell them apart would re-introduce
// the same claim one layer up.
func TestProvenanceSaysWhenTheSplitWasNotMeasured(t *testing.T) {
	m := Measurement{At: time.Now(), Cores: 8, PerformanceCores: 8, CPUCapacity: 8}
	if got := Provenance(m); strings.Contains(got, "8 performance") {
		t.Errorf("provenance claims a measured performance-core split on a platform that publishes none:\n%s", got)
	}
}
