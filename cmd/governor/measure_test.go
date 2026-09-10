package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// The grant round-trip measurement (GOV-07, design.md section 4.4: p99 < 15 ms).
//
// This is a measurement, not an assertion, and it is opt-in for a reason this repo has
// already paid for once. GOV-B04 existed because a test asserting a wall-clock figure was
// measuring the CI runner's spare capacity rather than the code, and it failed under load
// while the code was fine. A latency ceiling checked on every push is that same test.
//
// So the ceiling lives in design.md, backed by numbers recorded from a run of this, and
// what CI enforces is the fail-open ceiling — which is a bound on a timeout the gate sets
// itself, not on how fast the machine happens to be.
//
//	GOVERNOR_MEASURE=1 go test ./cmd/governor/ -run TestMeasure -v
const measureEnv = "GOVERNOR_MEASURE"

// TestMeasureGrantRoundTrip reports the latency distribution of a granted acquire.
//
// Two figures, because they answer different questions and conflating them is how a budget
// gets misread. The round-trip is dial-write-read against a live daemon: it is what
// design.md section 4.4 bounds at p99 < 15 ms, and it is the part the gate controls. The
// end-to-end figure additionally carries Go process start, which GOV-05 measured at ~12 ms
// and which no amount of work in this repo can reduce.
func TestMeasureGrantRoundTrip(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skipf("set %s=1 to run the latency measurement", measureEnv)
	}

	const (
		budget = 512
		runs   = 300
	)
	endpoint, d := startTestDaemon(t, budget)
	t.Setenv(transport.EndpointEnv, endpoint)

	// In-process: the gate's own body, minus the process the OS has to start.
	inProc := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		if d.table.InUse() >= budget-2 {
			d.table.ReleaseSession("measure")
		}
		payload := preToolUsePayload("measure", fmt.Sprintf("toolu_%04d", i))

		var stdout, stderr bytes.Buffer
		start := time.Now()
		code := gate(strings.NewReader(payload), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
		elapsed := time.Since(start)

		if code != 0 || stdout.Len() != 0 {
			t.Fatalf("run %d: code %d stdout %q — the measurement must run on the grant path",
				i, code, stdout.String())
		}
		inProc = append(inProc, elapsed)
	}
	report(t, "grant round-trip, in process", inProc)

	// End to end, through the real binary, exactly as a hook invokes it.
	bin := buildGovernor(t)
	endToEnd := make([]time.Duration, 0, 50)
	for i := 0; i < 50; i++ {
		if d.table.InUse() >= budget-2 {
			d.table.ReleaseSession("measure")
		}
		cmd := exec.Command(bin, "gate")
		cmd.Env = append(os.Environ(), transport.EndpointEnv+"="+endpoint)
		cmd.Stdin = strings.NewReader(preToolUsePayload("measure", fmt.Sprintf("toolu_e%04d", i)))

		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		start := time.Now()
		err := cmd.Run()
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("run %d: stdout %q, want empty on the grant path", i, stdout.String())
		}
		endToEnd = append(endToEnd, elapsed)
	}
	report(t, "grant end to end, real binary (includes ~12 ms process start)", endToEnd)
}

func report(t *testing.T, label string, d []time.Duration) {
	t.Helper()
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	t.Logf("%s — n=%d p50 %v p95 %v p99 %v max %v",
		label, len(d), pct(d, 50).Round(10*time.Microsecond), pct(d, 95).Round(10*time.Microsecond),
		pct(d, 99).Round(10*time.Microsecond), d[len(d)-1].Round(10*time.Microsecond))
}

// pct returns the p-th percentile of a sorted slice, by nearest rank.
func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (p*len(sorted) + 99) / 100
	if i < 1 {
		i = 1
	}
	if i > len(sorted) {
		i = len(sorted)
	}
	return sorted[i-1]
}

// buildGovernor builds the real binary, so the end-to-end figure includes everything a hook
// pays for rather than everything except the parts that are hard to measure.
func buildGovernor(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "governor")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}
