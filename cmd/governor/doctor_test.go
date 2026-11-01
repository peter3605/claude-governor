package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// Tests for `governor doctor` (GOV-20).
//
// The property that matters most here is not that any individual check is right — it is that
// no check can fail without naming a fix. That is the whole of design.md section 13.1's
// "diagnose, do not describe", and TestEveryFailureNamesACommandToRun asserts it across
// every broken machine this file knows how to build, so a check added later cannot quietly
// go back to describing.
//
// Everything runs against a staged machine: a temporary HOME, a temporary state directory,
// and an endpoint pointed somewhere disposable. Nothing here may read or write the
// developer's own ~/.claude — doctor stats paths, reads settings and execs a binary, and a
// suite that did any of that to a real install would be reporting on the developer's laptop
// rather than on the code.

// world is a staged machine for doctor to inspect.
type world struct {
	t          *testing.T
	home       string
	settings   string
	stateDir   string
	pluginRoot string
}

// newWorld stages an empty machine: no daemon, no hooks, no config.
func newWorld(t *testing.T) *world {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Cleared rather than left alone: a developer running the suite inside a plugin context
	// would otherwise have their own CLAUDE_PLUGIN_ROOT expand the hook commands here.
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	t.Setenv(config.DataDirEnv, "")

	// Short, because the endpoint under it has to fit in sockaddr_un.
	stateDir := sockDir(t)
	t.Setenv(transport.StateDirEnv, stateDir)
	t.Setenv(transport.EndpointEnv, "unix://"+filepath.Join(stateDir, "absent"))

	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return &world{
		t:          t,
		home:       home,
		settings:   filepath.Join(home, ".claude", "settings.json"),
		stateDir:   stateDir,
		pluginRoot: filepath.Join(home, ".claude", "plugins", "claude-governor"),
	}
}

// hookSpec is one hook registration to stage.
type hookSpec struct{ event, matcher, args string }

// goodHooks are the six registrations of design.md section 4.1, correctly wired.
//
// It is built FROM wantedHooks rather than written out, for the reason pluginHooks is: a
// fixture carrying its own private copy of the right answer is a fixture that keeps passing
// after the right answer changes. GOV-15 added a sixth hook and this list did not notice,
// which is how that was discovered.
func goodHooks() []hookSpec {
	var out []hookSpec
	for _, w := range wantedHooks {
		out = append(out, hookSpec{
			event:   w.event,
			matcher: strings.Join(w.covers, "|"),
			args:    w.args,
		})
	}
	return out
}

