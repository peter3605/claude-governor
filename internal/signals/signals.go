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
	// MemoryPressure is a secondary cut trigger in [0,1].
	MemoryPressure float64
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
