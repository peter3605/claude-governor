package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// Protocol versioning and the upgrade path (GOV-24, design.md sections 7 and 13.4).
//
// The subject here is the one state every upgrade passes through: a live daemon of one
// version meeting gates of another. design.md 13.4 makes it unavoidable rather than
// exceptional — the new binary arrives by rename(2), which leaves the running daemon on the
// old inode on purpose, so for as long as that process holds the socket the machine is
// running two versions at once.
//
// The only acceptable outcome is ungoverned spawns. A stalled session is not, and it is the
// failure that would actually be reported: an ungoverned spawn looks exactly like no
// governor at all, while a gate that hangs looks like Claude Code is broken.
//
// These tests drive a REAL daemon at one version against a REAL gate at another, in the same
// process. That is what daemon.speaks and gate's speaks parameter exist for. The alternative
// — a hand-written listener echoing a canned rejection line — proves only that the gate can
// parse a refusal somebody wrote by hand; it cannot prove the daemon emits one, and the two
// halves failing to agree is the entire defect class.

// otherVersion is a protocol version this binary does not speak.
const otherVersion = protocolVersion + 1

// TestAGateMeetingANewerDaemonSpawnsUngoverned is the upgrade in progress: the daemon has
// been replaced and this session's hooks are still the old binary.
func TestAGateMeetingANewerDaemonSpawnsUngoverned(t *testing.T) {
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.speaks = otherVersion
	})

	// runGateSpeaking asserts the whole fail-open contract: exit 0, an empty stdout, and
	// back inside the ceiling. Empty stdout IS the ungoverned spawn — the grant path and
	// the fail-open path are deliberately the same bytes (design.md section 4.2).
	diag := runGateSpeaking(t, endpoint, samplePayload, transport.Dial{}, protocolVersion)

	if !strings.Contains(diag, "failing open") {
		t.Errorf("stderr = %q, want it to say the gate failed open", diag)
	}
	// Named for the reason, not merely absent: a gate that failed open because it could not
	// dial would pass the assertion above while testing nothing about versions.
	if !strings.Contains(diag, "protocol") {
		t.Errorf("stderr = %q, want it to name the protocol mismatch as the cause", diag)
	}

	// The refused spawn must not have cost a slot. Asked in the daemon's OWN version,
	// because that is the only version it will answer.
	st := call(t, endpoint, map[string]any{"v": otherVersion, "op": "status"})
	if st.InUse != 0 {
		t.Errorf("%d leases held after a cross-version acquire, want 0", st.InUse)
	}
}

// TestAGateMeetingAnOlderDaemonSpawnsUngoverned is the other half of the same upgrade: the
// binary was replaced, a new session's hooks are the new binary, and the daemon holding the
// socket is still the old one. This is the direction that actually happens on disk, because
// section 13.4 requires the old daemon to survive the rename.
func TestAGateMeetingAnOlderDaemonSpawnsUngoverned(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	diag := runGateSpeaking(t, endpoint, samplePayload, transport.Dial{}, otherVersion)

	if !strings.Contains(diag, "failing open") {
		t.Errorf("stderr = %q, want it to say the gate failed open", diag)
	}
	if !strings.Contains(diag, "protocol") {
		t.Errorf("stderr = %q, want it to name the protocol mismatch as the cause", diag)
	}
	if st := call(t, endpoint, acquireStatusReq()); st.InUse != 0 {
		t.Errorf("%d leases held after a cross-version acquire, want 0", st.InUse)
	}
}

