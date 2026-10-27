package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The state measured on this machine on 2026-09-09, which is what GOV-29 was filed from.
//
// Six sessions were live. Four of them started on 2026-09-08, some twenty-nine hours before
// the gate's hooks file was written at 18:57:50 local on the 9th, so they could not have
// loaded the gate and never will. `governor doctor` looked at that machine and reported
// 8 of 8, which is the defect this file exists to keep fixed.
//
// The times are the real ones. A synthetic "old" and "new" would exercise the comparison just
// as well and would not record what the failure actually looked like — and the four Sep-8
// sessions reporting Claude Code 2.1.263 against the newer sessions' 2.1.267 is itself part
// of the evidence that they predate the install.
var (
	measuredHooksInstalled = time.Date(2026, 9, 9, 22, 57, 50, 0, time.UTC)
	measuredDaemonStarted  = time.Date(2026, 9, 9, 23, 9, 1, 0, time.UTC)

	// The four that cannot be governed, oldest first.
	measuredUngoverned = []signals.LiveSession{
		{ID: "416e285a-b3d9-4e5b-8b4b-da516f267119", PID: 90756, CWD: "/Users/peterho/Repos/nightcrew",
			Started: time.Date(2026, 9, 8, 17, 8, 6, 0, time.UTC), Version: "2.1.263"},
		{ID: "31107751-d074-4e80-92dd-9bcb6ef14162", PID: 91363, CWD: "/Users/peterho/Repos/muncheez",
			Started: time.Date(2026, 9, 8, 17, 9, 29, 0, time.UTC), Version: "2.1.263"},
		{ID: "344e1498-3540-4cad-92c4-da7b0fc0de8f", PID: 91500, CWD: "/Users/peterho/Repos/concertFinder",
			Started: time.Date(2026, 9, 8, 17, 9, 45, 0, time.UTC), Version: "2.1.263"},
		{ID: "a8453b03-5031-410e-b5d8-4a459820dc8e", PID: 92107, CWD: "/Users/peterho/Repos/budgetr",
			Started: time.Date(2026, 9, 8, 17, 10, 56, 0, time.UTC), Version: "2.1.263"},
	}
	// The two that started after the hooks landed and did register.
	measuredGoverned = []signals.LiveSession{
		{ID: "286a8d57-a9f9-4017-b51a-38c28736d4e3", PID: 99459, CWD: "/Users/peterho/Repos/edgefinder-v2",
			Started: time.Date(2026, 9, 9, 23, 48, 58, 0, time.UTC), Version: "2.1.267"},
		{ID: "2bcbd3bd-a469-4be6-add4-e528a09b4e34", PID: 5364, CWD: "/Users/peterho/Repos/claude-governor",
			Started: time.Date(2026, 9, 9, 23, 54, 48, 0, time.UTC), Version: "2.1.267"},
	}
)

func measuredMachine() []signals.LiveSession {
	return append(append([]signals.LiveSession{}, measuredUngoverned...), measuredGoverned...)
}

func ids(live []signals.LiveSession) []string {
	out := make([]string, 0, len(live))
	for _, s := range live {
		out = append(out, s.ID)
	}
	return out
}

// machineWith starts a daemon that sees exactly the given sessions, and points doctor at it
// over a real socket. registered names the sessions whose SessionStart hook reached it.
// machineWith is a machine that has been spawning subagents normally: every Claude Code
// version running on it has been seen to send a spawn tool name the matcher caught. That is
// what makes the spawn check pass, so a test using this helper is about whatever it changed,
// not about GOV-30's ledger. Use machineWithSurface to control the ledger directly.
func machineWith(t *testing.T, live []signals.LiveSession, hooksAt, daemonAt time.Time, registered []string) string {
	t.Helper()
	seen := map[string][]string{}
	for _, s := range live {
		seen[s.Version] = []string{"Agent"}
	}
	return machineWithSurface(t, live, hooksAt, daemonAt, registered, seen)
}

