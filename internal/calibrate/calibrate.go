// Package calibrate measures this host and derives the constants the control loop runs on.
//
// It exists because of one project rule: defaults come from the machine, not from a constant
// tuned on one laptop. `governor calibrate` is how a stranger gets numbers that fit their
// machine instead of Peter's, and this package is the part of it that can be reasoned about
// and tested without a host attached — Measure reads the machine, Derive turns a reading into
// constants, and the two are separate so the second can be exercised over hosts nobody owns.
//
// # What is calibrated, and what deliberately is not
//
// THE RUN-QUEUE CUT THRESHOLD, and it is the only control input the loop has. GOV-32 removed
// host utilisation from the loop entirely: it cuts on `RunQueue > RunQueueMax`, climbs on
// `RunQueue <= RunQueueMax - RunQueueBand`, and holds between (docs/climb-condition.md §5).
// So the threshold is the one number whose value decides how this machine behaves.
//
// B_MAX, through the effective capacity the probe measures. `budget.MaxSlots` already derives
// a ceiling from CPUCapacity, which is the right shape; what it cannot see is that a core is
// not a core. See Probe.
//
// NOT THE SETPOINT. It stopped being a control input in GOV-32 and writing a calibrated one
// would produce a number no code reads — the reasoning is recorded at budget.Setpoint.
//
// NOT THE EWMA WINDOW, and this package is where that decision is recorded because GOV-25's
// Done-when asked for it. signals.DefaultCPUWindow smooths CPU. After GOV-32 the loop does not
// control on CPU: `budget.Cutting` and `budget.Climbing` read `Reading.RunQueue` and nothing
// else, and the smoothed CPU the daemon computes survives only as the diagnostic column
// `governor status` prints and tools/sweep reports. Calibrating that window would write a
// number that cannot change what the machine does, which is precisely the objection that
// struck the setpoint from the same clause. The one alternative — smoothing the run queue and
// calibrating THAT window — is worse than leaving it alone: `loadavg1` is already a one-minute
// decaying average, and docs/climb-condition.md §7 already records the loop as slower on a
// rising edge than it used to be. A second filter over an averaged signal buys nothing and
// costs more lag on the one edge that is already the weak one. Owner ruled the clause struck
// on that evidence, the same way GOV-32 struck the setpoint.
package calibrate

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/signals"
)

