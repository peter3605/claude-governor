package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/transport"
)

// TestMain points the package's tests at a state directory of their own, before any of them
// runs.
//
// WHY THIS EXISTS. Anything in this package that resolves the endpoint or the state
// directory without being told falls back to the developer's real ~/.claude/governor
// (transport.DefaultEndpoint). A test that forgets to set GOVERNOR_ENDPOINT therefore does
// not fail — it quietly measures, and sometimes WRITES TO, the machine it is running on.
//
// That is not hypothetical. The first version of
// TestGateStartsADaemonOnlyWhenNothingIsListening forgot exactly this: it wrote a real
// cooldown marker into ~/.claude/governor on the developer's machine, and failed on both CI
// runners where that directory does not exist. GOV-10 fixed the same class of accident in
// main_test.go, and GOV-06 hit it as a test daemon holding the real socket.
//
// WHAT THIS DOES AND DOES NOT BUY, stated precisely because the difference was measured
// rather than assumed. It bounds the DAMAGE: a test that forgets writes into a temp directory
// instead of the developer's home, and behaves the same way on CI as it does locally. It does
// NOT by itself make a forgotten t.Setenv FAIL — with only this in place, reverting that line
// still passed, because two cases sharing this directory also share the lazy-start cooldown
// and the second is rate-limited into looking correct. What catches it is the test asserting
// WHY the gate did what it did rather than only how many times: a case that must reach a
// wedged daemon checks the diagnostics for the read failure, which a run against the wrong
// endpoint cannot produce. The two together are what make that mistake loud.
//
// A test that wants its own endpoint still sets one with t.Setenv, which overrides this and
// is restored afterwards. What this removes is the silent reach into a real home directory,
// not the ability to choose.
//
// The two tests that manipulate HOME wholesale — install and uninstall — are unaffected:
// they set both variables explicitly, and this only supplies a default for tests that set
// neither.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gov-pkg")
	if err != nil {
		panic("isolate the package's state directory: " + err.Error())
	}
	// Not t.TempDir()-shaped: a unix socket path is capped at 104 bytes on darwin, and a
	// path carrying a test name blows that (see sockDir).
	os.Setenv(transport.StateDirEnv, dir)
	os.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(dir, "sock"))
	registerStateDir(dir)

	// The stray-daemon scan reads the whole process table, which is the one thing in doctor
	// that no test can stage (GOV-B11). Left live, every test asserting "all passing" fails on
	// any machine with a governor daemon running on it — including the machine this story was
	// filed from. strays_test.go puts the real scan back for the tests that are about it.
	scanForStrays = noStrays

	// Hash this test binary once, up front (GOV-43). The statusline digests its own executable
	// on a cache miss, and the test binary is large enough that doing it first inside some
	// test's 50 ms statusline budget would make that test's outcome depend on the disk.
	currentSelfBuild(nil)
	thisBuild()

	code := m.Run()

	// Reap before removing, and in that order (GOV-B11). A daemon still running here holds a
	// socket inside dir; removing the tree first unlinks that socket without stopping the
	// process, which is how a governor ends up resident for 28 days answering on a path that
	// no longer exists. Reaping second would leave nothing to find the pid file in.
	if killed := reapStrayDaemons(); len(killed) > 0 {
		fmt.Fprintf(os.Stderr, "\nTestMain reaped %d daemon(s) the tests left running:\n  %s\n"+
			"That is a reaping bug in whichever test started them, not a clean run — the\n"+
			"backstop exists to stop the machine accumulating them, not to excuse them.\n",
			len(killed), strings.Join(killed, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	os.RemoveAll(dir)
	os.Exit(code)
}
