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
	"runtime"
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
	// warn is something found, worth saying, and not a defect in this installation.
	//
	// It exists because GOV-B11 needed a check whose finding is "untidy, not broken": a
	// foreign governor daemon on a scratch socket collides with nothing and double-leases
	// nothing, so failing on it would cry wolf, and passing on it is how four of them ran
	// for up to 28 days while doctor reported ten checks all passing. Neither existing state
	// could say it. A warning appears in the list, is counted separately in the summary, and
	// deliberately does NOT move the exit code — a warning that fails the command is a
	// failure wearing a different label.
	//
	// GOV-30 GENERALISES THIS, and should extend it rather than re-derive it. That story is
	// about checks that report `ok` when they could not actually verify anything — spawn
	// against an unmeasured Claude Code version, sessions before the daemon could say which
	// were live. Those are a different finding ("could not verify") from this one ("verified,
	// and found something untidy"), and GOV-30 may well want a fourth state or a different
	// exit-code rule for them. What it should not have to do is invent the plumbing again.
	warn
	// unverified is a check that could not answer its own question. Not "I looked and it is
	// fine", not "I looked and it is broken" — "I could not look".
	//
	// GOV-30 added it, taking the invitation above. The case that forced it: the spawn check
	// compares the Claude Code version the tool names were measured against to the version
	// actually running, and when those differ it had no way to say so except `ok` with a
	// sentence of caveat after it. The summary then said "10 checks, all passing" and exit 0.
	// A stranger sees ten green lines. This repo's founding notice is that "a wrong tool name
	// means the hook silently never fires and everything downstream looks like it works" — so
	// the one check written to catch that was reporting the pre-failure condition as a pass.
	//
	// IT IS NOT A FAILURE, and that distinction is the whole point of adding a state rather
	// than reclassifying to fail. Nothing is known to be broken; something is unconfirmed. A
	// newer Claude Code is a routine event and failing on it would put a red line in front of
	// every user after every upgrade, which teaches them that a red line means nothing — the
	// exact cost checkSpawnTools's own comment was written to avoid.
	//
	// IT DOES MOVE THE EXIT CODE, to doctorExitUnverified rather than 1, because a report
	// that cannot be trusted must not be scriptable as success. Giving it its own code is
	// what lets `governor install` tell "this install is broken" from "this install is fine
	// and one claim is not yet earned" — see verifyInstall.
	unverified
)

// doctorExitUnverified is the exit status when nothing failed but at least one check could
// not verify what it claims.
//
// It is a third code because the two it sits between already mean something a script acts
// on: 0 is "this machine is good", 1 is "this machine is broken and here are the fixes",
// and 2 is "you typed the command wrong". "I could not tell" is none of those, and folding
// it into 1 would fail every `governor install` run on a Claude Code version nobody has
// measured yet — which is every version, eventually, since that is what measurement expiring
// means.
const doctorExitUnverified = 3

func (s checkState) label() string {
	switch s {
	case pass:
		return "ok"
	case fail:
		return "FAIL"
	case warn:
		return "warn"
	case unverified:
		return "unknown"
	case skip:
		return "skip"
	default:
		// Every state is named above. A new one reaching here would otherwise print as some
		// other state's label and be silently dropped by doctor_test's parser — the same
		// "mislabelled, and everything downstream looks fine" shape this command exists to
		// remove. So it is loud instead.
		return "BUG?"
	}
}

// finding is one check's verdict.
type finding struct {
	// name is the short column label — the subsystem, not the sentence.
	name string
	// state is pass, fail, skip, warn or unverified.
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
	// gate is the PreToolUse gate registration, if there is one. Several checks read it.
	gate *hookRef
	// matched holds every registration that requires a matcher, by event. checkMatcher walks
	// it so that a second matched hook is checked by construction rather than by somebody
	// remembering to add a case.
	matched map[string]*hookRef
	// strayMinAge overrides how old a foreign daemon must be before checkStrayDaemons reports
	// it. Zero means strayMinimumAge, which is what every caller outside a test leaves it at.
	strayMinAge time.Duration
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
	d.checkDaemonBuild()
	d.checkConfig()
	d.checkCalibration()
	d.checkBudget()
	d.checkSessions()
	d.checkLeases()
	d.checkStrayDaemons()
}

// ---------------------------------------------------------------------------
// stray daemons
// ---------------------------------------------------------------------------

