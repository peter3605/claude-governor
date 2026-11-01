package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// GOV-30. The defect these tests exist for, in one line of real output:
//
//	ok    spawn   Agent, Task, SendMessage, measured against Claude Code 2.1.267 — NOT
//	              re-checked against 2.1.268 and 2.1.270, which is running here
//
// followed by "10 checks, all passing." and exit 0. The check had NOTICED that its claim was
// stale and still reported a pass, which is the pre-condition of the one failure this whole
// project exists to catch: a renamed spawn tool, a matcher that never fires, and everything
// downstream looking like it works.

// sessionOn is one measured session, moved onto the given Claude Code version.
func sessionOn(version string) []signals.LiveSession {
	s := measuredGoverned[0]
	s.Version = version
	return []signals.LiveSession{s}
}

// stagedMachine is a working install whose only interesting variable is the spawn ledger.
func stagedMachine(t *testing.T, version string, seen map[string][]string) {
	t.Helper()
	stageWorkingInstall(t)
	t.Setenv(transport.EndpointEnv, machineWithSurface(t, sessionOn(version),
		measuredHooksInstalled, measuredDaemonStarted, ids(measuredGoverned[:1]), seen))
}

// ---------------------------------------------------------------------------
// clause 1: a check that could not verify does not report ok
// ---------------------------------------------------------------------------

// TestAnUnconfirmedSpawnSurfaceIsNotReportedAsPassing is the acceptance criterion verbatim:
// force the spawn check into the state it was in on 2026-09-12 and both the exit status and
// the summary line must change.
func TestAnUnconfirmedSpawnSurfaceIsNotReportedAsPassing(t *testing.T) {
	// A daemon that has never seen a spawn, on a version nothing has been measured against.
	stagedMachine(t, "2.4.0", nil)

	code, found, out := doctor(t)
	f := want(t, found, "spawn", "unknown", out)
	if !strings.Contains(f.detail, "2.4.0") {
		t.Errorf("detail does not name the version it could not confirm: %q", f.detail)
	}
	if f.fix == "" {
		t.Errorf("an unverified check with no way forward is just a complaint: %q", f.detail)
	}
	if strings.Contains(out, "all passing") {
		t.Errorf("the summary still claims everything passed:\n%s", out)
	}
	if !strings.Contains(out, "could not be verified") {
		t.Errorf("the summary does not name what could not be verified:\n%s", out)
	}
	if code != doctorExitUnverified {
		t.Fatalf("exit %d, want %d — a report that cannot be trusted must not script as success:\n%s",
			code, doctorExitUnverified, out)
	}
}

