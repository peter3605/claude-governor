package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Stray daemon detection for `governor doctor` (GOV-B11).
//
// WHAT THIS IS FOR. Four governor-shaped processes were found running on the dev machine,
// aged three days, three days, twenty days and twenty-eight days, while `governor doctor`
// reported ten checks all passing and `0 in use, 0 waiting`. Each held its own scratch
// socket, so none of them collided with the installed daemon, none double-leased anything,
// and no instrument in the project was looking for them. They were harmless and invisible,
// and invisible is the half worth fixing: the same blind spot that hid four idle daemons
// would hide one that was busy.
//
// WHY IT IS A WARNING. A second daemon on a different socket is untidy, not broken. Failing
// the command on it would make `governor doctor` fail routinely on any machine where somebody
// had ever run the binary by hand, and a diagnostic that cries wolf gets ignored — which
// costs more than the thing it was reporting.
//
// WHAT IT DELIBERATELY DOES NOT MATCH, because the obvious widening is wrong: "any process
// named governor" catches a developer's own `go run ./cmd/governor` in the next window, and
// a check that fires during normal work on this repo is a check that gets silenced. The
// filter is therefore narrow on two axes at once — the process must be a DAEMON (argv), and
// it must have been running long enough that no interactive session is plausibly holding it
// (age). The `trace sample` process the Phase 0 sampler runs falls outside the argv filter
// structurally rather than by being special-cased, which is the more durable way to exclude
// it.

// strayMinimumAge is how long a foreign daemon must have been running before it is reported.
//
// It separates "somebody is working" from "somebody forgot". A developer running a daemon by
// hand to try something is inside this window and is never reported; the orphans that
// prompted this were three to twenty-eight DAYS old, so anything in that class is caught many
// times over. Ten minutes is deliberately nearer the short end: the cost of reporting a
// daemon that is still wanted is one line of output that says a dev daemon is expected, and
// the cost of missing one is however long it runs unnoticed.
const strayMinimumAge = 10 * time.Minute

// strayLsofBudget bounds the one subprocess this check runs per candidate.
//
// Same reasoning as doctorExecBudget: doctor may exec, but it may not hang. lsof on a busy
// machine is occasionally slow, and the socket path is supplementary information — a stray
// is worth reporting with its socket unknown, and not worth waiting on.
const strayLsofBudget = 2 * time.Second

// scanForStrays is the process-table scan checkStrayDaemons uses.
//
// A package-level indirection purely so the package's OTHER tests can be isolated from it.
// Every other check in doctor.go reports on this installation, and the tests stage an
// installation to match; this one reports on the whole machine, which no test can stage. The
// tests that assert a clean report — TestDoctorPassesOnAWorkingInstall and its kind — would
// otherwise fail on any developer's machine that happened to have a governor daemon running,
// which is every machine this story was filed from. TestMain neutralises it for the package
// and the tests in strays_test.go put the real scan back for their own duration.
//
// This is the same reasoning as main_isolation_test.go's, one level out: a test must not
// silently depend on the machine it runs on.
var scanForStrays = findStrayDaemons

// strayDaemon is one governor daemon that is not the one this machine is meant to be running.
type strayDaemon struct {
	pid int
	// path is argv[0], resolved against the process's working directory when it is relative.
	// It is taken from argv rather than from Exe() because Exe() FAILS for exactly the
	// processes that matter most: both scratch daemons on the dev machine had had their
	// binary deleted out from under them, and proc_pidpath then returns nothing. A path that
	// no longer exists on disk is still the most useful thing to print about a process.
	path   string
	age    time.Duration
	socket string // "" when it could not be determined
}

