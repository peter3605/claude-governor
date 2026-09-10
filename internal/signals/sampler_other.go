//go:build !darwin && !linux

package signals

import "runtime"

// cpuCapacity reports the host's core count on any platform that is neither macOS nor Linux.
//
// `make cross` builds only darwin and linux (the four targets in the Makefile), so nothing
// ships through this file today. It exists for the same reason thermal_other.go does: the
// package must build wherever someone points a Go toolchain at it, and a missing platform
// should degrade to the honest host figure rather than fail to compile.
func cpuCapacity() (float64, string) {
	return float64(runtime.NumCPU()), "host"
}