// TestAnUnverifiedCheckIsNotAFailure holds the other edge, on a machine that also has a
// warning on it: nothing here is broken, so no line may say FAIL and the exit code must not be
// the one that means broken. The two states are counted apart in the summary.
func TestAnUnverifiedCheckIsNotAFailure(t *testing.T) {
	stagedMachine(t, "2.4.0", nil)

	code, found, out := doctor(t)
	if code == 1 {
		t.Fatalf("an unconfirmed claim was reported as a broken machine:\n%s", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Fatalf("an unconfirmed claim printed a failure:\n%s", out)
	}
	for name, f := range found {
		if f.state == "unknown" && f.fix == "" {
			t.Errorf("%s could not verify itself and names nothing to do about it: %q", name, f.detail)
		}
	}
}

// ---------------------------------------------------------------------------
// clause 2: the surface is confirmed against the running harness
// ---------------------------------------------------------------------------

// TestSpawnIsConfirmedByASpawnTheGateActuallyCaught is the positive case, and the reason the
// ledger is evidence rather than bookkeeping: the gate runs ONLY because the matcher fired,
// so a recorded sighting on a version is proof the matcher is not blind on that version.
func TestSpawnIsConfirmedByASpawnTheGateActuallyCaught(t *testing.T) {
	stagedMachine(t, "2.4.0", map[string][]string{"2.4.0": {"Agent"}})

	code, found, out := doctor(t)
	f := want(t, found, "spawn", "ok", out)
	if !strings.Contains(f.detail, "2.4.0") || !strings.Contains(f.detail, "Agent") {
		t.Errorf("a pass that does not say what confirmed it is the old unfalsifiable claim: %q", f.detail)
	}
	if code != 0 {
		t.Fatalf("exit %d on a machine whose matcher is confirmed firing, want 0:\n%s", code, out)
	}
}

// TestARenamedSpawnToolIsReportedAsUnconfirmed is the failure this project exists to catch,
// modelled end to end.
//
// A Claude Code that renames the spawn tool does not announce it. The matcher simply stops
// matching, the gate stops being invoked, and NOTHING is observed on the new version — while
// every other check stays green, because the daemon is healthy, the hooks are registered and
// the matcher on disk is exactly the one this binary asks for. Silence on the running version
// is the only signal available, and the check has to be willing to speak on it.
func TestARenamedSpawnToolIsReportedAsUnconfirmed(t *testing.T) {
	// Plenty of evidence — for the version that is no longer running.
	stagedMachine(t, "2.9.0", map[string][]string{"2.1.270": {"Agent", "SendMessage"}})

	code, found, out := doctor(t)
	f := want(t, found, "spawn", "unknown", out)
	if !strings.Contains(f.detail, "2.9.0") {
		t.Errorf("detail does not name the version with no sighting: %q", f.detail)
	}
	if !strings.Contains(f.detail, "2.1.270") {
		t.Errorf("detail throws away the evidence it does have: %q", f.detail)
	}
	if code != doctorExitUnverified {
		t.Fatalf("exit %d, want %d:\n%s", code, doctorExitUnverified, out)
	}
	// The matcher check is the one that CANNOT catch this — install writes the matcher from
	// spawnToolNames, so the two agree by construction no matter what the harness sends. If
	// this assertion ever fails, the test has stopped modelling the interesting case.
	if m := want(t, found, "matcher", "ok", out); !strings.Contains(m.detail, "Agent") {
		t.Errorf("matcher check unexpectedly implicated: %q", m.detail)
	}
}

// TestEveryRunningVersionHasToBeConfirmed: one confirmed version does not vouch for another.
// Several Claude Code versions run side by side on a developer's machine — the measured
// sample had three — and a rename lands on one of them first.
func TestEveryRunningVersionHasToBeConfirmed(t *testing.T) {
	stageWorkingInstall(t)
	live := []signals.LiveSession{measuredGoverned[0], measuredGoverned[1]}
	live[0].Version = "2.1.270"
	live[1].Version = "2.2.0"
	t.Setenv(transport.EndpointEnv, machineWithSurface(t, live,
		measuredHooksInstalled, measuredDaemonStarted, ids(live),
		map[string][]string{"2.1.270": {"Agent"}}))

	_, found, out := doctor(t)
	f := want(t, found, "spawn", "unknown", out)
	if !strings.Contains(f.detail, "2.2.0") {
		t.Errorf("the unconfirmed version is not named: %q", f.detail)
	}
	if strings.Contains(f.detail, "no spawn seen yet on Claude Code 2.1.270") {
		t.Errorf("the confirmed version was reported as unconfirmed: %q", f.detail)
	}
}

// TestDoctorPassesEveryCheckOnAFullyObservableMachine is the all-green case, and the other
// half of TestDoctorPassesOnAWorkingInstall: give the two checks that can be blind what they
// need to see, and every line does go ok.
func TestDoctorPassesEveryCheckOnAFullyObservableMachine(t *testing.T) {
	stagedMachine(t, "2.1.270", map[string][]string{"2.1.270": {"Agent"}})

	code, found, out := doctor(t)
	for name, f := range found {
		if f.state != "ok" {
			t.Errorf("%s: %s (%s), want ok\n%s", name, f.state, f.detail, out)
		}
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "all passing") {
		t.Errorf("report does not say it passed:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// the ledger itself
// ---------------------------------------------------------------------------

// TestTheLedgerRecordsOnlyWhatTheGateSaw. It records tool names off real payloads and nothing
// else: no name from spawnToolNames leaks into it, because a ledger seeded from this repo's
// own source could never contradict this repo's own source.
func TestTheLedgerRecordsOnlyWhatTheGateSaw(t *testing.T) {
	l := newSurfaceLedger(t.TempDir())

	if l.note("2.1.270", "") {
		t.Error("an acquire carrying no tool name wrote a sighting")
	}
	if got := l.sightings(); len(got) != 0 {
		t.Fatalf("ledger is not empty: %+v", got)
	}
	if !l.note("2.1.270", "Agent") {
		t.Error("a first sighting did not report itself as new")
	}
	if l.note("2.1.270", "Agent") {
		t.Error("a repeat sighting asked for a needless disk write")
	}
	if !l.note("2.1.270", "SendMessage") {
		t.Error("a second tool name on a known version did not report itself as new")
	}

	got := l.sightings()
	if len(got) != 1 {
		t.Fatalf("want one version, got %+v", got)
	}
	if strings.Join(got[0].Tools, ",") != "Agent,SendMessage" {
		t.Errorf("tools %v, want Agent and SendMessage sorted", got[0].Tools)
	}
	if got[0].Count != 3 {
		t.Errorf("count %d, want 3 — every acquire counts, not only the novel ones", got[0].Count)
	}
}

// TestTheLedgerSurvivesADaemonRestart. Without this the evidence is lost on every `governor
// install`, which restarts the daemon — and the check would go unknown after every upgrade
// until something happened to spawn.
func TestTheLedgerSurvivesADaemonRestart(t *testing.T) {
	dir := t.TempDir()
	first := newSurfaceLedger(dir)
	first.note("2.1.270", "Agent")
	first.save()

	second := newSurfaceLedger(dir)
	got := second.sightings()
	if len(got) != 1 || len(got[0].Tools) != 1 || got[0].Tools[0] != "Agent" {
		t.Fatalf("ledger did not survive the restart: %+v", got)
	}

	// Mode matters: it sits beside the socket in a directory the install promises is the
	// user's alone.
	info, err := os.Stat(filepath.Join(dir, surfaceFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("ledger mode %v, want 0600", info.Mode().Perm())
	}
}

// TestAMangledLedgerIsNotFatal. It is evidence, not state: a daemon that refused to start
// over an unreadable diagnostic would be a worse bug than the one being diagnosed.
func TestAMangledLedgerIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, surfaceFile), []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newSurfaceLedger(dir)
	if got := l.sightings(); len(got) != 0 {
		t.Fatalf("garbage was read as evidence: %+v", got)
	}
	if !l.note("2.1.270", "Agent") {
		t.Error("the ledger did not recover for the next spawn")
	}
}

// TestTheDaemonRecordsTheToolNameTheGateReports is the wire end of it: the gate puts the
// tool_name from the payload on the acquire, and the daemon turns that into a sighting
// against the session's Claude Code version.
func TestTheDaemonRecordsTheToolNameTheGateReports(t *testing.T) {
	live := sessionOn("2.1.270")
	_, d := startTestDaemonConfigured(t, 4, leaseOptionsForSurfaceTest(), func(d *daemon) {
		sessions := live
		d.liveSessions = func() ([]signals.LiveSession, error) { return sessions, nil }
	})

	d.acquire(request{V: protocolVersion, Op: "acquire", Session: live[0].ID, Tool: "Agent"})

	// noteSpawnSurface deliberately runs off the reply path, so the sighting lands shortly
	// after the acquire returns rather than during it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := d.surface.sightings()
		if len(got) == 1 && got[0].Version == "2.1.270" && len(got[0].Tools) == 1 && got[0].Tools[0] == "Agent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never recorded what the gate reported: %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The other half of this — that an older gate's acquire, which carries no tool name, leaves
	// no trace — is NOT asserted here. A sleep-then-check cannot tell "did not record" from
	// "has not recorded yet", so it would be a test that passes whether or not the guard
	// exists. TestTheLedgerRecordsOnlyWhatTheGateSaw covers it synchronously instead.
}

// TestTheGateSendsTheToolNameItMatched binds the payload to the wire. Without it the gate
// could stop forwarding the name and every surface check would quietly go unknown forever,
// which looks like a harness change rather than a governor bug.
func TestTheGateSendsTheToolNameItMatched(t *testing.T) {
	for _, name := range spawnToolNames {
		p := hookPayload{SessionID: "s", ToolName: name}
		req := acquireRequestFor(p, decide(p), protocolVersion)
		if req.Tool != name {
			t.Errorf("gate sent tool %q for a %q payload", req.Tool, name)
		}
	}
}

// ---------------------------------------------------------------------------
// clause 4: the matcher and the recorded surface cannot drift apart
// ---------------------------------------------------------------------------

// TestRecordedSurfaceMatchesTheMatcher ties three things that are written in three places and
// must say the same thing: the tool names this binary matches on (spawnToolNames), the
// version the repo claims to have measured them against (spawnToolNamesConfirmedAgainst), and
// the measurement itself (docs/hook-surface.md).
//
// The direction that matters is a matcher expecting a name the harness no longer sends. That
// cannot be caught by reading code, because install writes the matcher FROM spawnToolNames —
// the only external record is the measurement, so the measurement is what this reads.
func TestRecordedSurfaceMatchesTheMatcher(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "hook-surface.md"))
	if err != nil {
		t.Fatalf("the measured surface is the only external record there is: %v", err)
	}
	surface := string(doc)

	// The version stamp must name a version the document actually reports measuring. A stamp
	// bumped without a re-measurement is the unfalsifiable claim all over again.
	stamp := regexp.QuoteMeta(spawnToolNamesConfirmedAgainst)
	if !regexp.MustCompile(`(?m)` + stamp + `\b`).MatchString(surface) {
		t.Fatalf("spawnToolNamesConfirmedAgainst is %q, which docs/hook-surface.md does not record measuring; "+
			"re-run the probe in section 12 and record it before moving the stamp",
			spawnToolNamesConfirmedAgainst)
	}

	// Every name the matcher is built from has to appear in the measurement as a tool name
	// seen on the wire, not merely mentioned in prose.
	for _, name := range spawnToolNames {
		if !strings.Contains(surface, `"tool_name":"`+name+`"`) &&
			!strings.Contains(surface, `"tool_name": "`+name+`"`) {
			t.Errorf("the matcher covers %q but no measured payload in docs/hook-surface.md carries it; "+
				"a matcher that matches names the harness never sends is untested, not safe", name)
		}
	}
}

// leaseOptionsForSurfaceTest keeps the daemon's lease behaviour at its defaults; it exists so
// the surface test reads without a bare empty literal in the middle of it.
func leaseOptionsForSurfaceTest() lease.Options { return lease.Options{} }

// TestASendMessageSightingDoesNotConfirmTheSpawnPath is the hole an adversarial review of the
// first cut of GOV-30 found, and it is the story's own failure mode wearing a disguise.
//
// The matcher covers three names, and only two of them START a subagent. `SendMessage` RESUMES
// one (docs/hook-surface.md section 8.3, path 2). So if a future Claude Code renames the spawn
// tool, `Agent` stops matching and every fresh spawn goes ungoverned — while `SendMessage`
// keeps matching, keeps reaching the gate, and keeps landing sightings on the new version. A
// check that passed on "any sighting at all" would print ok, exit 0, and be wrong in precisely
// the way this story was filed to prevent: a different tool firing covers the silence.
func TestASendMessageSightingDoesNotConfirmTheSpawnPath(t *testing.T) {
	stagedMachine(t, "2.9.0", map[string][]string{"2.9.0": {"SendMessage"}})

	code, found, out := doctor(t)
	f := want(t, found, "spawn", "unknown", out)
	if !strings.Contains(f.detail, "2.9.0") {
		t.Errorf("detail does not name the version whose spawn path is unconfirmed: %q", f.detail)
	}
	if code != doctorExitUnverified {
		t.Fatalf("exit %d, want %d — a resume is not proof that a spawn is caught:\n%s",
			code, doctorExitUnverified, out)
	}
}

// TestAnOlderDaemonIsNotAccusedOfAnUngovernedMachine. The ledger field is additive and the
// protocol version is deliberately not bumped for it, so a doctor from this build will meet
// daemons that send no ledger at all. Without SpawnSurfaceKnown it could not tell that from a
// daemon that keeps one and has seen nothing — and it told the user, in the strongest words in
// the report, that their spawns were not being governed. They were.
//
// Driven straight at the check rather than through a daemon, because the daemon this describes
// is one this build cannot start: status() always sets the flag.
func TestAnOlderDaemonIsNotAccusedOfAnUngovernedMachine(t *testing.T) {
	d := &diagnosis{up: true}
	d.reply.Sessions = []sessionStat{{Session: "s", Live: true, Version: "2.1.270"}}
	d.reply.SpawnSurfaceKnown = false
	d.checkSpawnTools()

	f := findingNamed(t, d, "spawn")
	if f.state != unverified {
		t.Fatalf("state %v, want unverified: %q", f.state, f.detail)
	}
	if strings.Contains(f.detail, "not firing") || strings.Contains(f.fix, "not firing") {
		t.Errorf("an old daemon was reported as a dead matcher: %q / %q", f.detail, f.fix)
	}
	if !strings.Contains(f.fix, governorCommand("install")) {
		t.Errorf("the fix for an old daemon is to upgrade it: %q", f.fix)
	}

	// And the same daemon once it IS this build: a ledger it keeps, with nothing in it yet,
	// is a different sentence and a different fix.
	d2 := &diagnosis{up: true}
	d2.reply.Sessions = d.reply.Sessions
	d2.reply.SpawnSurfaceKnown = true
	d2.checkSpawnTools()
	if f2 := findingNamed(t, d2, "spawn"); !strings.Contains(f2.fix, "spawn a subagent") {
		t.Errorf("a daemon that is recording and has seen nothing should ask for a spawn: %q", f2.fix)
	}
}

// TestConcurrentSavesDoNotLoseASighting. Two goroutines that each record a new pair snapshot,
// marshal and rename independently; without writeMu the OLDER snapshot can land last, and the
// pair it is missing is one that really was observed. The cost is not cosmetic — after the next
// daemon restart doctor tells the user their spawns are not being governed.
func TestConcurrentSavesDoNotLoseASighting(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		dir := t.TempDir()
		l := newSurfaceLedger(dir)

		versions := []string{"2.1.267", "2.1.268", "2.1.270", "2.2.0", "2.3.0", "2.4.0", "2.5.0", "2.6.0"}
		var wg sync.WaitGroup
		for _, v := range versions {
			wg.Add(1)
			go func(v string) {
				defer wg.Done()
				if l.note(v, "Agent") {
					l.save()
				}
			}(v)
		}
		wg.Wait()

		onDisk := newSurfaceLedger(dir).sightings()
		if len(onDisk) != len(versions) {
			t.Fatalf("attempt %d: memory holds %d versions, disk holds %d — a sighting was lost",
				attempt, len(l.sightings()), len(onDisk))
		}
	}
}