// BuildHeadroom is how far above a host's ambient run queue the cut threshold is placed,
// in run-queue-per-core.
//
// THIS IS THE CONSTANT, AND THE AMBIENT BASELINE IS THE MEASUREMENT. The split matters and is
// the whole design: what differs between machines is not how much run queue a build produces —
// that is a property of the build and of the core count, both of which the existing arithmetic
// already handles — but what the machine is ALREADY DOING before the governor sees it. A
// developer's working laptop and a quiet CI box run the same compiler and idle three
// run-queue-units apart.
//
// 4.30 IS MEASURED, AND THE FIRST VALUE THIS STORY SHIPPED WAS 1.65 AND WAS WRONG. That number
// was derived by subtracting the Phase 0 trace's settled run queue (p50 0.352) from the
// threshold GOV-31 measured against build load (2.0), which has the appealing property that a
// host idling where Phase 0's host idled calibrates back to exactly 2.0. It is wrong because
// GOV-31's concurrency curve is UNDER-CONVERGED, and docs/run-queue-sweep.md §4 Correction 2
// says so about its own numbers: one build runs 8.1 s against a one-minute decaying average, so
// its 1.22 per core "is mostly the residue of the settle threshold". That curve measures the
// leading edge of a build, not a build.
//
// MEASURED AGAIN UNDER SUSTAINED LOAD — builds relaunched for 180 s, three averaging windows,
// with the cut placed out of reach so the governor admits everything (docs/calibration.md §5):
//
//	builds   rq p50   rq p90   rq max   cpu p50
//	     1     3.04     3.81     4.07      0.84
//	     2     5.83     7.20     7.61      0.92
//	     3    10.06    11.04    11.98      0.99
//
// One sustained build reaches 3.04 at the median and 4.07 at the peak, against the 1.22 the
// under-converged curve recorded. So `ambient + 1.65` puts the cut at 2.30 on a host measured
// idling at 0.65 — BELOW what one build produces — and the budget floors with a single build
// running. That is the exact defect this story exists to remove, reintroduced by its own
// constant, and it was caught by re-running the sweep rather than by reasoning.
//
// THE INTERVAL IS BOUNDED ON BOTH SIDES, which is what makes 4.30 a derivation rather than a
// preference:
//
//	cut >  4.07   the PEAK at one build, so one build does not floor the budget
//	cut <= 5.83   the median at two builds, so a second build still trips the cut
//
// The midpoint of (4.07, 5.83] is 4.95, and 4.95 - 0.65 = 4.30 against the ambient actually
// measured on the host both bounds come from.
//
// THE PEAK IS THE BOUND, NOT THE MEDIAN, AND THAT IS THE SWEEP'S REAL FINDING. AIMD is
// deliberately asymmetric — it halves on a cut and climbs one slot per tick — so a rare
// excursion above the threshold is not a rare event for the budget. Measured: an arm whose
// threshold sat 0.04 BELOW its own peak run queue satisfied the cut condition on only 7% of
// ticks, satisfied the climb condition on 48%, and still ended at the floor. Sorting the four
// sweep arms by "did the threshold clear that arm's own peak" orders all four correctly;
// sorting them by the threshold's value does not (docs/calibration.md §3).
//
// WHAT THIS DELIBERATELY DOES NOT DO: reach the two-build operating point. budget.RunQueueBand's
// derivation assumes the loop settles at two concurrent builds, on the grounds that the old
// curve put this machine's throughput optimum there. Holding two builds would need the cut above
// their 7.61 peak, i.e. a headroom near 8.2 — and the evidence for two builds being the optimum
// comes from the same under-converged curve, while `sweep -recovery` records no completion
// counts, so this story measured no throughput at all. Fixing the measured defect is in scope;
// re-deciding the operating point on throughput data nobody has collected is not.
//
// ONE HOST, ONE WORKLOAD. Both bounds come from `go build -a ./...` over generated 9,000-
// function modules on one 8-core fanless laptop. A lighter build, an I/O-bound suite or a
// machine with a different core count will sit differently, and the honest status of 4.30 is
// "the first value derived from a converged measurement", not "the right value everywhere".
const BuildHeadroom = 4.30

// Derived is the set of constants a calibration produces. Zero means "not calibrated": every
// field is optional in the config, and an absent one leaves the compiled-in default in force
// (design.md §13.4, every key ships with a default that leaves its feature inert).
type Derived struct {
	// RunQueueMax is the run-queue-per-core above which the budget is halved.
	RunQueueMax float64
	// BMax is the ceiling on build concurrency in slots.
	BMax int
}

// Measurement is what Measure read off this host. It is the input to Derive and the body of
// the provenance note, so that a number in config can be traced back to the reading that
// produced it rather than treated as arbitrary six months later.
type Measurement struct {
	// At is when the measurement was taken.
	At time.Time
	// Cores is the machine's logical core count.
	Cores int
	// PerformanceCores is how many of them are the fast ones, where the platform says.
	PerformanceCores int
	// PerformanceCoresKnown distinguishes a measured split from a defaulted one.
	PerformanceCoresKnown bool
	// MemoryBytes is physical memory.
	MemoryBytes uint64
	// CPUCapacity is the cores this process may actually use: a cgroup quota where one is
	// set, the core count otherwise. It is the ceiling on everything below.
	CPUCapacity float64
	// CPUCapacitySource is "host" or "cgroup".
	CPUCapacitySource string
	// AmbientRunQueue is what this host's run queue per core sits at with none of the
	// governor's own work running. See Ambient — it is the reading the threshold is placed
	// above, and the one thing here that genuinely varies between machines.
	AmbientRunQueue float64
	// AmbientOK is whether that reading was actually taken.
	//
	// IT IS NOT REDUNDANT WITH AmbientRunQueue == 0, and conflating the two is a bug this
	// file had until the headroom grew. A host whose load average could not be read reports
	// zero, and so does a genuinely idle one; while the headroom was 1.65 both landed on the
	// shipped constant through the floor and the distinction cost nothing. At 4.30 the floor
	// no longer binds, so an unreadable host would calibrate as though it were idle — which
	// is exactly the "silence is not a quiet machine" failure Ambient's own comment warns
	// about, arriving through the back door. Derive refuses to calibrate a threshold without
	// this bit set.
	AmbientOK bool
	// LoadAvg1 is the raw one-minute load average AmbientRunQueue was computed from, kept
	// so the derived figure can be re-derived against a different divisor later.
	LoadAvg1 float64
	// Probe is what the load probe found, and whether it ran at all.
	Probe ProbeResult
}

