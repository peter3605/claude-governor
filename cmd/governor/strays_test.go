package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// GOV-B11 clause 2: doctor can see a second daemon.
//
// A REAL daemon on a real scratch socket, not a fabricated process list. The bug being fixed
// is that four real daemons were invisible to a real doctor run, and every part of that
// invisibility lived in the gap between the process table and what doctor looked at — which
// is exactly the part a fake would skip.
//
// EVERY ASSERTION HERE IS ABOUT THE ONE PID THIS TEST STARTED, never about the size or
// emptiness of the list. That is not fastidiousness: the first version of this test asserted
// "no strays" after the kill and failed on the developer's own machine, because the machine
// legitimately had three — the installed daemon and the two orphans that prompted the story.
// A check about the whole host cannot be tested by asserting the host is clean; it can only
// be tested by asserting what it says about a process the test controls. The same mistake in
// the other direction is what TestMain exists to stop (see main_isolation_test.go).
func TestDoctorNamesAForeignDaemonAndGoesQuietWhenItStops(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts a real daemon")
	}
	withRealStrayScan(t)
	state := sockDir(t)
	pid := startScratchDaemon(t, state)

	// A one-nanosecond threshold, because the daemon under test is seconds old and the shipped
	// threshold is ten minutes. The threshold itself is TestAYoungDaemonIsNotYetAStray's
	// subject; what this test is about is whether a daemon that HAS passed it is described
	// well enough to act on.
	d := &diagnosis{strayMinAge: time.Nanosecond}
	d.run()

	f := findingNamed(t, d, "strays")
	if f.state != warn {
		t.Fatalf("strays check reported %q while a foreign daemon was running, want warn: a "+
			"second daemon must not be invisible behind a clean bill of health (detail: %s)",
			f.state.label(), f.detail)
	}

	// pid, path, age and socket — all four, because the story asks for all four and a report
	// missing any of them cannot be acted on without going process-hunting by hand.
	line := strayLineFor(t, f.detail, pid)
	for _, want := range []string{realGovernor(t), "age ", filepath.Join(state, "sock")} {
		if !strings.Contains(line, want) {
			t.Errorf("the line for pid %d does not name %q:\n  %s", pid, want, line)
		}
	}
	if !strings.Contains(f.fix, strconv.Itoa(pid)) {
		t.Errorf("strays fix %q does not name pid %d as one to kill", f.fix, pid)
	}

	// And it goes quiet for that daemon once it stops. Without this half the test would pass
	// against a check that reported everything it ever saw and never forgot anything.
	if !killDaemon(pid) {
		t.Fatalf("could not stop the scratch daemon %d", pid)
	}
	after := &diagnosis{strayMinAge: time.Nanosecond}
	after.run()
	if strayNamesPID(findingNamed(t, after, "strays").detail, pid) {
		t.Errorf("strays still names pid %d after it was killed:\n%s",
			pid, findingNamed(t, after, "strays").detail)
	}
}

// A daemon younger than the threshold is somebody's working daemon, and is not reported.
//
// This is the trap the story names explicitly: widened to "any process called governor", the
// check fires on a developer's own `go run ./cmd/governor` during ordinary work on this repo,
// and a check that fires during normal work is a check that gets silenced. The age filter is
// what prevents that, so it is asserted rather than assumed.
func TestAYoungDaemonIsNotYetAStray(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts a real daemon")
	}
	withRealStrayScan(t)
	state := sockDir(t)
	pid := startScratchDaemon(t, state)
	t.Cleanup(func() { killDaemon(pid) })

	d := &diagnosis{} // zero means the shipped strayMinimumAge
	d.run()

	if strayNamesPID(findingNamed(t, d, "strays").detail, pid) {
		t.Errorf("a daemon seconds old was reported as a stray, though the threshold is %v:\n%s",
			strayMinimumAge, findingNamed(t, d, "strays").detail)
	}
}