// writeHooks stages hook registrations into path, as either a settings.json or a plugin
// hooks.json — the two files have the same shape and doctor reads both.
func (w *world) writeHooks(path, bin string, specs []hookSpec) {
	w.t.Helper()
	hooks := map[string][]map[string]any{}
	for _, s := range specs {
		entry := map[string]any{
			"hooks": []any{map[string]any{
				"type": "command", "command": bin + " " + s.args, "timeout": 30,
			}},
		}
		if s.matcher != "" {
			entry["matcher"] = s.matcher
		}
		hooks[s.event] = append(hooks[s.event], entry)
	}
	body, err := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	if err != nil {
		w.t.Fatalf("encode hooks: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		w.t.Fatalf("write %s: %v", path, err)
	}
}

// writeConfig stages a config.json at the path config.Path resolves to.
// writeCalibration gives the world a config of the shape `governor calibrate` writes.
//
// A FULLY WORKING INSTALL IS A CALIBRATED ONE, which is why this belongs in the fixtures that
// assert every check passes rather than only in the calibration tests. `governor install` ends
// by running `calibrate` (GOV-25), so a machine that has been installed and not calibrated is
// not the ordinary state — it is the state of a machine whose calibration could not run, and
// doctor is supposed to say so. A fixture that hand-assembles a working install has to
// assemble that part too, or it is testing a machine no user has.
func (w *world) writeCalibration() {
	w.writeConfig(fmt.Sprintf(
		`{"schema": %d, "run_queue_max": 3.42, "b_max": 4, "calibrated_at": "2026-09-16T00:00:00Z", "calibrated_from": "governor calibrate 2026-09-16T00:00:00Z; %d cores (4 performance, 4 efficiency); ambient run queue 1.77 per core"}`,
		configSchemaVersion, runtime.NumCPU()))
}

func (w *world) writeConfig(body string) {
	w.t.Helper()
	path, err := config.Path()
	if err != nil {
		w.t.Fatalf("config path: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		w.t.Fatalf("write config: %v", err)
	}
}

// selfBinary is this test binary's own path.
//
// It stands in for an installed governor, and it is the right stand-in rather than a
// convenient one: it is a real executable whose `version` output is this build's, so a hook
// pointing at it is exactly the "the hooks run this binary" case. `go test` names it
// governor.test, which isGovernorBinary recognises for the same reason it recognises
// governor-darwin-arm64.
func selfBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	abs, err := filepath.Abs(self)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// fakeGovernor writes an executable that answers `version` with the three lines given.
func fakeGovernor(t *testing.T, dir, name, binary string, protocol, schema int) string {
	t.Helper()
	return markedGovernor(t, dir, name, binary, protocol, schema, "")
}

// markedGovernor is fakeGovernor with a mark that changes the file's BYTES and none of its
// numbers.
//
// It exists because two fakeGovernors reporting the same version are byte-identical, and the
// case GOV-B13 is about is two builds that answer `version` identically and are not the same
// build. Without a way to differ in content alone, the only staleness a test could stage is
// the version-string kind — the kind that already worked.
func markedGovernor(t *testing.T, dir, name, binary string, protocol, schema int, mark string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := fmt.Sprintf("#!/bin/sh\necho \"governor %s\"\necho \"protocol %d\"\necho \"config schema %d\"\n",
		binary, protocol, schema)
	if mark != "" {
		// A comment: it changes the digest and cannot change a line of the output.
		body += "# " + mark + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// copyOfSelf writes a byte-identical copy of this test binary at a fresh path.
//
// The point is a file that is NOT the same inode as the running executable but holds exactly
// its bytes — the "hooks run the current build, installed at its own path" case, which is
// what a correct install looks like from doctor's side and which sameFile alone cannot see.
func copyOfSelf(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(selfBinary(t))
	if err != nil {
		t.Fatalf("read self: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	return path
}

// line is one parsed report line.
type line struct {
	state  string
	detail string
	fix    string
}

// Every state label the report can print. A state missing from this alternation does not
// fail loudly — every line in it is silently dropped from the parsed map, and the test then
// complains that a check is absent from a report it is plainly in. Add the label here in the
// same change that adds the state.
var findingRE = regexp.MustCompile(`^(ok|FAIL|skip|warn|unknown)\s+(\S+)\s+(.*)$`)

// doctor runs the command against whatever the world currently holds and parses its report.
func doctor(t *testing.T) (int, map[string]line, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor"}, &stdout, &stderr)
	out := stdout.String()

	found := map[string]line{}
	var order []string
	for _, raw := range strings.Split(out, "\n") {
		if strings.HasPrefix(raw, strings.Repeat(" ", fixIndent)) {
			// A fix line, belonging to the finding above it.
			if len(order) == 0 {
				t.Fatalf("fix line with no finding above it:\n%s", out)
			}
			name := order[len(order)-1]
			f := found[name]
			f.fix = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "fix"))
			found[name] = f
			continue
		}
		m := findingRE.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		if _, dup := found[m[2]]; dup {
			t.Fatalf("two findings named %q; the report has to be readable:\n%s", m[2], out)
		}
		found[m[2]] = line{state: m[1], detail: m[3]}
		order = append(order, m[2])
	}
	if len(found) == 0 {
		t.Fatalf("doctor printed no findings (stderr: %s)\n%s", stderr.String(), out)
	}
	return code, found, out
}

// want asserts one check's state, and returns its line for further assertions.
func want(t *testing.T, found map[string]line, name, state, out string) line {
	t.Helper()
	got, ok := found[name]
	if !ok {
		t.Fatalf("no %q check in the report:\n%s", name, out)
	}
	if got.state != state {
		t.Fatalf("%s: %s (%s), want %s\n%s", name, got.state, got.detail, state, out)
	}
	return got
}

// ---------------------------------------------------------------------------
// the property this command exists for
// ---------------------------------------------------------------------------

// TestEveryFailureNamesACommandToRun is the acceptance criterion itself.
//
// design.md section 13.1: doctor diagnoses, it does not describe. A failing check that ends
// in a statement of fact leaves the reader exactly where the status line left them, so this
// walks every broken machine the file knows how to build and asserts that each failure ends
// in something typeable.
//
// It checks for the placeholder report() substitutes too. That placeholder exists so a
// missing fix is loud rather than silent, and a test that accepted it would be asserting the
// safety net rather than the property.
func TestEveryFailureNamesACommandToRun(t *testing.T) {
	broken := map[string]func(t *testing.T){
		"nothing installed": func(t *testing.T) {
			newWorld(t)
		},
		"hooks missing": func(t *testing.T) {
			w := newWorld(t)
			w.writeHooks(w.settings, selfBinary(t), goodHooks()[:2])
		},
		"hook binary absent": func(t *testing.T) {
			w := newWorld(t)
			w.writeHooks(w.settings, filepath.Join(w.home, "nowhere", "governor"), goodHooks())
		},
		"matcher misses the real tool name": func(t *testing.T) {
			w := newWorld(t)
			specs := goodHooks()
			specs[0].matcher = "Task"
			w.writeHooks(w.settings, selfBinary(t), specs)
		},
		"settings unreadable": func(t *testing.T) {
			w := newWorld(t)
			if err := os.WriteFile(w.settings, []byte(`{"hooks": 7}`), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		},
		"config from the future": func(t *testing.T) {
			w := newWorld(t)
			w.writeConfig(fmt.Sprintf(`{"schema": %d}`, configSchemaVersion+1))
		},
		"daemon is another build (GOV-43)": func(t *testing.T) {
			w := newWorld(t)
			endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) { d.build = "" })
			t.Setenv(transport.EndpointEnv, endpoint)
			w.writeHooks(w.settings, selfBinary(t), goodHooks())
		},
		"endpoint is not unix": func(t *testing.T) {
			newWorld(t)
			t.Setenv(transport.EndpointEnv, "tcp://127.0.0.1:9999")
		},
		"socket is not a socket": func(t *testing.T) {
			w := newWorld(t)
			path := filepath.Join(w.stateDir, "sock")
			if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			t.Setenv(transport.EndpointEnv, "unix://"+path)
		},
	}

	for name, stage := range broken {
		t.Run(name, func(t *testing.T) {
			stage(t)
			code, found, out := doctor(t)
			failures := 0
			for check, f := range found {
				if f.state != "FAIL" {
					continue
				}
				failures++
				if f.fix == "" {
					t.Errorf("%s failed with no fix line:\n%s", check, out)
				}
				if strings.Contains(f.fix, "bug in governor doctor") {
					t.Errorf("%s failed with the missing-fix placeholder, not a fix:\n%s", check, out)
				}
			}
			if failures == 0 {
				t.Fatalf("a deliberately broken machine produced no failures:\n%s", out)
			}
			if code == 0 {
				t.Errorf("exit 0 with %d failing checks, want non-zero:\n%s", failures, out)
			}
		})
	}
}

// TestDoctorPassesOnAWorkingInstall is the other half of the same property: a machine with
// nothing wrong with it must produce nothing to fix, or the command is noise a person learns
// to ignore.
func TestDoctorPassesOnAWorkingInstall(t *testing.T) {
	w := newWorld(t)
	w.writeCalibration()
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, loadsItsConfig)
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())

	code, found, out := doctor(t)
	// NOTHING TO FIX is the property, and it is asserted as "nothing failed" rather than
	// "everything says ok". This world has no session registry and no daemon that has ever
	// seen a spawn, so two checks genuinely cannot verify themselves in it and GOV-30 made
	// them say so. Demanding ok from them here would be demanding the report lie — which is
	// the bug GOV-30 was filed for, reintroduced through its test.
	//
	// TestDoctorPassesEveryCheckOnAFullyObservableMachine is the other half: give those two
	// checks what they need and every line does go green.
	unverifiable := map[string]bool{"sessions": true, "spawn": true}
	for name, f := range found {
		switch {
		case f.state == "ok":
		case f.state == "unknown" && unverifiable[name]:
		default:
			t.Errorf("%s: %s (%s), want ok\n%s", name, f.state, f.detail, out)
		}
	}
	if code != doctorExitUnverified {
		t.Fatalf("exit %d on a working install with nothing to verify against, want %d:\n%s",
			code, doctorExitUnverified, out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a working install reported a failure:\n%s", out)
	}
	if !strings.Contains(out, "could not be verified") {
		t.Errorf("report does not name what it could not verify:\n%s", out)
	}
}

// TestSkippedChecksDoNotCountAsFailures holds the third state honest.
//
// A skip is only reachable downstream of a check that already failed and already named its
// fix. It must not be counted as a defect of its own, or an unreachable daemon would be
// reported as four problems with four fixes for one cause.
func TestSkippedChecksDoNotCountAsFailures(t *testing.T) {
	newWorld(t)

	code, found, out := doctor(t)
	if code == 0 {
		t.Fatalf("exit 0 on a machine with nothing installed, want non-zero:\n%s", out)
	}
	skipped, failed := 0, 0
	for _, f := range found {
		switch f.state {
		case "skip":
			skipped++
		case "FAIL":
			failed++
		}
	}
	if skipped == 0 {
		t.Fatalf("expected some checks to skip with no daemon and no hooks:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d of %d checks failed", failed, len(found))) {
		t.Errorf("tally does not report %d of %d:\n%s", failed, len(found), out)
	}
	// Every skip must point at the check that caused it, or it is an unexplained gap.
	for name, f := range found {
		if f.state == "skip" && !strings.Contains(f.detail, "see ") && !strings.Contains(f.detail, "not running") {
			t.Errorf("%s skipped without naming what it is waiting on: %q", name, f.detail)
		}
	}
}

// ---------------------------------------------------------------------------
// the matcher — the check that earns its keep
// ---------------------------------------------------------------------------

// TestDoctorCatchesAMatcherThatDoesNotCoverTheRealToolNames is the reason this command was
// built before the installer.
//
// A wrong matcher is the one failure with no symptom: the hook is registered, the binary
// exists, the daemon is healthy, and the gate is never invoked. GOV-01 was spent measuring
// that tool_name is Agent on 2.1.263, and GOV-28 added SendMessage as a second spawn path;
// this is where that measurement is spent.
//
// "Task" alone is a failure on purpose. It happens to fire on 2.1.263, because the harness
// carries Task as an alias — design.md section 10.1 says in as many words that registering
// only Task "would have worked by luck". A diagnostic that passes something for working by
// luck will pass it on the version where the luck runs out.
func TestDoctorCatchesAMatcherThatDoesNotCoverTheRealToolNames(t *testing.T) {
	cases := []struct {
		name    string
		matcher string
		state   string
		says    []string
	}{
		{name: "the wiring from section 4.1", matcher: "Agent|Task|SendMessage|Bash", state: "ok"},
		{name: "the wiring before Bash was gated", matcher: "Agent|Task|SendMessage", state: "FAIL",
			says: []string{"Bash"}},
		{name: "a superset still matches", matcher: ".*", state: "ok"},
		{name: "Task alone, which fires only by luck", matcher: "Task", state: "FAIL",
			says: []string{"Agent", "SendMessage"}},
		{name: "the old wiring, before SendMessage", matcher: "Agent|Task", state: "FAIL",
			says: []string{"SendMessage"}},
		{name: "Bash alone, which governs no spawn", matcher: "Bash", state: "FAIL",
			says: []string{"Agent", "Task", "SendMessage"}},
		{name: "no matcher at all", matcher: "", state: "FAIL",
			says: []string{"every tool call"}},
		{name: "an expression that does not compile", matcher: "Agent|[", state: "FAIL",
			says: []string{"never fires"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			specs := goodHooks()
			specs[0].matcher = tc.matcher
			w.writeHooks(w.settings, selfBinary(t), specs)

			_, found, out := doctor(t)
			got := want(t, found, "matcher", tc.state, out)
			for _, s := range tc.says {
				if !strings.Contains(got.detail, s) {
					t.Errorf("matcher finding %q does not name %q\n%s", got.detail, s, out)
				}
			}
			if tc.state == "FAIL" && !strings.Contains(got.fix, strings.Join(gatedToolNames, "|")) {
				t.Errorf("the fix does not name the matcher to set: %q", got.fix)
			}
		})
	}
}

// TestEverySpawnToolNameIsCoveredByTheShippedMatcher ties the matcher doctor demands to the
// matcher design.md section 4.1 actually registers.
//
// Without this the two drift silently in the worst direction: doctor would pass an install
// whose matcher misses a spawn path it has itself stopped asking about.
func TestEverySpawnToolNameIsCoveredByTheShippedMatcher(t *testing.T) {
	re, err := regexp.Compile(strings.Join(spawnToolNames, "|"))
	if err != nil {
		t.Fatalf("the matcher doctor recommends does not compile: %v", err)
	}
	for _, name := range spawnToolNames {
		if !re.MatchString(name) {
			t.Errorf("the recommended matcher does not match %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// hooks
// ---------------------------------------------------------------------------

func TestDoctorNamesTheHooksThatAreNotRegistered(t *testing.T) {
	w := newWorld(t)
	// Everything but the last two, whatever those happen to be, so this keeps naming a real
	// registered hook as the negative control after the list grows again.
	staged := goodHooks()[:len(wantedHooks)-2]
	w.writeHooks(w.settings, selfBinary(t), staged)

	_, found, out := doctor(t)
	got := want(t, found, "hooks", "FAIL", out)
	for _, missing := range wantedHooks[len(wantedHooks)-2:] {
		if !strings.Contains(got.detail, missing.event) {
			t.Errorf("the finding does not name the missing %s hook: %q", missing.event, got.detail)
		}
	}
	if registered := staged[len(staged)-1].event; strings.Contains(got.detail, registered) {
		t.Errorf("the finding names a hook that IS registered: %q", got.detail)
	}
}

// TestDoctorNamesAHookWiredToTheWrongSubcommand separates two failures that read identically
// from the outside.
//
// A SubagentStart running `governor release` returns leases at the moment they should be
// bound. Every symptom of that is a budget that drifts, which sends someone to the daemon
// rather than to one word in their settings file.
func TestDoctorNamesAHookWiredToTheWrongSubcommand(t *testing.T) {
	w := newWorld(t)
	specs := goodHooks()
	for i := range specs {
		if specs[i].event == "SubagentStart" {
			specs[i].args = "release" // SubagentStart running the SubagentStop command
		}
	}
	w.writeHooks(w.settings, selfBinary(t), specs)

	_, found, out := doctor(t)
	got := want(t, found, "hooks", "FAIL", out)
	if !strings.Contains(got.detail, "SubagentStart") || !strings.Contains(got.detail, "instead of") {
		t.Errorf("the finding does not say what SubagentStart runs instead: %q\n%s", got.detail, out)
	}
}

func TestDoctorNamesAHookPointingAtAMissingBinary(t *testing.T) {
	w := newWorld(t)
	absent := filepath.Join(w.home, "nowhere", "governor")
	w.writeHooks(w.settings, absent, goodHooks())

	_, found, out := doctor(t)
	got := want(t, found, "hooks", "FAIL", out)
	if !strings.Contains(got.detail, "not an executable file") {
		t.Errorf("the finding does not say the binary is missing: %q\n%s", got.detail, out)
	}
}

// TestDoctorFindsHooksThePluginRegistered covers the second of the two files design.md
// section 13.3 names.
//
// The expansion is the point. Section 4.1 registers the hooks as
// "${CLAUDE_PLUGIN_ROOT}/bin/governor gate", so a doctor that stat'd the command verbatim
// would report "not an executable file" for every correctly installed plugin there is —
// which is the false alarm that teaches people to ignore a diagnostic.
func TestDoctorFindsHooksThePluginRegistered(t *testing.T) {
	w := newWorld(t)
	// A byte-copy of this build rather than a fakeGovernor, because "correctly installed" is
	// what this world is staging and install copies the running binary verbatim. A stand-in
	// that merely reported the right version numbers was enough while the binary check only
	// compared numbers; GOV-B13 made it compare bytes, and a fixture that cannot pass the
	// real check is a fixture describing an install nobody has.
	bin := copyOfSelf(t, mkdir(t, filepath.Join(w.pluginRoot, "bin")), "governor")
	w.writeHooks(filepath.Join(w.pluginRoot, "hooks", "hooks.json"),
		"${CLAUDE_PLUGIN_ROOT}/bin/governor", goodHooks())

	_, found, out := doctor(t)
	want(t, found, "hooks", "ok", out)
	want(t, found, "matcher", "ok", out)
	if got := want(t, found, "binary", "ok", out); !strings.Contains(got.detail, filepath.Base(bin)) {
		t.Errorf("the binary finding does not name the plugin's binary: %q", got.detail)
	}
}

// TestDoctorIgnoresThePhase0TraceHooks keeps two hook families apart.
//
// The trace hooks are governor hooks pointing at a governor binary, and one of them sits on
// SessionStart — a wanted event. Counted, they would answer the binary check on a machine
// with no gate installed at all, and `governor trace ensure` would be reported as the wrong
// subcommand for a hook it has nothing to do with.
func TestDoctorIgnoresThePhase0TraceHooks(t *testing.T) {
	w := newWorld(t)
	self := selfBinary(t)
	w.writeHooks(w.settings, self, []hookSpec{
		{event: "SubagentStart", args: "trace agent"},
		{event: "SubagentStop", args: "trace agent"},
		{event: "SessionStart", args: "trace ensure"},
	})

	_, found, out := doctor(t)
	got := want(t, found, "hooks", "FAIL", out)
	if !strings.Contains(got.detail, fmt.Sprintf("%d of %d not registered", len(wantedHooks), len(wantedHooks))) {
		t.Errorf("trace hooks were counted as governor hooks: %q\n%s", got.detail, out)
	}
	if strings.Contains(got.detail, "instead of") {
		t.Errorf("a trace hook was reported as a misregistered gate hook: %q", got.detail)
	}
	want(t, found, "binary", "skip", out)
}

// ---------------------------------------------------------------------------
// versions
// ---------------------------------------------------------------------------

// TestDoctorCatchesADaemonSpeakingAnotherProtocol covers the failure that looks healthiest
// from the outside.
//
// The daemon is up, the socket is fine, the hooks are registered — and every hook is failing
// open on the version reply (invariant 8), so nothing on the machine is governed. The daemon
// check must still pass, because telling someone to start a daemon that is already running
// is the opposite of a diagnosis.
func TestDoctorCatchesADaemonSpeakingAnotherProtocol(t *testing.T) {
	newWorld(t)
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) {
		fmt.Fprintf(c, `{"v":%d,"ok":false,"reason":"unsupported_version","supported":[%d]}`+"\n",
			protocolVersion+1, protocolVersion+1)
	})
	chmodSocket(t, endpoint, 0o600)
	t.Setenv(transport.EndpointEnv, endpoint)

	_, found, out := doctor(t)
	want(t, found, "daemon", "ok", out)
	got := want(t, found, "protocol", "FAIL", out)
	if !strings.Contains(got.detail, fmt.Sprintf("v%d", protocolVersion+1)) {
		t.Errorf("the finding does not name the daemon's version: %q", got.detail)
	}
	if !strings.Contains(got.detail, "failing open") {
		t.Errorf("the finding does not say what the mismatch costs: %q", got.detail)
	}
}

// TestDoctorCatchesHooksRunningADifferentBuild covers the state an upgrade passes through.
//
// The hooks and the daemon are separate processes started at separate times, and nothing
// forces them to be the same build once a new binary is on disk.
func TestDoctorCatchesHooksRunningADifferentBuild(t *testing.T) {
	w := newWorld(t)
	bin := fakeGovernor(t, mkdir(t, filepath.Join(w.home, "old")), "governor",
		"0.0.0-ancient", protocolVersion, configSchemaVersion)
	w.writeHooks(w.settings, bin, goodHooks())

	_, found, out := doctor(t)
	got := want(t, found, "binary", "FAIL", out)
	if !strings.Contains(got.detail, "0.0.0-ancient") || !strings.Contains(got.detail, binaryVersion) {
		t.Errorf("the finding does not name both versions: %q\n%s", got.detail, out)
	}
}

// TestDoctorCatchesAStaleInstallThatReportsTheSameVersion is GOV-B13's acceptance criterion.
//
// EVERY NUMBER MATCHES IN BOTH HALVES OF THIS TEST. That is the whole point: on a pre-release
// build `binaryVersion` is `0.0.0-dev` for every build ever made, so a check that compares
// version strings cannot tell a current install from one four merged PRs behind — and for
// twelve days it did not, reporting `ok binary` while the gate ran GOV-18's predecessor. A
// test that staged staleness by giving the two binaries different versions would exercise
// the path that already worked and would leave this one exactly as broken as it was.
//
// So the two halves differ in CONTENT alone, and the check has to notice that or fail.
func TestDoctorCatchesAStaleInstallThatReportsTheSameVersion(t *testing.T) {
	w := newWorld(t)
	dir := mkdir(t, filepath.Join(w.home, "install"))

	// Stale: a different build, answering `version` with this build's own three numbers.
	stale := markedGovernor(t, dir, "governor", binaryVersion, protocolVersion, configSchemaVersion,
		"built from an older commit")
	w.writeHooks(w.settings, stale, goodHooks())

	_, found, out := doctor(t)
	got := want(t, found, "binary", "FAIL", out)
	if !strings.Contains(got.detail, "stale") {
		t.Errorf("the finding does not call the install stale: %q\n%s", got.detail, out)
	}
	// The fix names THIS binary by path, not bare `governor`: doctor is a different, newer
	// file than the hooked one, and a bare name can resolve to the hooked copy (GOV-43).
	if want := selfBinary(t) + " install"; got.fix != want {
		t.Errorf("fix = %q, want %q\n%s", got.fix, want, out)
	}
	// The version is not evidence of staleness here and the report must not imply it is.
	if strings.Contains(got.detail, "vs") {
		t.Errorf("the finding reports a version difference where there is none: %q", got.detail)
	}

	// Current: the same bytes this build is running, installed at its own path. Not the same
	// inode, so sameFile cannot shortcut it — the digest is what has to clear it.
	current := copyOfSelf(t, dir, "governor")
	if current != stale {
		t.Fatalf("the reinstall wrote a different path (%s vs %s); "+
			"the second half is no longer testing the first half's install", current, stale)
	}

	// Only the binary finding is asserted on. This world has no daemon, so the report
	// legitimately carries failures that have nothing to do with the install being current;
	// asserting on the exit code here would tie GOV-B13's test to every other check's world.
	_, found, out = doctor(t)
	got = want(t, found, "binary", "ok", out)
	if !strings.Contains(got.detail, "byte-identical") {
		t.Errorf("the finding does not say what cleared it: %q\n%s", got.detail, out)
	}
	if got.fix != "" {
		t.Errorf("a current install still names a fix: %q\n%s", got.fix, out)
	}
}

// TestDoctorSaysWhatItCouldNotCheckWhenItIsTheInstalledBinary holds clause 3.
//
// Invoked through the install — the ordinary way — the binary running the check and the
// binary the hooks run are one file, and staleness is unknowable from there. The check may
// not imply otherwise. It stays a pass because nothing is known to be wrong and because
// `unverified` is exit 3, which would land on every ordinary doctor run and on `governor
// install`'s own verification; the honesty goes in the sentence instead.
func TestDoctorSaysWhatItCouldNotCheckWhenItIsTheInstalledBinary(t *testing.T) {
	w := newWorld(t)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())

	_, found, out := doctor(t)
	got := want(t, found, "binary", "ok", out)
	if !strings.Contains(got.detail, "staleness unchecked") {
		t.Errorf("the finding claims more than it checked: %q\n%s", got.detail, out)
	}
	if !strings.Contains(got.detail, "fresh build") {
		t.Errorf("the finding does not say how to get the answer it could not get: %q", got.detail)
	}
}

func TestDoctorCatchesHooksPointingAtDifferentBinaries(t *testing.T) {
	w := newWorld(t)
	one := fakeGovernor(t, mkdir(t, filepath.Join(w.home, "a")), "governor",
		binaryVersion, protocolVersion, configSchemaVersion)
	two := fakeGovernor(t, mkdir(t, filepath.Join(w.home, "b")), "governor",
		binaryVersion, protocolVersion, configSchemaVersion)

	specs := goodHooks()
	hooks := map[string][]map[string]any{}
	for i, s := range specs {
		bin := one
		if i == 0 {
			bin = two
		}
		entry := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": bin + " " + s.args}},
		}
		if s.matcher != "" {
			entry["matcher"] = s.matcher
		}
		hooks[s.event] = append(hooks[s.event], entry)
	}
	body, err := json.Marshal(map[string]any{"hooks": hooks})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(w.settings, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, found, out := doctor(t)
	got := want(t, found, "binary", "FAIL", out)
	if !strings.Contains(got.detail, "2 different binaries") {
		t.Errorf("the finding does not say the hooks disagree: %q\n%s", got.detail, out)
	}
}

// TestDoctorJudgesTheConfigSchemaInOneDirectionOnly.
//
// Absent is a pass: a machine that has never been calibrated runs on built-in defaults,
// which is a state and not a fault. Older is a pass too, because migrating it forward is
// exactly what the config.Store contract promises. Only a config from the future is a
// failure — it is the one this binary cannot migrate, because it does not know what changed.
func TestDoctorJudgesTheConfigSchemaInOneDirectionOnly(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		write bool
		state string
	}{
		{name: "never calibrated", write: false, state: "ok"},
		{name: "written by this build", write: true, body: fmt.Sprintf(`{"schema": %d}`, configSchemaVersion), state: "ok"},
		{name: "written by an older build", write: true, body: `{"schema": 0}`, state: "ok"},
		{name: "written by a newer build", write: true, body: fmt.Sprintf(`{"schema": %d}`, configSchemaVersion+1), state: "FAIL"},
		{name: "no schema key at all", write: true, body: `{"setpoint": 0.7}`, state: "FAIL"},
		{name: "truncated mid-write", write: true, body: `{"schema":`, state: "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			if tc.write {
				w.writeConfig(tc.body)
			}
			_, found, out := doctor(t)
			want(t, found, "config", tc.state, out)
		})
	}
}

