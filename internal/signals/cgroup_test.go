package signals

import "testing"

// The cases that matter are textual, which is why this parsing is portable rather than
// tucked behind the linux build tag: every one of them runs on both CI platforms.
func TestParseCgroupV2(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		cores float64
		ok    bool
	}{
		{"two cores", "200000 100000\n", 2, true},
		{"half a core", "50000 100000\n", 0.5, true},
		{"unlimited reads as no limit", "max 100000\n", 0, false},
		{"empty file", "", 0, false},
		{"one field", "200000\n", 0, false},
		{"zero period is not a divisor", "200000 0\n", 0, false},
		{"negative quota", "-1 100000\n", 0, false},
		{"garbage", "hello world\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cores, ok := parseCgroupV2(tc.body)
			if ok != tc.ok || cores != tc.cores {
				t.Errorf("parseCgroupV2(%q) = %v, %v; want %v, %v", tc.body, cores, ok, tc.cores, tc.ok)
			}
		})
	}
}

func TestParseCgroupV1(t *testing.T) {
	for _, tc := range []struct {
		name          string
		quota, period string
		cores         float64
		ok            bool
	}{
		{"two cores", "200000", "100000", 2, true},
		{"unlimited is -1", "-1", "100000", 0, false},
		{"missing period", "200000", "", 0, false},
		{"zero period", "200000", "0", 0, false},
		{"both missing", "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cores, ok := parseCgroupV1(tc.quota, tc.period)
			if ok != tc.ok || cores != tc.cores {
				t.Errorf("parseCgroupV1(%q,%q) = %v, %v; want %v, %v",
					tc.quota, tc.period, cores, ok, tc.cores, tc.ok)
			}
		})
	}
}

// The clamp is the judgement in this file. A cgroup can be granted more bandwidth than the
// machine has; believing it would report headroom the hardware cannot deliver, which is the
// same error the cgroup check exists to prevent.
func TestCapacityFrom(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cgroup     float64
		hasCgroup  bool
		host       float64
		wantCores  float64
		wantSource string
	}{
		{"no cgroup falls back to the host", 0, false, 8, 8, "host"},
		{"a real limit wins", 2, true, 64, 2, "cgroup"},
		{"a fractional limit survives", 0.5, true, 8, 0.5, "cgroup"},
		{"a quota over the host is clamped", 128, true, 8, 8, "host (cgroup quota exceeds it)"},
		{"a zero limit is not a limit", 0, true, 8, 8, "host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cores, source := capacityFrom(tc.cgroup, tc.hasCgroup, tc.host)
			if cores != tc.wantCores || source != tc.wantSource {
				t.Errorf("capacityFrom(%v,%v,%v) = %v, %q; want %v, %q",
					tc.cgroup, tc.hasCgroup, tc.host, cores, source, tc.wantCores, tc.wantSource)
			}
		})
	}
}

// Whatever platform this runs on, the capacity must be a usable positive number and must
// name its source. A zero here would become a division by zero in RunQueue.
func TestCPUCapacityIsUsableOnThisPlatform(t *testing.T) {
	cores, source := cpuCapacity()
	if cores <= 0 {
		t.Errorf("cpuCapacity() = %v cores, want a positive number", cores)
	}
	if source == "" {
		t.Error("cpuCapacity() returned no source; a trace cannot tell a small machine from a throttled one")
	}
	t.Logf("this host: %v cores, source %q", cores, source)
}