// EffectiveCapacity is the cores this host actually delivers under parallel load.
//
// It is the probe's answer where the probe ran and CPUCapacity where it did not, and it is
// never above CPUCapacity: a cgroup quota is a hard ceiling and a throughput measurement that
// exceeded it would be measuring the wrong thing. A host with no probe result falls back to
// exactly today's behaviour, which is the inert default the config rule asks for.
func (m Measurement) EffectiveCapacity() float64 {
	c := m.CPUCapacity
	if c <= 0 {
		c = float64(m.Cores)
	}
	if m.Probe.OK && m.Probe.EffectiveCores > 0 && m.Probe.EffectiveCores < c {
		return m.Probe.EffectiveCores
	}
	return c
}

// Derive turns a measurement into the constants the loop runs on.
//
// PURE, AND THAT IS WHY IT IS SEPARATE FROM Measure. The Done-when requires that two different
// machines produce materially different values, and the only way to test that claim without
// two machines is to be able to hand this function two readings. It also means the derivation
// can be argued about against a recorded Measurement long after the host has changed.
//
// EACH OUTPUT IS DERIVED ONLY FROM ITS OWN MEASUREMENT, OR NOT AT ALL. A zero field means
// "this run did not measure it", and Resolve decides what that means against what is already
// stored. B_max in particular is NOT derived from CPUCapacity when the probe did not produce a
// trusted figure: on 2026-10-31 that fallback wrote b_max 6 against a measured ceiling of 4 on
// this host — the raw core count with no efficiency-core discount, which is the most permissive
// answer available, written as though it had been measured (GOV-47).
func Derive(m Measurement) Derived {
	var d Derived
	if m.Probe.OK {
		d.BMax = budget.MaxSlots(m.EffectiveCapacity())
	}
	if m.AmbientOK {
		d.RunQueueMax = deriveRunQueueMax(m.AmbientRunQueue)
	}
	return d
}

// Prior is what an earlier calibration left in the config.
type Prior struct {
	RunQueueMax float64
	BMax        int
	// At is when that calibration ran, RFC3339, for saying where a kept value came from.
	At string
}

// Outcome is what a calibration writes, and which of it was carried forward rather than
// measured this time.
type Outcome struct {
	Derived
	// BMaxKept and RunQueueMaxKept say the value is the prior one, because this run could not
	// measure it. KeptFrom is when the prior calibration ran.
	BMaxKept        bool
	RunQueueMaxKept bool
	KeptFrom        string
}

