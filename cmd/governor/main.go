// Command governor is the single static binary described in docs/design.md section 9.
// Every subcommand of the plugin — the hot-path gate, the daemon, the install surface —
// ships as one file, because the distribution story is "download one thing".
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/peter3605/claude-governor/internal/config"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// Version strings reported by `governor version`. The protocol version is the "v" field
// every wire message carries (section 7); the config schema version is the "schema" key in
// config.json (section 13.4). All three move independently.
//
// configSchemaVersion is config.Schema rather than a second literal 1. `doctor` compares the
// file's schema against this constant while config.FileStore migrates against its own, and
// two constants that must agree but can be edited apart is how a binary comes to report a
// schema it does not write.
const (
	protocolVersion     = 1
	configSchemaVersion = config.Schema
)

// binaryVersion is a var rather than a const so that a release build can stamp the tag into
// it with `-ldflags -X main.binaryVersion=...`.
//
// It is not cosmetic. `governor doctor` compares the version the hooks run against the
// version running the check, and two different builds that both call themselves 0.0.0-dev
// compare equal — so an upgrade that left stale hooks behind would pass the one check that
// exists to catch it. Nothing writes to it after start-up.
var binaryVersion = "0.0.0-dev"

// command is one subcommand of the binary. run returns the process exit code.
type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

