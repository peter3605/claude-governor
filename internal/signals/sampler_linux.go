//go:build linux

package signals

import "runtime"

// cpuCapacity reports how many cores this host will actually let the governor use, and
// where that number came from.
//
// The cgroup number is preferred over the host's core count wherever there is one, which is
// GOV-12's requirement and not a detail. Inside a container, raw host CPU is not merely
// imprecise, it is actively misleading in the direction that breaks the control loop: a
// cgroup quota of two cores on a 64-core host reads as a machine that is 97% idle, so the
// loop keeps increasing the budget while the container is already throttled flat. The run
// queue divisor has the same problem — loadavg is the host's, but the cores available to
// divide it by are the cgroup's.
//
// LIMITATION, STATED RATHER THAN HIDDEN: the well-known paths in cgroup.go are read directly
// rather than resolved through /proc/self/cgroup and the mount table. That covers the
// ordinary container case — Docker, Kubernetes and systemd all surface the process's own
// limits at those paths — and returns "host" for the nested or non-standard mounts it does
// not. The failure mode is falling back to the host figure, which is what a non-cgroup
// machine reports anyway, so an unrecognised layout degrades to today's behaviour rather
// than to a wrong number.
func cpuCapacity() (float64, string) {
	cores, ok := cgroupCores()
	return capacityFrom(cores, ok, float64(runtime.NumCPU()))
}