// TestTheDaemonNamesTheVersionsItSpeaks is the daemon's half of the contract: it ANSWERS a
// version it cannot serve rather than ignoring it, and the answer says what it does serve.
//
// Ignoring would be the quiet disaster. A gate with no reply waits out its budget and then
// fails open, so the spawn is right and the latency is not — every spawn on the machine
// would pay the full budget for the life of the mismatched daemon.
func TestTheDaemonNamesTheVersionsItSpeaks(t *testing.T) {
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.speaks = otherVersion
	})

	r := call(t, endpoint, map[string]any{"v": protocolVersion, "op": "acquire", "session": "s"})
	if r.Reason != "unsupported_version" {
		t.Fatalf("reason %q, want %q", r.Reason, "unsupported_version")
	}
	if len(r.Supported) != 1 || r.Supported[0] != otherVersion {
		t.Errorf("supported = %v, want [%d]: the reply must name what the daemon does speak", r.Supported, otherVersion)
	}
	if r.V != otherVersion {
		t.Errorf("reply v = %d, want %d: a daemon answers in its own version", r.V, otherVersion)
	}
}

// TestAVersionMismatchIsNotRetried pins the one wrong fix.
//
// A mismatch is permanent for the life of the daemon, so a gate that retried would turn a
// bounded fail-open into a stall — the exact outcome this story forbids, arrived at by
// something that looks like robustness. One request per gate, and the count is what proves
// it rather than the absence of a loop in the source.
func TestAVersionMismatchIsNotRetried(t *testing.T) {
	var requests int
	endpoint := countingListener(t, &requests, otherVersion)

	runGateSpeaking(t, endpoint, samplePayload, transport.Dial{}, protocolVersion)

	if requests != 1 {
		t.Errorf("the gate sent %d requests, want exactly 1: a version mismatch is permanent, "+
			"so retrying only moves the stall", requests)
	}
}

// TestEveryReplyCarriesTheProtocolVersion sweeps the operations of design.md section 7.
//
// "Every message carries v" is a claim about all of them, and a per-message test proves it
// only for the messages somebody remembered to write one for. This fails when a new
// operation is added without a version on its reply, which is when it is cheap to fix.
func TestEveryReplyCarriesTheProtocolVersion(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	// Ordered so that confirm and release act on a lease acquire has really granted, rather
	// than exercising only their no-op paths.
	granted := call(t, endpoint, acquireReq("vsweep"))
	if granted.Lease == "" {
		t.Fatalf("acquire did not grant a lease: %+v", granted)
	}

	ops := []struct {
		name string
		req  map[string]any
	}{
		{"acquire", acquireReq("vsweep2")},
		{"confirm", map[string]any{"v": protocolVersion, "op": "confirm", "session": "vsweep",
			"lease": granted.Lease, "agent_id": "ag_1", "pid": os.Getpid()}},
		{"release", map[string]any{"v": protocolVersion, "op": "release", "session": "vsweep", "agent_id": "ag_1"}},
		{"session", map[string]any{"v": protocolVersion, "op": "session", "action": "start",
			"session": "vsweep3", "pid": os.Getpid(), "weight": 1}},
		{"status", acquireStatusReq()},
		{"unknown op", map[string]any{"v": protocolVersion, "op": "nonesuch"}},
		{"unsupported version", map[string]any{"v": otherVersion, "op": "status"}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			if got := call(t, endpoint, op.req).V; got != protocolVersion {
				t.Errorf("reply to %s carried v=%d, want %d", op.name, got, protocolVersion)
			}
		})
	}
}

// TestEveryRequestCarriesTheProtocolVersion is the same claim for the requests this binary
// sends, read off the wire rather than off the source.
//
// The daemon rejects a request whose v it does not recognise, and a request with NO v decodes
// as v=0 — so a client that forgot the field is refused every time and the machine silently
// stops being governed. That is worth a test that cannot be satisfied by a struct literal
// that merely looks right.
func TestEveryRequestCarriesTheProtocolVersion(t *testing.T) {
	lines := make(chan string, 8)
	endpoint := recordingListener(t, lines)
	t.Setenv(transport.EndpointEnv, endpoint)

	send := []struct {
		name string
		fn   func()
	}{
		{"gate", func() { runGateSpeaking(t, endpoint, samplePayload, transport.Dial{}, protocolVersion) }},
		{"confirm", func() { runHookCmd(t, endpoint, subagentStartPayload("s1", "ag_1"), buildConfirm) }},
		{"release", func() { runHookCmd(t, endpoint, subagentStopPayload("s1", "ag_1"), buildRelease) }},
		{"session", func() { runHookCmd(t, endpoint, sessionPayload("s1", "SessionStart"), buildSession("start")) }},
		{"statusline", func() { runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0) }},
	}
	for _, op := range send {
		name := op.name
		t.Run(name, func(t *testing.T) {
			op.fn()
			select {
			case line := <-lines:
				var got struct {
					V  int    `json:"v"`
					Op string `json:"op"`
				}
				if err := json.Unmarshal([]byte(line), &got); err != nil {
					t.Fatalf("decode %s request %q: %v", name, line, err)
				}
				if got.V != protocolVersion {
					t.Errorf("%s sent v=%d, want %d (line: %s)", name, got.V, protocolVersion, line)
				}
				if got.Op == "" {
					t.Errorf("%s sent no op (line: %s)", name, line)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s sent no request", name)
			}
		})
	}
}