// A warning must not be rounded off into "all passing".
//
// The reason GOV-B11 exists is not that four daemons were running; it is that the summary
// line said ten checks all passing while they did. This is a unit test on report() rather
// than on a live run, because the claim is about the sentence doctor prints and nothing else
// — running the real checks to reach it would make the assertion depend on the machine.
func TestAWarningIsNotReportedAsAllPassing(t *testing.T) {
	for _, tt := range []struct {
		name     string
		findings []finding
		wantCode int
		wantLine string
	}{
		{
			name:     "a warning alongside passes",
			findings: []finding{{name: "a", state: pass}, {name: "b", state: warn, fix: "kill 1"}},
			wantCode: 0,
			wantLine: "2 checks, 1 passing and 1 to look at.",
		},
		{
			name:     "no warning, and the old sentence is unchanged",
			findings: []finding{{name: "a", state: pass}, {name: "b", state: pass}},
			wantCode: 0,
			wantLine: "2 checks, all passing.",
		},
		{
			name: "a warning does not hide a failure, and a failure does not hide a warning",
			findings: []finding{
				{name: "a", state: fail, fix: "governor install"},
				{name: "b", state: warn, fix: "kill 1"},
			},
			wantCode: 1,
			wantLine: "1 of 2 checks failed, and 1 more to look at.",
		},
		// GOV-30's state, through the same summary machinery. "all passing" must not survive
		// a check that could not verify itself, and the exit code must say so distinctly —
		// see doctorExitUnverified for why it is 3 and not 1.
		{
			name:     "a check that could not verify is not passing",
			findings: []finding{{name: "a", state: pass}, {name: "b", state: unverified, fix: "spawn one"}},
			wantCode: doctorExitUnverified,
			wantLine: "2 checks, 1 passing and 1 that could not be verified.",
		},
		{
			name: "a warning and an unverified check are counted apart",
			findings: []finding{
				{name: "a", state: pass},
				{name: "b", state: warn, fix: "kill 1"},
				{name: "c", state: unverified, fix: "spawn one"},
			},
			wantCode: doctorExitUnverified,
			wantLine: "3 checks, 1 passing, 1 to look at and 1 that could not be verified.",
		},
		{
			// A failure outranks it: the machine is broken, and that is the more urgent of
			// the two things the summary has to say.
			name: "a failure outranks a check that could not verify",
			findings: []finding{
				{name: "a", state: fail, fix: "governor install"},
				{name: "b", state: unverified, fix: "spawn one"},
			},
			wantCode: 1,
			wantLine: "1 of 2 checks failed, and 1 more that could not be verified.",
		},
		{
			name: "everything at once still reads as a sentence",
			findings: []finding{
				{name: "a", state: fail, fix: "governor install"},
				{name: "b", state: warn, fix: "kill 1"},
				{name: "c", state: unverified, fix: "spawn one"},
			},
			wantCode: 1,
			wantLine: "1 of 3 checks failed, with 1 to look at and 1 that could not be verified.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			code := (&diagnosis{findings: tt.findings}).report(&out)
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d:\n%s", code, tt.wantCode, out.String())
			}
			if !strings.Contains(out.String(), tt.wantLine) {
				t.Errorf("summary does not contain %q:\n%s", tt.wantLine, out.String())
			}
		})
	}
}

// The sampler is not a daemon, and neither is a gate.
//
// Structural rather than special-cased: the argv filter is what excludes them, and that is
// the property worth pinning, because the alternative — a list of pids or names to ignore —
// goes stale silently. Killing the Phase 0 sampler ends the trace stream GOV-03 and GOV-15
// read, so this is a mistake with a real cost.
func TestOnlyDaemonArgvCountsAsAStray(t *testing.T) {
	for _, tt := range []struct {
		argv []string
		want bool
	}{
		{[]string{"/usr/local/bin/governor", "daemon"}, true},
		{[]string{"./governor", "daemon", "--budget", "4"}, true},
		{[]string{"/x/governor", "--verbose", "daemon"}, true},
		// The Phase 0 sampler, which must never be matched.
		{[]string{"/x/governor", "trace", "sample"}, false},
		{[]string{"/x/governor", "gate"}, false},
		{[]string{"/x/governor", "doctor"}, false},
		{[]string{"/x/governor", "install", "--daemon"}, false},
		{[]string{"/x/governor"}, false},
		// Something else entirely that happens to take a `daemon` argument.
		{[]string{"/usr/sbin/sshd", "daemon"}, false},
		{nil, false},
	} {
		if got := isGovernorDaemonArgv(tt.argv); got != tt.want {
			t.Errorf("isGovernorDaemonArgv(%q) = %v, want %v", tt.argv, got, tt.want)
		}
	}
}

// noStrays is the scan TestMain installs for the package: it finds nothing, so doctor's
// report depends only on the installation a test staged.
func noStrays(context.Context, int, string, time.Duration) []strayDaemon { return nil }

// withRealStrayScan puts the real process-table scan back for one test, and takes it away
// again afterwards. Only the tests that are ABOUT the scan may use it.
func withRealStrayScan(t *testing.T) {
	t.Helper()
	prev := scanForStrays
	scanForStrays = findStrayDaemons
	t.Cleanup(func() { scanForStrays = prev })
}

// startScratchDaemon runs the real governor as a daemon on a socket of its own and returns
// its pid. startDetachedDaemon registers the cleanup that reaps it.
func startScratchDaemon(t *testing.T, state string) int {
	t.Helper()
	startDetachedDaemon(t, realGovernor(t), paths{state: state})
	pid, err := readDaemonPID(state)
	if err != nil {
		t.Fatalf("read scratch daemon pid: %v", err)
	}
	return pid
}

// strayNamesPID reports whether a strays detail has a line for a given pid.
func strayNamesPID(detail string, pid int) bool {
	for _, line := range strings.Split(detail, "\n") {
		if strings.Contains(line, "pid "+strconv.Itoa(pid)+" ") {
			return true
		}
	}
	return false
}

// strayLineFor returns the one line of a strays detail that describes a given pid.
func strayLineFor(t *testing.T, detail string, pid int) string {
	t.Helper()
	for _, line := range strings.Split(detail, "\n") {
		if strings.Contains(line, "pid "+strconv.Itoa(pid)+" ") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("strays did not name the daemon this test started (pid %d):\n%s", pid, detail)
	return ""
}

// findingNamed returns the one finding with a given name, failing if there is not exactly one.
func findingNamed(t *testing.T, d *diagnosis, name string) finding {
	t.Helper()
	var out []finding
	for _, f := range d.findings {
		if f.name == name {
			out = append(out, f)
		}
	}
	if len(out) != 1 {
		t.Fatalf("found %d findings named %q, want exactly 1", len(out), name)
	}
	return out[0]
}
