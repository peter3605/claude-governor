package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// `governor doctor` (GOV-20, design.md sections 13.1 and 13.2).
//
// The rule this whole file is written against is design.md section 13.1's: doctor
// DIAGNOSES, it does not describe. "daemon: not running" is a description — the user can
// see that much from the status line that sent them here. "daemon: not running — start it
// with `governor daemon`" is a diagnosis. So every failing check carries a fix, and the fix
// is a command that can be typed, not a suggestion to investigate.
//
// It is built before `governor install` (GOV-21) rather than after, deliberately: this is
// what tells you whether the installer worked. On a machine where nothing has been
// installed, doctor is *supposed* to fail — that is the correct report, not a bug in the
// tool.
//
// The check that earns its keep is the matcher one. A wrong tool_name is silent: nothing
// errors, the plugin looks installed, the daemon looks healthy, and no spawn is ever
// governed. Every other failure here announces itself eventually; that one does not.
//
// Unlike everything in gate.go and hooks.go, nothing in this file is on a hot path. It is
// run by a person who has already decided to wait for an answer, so it may open files,
// stat paths and exec a binary — all of which are forbidden a hook.

// doctorDialBudget is how long doctor waits for the daemon.
//
// It matches `governor status`'s allowance rather than the gate's 100 ms, and for the same
// reason: this is typed by a person, not fired on a spawn. It is still bounded, because a
// diagnostic that hangs against the wedged daemon it was run to diagnose is the worst
// possible outcome for this particular command.
const doctorDialBudget = time.Second

// doctorExecBudget bounds the one subprocess doctor runs: `<hooked binary> version`.
//
// Generous, because that binary may be cold on disk or on a network mount, and a slow
// answer is still an answer. It exists so that a hook pointing at something that is not
// this program — a script that reads stdin, say — cannot hang the diagnosis.
const doctorExecBudget = 2 * time.Second

// checkState is how a single check came out.
type checkState int

const (
	pass checkState = iota
	fail
	// skip is a check that could not be judged because something it depends on already
	// failed and already named its fix.
	//
	// It is a third state rather than a failure on purpose. With the daemon down there is
	// no protocol version to compare and no lease table to read, and reporting those as two
	// further failures would print three findings for one cause and three fixes for one
	// problem — which is describing rather than diagnosing, at length. A skip is only ever
	// reachable downstream of a check that has already failed, so nothing goes unreported,
	// and it does not move the exit code because there is no defect to report.
	skip
)

func (s checkState) label() string {
	switch s {
	case pass:
		return "ok"
	case fail:
		return "FAIL"
	default:
		return "skip"
	}
}

// finding is one check's verdict.
type finding struct {
	// name is the short column label — the subsystem, not the sentence.
	name string
	// state is pass, fail or skip.
	state checkState
	// detail is what was actually found, in the words of the thing that was inspected.
	detail string
	// fix is the command to type. It is REQUIRED on a failure: a check that cannot name a
	// fix is a check that only describes, and report() asserts this rather than trusting it.
	fix string
}

// diagnosis runs the checks and carries the facts each one leaves for the next.
//
// The checks are ordered by dependency and, as it happens, by how a person actually debugs
// this: is it running, is its socket sane, is it wired in, is the wiring right, do the
// versions agree, is anything stuck.
type diagnosis struct {
	findings []finding

	// endpoint is the daemon endpoint URL, resolved once.
	endpoint string
	// up is whether a well-formed status reply arrived.
	up bool
	// reply is that status reply, meaningful only when up.
	reply doctorStatus
	// hooks are the governor hook registrations found across every file that can hold one.
	hooks []hookRef
	// gate is the PreToolUse gate registration, if there is one. The matcher check reads it.
	gate *hookRef
}

