package signals

import (
	"os"
	"strconv"
	"strings"
)

// The cgroup CPU bandwidth files. v2 puts both numbers in one file as "$QUOTA $PERIOD",
// with the literal "max" for an unlimited cgroup; v1 splits them, and uses -1 for unlimited.
const (
	cgroupV2Max    = "/sys/fs/cgroup/cpu.max"
	cgroupV1Quota  = "/sys/fs/cgroup/cpu/cpu.cfs_quota_us"
	cgroupV1Period = "/sys/fs/cgroup/cpu/cpu.cfs_period_us"
)

// This parsing lives in a file with no build tag, and is called only from sampler_linux.go,
// on purpose. cgroups exist only on Linux, but a parser that can only be compiled on Linux
// can only be TESTED on Linux — and the interesting cases here are all textual: "max", a
// negative quota, a zero period, a quota larger than the machine. Keeping the logic portable
// means both CI platforms exercise it and a darwin laptop can debug it, while the paths that
// genuinely only exist on Linux stay behind the build tag where they belong.

// cgroupCores returns the CPU bandwidth limit in cores, and whether one is set at all.
func cgroupCores() (float64, bool) {
	if cores, ok := parseCgroupV2(readFileString(cgroupV2Max)); ok {
		return cores, true
	}
	return parseCgroupV1(readFileString(cgroupV1Quota), readFileString(cgroupV1Period))
}

// readFileString returns a file's contents, or "" if it cannot be read. An unreadable
// bandwidth file and an absent one mean the same thing here — no limit to be found — so
// they are not distinguished.
func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseCgroupV2 reads a cpu.max body: "$QUOTA $PERIOD", or "max $PERIOD" when unlimited.
func parseCgroupV2(body string) (float64, bool) {
	fields := strings.Fields(body)
	if len(fields) != 2 || fields[0] == "max" {
		// "max" is an unlimited cgroup. That is the absence of a limit, and treating it as
		// one would clamp an unconstrained host to whatever the period happened to be.
		return 0, false
	}
	quota, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || quota <= 0 {
		return 0, false
	}
	period, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || period <= 0 {
		return 0, false
	}
	return quota / period, true
}

// parseCgroupV1 reads the two cfs files. A quota of -1 is v1's unlimited.
func parseCgroupV1(quotaBody, periodBody string) (float64, bool) {
	quota, err := strconv.ParseFloat(strings.TrimSpace(quotaBody), 64)
	if err != nil || quota <= 0 {
		return 0, false
	}
	period, err := strconv.ParseFloat(strings.TrimSpace(periodBody), 64)
	if err != nil || period <= 0 {
		return 0, false
	}
	return quota / period, true
}

// capacityFrom folds a cgroup reading together with the host's core count.
//
// Separated from the file reading so the clamp is testable without a filesystem. The clamp
// is the part with a judgement in it: a cgroup may be granted more bandwidth than the machine
// physically has, and reporting capacity the hardware cannot deliver is the same false
// headroom the cgroup check exists to prevent, just in the other direction.
func capacityFrom(cgroupCores float64, hasCgroup bool, hostCores float64) (float64, string) {
	if !hasCgroup || cgroupCores <= 0 {
		return hostCores, "host"
	}
	if cgroupCores > hostCores {
		return hostCores, "host (cgroup quota exceeds it)"
	}
	return cgroupCores, "cgroup"
}
