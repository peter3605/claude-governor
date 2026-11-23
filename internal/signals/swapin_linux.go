//go:build linux

package signals

import (
	"context"

	"github.com/shirou/gopsutil/v4/mem"
)

// swapInBytes reads the cumulative bytes faulted back in from swap since boot, and whether
// the platform let it be read at all.
//
// On Linux gopsutil already answers it: Sin is /proc/vmstat's pswpin, in pages, which gopsutil
// scales by 4 KiB. That scale is gopsutil's assumption rather than a read of the page size,
// and it is right on every architecture the release ships for (amd64 and arm64 both default
// to 4 KiB pages; a 64 KiB-page arm64 kernel would read 16x low). It is kept rather than
// re-derived because the threshold this feeds is measured in multiples of the baseline, not
// in absolute bytes, and a constant factor moves both sides together.
//
// A host without swap, or a container that hides /proc/vmstat, reads an error or a counter
// that never moves. The first is false and does not cut; the second is a true zero, which is
// the right answer for a machine with nothing to swap in.
func swapInBytes(ctx context.Context) (uint64, bool) {
	sw, err := mem.SwapMemoryWithContext(ctx)
	if err != nil || sw == nil {
		return 0, false
	}
	return sw.Sin, true
}
