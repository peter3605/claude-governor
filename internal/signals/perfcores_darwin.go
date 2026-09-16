//go:build darwin

package signals

import "golang.org/x/sys/unix"

// performanceCores reports how many of this machine's cores are the fast ones, and whether
// that split could be measured at all.
//
// MEASURED FROM THE MACHINE, NOT INFERRED FROM THE ARCHITECTURE. Apple silicon reports its
// core types through `hw.perflevelN.*`, which is readable by any process with no entitlement
// and no `sudo` — which is what makes it usable here at all, given the project rule that a
// dependency on elevated privileges kills the install story. Measured on an M-series host:
//
//	hw.perflevel0.name: Performance   hw.perflevel0.logicalcpu: 4
//	hw.perflevel1.name: Efficiency    hw.perflevel1.logicalcpu: 4
//
// against `runtime.NumCPU()` of 8. The two numbers disagree by a factor of two, and which one
// is right depends entirely on the question: eight cores will all run work, and four of them
// run it several times faster.
//
// THE NAME IS CHECKED RATHER THAN THE INDEX ASSUMED. Level 0 is the fastest level on every
// Apple silicon part shipped so far, but "level 0 is the fast one" is a convention this code
// would be silently wrong about if it ever changed, and the kernel publishes the name right
// beside the count. Reading the name costs one more sysctl and turns an assumption into a
// measurement. Anything that does not name itself "Performance" is treated as not
// distinguishable rather than guessed at.
//
// AN INTEL MAC HAS NO PERFLEVELS AT ALL, and that is the ok bool's main job. The sysctl is
// absent, the read errors, and the honest answer is "this machine does not make the
// distinction" — which is a different fact from "all of its cores are performance cores",
// even though the two produce the same core count. signals.Host keeps them apart so that
// `calibrate` can report which one it found.
func performanceCores() (int, bool) {
	name, err := unix.Sysctl("hw.perflevel0.name")
	if err != nil || name != "Performance" {
		return 0, false
	}
	n, err := unix.SysctlUint32("hw.perflevel0.logicalcpu")
	if err != nil || n == 0 {
		return 0, false
	}
	return int(n), true
}