// findStrayDaemons lists long-lived governor daemons other than the one at ownSocket.
//
// minAge is the threshold below which a daemon is assumed to belong to whoever is at the
// keyboard. It is a parameter rather than a constant read in here so that a test can ask the
// same question of a daemon it started a moment ago; nothing in the shipped path passes
// anything but strayMinimumAge.
//
// ownPID and ownSocket describe the daemon doctor is talking to; either may be zero or empty
// when doctor could not establish one, in which case nothing is excluded on that ground and
// the installed daemon may appear in the list. That is the right way round: a doctor that
// cannot tell which daemon is the real one should over-report rather than stay quiet.
func findStrayDaemons(ctx context.Context, ownPID int, ownSocket string, minAge time.Duration) []strayDaemon {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil
	}
	now := time.Now()
	self := os.Getpid()

	var found []strayDaemon
	for _, p := range procs {
		pid := int(p.Pid)
		if pid == self || pid == ownPID {
			continue
		}
		argv, err := p.CmdlineSliceWithContext(ctx)
		if err != nil || !isGovernorDaemonArgv(argv) {
			continue
		}
		ms, err := p.CreateTimeWithContext(ctx)
		if err != nil {
			continue
		}
		age := now.Sub(time.UnixMilli(ms))
		if age < minAge {
			continue // somebody is probably using it
		}

		path := argv[0]
		if !filepath.IsAbs(path) {
			// A daemon started as `./governor` says only "./governor" in its argv, which
			// names nothing a reader could act on. Its working directory is what makes the
			// path meaningful — and is, on the dev machine, the only surviving evidence of
			// where a deleted binary came from.
			if cwd, err := p.CwdWithContext(ctx); err == nil && cwd != "" {
				path = filepath.Join(cwd, path)
			}
		}

		sock := unixSocketOf(ctx, pid)
		if sock != "" && ownSocket != "" && sameSocket(sock, ownSocket) {
			continue // this IS the daemon doctor is talking to, reached by a different path
		}
		found = append(found, strayDaemon{pid: pid, path: path, age: age, socket: sock})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].age > found[j].age })
	return found
}

// isGovernorDaemonArgv reports whether an argv is a governor running as a daemon.
//
// Both halves are required, and the second is what keeps this from matching the Phase 0
// sampler (`governor trace sample`), a gate (`governor gate`), or doctor itself. The
// subcommand is looked for beyond argv[1] as well, so that a daemon started with a global
// flag before the subcommand is still recognised.
func isGovernorDaemonArgv(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "governor" {
		return false
	}
	for _, a := range argv[1:] {
		if a == "daemon" {
			return true
		}
		// Stop at the first thing that is plainly another subcommand, so that
		// `governor install --daemon`-shaped argv cannot be read as a daemon.
		if !strings.HasPrefix(a, "-") && a != "daemon" {
			return false
		}
	}
	return false
}

// unixSocketOf returns a unix socket the process holds, or "" if that cannot be established.
//
// Via lsof, which is the only portable-enough way to get this. gopsutil was tried first and
// cannot do it on darwin: OpenFiles and Environ both return "not implemented yet", and
// Connections returns an empty list for a process holding a unix socket. Since doctor is
// explicitly allowed to exec a bounded subprocess and this is not a hot path, lsof is the
// pragmatic answer.
//
// A missing or slow lsof degrades to "", and the stray is still reported without its socket.
// The socket is the least important of the four facts — a pid, a path and an age already
// identify the process well enough to act on.
func unixSocketOf(ctx context.Context, pid int) string {
	ctx, cancel := context.WithTimeout(ctx, strayLsofBudget)
	defer cancel()

	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", fmt.Sprint(pid), "-U", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		// -Fn emits one field per line, each prefixed by its identifier; "n" is the name.
		// Endpoints of an established connection appear as "->(none)" or a peer address and
		// are not the bound path, so anything that is not an absolute path is skipped.
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

// sameSocket reports whether two socket paths are the same file.
//
// String comparison is not enough: /tmp is a symlink to /private/tmp on darwin, so the same
// socket is named two ways by two processes and a stray would be reported that is in fact
// the installed daemon. Falls back to string equality when neither path can be stat'd, which
// is the case for a socket whose directory has been removed underneath it.
func sameSocket(a, b string) bool {
	if sameFile(a, b) {
		return true
	}
	ra, erra := filepath.EvalSymlinks(a)
	rb, errb := filepath.EvalSymlinks(b)
	if erra == nil && errb == nil {
		return ra == rb
	}
	return a == b
}

// describe renders one stray as the line doctor prints for it.
func (s strayDaemon) describe() string {
	sock := s.socket
	if sock == "" {
		sock = "socket unknown"
	}
	return fmt.Sprintf("pid %d  %s  age %s  %s", s.pid, s.path, roundAge(s.age), sock)
}

// roundAge renders a duration at a granularity a person reads rather than parses.
func roundAge(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Second).String()
	}
}
