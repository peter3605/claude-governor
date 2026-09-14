package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The one-time first-run notice (GOV-22).
//
// What these hold is the difference between a tool people keep and a tool people uninstall
// on principle: that it says, once, what it has started doing to their machine and how to
// make it stop — and that "once" is true even with sessions starting in parallel, and that
// what it says about the status line is still what the status line prints.

// pluginData points ${CLAUDE_PLUGIN_DATA} at a directory this test owns, which is where the
// marker resolves in a real plugin context.
func pluginData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.DataDirEnv, dir)
	return dir
}

// notice runs the emission once and returns what it wrote to stdout.
func notice(t *testing.T) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	emitFirstRunNotice(&stdout, &stderr)
	if s := stderr.String(); s != "" {
		t.Logf("stderr: %s", s)
	}
	return stdout.String()
}

// The first session is told, and no session after it is.
//
// The second half is the whole point. A notice that reappears every session is not a notice,
// it is the tool talking over its user, and it is worse than never having explained itself.
func TestTheFirstSessionIsToldAndNoSessionAfterItIs(t *testing.T) {
	data := pluginData(t)

	if got := notice(t); got == "" {
		t.Fatal("the first session was told nothing")
	}
	if _, err := os.Stat(filepath.Join(data, onboardMarker)); err != nil {
		t.Errorf("the first session printed the notice without recording that it had: %v", err)
	}
	for i := 2; i <= 4; i++ {
		if got := notice(t); got != "" {
			t.Errorf("session %d was told again: %s", i, got)
		}
	}
}

// The notice reaches the PERSON, and puts nothing in the model's context.
//
// docs/hook-surface.md section 16 measured the two to be disjoint channels: systemMessage is
// rendered to the user and contributes literally nothing to the model, additionalContext is
// the exact reverse. Sending this as additionalContext would mean nobody but the model ever
// reads it, and would also make it a standing instruction the model carries for the rest of
// the session — which is the story's own trap and GOV-17's job, not this one's.
func TestTheNoticeGoesToThePersonAndNotToTheModel(t *testing.T) {
	pluginData(t)

	var got map[string]any
	if err := json.Unmarshal([]byte(notice(t)), &got); err != nil {
		t.Fatalf("the notice is not parseable JSON, so Claude Code would report a hook error "+
			"instead of showing it: %v", err)
	}
	if _, ok := got["systemMessage"].(string); !ok {
		t.Errorf("the notice carries no systemMessage, which is the only field the user sees: %v", got)
	}
	if _, ok := got["hookSpecificOutput"]; ok {
		t.Error("the notice carries hookSpecificOutput, which puts it in the model's context " +
			"and not in front of the user (docs/hook-surface.md section 16)")
	}
	if len(got) != 1 {
		t.Errorf("the notice carries %d fields, want only systemMessage: %v", len(got), sortedKeys(boolKeys(got)))
	}
}

// It leads with how to turn it off, and the command it names is real.
//
// Lead with the off switch: a tool that silently changes when your agents run and does not
// say how to stop it is the kind of thing people remove on principle, and burying that under
// two paragraphs of what it does for them is the same as not saying it.
func TestTheNoticeLeadsWithHowToTurnItOff(t *testing.T) {
	pluginData(t)
	msg := systemMessageOf(t, notice(t))

	first, _, _ := strings.Cut(msg, "\n")
	if !strings.Contains(first, "governor uninstall") {
		t.Errorf("the first line does not say how to turn it off: %q", first)
	}
	if !registered("uninstall") {
		t.Error("the notice names `governor uninstall`, which is not a subcommand of this binary")
	}
	// The measured caps, docs/hook-surface.md section 16.3. Well clear of both rather than
	// near them: a truncated notice loses whichever part it was cut before.
	if lines := strings.Count(msg, "\n") + 1; lines > 20 {
		t.Errorf("the notice is %d lines; Claude Code truncates systemMessage at 20", lines)
	}
	if len(msg) > 4000 {
		t.Errorf("the notice is %d characters; Claude Code truncates systemMessage at 4000", len(msg))
	}
}

// Everything the notice says about the status line is what the status line actually renders.
//
// This is the assertion that stops the notice quietly becoming a lie. The status line is a
// row of numbers that means nothing unexplained, so the notice explains it by example — and
// an example that drifts out of date teaches a stranger to read a line that no longer exists.
// design.md section 9 illustrated the line with a leading glyph and middle-dot separators for
// a year after GOV-10 shipped it with neither, which is exactly the kind of drift this
// catches — and GOV-B09 had to catch it there too, in
// TestTheDocsIllustrateTheStatusLineThatActuallyRenders.
func TestTheNoticeDescribesTheStatusLineThatActuallyRenders(t *testing.T) {
	cpu := 0.61
	live := render(statusView{
		OK: true, InUse: 3, Slots: 3, Budget: 5, Mine: 1, CPU: &cpu, Thermal: signals.Nominal,
	}, 0)

	for _, quoted := range []string{"3/5 slots", "(1 yours)", "cpu 61%", "thermal ok"} {
		if !strings.Contains(onboardingNotice, quoted) {
			t.Errorf("the notice does not explain %q, which the status line prints", quoted)
		}
		if !strings.Contains(live, quoted) {
			t.Errorf("the notice quotes %q, but the status line renders %q", quoted, live)
		}
	}

	// The daemon-down line, quoted verbatim, because it is the one a confused person will be
	// looking at when they go and read this.
	if down := render(statusView{}, 0); !strings.Contains(onboardingNotice, down) {
		t.Errorf("the notice does not quote the daemon-down line, which renders as %q", down)
	}

	// And the ungoverned line (GOV-29), for the same reason and more so: it is the one line
	// that asks the reader to do something, and a notice that explained a line the renderer
	// no longer prints would send them looking for text that never appears.
	ungoverned := render(statusView{OK: true, Ungoverned: true}, 0)
	if !strings.Contains(onboardingNotice, ungoverned) {
		t.Errorf("the notice does not quote the ungoverned line, which renders as %q", ungoverned)
	}
	if !strings.Contains(onboardingNotice, "restarting") {
		t.Errorf("the notice quotes the ungoverned line without saying what to do about it")
	}
}

