package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// firstInstall is the history of a machine whose only gate was installed at at: the world
// every GOV-29 test was written about.
func firstInstall(at time.Time) []hookEpoch {
	return []hookEpoch{{At: at.UTC().Format(time.RFC3339), Hooks: currentHookNames()}}
}

func firstInstallHistory(at time.Time) func() ([]hookEpoch, bool) {
	return func() ([]hookEpoch, bool) {
		if at.IsZero() {
			return nil, false
		}
		return firstInstall(at), true
	}
}

// currentHookNames is wantedHooks as the history spells it.
func currentHookNames() []string {
	out := make([]string, 0, len(wantedHooks))
	for _, w := range wantedHooks {
		out = append(out, w.event+" "+w.args)
	}
	return out
}

// without is names less one.
func without(names []string, drop string) []string {
	var out []string
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}

// The machine as it was on 2026-11-23, measured, which is what GOV-B52 was filed from.
//
// 0.1.6 installed its hooks at 2026-11-15 03:11 UTC (daemon.log: "0.1.6 build 78ae6a98f019"
// at 22:11:49 EST on the 14th). The earbrief session started at 16:38 UTC that day
// (~/.claude/sessions, startedAt 1794760707418) and ran builds through its gate — daemon.log
// has its release lines on 11-21. 0.1.7 rewrote hooks.json at 06:11:53 UTC on 11-22, adding
// the UserPromptSubmit settle hook, and that mtime is what hooks_installed still reports. The
// 0.1.8 daemon came up at 17:32:42 UTC on 11-23 and doctor then FAILed earbrief as
// "ungoverned (started before the hooks were installed)".
var (
	b52HooksV016      = time.Date(2026, 11, 15, 3, 11, 48, 0, time.UTC)
	b52HooksV017      = time.Date(2026, 11, 22, 6, 11, 53, 0, time.UTC)
	b52DaemonStarted  = time.Date(2026, 11, 23, 17, 32, 42, 0, time.UTC)
	b52EarbriefOnV016 = signals.LiveSession{
		ID: "4641a070-b488-444b-968d-918388c4bd69", PID: 79705, CWD: "/Users/peterho/Repos/earbrief",
		Started: time.Date(2026, 11, 15, 16, 38, 27, 0, time.UTC), Version: "2.1.325", Kind: "interactive",
	}
	// A session from before any gate existed on this machine, for case (a).
	b52Prehistoric = signals.LiveSession{
		ID: "9f0c1d2e-0000-4000-8000-00000000b52a", PID: 1234, CWD: "/Users/peterho/Repos/nightcrew",
		Started: b52HooksV016.Add(-6 * time.Hour), Version: "2.1.320", Kind: "interactive",
	}
	// A session started after the latest rewrite that registered: case (c).
	b52Current = signals.LiveSession{
		ID: "c0ffee00-0000-4000-8000-00000000b52c", PID: 4321, CWD: "/Users/peterho/Repos/claude-governor",
		Started: b52DaemonStarted.Add(time.Hour), Version: "2.1.330", Kind: "interactive",
	}
)

// b52History is the history install would have kept, had it kept one from 0.1.6 on.
func b52History() []hookEpoch {
	return []hookEpoch{
		{At: b52HooksV016.Format(time.RFC3339), Hooks: without(currentHookNames(), "UserPromptSubmit settle")},
		{At: b52HooksV017.Format(time.RFC3339), Hooks: currentHookNames()},
	}
}

// b52Machine is machineWith with the hooks history stated outright.
func b52Machine(t *testing.T, live []signals.LiveSession, hist []hookEpoch, registered []string) string {
	t.Helper()
	endpoint, d := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.liveSessions = func() ([]signals.LiveSession, error) { return live, nil }
		d.hooksInstalledAt = func() time.Time { return b52HooksV017 }
		d.hookHistory = func() ([]hookEpoch, bool) { return hist, len(hist) > 0 }
		d.started = b52DaemonStarted
		for _, s := range live {
			d.surface.note(s.Version, "Agent")
		}
		loadsItsConfig(d)
	})
	for _, id := range registered {
		for _, s := range live {
			if s.ID == id {
				d.table.StartSession(lease.Session{ID: id, PID: s.PID, Weight: 1})
			}
		}
	}
	return endpoint
}