// ---------------------------------------------------------------------------
// socket
// ---------------------------------------------------------------------------

// TestDoctorCatchesASocketOtherUsersCanWrite.
//
// transport.Listen chmods the socket 0600 because it carries this user's session and lease
// state and v1 is single-user. A socket another local user can write to is a socket another
// local user can acquire leases on, starving this one's agents from outside the budget
// entirely — and nothing about that is visible from the status line.
func TestDoctorCatchesASocketOtherUsersCanWrite(t *testing.T) {
	newWorld(t)
	endpoint, _ := startTestDaemon(t, 4)
	t.Setenv(transport.EndpointEnv, endpoint)
	chmodSocket(t, endpoint, 0o666)

	_, found, out := doctor(t)
	want(t, found, "daemon", "ok", out)
	got := want(t, found, "socket", "FAIL", out)
	if !strings.Contains(got.detail, "take leases") {
		t.Errorf("the finding does not say what a loose mode costs: %q", got.detail)
	}
	if !strings.Contains(got.fix, "chmod") {
		t.Errorf("the fix does not name a chmod: %q", got.fix)
	}
}

func TestDoctorCatchesSomethingThatIsNotASocket(t *testing.T) {
	w := newWorld(t)
	path := filepath.Join(w.stateDir, "sock")
	if err := os.WriteFile(path, []byte("a file in the way"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(transport.EndpointEnv, "unix://"+path)

	_, found, out := doctor(t)
	got := want(t, found, "socket", "FAIL", out)
	if !strings.Contains(got.detail, "is not a socket") {
		t.Errorf("the finding does not say what is in the way: %q", got.detail)
	}
	if !strings.Contains(got.fix, "mv ") {
		t.Errorf("the fix does not name how to move it aside: %q", got.fix)
	}
}

func TestDoctorRefusesAnEndpointItCannotSpeak(t *testing.T) {
	newWorld(t)
	t.Setenv(transport.EndpointEnv, "tcp://127.0.0.1:9999")

	_, found, out := doctor(t)
	got := want(t, found, "socket", "FAIL", out)
	if !strings.Contains(got.detail, "unix://") {
		t.Errorf("the finding does not say what v1 speaks: %q", got.detail)
	}
	if !strings.Contains(got.fix, transport.EndpointEnv) {
		t.Errorf("the fix does not name the variable to change: %q", got.fix)
	}
}

// ---------------------------------------------------------------------------
// leases
// ---------------------------------------------------------------------------

// TestDoctorCatchesALeaseNoOwnerCanReclaim.
//
// The reaper collects a lease three ways — its TTL expires, its session's process is gone,
// or Liveness says its owner is gone — and the last two need an owning pid, bound at
// `session start` and at `confirm`. A lease held under a session with no pid has neither
// available to it: nothing can be asked about it, and it holds its slot for a full TTL,
// which for a confirmed lease is half an hour. That is what a full budget on an idle machine
// actually looks like, and it is invisible from the status line, which reports the same
// "4/4 slots" either way.
func TestDoctorCatchesALeaseNoOwnerCanReclaim(t *testing.T) {
	newWorld(t)
	endpoint, _ := startTestDaemon(t, 4)
	t.Setenv(transport.EndpointEnv, endpoint)

	// A spawn from a session that never registered: the gate acquires, and nothing on the
	// machine knows whose process it belongs to.
	if r := call(t, endpoint, acquireReq("orphan-session")); r.OK == nil || !*r.OK {
		t.Fatalf("acquire refused: %+v", r)
	}

	_, found, out := doctor(t)
	got := want(t, found, "leases", "FAIL", out)
	if !strings.Contains(got.detail, "no owning pid") {
		t.Errorf("the finding does not say why the lease is stuck: %q\n%s", got.detail, out)
	}
	if !strings.Contains(got.fix, "SessionStart") {
		t.Errorf("the fix does not name the hook that would have bound an owner: %q", got.fix)
	}
}

// TestDoctorLeavesALiveSessionsLeasesAlone is the other direction, and it matters more than
// it looks: a false "your leases are stranded" sends someone to restart a daemon that is
// working perfectly, and a diagnostic that cries wolf is one nobody runs twice.
func TestDoctorLeavesALiveSessionsLeasesAlone(t *testing.T) {
	newWorld(t)
	endpoint, _ := startTestDaemon(t, 4)
	t.Setenv(transport.EndpointEnv, endpoint)

	call(t, endpoint, map[string]any{
		"v": protocolVersion, "op": "session", "action": "start",
		"principal": map[string]any{"user": "tester"},
		"session":   "live-session", "pid": os.Getpid(), "weight": 1,
	})
	if r := call(t, endpoint, acquireReq("live-session")); r.OK == nil || !*r.OK {
		t.Fatalf("acquire refused: %+v", r)
	}

	_, found, out := doctor(t)
	got := want(t, found, "leases", "ok", out)
	if !strings.Contains(got.detail, "1 held") {
		t.Errorf("the finding does not report the held lease: %q", got.detail)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

func chmodSocket(t *testing.T, endpoint string, mode os.FileMode) {
	t.Helper()
	path := strings.TrimPrefix(endpoint, "unix://")
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// TestSocketFaultsJudgesOwnershipAndMode covers the ownership branch, which no integration
// test can reach: creating a socket owned by another user takes root, and a branch that only
// runs on someone else's machine is a branch that is never tested before it matters.
func TestSocketFaultsJudgesOwnershipAndMode(t *testing.T) {
	const me, them = 501, 502
	cases := []struct {
		name     string
		facts    socketFacts
		problems int
		says     string
	}{
		{name: "what transport.Listen creates",
			facts: socketFacts{self: me, owner: me, ownerKnown: true, perm: 0o600, dirPerm: 0o700, dirKnown: true}},
		{name: "a readable directory is not a fault",
			facts: socketFacts{self: me, owner: me, ownerKnown: true, perm: 0o600, dirPerm: 0o755, dirKnown: true}},
		{name: "another user's socket",
			facts:    socketFacts{self: me, owner: them, ownerKnown: true, perm: 0o600, dirPerm: 0o700, dirKnown: true},
			problems: 1, says: "uid 502"},
		{name: "world-writable",
			facts:    socketFacts{self: me, owner: me, ownerKnown: true, perm: 0o666, dirPerm: 0o700, dirKnown: true},
			problems: 1, says: "take leases"},
		{name: "a directory anyone can write to",
			facts:    socketFacts{self: me, owner: me, ownerKnown: true, perm: 0o600, dirPerm: 0o777, dirKnown: true},
			problems: 1, says: "replace the socket"},
		{name: "an owner the platform does not report is not a fault",
			facts: socketFacts{self: me, owner: 0, ownerKnown: false, perm: 0o600, dirPerm: 0o700, dirKnown: true}},
		{name: "everything wrong at once",
			facts:    socketFacts{self: me, owner: them, ownerKnown: true, perm: 0o666, dirPerm: 0o777, dirKnown: true},
			problems: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.facts.path = "/tmp/x/sock"
			problems, fixes := tc.facts.faults()
			if len(problems) != tc.problems {
				t.Fatalf("%d problems %v, want %d", len(problems), problems, tc.problems)
			}
			if len(fixes) != len(problems) {
				t.Fatalf("%d problems but %d fixes: every fault has to name its own fix", len(problems), len(fixes))
			}
			if tc.says != "" && !strings.Contains(strings.Join(problems, "; "), tc.says) {
				t.Errorf("problems %v do not say %q", problems, tc.says)
			}
		})
	}
}

// TestDoctorNamesASocketPathTooLongToBind covers the one failure this command found in
// itself.
//
// A path past sockaddr_un's limit produces EINVAL from both bind and connect, which reads as
// "invalid argument" and not as "four characters too long". Before this, doctor answered it
// with "governor daemon" — a fix that cannot work, because the daemon cannot bind there
// either. Naming a fix that does not fix it is worse than describing, which is why it gets a
// test of its own rather than a note.
func TestDoctorNamesASocketPathTooLongToBind(t *testing.T) {
	w := newWorld(t)
	long := filepath.Join(w.home, strings.Repeat("d", 120), "sock")
	t.Setenv(transport.EndpointEnv, "unix://"+long)

	_, found, out := doctor(t)
	got := want(t, found, "daemon", "FAIL", out)
	if !strings.Contains(got.detail, "past this platform's limit") {
		t.Errorf("the finding does not name the path limit: %q\n%s", got.detail, out)
	}
	if strings.TrimSpace(got.fix) == "governor daemon" {
		t.Errorf("the fix is one that cannot work: %q", got.fix)
	}
	if !strings.Contains(got.fix, transport.StateDirEnv) {
		t.Errorf("the fix does not name the variable to shorten: %q", got.fix)
	}
}

// ---------------------------------------------------------------------------
// calibration
// ---------------------------------------------------------------------------

// TestDoctorReportsWhetherThisHostWasEverMeasured.
//
// A separate finding from `config`, because the two have different fixes — `config` asks
// whether the file can be read and its fix is to move it aside; this asks whether anybody
// measured this machine and its fix is one command. design.md section 13.2 records the same
// reasoning for splitting "versions agree" into three findings.
//
// Never calibrated is a WARN and not a FAIL. Every calibrated key ships with a default that
// leaves it inert, so an uncalibrated governor works — it just works to constants measured on
// somebody else's laptop, which is real, worth printing, and not a defect in this
// installation. A warn does not move the exit code.
func TestDoctorReportsWhetherThisHostWasEverMeasured(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		write bool
		state string
	}{
		{name: "no config at all", write: false, state: "warn"},
		{
			name: "a config with no calibration in it", write: true,
			body:  fmt.Sprintf(`{"schema": %d, "b_max": 4}`, configSchemaVersion),
			state: "warn",
		},
		{
			name: "calibrated on this machine", write: true,
			body: fmt.Sprintf(`{"schema": %d, "run_queue_max": 3.4, "b_max": 5, "calibrated_at": "2026-09-16T00:00:00Z", "calibrated_from": "governor calibrate 2026-09-16T00:00:00Z; %d cores (4 performance, 4 efficiency)"}`,
				configSchemaVersion, runtime.NumCPU()),
			state: "ok",
		},
		{
			name: "calibrated on a machine with a different core count", write: true,
			body: fmt.Sprintf(`{"schema": %d, "run_queue_max": 3.4, "b_max": 5, "calibrated_at": "2026-09-16T00:00:00Z", "calibrated_from": "governor calibrate 2026-09-16T00:00:00Z; %d cores (no performance/efficiency split published by this platform)"}`,
				configSchemaVersion, runtime.NumCPU()+9),
			state: "warn",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			if tc.write {
				w.writeConfig(tc.body)
			}
			_, found, out := doctor(t)
			want(t, found, "calibration", tc.state, out)
		})
	}
}

// TestBeingUncalibratedDoesNotMoveTheExitCode.
//
// The warn state exists so a finding can be worth printing without being a defect, and the
// summary line may not round one off. This matters beyond tidiness because `install` ends by
// running doctor and PROPAGATES ITS EXIT CODE: if an uncalibrated machine failed, then every
// host where the calibration could not run would report a failed install — and every machine
// is uncalibrated for the moment between the binary landing and `calibrate` finishing.
//
// It is asserted by running the same world twice, calibrated and not, rather than against an
// absolute exit code. The fixture has no hooks installed, so doctor exits non-zero either way
// for a reason that has nothing to do with this check; what this has to show is that the
// calibration finding does not CHANGE the answer.
func TestBeingUncalibratedDoesNotMoveTheExitCode(t *testing.T) {
	calibrated := fmt.Sprintf(
		`{"schema": %d, "run_queue_max": 3.4, "b_max": 5, "calibrated_at": "2026-09-16T00:00:00Z", "calibrated_from": "governor calibrate 2026-09-16T00:00:00Z; %d cores (4 performance, 4 efficiency)"}`,
		configSchemaVersion, runtime.NumCPU())

	withCalibration := func(body string) (int, line) {
		t.Helper()
		w := newWorld(t)
		if body != "" {
			w.writeConfig(body)
		}
		code, found, out := doctor(t)
		got, ok := found["calibration"]
		if !ok {
			t.Fatalf("no calibration check in the report:\n%s", out)
		}
		return code, got
	}

	withCode, withFinding := withCalibration(calibrated)
	withoutCode, withoutFinding := withCalibration("")

	if withFinding.state != "ok" {
		t.Errorf("a calibrated host reported %q", withFinding.state)
	}
	if withoutFinding.state != "warn" {
		t.Errorf("an uncalibrated host reported %q, want warn", withoutFinding.state)
	}
	if withoutFinding.fix == "" {
		t.Error("the calibration warning names no fix, so it is a description rather than a diagnosis")
	}
	if withCode != withoutCode {
		t.Errorf("exit %d calibrated against %d uncalibrated: the warning moved the exit code, so `install` would fail on a host that could not calibrate",
			withCode, withoutCode)
	}
}

// GOV-34 clause 4: doctor reports a budget below what the host could allow, and says which
// signal is responsible.
//
// THE MACHINE COULD NOT ANSWER "WHY ONLY THREE" FROM ANY SURFACE IT HAD. On 2026-09-21 a
// session spent fifteen minutes unable to run a build with the refusal text, `governor status`
// and `governor doctor` all in front of it, and none of the three could say whether the budget
// it kept being refused against was this host's ceiling or a cut below it. It is a warning
// rather than a failure because a cut budget is the control loop working — see checkBudget.
func TestDoctorNamesTheSignalCuttingTheBudget(t *testing.T) {
	w := newWorld(t)
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		// A controller with room to be cut, driven past its threshold once. Wired before
		// serve starts, which is the whole reason startTestDaemonConfigured exists.
		d.loop = &budget.Controller{Max: 6}
		d.loop.Step(budget.Reading{RunQueue: budget.RunQueueMax * 4})
		d.sources = []budget.Source{budget.LocalSource{Live: d.liveBudget, InUse: d.table.InUse}}
	})
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())
	w.writeCalibration()

	code, found, out := doctor(t)
	got := want(t, found, "budget", "warn", out)
	if !strings.Contains(got.detail, "run queue") {
		t.Errorf("the finding does not name the signal responsible: %q", got.detail)
	}
	if !strings.Contains(got.detail, "6") {
		t.Errorf("the finding does not say what the budget could have been: %q", got.detail)
	}
	// A warning is not a defect and must not fail the command, for the same reason a stray
	// daemon does not: a red line that means "working as designed" teaches a reader to
	// ignore red lines. Asserted as "nothing FAILED" rather than "exit 0", because this
	// world has no session registry and no daemon that has seen a spawn, so two checks
	// legitimately cannot verify themselves and carry the exit code to 3 on their own.
	if code == 1 {
		t.Errorf("exit code = 1: a cut budget was reported as a broken install\n%s", out)
	}
	for name, f := range found {
		if f.state == "FAIL" {
			t.Errorf("check %q failed on a machine whose only finding is a cut budget: %q\n%s",
				name, f.detail, out)
		}
	}
}

