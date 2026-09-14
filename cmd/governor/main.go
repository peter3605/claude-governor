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
	{"mcp", "serve the advisory MCP tool over stdio", stub("mcp")},
	{"statusline", "render the one-line status for a session", runStatusline},
	{"trace", "Phase 0 instrumentation: record hook events and 1 Hz host samples", runTrace},
	{"install", "install the plugin, hooks and daemon", runInstall},
	{"uninstall", "reverse install and restore the settings backup", runUninstall},
	{"doctor", "diagnose a broken install and name each fix", runDoctor},
	{"calibrate", "measure this host and write its defaults", stub("calibrate")},
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
	v := view(query(stderr, transport.Dial{}, time.Now().Add(statusBudget)), "")
	if !v.OK {
		fmt.Fprintf(stdout, "daemon not running\n")
		fmt.Fprintf(stdout, "thermal %s\n", signals.ReadThermal())
		return 0
	}
	fmt.Fprintf(stdout, "budget %d\n", v.Budget)
	fmt.Fprintf(stdout, "in use %d\n", v.InUse)
	fmt.Fprintf(stdout, "waiting %d\n", v.Waiting)
	if v.CPU != nil {
		fmt.Fprintf(stdout, "cpu %.2f\n", *v.CPU)
	}
	fmt.Fprintf(stdout, "thermal %s\n", v.Thermal)
	return 0
}

// statusBudget is the allowance for `governor status`.
//
// It is larger than the statusline's because the two are run by different things: the
// statusline is rendered on every assistant message and must not be felt, while this is
// typed by a person who has already decided to wait for an answer. It is still bounded,
// because a command that hangs against a wedged daemon is a command that gets reported as
// a hung terminal.
const statusBudget = time.Second

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