// machineWithSurface is machineWith with the spawn-surface ledger stated outright: seen maps
// a Claude Code version to the spawn tool names the gate has been observed to catch on it.
// An empty map is a daemon that has never seen a spawn.
func machineWithSurface(t *testing.T, live []signals.LiveSession, hooksAt, daemonAt time.Time, registered []string, seen map[string][]string) string {
	t.Helper()
	endpoint, d := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		sessions := live
		d.liveSessions = func() ([]signals.LiveSession, error) { return sessions, nil }
		d.hooksInstalledAt = func() time.Time { return hooksAt }
		if !daemonAt.IsZero() {
			d.started = daemonAt
		}
		for version, tools := range seen {
			for _, tool := range tools {
				d.surface.note(version, tool)
			}
		}
	})
	for _, id := range registered {
		pid := 0
		for _, s := range live {
			if s.ID == id {
				pid = s.PID
			}
		}
		d.table.StartSession(lease.Session{ID: id, PID: pid, Weight: 1})
	}
	return endpoint
}

// stageWorkingInstall gives doctor a machine whose every other check passes, so that the
// sessions check is the only thing a test's result can be about.
func stageWorkingInstall(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())
	// A working install is a CALIBRATED one after GOV-25: `governor install` ends by running
	// `calibrate`, so a machine with hooks and no calibration is not the ordinary state but
	// the state of one whose calibration could not run — which doctor is supposed to warn
	// about. See world.writeCalibration.
	w.writeCalibration()
	return w
}

// THE TEST THE STORY ASKED FOR: doctor must go red against the machine as it actually was.
func TestDoctorFailsOnSessionsThatPredateTheInstall(t *testing.T) {
	stageWorkingInstall(t)
	t.Setenv(transport.EndpointEnv, machineWith(t, measuredMachine(),
		measuredHooksInstalled, measuredDaemonStarted, ids(measuredGoverned)))

	code, found, out := doctor(t)
	if code == 0 {
		t.Fatalf("doctor passed on a machine with four ungoverned sessions:\n%s", out)
	}
	f := want(t, found, "sessions", "FAIL", out)

	// How many of how many, or the reader cannot tell one stale session from a machine
	// where nothing at all is being governed.
	if !strings.Contains(f.detail, "4 of 6") {
		t.Errorf("detail does not count the sessions: %q", f.detail)
	}
	// Named somewhere a person can act on. A bare UUID is not somewhere.
	for _, repo := range []string{"nightcrew", "muncheez", "concertFinder", "budgetr"} {
		if !strings.Contains(f.detail, repo) {
			t.Errorf("detail does not name the %s session: %q", repo, f.detail)
		}
	}
	// The two that registered must not be swept into the fix.
	for _, repo := range []string{"claude-governor", "edgefinder-v2"} {
		if strings.Contains(f.fix, repo) {
			t.Errorf("fix tells the user to restart %s, which is governed: %q", repo, f.fix)
		}
	}
	if !strings.Contains(f.fix, "restart") {
		t.Errorf("fix does not name the fix: %q", f.fix)
	}
}

// The complement, which is what stops the check from being one that always fails.
func TestDoctorPassesWhenEverySessionPostdatesTheInstall(t *testing.T) {
	stageWorkingInstall(t)
	t.Setenv(transport.EndpointEnv, machineWith(t, measuredGoverned,
		measuredHooksInstalled, measuredDaemonStarted, ids(measuredGoverned)))

	code, found, out := doctor(t)
	f := want(t, found, "sessions", "ok", out)
	if !strings.Contains(f.detail, "2 of 2") {
		t.Errorf("detail = %q, want it to report 2 of 2 registered", f.detail)
	}
	if code != 0 {
		t.Fatalf("exit %d on a fully governed machine, want 0:\n%s", code, out)
	}
}