// The other side of the same check: a machine whose budget is at its ceiling has nothing to
// report, and must say so rather than warning about a cut that is not happening.
func TestDoctorIsQuietWhenNothingIsCuttingTheBudget(t *testing.T) {
	w := newWorld(t)
	endpoint, _ := startTestDaemonConfigured(t, 6, lease.Options{}, func(d *daemon) {
		d.loop = &budget.Controller{Max: 6}
		d.loop.Step(budget.Reading{RunQueue: 0})
		d.sources = []budget.Source{budget.LocalSource{Live: d.liveBudget, InUse: d.table.InUse}}
	})
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())
	w.writeCalibration()

	_, found, out := doctor(t)
	got := want(t, found, "budget", "ok", out)
	if strings.Contains(got.detail, "cut by") {
		t.Errorf("the finding claims a cut on a machine at its ceiling: %q", got.detail)
	}
}

// ---------------------------------------------------------------------------
// GOV-46 clause 4: every fix line names a binary that works
// ---------------------------------------------------------------------------

// TestEveryDoctorFixLineNamesABinaryThatWorks is the audit, kept as a test so it stays done.
//
// GOV-43 made the stale-build fix lines full-path and missed the budget check's `kill … &&
// governor install`; GOV-45 then removed the ~/.local/bin link that had made the bare name
// resolve to the hooked copy, so on a brew machine every remaining bare `governor install`
// named whatever else was on PATH — on the machine that found GOV-46, an older release, which
// the fix would have silently reinstalled. Checking one line at a time is how the last one was
// missed, so this reads every string literal in doctor.go: any `governor install`, `governor
// calibrate` or `governor doctor` in one must go through governorCommand instead.
func TestEveryDoctorFixLineNamesABinaryThatWorks(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doctor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse doctor.go: %v", err)
	}
	bare := regexp.MustCompile(`(^|[^-:\w/])governor (install|calibrate|doctor|uninstall)\b`)
	// Not commands a person is told to type: the flag set's own name, and the sentence for a
	// check that forgot its fix.
	allowed := map[string]bool{
		`"governor doctor"`: true,
		`"no fix recorded for this check — that is a bug in governor doctor"`: true,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || allowed[lit.Value] {
			return true
		}
		if bare.MatchString(lit.Value) {
			t.Errorf("%s: %s names bare `governor`, which may resolve to another copy on PATH; "+
				"spell it with governorCommand", fset.Position(lit.Pos()), lit.Value)
		}
		return true
	})
}

