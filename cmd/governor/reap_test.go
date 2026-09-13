package main

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Daemon reaping for the package's tests (GOV-B11).
//
// THE SHAPE OF THE LEAK THIS CLOSES. A daemon started under a test's state directory
// outlives the directory: TestMain and sockDir both remove their temp trees when they are
// done, and a unix socket that has been unlinked underneath a listening process does not
// stop it. The result is a `governor daemon` holding a socket nothing can reach, resident
// until the machine is rebooted, costing nothing and colliding with nothing — which is
// precisely why four of them sat on the dev machine for up to 28 days while `governor
// doctor` reported ten checks all passing.
//
// WHY A BACKSTOP AND NOT ONLY t.Cleanup. Most daemons in this package are started by the
// CODE UNDER TEST rather than by the test — the gate's lazy-start path forks one, and
// `install` detaches one — so the test holds no *exec.Cmd to defer against and has to find
// the process through the pid file the daemon writes. That file is written ASYNCHRONOUSLY,
// after the command that triggered it has already returned, so a test that fails in the
// window between the two has nothing to clean up yet and a daemon arrives moments later
// with nobody left to reap it. A t.Cleanup cannot close that race on its own; a sweep that
// runs after every test in the package has finished can.
//
// This is the GOV-B04 pattern applied where GOV-B04 did not reach. That story fixed one
// test in internal/signals by putting the child in its own process group and signalling the
// group; the principle it established — reap on EVERY exit path, and do not trust the happy
// one — is what the rest of this file implements for processes that are not the test's
// children at all.

// stateDirs are the temp directories this package's tests have pointed a daemon at.
//
// Registered rather than discovered. The alternative is scanning the process table for
// anything named `governor`, which cannot tell one of these from the developer's own
// installed daemon or a `go run ./cmd/governor` in another window — and killing either of
// those would make the test suite a hazard rather than a check.
var stateDirs struct {
	sync.Mutex
	dirs []string
}

// registerStateDir records a directory that a daemon may be started under, so that
// reapStrayDaemons can find it after the tests have finished.
func registerStateDir(dir string) {
	stateDirs.Lock()
	defer stateDirs.Unlock()
	stateDirs.dirs = append(stateDirs.dirs, dir)
}

// reapStrayDaemons kills every daemon still running under a registered state directory.
//
// It runs from TestMain after m.Run() returns, which covers every way a test can end that
// leaves the process alive — a pass, a t.Fatal, a t.Skip, a failed assertion — and is the
// last point at which the pid files still exist. It does NOT cover a panic that aborts the
// test binary, because nothing in the process runs after that; that case is caught by
// tools/leakcheck.sh on the next run instead.
//
// It reports what it killed rather than doing it silently. A test that needed this is a
// test with a reaping bug, and a quiet backstop would hide the bug it is compensating for.
func reapStrayDaemons() []string {
	stateDirs.Lock()
	dirs := append([]string(nil), stateDirs.dirs...)
	stateDirs.Unlock()

	var killed []string
	for _, dir := range dirs {
		// Walk rather than reading dir/daemon.pid directly: stagedHome nests its state one
		// level down (sockDir(t)/gov), and install may have staged more than one.
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != daemonPIDFile {
				return nil //nolint:nilerr // an unreadable tree is nothing to reap
			}
			if pid, ok := readPIDFileAt(path); ok && killDaemon(pid) {
				killed = append(killed, strconv.Itoa(pid)+" ("+path+")")
			}
			return nil
		})
	}
	return killed
}

// killDaemon stops a daemon and waits for it to go, escalating if it will not.
//
// SIGTERM first, because that is what uninstall sends and it lets the daemon unlink its
// socket on the way out. SIGKILL after a grace period, because this is a backstop and the
// one outcome it may not produce is a process that is still running when it returns — a
// daemon wedged badly enough to ignore SIGTERM is exactly the daemon most worth reaping.
//
// It reports whether it actually killed something, so a caller can tell "there was nothing
// to do" from "there was, and now there is not".
func killDaemon(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false // already gone, or never ours to signal
	}
	// Never signal the process running the tests, whatever a stale pid file claims. A pid
	// file left behind by an earlier run can name a recycled pid, and on a machine busy
	// enough to recycle one it could name this one.
	if pid == os.Getpid() {
		return false
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if waitGone(pid, 2*time.Second) {
		return true
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	waitGone(pid, 2*time.Second)
	return true
}

// waitGone waits for a pid to leave the process table, and reports whether it did.
func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}