// A daemon restart must NOT be reported as an ungoverned machine.
//
// This is the failure mode the check was most likely to ship with. The session table is in
// memory and `governor install` restarts the daemon, so straight after any upgrade every live
// session is missing from it — including the ones that loaded the hooks perfectly well and
// are being governed. A check that read only "is it registered" would tell someone to restart
// every session on the machine every time they upgraded: the same confidently-wrong
// instrument this story exists to remove, pointing the other way.
func TestDoctorDoesNotBlameSessionsForADaemonRestart(t *testing.T) {
	stageWorkingInstall(t)
	// A daemon that came up after both sessions did, and has heard from neither since.
	restarted := measuredGoverned[1].Started.Add(time.Hour)
	t.Setenv(transport.EndpointEnv, machineWith(t, measuredGoverned,
		measuredHooksInstalled, restarted, nil))

	code, found, out := doctor(t)
	if f := want(t, found, "sessions", "ok", out); strings.Contains(f.detail, "ungoverned") {
		t.Errorf("a daemon restart was reported as ungoverned sessions: %q", f.detail)
	}
	if code != 0 {
		t.Fatalf("exit %d after a mere daemon restart, want 0:\n%s", code, out)
	}
}

// A session that started after both the hooks AND this daemon, and still never registered,
// is a real fault rather than a restart artefact: its SessionStart hook did not arrive.
func TestDoctorFailsWhenASessionShouldHaveRegisteredAndDidNot(t *testing.T) {
	stageWorkingInstall(t)
	// A daemon that was already up before either session started.
	early := measuredGoverned[0].Started.Add(-time.Hour)
	t.Setenv(transport.EndpointEnv, machineWith(t, measuredGoverned,
		measuredHooksInstalled, early, nil))

	code, found, out := doctor(t)
	if code == 0 {
		t.Fatalf("doctor passed on sessions that never registered with a daemon older than they are:\n%s", out)
	}
	if f := want(t, found, "sessions", "FAIL", out); !strings.Contains(f.detail, "SessionStart") {
		t.Errorf("detail does not say what went wrong: %q", f.detail)
	}
}

// An unreadable registry is "unknown", never "all clear".
//
// A machine whose Claude Code keeps no session registry must not read as a machine with
// nothing running. That reading is exactly what let four ungoverned sessions pass for
// healthy, and an instrument that cannot read has to say so rather than guess.
func TestDoctorSaysSoWhenItCannotSeeTheSessions(t *testing.T) {
	stageWorkingInstall(t)
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.liveSessions = func() ([]signals.LiveSession, error) { return nil, signals.ErrNoRegistry }
	})
	t.Setenv(transport.EndpointEnv, endpoint)

	code, found, out := doctor(t)
	// GOV-30: unknown, not ok. It used to be a pass whose detail led with "not verified",
	// and the summary underneath counted it toward "N checks, all passing" — the state column
	// is the word a hurried reader takes away, so that is where it has to be said.
	f := want(t, found, "sessions", "unknown", out)
	// It must not be a skip: this command reserves that for a check downstream of one that
	// already failed, and nothing here failed. It must also not read as a verification.
	if !strings.Contains(f.detail, "not verified") {
		t.Errorf("an unverifiable check does not say so: %q", f.detail)
	}
	if !strings.Contains(f.detail, "registry") {
		t.Errorf("detail does not say why it cannot tell: %q", f.detail)
	}
	// Both causes named. Blaming a missing registry alone sends someone hunting for a file
	// that, against an older daemon, is sitting there in perfect health — verified against a
	// real pre-GOV-29 daemon, which reports exactly this line on a machine whose registry has
	// five live sessions in it.
	if !strings.Contains(f.detail, "older") {
		t.Errorf("detail blames the registry without allowing for an older daemon: %q", f.detail)
	}
	if code != doctorExitUnverified {
		t.Fatalf("exit %d, want %d — a check that could not run must not read as broken, "+
			"nor as success\n%s", code, doctorExitUnverified, out)
	}
}

