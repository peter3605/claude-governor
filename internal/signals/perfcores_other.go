//go:build !darwin

package signals

// performanceCores reports that this platform does not distinguish core types.
//
// Linux DOES run on heterogeneous parts — arm64 big.LITTLE is the same idea as Apple's P and
// E cores — but it publishes no equivalent of `hw.perflevel0.name`. The nearest thing is
// per-CPU `cpufreq` maximum frequencies under sysfs, which is absent on most server kernels,
// absent inside most containers, and reports a clock rather than a class. Deriving a core
// split from it would be exactly the "measure, do not assume" failure the corpus work refused:
// a plausible rule producing a number nobody measured.
//
// So the answer is "not distinguishable", which `calibrate` reports as such rather than
// rendering as a measurement. Thermal takes the same shape on this platform and for the same
// reason — Unknown is a reading, not a zero.
func performanceCores() (int, bool) { return 0, false }
