//go:build !darwin || !cgo

package main

// IOReport and IOHID exist only on macOS. Off darwin, or in a CGO_ENABLED=0 build, the tool
// compiles and every reading is "unknown", which is what Linux reports for thermal too.
const supported = false

func newOSSource() (source, error) { return nil, failure("needs darwin with cgo") }