// statusOf runs `governor status` against the endpoint in the environment.
func statusOf(t *testing.T) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	runStatus(nil, &stdout, &stderr)
	return stdout.String()
}

// THE TEST THE STORY ASKED FOR (clause 1): a session that loaded the previous release's
// hooks is not called ungoverned by either surface.
func TestASessionOnAnEarlierInstallsHooksIsNotUngoverned(t *testing.T) {
	stageWorkingInstall(t)
	t.Setenv(transport.EndpointEnv, b52Machine(t,
		[]signals.LiveSession{b52EarbriefOnV016, b52Current}, b52History(), []string{b52Current.ID}))

	code, found, out := doctor(t)
	f := want(t, found, "sessions", "warn", out)
	if code != 0 {
		t.Errorf("doctor exited %d over a governed session:\n%s", code, out)
	}
	if strings.Contains(f.detail, "ungoverned") || strings.Contains(f.detail, "started before the hooks were installed") {
		t.Errorf("doctor calls the 0.1.6 session ungoverned: %q", f.detail)
	}
	// Clause 2(b): it names what the session lacks, and that a restart picks it up.
	for _, s := range []string{"earbrief", "older set of the governor's hooks", "UserPromptSubmit settle"} {
		if !strings.Contains(f.detail, s) {
			t.Errorf("detail does not say %q: %q", s, f.detail)
		}
	}
	if !strings.Contains(f.fix, "restart") || !strings.Contains(f.fix, "earbrief") {
		t.Errorf("fix does not say a restart of earbrief picks the hooks up: %q", f.fix)
	}
	if strings.Contains(f.fix, "claude-governor") {
		t.Errorf("fix sweeps in the current session: %q", f.fix)
	}

	st := statusOf(t)
	if strings.Contains(st, "ungoverned") {
		t.Errorf("governor status calls the 0.1.6 session ungoverned:\n%s", st)
	}
	if !strings.Contains(st, "older set of the governor's hooks") || !strings.Contains(st, "UserPromptSubmit settle") {
		t.Errorf("governor status does not say the session is on older hooks and what it lacks:\n%s", st)
	}
}

// Clause 2(a): the history must not quieten the case GOV-29 exists for.
func TestASessionOlderThanEveryGateIsStillUngoverned(t *testing.T) {
	stageWorkingInstall(t)
	t.Setenv(transport.EndpointEnv, b52Machine(t,
		[]signals.LiveSession{b52Prehistoric, b52EarbriefOnV016, b52Current}, b52History(), []string{b52Current.ID}))

	code, found, out := doctor(t)
	if code == 0 {
		t.Fatalf("doctor passed with a session that predates every gate:\n%s", out)
	}
	f := want(t, found, "sessions", "FAIL", out)
	if !strings.Contains(f.detail, "1 of 3") || !strings.Contains(f.detail, "nightcrew (started before the hooks were installed)") {
		t.Errorf("detail does not name the one ungoverned session: %q", f.detail)
	}
	// The older-hooks one is reported beside it, in its own words, not counted as ungoverned.
	if !strings.Contains(f.detail, "earbrief (missing the UserPromptSubmit settle hook)") {
		t.Errorf("detail lost the older-hooks session: %q", f.detail)
	}
	if strings.Contains(f.fix, "earbrief") {
		t.Errorf("the FAIL's fix names the governed earbrief session: %q", f.fix)
	}
	if st := statusOf(t); !strings.Contains(st, "ungoverned (started before the hooks were installed)") {
		t.Errorf("governor status does not mark the prehistoric session:\n%s", st)
	}
}