// acquireStatusReq is a status request in this binary's version.
func acquireStatusReq() map[string]any {
	return map[string]any{"v": protocolVersion, "op": "status",
		"principal": map[string]any{"user": "tester", "team": nil}}
}

// ---------------------------------------------------------------------------
// upgrade: install replaces a daemon that is not this binary
// ---------------------------------------------------------------------------

// TestTheDaemonReportsWhichBinaryItIs is what makes the replace decision possible at all.
//
// Before GOV-24 nothing could tell which governor held the socket: the protocol "v" answers
// a narrower question, since two builds can speak v1 and still differ. An install with no
// way to ask this cannot tell a current daemon from a stale one, and so either never
// replaces (leaving an upgrade half-landed) or always does (dropping live leases on every
// re-install).
func TestTheDaemonReportsWhichBinaryItIs(t *testing.T) {
	endpoint, _ := startTestDaemon(t, 4)

	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got struct {
		BinaryVersion string `json:"binary_version"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode status %q: %v", line, err)
	}
	if got.BinaryVersion != binaryVersion {
		t.Errorf("binary_version = %q, want %q", got.BinaryVersion, binaryVersion)
	}
}

// TestInstallReplacesADaemonOfAnotherVersion is the Done-when's last clause, end to end
// against real processes: an upgrade must leave the NEW daemon holding the socket.
//
// Staged the way an upgrade actually happens rather than by simulating one. A binary
// stamped with a different version is installed over a running daemon of the current one,
// which is precisely the state section 13.4's rename(2) produces: new bytes on disk, old
// process still serving.
func TestInstallReplacesADaemonOfAnotherVersion(t *testing.T) {
	p := stagedHome(t)

	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })

	endpoint := "unix://" + filepath.Join(p.state, "sock")
	before, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid: %v", err)
	}

	// The upgrade. A different version stamp is also different bytes, so this exercises the
	// digest signal; the interrogation signal has its own test below.
	upgraded := governorStampedAs(t, "9.9.9-upgrade")
	code, out, errOut := runRealBinary(t, upgraded, p, "install")
	if code != 0 {
		t.Fatalf("upgrade install: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "replacing the running daemon") {
		t.Errorf("install did not say it replaced the daemon:\n%s", out)
	}

	after, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid after upgrade: %v", err)
	}
	if after == before {
		t.Fatalf("the daemon was not replaced: pid %d before and after. An upgraded binary "+
			"with the old daemon still on the socket is an upgrade that half-landed", before)
	}
	if !daemonAnswers(endpoint) {
		t.Fatalf("no daemon answering after the upgrade\n%s\n%s", out, daemonLog(t, p))
	}
	if procAlive(before) {
		t.Errorf("the old daemon (pid %d) is still running after the upgrade", before)
	}
	if v := daemonBinaryVersion(t, endpoint); v != "9.9.9-upgrade" {
		t.Errorf("the daemon on the socket reports version %q, want the upgraded %q", v, "9.9.9-upgrade")
	}
}

// TestInstallReplacesAStaleDaemonWhoseBinaryIsAlreadyCurrent covers the case the digest
// cannot see.
//
// An earlier install that failed to replace the daemon leaves the machine with a CORRECT
// binary on disk and a stale process on the socket. The next install copies nothing — the
// digests already match — so if the replace decision rested on the copy alone, that daemon
// would never be replaced by any number of re-installs. Interrogating it is what closes
// that, and this is the test that fails if the two signals are collapsed into one.
func TestInstallReplacesAStaleDaemonWhoseBinaryIsAlreadyCurrent(t *testing.T) {
	p := stagedHome(t)

	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })
	endpoint := "unix://" + filepath.Join(p.state, "sock")

	// Put a daemon of another version on the socket while leaving the installed binary
	// exactly as it is, which is the residue of an install that did not replace.
	stopTheDaemon(t, p)
	stale := governorStampedAs(t, "0.0.1-stale")
	startDetachedDaemon(t, stale, p)
	if v := daemonBinaryVersion(t, endpoint); v != "0.0.1-stale" {
		t.Fatalf("staging failed: the daemon reports %q, want the stale %q", v, "0.0.1-stale")
	}

	code, out, errOut := runReal(t, p, "install")
	if code != 0 {
		t.Fatalf("re-install: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "replacing the running daemon") {
		t.Errorf("install left a stale daemon on the socket:\n%s", out)
	}
	if v := daemonBinaryVersion(t, endpoint); v != binaryVersion {
		t.Errorf("the daemon reports version %q after a re-install, want %q", v, binaryVersion)
	}
}

// TestInstallReplacesADaemonOfTheSameVersionButDifferentBytes is the case the version
// string cannot see, and it is the one that hits developers hardest.
//
// Two builds from different source both call themselves 0.0.0-dev and compare EQUAL, which
// is the same trap design.md 13.4 records for doctor's stale-hooks check. An install that
// decided by version alone would leave the old daemon running for exactly the audience most
// likely to be upgrading — the person rebuilding the binary — and the symptom would be code
// changes that appear to have no effect.
//
// The binary here is built with optimisations off: different bytes, identical version. Only
// the digest can tell them apart, so this test fails if that signal is ever dropped in
// favour of comparing versions.
func TestInstallReplacesADaemonOfTheSameVersionButDifferentBytes(t *testing.T) {
	p := stagedHome(t)

	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })
	before, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid: %v", err)
	}
	endpoint := "unix://" + filepath.Join(p.state, "sock")
	if v := daemonBinaryVersion(t, endpoint); v != binaryVersion {
		t.Fatalf("staging: daemon reports %q, want %q", v, binaryVersion)
	}

	rebuilt := governorRebuilt(t)
	code, out, errOut := runRealBinary(t, rebuilt, p, "install")
	if code != 0 {
		t.Fatalf("re-install of a rebuilt binary: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "replacing the running daemon") {
		t.Errorf("a rebuilt binary reporting the same version did not replace the daemon:\n%s", out)
	}
	after, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid after the rebuild: %v", err)
	}
	if after == before {
		t.Errorf("the daemon was not replaced: pid %d before and after. Two builds that both "+
			"call themselves %s are still different binaries", before, binaryVersion)
	}
}

// TestInstallLeavesACurrentDaemonAlone is the other side of the same decision, and it is
// what stops the replace path from being bought at the cost of idempotency.
//
// Restarting a healthy daemon drops every live lease on the machine for no reason. A second
// install of the same binary must find the daemon it started and leave it exactly where it
// is.
func TestInstallLeavesACurrentDaemonAlone(t *testing.T) {
	p := stagedHome(t)

	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })
	before, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid: %v", err)
	}

	code, out, errOut := runReal(t, p, "install")
	if code != 0 {
		t.Fatalf("second install: exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, "replacing the running daemon") {
		t.Errorf("a second install of the same binary replaced a healthy daemon:\n%s", out)
	}
	if !strings.Contains(out, "already running") {
		t.Errorf("a second install did not report the running daemon:\n%s", out)
	}
	after, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid after the second install: %v", err)
	}
	if after != before {
		t.Errorf("the daemon was replaced by an identical install: pid %d became %d", before, after)
	}
}

// TestAnUpgradeRenamesTheBinaryRatherThanOverwritingIt pins design.md 13.4's normative
// requirement to an observable fact.
//
// GOV-02 measured the failure: `cp` onto a running Mach-O on macOS invalidates its code
// signature, and the kernel then SIGKILLs every later execution — exit 137, nothing on
// stdout, stderr or any log. A process killed by a signal cannot exit 0 however carefully it
// is written, so the gate's fail-open guarantee goes with it, and during an upgrade that
// lands on every hook of every live session at once. It presents as "the hook is broken"
// rather than as an install problem, which is why it gets debugged in the wrong place.
//
// Until this story the requirement was latent: install never replaced a running daemon, so
// nothing here performed an upgrade. It performs one now, which is what makes this worth a
// test rather than a comment.
//
// The inode is the evidence. rename(2) swaps a directory entry, so the path points at a NEW
// inode and every running process stays on the old one, correctly signed. A copy onto the
// existing file writes through to the SAME inode, which is precisely the corruption. The
// two are indistinguishable by content and differ exactly here.
func TestAnUpgradeRenamesTheBinaryRatherThanOverwritingIt(t *testing.T) {
	p := stagedHome(t)

	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })

	installed := filepath.Join(p.plugin, "bin", "governor")
	before, err := inodeOf(installed)
	if err != nil {
		t.Fatalf("stat %s: %v", installed, err)
	}

	upgraded := governorStampedAs(t, "9.9.9-rename")
	if code, out, errOut := runRealBinary(t, upgraded, p, "install"); code != 0 {
		t.Fatalf("upgrade install: exit %d\n%s\n%s", code, out, errOut)
	}

	after, err := inodeOf(installed)
	if err != nil {
		t.Fatalf("stat %s after the upgrade: %v", installed, err)
	}
	if after == before {
		t.Errorf("the upgraded binary landed on the same inode (%d), so it was written in "+
			"place rather than renamed over. design.md 13.4: a copy onto a running Mach-O "+
			"invalidates its code signature and the kernel SIGKILLs every later execution", before)
	}
}

// inodeOf returns a file's inode number, which is what tells a rename from a copy.
func inodeOf(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no stat for %s on this platform", path)
	}
	return uint64(st.Ino), nil
}

// TestNoSpawnStallsWhileTheDaemonIsBeingReplaced is the clause "without any session
// hanging", asserted where it is actually at risk.
//
// The window is real and unavoidable: between the old daemon releasing the socket and the
// new one binding it there is no daemon at all, and for part of that window the socket FILE
// is still on disk with nothing behind it — the shape that hangs a naive client. A gate that
// meets it must fall open on its own budget. What must never happen is a gate that blocks
// until the hook's own 30 s timeout, because that stalls the session rather than the spawn.
//
// WHAT THIS TEST DOES NOT ASSERT, and why. It does not require an empty stdout. A refusal is
// a bounded, correct answer, and demanding silence here would make the test fail whenever the
// budget legitimately filled — which is a statement about capacity, not about hanging. The
// claim is the BOUND, so the bound is what is measured.
//
// Each spawn is released before the next, for the same reason. An unreleased loop fills the
// budget within four iterations and every later gate is then parked for gateWaitMax by a
// daemon behaving perfectly — twenty seconds that look exactly like the stall being hunted,
// arriving from the opposite cause. That is the failure this test was rewritten to stop
// reporting.
func TestNoSpawnStallsWhileTheDaemonIsBeingReplaced(t *testing.T) {
	p := stagedHome(t)

	if code, out, errOut := runReal(t, p, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() { runReal(t, p, "uninstall", "--purge") })
	endpoint := "unix://" + filepath.Join(p.state, "sock")

	// Set once, outside the loop: t.Setenv is not safe to call from the goroutine below.
	t.Setenv(transport.EndpointEnv, endpoint)

	upgraded := governorStampedAs(t, "9.9.9-swap")

	// The upgrade runs while gates fire. Started first so the window is inside the loop
	// rather than after it.
	type result struct {
		code int
		out  string
		err  string
	}
	installed := make(chan result, 1)
	go func() {
		code, out, errOut := runRealBinary(t, upgraded, p, "install")
		installed <- result{code, out, errOut}
	}()

	var spawns, worst int64
	for done := false; !done; {
		select {
		case r := <-installed:
			if r.code != 0 {
				t.Fatalf("upgrade install: exit %d\n%s\n%s", r.code, r.out, r.err)
			}
			done = true
		default:
		}

		// Register the session first, exactly as SessionStart does. Without it the leases
		// this loop takes have no owning process, and `governor doctor` — which the install
		// under test runs to verify itself — correctly reports them as unreclaimable and
		// exits non-zero. That was a true finding about a state only this test manufactures,
		// so the fix is for the test to stop manufacturing it rather than to stop asserting
		// the install succeeded. It is re-sent every iteration because the replacement daemon
		// starts with an empty session registry.
		sessionOp(endpoint, "start")

		var stdout, stderr bytes.Buffer
		start := time.Now()
		code := gate(strings.NewReader(samplePayload), &stdout, &stderr, transport.Dial{}, gateBudget, protocolVersion)
		elapsed := time.Since(start)
		spawns++
		if int64(elapsed) > worst {
			worst = int64(elapsed)
		}

		if code != 0 {
			t.Fatalf("gate exit %d during the swap, want 0: a spawn must never fail because "+
				"the daemon is being replaced (stderr: %s)", code, stderr.String())
		}
		if elapsed > failOpenCeiling {
			t.Fatalf("a spawn took %v during the daemon swap, want under %v. The window between "+
				"the old daemon releasing the socket and the new one binding it must cost a "+
				"spawn its governance, never a session its responsiveness (stderr: %s)",
				elapsed.Round(time.Millisecond), failOpenCeiling, stderr.String())
		}

		// Return whatever this spawn took, so the budget cannot fill and turn a correct
		// capacity wait into a false report of a stall.
		sessionOp(endpoint, "end")

		// Paced rather than spun. An unpaced loop ran ~10,000 spawns in under three seconds,
		// which is not a load any machine produces and which starved the install it was
		// meant to be running alongside. A few milliseconds is still far finer than the swap
		// window, so the window is hit either way.
		time.Sleep(2 * time.Millisecond)
	}

	if spawns == 0 {
		t.Fatal("no spawn ran during the swap; the test measured nothing")
	}
	t.Logf("%d spawns across the swap, worst %v", spawns, time.Duration(worst).Round(time.Millisecond))

	if v := daemonBinaryVersion(t, endpoint); v != "9.9.9-swap" {
		t.Errorf("the daemon reports %q after the swap, want %q", v, "9.9.9-swap")
	}
}

// sessionOp starts or ends the sample payload's session on the daemon.
//
// Best effort by design: during the swap there is no daemon to tell, and that is the state
// the loop above is deliberately creating. A failure here is the expected case, not an error.
func sessionOp(endpoint, action string) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	conn, err := transport.Dial{}.Dial(ctx, endpoint)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	fmt.Fprintf(conn, `{"v":%d,"op":"session","action":%q,"session":%q,"pid":%d,"weight":1}`+"\n",
		protocolVersion, action, samplePayloadSession, os.Getpid())
	_, _ = bufio.NewReader(conn).ReadString('\n')
}

// samplePayloadSession is the session id inside samplePayload.
const samplePayloadSession = "2ed4bf1c-470b-4e08-9b48-4f310663cb6e"

// daemonBinaryVersion asks the daemon on the endpoint which binary it is.
func daemonBinaryVersion(t *testing.T, endpoint string) string {
	t.Helper()
	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status on %s: %v", endpoint, err)
	}
	var got struct {
		BinaryVersion string `json:"binary_version"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode status %q: %v", line, err)
	}
	return got.BinaryVersion
}