// doctorStatus is a status reply, plus the two fields an `unsupported_version` refusal
// carries.
//
// A daemon a major version ahead answers status with a refusal rather than a status reply
// (design.md section 7), and those two shapes arrive on the same connection in reply to the
// same request. The statusline treats the difference as "no answer" because there is
// nothing it could usefully render either way; doctor exists precisely to tell a person
// which of the two happened, so it decodes both.
type doctorStatus struct {
	statusResponse
	Reason    string `json:"reason"`
	Supported []int  `json:"supported"`
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("governor doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d := &diagnosis{}
	d.run()
	return d.report(stdout)
}

// run performs every check, in dependency order.
func (d *diagnosis) run() {
	d.checkDaemon()
	d.checkSocket()
	d.checkHooks()
	d.checkMatcher()
	d.checkSpawnTools()
	d.checkProtocol()
	d.checkBinary()
	d.checkConfig()
	d.checkSessions()
	d.checkLeases()
}

// ---------------------------------------------------------------------------
// spawn tools
// ---------------------------------------------------------------------------

// checkSpawnTools reports which Claude Code versions are running here against the version the
// spawn tool names were last measured on.
//
// WHY THIS IS SEPARATE FROM checkMatcher. That check reads the matcher off disk and tests it
// against spawnToolNames, and it earns its keep: a hand-edited or stale matcher fails it. What
// it cannot do is notice that spawnToolNames itself has gone out of date, because install
// writes the matcher from that same list — the two agree by construction on every stock
// install. A tool rename in Claude Code would leave both agreeing, doctor printing ok, and
// nothing governed.
//
// This check cannot detect a rename either, and does not pretend to. It reports a fact from
// outside this binary — the version actually running, read from the harness's own session
// registry — so that the matcher's correctness stops being a claim with no expiry date.
//
// IT IS DELIBERATELY NOT A FAILURE. A newer Claude Code is a routine event and, for anyone
// who is not maintaining this program, not a fixable one; failing on it would put a red line
// in front of every user after every upgrade and teach them that a red line means nothing.
// The whole report is worth less than that costs.
func (d *diagnosis) checkSpawnTools() {
	names := strings.Join(spawnToolNames, ", ")
	var newer []string
	seen := map[string]bool{}
	for _, s := range d.reply.Sessions {
		if !s.Live || s.Version == "" || seen[s.Version] {
			continue
		}
		seen[s.Version] = true
		if compareVersions(s.Version, spawnToolNamesConfirmedAgainst) > 0 {
			newer = append(newer, s.Version)
		}
	}
	if len(seen) == 0 {
		// Nothing was read, so nothing was compared. Saying "measured against 2.1.267" here
		// would read as "checked against what is running on this machine" when no version was
		// observed at all — and on a host with no session registry that is the DEFAULT output,
		// not a rare one. The sibling sessions check states its own limit for this reason and
		// so does this.
		d.add("spawn", pass, fmt.Sprintf(
			"%s, measured against Claude Code %s — no running version seen here to compare against",
			names, spawnToolNamesConfirmedAgainst), "")
		return
	}
	if len(newer) == 0 {
		d.add("spawn", pass, fmt.Sprintf("%s, measured against Claude Code %s",
			names, spawnToolNamesConfirmedAgainst), "")
		return
	}
	sort.Strings(newer)
	d.add("spawn", pass, fmt.Sprintf(
		"%s, measured against Claude Code %s — NOT re-checked against %s, which is running here",
		names, spawnToolNamesConfirmedAgainst, strings.Join(newer, " and ")), "")
}

// compareVersions orders two dotted numeric versions. Non-numeric or ragged input compares
// equal rather than guessing, because the only thing this drives is whether to add a caveat
// and a wrong guess would either cry wolf or stay silent when it mattered.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, err1 := strconv.Atoi(as[i])
		y, err2 := strconv.Atoi(bs[i])
		if err1 != nil || err2 != nil {
			return 0
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// sessionVerdict is what became of one live session.
type sessionVerdict struct {
	stat sessionStat
	// why is the reason it is ungoverned, empty when it is governed.
	why string
}

// label names a session in terms a person can act on. A bare UUID is not something anyone
// can find in a window.
func (v sessionVerdict) label() string {
	if v.stat.CWD != "" {
		return filepath.Base(v.stat.CWD)
	}
	if len(v.stat.Session) > 8 {
		return v.stat.Session[:8]
	}
	return v.stat.Session
}

// checkSessions compares the sessions running on this machine against the sessions the
// daemon has actually heard from.
//
// THIS IS THE CHECK GOV-29 EXISTS FOR. Hooks are bound at session start, so a session that
// was already open when the governor was installed never loaded the gate and is ungoverned
// for the rest of its life. Nothing else in this command can see that: every other check
// reads a file, a version or a socket, and all of them are perfectly happy on a machine
// where not one spawn is being governed. On 2026-09-09 this machine ran six sessions, four
// of them ungoverned, and doctor reported 8 of 8.
//
// The evidence has to come from outside the hook surface, because the hooks of an ungoverned
// session never run. It comes from the harness's own session registry
// (docs/hook-surface.md section 17), joined to the daemon's session table on session_id.
func (d *diagnosis) checkSessions() {
	if !d.up {
		d.add("sessions", skip, "no daemon to ask which sessions have registered (see daemon above)", "")
		return
	}
	if !d.reply.SessionsKnown {
		// Either this Claude Code keeps no session registry, or the daemon predates GOV-29.
		// Neither is a failure and neither is a skip: a skip in this command means "downstream
		// of a check that already failed and already named its fix", and nothing here failed.
		// It is reported as a pass that states its own limit, on the rule that makes an
		// unmeasurable thermal state nominal rather than critical — an instrument that cannot
		// read says so instead of guessing a reading.
		//
		// The detail leads with "not verified" deliberately. This is the one line in the
		// report where "ok" would otherwise be read as "checked and fine", and that misreading
		// is the entire defect this check was added to remove.
		d.add("sessions", pass,
			"not verified — the daemon did not say which sessions are live "+
				"(a daemon older than this binary, or a host with no session registry)", "")
		return
	}

	if d.gate == nil || d.reply.HooksInstalled == "" {
		// There is no gate hook to have loaded, so of course no session loaded one. Saying
		// "restart these sessions" here would name a fix that fixes nothing — restarting into
		// a machine with no hooks installed leaves it exactly as ungoverned. The hooks check
		// above has already failed and already named the real fix, which is what a skip means.
		d.add("sessions", skip, "no gate hook for a session to have loaded (see hooks above)", "")
		return
	}

	var live, ungoverned []sessionVerdict
	registered, replaced := 0, 0
	for _, s := range d.reply.Sessions {
		if !s.Live {
			continue
		}
		v := sessionVerdict{stat: s}
		live = append(live, v)
		switch g := classify(d.reply.statusResponse, s); g {
		case govRegistered:
			registered++
		case govPreviousDaemon:
			replaced++
		default:
			// Only an interactive session is reported. Restarting is the sole fix an
			// ungoverned session has, and that is not an action that exists for a headless
			// `claude -p` run — which will have exited before anyone reads the advice.
			// A missing kind is treated as interactive: it is the case where this cannot
			// tell, and staying quiet about a real terminal session would lose the finding.
			if g.ungoverned() && (s.Kind == "" || s.Kind == "interactive") {
				v.why = g.why()
				ungoverned = append(ungoverned, v)
			}
		}
	}

	if len(live) == 0 {
		d.add("sessions", pass, "no Claude Code sessions running", "")
		return
	}
	if len(ungoverned) == 0 {
		detail := fmt.Sprintf("%d of %d live session(s) registered with the daemon", registered, len(live))
		if replaced > 0 {
			// Said out loud rather than folded into the count. Otherwise a healthy machine
			// straight after an upgrade reads "0 of 5 registered" over an ok, which is the
			// sort of line that gets a working install debugged.
			detail += fmt.Sprintf("; %d registered with a daemon that has since been replaced", replaced)
		}
		d.add("sessions", pass, detail, "")
		return
	}

	names := make([]string, 0, len(ungoverned))
	for _, v := range ungoverned {
		names = append(names, fmt.Sprintf("%s (%s)", v.label(), v.why))
	}
	d.add("sessions", fail,
		fmt.Sprintf("%d of %d live session(s) are ungoverned: %s",
			len(ungoverned), len(live), strings.Join(names, ", ")),
		fmt.Sprintf("restart %s — hooks are bound when a session starts, so an open session never picks them up",
			pluralSessions(ungoverned)))
}

// pluralSessions names what to restart, by directory where there is one.
func pluralSessions(v []sessionVerdict) string {
	if len(v) == 1 {
		return "the Claude Code session in " + v[0].label()
	}
	names := make([]string, 0, len(v))
	for _, s := range v {
		names = append(names, s.label())
	}
	return "the Claude Code sessions in " + strings.Join(names, ", ")
}

// report prints the findings and returns the process exit code.
//
// Non-zero if any check failed, per this story's acceptance criterion. It is 1 rather than
// 2, because 2 is what this binary already returns for a usage error and a script that
// cannot tell "your install is broken" from "you typed the command wrong" is a script that
// will conflate them.
func (d *diagnosis) report(stdout io.Writer) int {
	failed := 0
	for _, f := range d.findings {
		fmt.Fprintf(stdout, "%-4s  %-8s  %s\n", f.state.label(), f.name, f.detail)
		if f.state != fail {
			continue
		}
		failed++
		fix := f.fix
		if fix == "" {
			// Belt and braces on the one rule this command exists to keep. A failure with
			// no fix is the describing-not-diagnosing bug, and it is far better to say so
			// on the spot than to ship a check that quietly stops earning its keep.
			fix = "no fix recorded for this check — that is a bug in governor doctor"
		}
		fmt.Fprintf(stdout, "      %-8s  %s\n", "fix", fix)
	}
	if failed == 0 {
		fmt.Fprintf(stdout, "\n%d checks, all passing.\n", len(d.findings))
		return 0
	}
	fmt.Fprintf(stdout, "\n%d of %d checks failed.\n", failed, len(d.findings))
	return 1
}

func (d *diagnosis) add(name string, state checkState, detail, fix string) {
	d.findings = append(d.findings, finding{name: name, state: state, detail: detail, fix: fix})
}

// ---------------------------------------------------------------------------
// daemon
// ---------------------------------------------------------------------------

// checkDaemon asks whether anything is listening on the configured endpoint and answering.
//
// Reachability only. A daemon that answers in a protocol this binary does not speak is
// running perfectly well and is a different problem with a different fix, so it is
// checkProtocol's finding and not this one's — conflating them would tell someone to start
// a daemon that is already up.
func (d *diagnosis) checkDaemon() {
	endpoint, err := transport.DefaultEndpoint()
	if err != nil {
		d.add("daemon", fail,
			fmt.Sprintf("cannot resolve the endpoint: %v", err),
			fmt.Sprintf("set %s to a unix:// URL, e.g. %s=unix://$HOME/.claude/governor/sock",
				transport.EndpointEnv, transport.EndpointEnv))
		return
	}
	d.endpoint = endpoint

	// A path past sockaddr_un's limit is diagnosed before the dial rather than after it.
	// The dial's own answer is EINVAL — "connect: invalid argument" — and the fix for that
	// is emphatically not "start the daemon", which cannot bind there either. Naming a fix
	// that cannot work is the failure this whole command exists to prevent.
	if path, err := socketPath(endpoint); err == nil && len(path) > transport.MaxSocketPath {
		d.add("daemon", fail,
			fmt.Sprintf("the socket path is %d bytes, past this platform's limit of %d — nothing can bind or connect there",
				len(path), transport.MaxSocketPath),
			fmt.Sprintf("point the state directory somewhere shorter: %s=$HOME/.claude/governor, then: governor install",
				transport.StateDirEnv))
		return
	}

	// The exchange reports what went wrong to a diag writer. Everywhere else in this binary
	// that is stderr, because everywhere else the failure is silent by design; here the
	// failure IS the output, so it is captured and folded into the finding. Printing it to
	// stderr as well would put half the diagnosis above the report and half inside it.
	var why strings.Builder
	var resp doctorStatus
	if !exchange(&why, transport.Dial{}, time.Now().Add(doctorDialBudget), statusRequest{
		V: protocolVersion, Op: "status", Principal: localPrincipal(),
	}, &resp) {
		// The endpoint is already in the sentence; the dial's own restatement of it is not
		// worth a second forty characters on a line a person has to read.
		reason := strings.TrimPrefix(lastLine(why.String()), "dial "+endpoint+": ")
		d.add("daemon", fail,
			fmt.Sprintf("nothing answering on %s (%s) — every spawn on this machine is ungoverned",
				short(endpoint), short(reason)),
			"governor install")
		return
	}
	d.up = true
	d.reply = resp

	// A refusal is still a reachable daemon. Say what it said; checkProtocol judges it.
	if resp.Reason != "" {
		d.add("daemon", pass,
			fmt.Sprintf("answering on %s, and refusing this request: %s", short(endpoint), resp.Reason), "")
		return
	}
	d.add("daemon", pass,
		fmt.Sprintf("answering on %s — budget %d, %d in use, %d waiting",
			short(endpoint), resp.Budget, resp.InUse, resp.Waiting), "")
}

// ---------------------------------------------------------------------------
// socket
// ---------------------------------------------------------------------------

// checkSocket inspects the endpoint's socket file and the directory holding it.
//
// The expectations are not invented here: transport.Listen creates the directory 0700 and
// chmods the socket 0600, because the socket carries this user's session and lease state
// and v1 is single-user. Anything looser means something other than this daemon made the
// file — and a socket another local user can write to is a socket another local user can
// take leases on, starving this one's agents from outside the budget entirely.
func (d *diagnosis) checkSocket() {
	if d.endpoint == "" {
		d.add("socket", skip, "no endpoint to inspect", "")
		return
	}
	path, err := socketPath(d.endpoint)
	if err != nil {
		d.add("socket", fail, err.Error(),
			fmt.Sprintf("unset %s, or set it to a unix:// URL", transport.EndpointEnv))
		return
	}

	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if !d.up {
			// Same cause as the daemon check, which has already named the fix.
			d.add("socket", skip, fmt.Sprintf("no socket at %s (the daemon is not running)", short(path)), "")
			return
		}
		d.add("socket", fail,
			fmt.Sprintf("a daemon answered but there is no socket at %s", short(path)),
			"restart the daemon so the endpoint and the socket agree: governor install")
		return
	}
	if err != nil {
		d.add("socket", fail, fmt.Sprintf("cannot stat %s: %v", short(path), err),
			fmt.Sprintf("check the permissions on %s", short(filepath.Dir(path))))
		return
	}

	if fi.Mode()&os.ModeSocket == 0 {
		// transport.Listen refuses to replace this rather than eating a file it did not
		// create, so the daemon will not start until it is moved out of the way.
		d.add("socket", fail,
			fmt.Sprintf("%s exists and is not a socket (mode %s) — the daemon refuses to replace it",
				short(path), fi.Mode()),
			fmt.Sprintf("mv %s %s.bak && governor install", path, path))
		return
	}

	facts := socketFacts{path: path, self: os.Getuid(), perm: fi.Mode().Perm()}
	facts.owner, facts.ownerKnown = ownerUID(fi)
	if dfi, err := os.Stat(filepath.Dir(path)); err == nil {
		facts.dirPerm, facts.dirKnown = dfi.Mode().Perm(), true
	}

	problems, fixes := facts.faults()
	if len(problems) > 0 {
		d.add("socket", fail, short(path)+": "+strings.Join(problems, "; "), strings.Join(fixes, " ; "))
		return
	}
	d.add("socket", pass, fmt.Sprintf("%s is yours, mode %04o", short(path), fi.Mode().Perm()), "")
}

// socketFacts is an inspected socket, reduced to the things worth judging.
//
// It is a struct passed to a pure function rather than three more branches inside
// checkSocket for one reason: a test cannot create a socket owned by another user without
// root, so the ownership branch would otherwise be code that only ever runs on someone
// else's machine, at the moment it matters most.
type socketFacts struct {
	path string
	// owner is the uid owning the socket; ownerKnown is false where the platform does not
	// carry one.
	owner      int
	ownerKnown bool
	self       int
	perm       os.FileMode
	// dirPerm is the mode of the directory holding it; dirKnown is false when it could not
	// be read.
	dirPerm  os.FileMode
	dirKnown bool
}

// faults reports what is wrong with an inspected socket, and the fix for each.
//
// The expectations are transport.Listen's, not invented here: it creates the directory 0700
// and chmods the socket 0600, because the socket carries this user's session and lease state
// and v1 is single-user. Anything looser means something other than this daemon made the
// file — and a socket another local user can write to is a socket another local user can
// take leases on, starving this one's agents from outside the budget entirely.
func (f socketFacts) faults() (problems, fixes []string) {
	if f.ownerKnown && f.owner != f.self {
		problems = append(problems, fmt.Sprintf("owned by uid %d, not by you (uid %d)", f.owner, f.self))
		fixes = append(fixes, fmt.Sprintf("stop that user's daemon, then: rm %s && governor install", f.path))
	}
	if f.perm&0o077 != 0 {
		problems = append(problems, fmt.Sprintf("mode %04o: other users on this machine can take leases from your budget", f.perm))
		fixes = append(fixes, fmt.Sprintf("chmod 600 %s", f.path))
	}
	// Group or other WRITE on the directory only. A socket is reached by writing to it, and
	// the directory's own read bit does not grant that — refusing a 0755 state directory
	// would be a fault reported about a machine that is fine.
	if f.dirKnown && f.dirPerm&0o022 != 0 {
		problems = append(problems, fmt.Sprintf("its directory is mode %04o: another user can replace the socket", f.dirPerm))
		fixes = append(fixes, fmt.Sprintf("chmod 700 %s", filepath.Dir(f.path)))
	}
	return problems, fixes
}

// ownerUID reads the owning uid out of a FileInfo, and reports false where the platform
// does not carry one. Both v1 platforms do (design.md section 13.5).
func ownerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// ---------------------------------------------------------------------------
// hooks
// ---------------------------------------------------------------------------

// wantedHook is one of the five registrations of design.md section 4.1.
//
// The key is (event, args): an entry on the right event running the wrong subcommand is not
// the hook it looks like. A SubagentStart wired to `governor release` would return leases at
// the moment they should be bound, and every symptom of it would look like a budget bug
// rather than a wiring one.
type wantedHook struct {
	event string
	args  string
	// matched is whether a matcher is required on this event, which is true only of the
	// gate. checkMatcher does the judging; this only says whose matcher to judge.
	matched bool
}

var wantedHooks = []wantedHook{
	{event: "PreToolUse", args: "gate", matched: true},
	{event: "SubagentStart", args: "confirm"},
	{event: "SubagentStop", args: "release"},
	{event: "SessionStart", args: "session start"},
	{event: "SessionEnd", args: "session end"},
}

// hookRef is one governor hook registration, as found on disk.
type hookRef struct {
	event   string
	matcher string
	// command is the command string verbatim, before expansion.
	command string
	// bin is the binary the command runs, with ${CLAUDE_PLUGIN_ROOT} and friends expanded,
	// or "" when the command is not a simple <path> <args...>.
	bin string
	// args is everything after the binary, joined — "session start", "gate".
	args string
	// source is the file it was found in.
	source string
}

// hookSource is a file that can hold hook registrations.
type hookSource struct {
	path string
	// pluginRoot is what ${CLAUDE_PLUGIN_ROOT} expands to for commands in this file, or ""
	// when the file is not a plugin's.
	pluginRoot string
}

// hookSources are the two places a governor hook can be registered.
//
// All three are named by design.md section 13.3. GOV-21 settled where install writes — the
// plugin's own hooks/hooks.json under ~/.claude/skills — and the settings file is still read
// because a user may wire a hook there by hand, which is the supported way to keep one of
// their own.
func hookSources() ([]hookSource, error) {
	settings, err := settingsPath()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	// The directory `governor install` actually writes, and the only one of the three a
	// current Claude Code loads. GOV-21 measured it: ~/.claude/skills/<name>/ auto-loads as
	// <name>@skills-dir with no settings entry, and its hooks/hooks.json fires
	// (hook-surface.md section 15).
	pluginRoot := filepath.Join(home, ".claude", "skills", pluginName)
	// The directory design.md section 13.3 named before that measurement. It is inert on
	// 2.1.265 and install no longer writes it, but it is still read here: a machine
	// installed by an earlier build has hooks sitting in it, and a doctor that could not see
	// them would report "not registered" for a file it is looking straight at, which is
	// worse than the original mistake.
	legacyRoot := filepath.Join(home, ".claude", "plugins", pluginName)
	return []hookSource{
		{path: settings},
		{path: filepath.Join(pluginRoot, "hooks", "hooks.json"), pluginRoot: pluginRoot},
		{path: filepath.Join(legacyRoot, "hooks", "hooks.json"), pluginRoot: legacyRoot},
	}, nil
}

// gateHooksInstalledAt is when the file holding the PreToolUse gate hook was last written,
// or the zero time when no such registration exists anywhere.
//
// This is the pivot GOV-29 turns on. Hooks bind at session start (docs/hook-surface.md
// section 17), so a session that started before this instant is running whatever hooks
// existed then — for a session older than the install, none — and no amount of restarting the
// daemon will govern it. Only restarting the session will.
//
// IT MUST BE THE FILE THAT ACTUALLY HOLDS THE GATE, not the newest of the files that could.
// The first thing hookSources lists is ~/.claude/settings.json, which is the USER'S file:
// accepting a permission prompt, editing /config or installing any other tool rewrites it.
// Taking the newest mtime across all three — which this did until it was reviewed — meant an
// unrelated settings edit moved this instant to now, placed every live session before it, and
// reported the whole machine as ungoverned with a fix telling the user to restart six working
// sessions. That is the false-positive the story calls a serious defect, fired by ordinary
// use rather than by an edge case.
//
// It is a sound marker rather than a churning one because installer.writeFile leaves a file
// whose content already matches strictly alone, so reinstalling the same build does not move
// it.
func gateHooksInstalledAt() time.Time {
	sources, err := hookSources()
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, src := range sources {
		refs, err := scanHooks(src)
		if err != nil {
			continue
		}
		holdsGate := false
		for _, ref := range refs {
			// Matched on event AND subcommand, the same pair wantedHooks uses. A file
			// carrying somebody else's PreToolUse hook is not a file that governs spawns.
			if ref.event == "PreToolUse" && ref.args == "gate" {
				holdsGate = true
				break
			}
		}
		if !holdsGate {
			continue
		}
		fi, err := os.Stat(src.path)
		if err != nil {
			continue
		}
		// Newest across the files that DO hold a gate, because where a gate is registered in
		// more than one place the last write is what describes a session starting now.
		if m := fi.ModTime(); m.After(newest) {
			newest = m
		}
	}
	return newest
}

// checkHooks reports whether all five hooks are registered and point at a binary that runs.
func (d *diagnosis) checkHooks() {
	sources, err := hookSources()
	if err != nil {
		d.add("hooks", fail, fmt.Sprintf("cannot locate the settings file: %v", err),
			"set HOME, then: governor install")
		return
	}
	// The first source is the settings file, and it is the one a report names: it is where
	// install merges the hooks and the only one of the two a user edits by hand.
	settings := sources[0].path

	var unreadable []string
	seen := map[string]bool{}
	for _, src := range sources {
		refs, err := scanHooks(src)
		if err != nil {
			unreadable = append(unreadable, err.Error())
			continue
		}
		d.hooks = append(d.hooks, refs...)
		if len(refs) > 0 {
			seen[src.path] = true
		}
	}

	var missing []string
	var broken []string
	for _, w := range wantedHooks {
		ref := d.findHook(w)
		if ref == nil {
			if wrong := d.misregistered(w); wrong != "" {
				broken = append(broken, wrong)
				continue
			}
			missing = append(missing, w.event+" → governor "+w.args)
			continue
		}
		if w.matched {
			d.gate = ref
		}
		switch {
		case ref.bin == "":
			broken = append(broken, fmt.Sprintf("%s runs %q, which is not a plain command this can check",
				w.event, ref.command))
		case !executableExists(ref.bin):
			broken = append(broken, fmt.Sprintf("%s points at %s, which is not an executable file",
				w.event, short(ref.bin)))
		}
	}

	// An unreadable settings file is reported whatever else is true: it is why the hooks
	// look missing, and telling someone to install over a file nothing can parse would have
	// them run a command that refuses.
	if len(unreadable) > 0 {
		d.add("hooks", fail, strings.Join(unreadable, "; "),
			"fix or move that file, then: governor install")
		return
	}
	// Missing and broken are reported together rather than one at a time. A settings file
	// with three hooks absent and a fourth wired to the wrong subcommand has two distinct
	// problems, and a report that named only the first would send someone back here for a
	// second round after fixing it.
	var problems []string
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d not registered in %s: %s",
			len(missing), len(wantedHooks), short(settings), strings.Join(missing, ", ")))
	}
	problems = append(problems, broken...)
	if len(problems) > 0 {
		d.add("hooks", fail, strings.Join(problems, "; "), "governor install")
		return
	}
	d.add("hooks", pass,
		// "on disk" is not padding. This check reads a FILE; it cannot tell whether any
		// running session has loaded what is in it, and hooks bind at session start so the
		// two genuinely come apart. Reported as "all 5 registered" alone, it read as proof
		// that the machine was governed on a day when four of six sessions were not — see
		// the sessions check below, which is the half that can actually tell.
		fmt.Sprintf("all %d registered on disk in %s → %s",
			len(wantedHooks), shortList(sortedKeys(seen)), short(d.gate.bin)), "")
}