// checkStrayDaemons reports governor daemons running on this machine other than this one.
//
// Last, because it is the only check that is not about whether THIS installation works. It
// answers a different question — what else is running that nobody asked for — and it is the
// question nothing in this project was asking when four orphaned daemons accumulated over
// twenty-eight days behind a clean bill of health. See strays.go for what it matches and,
// more importantly, what it deliberately does not.
func (d *diagnosis) checkStrayDaemons() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	own := 0
	if pid, ok := readPIDFileAt(d.pidPath()); ok {
		own = pid
	}
	sock, _ := socketPath(d.endpoint)

	minAge := d.strayMinAge
	if minAge == 0 {
		minAge = strayMinimumAge
	}
	strays := scanForStrays(ctx, own, sock, minAge)
	if len(strays) == 0 {
		d.add("strays", pass,
			"no other governor daemon is running on this machine", "")
		return
	}

	lines := make([]string, 0, len(strays))
	for _, s := range strays {
		lines = append(lines, s.describe())
	}
	// The plural is worth getting right: this output is read by someone deciding whether to
	// kill a process, and "1 other governor daemon" reads very differently from "4".
	noun := "daemon is"
	if len(strays) > 1 {
		noun = "daemons are"
	}
	d.add("strays", warn,
		fmt.Sprintf("%d other governor %s running, on their own sockets — nothing here is "+
			"double-leased, and a daemon you started yourself is expected:\n      %s",
			len(strays), noun, strings.Join(lines, "\n      ")),
		fmt.Sprintf("if you did not start it, stop it: kill %s", strayPIDs(strays)))
}

// strayPIDs renders the pids as a kill(1) argument list, so the fix is a command that can be
// typed rather than a suggestion to go and find them.
func strayPIDs(strays []strayDaemon) string {
	pids := make([]string, 0, len(strays))
	for _, s := range strays {
		pids = append(pids, strconv.Itoa(s.pid))
	}
	return strings.Join(pids, " ")
}