// Resolve decides what to write: this run's figure where it produced one, and otherwise the
// figure already stored.
//
// A RUN THAT COULD NOT MEASURE SOMETHING HAS NO STANDING TO ERASE IT. A declined probe and an
// unreadable load average both leave their field zero, and before GOV-47 that zero — or worse,
// a core-count fallback — was written over a value an earlier run had actually measured. The
// two outputs come from different measurements taken under different conditions, so a failure
// in one is no evidence against the other, and each is resolved on its own.
//
// Where nothing is stored either, zero is written and means what it always has: uncalibrated,
// and the daemon runs on its compiled-in derivation (design.md §13.4).
func Resolve(d Derived, prev Prior) Outcome {
	o := Outcome{Derived: d}
	if d.BMax == 0 && prev.BMax > 0 {
		o.BMax, o.BMaxKept = prev.BMax, true
	}
	if d.RunQueueMax == 0 && prev.RunQueueMax > 0 {
		o.RunQueueMax, o.RunQueueMaxKept = prev.RunQueueMax, true
	}
	if o.BMaxKept || o.RunQueueMaxKept {
		o.KeptFrom = prev.At
	}
	return o
}

// deriveRunQueueMax places the cut a fixed headroom above what this host idles at.
//
// IT NEVER GOES BELOW THE SHIPPED CONSTANT. 2.0 is the one value that had been measured
// against real build load before this story, and a calibration that came out tighter than it
// would be trading a measured constant for an extrapolation in the direction that throttles.
//
// THE FLOOR NO LONGER BINDS IN PRACTICE, and saying so is better than leaving a reader to
// wonder. With BuildHeadroom at 4.30 every non-negative ambient reading lands above 2.0, so
// this branch is unreachable on any real host. It is kept as a guard on the constant rather
// than on the reading: if BuildHeadroom is ever lowered, this is what stops the calibration
// silently going tighter than the only independently measured figure.
//
// An unreadable ambient figure does NOT arrive here at all. Derive checks AmbientOK first and
// declines to calibrate a threshold without a baseline, because a host whose load could not be
// read is a host whose threshold must not be inferred from silence.
func deriveRunQueueMax(ambient float64) float64 {
	v := ambient + BuildHeadroom
	if v < budget.RunQueueMax {
		return budget.RunQueueMax
	}
	// Round to two decimals. The input is a one-minute decaying average read off a moving
	// machine and the output is written into a file a person will read; carrying fifteen
	// significant figures of a quantity this noisy would be claiming a precision the
	// instrument does not have.
	return math.Round(v*100) / 100
}

// Measure reads this host. It is the impure half: it touches the machine, and it runs the
// load probe unless opts says not to.
//
// EVERY READING IS OPTIONAL AND A FAILURE IS NOT FATAL, in the shape signals.Sampler already
// uses: a signal that cannot be read leaves its field zero and the rest of the measurement
// still returns. `calibrate` failing outright because one sysctl was unavailable would make an
// install fail on a machine the governor would otherwise run on perfectly well.
func Measure(ctx context.Context, opts Options) (Measurement, error) {
	m := Measurement{At: time.Now()}

	s := signals.NewSampler()
	h, err := s.Host(ctx)
	if err != nil {
		return m, fmt.Errorf("read host: %w", err)
	}
	m.Cores = h.Cores
	m.PerformanceCores = h.PerformanceCores
	m.PerformanceCoresKnown = h.PerformanceCoresKnown
	m.MemoryBytes = h.MemoryBytes

	// One sample carries CPUCapacity and its source, and the load average the ambient figure
	// is computed from. The priming read's zeroes do not matter here: capacity and loadavg
	// are absolute readings, not differences between ticks, so they are right on the first
	// sample. CPU% and the process-tree rates are the fields that need two, and nothing here
	// reads them.
	if sample, err := s.Sample(ctx); err == nil {
		m.CPUCapacity = sample.CPUCapacity
		m.CPUCapacitySource = sample.CPUCapacitySource
	}
	if m.CPUCapacity <= 0 {
		m.CPUCapacity = float64(m.Cores)
		m.CPUCapacitySource = "host"
	}

	m.LoadAvg1, m.AmbientRunQueue, m.AmbientOK = Ambient(ctx, m.CPUCapacity, opts)

	if !opts.NoProbe {
		m.Probe = Probe(ctx, m.Cores, m.AmbientRunQueue, opts)
	}
	return m, nil
}