// commands is the full surface from design.md section 9. Everything not yet implemented is
// a stub that exits 0 — the story that implements it replaces the stub in place.
var commands = []command{
	{"daemon", "run the budget daemon", runDaemon},
	{"gate", "PreToolUse hook: acquire a lease for one subagent spawn", runGate},
	{"confirm", "SubagentStart hook: bind a granted lease to a live agent", runConfirm},
	{"release", "SubagentStop hook: return a lease to the pool", runRelease},
	{"finish", "PostToolUse hook: return a build lease when its command completes", runFinish},
	{"session", "SessionStart/SessionEnd hook: register or drop a session", runSession},
	{"status", "print the current budget and its holders", runStatus},
	{"limit", "pin the budget until it expires or is released", runLimit},
	{"pause", "stop gating without stopping the daemon", runPause},
	{"resume", "release a pinned budget or a pause", runResume},
	{"boost", "double this session's share weight", runBoost},
	{"mcp", "serve the advisory MCP tool over stdio", runMCP},
	{"statusline", "render the one-line status for a session", runStatusline},
	{"trace", "Phase 0 instrumentation: record hook events and 1 Hz host samples", runTrace},
	{"install", "install the plugin, hooks and daemon", runInstall},
	{"uninstall", "reverse install and restore the settings backup", runUninstall},
	{"doctor", "diagnose a broken install and name each fix", runDoctor},
	{"calibrate", "measure this host and write its defaults", runCalibrate},
	{"version", "print binary, protocol and config schema versions", runVersion},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "governor: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

// stub is the not-yet-implemented body of a subcommand. It exits 0 and writes nothing to
// stdout: the hooks read stdout, and for the gate an empty stdout with exit 0 is precisely
// the grant path (design.md section 4.2). A stub must therefore look like a grant, never
// like a refusal.
func stub(name string) func([]string, io.Writer, io.Writer) int {
	return func(_ []string, _ io.Writer, stderr io.Writer) int {
		fmt.Fprintf(stderr, "governor %s: not implemented yet\n", name)
		return 0
	}
}

// runStatus prints what the governor currently knows about this host.
//
// This is the client half of the status operation in design.md section 7, which GOV-06
// deliberately left to GOV-10 along with the statusline. It reads the same reply the
// statusline does and prints all of it rather than one line's worth: `governor status` is
// what a person runs when the status line has told them something is wrong, so it is the
// place to be complete rather than brief.
//
// Output is one "key value" line per fact, the format this command has always used, so the
// doctor (GOV-20) can parse it without taking a dependency on a JSON shape that is still
// growing. Nothing the daemon did not send is printed: a field it omitted is a field this
// host has no reading for, and a placeholder would be a claim rather than a measurement.
//
// The thermal line is read locally when the daemon is unreachable. It is the same line
// either way — this process can read ProcessInfo.thermalState as well as the daemon can —
// so an unreachable daemon costs the budget and the lease table, not every fact.
func runStatus(_ []string, stdout, stderr io.Writer) int {
	printStatus(stdout, query(stderr, transport.Dial{}, time.Now().Add(statusBudget)))
	return 0
}

// printStatus writes the report for one status reply.
//
// Split from runStatus so that what this command PRINTS can be tested without standing up a
// daemon — the acceptance criteria for GOV-B01 are a list of facts that have to appear here,
// and a test that could only check them through a socket would not have been written.
func printStatus(stdout io.Writer, c cachedStatus) {
	v := view(c, "")
	if !v.OK {
		fmt.Fprintf(stdout, "daemon not running\n")
		fmt.Fprintf(stdout, "thermal %s\n", signals.ReadThermal())
		return
	}
	fmt.Fprintf(stdout, "budget %d\n", v.Budget)
	fmt.Fprintf(stdout, "in use %d\n", v.InUse)
	fmt.Fprintf(stdout, "waiting %d\n", v.Waiting)
	// The manual overrides (section 9.1), printed only when one is in force. This is the
	// command a person runs after the status line has told them something is odd, so it is the
	// place where "the budget is 2 because somebody pinned it" has to be legible — a pinned
	// budget that reported only its number would send them to debug a control loop that is
	// not running.
	if v.Pinned {
		fmt.Fprintf(stdout, "pinned yes%s\n", untilSuffix(v.PinnedUntil))
	}
	// WHY THE BUDGET IS THE NUMBER IT IS, when it is below what this host could allow
	// (GOV-34). Printed here for the same reason the pin is: this is the command a person
	// runs when their session is the one being refused, and "budget 4" on a machine
	// calibrated for six reads exactly like a six-slot machine's "budget 6" unless something
	// says otherwise. On 2026-09-21 nothing did, and the refusal that sent them here could
	// not say either. Both now quote one controller (section 6.1a).
	if c.Reply.Cut != "" && c.Reply.Ceiling > v.Budget {
		fmt.Fprintf(stdout, "cut %s (ceiling %d)\n", c.Reply.Cut, c.Reply.Ceiling)
	}
	if v.Paused {
		fmt.Fprintf(stdout, "paused yes%s\n", untilSuffix(v.PausedUntil))
	}
	if v.CPU != nil {
		fmt.Fprintf(stdout, "cpu %.2f\n", *v.CPU)
	}
	fmt.Fprintf(stdout, "thermal %s\n", v.Thermal)
	// THE PER-SESSION SHARES, which GOV-B01's acceptance criteria name and which this command
	// did not print until it did. The machine-wide totals above answer "is the budget full";
	// only this answers "full of whose work", which is the question somebody runs `status`
	// to settle when their own session is the one being refused.
	//
	// One line per session, with the weight beside the counts because since GOV-18 the weight
	// is what decides who refills a contended budget — and since this story a person can move
	// it, so a session sitting at 2 is a fact the others need to be able to see.
	for _, s := range c.Reply.Sessions {
		fmt.Fprintf(stdout, "session %s weight %d tentative %d confirmed %d%s\n",
			s.Session, sessionWeight(s.Weight), s.Tentative, s.Confirmed, liveSuffix(s))
	}
}

// sessionWeight reads a weight the daemon may not have sent. Zero means the daemon recorded no
// weight for this session, and the table reads that as the default 1 — so this prints 1 too,
// rather than a 0 that would look like a session entitled to nothing.
func sessionWeight(w int) int {
	if w <= 0 {
		return 1
	}
	return w
}

// liveSuffix marks a session that is running but has never registered with this daemon.
//
// It is the one thing on these lines that is not a number, and it is here because a session in
// that state is holding no leases and looks identical to an idle governed one — which is the
// exact misreading GOV-29 was filed about, in the place a person goes to check after the status
// line has sent them.
func liveSuffix(s sessionStat) string {
	if s.Live && !s.Registered {
		return " ungoverned"
	}
	return ""
}

// statusBudget is the allowance for `governor status`.
//
// It is larger than the statusline's because the two are run by different things: the
// statusline is rendered on every assistant message and must not be felt, while this is
// typed by a person who has already decided to wait for an answer. It is still bounded,
// because a command that hangs against a wedged daemon is a command that gets reported as
// a hung terminal.
const statusBudget = time.Second

// untilSuffix renders " until <RFC3339>" for the key/value lines above, or nothing when the
// deadline is absent. The wire format rather than a friendly one: this output is parsed by
// doctor and by scripts, and a locale-dependent time is not something either can read.
func untilSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " until " + t.UTC().Format(time.RFC3339)
}

func runVersion(_ []string, stdout, _ io.Writer) int {
	fmt.Fprintf(stdout, "governor %s\n", binaryVersion)
	fmt.Fprintf(stdout, "protocol %d\n", protocolVersion)
	fmt.Fprintf(stdout, "config schema %d\n", configSchemaVersion)
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: governor <command> [args]")
	fmt.Fprintln(w)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-11s %s\n", c.name, c.summary)
	}
}