// TestTheLedgerRefusesAToolTheMatcherDoesNotCover. The gate only reaches the daemon for a name
// the matcher caught, so this is defence in depth — but doctor draws a conclusion from this
// file, and evidence accepted from an unexpected source is how a diagnostic starts confirming
// things nobody measured.
func TestTheLedgerRefusesAToolTheMatcherDoesNotCover(t *testing.T) {
	l := newSurfaceLedger(t.TempDir())
	for _, tool := range []string{"Bash", "Read", "TotallyNotASpawn", "agent", "AgentX"} {
		if l.note("2.1.270", tool) {
			t.Errorf("the ledger recorded %q, which the matcher does not cover", tool)
		}
	}
	if got := l.sightings(); len(got) != 0 {
		t.Fatalf("ledger is not empty: %+v", got)
	}
	for _, tool := range spawnToolNames {
		if !l.note("2.1.270", tool) {
			t.Errorf("the ledger refused %q, which the matcher is built from", tool)
		}
	}
}

// TestOnlyTheToolsThatStartASubagentConfirmTheSurface pins the two lists apart. They are
// written in one file and read in another, and collapsing them is the review finding that
// TestASendMessageSightingDoesNotConfirmTheSpawnPath exists to keep found.
func TestOnlyTheToolsThatStartASubagentConfirmTheSurface(t *testing.T) {
	for _, tool := range spawnConfirmingTools {
		if !matchedSpawnTool(tool) {
			t.Errorf("%q confirms the surface but the matcher does not cover it — the gate would never see it", tool)
		}
		if !confirmsSpawnPath(tool) {
			t.Errorf("%q is in spawnConfirmingTools and does not confirm the spawn path", tool)
		}
	}
	if confirmsSpawnPath("SendMessage") {
		t.Error("SendMessage resumes a subagent, it does not start one; it cannot confirm the spawn path")
	}
	if len(spawnConfirmingTools) >= len(spawnToolNames) {
		t.Error("the confirming set must stay narrower than the matched set, or a working tool can cover for a broken one")
	}
}