// The daemon must report the registry as unreadable rather than empty when it cannot read it.
// Empty and unknown are different answers and only one of them means "nothing to worry about".
func TestStatusDistinguishesAnUnreadableRegistryFromAnEmptyOne(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		d := newDaemon(4, nil)
		if got := d.status(request{}); got.SessionsKnown {
			t.Errorf("SessionsKnown = true with no registry to read")
		}
	})
	t.Run("readable and empty", func(t *testing.T) {
		d := newDaemon(4, nil)
		d.liveSessions = func() ([]signals.LiveSession, error) { return nil, nil }
		got := d.status(request{})
		if !got.SessionsKnown {
			t.Errorf("SessionsKnown = false for a registry that read cleanly")
		}
		if len(got.Sessions) != 0 {
			t.Errorf("Sessions = %+v, want none", got.Sessions)
		}
	})
}

// ---------------------------------------------------------------------------
// the status line
// ---------------------------------------------------------------------------

// ungovernedReply is a daemon's answer on a machine where the session reading the line
// started before the hooks were installed.
func ungovernedReply(sessionStarted, hooksAt, daemonAt time.Time, registered bool) statusReply {
	return statusReply{
		V: protocolVersion, Budget: 4, InUse: 0, Thermal: "nominal",
		DaemonStarted:  daemonAt.UTC().Format(time.RFC3339),
		HooksInstalled: hooksAt.UTC().Format(time.RFC3339),
		SessionsKnown:  true,
		Sessions: []sessionStat{{
			Session: sampleSessionID, Live: true, Registered: registered,
			Started: sessionStarted.UTC().Format(time.RFC3339),
			CWD:     "/Users/peterho/Repos/nightcrew",
		}},
	}
}

