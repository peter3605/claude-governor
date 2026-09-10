//go:build darwin

package signals

import "runtime"

// cpuCapacity reports the cores available to the governor on macOS, which is always the
// host's own count.
//
// There is no cgroup equivalent here and no container runtime to hide behind: a macOS host
// runs Claude Code directly. The function exists as the darwin half of the pair so that the
// caller never has to ask which platform it is on — the same shape ReadThermal already uses
// for the one other genuinely platform-specific reading.
//
// PerformanceCores is deliberately not consulted. Apple silicon's efficiency cores are real
// cores that the scheduler will hand work to, so counting only the performance ones would
// under-report capacity; splitting them properly is `governor calibrate`'s job (GOV-25),
// measured on the machine rather than guessed here.
func cpuCapacity() (float64, string) {
	return float64(runtime.NumCPU()), "host"
}