// readPIDFileAt reads a daemon pid file, tolerating every way it can be absent or malformed.
func readPIDFileAt(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
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
// describeSightings renders a version -> tools map as "Agent on 2.1.270; Task on 2.2.0",
// sorted, skipping the undated bucket's version label since there is none to print.
func describeSightings(by map[string][]string) string {
	out := make([]string, 0, len(by))
	for v, tools := range by {
		if len(tools) == 0 {
			continue
		}
		if v == "" {
			out = append(out, strings.Join(tools, ", ")+" on an unknown version")
			continue
		}
		out = append(out, fmt.Sprintf("%s on %s", strings.Join(tools, ", "), v))
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

func (d *diagnosis) checkSpawnTools() {
	names := strings.Join(spawnToolNames, ", ")

	// A daemon that predates GOV-30 sends no ledger at all, and an empty ledger from a daemon
	// that does is a different fact entirely: one is "nobody is recording", the other is
	// "recording, and nothing has spawned". Mirrors SessionsKnown, and for the same reason —
	// see the fix this branch names, which is an upgrade rather than an accusation.
	if !d.reply.SpawnSurfaceKnown {
		d.add("spawn", unverified, fmt.Sprintf(
			"%s — the daemon does not report which spawn tools it has caught, so nothing here confirms the matcher fires",
			names),
			"restart the daemon on this binary with `governor install` — a daemon older than "+
				"this build keeps no record of what the gate caught")
		return
	}

	// What the gate has actually been seen to catch, by Claude Code version — restricted to
	// the names that prove the SPAWN path is matched. See spawnConfirmingTools: a SendMessage
	// resume reaches the gate even on a version where the spawn tool has been renamed, so
	// counting it as confirmation would let a working tool cover for a broken one.
	confirmed := map[string][]string{}
	seenAnywhere := map[string][]string{}
	for _, o := range d.reply.SpawnSurface {
		var proving []string
		for _, tool := range o.Tools {
			if confirmsSpawnPath(tool) {
				proving = append(proving, tool)
			}
		}
		if len(o.Tools) > 0 {
			seenAnywhere[o.Version] = o.Tools
		}
		if len(proving) > 0 {
			confirmed[o.Version] = proving
		}
	}

	// The versions running here, which are the ones the claim has to hold for.
	var running []string
	seen := map[string]bool{}
	for _, s := range d.reply.Sessions {
		if !s.Live || s.Version == "" || seen[s.Version] {
			continue
		}
		seen[s.Version] = true
		running = append(running, s.Version)
	}
	sort.Strings(running)

	if len(running) == 0 {
		// No version could be read, so there is nothing to hold the claim against. Before
		// GOV-30 this said `ok` with a caveat, and on a host with no session registry that
		// was the DEFAULT output rather than a rare one — ten green lines on a machine where
		// nothing had been checked at all.
		detail := fmt.Sprintf("%s — no Claude Code version seen here, so nothing confirms the matcher fires on it", names)
		if caught := describeSightings(seenAnywhere); caught != "" {
			// The evidence is real but it cannot be dated, and a sighting that cannot be
			// dated cannot expire — which is the unfalsifiable claim this story removed. So
			// it is printed and still not treated as confirmation.
			detail = fmt.Sprintf("%s — the gate has caught %s, but no Claude Code version could be read here to date that against",
				names, caught)
		}
		d.add("spawn", unverified, detail,
			"nothing to type if this host keeps no session registry — the sessions check above "+
				"says which case this is; a sighting that cannot be dated cannot expire, so it "+
				"is not treated as confirmation")
		return
	}

	var missing []string
	var held []string
	for _, v := range running {
		if tools, ok := confirmed[v]; ok {
			held = append(held, fmt.Sprintf("%s on %s", strings.Join(tools, ", "), v))
			continue
		}
		missing = append(missing, v)
	}

	if len(missing) == 0 {
		// The strong statement, and the only one this check is now willing to make as a pass:
		// on every version running here, the harness has been observed to send a tool name
		// this matcher caught. That is not a belief about a version number, it is a payload.
		d.add("spawn", pass, fmt.Sprintf("%s — matcher confirmed firing by real spawns (%s)",
			names, strings.Join(held, "; ")), "")
		return
	}

	// UNVERIFIED, NOT FAILED. Silence has two causes and this check cannot tell them apart:
	// either nothing has spawned a subagent on that version yet, or Claude Code renamed the
	// tool and the matcher has stopped firing entirely. The first is routine; the second is
	// the failure this project exists to catch. Reporting `ok` covered both, which is the
	// defect GOV-30 was filed for.
	detail := fmt.Sprintf("%s — no spawn seen yet on Claude Code %s, so nothing confirms the matcher still fires there",
		names, joinClauses(missing))
	switch {
	case len(held) > 0:
		detail += fmt.Sprintf(" (confirmed on %s)", strings.Join(held, "; "))
	default:
		// Evidence from a version that is no longer running is still worth printing, and in
		// the case this check was written for it is the most informative line in the report:
		// "the matcher was firing on the version you were on yesterday and has not been seen
		// since you upgraded" is what a rename looks like from the outside. Without it the
		// reader cannot tell a matcher that broke from one that has simply never been tried.
		if last := describeSightings(confirmed); last != "" {
			detail += fmt.Sprintf(" (last seen firing: %s)", last)
		}
	}
	fix := "spawn a subagent in a session running " + missing[0]
	if len(missing) > 1 {
		fix = "spawn a subagent in a session on each of those versions"
	}
	d.add("spawn", unverified, detail, fix+", then re-run `governor doctor` — if it still says "+
		"this afterwards, the matcher is not firing and those spawns are not being governed")
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
		//
		// It USED TO BE A PASS whose detail led with "not verified", on the reasoning that
		// saying so plainly was enough. GOV-30 disagreed with that in general and with this
		// line in particular: the summary underneath still counted it toward "N checks, all
		// passing", and the one word a hurried reader takes from a report is the one in the
		// state column. An instrument that cannot read now says so in the column too.
		d.add("sessions", unverified,
			"not verified — the daemon did not say which sessions are live "+
				"(a daemon older than this binary, or a host with no session registry)",
			"upgrade the daemon with `governor install` if it is older than this binary; "+
				"otherwise this host keeps no session registry and there is nothing to fix")
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
		switch g := classifyGovernance(d.reply.statusResponse, s); g {
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

// stateColumn is the width of the leading state column, and fixIndent aligns a fix line
// under the check it belongs to.
//
// Seven, because "unknown" is seven characters. GOV-30 widened it from four and chose the
// whole word over an abbreviation that fit: this column is the first thing read and the
// state it was added for exists precisely because an unclear report cost a week of trusting
// green lines. fixIndent is stateColumn+2 and must stay so — doctor_test's parser attaches a
// fix line to the finding above it by that indent.
const (
	stateColumn = 7
	fixIndent   = stateColumn + 2
)

// joinClauses renders a list as prose: "a", "a and b", "a, b and c".
func joinClauses(c []string) string {
	switch len(c) {
	case 0:
		return ""
	case 1:
		return c[0]
	default:
		return strings.Join(c[:len(c)-1], ", ") + " and " + c[len(c)-1]
	}
}

// report prints the findings and returns the process exit code.
//
// Non-zero if any check failed, per GOV-20's acceptance criterion. It is 1 rather than
// 2, because 2 is what this binary already returns for a usage error and a script that
// cannot tell "your install is broken" from "you typed the command wrong" is a script that
// will conflate them. GOV-30 added a third non-zero code for a third thing a script needs to
// tell apart — see doctorExitUnverified.
func (d *diagnosis) report(stdout io.Writer) int {
	failed, warned, unknowns, skipped := 0, 0, 0, 0
	for _, f := range d.findings {
		if f.state == skip {
			// Counted so the summary's "N passing" does not quietly include checks that never
			// ran. Skips are only reachable downstream of a failure today, which takes the
			// other branch — but GOV-30 made the no-failures-with-extras branch reachable for
			// the first time, so the arithmetic now has to be right in a case it never met.
			skipped++
		}
		fmt.Fprintf(stdout, "%-*s  %-8s  %s\n", stateColumn, f.state.label(), f.name, f.detail)
		// A warning carries a fix too — it is something a person can act on — but it is not
		// a failure, so it neither counts toward the failure total nor changes the exit code.
		// An unverified check is the same shape: it is worth acting on (the fix is usually
		// what would let it verify) without being a defect.
		if f.state == warn || f.state == unverified {
			if f.state == warn {
				warned++
			} else {
				unknowns++
			}
			if f.fix != "" {
				fmt.Fprintf(stdout, "%*s%-8s  %s\n", fixIndent, "", "fix", f.fix)
			}
			continue
		}
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
		fmt.Fprintf(stdout, "%*s%-8s  %s\n", fixIndent, "", "fix", fix)
	}
	// The clauses the summary needs beyond the headline, in the order they read best. The
	// count is kept apart from the phrase because the two branches below word them
	// differently — "1 to look at" when nothing failed, "1 more to look at" when something
	// did — and reassembling one from the other by string surgery is how a summary line
	// starts lying about arithmetic.
	var extra []struct {
		n      int
		phrase string
	}
	if warned > 0 {
		extra = append(extra, struct {
			n      int
			phrase string
		}{warned, "to look at"})
	}
	if unknowns > 0 {
		extra = append(extra, struct {
			n      int
			phrase string
		}{unknowns, "that could not be verified"})
	}
	clause := func(prefix string) []string {
		out := make([]string, 0, len(extra))
		for _, e := range extra {
			out = append(out, fmt.Sprintf("%d %s%s", e.n, prefix, e.phrase))
		}
		return out
	}
	if failed == 0 {
		// "all passing" is a claim, and it may only be made when it is true. A run with a
		// warning in it is the exact case GOV-B11 was filed about — ten green lines above
		// four orphaned daemons — and a run with an unverified check in it is GOV-30's,
		// where the tenth green line was a check saying it had not looked. Both are named
		// here rather than rounded off.
		if len(extra) == 0 {
			fmt.Fprintf(stdout, "\n%d checks, all passing.\n", len(d.findings))
			return 0
		}
		clauses := append([]string{fmt.Sprintf("%d passing", len(d.findings)-warned-unknowns-skipped)}, clause("")...)
		fmt.Fprintf(stdout, "\n%d checks, %s.\n", len(d.findings), joinClauses(clauses))
		if unknowns > 0 {
			return doctorExitUnverified
		}
		// A warning on its own still exits 0: nothing here is broken.
		return 0
	}
	// A failure outranks everything below it. The exit code says "broken" even when something
	// also could not be verified, because that is the more urgent of the two and the fixes
	// printed above are for it.
	switch {
	case len(extra) == 0:
		fmt.Fprintf(stdout, "\n%d of %d checks failed.\n", failed, len(d.findings))
	case len(extra) == 1:
		fmt.Fprintf(stdout, "\n%d of %d checks failed, and %s.\n",
			failed, len(d.findings), clause("more ")[0])
	default:
		fmt.Fprintf(stdout, "\n%d of %d checks failed, with %s.\n",
			failed, len(d.findings), joinClauses(clause("")))
	}
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

// wantedHook is one of the six registrations of design.md section 4.1.
//
// The key is (event, args): an entry on the right event running the wrong subcommand is not
// the hook it looks like. A SubagentStart wired to `governor release` would return leases at
// the moment they should be bound, and every symptom of it would look like a budget bug
// rather than a wiring one.
type wantedHook struct {
	event string
	args  string
	// covers is the set of tool names this hook's matcher must match, or nil when the event
	// takes no matcher at all. checkMatcher does the judging; this only says what to judge
	// the matcher against.
	//
	// It replaced a bool when GOV-15 added a second matched hook. The two matchers are not
	// the same expression and must not be checked against one list: the gate has to cover
	// every spawn name AND Bash, while finish only ever wants Bash. A shared list would have
	// had doctor demand that PostToolUse match `Agent`, and an install satisfying it would
	// put a socket round trip on the end of every subagent spawn for nothing.
	covers []string
}

// matched reports whether this hook requires a matcher.
func (w wantedHook) matched() bool { return len(w.covers) > 0 }

var wantedHooks = []wantedHook{
	{event: "PreToolUse", args: "gate", covers: gatedToolNames},
	{event: "PostToolUse", args: "finish", covers: []string{buildToolName}},
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

// hookSources are the places a governor hook can be registered.
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
	// It was only ever written under the plugin's old name, so it is spelled with that one.
	legacyRoot := filepath.Join(home, ".claude", "plugins", legacyPluginName)
	// The tree an install from before the rename left (marketplace.go). Read so that a
	// machine still carrying it is seen to register every hook twice.
	renamedRoot := legacyPluginTree(home)
	return []hookSource{
		{path: settings},
		{path: filepath.Join(pluginRoot, "hooks", "hooks.json"), pluginRoot: pluginRoot},
		{path: filepath.Join(legacyRoot, "hooks", "hooks.json"), pluginRoot: legacyRoot},
		{path: filepath.Join(renamedRoot, "hooks", "hooks.json"), pluginRoot: renamedRoot},
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

// checkHooks reports whether all six hooks are registered and point at a binary that runs.
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
		if w.matched() {
			if d.matched == nil {
				d.matched = map[string]*hookRef{}
			}
			d.matched[w.event] = ref
		}
		if w.event == "PreToolUse" {
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
	// The gate in more than one file is not redundancy. Each loaded registration runs, so
	// every spawn takes one lease per copy and the budget fills at a fraction of its size.
	// The usual cause is the tree left from before the rename, which install removes.
	if gates := d.gateFiles(); len(gates) > 1 {
		problems = append(problems, fmt.Sprintf("the gate is registered in %d places (%s), so every spawn takes %d leases",
			len(gates), shortList(gates), len(gates)))
	}
	if len(problems) > 0 {
		d.add("hooks", fail, strings.Join(problems, "; "), "governor install")
		return
	}
	// Every file above can be right and none of it loaded. A marketplace plugin of the same
	// name takes precedence over the skills-dir tree (hook-surface.md section 23), so a copy
	// that is not a link to this tree would hold the hooks instead — and it carries no binary.
	if foreign, fix := foreignCopies(sources[1].pluginRoot); foreign != "" {
		d.add("hooks", fail, foreign, fix)
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

// gateFiles is every file that registers the PreToolUse gate, in the order they were read.
func (d *diagnosis) gateFiles() []string {
	var files []string
	seen := map[string]bool{}
	for _, h := range d.hooks {
		if h.event == "PreToolUse" && h.args == "gate" && !seen[h.source] {
			seen[h.source] = true
			files = append(files, h.source)
		}
	}
	return files
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
	for _, w := range wantedHooks {
		if w.matched() {
			d.checkOneMatcher(w)
		}
	}
}

// checkOneMatcher judges a single matched hook's matcher against the tool names it must cover.
func (d *diagnosis) checkOneMatcher(w wantedHook) {
	name := "matcher"
	if w.event != "PreToolUse" {
		// Two matchers means two findings, and a finding nobody can attribute to a hook is a
		// finding nobody can fix.
		name = "matcher:" + w.event
	}
	ref := d.matched[w.event]
	if ref == nil {
		// The hooks check has already failed and named the fix. A second failure here
		// would be a second fix for one cause.
		d.add(name, skip, fmt.Sprintf("no %s governor hook registered (see hooks above)", w.event), "")
		return
	}
	want := strings.Join(w.covers, "|")

	fix := fmt.Sprintf("set \"matcher\": %q on that hook, or re-run: governor install", want)

	if ref.matcher == "" {
		// An absent matcher is not a missing hook — the harness runs an unmatched hook for
		// EVERY tool call. The gate would still fire on a spawn, so this is not silent in
		// the dangerous direction; it is this binary executed on every Bash, Read and Edit
		// in every session on the machine. design.md section 13.3 records the same hazard
		// for the Phase 0 PostToolUse hook, where the matcher is called load-bearing rather
		// than tidy.
		d.add(name, fail,
			fmt.Sprintf("the %s governor hook has no matcher: it runs governor on every tool call in every session", w.event),
			fix)
		return
	}

	re, err := regexp.Compile(ref.matcher)
	if err != nil {
		// An uncompilable matcher is the silent case in full: nothing to match against, so
		// nothing ever fires.
		d.add(name, fail,
			fmt.Sprintf("the %s matcher %q is not a valid expression (%v), so that hook never fires",
				w.event, ref.matcher, err),
			fix)
		return
	}

	var missed []string
	for _, tool := range w.covers {
		if !re.MatchString(tool) {
			missed = append(missed, tool)
		}
	}
	if len(missed) > 0 {
		// Note what is NOT accepted here: a matcher of exactly "Task" happens to fire on
		// 2.1.263, because the harness carries Task as an alias for the tool whose payload
		// says Agent (docs/hook-surface.md section 1). design.md section 10.1 is explicit
		// that registering only Task "would have worked by luck", and a diagnostic that
		// passes something for working by luck is a diagnostic that will pass it on the
		// version where the luck runs out.
		d.add(name, fail,
			fmt.Sprintf("the %s matcher %q does not match %s — that path is never governed",
				w.event, ref.matcher, strings.Join(missed, ", ")),
			fix)
		return
	}
	d.add(name, pass,
		fmt.Sprintf("%q matches %s", ref.matcher, strings.Join(w.covers, ", ")), "")
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
//
// THE VERSION NUMBERS ARE NOT ENOUGH, AND GOV-B13 IS WHAT THAT COST. Every development build
// calls itself `0.0.0-dev`, so two different builds compare equal on all three numbers and
// this check reported `ok` for twelve days while the hooks ran a binary four merged PRs
// behind main. section 13.4 had predicted it in as many words. The bytes are therefore
// compared as well as the numbers, by the same digest `governor install` uses to decide
// whether it has anything to copy — which makes the two agree by construction: if install
// would replace the binary, doctor says the install is stale, and if it would not, doctor
// says it is current. Nothing here writes; it is the same predicate read from the other end.
//
// ITS BLIND SPOT IS INHERENT AND IS NAMED IN THE REPORT. Any check of this shape compares the
// binary running the check against the binary the hooks run, so when doctor IS the installed
// binary the two are one file and there is nothing to compare — which is the ordinary way
// doctor is invoked. That case says so in its own detail line rather than claiming more than
// it checked. See section 13.4.
func (d *diagnosis) checkBinary() {
	// Only the binaries behind the six hooks of section 4.1. A governor command wired to
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
	// One file, so there is no second build to compare against. Saying only "the hooks run
	// this binary" would be true and would also be the sentence that hid GOV-B13, so the
	// detail names what this invocation could not look at. It stays a pass: nothing is known
	// to be wrong, and spending `unverified` here would put exit 3 on every ordinary run of
	// doctor — the "red line in front of every user" cost that state's own comment exists to
	// avoid. section 13.4 carries the same limit for a reader who looks there instead.
	if sameFile(bin, self) {
		d.add("binary", pass,
			fmt.Sprintf("the hooks run this binary (%s %s) — staleness unchecked, "+
				"run doctor from a fresh build to compare", short(bin), binaryVersion), "")
		return
	}

	// Identical bytes settle it before any subprocess does. Two files holding the same bytes
	// are the same build whatever they call themselves, so there is nothing to ask and the
	// `version` exec is skipped — the cheap, certain answer ahead of the expensive, parsed one.
	same, err := sameContent(bin, self)
	if err != nil {
		// Could not read one of them, so the comparison did not happen. GOV-30's state is the
		// honest report: not "I looked and it is fine", not "I looked and it is stale" — "I
		// could not look". Passing here is how a check that cannot verify starts lying.
		d.add("binary", unverified,
			fmt.Sprintf("cannot compare %s with this build (%v)", short(bin), err),
			"governor install")
		return
	}
	if same {
		d.add("binary", pass,
			fmt.Sprintf("the hooks run %s, byte-identical to this build (%s)",
				short(bin), binaryVersion), "")
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
	// Every number matches and the bytes do not: a different build wearing the same version.
	// This is the GOV-B13 case exactly, and it is the one the old check called `ok`. It is a
	// failure rather than a warning because the install is genuinely not what the repo says
	// it is, and the fix is the ordinary one every other finding names.
	d.add("binary", fail,
		fmt.Sprintf("the hooks run %s, a DIFFERENT BUILD reporting the same version (%s) — "+
			"the install is stale", short(bin), v.binary),
		"governor install")
}

// checkDaemonBuild compares the build of the RUNNING DAEMON with the binary the hooks run.
//
// Neither neighbouring check covers it, and GOV-43 is what that cost. checkProtocol fires
// only when the wire version differs, and checkBinary compares the hooked binary with the
// binary running doctor — files on disk, both of them. Nothing compared the process holding
// the socket with either, so on 2026-10-31 a daemon started on 2026-09-20 from a 0.0.0-dev
// build served every session for 40 days with v0.1.1 installed, every check here green, and
// every fix shipped in between not running anywhere.
//
// The daemon's build is the digest of its own executable taken at startup (build.go); the
// hooks' build is the digest of the file they name. Same predicate `governor install` uses,
// so the fix line is the one that clears it: install replaces a daemon whose build differs.
func (d *diagnosis) checkDaemonBuild() {
	if !d.up {
		d.add("build", skip, "no daemon to compare builds with (see daemon above)", "")
		return
	}
	if d.reply.V != protocolVersion || d.reply.Reason != "" {
		d.add("build", skip, "the daemon speaks another protocol (see protocol above)", "")
		return
	}
	bins := map[string]bool{}
	for _, w := range wantedHooks {
		if ref := d.findHook(w); ref != nil && ref.bin != "" {
			bins[ref.bin] = true
		}
	}
	if len(bins) != 1 {
		d.add("build", skip, "no single hooked binary to compare the daemon with (see binary above)", "")
		return
	}
	bin := sortedKeys(bins)[0]
	running := fmt.Sprintf("the running daemon (%s", orUnknown(d.reply.BinaryVersion))
	if d.reply.DaemonStarted != "" {
		running += ", up since " + d.reply.DaemonStarted
	}
	running += ")"

	switch d.reply.Build {
	case "":
		// The pid 23289 shape exactly: same protocol, a version string that proves nothing,
		// and no build field because the build predates it — which settles that it is older
		// than the binary the hooks run, since that binary is new enough to ask.
		d.add("build", fail,
			fmt.Sprintf("%s predates build reporting, so it is an older build than the hooks run (%s) — "+
				"fixes since it started are not running", running, short(bin)),
			"governor install")
		return
	case buildUnknown:
		d.add("build", unverified,
			fmt.Sprintf("%s could not read its own executable, so its build is unknown", running),
			"governor install")
		return
	}
	want, err := digest(bin)
	if err != nil {
		d.add("build", unverified,
			fmt.Sprintf("cannot read %s to compare with the daemon's build (%v)", short(bin), err),
			"governor install")
		return
	}
	if d.reply.Build != want {
		d.add("build", fail,
			fmt.Sprintf("%s is build %s, but the hooks run build %s (%s) — the daemon is stale",
				running, shortBuild(d.reply.Build), shortBuild(want), short(bin)),
			"governor install")
		return
	}
	d.add("build", pass,
		fmt.Sprintf("the daemon is running the hooked binary (build %s)", shortBuild(want)), "")
}

func orUnknown(s string) string {
	if s == "" {
		return "version unknown"
	}
	return s
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

// ---------------------------------------------------------------------------
// budget
// ---------------------------------------------------------------------------

// checkBudget reports a budget that is below what this host could allow, and names what is
// holding it down (GOV-34 clause 4).
//
// THE QUESTION IT ANSWERS IS "WHY IS THIS MACHINE ONLY ALLOWING THREE", and before this story
// nothing could answer it. The budget on the wire has always been the EFFECTIVE one — acquire
// passes one `limit` to the admission test and to the refusal, so those two cannot disagree —
// but a reader seeing "3" had no way to tell a machine calibrated for three from a six-slot
// machine cut to three by the run queue. On 2026-09-21 a session spent fifteen minutes unable
// to run a build and had `governor status`, the refusal text and doctor all available, and not
// one of the three could tell it why.
//
// IT IS A WARNING, NOT A FAILURE, and the distinction is the same one checkStrayDaemons drew.
// A cut budget is the governor WORKING: the run queue was high, the loop halved the budget,
// and it will climb back. Nothing is broken and no install is defective, so failing would be
// crying wolf at the control loop doing its job — and a red line that means "working as
// designed" teaches a reader that red lines mean nothing. What it is instead is the one fact
// that makes an otherwise inexplicable refusal explicable, which is worth a line.
//
// A PIN IS REPORTED SEPARATELY FROM A CUT because they are not the same event and do not have
// the same fix. A cut is the machine's own answer and resolves itself; a pin is a person
// overruling it (design.md section 9.1) and resolves when that person says so, or when it
// expires. Reading a pin as "something is cutting the budget" would send someone hunting a
// load spike that is not there.
func (d *diagnosis) checkBudget() {
	if !d.up {
		d.add("budget", skip, "no daemon to read the budget from (see daemon above)", "")
		return
	}
	r := d.reply

	switch {
	case r.Pinned:
		detail := fmt.Sprintf("%d slots, pinned by hand", r.Budget)
		if r.SourceBudget > 0 {
			detail = fmt.Sprintf("%d slots, pinned by hand over the %d the control loop chose",
				r.Budget, r.SourceBudget)
		}
		if r.PinnedUntil != "" {
			detail += " until " + r.PinnedUntil
		}
		d.add("budget", warn, detail,
			"let it expire, or lift it now: build-governor:governor resume")

	case r.Cut != "" && r.Ceiling > r.Budget:
		// The case the clause is about. Name the number, name the signal — the same
		// discipline design.md section 4.2 puts on a refusal, for the same reason.
		d.add("budget", warn,
			fmt.Sprintf("%d of a possible %d slots: the budget is cut by %s",
				r.Budget, r.Ceiling, r.Cut),
			"nothing to fix — it climbs back as the machine quietens; to override it now: "+
				"build-governor:governor limit N")

	case r.Ceiling > r.Budget:
		// Below the ceiling with nothing claiming responsibility. The controller sets its cut
		// and its budget under one lock, so this should be unreachable; reporting it as a
		// definite state rather than passing over it is what would surface the day it is not.
		d.add("budget", warn,
			fmt.Sprintf("%d of a possible %d slots, and the daemon reports no signal cutting it",
				r.Budget, r.Ceiling),
			"restart the daemon so the control loop re-reads the host: kill $(cat "+
				short(d.pidPath())+") && governor install")

	case r.Ceiling > 0:
		d.add("budget", pass,
			fmt.Sprintf("%d slots, the most this host allows, with nothing cutting it", r.Budget), "")

	default:
		// A daemon with no control loop — which is every daemon a test builds, and a daemon
		// that has not yet taken its first sample. Its budget is fixed, so the question this
		// check asks has a definite answer and it is "no". That is a pass, not a shrug.
		d.add("budget", pass, fmt.Sprintf("%d slots, fixed", r.Budget), "")
	}
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

// ---------------------------------------------------------------------------
// calibration
// ---------------------------------------------------------------------------

// checkCalibration reports whether this host's limits were measured on this host.
//
// IT IS SEPARATE FROM checkConfig BECAUSE THE TWO HAVE DIFFERENT FIXES, which is the same
// reason design.md section 13.2 records for splitting "versions agree" into three findings: a
// line carrying two fixes is a line nobody acts on. checkConfig asks whether the file can be
// read at all and its fix is to move it aside; this asks whether anybody ever measured this
// machine and its fix is to run one command.
//
// NEVER CALIBRATED IS A WARN, NOT A FAIL. Every calibrated key ships with a default that
// leaves it inert (section 13.4), so an uncalibrated governor works — it just works to
// constants measured on somebody else's laptop, which is precisely the thing the project rule
// objects to. That is real, worth printing, and not a defect in this installation, which is
// what the warn state is for. It does not move the exit code.
//
// A CALIBRATION FROM A DIFFERENT MACHINE IS THE CHECK WORTH HAVING. `${CLAUDE_PLUGIN_DATA}`
// lives in a home directory, and home directories get synced, imaged and copied into
// containers. A config carrying an 8-core laptop's ceiling on a 2-core container is not
// corrupt and not from the future, so nothing else here would say a word about it — and the
// number is wrong in the direction that over-admits.
func (d *diagnosis) checkCalibration() {
	path, err := config.Path()
	if err != nil {
		// checkConfig has already failed on this and named the fix. Saying it twice would
		// print two problems for one cause, which is what the skip state is for.
		d.add("calibration", skip, "cannot locate the config", "")
		return
	}
	store := &config.FileStore{Path: path}
	c, err := store.Load(context.Background())
	if err != nil {
		d.add("calibration", skip, "the config could not be read; see the config check above", "")
		return
	}
	if c.CalibratedAt == "" {
		d.add("calibration", warn,
			"this host has never been measured, so the budget runs on constants measured on another machine",
			"governor calibrate")
		return
	}

	detail := fmt.Sprintf("measured %s: run-queue cut %.2f per core, b_max %d slots",
		c.CalibratedAt, c.RunQueueMax, c.BMax)

	if cores, ok := coresInProvenance(c.CalibratedFrom); ok {
		if now := runtime.NumCPU(); now != cores {
			d.add("calibration", warn,
				fmt.Sprintf("calibrated on a %d-core machine and this one has %d — the numbers are somebody else's", cores, now),
				"governor calibrate")
			return
		}
	}
	d.add("calibration", pass, detail, "")
}

// coresInProvenance digs the core count back out of the provenance note.
//
// READING IT BACK OUT OF PROSE IS NOT THE NICE VERSION, and the nice version is a structured
// field. It is done this way because the note is written to be read by a PERSON opening
// config.json — that is the whole reason it exists — and adding a parallel machine-readable
// copy of one number inside it would be two representations of the same fact, which is how
// they come to disagree. The failure mode is benign and one-directional: a note this cannot
// parse produces no finding at all, never a wrong one.
func coresInProvenance(note string) (int, bool) {
	_, rest, found := strings.Cut(note, "; ")
	if !found {
		return 0, false
	}
	count, _, found := strings.Cut(rest, " cores")
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