// misregistered describes a governor hook sitting on a wanted event under the wrong
// subcommand, or "" when there is none.
//
// It is worth a sentence of its own because the two failures read identically from the
// outside and have opposite causes. A SubagentStart wired to `governor release` returns
// leases at the moment they should be bound, and every symptom of that is a budget that
// drifts — which sends someone looking at the daemon rather than at one word in their
// settings file.
func (d *diagnosis) misregistered(w wantedHook) string {
	for _, h := range d.hooks {
		if h.event == w.event && h.args != w.args {
			return fmt.Sprintf("%s runs `governor %s` instead of `governor %s`", w.event, h.args, w.args)
		}
	}
	return ""
}

// findHook returns the registration for one wanted hook, or nil.
func (d *diagnosis) findHook(w wantedHook) *hookRef {
	for i := range d.hooks {
		if d.hooks[i].event == w.event && d.hooks[i].args == w.args {
			return &d.hooks[i]
		}
	}
	return nil
}

// scanHooks reads one file's governor hook registrations.
//
// A file that does not exist yields nothing and no error: neither of the two sources is
// required to be present, and the finding a person needs is "the hooks are not registered",
// not "a file you have never heard of is missing".
func scanHooks(src hookSource) ([]hookRef, error) {
	raw, err := os.ReadFile(src.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", short(src.path), err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: hooks are not in the expected shape (%v)", short(src.path), err)
	}

	var out []hookRef
	for event, entries := range doc.Hooks {
		for _, entry := range entries {
			for _, h := range entry.Hooks {
				// The Phase 0 trace hooks are governor hooks pointing at a governor
				// binary, and they are not this command's business: they are a separate
				// family that design.md section 9 deletes when Phase 1 lands. Left in, they
				// would answer the binary check on a machine with no gate installed at all,
				// and a `governor trace ensure` on SessionStart would be reported as the
				// wrong subcommand for a hook it has nothing to do with.
				if isTraceCommand(h.Command) {
					continue
				}
				bin, args, ok := splitHookCommand(h.Command, src)
				if !ok || !isGovernorBinary(bin) {
					continue
				}
				out = append(out, hookRef{
					event: event, matcher: entry.Matcher, command: h.Command,
					bin: bin, args: args, source: src.path,
				})
			}
		}
	}
	// Deterministic, so two runs against one file report the same hook first.
	sort.Slice(out, func(i, j int) bool {
		if out[i].event != out[j].event {
			return out[i].event < out[j].event
		}
		return out[i].args < out[j].args
	})
	return out, nil
}