// TestASkippedCheckIsNotCountedAsPassing. GOV-30 made the no-failures-with-extras branch
// reachable for the first time, so the passing count now has to be right in a case it never met.
func TestASkippedCheckIsNotCountedAsPassing(t *testing.T) {
	var out strings.Builder
	code := (&diagnosis{findings: []finding{
		{name: "a", state: pass},
		{name: "b", state: skip},
		{name: "c", state: unverified, fix: "spawn one"},
	}}).report(&out)
	if !strings.Contains(out.String(), "3 checks, 1 passing and 1 that could not be verified.") {
		t.Errorf("a check that never ran was counted as passing:\n%s", out.String())
	}
	if code != doctorExitUnverified {
		t.Errorf("exit %d, want %d", code, doctorExitUnverified)
	}
}

// TestEveryStateHasItsOwnLabel. A state falling through to another state's label is silently
// dropped by findingRE, and the check then reads as absent from a report it is plainly in —
// the same "mislabelled, everything downstream looks fine" shape this story removed.
func TestEveryStateHasItsOwnLabel(t *testing.T) {
	seen := map[string]checkState{}
	for _, st := range []checkState{pass, fail, skip, warn, unverified} {
		if other, dup := seen[st.label()]; dup {
			t.Errorf("states %d and %d both print as %q", other, st, st.label())
		}
		seen[st.label()] = st
		if !findingRE.MatchString(st.label() + "  name  detail") {
			t.Errorf("findingRE does not recognise the label %q; every line in that state is dropped", st.label())
		}
	}
	if got := checkState(99).label(); got != "BUG?" {
		t.Errorf("an unnamed state printed as %q, which reads as a real verdict", got)
	}
}