// stopTheDaemon stops whatever daemon is running under this staged home and waits for the
// socket to be free, so a replacement can bind it.
func stopTheDaemon(t *testing.T, p paths) {
	t.Helper()
	pid, err := readDaemonPID(p.state)
	if err != nil {
		t.Fatalf("read daemon pid: %v", err)
	}
	if err := syscallKill(pid); err != nil {
		t.Fatalf("stop daemon %d: %v", pid, err)
	}
	endpoint := "unix://" + filepath.Join(p.state, "sock")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !procAlive(pid) && !daemonAnswers(endpoint) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon %d did not stop", pid)
}

// startDetachedDaemon runs a given governor binary as a daemon against a staged home and
// waits for it to answer, so a test can stage a daemon that install did not start.
func startDetachedDaemon(t *testing.T, binary string, p paths) {
	t.Helper()
	endpoint := "unix://" + filepath.Join(p.state, "sock")
	cmd := daemonCommand(binary, p)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon %s: %v", binary, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	t.Cleanup(func() {
		if procAlive(pid) {
			_ = syscallKill(pid)
		}
	})
	// The daemon writes its own pid file, and install's replace path reads it. Waiting for
	// the file rather than only for the socket is what makes the staging complete: a socket
	// with no pid file is a different scenario, and not the one being staged here.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := readDaemonPID(p.state); err == nil && got == pid && daemonAnswers(endpoint) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("staged daemon %s never answered on %s", binary, endpoint)
}

