//go:build !darwin || !cgo

package main

import (
	"errors"
	"time"
)

// The probe answers a question about Apple silicon's P- and E-clusters, so it exists only
// there. Off darwin, or in a CGO_ENABLED=0 build, it compiles and refuses to run, so
// `go build ./...` and `make cross` stay green on every target.
const supported = false

var errUnsupported = errors.New("clockprobe needs darwin with cgo")

func timePair(uint64, uint64) (int64, int64, error) { return 0, 0, errUnsupported }
func spin(time.Duration)                            {}
func setQoS(bool) error                             { return errUnsupported }