// Clause 3: when the evidence cannot be read, the verdict is unknown, never ungoverned.
func TestNoHooksHistoryIsUnknownNotUngoverned(t *testing.T) {
	for name, hist := range map[string][]hookEpoch{
		"no history at all": nil,
		// The upgrade from a build before GOV-B52: install seeds from the file it found, and
		// cannot say what came before it.
		"a history seeded after the fact": {{At: b52HooksV017.Format(time.RFC3339), Hooks: currentHookNames(), Seeded: true}},
	} {
		t.Run(name, func(t *testing.T) {
			stageWorkingInstall(t)
			t.Setenv(transport.EndpointEnv, b52Machine(t,
				[]signals.LiveSession{b52EarbriefOnV016, b52Current}, hist, []string{b52Current.ID}))
			code, found, out := doctor(t)
			if f := found["sessions"]; f.state == "FAIL" || strings.Contains(f.detail, "ungoverned") {
				t.Errorf("sessions: %s %q, want no fault from missing evidence (exit %d)\n%s", f.state, f.detail, code, out)
			}
			if st := statusOf(t); strings.Contains(st, "ungoverned") {
				t.Errorf("governor status calls it ungoverned:\n%s", st)
			}
		})
	}
}

func TestClassifyBeforeTheLatestHooks(t *testing.T) {
	reply := statusResponse{
		SessionsKnown: true, HooksInstalled: b52HooksV017.Format(time.RFC3339),
		DaemonStarted: b52DaemonStarted.Format(time.RFC3339),
		HookHistory:   b52History(), HookHistoryKnown: true,
	}
	at := func(t time.Time) sessionStat {
		return sessionStat{Session: "s", Live: true, Started: t.Format(time.RFC3339)}
	}
	cases := []struct {
		name    string
		started time.Time
		want    governance
	}{
		{"before every gate", b52HooksV016.Add(-time.Minute), govNeverLoaded},
		{"exactly at the first install", b52HooksV016, govOlderHooks},
		{"between the two writes", b52EarbriefOnV016.Started, govOlderHooks},
		// After the latest write it is the GOV-29 path as before: registered with a replaced
		// daemon, here.
		{"after the latest write", b52HooksV017.Add(time.Hour), govPreviousDaemon},
	}
	for _, c := range cases {
		if got := classifyGovernance(reply, at(c.started)); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}
	if m := missingHooksFor(reply, at(b52EarbriefOnV016.Started)); len(m) != 1 || m[0] != "UserPromptSubmit settle" {
		t.Errorf("missing = %v, want [UserPromptSubmit settle]", m)
	}
	if govOlderHooks.ungoverned() {
		t.Error("govOlderHooks reads as ungoverned")
	}
}