// governorStampedAs builds the governor with a given binaryVersion.
//
// Two things at once, both wanted: the stamp makes the daemon report a different version,
// and it also makes the file different bytes, which is what an upgrade is on disk. Built
// through the same -ldflags path a release uses (design.md 13.4), so this cannot pass
// against a build configuration a release could not produce.
func governorStampedAs(t *testing.T, version string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gov-stamped")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	out := filepath.Join(dir, "governor")
	buildGovernorTo(t, out, "-ldflags", "-X main.binaryVersion="+version)
	return out
}

// countingListener answers every request with a reply in the given protocol version and
// counts the requests it was sent.
func countingListener(t *testing.T, count *int, speaks int) string {
	t.Helper()
	var mu sync.Mutex
	reply := fmt.Sprintf(`{"v":%d,"ok":false,"reason":"unsupported_version","supported":[%d]}`, speaks, speaks)
	return listenerFunc(t, func(c net.Conn, _ []byte) {
		mu.Lock()
		*count++
		mu.Unlock()
		c.Write([]byte(reply + "\n"))
	})
}

// recordingListener hands each request line it receives to lines, and answers in this
// binary's version so the client under test completes normally.
func recordingListener(t *testing.T, lines chan<- string) string {
	t.Helper()
	return listenerFunc(t, func(c net.Conn, req []byte) {
		select {
		case lines <- strings.TrimSpace(string(req)):
		default:
		}
		// A grant-shaped reply carrying the fields every client reads. It is answered rather
		// than dropped so that each hook finishes its exchange and the next one starts clean.
		fmt.Fprintf(c, `{"v":%d,"ok":true,"lease":"L-test","budget":4,"in_use":1,`+
			`"cpu":null,"thermal":"nominal","sources":[],"sessions":[],"sessions_known":false}`+"\n",
			protocolVersion)
	})
}

// daemonCommand builds the command that runs a governor binary as a daemon against a staged
// home, detached the way install detaches it.
func daemonCommand(binary string, p paths) *exec.Cmd {
	cmd := exec.Command(binary, "daemon")
	cmd.Env = stagedEnv(p)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// syscallKill asks a process to stop the way uninstall does, with SIGTERM so the daemon can
// close its listener rather than leaving the socket file behind.
func syscallKill(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// governorRebuilt builds a binary with the SAME version stamp and different bytes.
//
// Optimisations off is what makes the object code differ while binaryVersion keeps its
// default. Any build flag that changes the output without changing that variable would do;
// this one is stable across Go versions and obviously unrelated to the governor's behaviour.
func governorRebuilt(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gov-rebuilt")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	out := filepath.Join(dir, "governor")
	buildGovernorTo(t, out, "-gcflags=all=-N -l")
	return out
}