// governorCommand is what makes those lines work, and its rule is a property rather than a
// guess about install routes: the bare name only when bare `governor` on PATH IS this binary.
func TestGovernorCommandRunsThisBinary(t *testing.T) {
	self := selfBinary(t)

	t.Run("an older governor elsewhere on PATH", func(t *testing.T) {
		// The GOV-46 machine: GOV-45 retired the ~/.local/bin link, so bare `governor` is an
		// older brew release. Naming it would downgrade the install.
		dir := mkdir(t, filepath.Join(t.TempDir(), "brew"))
		markedGovernor(t, dir, "governor", "0.1.1", protocolVersion, configSchemaVersion, "older brew")
		t.Setenv("PATH", dir)
		if got, want := governorCommand("install"), self+" install"; got != want {
			t.Errorf("governorCommand = %q, want %q", got, want)
		}
	})
	t.Run("no governor on PATH at all", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if got, want := governorCommand("calibrate"), self+" calibrate"; got != want {
			t.Errorf("governorCommand = %q, want %q", got, want)
		}
	})
	t.Run("bare governor is this binary", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(self, filepath.Join(dir, "governor")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		t.Setenv("PATH", dir)
		if got := governorCommand("install"); got != "governor install" {
			t.Errorf("governorCommand = %q, want the bare name when it resolves here", got)
		}
	})
}

// GOV-46 clause 1 as doctor sees it: a budget below its ceiling whose cut has recovered is not
// reported as a cut, and is no longer answered with a restart — the loop reconciles it. The
// restart line this replaced was also the fix line clause 4 was filed on.
func TestDoctorDoesNotRestartARecoveringBudget(t *testing.T) {
	w := newWorld(t)
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.loop = &budget.Controller{Max: 6}
		d.loop.Step(budget.Reading{RunQueue: budget.RunQueueMax * 4})
		for i := 0; i < budget.NominalTicksBeforeIncrease+1; i++ {
			d.loop.Step(budget.Reading{RunQueue: 0, InUse: true})
		}
		d.sources = []budget.Source{budget.LocalSource{Live: d.liveBudget, InUse: d.table.InUse}}
	})
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())
	w.writeCalibration()

	_, found, out := doctor(t)
	got := want(t, found, "budget", "warn", out)
	if strings.Contains(got.detail, "cut by") {
		t.Errorf("a recovered signal is reported as a cut: %q", got.detail)
	}
	if strings.Contains(got.fix, "kill") || strings.Contains(got.fix, "install") {
		t.Errorf("a recovering budget is answered with a restart: %q", got.fix)
	}
}