// The install notice, same rule: an upgrade does not tell the user their sessions are
// ungoverned.
func TestInstallNoticeTellsAnUpgradeFromAnInstall(t *testing.T) {
	live := []signals.LiveSession{b52Prehistoric, b52EarbriefOnV016}
	got := openSessionsNoticeFrom(b52History(), b52HooksV017, func(string) ([]signals.LiveSession, error) { return live, nil })
	if !strings.Contains(got, "1 Claude Code session(s) are running now and will stay UNGOVERNED until restarted: nightcrew") {
		t.Errorf("notice lost the prehistoric session:\n%s", got)
	}
	if !strings.Contains(got, "keep the ones they loaded until restarted: earbrief") {
		t.Errorf("notice does not put earbrief on older hooks:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// what install records
// ---------------------------------------------------------------------------

// hooksFile writes a plugin hooks.json holding names (a subset of wantedHooks) and gives it
// mtime at.
func hooksFile(t *testing.T, path string, drop []string, at time.Time) {
	t.Helper()
	raw, err := pluginHooks()
	if err != nil {
		t.Fatal(err)
	}
	if len(drop) > 0 {
		var doc map[string]map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, ev := range drop {
			delete(doc["hooks"], ev)
		}
		if raw, err = json.Marshal(doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestInstallKeepsTheHooksHistory(t *testing.T) {
	newWorld(t) // an isolated HOME, so gateHooksInstalledAt sees only what this test writes
	p, err := resolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()

	t.Run("a first install is unseeded", func(t *testing.T) {
		before := observeHookFileBefore(p.hooks, p.plugin)
		if before.anyGate {
			t.Fatal("the isolated world already has a gate")
		}
		hooksFile(t, p.hooks, []string{"UserPromptSubmit"}, b52HooksV016)
		if wrote, err := recordHookHistory(state, p.hooks, p.plugin, before); err != nil || !wrote {
			t.Fatalf("record = %v, %v", wrote, err)
		}
		h, err := readHookHistory(state)
		if err != nil || len(h) != 1 || h[0].Seeded || h[0].At != b52HooksV016.Format(time.RFC3339) {
			t.Fatalf("history = %+v, %v; want one unseeded entry at the write", h, err)
		}
	})

	t.Run("a reinstall that changed nothing writes nothing", func(t *testing.T) {
		before := observeHookFileBefore(p.hooks, p.plugin)
		if wrote, err := recordHookHistory(state, p.hooks, p.plugin, before); err != nil || wrote {
			t.Fatalf("record = %v, %v; want a no-op", wrote, err)
		}
	})

	t.Run("an upgrade that changed the hooks appends", func(t *testing.T) {
		before := observeHookFileBefore(p.hooks, p.plugin)
		hooksFile(t, p.hooks, nil, b52HooksV017)
		if _, err := recordHookHistory(state, p.hooks, p.plugin, before); err != nil {
			t.Fatal(err)
		}
		h, _ := readHookHistory(state)
		if len(h) != 2 || h[1].At != b52HooksV017.Format(time.RFC3339) {
			t.Fatalf("history = %+v, want the 0.1.7 write appended", h)
		}
		reply := statusResponse{HookHistory: h, HookHistoryKnown: true}
		if m := missingHooks(reply.HookHistory, h[0]); len(m) != 1 || m[0] != "UserPromptSubmit settle" {
			t.Errorf("missing = %v, want the settle hook", m)
		}
	})

	t.Run("an install over a gate it never saw written is seeded", func(t *testing.T) {
		fresh := t.TempDir()
		before := observeHookFileBefore(p.hooks, p.plugin) // the 0.1.7 file is already there
		hooksFile(t, p.hooks, nil, b52DaemonStarted)
		if _, err := recordHookHistory(fresh, p.hooks, p.plugin, before); err != nil {
			t.Fatal(err)
		}
		h, _ := readHookHistory(fresh)
		if len(h) != 2 || !h[0].Seeded || h[0].At != b52HooksV017.Format(time.RFC3339) || h[1].Seeded {
			t.Fatalf("history = %+v, want a seeded entry for the old file, then the new write", h)
		}
	})

	t.Run("a malformed history is unreadable, not empty", func(t *testing.T) {
		bad := t.TempDir()
		os.WriteFile(filepath.Join(bad, hookHistoryFile), []byte("{not json"), 0o644)
		if _, err := readHookHistory(bad); err == nil {
			t.Error("a malformed history read cleanly")
		}
	})
}

// Clause 3's uninstall half. The byte-identical test walks the staged HOME and the state
// directory is staged outside it, so the history needs its own proof: install writes it, and
// uninstall takes it — and its staging file — with the rest.
func TestUninstallRemovesTheHooksHistory(t *testing.T) {
	p := stagedHome(t)
	installOK(t)

	path := filepath.Join(p.state, hookHistoryFile)
	h, err := readHookHistory(p.state)
	if err != nil || len(h) != 1 || h[0].Seeded {
		t.Fatalf("after a first install, history = %+v, %v; want one unseeded entry", h, err)
	}
	// A staging file left by an install killed between write and rename.
	writeFileT(t, filepath.Join(p.state, hookHistoryTempFile), "[")

	// Install twice changes nothing the second time, history included.
	fi, _ := os.Stat(path)
	installOK(t)
	if again, _ := os.Stat(path); again == nil || !again.ModTime().Equal(fi.ModTime()) {
		t.Errorf("a second install rewrote %s", path)
	}

	uninstallOK(t)
	for _, f := range []string{path, filepath.Join(p.state, hookHistoryTempFile)} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s behind; err = %v", f, err)
		}
	}
	if _, err := os.Lstat(p.state); !os.IsNotExist(err) {
		t.Errorf("state directory %s survived uninstall; err = %v", p.state, err)
	}
}