// splitHookCommand expands a hook command and splits it into binary and arguments.
//
// Expansion has to happen before anything else looks at the string. design.md section 4.1
// registers the hooks as "${CLAUDE_PLUGIN_ROOT}/bin/governor gate", so a doctor that stat'd
// the command verbatim would report "not an executable file" for every correctly installed
// plugin on earth — the exact false alarm that teaches people to ignore a diagnostic.
//
// ok is false for a command this cannot decompose: a shell pipeline, a quoted path with
// spaces, anything with a metacharacter in it. That is reported as "not a plain command
// this can check" rather than guessed at, because the alternative to guessing wrong here is
// telling someone their working hook is broken.
func splitHookCommand(cmd string, src hookSource) (bin, args string, ok bool) {
	cmd = strings.TrimSpace(expandHookVars(cmd, src))
	if cmd == "" {
		return "", "", false
	}
	if strings.ContainsAny(cmd, "#;&|<>$`\"'*?") {
		return "", "", false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", "", false
	}
	return fields[0], strings.Join(fields[1:], " "), true
}

// expandHookVars substitutes the variables Claude Code expands in a hook command, plus a
// leading ~.
//
// CLAUDE_PLUGIN_ROOT is taken from the environment when it is set — a doctor run from
// inside a plugin's own context has the real value — and otherwise from the plugin
// directory the file was found in, which is the same thing by construction.
func expandHookVars(cmd string, src hookSource) string {
	root := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if root == "" {
		root = src.pluginRoot
	}
	for _, v := range []struct{ name, value string }{
		{"CLAUDE_PLUGIN_ROOT", root},
		{"CLAUDE_PLUGIN_DATA", os.Getenv(config.DataDirEnv)},
	} {
		if v.value == "" {
			continue
		}
		cmd = strings.ReplaceAll(cmd, "${"+v.name+"}", v.value)
		cmd = strings.ReplaceAll(cmd, "$"+v.name, v.value)
	}
	if strings.HasPrefix(cmd, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cmd = filepath.Join(home, cmd[2:])
		}
	}
	return cmd
}

