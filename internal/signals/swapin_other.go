//go:build !darwin && !linux

package signals

import "context"

// swapInBytes reports that swap-in cannot be read on a platform that is neither macOS nor
// Linux. Nothing ships through this file (see sampler_other.go); it reports false so that
// such a build fails open on memory rather than inventing a zero.
func swapInBytes(_ context.Context) (uint64, bool) { return 0, false }
