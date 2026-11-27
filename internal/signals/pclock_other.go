//go:build !darwin || !cgo

package signals

// newPClockSource reports the P-cluster clock unavailable on every host without IOReport: all
// of Linux, and a macOS binary built with CGO_ENABLED=0, which `make cross` does. Unknown is
// nominal, exactly as thermal's Unknown is: a machine that cannot read its clock is not a
// throttled machine.
func newPClockSource() (pclockSource, error) {
	return nil, pclockFailure("no IOReport on this platform")
}