// isGovernorBinary reports whether a hook command's binary is ours.
//
// Decided by the binary's NAME rather than by an exact path, for the reason trace_install.go
// gives at isTraceCommand: the release binaries are governor-darwin-arm64 and friends,
// install.sh may place one anywhere, and `go test` builds it as governor.test. A check keyed
// on one exact path would report every one of those as "no governor hooks registered".
func isGovernorBinary(bin string) bool {
	return bin != "" && strings.Contains(strings.ToLower(filepath.Base(bin)), "governor")
}

// executableExists reports whether path is a regular file anyone can execute.
func executableExists(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// ---------------------------------------------------------------------------
// matcher
// ---------------------------------------------------------------------------

// checkMatcher is the one that earns its keep.
//
// A wrong matcher is the failure with no symptom: the hook is registered, the binary exists,
// the daemon is healthy, and the gate is simply never invoked. Nothing errors and nothing is
// governed, and it is found weeks later or not at all — which is why GOV-01 was spent
// measuring the real tool name in the first place.
//
// The comparison is against docs/hook-surface.md section 1 by way of spawnToolNames: on
// 2.1.263 the payload's tool_name is Agent, Task is carried as a live alias within the same
// version, and SendMessage is a second spawn path GOV-28 added. A matcher that misses any of
// the three leaves that path ungoverned.
func (d *diagnosis) checkMatcher() {
	if d.gate == nil {
		// The hooks check has already failed and named the fix. A second failure here
		// would be a second fix for one cause.
		d.add("matcher", skip, "no PreToolUse gate hook registered (see hooks above)", "")
		return
	}
	want := strings.Join(spawnToolNames, "|")

	if d.gate.matcher == "" {
		// An absent matcher is not a missing hook — the harness runs an unmatched
		// PreToolUse for EVERY tool call. The gate would still fire on a spawn, so this is
		// not silent in the dangerous direction; it is this binary executed on every Bash,
		// Read and Edit in every session on the machine. design.md section 13.3 records the
		// same hazard for the Phase 0 PostToolUse hook, where the matcher is called
		// load-bearing rather than tidy.
		d.add("matcher", fail,
			"the PreToolUse gate hook has no matcher: it runs governor on every tool call in every session",
			fmt.Sprintf("set \"matcher\": %q on that hook, or re-run: governor install", want))
		return
	}

	re, err := regexp.Compile(d.gate.matcher)
	if err != nil {
		// An uncompilable matcher is the silent case in full: nothing to match against, so
		// nothing ever fires.
		d.add("matcher", fail,
			fmt.Sprintf("the PreToolUse matcher %q is not a valid expression (%v), so the gate never fires",
				d.gate.matcher, err),
			fmt.Sprintf("set \"matcher\": %q on that hook, or re-run: governor install", want))
		return
	}

	var missed []string
	for _, name := range spawnToolNames {
		if !re.MatchString(name) {
			missed = append(missed, name)
		}
	}
	if len(missed) > 0 {
		// Note what is NOT accepted here: a matcher of exactly "Task" happens to fire on
		// 2.1.263, because the harness carries Task as an alias for the tool whose payload
		// says Agent (docs/hook-surface.md section 1). design.md section 10.1 is explicit
		// that registering only Task "would have worked by luck", and a diagnostic that
		// passes something for working by luck is a diagnostic that will pass it on the
		// version where the luck runs out.
		d.add("matcher", fail,
			fmt.Sprintf("the PreToolUse matcher %q does not match %s — those spawns are never governed",
				d.gate.matcher, strings.Join(missed, ", ")),
			fmt.Sprintf("set \"matcher\": %q on that hook, or re-run: governor install", want))
		return
	}
	d.add("matcher", pass,
		fmt.Sprintf("%q matches %s", d.gate.matcher, strings.Join(spawnToolNames, ", ")), "")
}

// ---------------------------------------------------------------------------
// versions: protocol, binary, config schema
// ---------------------------------------------------------------------------

// checkProtocol compares the daemon's protocol version with this binary's.
//
// This is the first of the three halves of "binary, protocol and config schema versions
// mutually consistent". They are three findings rather than one because they have three
// different fixes, and a single line carrying three of them is a line nobody acts on.
//
// A disagreement here is not cosmetic. Every hook treats an unrecognised version exactly as
// it treats an unreachable daemon and fails open (invariant 8), so the whole machine is
// silently ungoverned while a healthy-looking daemon sits there answering.
func (d *diagnosis) checkProtocol() {
	if !d.up {
		d.add("protocol", skip, "no daemon to compare versions with (see daemon above)", "")
		return
	}
	if d.reply.V == protocolVersion && d.reply.Reason == "" {
		d.add("protocol", pass, fmt.Sprintf("daemon and this binary both speak v%d", protocolVersion), "")
		return
	}
	detail := fmt.Sprintf("the daemon speaks protocol v%d and this binary speaks v%d",
		d.reply.V, protocolVersion)
	if len(d.reply.Supported) > 0 {
		detail += fmt.Sprintf(" (it supports %v)", d.reply.Supported)
	}
	detail += " — every hook is failing open, so nothing is governed"
	d.add("protocol", fail, detail,
		"restart the daemon on the same build as the hooks: governor install")
}

// checkBinary compares the binary the hooks run with the one running this check.
//
// The hooks and the daemon are separate processes started at separate times, and after an
// upgrade there is nothing that forces them to be the same build. This asks the hooked
// binary what it is, by running it — the one subprocess in this file, bounded, and only ever
// a path that this file has already established is a governor binary.
func (d *diagnosis) checkBinary() {
	// Only the binaries behind the five hooks of section 4.1. A governor command wired to
	// some other event by hand is the user's business and not evidence about the gate's
	// build; counting it would report "the hooks point at 2 different binaries" for a
	// perfectly good install with one extra hook in it.
	bins := map[string]bool{}
	for _, w := range wantedHooks {
		if ref := d.findHook(w); ref != nil && ref.bin != "" {
			bins[ref.bin] = true
		}
	}
	switch len(bins) {
	case 0:
		d.add("binary", skip, "no hooked binary to check (see hooks above)", "")
		return
	case 1:
	default:
		d.add("binary", fail,
			fmt.Sprintf("the hooks point at %d different binaries: %s",
				len(bins), shortList(sortedKeys(bins))),
			"governor install")
		return
	}
	bin := sortedKeys(bins)[0]

	self, err := os.Executable()
	if err == nil {
		if abs, err := filepath.Abs(self); err == nil {
			self = abs
		}
	}
	if sameFile(bin, self) {
		d.add("binary", pass, fmt.Sprintf("the hooks run this binary (%s %s)", short(bin), binaryVersion), "")
		return
	}

	v, err := hookedBinaryVersions(bin)
	if err != nil {
		d.add("binary", fail,
			fmt.Sprintf("%s did not answer `version` (%v)", short(bin), err),
			"governor install")
		return
	}
	var diffs []string
	if v.binary != binaryVersion {
		diffs = append(diffs, fmt.Sprintf("binary %s vs %s", v.binary, binaryVersion))
	}
	if v.protocol != protocolVersion {
		diffs = append(diffs, fmt.Sprintf("protocol %d vs %d", v.protocol, protocolVersion))
	}
	if v.schema != configSchemaVersion {
		diffs = append(diffs, fmt.Sprintf("config schema %d vs %d", v.schema, configSchemaVersion))
	}
	if len(diffs) > 0 {
		d.add("binary", fail,
			fmt.Sprintf("the hooks run %s, which is a different build: %s",
				short(bin), strings.Join(diffs, ", ")),
			"governor install")
		return
	}
	d.add("binary", pass,
		fmt.Sprintf("the hooks run %s, same versions as this build (%s)", short(bin), v.binary), "")
}

// hookedVersions is what `governor version` reports.
type hookedVersions struct {
	binary   string
	protocol int
	schema   int
}

// hookedBinaryVersions runs `<bin> version` and parses its three lines.
//
// It parses the plain-text output rather than asking for JSON because that output is a
// stable, tested contract (TestVersionReportsAllThreeVersions) and adding a second
// machine-readable form of the same three numbers would be a second thing to keep true.
func hookedBinaryVersions(bin string) (hookedVersions, error) {
	ctx, cancel := context.WithTimeout(context.Background(), doctorExecBudget)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "version")
	// Nothing on stdin: a binary that is not what it claims to be must not be able to hang
	// this by waiting for input.
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		return hookedVersions{}, err
	}

	var v hookedVersions
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "governor "):
			v.binary = strings.TrimPrefix(line, "governor ")
		case strings.HasPrefix(line, "protocol "):
			fmt.Sscanf(line, "protocol %d", &v.protocol)
		case strings.HasPrefix(line, "config schema "):
			fmt.Sscanf(line, "config schema %d", &v.schema)
		}
	}
	if v.binary == "" {
		return hookedVersions{}, fmt.Errorf("output is not a governor version report")
	}
	return v, nil
}

