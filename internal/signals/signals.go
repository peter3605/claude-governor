// Package signals samples the host state that drives the control loop.
//
// The seam (design.md section 12.1): everything platform-specific lives behind Sampler,
// with a sampler_darwin.go / sampler_linux.go pair. Tier two does not change this — CPU
// does not aggregate across hosts, so there is no remote sampler to write.
package signals

import "context"

// Thermal is the value of ProcessInfo.thermalState on macOS (design.md section 5.3).
type Thermal string

const (
	Nominal  Thermal = "nominal"
	Fair     Thermal = "fair"
	Serious  Thermal = "serious"
	Critical Thermal = "critical"
	// Unknown is what a host with no thermal API reports — every Linux host. It is treated
	// as nominal and MUST NOT be a cut trigger: a machine that cannot measure its own
	// temperature is not a hot machine.
	Unknown Thermal = "unknown"
)

// Sample is one 1 Hz reading of the signals in section 5.1.
type Sample struct {
	// CPU is host utilisation in [0,1].
	CPU float64
	// RunQueue is loadavg1 divided by the core count: contention that CPU% misses.
	RunQueue float64
	// Thermal is the thermal pressure state, or Unknown where the platform has none.
	Thermal Thermal
	// MemoryPressure is a secondary cut trigger in [0,1]. It is derived: the fraction of
	// physical memory that is not available to a new allocation.
	MemoryPressure float64

	// The fields below are the raw readings the derived ones above are computed from.
	// They are recorded rather than discarded because Phase 0's whole purpose is to tune
	// the setpoint against real traces (design.md section 5.2), and a trace that stores
	// only the derived value cannot be re-derived against a different formula later.

	// LoadAvg1 is the raw 1-minute load average. RunQueue is this over Cores.
	LoadAvg1 float64
	// MemAvailableBytes is memory available to a new allocation without swapping.
	MemAvailableBytes uint64
	// MemTotalBytes is physical memory.
	MemTotalBytes uint64
	// Tree is the cost of the claude process tree.
	Tree Tree
	// Cores is the core count the RunQueue divisor came from.
	Cores int
	// CostMS is how long this sample itself took to collect, in milliseconds.
	//
	// It is in the record because GOV-02 requires the sampler be invisible, and the only
	// honest way to hold that claim is to measure it continuously rather than benchmark it
	// once. A trace whose own cost is rising is a trace to stop trusting.
	CostMS float64
}

// Host is the fixed shape of the machine, measured once by `governor calibrate` rather
// than compiled in as a constant.
type Host struct {
	Cores            int
	PerformanceCores int
	MemoryBytes      uint64
}

// Sampler reads the host. Implementations are per-platform and MUST NOT require
// privileges: powermetrics and anything else behind sudo is out of scope, because a
// dependency on elevated privileges kills the install story.
type Sampler interface {
	Sample(ctx context.Context) (Sample, error)
	Host(ctx context.Context) (Host, error)
}

// Tree is the cost of the `claude` process tree (design.md section 5.1, "per-agent cost").
//
// Roots and All are reported separately on purpose. Phase 0 (GOV-02) exists to answer one
// question — does the CPU come from the agents themselves, or from the `npm test` they
// launched — and that distinction is exactly the gap between CPURoots and CPU. Collapsing
// them into a single number would discard the finding this phase is being run to get.
type Tree struct {
	// Roots is the number of live processes whose command name is exactly "claude".
	//
	// The match is exact and case-sensitive, which is load-bearing rather than incidental:
	// on macOS the Claude *desktop app* runs as "Claude" and "Claude Helper", and the
	// browser native host runs from a version-numbered path. Neither is a Claude Code
	// session, and folding either one in would put an Electron UI's CPU into the governor's
	// control signal.
	Roots int
	// Procs is Roots plus every transitive descendant.
	Procs int
	// CPURoots is cores busy across the root processes alone, over the sampling interval.
	// 1.0 means one core saturated.
	CPURoots float64
	// CPU is cores busy across the whole tree, roots and descendants together.
	CPU float64
	// RSSBytes is resident memory summed across the whole tree.
	RSSBytes uint64
}
