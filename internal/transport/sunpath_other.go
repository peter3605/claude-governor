//go:build !darwin

package transport

// MaxSocketPath is the longest usable unix socket path on this platform. See the darwin
// file for why the limit is named rather than left to produce EINVAL.
const MaxSocketPath = 107