// checkConfig compares the config file's schema version with this binary's.
//
// Absent is a pass, and deliberately so: design.md section 13.4 has migration happen on
// read and `calibrate` write the file, so a machine that has never been calibrated has no
// config and is running on built-in defaults, which is a state and not a fault.
//
// A config OLDER than this binary is also a pass. Migrating it forward is exactly what the
// config.Store contract promises, so reporting it would be reporting a supported case.
// Only a config from the FUTURE is a failure, because that is the one this binary cannot
// migrate — it does not know what changed.
func (d *diagnosis) checkConfig() {
	path, err := config.Path()
	if err != nil {
		d.add("config", fail, fmt.Sprintf("cannot locate the config: %v", err),
			fmt.Sprintf("set %s or HOME", config.DataDirEnv))
		return
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		d.add("config", pass,
			fmt.Sprintf("no config at %s; built-in defaults apply", short(path)), "")
		return
	}
	if err != nil {
		d.add("config", fail, fmt.Sprintf("cannot read %s: %v", short(path), err),
			fmt.Sprintf("check the permissions on %s", short(path)))
		return
	}
	var doc struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		d.add("config", fail, fmt.Sprintf("%s is not valid JSON (%v)", short(path), err),
			fmt.Sprintf("mv %s %s.bak && governor calibrate", path, path))
		return
	}
	if doc.Schema == nil {
		d.add("config", fail,
			fmt.Sprintf("%s carries no \"schema\" key, so nothing can tell which version wrote it", short(path)),
			fmt.Sprintf("mv %s %s.bak && governor calibrate", path, path))
		return
	}
	if *doc.Schema > configSchemaVersion {
		d.add("config", fail,
			fmt.Sprintf("%s is schema %d and this binary understands %d — it was written by a newer governor",
				short(path), *doc.Schema, configSchemaVersion),
			"upgrade this binary, or: mv "+path+" "+path+".bak && governor calibrate")
		return
	}
	if *doc.Schema < configSchemaVersion {
		d.add("config", pass,
			fmt.Sprintf("%s is schema %d, migrated forward to %d on read",
				short(path), *doc.Schema, configSchemaVersion), "")
		return
	}
	d.add("config", pass, fmt.Sprintf("%s is schema %d", short(path), *doc.Schema), "")
}

