package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BUILD IDENTITY (GOV-43).
//
// A version string does not identify a build. Every pre-release build calls itself
// 0.0.0-dev, and a release can be followed by any number of local builds that still say
// v0.1.1, so two different binaries routinely report the same version and the same protocol.
// `binary_version` therefore cannot answer the one question that matters after an upgrade —
// is the daemon holding the socket the binary that is installed now? — and on 2026-10-31 it
// did not: a daemon started on 2026-09-20 served every session for 40 days while v0.1.1 sat
// on disk, and nothing on the machine noticed.
//
// The identity is the sha256 of the executable's bytes, the same digest `governor install`
// already uses to decide whether it has anything to copy (sameContent). Using that one
// predicate everywhere means the three readers cannot disagree: if install would replace the
// binary, the daemon it started from the old one reports a different build, and doctor and
// the status line say so.

// buildUnknown is what a daemon reports when it could not read its own executable.
//
// It is a value rather than an empty field on purpose, because the empty field already
// means something: a daemon older than this field, which is certainly not the current build.
// "Could not tell" and "is definitely stale" must not render the same, and every reader treats
// this one as no evidence either way.
const buildUnknown = "unknown"

var (
	buildOnce sync.Once
	buildID   string
)

// thisBuild is the digest of the running executable, taken once per process.
//
// THE DAEMON MUST CALL THIS AT STARTUP, before anything can replace the file. Section 13.4
// installs a new binary by rename(2), which leaves the running process on the old inode while
// the PATH now names the new one — so a daemon that hashed os.Executable() lazily, on its first
// status request after an upgrade, would hash the NEW file and report itself current. That is
// precisely the lie this field exists to end. runDaemon takes it before binding the socket.
func thisBuild() string {
	buildOnce.Do(func() {
		buildID = buildUnknown
		exe, err := os.Executable()
		if err != nil {
			return
		}
		if d, err := digest(exe); err == nil {
			buildID = d
		}
	})
	return buildID
}

// shortBuild abbreviates a build digest for a human, as git abbreviates a commit.
func shortBuild(b string) string {
	if len(b) > 12 {
		return b[:12]
	}
	return b
}

// selfBuild is the statusline's record of its own executable's digest, carried in its cache.
//
// The statusline runs on every assistant message in every session under a 50 ms budget
// (statuslineBudget), so hashing a 6 MB binary on each cache miss is a cost it should not pay
// when nothing has changed. The digest is reused while the file's path, size and modification
// time are unchanged — the same three facts that would change on any install, since install
// writes a new file and renames it into place.
type selfBuild struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Digest  string    `json:"digest"`
}

// currentSelfBuild returns this executable's digest, reusing prev when it still describes
// the same file. A zero selfBuild means it could not be determined.
func currentSelfBuild(prev *selfBuild) selfBuild {
	exe, err := os.Executable()
	if err != nil {
		return selfBuild{}
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return selfBuild{}
	}
	same := func(b *selfBuild) bool {
		return b != nil && b.Digest != "" && b.Path == exe && b.Size == fi.Size() && b.ModTime.Equal(fi.ModTime())
	}
	if same(prev) {
		return *prev
	}
	selfMemoMu.Lock()
	defer selfMemoMu.Unlock()
	if same(selfMemo) {
		return *selfMemo
	}
	d, err := digest(exe)
	if err != nil {
		return selfBuild{}
	}
	selfMemo = &selfBuild{Path: exe, Size: fi.Size(), ModTime: fi.ModTime(), Digest: d}
	return *selfMemo
}

// selfMemo is the last digest this process computed for itself. A shipped statusline is one
// short process and computes it at most once anyway; the memo is for a long-lived caller —
// the test binary, which is several times the shipped binary's size and renders hundreds of
// lines under the same 50 ms budget. TestMain warms it.
var (
	selfMemoMu sync.Mutex
	selfMemo   *selfBuild
)

// daemonBuildVerdict compares the build a daemon reports with the build of the binary
// asking. It returns stale=true only on positive evidence: an absent field (a daemon older
// than GOV-43, so older than any binary that can ask) or a digest that differs. A daemon
// that could not read itself, or a reader that could not read itself, is no evidence.
func daemonBuildVerdict(daemonBuild, mine string) (stale bool) {
	if mine == "" || mine == buildUnknown || daemonBuild == buildUnknown {
		return false
	}
	return daemonBuild != mine
}