// It stays silent when it cannot record itself, rather than repeating forever.
//
// And it does not create the directory to get around that. daemon.start and statusline.json
// follow the same rule for the same reason (design.md section 13.3): a hook that put a
// directory back would leave residue moments after the one command that promises to remove
// everything.
func TestTheNoticeIsSilentWhenItCannotRecordItself(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "not-created-by-us")
	t.Setenv(config.DataDirEnv, absent)

	if got := notice(t); got != "" {
		t.Errorf("printed a notice it had no way to record, so it would print again every "+
			"session forever: %s", got)
	}
	if _, err := os.Stat(absent); err == nil {
		t.Error("created the data directory, which uninstall would then have to remove twice")
	}
}

// Sessions starting at once are told once between them, not once each.
//
// Parallel session starts are ordinary — opening three terminals is enough — and a
// check-then-write would have every one of them find no marker and every one of them print.
// The claim is the O_EXCL create itself, which exactly one caller can win.
func TestSimultaneousSessionsAreToldOnceBetweenThem(t *testing.T) {
	pluginData(t)

	const sessions = 16
	var wg sync.WaitGroup
	shown := make([]string, sessions)
	start := make(chan struct{})
	for i := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stdout, stderr bytes.Buffer
			<-start
			emitFirstRunNotice(&stdout, &stderr)
			shown[i] = stdout.String()
		}()
	}
	close(start)
	wg.Wait()

	told := 0
	for _, s := range shown {
		if s != "" {
			told++
		}
	}
	if told != 1 {
		t.Errorf("%d of %d simultaneous sessions printed the notice, want exactly 1", told, sessions)
	}
}

// `session end` never shows it, and neither does a `session` with no action.
//
// Through the real subcommand rather than the emission, because the gate being tested is the
// wiring: runSession decides from its argument, and an install that wired the hooks without
// one must not greet a person as their session closes.
func TestOnlySessionStartShowsTheNotice(t *testing.T) {
	isolateRealState(t)
	pluginData(t)

	for _, args := range [][]string{{"session", "end"}, {"session"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Errorf("governor %s: exit %d, want 0", strings.Join(args, " "), code)
		}
		if stdout.Len() != 0 {
			t.Errorf("governor %s wrote %q to stdout", strings.Join(args, " "), stdout.String())
		}
	}

	// And the one that does, so this test cannot pass by the notice being broken outright.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"session", "start"}, &stdout, &stderr); code != 0 {
		t.Errorf("governor session start: exit %d, want 0", code)
	}
	if stdout.Len() == 0 {
		t.Error("governor session start printed no notice on a first run")
	}
}

// systemMessageOf pulls the user-facing text out of an emitted notice.
func systemMessageOf(t *testing.T, raw string) string {
	t.Helper()
	var got struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("parse the notice: %v (%q)", err, raw)
	}
	return got.SystemMessage
}

// boolKeys adapts a decoded object to the set sortedKeys takes.
func boolKeys(m map[string]any) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// sessionStartCeiling is what `governor session start` is allowed to cost in total.
//
// SessionStart is the one hook that now spends two budgets: the notice's claim, then the
// daemon exchange. That is deliberate (see firstRunNoticeBudget) but it doubles the worst
// case, and a doubling nothing asserts is a doubling that keeps going. The lifecycle tests
// drive lifecycleHook directly and so cannot see it — this is the only place the whole
// subcommand is timed.
//
// It is asserted through timed, which discounts what a loaded runner cost rather than what
// the subcommand did. latency_test.go says why, and why neither half is the thing to raise.
const sessionStartCeiling = failOpenCeiling + firstRunNoticeBudget

// `governor session start` exits 0 inside its ceiling with nothing listening.
//
// Both halves must fail open independently: no daemon, and a notice that may or may not have
// anything to claim. A session that hangs while a governor decides whether to introduce
// itself is the worst possible first impression, and it is the one this whole file could
// plausibly have introduced.
func TestSessionStartFailsOpenInsideItsCeiling(t *testing.T) {
	t.Setenv(transport.EndpointEnv, "unix://"+sockDir(t)+"/absent")

	for _, tc := range []struct {
		name string
		data func(t *testing.T)
	}{
		{"with a notice to show", func(t *testing.T) { pluginData(t) }},
		{"with the notice already shown", func(t *testing.T) {
			dir := pluginData(t)
			writeFileT(t, filepath.Join(dir, onboardMarker), "already\n")
		}},
		{"with nowhere to record it", func(t *testing.T) {
			t.Setenv(config.DataDirEnv, filepath.Join(t.TempDir(), "absent"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.data(t)
			var stdout, stderr bytes.Buffer
			var code int
			took := timed(func() {
				code = run([]string{"session", "start"}, &stdout, &stderr)
			})

			if code != 0 {
				t.Errorf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
			}
			if took.exceeds(sessionStartCeiling) {
				t.Errorf("took %v, want under %v", took, sessionStartCeiling)
			}
		})
	}
}
