// Command governor is the single static binary described in docs/design.md section 9.
// Every subcommand of the plugin — the hot-path gate, the daemon, the install surface —
// ships as one file, because the distribution story is "download one thing".
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/peter3605/claude-governor/internal/signals"
)

// Version strings reported by `governor version`. The protocol version is the "v" field
// every wire message carries (section 7); the config schema version is the "schema" key in
// config.json (section 13.4). All three move independently.
const (
	binaryVersion       = "0.0.0-dev"
	protocolVersion     = 1
	configSchemaVersion = 1
)

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
	{"session", "SessionStart/SessionEnd hook: register or drop a session", runSession},
	{"status", "print the current budget and its holders", runStatus},
	{"mcp", "serve the advisory MCP tool over stdio", stub("mcp")},
	{"statusline", "render the one-line status for a session", stub("statusline")},
	{"trace", "Phase 0 instrumentation: record hook events and 1 Hz host samples", runTrace},
	{"install", "install the plugin, hooks and daemon", stub("install")},
	{"uninstall", "reverse install and restore the settings backup", stub("uninstall")},
	{"doctor", "diagnose a broken install and name each fix", stub("doctor")},
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
// Today that is the thermal state and nothing else: the daemon (GOV-06) and its sampler
// (GOV-12) do not exist yet, so there is no budget, no lease table and no CPU reading to
// report, and inventing placeholder numbers for them would be worse than omitting them.
// When the daemon lands this becomes the client half of the status operation in design.md
// section 7 and the rest of the fields arrive over the wire — the thermal line is the same
// line either way, which is what makes it safe to read now.
//
// Output is one "key value" line per fact so the statusline (GOV-10) and doctor (GOV-20)
// can parse it without a JSON dependency on a shape that is still growing.
func runStatus(_ []string, stdout, _ io.Writer) int {
	fmt.Fprintf(stdout, "thermal %s\n", signals.ReadThermal())
	return 0
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