// THE STATUS LINE HALF OF GOV-29.
//
// A session whose hooks never loaded holds no leases, so before this it rendered "0/4 slots"
// — the same line a fully governed idle session shows. That reading is what a person saw on
// 2026-09-09 while subagents were running, and the conclusion they drew from it was that the
// governor was working. The number was true. It was also useless.
func TestStatuslineSaysWhenThisSessionIsUngoverned(t *testing.T) {
	hooksAt := measuredHooksInstalled
	daemonAt := measuredDaemonStarted

	t.Run("a session that predates the hooks is told to restart", func(t *testing.T) {
		isolateState(t)
		started := hooksAt.Add(-29 * time.Hour)
		endpoint, _, _ := statusDaemon(t, ungovernedReply(started, hooksAt, daemonAt, false))
		got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		if want := "governor: ungoverned - restart to enable"; got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
		// The slot count must be gone, not merely accompanied. Leaving it on the line keeps
		// the misreading available, which is the whole defect.
		if strings.Contains(got, "slots") {
			t.Errorf("line still shows a slot count for an ungoverned session: %q", got)
		}
	})

	t.Run("a governed session holding nothing still reads as slots", func(t *testing.T) {
		isolateState(t)
		started := hooksAt.Add(time.Hour)
		endpoint, _, _ := statusDaemon(t, ungovernedReply(started, hooksAt, daemonAt, true))
		got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		if want := "0/4 slots - thermal ok"; got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
	})

	// The daemon-restart case again, on the surface where getting it wrong is most costly:
	// this line is read continuously, and one that cries "ungoverned" after every upgrade is
	// one a person learns to ignore before the day it is right.
	t.Run("a daemon restart does not make a session read as ungoverned", func(t *testing.T) {
		isolateState(t)
		started := hooksAt.Add(time.Hour)
		restarted := started.Add(time.Hour)
		endpoint, _, _ := statusDaemon(t, ungovernedReply(started, hooksAt, restarted, false))
		got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		if strings.Contains(got, "ungoverned") {
			t.Errorf("a daemon restart was reported as an ungoverned session: %q", got)
		}
	})

	// A daemon that predates GOV-29 sends none of the governance fields. The line must fall
	// back to what it always said rather than accusing a session of being ungoverned on the
	// strength of fields that simply are not there.
	t.Run("an older daemon does not trigger it", func(t *testing.T) {
		isolateState(t)
		endpoint, _, _ := statusDaemon(t, statusReply{
			V: protocolVersion, Budget: 4, InUse: 0, Thermal: "nominal",
			Sessions: []sessionStat{{Session: sampleSessionID}},
		})
		got := runStatuslineFor(t, endpoint, sampleStatusLinePayload, 0)
		if want := "0/4 slots - thermal ok"; got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// what install says on its way out
// ---------------------------------------------------------------------------

// TestInstallNamesTheSessionsThatStayUngoverned.
//
// The line this replaced read "Sessions already running pick the hooks up when they next
// start" — true, and phrased as reassurance, as though it happens by itself. It does not.
// The install that produced GOV-29 printed that sentence and left four sessions ungoverned
// for twenty-nine hours.
func TestInstallNamesTheSessionsThatStayUngoverned(t *testing.T) {
	enumerating := func(live []signals.LiveSession, err error) func(string) ([]signals.LiveSession, error) {
		return func(string) ([]signals.LiveSession, error) { return live, err }
	}

	t.Run("counts and names the sessions that predate the hooks", func(t *testing.T) {
		got := openSessionsNotice(measuredHooksInstalled, enumerating(measuredMachine(), nil))
		if !strings.Contains(got, "4 Claude Code session(s)") {
			t.Errorf("notice does not count them: %q", got)
		}
		if !strings.Contains(got, "UNGOVERNED") || !strings.Contains(got, "restart") {
			t.Errorf("notice does not say what is wrong or what to do: %q", got)
		}
		for _, repo := range []string{"nightcrew", "muncheez", "concertFinder", "budgetr"} {
			if !strings.Contains(got, repo) {
				t.Errorf("notice does not name %s: %q", repo, got)
			}
		}
		// The two that started after the hooks are governed and must not be listed.
		for _, repo := range []string{"claude-governor", "edgefinder-v2"} {
			if strings.Contains(got, repo) {
				t.Errorf("notice tells the user to restart %s, which is governed: %q", repo, got)
			}
		}
	})

	// A reinstall that changed nothing must not tell someone to restart sessions that are
	// already governed. installer.writeFile leaves matching content alone, so the hook
	// file's mtime survives a no-op install and these sessions still postdate it.
	t.Run("a no-op reinstall does not accuse governed sessions", func(t *testing.T) {
		got := openSessionsNotice(measuredHooksInstalled, enumerating(measuredGoverned, nil))
		if strings.Contains(got, "UNGOVERNED") {
			t.Errorf("a no-op reinstall reported governed sessions as ungoverned: %q", got)
		}
		if !strings.Contains(got, "all of them are governed") {
			t.Errorf("notice does not say the machine is clean: %q", got)
		}
	})

	// With no registry to read, it must still warn. The generic sentence is true on every
	// machine; a count would be a guess.
	t.Run("says something true when it cannot count", func(t *testing.T) {
		got := openSessionsNotice(measuredHooksInstalled, enumerating(nil, signals.ErrNoRegistry))
		if !strings.Contains(got, "NOT governed until you restart them") {
			t.Errorf("notice does not warn at all: %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// the spawn tool names, and the claim that expires
// ---------------------------------------------------------------------------

// TestCompareVersions covers the version ordering that the sessions check still uses to
// decide whether a session predates the install. GOV-30 removed the spawn check's use of it:
// comparing a measured-against constant to the running version could only ever say that a
// measurement was old, never that the matcher had stopped firing, and those are the two cases
// that had to be told apart. See surface_test.go for what replaced it.
func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2.1.267", "2.1.267", 0},
		{"2.1.268", "2.1.267", 1},
		{"2.1.266", "2.1.267", -1},
		{"2.2.0", "2.1.267", 1},
		{"3.0.0", "2.1.267", 1},
		{"1.9.9", "2.1.267", -1},
		// Ragged and non-numeric input compares equal rather than guessing: the only thing
		// this drives is whether to add a caveat, and a guess would either cry wolf or stay
		// quiet exactly when it mattered.
		{"2.1", "2.1.267", 0},
		{"2.1.x", "2.1.267", 0},
		{"", "2.1.267", 0},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// With no gate hook installed, "restart these sessions" is a fix that fixes nothing —
// restarting into a machine with no hooks leaves it exactly as ungoverned. The hooks check
// has already failed and already named the real fix, which is precisely what a skip means.
func TestDoctorDoesNotTellYouToRestartWhenNothingIsInstalled(t *testing.T) {
	newWorld(t) // no hooks staged at all
	t.Setenv(transport.EndpointEnv, machineWith(t, measuredMachine(),
		time.Time{}, measuredDaemonStarted, nil))

	code, found, out := doctor(t)
	if code == 0 {
		t.Fatalf("doctor passed with nothing installed:\n%s", out)
	}
	f := want(t, found, "sessions", "skip", out)
	if strings.Contains(f.fix, "restart") {
		t.Errorf("sessions named a restart as the fix on a machine with no hooks: %q", f.fix)
	}
	if !strings.Contains(f.detail, "see hooks above") {
		t.Errorf("skip does not point at the check that caused it: %q", f.detail)
	}
	want(t, found, "hooks", "FAIL", out)
}

// ---------------------------------------------------------------------------
// which file's mtime decides, and the verdicts that must never be faults
// ---------------------------------------------------------------------------

// THE FILE THAT HOLDS THE GATE DECIDES, not the newest file that could hold one.
//
// ~/.claude/settings.json is the USER'S file. Accepting a permission prompt, editing /config
// or installing any other tool rewrites it. Taking the newest mtime across every hook source
// — which this did until it was reviewed — meant an unrelated settings edit moved the install
// instant to now, placed every live session before it, and reported the whole machine as
// ungoverned with a fix telling the user to restart working sessions.
func TestHookInstallTimeIgnoresUnrelatedSettingsEdits(t *testing.T) {
	w := newWorld(t)
	bin := selfBinary(t)
	pluginHooks := filepath.Join(w.home, ".claude", "skills", "build-governor", "hooks", "hooks.json")
	w.writeHooks(pluginHooks, bin, goodHooks())

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(pluginHooks, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// The user's own settings file, with no governor hook in it, written just now.
	if err := os.WriteFile(w.settings, []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	got := gateHooksInstalledAt()
	if got.IsZero() {
		t.Fatal("no gate hook found at all")
	}
	if got.Sub(old).Abs() > time.Minute {
		t.Errorf("hook install time = %v, want the gate file's own mtime %v — an unrelated "+
			"settings edit must not move it", got, old)
	}
}

// And when the gate really is registered in settings.json, that file's mtime is what counts:
// the rule is "the file holding the gate", not "never settings.json".
func TestHookInstallTimeUsesSettingsWhenTheGateLivesThere(t *testing.T) {
	w := newWorld(t)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())

	old := time.Now().Add(-6 * time.Hour)
	if err := os.Chtimes(w.settings, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	got := gateHooksInstalledAt()
	if got.IsZero() || got.Sub(old).Abs() > time.Minute {
		t.Errorf("hook install time = %v, want %v", got, old)
	}
}

func TestClassifyNeverTurnsAnUnreadableFactIntoAFault(t *testing.T) {
	hooks := measuredHooksInstalled.UTC().Format(time.RFC3339)
	started := measuredHooksInstalled.Add(time.Hour)
	base := statusResponse{SessionsKnown: true, HooksInstalled: hooks}
	live := sessionStat{
		Session: "s", Live: true,
		Started: started.UTC().Format(time.RFC3339),
	}

	t.Run("a session holding leases is governed whatever the table says", func(t *testing.T) {
		// Snapshot invents a PID-less entry for a lease whose session it never saw register,
		// so Registered is false while the daemon is holding that session's leases in its
		// hand. Calling that ungoverned contradicts its own evidence.
		r := base
		r.DaemonStarted = started.Add(-time.Hour).UTC().Format(time.RFC3339)
		s := live
		s.Confirmed = 1
		if g := classifyGovernance(r, s); g.ungoverned() {
			t.Errorf("a session holding a confirmed lease was classified %v", g)
		}
	})

	t.Run("an unparseable daemon_started is unknown, not a fault", func(t *testing.T) {
		r := base
		// RFC3339 without a zone: what another build, or a future one, might send.
		r.DaemonStarted = "2026-09-09T23:09:01"
		if g := classifyGovernance(r, live); g.ungoverned() {
			t.Errorf("an unparseable daemon_started produced %v; an unreadable input must reach unknown", g)
		}
	})

	t.Run("a session too young to have registered yet is unknown", func(t *testing.T) {
		r := base
		r.DaemonStarted = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		s := live
		s.Started = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
		if g := classifyGovernance(r, s); g.ungoverned() {
			t.Errorf("a one-second-old session was classified %v; its SessionStart may still be in flight", g)
		}
	})

	// The complement: past the grace window, with a daemon older than the session and no
	// leases, it IS a fault. Without this the three subtests above could be satisfied by a
	// classify that never reports anything.
	t.Run("past the grace window it is still a fault", func(t *testing.T) {
		r := base
		r.DaemonStarted = measuredHooksInstalled.Add(30 * time.Minute).UTC().Format(time.RFC3339)
		s := live
		s.Started = measuredHooksInstalled.Add(time.Hour).UTC().Format(time.RFC3339)
		if g := classifyGovernance(r, s); !g.ungoverned() {
			t.Errorf("classify = %v, want an ungoverned verdict", g)
		}
	})
}

// Restarting is the only fix an ungoverned session has, and it is not an action that exists
// for a headless `claude -p` run — which will have exited before anyone reads the advice.
func TestOnlyInteractiveSessionsAreToldToRestart(t *testing.T) {
	headless := measuredUngoverned[0]
	headless.Kind = "print"
	headless.CWD = "/Users/peterho/Repos/batchjob"
	interactive := measuredUngoverned[1] // Kind unset, i.e. cannot tell — treated as interactive

	t.Run("doctor does not name a headless session", func(t *testing.T) {
		stageWorkingInstall(t)
		t.Setenv(transport.EndpointEnv, machineWith(t,
			[]signals.LiveSession{headless, interactive},
			measuredHooksInstalled, measuredDaemonStarted, nil))

		_, found, out := doctor(t)
		f := want(t, found, "sessions", "FAIL", out)
		if strings.Contains(f.detail, "batchjob") || strings.Contains(f.fix, "batchjob") {
			t.Errorf("a headless session was named as something to restart: %q / %q", f.detail, f.fix)
		}
		// The interactive one still must be, or this filter has swallowed the finding.
		if !strings.Contains(f.detail, "muncheez") {
			t.Errorf("the interactive session was lost along with the headless one: %q", f.detail)
		}
	})

	t.Run("install does not name a headless session", func(t *testing.T) {
		got := openSessionsNotice(measuredHooksInstalled, func(string) ([]signals.LiveSession, error) {
			return []signals.LiveSession{headless, interactive}, nil
		})
		if strings.Contains(got, "batchjob") {
			t.Errorf("install told the user to restart a headless run: %q", got)
		}
		if !strings.Contains(got, "muncheez") {
			t.Errorf("install lost the interactive session too: %q", got)
		}
	})
}