// ---------------------------------------------------------------------------
// leases
// ---------------------------------------------------------------------------

// checkLeases reports leases the daemon cannot reclaim by watching their owner die.
//
// "Stranded" is a precise thing here rather than a feeling. The reaper collects a lease
// three ways: its TTL expires, its session's process is gone, or — for a confirmed lease —
// Liveness says its owner is gone. All three of those need an owning PID, which is bound at
// `session start` and at `confirm`. A lease held under a session with NO pid has none of
// them available to it: nothing can be asked about it, and it holds its slot until the TTL
// runs out, which for a confirmed lease is half an hour.
//
// The other case, a lease under a session whose pid is dead, should be transient — the
// reaper runs every two seconds. Seeing one from here therefore says something stronger
// than "a lease is stuck": it says the reaper is not doing its job.
//
// The liveness question is asked with the same PIDLiveness the daemon's own reaper uses, so
// doctor and the daemon cannot disagree about who is alive. The status reply carries a pid
// but not its start time, so a recycled pid reads as alive and this under-reports rather
// than over-reports. That is the correct direction for a diagnostic: a false "your leases
// are stranded" would send someone to restart a daemon that is working.
func (d *diagnosis) checkLeases() {
	if !d.up {
		d.add("leases", skip, "no daemon to read the lease table from (see daemon above)", "")
		return
	}
	held, ownerless, dead := 0, 0, 0
	var names []string
	for _, s := range d.reply.Sessions {
		n := s.Tentative + s.Confirmed
		if n == 0 {
			continue
		}
		held += n
		switch {
		case s.PID <= 0:
			ownerless += n
			names = append(names, fmt.Sprintf("%s (%d, no owning pid)", sessionLabel(s.Session), n))
		case !lease.PIDLiveness{}.Alive(lease.Lease{OwnerPID: s.PID}):
			dead += n
			names = append(names, fmt.Sprintf("%s (%d, pid %d is gone)", sessionLabel(s.Session), n, s.PID))
		}
	}
	if held == 0 {
		d.add("leases", pass, "no leases held", "")
		return
	}
	if ownerless == 0 && dead == 0 {
		d.add("leases", pass,
			fmt.Sprintf("%d held, every one by a live session", held), "")
		return
	}
	// Leases live only in the daemon's memory, so a fresh daemon has none. The fix is
	// therefore a restart, and it has to be one a person can type: `governor install` starts
	// a daemon only when none is answering, so telling someone to run it while the stuck one
	// is still up would name a command that does nothing. The pid file GOV-21 added is what
	// makes the first half precise.
	fix := "restart the daemon to clear them: kill $(cat " + short(d.pidPath()) + ") && governor install"
	if ownerless > 0 {
		// The cause, not the symptom: an ownerless lease means the session was never
		// registered, which means the SessionStart hook did not run.
		fix = "register sessions so their leases can be reclaimed — check the SessionStart hook is wired: governor install"
	}
	d.add("leases", fail,
		fmt.Sprintf("%d of %d leases cannot be reclaimed by their owner: %s",
			ownerless+dead, held, strings.Join(names, ", ")),
		fix)
}

