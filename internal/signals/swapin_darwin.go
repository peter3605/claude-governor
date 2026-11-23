//go:build darwin

package signals

import (
	"context"

	"golang.org/x/sys/unix"
)

// swapInBytes reads the cumulative bytes faulted back in from swap since boot, and whether
// the platform let it be read at all.
//
// GOPSUTIL DOES NOT ANSWER THIS ON macOS, AND FOR EVERY RELEASE BEFORE GOV-B53 THE SIGNAL WAS
// ZERO HERE. mem.SwapMemory on darwin fills Total/Used/Free from vm.swapusage and never sets
// Sin, so SwapInBytesPerSec read 0 on every Mac, every tick — on a machine measured at 22 GiB
// of swap in use and peaks above 20,000 pages a second swapped back in (design.md section
// 5.3a). A zero that looks like "no pressure" was the instrument not reading.
//
// vm.compressor.swapper.swapins_total is the kernel's own counter and is what `vm_stat`
// reports as Swapins; the two agree to the page on this machine. It is a sysctl, so no
// privilege, no subprocess and no cgo — the same footing as vm.swapusage, which gopsutil
// already reads. The unit is pages, converted with vm.pagesize (16 KiB on Apple silicon, 4 KiB
// on Intel), which is why the page size is read rather than assumed.
//
// A kernel that lacks either sysctl reports false, and false is not a cut: an unreadable
// signal fails open (Reading.SwapInKnown).
func swapInBytes(_ context.Context) (uint64, bool) {
	pages, err := unix.SysctlUint64("vm.compressor.swapper.swapins_total")
	if err != nil {
		return 0, false
	}
	size, err := unix.SysctlUint32("vm.pagesize")
	if err != nil || size == 0 {
		return 0, false
	}
	return pages * uint64(size), true
}
