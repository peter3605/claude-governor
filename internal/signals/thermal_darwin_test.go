//go:build darwin && cgo

package signals

import "testing"

// On macOS with cgo the shim must actually reach ProcessInfo.thermalState. Unknown here
// means the shim is failing silently and the governor is flying blind on the one signal
// that matters most on a fanless chassis — which would otherwise look exactly like a
// healthy nominal machine.
func TestShimReadsRealThermalStateOnDarwin(t *testing.T) {
	if got := ReadThermal(); got == Unknown {
		t.Fatalf("ReadThermal() = %q on darwin/cgo, want a real state from ProcessInfo.thermalState", got)
	}
}