// pidPath is where the daemon serving this diagnosis's endpoint keeps its pid.
//
// Derived from the endpoint rather than assumed, so that a report about a daemon on a
// non-default endpoint names that daemon's pid file and not the default one's — a fix that
// kills the wrong process is worse than no fix at all.
func (d *diagnosis) pidPath() string {
	if path, err := daemonPIDPath(d.endpoint); err == nil {
		return path
	}
	if dir, err := transport.StateDir(); err == nil {
		return filepath.Join(dir, daemonPIDFile)
	}
	return daemonPIDFile
}

// sessionLabel shortens a session id for a report. Session ids are uuids and the first
// segment is enough to tell two apart in a diagnostic.
func sessionLabel(id string) string {
	if id == "" {
		return "an unregistered session"
	}
	if i := strings.IndexByte(id, '-'); i > 0 {
		return "session " + id[:i]
	}
	if len(id) > 8 {
		return "session " + id[:8]
	}
	return "session " + id
}

// ---------------------------------------------------------------------------
// formatting
// ---------------------------------------------------------------------------

// short abbreviates the home directory to ~ so a path fits on a line and reads as the one
// the user knows.
func short(s string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return s
	}
	if strings.HasPrefix(s, home+string(filepath.Separator)) {
		return "~" + s[len(home):]
	}
	// Endpoint URLs carry the same path one scheme along.
	if i := strings.Index(s, home+string(filepath.Separator)); i > 0 {
		return s[:i] + "~" + s[i+len(home):]
	}
	return s
}

// lastLine is the final non-empty line of a diagnostic, trimmed of the "governor: " prefix
// every writer in this binary puts on it.
//
// The last one rather than the first because the writers here report in order of
// specificity — resolve the endpoint, dial it, set a deadline, write, read — so the last
// thing said before giving up is the thing that actually went wrong.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return strings.TrimPrefix(l, "governor: ")
		}
	}
	return "no diagnostic"
}

func shortList(ss []string) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, short(s))
	}
	return strings.Join(out, ", ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameFile reports whether two paths name the same file on disk, following symlinks.
//
// Compared by identity rather than by string because the hook may name the binary through a
// symlink, a relative path or an install directory that is itself a link — and reporting
// "the hooks run a different build" about a path that is this very process is the kind of
// false alarm that gets a diagnostic ignored.
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
