//go:build !darwin || !cgo

package signals

// readThermal reports Unknown on every host without ProcessInfo.thermalState: all of
// Linux (design.md section 13.5 — there is no equivalent), and a macOS binary built with
// CGO_ENABLED=0, which `make cross` does.
//
// The build constraint is what keeps the "degrades rather than fails to build" promise:
// the cgo shim is not merely unused off darwin, it is not compiled, so no platform needs
// a C toolchain to build the governor.
func readThermal() Thermal {
	return Unknown
}
