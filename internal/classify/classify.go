// Package classify decides whether a Bash command is build-shaped, and how big it is.
//
// It exists because pointing the gate at `Bash` is a matcher change and deciding WHICH `Bash`
// is not. There are 20,105 Bash calls in this machine's transcripts against 86 gate-matched
// calls — about 220x — so leasing per call is obviously wrong, and a wrong predicate fails in
// both directions: gate everything and the terminal is unusable, gate nothing and the
// governor is decorative.
//
// The rules here were DERIVED from measured cost over that corpus, not guessed from binary
// names that look plausible. docs/bash-corpus.md carries the method, the corpus, the error
// rates both ways, and — importantly — the ceiling. The short version, because it constrains
// every caller:
//
//	catch rate      53.0% of expensive commands (35.2% held out)
//	false positives 35.7% of cheap commands   (28.8% held out)
//	gated share     18.4% of all Bash calls ever recorded
//	cost captured   78.6% of measured child core-seconds
//
// WHY THE CATCH RATE IS NOT HIGHER, AND WHY THAT IS A PROPERTY OF THE PROBLEM RATHER THAN OF
// THIS CODE: 48.7% of the commands measured to be expensive contain NO recognisable build
// tool at all. They are ad-hoc `python3 - <<'PY'` scripts, a `grep -a -o -E` over a 100 MB
// binary, an Xcode DerivedData sweep. No predicate over command text can catch them, and a
// predicate that tried would have to gate `grep`, which clause 3 of GOV-14 forbids outright.
//
// AND THE SAME TOOL IS NOT THE SAME COST IN TWO REPOS. Measured here: `uv run pytest` in one
// project is expensive 0.7% of the time over 148 calls, while `python -m pytest` in another is
// among the most expensive things on the machine. The binary name is a PRIOR, not an answer.
// GOV-15 should refine these classes from what it actually observes per host; this package is
// the seed that makes the first decision non-random, not the last word.
//
// Nothing here gates anything. GOV-14 ships classification and its evidence; pointing the gate
// at Bash is GOV-15, and the separation is deliberate — a wrong predicate that is already
// enforcing is a tool nobody can use.
package classify

import "strings"

// Class is how much CPU a gated command is expected to want.
//
// The boundaries come from measured peak child cores per command shape over the corpus, at the
// 90th percentile — see docs/bash-corpus.md. They are a floor rather than a calibration: the
// window under-represents the heavy modes (iOS UI suites, large pytest runs), so a shape's
// real p90 on another machine is more likely above these numbers than below.
type Class int

const (
	// Light is everything not gated: the common case, and by far the largest. Measured median
	// peak for commands with no toolchain binary is 0.02 cores.
	Light Class = iota
	// Medium is an ordinary build or test run. Measured p90 peak 0.87-1.18 cores.
	Medium
	// Heavy is a full-toolchain build. Measured p90 peak 3.5-4.6 cores — on an 8-core machine,
	// two of these at once is most of the machine.
	Heavy
)

func (c Class) String() string {
	switch c {
	case Medium:
		return "medium"
	case Heavy:
		return "heavy"
	default:
		return "light"
	}
}

// ExpectedPeakCores is the measured 90th-percentile peak child cores for the class.
//
// GOV-15's loop needs to know that `go test ./...` and `xcodebuild` are not the same size;
// this is the number that says so. It is deliberately the p90 and not the median: the loop is
// protecting against saturation, and sizing a build by its median would under-provision for
// exactly the invocations that cause the problem.
func (c Class) ExpectedPeakCores() float64 {
	switch c {
	case Medium:
		return 1.2
	case Heavy:
		return 4.5
	default:
		return 0.1
	}
}

// Result is the classification of one command.
type Result struct {
	// Gated reports whether the command should take a lease.
	Gated bool
	// Class is how big it is expected to be. Light whenever Gated is false.
	Class Class
	// Shapes names the gated invocations, as "bin" or "bin sub" — `go test`, `xcodebuild`,
	// `npm run build`. Empty whenever Gated is false, and in classifier order, so a command
	// chaining two gated invocations reports both.
	//
	// IT IS THE SHAPE AND NEVER THE COMMAND. A command line carries paths, flags, hostnames
	// and occasionally a secret; these two tokens carry none of them, and the instrument
	// that records them (internal/trace) is written to the user's disk and read by analysis
	// later. The same rule governs Agent.RawKeys there: names, never values.
	//
	// It exists because GOV-17 clause 3 has to count TEST-SUITE runs specifically, and the
	// Class enum cannot express that — `go build` and `go test` are both Medium, which is
	// correct for sizing a lease and useless for noticing that a session stopped running its
	// tests. Putting the distinction here rather than in the enum keeps it out of the sizing
	// decision, so the analysis can revise what counts as a test run without touching the
	// hot path or reopening the classifier's measured error rate.
	Shapes []string
}

// Classify decides whether a command is build-shaped.
//
// It performs no I/O and takes no locks, because it runs inside the gate BEFORE the daemon is
// contacted. That ordering is the point: the obvious implementation asks the daemon about
// every command and lets the daemon answer "not gated", which is a socket round trip on `cat`.
// At this call rate the classification has to be free, or the latency budget is spent before
// the decision is made.
func Classify(cmd string) Result {
	worst := Light
	gated := false
	var shapes []string
	for _, inv := range invocations(cmd) {
		b, sub := inv.bin, inv.sub
		if light[pair{b, sub}] {
			continue
		}
		c, ok := classOf(b, sub)
		if !ok {
			continue
		}
		gated = true
		if c > worst {
			worst = c
		}
		shapes = append(shapes, invocationShape(b, sub))
	}
	if !gated {
		return Result{Gated: false, Class: Light}
	}
	return Result{Gated: true, Class: worst, Shapes: shapes}
}

// recordableSub is the CLOSED vocabulary of subcommands that may be written to the trace log.
//
// It exists because `sub` is not a subcommand in general — it is the first word after the
// binary that is not a flag or an assignment, and for `xcodebuild -scheme Foo` that word is
// `Foo`, the name of somebody's product. Recording sub unfiltered would put project names,
// branch names and script names into a log on the user's disk. So the shape carries a
// subcommand only when it is one of these, and otherwise degrades to the bare binary, which
// is compiled in and cannot leak anything.
//
// The gated-subcommand half is derived from heavySub rather than retyped, so a pair added
// there is recordable without a second edit. The verbs below are the ones the toolchain
// entries take, and `test` is the one GOV-17 clause 3 turns on: `go build` and `go test` are
// the same Class and must not be the same shape.
var recordableSub = func() map[string]bool {
	m := map[string]bool{
		"test": true, "build": true, "bench": true, "check": true, "run": true,
	}
	for p := range heavySub {
		m[p.sub] = true
	}
	return m
}()

// invocationShape renders one invocation as the two tokens that identify it, and nothing else.
//
// Distinct from eval_test.go's shapeOf, which names a whole COMMAND by its gated binaries
// with the subcommand dropped and duplicates removed. That one groups the cost-class table in
// docs/bash-corpus.md, where `go test` and `go build` belong in one `go` row because the row
// is about what the binary costs. This one keeps the subcommand because the question it
// answers is the opposite: which of those two did the session actually run.
func invocationShape(bin, sub string) string {
	if sub == "" || !recordableSub[sub] {
		return bin
	}
	return bin + " " + sub
}

type pair struct{ bin, sub string }

// classOf returns the cost class of one invocation, and whether it is gated at all.
func classOf(bin, sub string) (Class, bool) {
	if c, ok := heavySub[pair{bin, sub}]; ok {
		return c, true
	}
	if c, ok := toolchain[bin]; ok {
		return c, true
	}
	return Light, false
}

// toolchain is the set of binaries whose job is to spawn compute, with the class measured for
// each where the corpus had enough calls to measure one.
//
// Shapes with a measured class, from docs/bash-corpus.md: xcodebuild p90 4.55 cores (n=9,
// expensive in 100% of calls); make p90 3.53 (n=8); go p90 1.18 (n=300); pytest p90 0.87
// (n=52). The rest are classed by ecosystem analogy to those four and are explicitly NOT
// measured — a full-toolchain native or JVM build is assumed to behave like `make` rather than
// like `go`, and the first host to measure otherwise should correct this table.
var toolchain = map[string]Class{
	// Go. Measured.
	"go": Medium,
	// Rust. Cargo drives rustc across a crate graph; assumed heavy by analogy with make.
	"cargo": Heavy,
	"rustc": Medium,
	// JS/TS.
	"npm": Medium, "npx": Medium, "pnpm": Medium, "yarn": Medium,
	"tsc": Medium, "jest": Medium, "vitest": Medium,
	"webpack": Heavy, "esbuild": Medium, "rollup": Medium, "vite": Medium,
	// Python. pytest measured.
	"pytest": Medium, "tox": Heavy, "nox": Heavy,
	"mypy": Medium, "pylint": Medium, "coverage": Medium,
	// Native toolchains. make measured.
	"make": Heavy, "cmake": Heavy, "ninja": Heavy, "bazel": Heavy, "buck": Heavy,
	"clang": Medium, "clang++": Medium, "gcc": Medium, "g++": Medium,
	"cc": Medium, "c++": Medium,
	// JVM.
	"gradle": Heavy, "mvn": Heavy, "javac": Medium, "kotlinc": Medium,
	// Apple. xcodebuild measured.
	"xcodebuild": Heavy, "xcodegen": Medium, "swiftc": Heavy, "xcrun": Medium,
	// .NET.
	"dotnet": Medium,
	// NOTE: docker, podman, terraform and xcrun are deliberately NOT here. They are
	// dispatchers whose common subcommands are control-plane, not compute — `docker ps`,
	// `terraform output`, `xcrun simctl list`. Blanket-gating them cost 1.5%, 0.7% and 1.5% of
	// ALL Bash calls on this corpus for almost no captured cost. They gate through heavySub
	// and dispatcher below, on the subcommands that actually build something.

	// A compose subcommand in neither heavySub nor light — `scale`, or one a later compose
	// adds. Medium rather than ungated, because a new verb that starts containers is more
	// likely than one that does nothing; rather than Heavy, because Heavy was GOV-B30.
	composeBin: Medium,
}

// heavySub covers binaries that are free in their common form and expensive in one particular
// subcommand. `git status` must never gate; `git clone` should.
var heavySub = map[pair]Class{
	{"git", "clone"}:       Medium,
	{"git", "gc"}:          Medium,
	{"git", "repack"}:      Medium,
	{"git", "fsck"}:        Medium,
	{"brew", "install"}:    Medium,
	{"brew", "upgrade"}:    Heavy,
	{"apt", "install"}:     Medium,
	{"swift", "build"}:     Heavy,
	{"swift", "test"}:      Heavy,
	{"pip", "install"}:     Medium,
	{"pip3", "install"}:    Medium,
	{"docker", "build"}:    Heavy,
	{"docker", "run"}:      Medium,
	{"docker", "pull"}:     Medium,
	{"docker", "push"}:     Medium,
	{"podman", "build"}:    Heavy,
	{"terraform", "plan"}:  Medium,
	{"terraform", "apply"}: Medium,
	{"terraform", "init"}:  Medium,
	{"uv", "sync"}:         Medium,
	{"uv", "lock"}:         Medium,
	{"poetry", "install"}:  Medium,

	// docker compose, by its OWN subcommand (GOV-B30). Until then the whole of `docker
	// compose` was one Heavy row, so `docker compose exec db psql -c 'select 1'` weighed 3.75
	// slots on a four-slot machine; compose was 39 of 78 Heavy gated calls over 11-03..11-09.
	//
	// MEASURED, on this host (Docker Desktop, 8 cores, 2026-11-14), by sampling the CPU of
	// Docker Desktop's VM and com.docker.* processes around each command, idle baseline
	// subtracted, three runs each. NOT by tools/childcpu: on macOS a container's CPU is spent
	// in the VM, never in the `docker` client's process tree, and the client tree read
	// 0.00-0.06 cores on EVERY command below, the 20-second build included. A child-tree
	// measurement would call all of compose free. Linux has no VM, so there the client tree
	// is still not the container (dockerd is not its child) but the host sampler sees it.
	//
	//	build --no-cache (go build -a std)  19-21 s  mean 3.1 cores  peak 6.4-6.7   Heavy
	//	build (cached)                       1.6 s  mean 1.4        peak 2.3
	//	up -d --wait (creates a container)   3.1-3.5 s  mean 0.6-1.0  peak 3.1-3.2   Medium
	//	run --rm <svc> psql --version        0.9-1.4 s  mean 2.2-2.6  peak 3.5-4.8   Medium
	//	start / stop                         0.6 s  mean 0.9-1.7     peak 1.1-2.1   ungated
	//	down                                 0.9-1.0 s  mean 1.3-1.6  peak 2.2-3.1   ungated
	//	exec <svc> psql -c '...'             0.6 s  mean 0.1-0.2     peak 0.2-0.7   ungated
	//	ps / logs / config                   0.6 s  mean <0.25       peak <0.75     ungated
	//
	// The line between gated and not is NOT the peak, which a 0.25 s sample inflates for a
	// sub-second burst; it is whether the command can do unbounded work. start/stop/down are
	// bounded lifecycle bursts of about one core-second that finish before a 1 Hz controller
	// could see them, so a lease buys nothing. up, run and create are gated because they can
	// pull a missing image and BUILD a missing `build:` service with no --build flag, and
	// `run` executes whatever its command is: the measured floor above is with the image
	// cached and a trivial command. --build on any of them adds a `build` invocation, Heavy.
	{"docker compose", "build"}:  Heavy,
	{"docker compose", "watch"}:  Heavy, // builds, then rebuilds on every change
	{"docker compose", "up"}:     Medium,
	{"docker compose", "run"}:    Medium,
	{"docker compose", "create"}: Medium,
	{"docker compose", "pull"}:   Medium, // as {"docker", "pull"}
	{"docker compose", "push"}:   Medium, // as {"docker", "push"}
}

// light is the exception list: subcommands of a gated binary that ask the tool about itself
// rather than building anything.
//
// Without it `go version` takes a lease, which is both useless and the kind of thing that
// makes a user uninstall a governor.
var light = map[pair]bool{
	{"go", "version"}: true, {"go", "env"}: true, {"go", "doc"}: true,
	{"go", "mod"}: true, {"go", "fmt"}: true, {"go", "help"}: true,
	{"npm", "ls"}: true, {"npm", "view"}: true, {"npm", "config"}: true,
	{"pip", "show"}: true, {"pip", "list"}: true, {"pip", "freeze"}: true,
	{"pip3", "show"}: true, {"pip3", "list"}: true, {"pip3", "freeze"}: true,
	{"docker", "ps"}: true, {"docker", "images"}: true, {"docker", "version"}: true,
	{"cargo", "version"}: true, {"xcrun", "--find"}: true,
	{"uv", "version"}: true, {"uv", "python"}: true,

	// docker compose's control plane, measured in the heavySub note above. `exec` is absent
	// on purpose: it is classified by the command it executes, like a runner, so `exec db
	// psql` is free and `exec app pytest` is pytest. `restart` is stop then start.
	{composeBin, ""}: true, {composeBin, "ps"}: true, {composeBin, "logs"}: true,
	{composeBin, "down"}: true, {composeBin, "stop"}: true, {composeBin, "start"}: true,
	{composeBin, "restart"}: true, {composeBin, "config"}: true, {composeBin, "ls"}: true,
	{composeBin, "top"}: true, {composeBin, "port"}: true, {composeBin, "images"}: true,
	{composeBin, "kill"}: true, {composeBin, "rm"}: true, {composeBin, "pause"}: true,
	{composeBin, "unpause"}: true, {composeBin, "version"}: true, {composeBin, "events"}: true,
	{composeBin, "cp"}: true, {composeBin, "wait"}: true, {composeBin, "attach"}: true,
	{composeBin, "stats"}: true,
}

// runner is a command that executes another command. The cost belongs to what it runs, not to
// it: `uv run pytest` is pytest, while `uv run python -c "print(1)"` is not a build.
//
// This distinction is not cosmetic, and the effect was measured rather than assumed. Gating the
// runners themselves instead of what they run costs 4.3 points of gated share (22.7% against
// 18.4% of all Bash calls) and 5.4 points of false-positive rate (41.1% against 35.7%), for
// EXACTLY the same catch rate — 53.0% either way. It is pure loss.
var runner = map[string]bool{
	"uv": true, "npx": true, "poetry": true, "pipenv": true,
	"rye": true, "pdm": true, "hatch": true, "bundle": true,
}

// pyModule is the set of modules that mean real work under `python -m`.
//
// This is how pytest usually appears in the corpus — `.venv/bin/python -m pytest` — and
// matching only the basename `python` misses every one of them. They were measured as misses
// before this rule existed.
var pyModule = map[string]bool{
	"pytest": true, "mypy": true, "build": true, "unittest": true,
	"tox": true, "nox": true, "ruff": true, "black": true,
	"compileall": true, "pip": true, "coverage": true, "pylint": true,
}

func isPython(b string) bool {
	return b == "python" || b == "python3" || b == "python2"
}

// runSub is the subcommand a runner uses to mean "now execute this". `uv run X` and
// `poetry run X` wrap X; `uv sync` does not wrap anything.
func runSub(b string) bool { return b == "run" || b == "exec" }

// dispatcher is a tool whose FIRST operand is the real command, with no `run` keyword in
// between. `xcrun swiftc x.swift` is swiftc; `xcrun simctl list devices` is simctl, which
// builds nothing.
//
// Same reasoning as runner, measured the same way: gating xcrun itself put 1.5% of every Bash
// call on this machine through the daemon to catch a handful of compiler invocations that the
// dispatched name catches anyway.
var dispatcher = map[string]bool{"xcrun": true}

type invocation struct{ bin, sub string }

// invocations returns the commands a shell string actually invokes, after unwrapping runners.
//
// It is a scan, not a shell grammar: quote state, heredoc removal, and a split on the operators
// that separate simple commands. That is enough to be right about the shapes that occur and
// cheap enough to run on every Bash call.
func invocations(cmd string) []invocation {
	var out []invocation
	forEachSegment(cmd, func(seg string) {
		out = appendCommand(out, fields(seg))
	})
	return out
}

// appendCommand appends the invocations of one simple command, given as its words.
//
// It is split out of invocations so that a command which executes ANOTHER command inside a
// container — `docker compose exec app pytest` — can classify the inner one by the same rules.
// The inner word list is strictly shorter than the outer, so the recursion terminates.
func appendCommand(out []invocation, ws []string) []invocation {
	i := 0
	// Skip env assignments and command prefixes: `LC_ALL=C time go test ...`.
	for i < len(ws) && (isAssign(ws[i]) || prefixWord[ws[i]]) {
		i++
	}
	if i >= len(ws) {
		return out
	}
	bin := base(ws[i])
	if bin == "" || keyword[bin] || isRedir(bin) || strings.HasPrefix(bin, "-") || strings.HasPrefix(bin, "$") {
		return out
	}
	rest := ws[i+1:]

	// `docker compose …` and the standalone `docker-compose …` are classified by compose's
	// own subcommand, which is past compose's global flags (GOV-B30). docker's global flags
	// are skipped first for the same reason: `docker --context prod compose ps`.
	if bin == "docker-compose" {
		return appendCompose(out, rest)
	}
	if bin == "docker" {
		if j := skipFlags(rest, dockerGlobalValued); j < len(rest) && rest[j] == "compose" {
			return appendCompose(out, rest[j+1:])
		}
	}

	sub := firstOperand(rest)

	// `python -m pytest` is pytest.
	if isPython(bin) && sub != "" && pyModule[sub] && hasFlag(rest, "-m") {
		return append(out, invocation{sub, ""})
	}
	// `xcrun swiftc x.swift` is swiftc.
	if dispatcher[bin] && sub != "" {
		return append(out, invocation{base(sub), firstOperandAfter(rest, sub)})
	}
	// `uv run pytest -q` is pytest. Only for the run/exec form: `uv sync` is uv's own work.
	if runner[bin] && runSub(sub) {
		if w := firstOperandAfter(rest, sub); w != "" {
			inner := base(w)
			if isPython(inner) {
				// `uv run python -m pytest` — look one level deeper.
				if m := firstOperandAfter(rest, w); m != "" && pyModule[base(m)] {
					return append(out, invocation{base(m), ""})
				}
			}
			return append(out, invocation{inner, firstOperandAfter(rest, w)})
		}
	}
	return append(out, invocation{bin, sub})
}

// composeBin is the pseudo-binary every compose invocation is recorded under, whichever of
// `docker compose` or `docker-compose` was typed. It is two tokens on purpose: the trace
// shape is "bin sub", so this renders as `docker compose up`, never as a bare `docker`.
const composeBin = "docker compose"

// appendCompose classifies the words after `docker compose` / `docker-compose`.
//
// `exec` and `run` carry a command to run inside a container, and the cost of that command
// is the cost that matters — the same rule as runner. `exec` adds nothing of its own (it was
// measured at 0.1-0.2 cores); `run` creates and removes a container, measured at about two
// core-seconds with a trivial command, so it is gated itself AND by what it runs.
func appendCompose(out []invocation, ws []string) []invocation {
	i := skipFlags(ws, composeGlobalValued)
	if i >= len(ws) {
		return append(out, invocation{composeBin, ""})
	}
	sub, rest := ws[i], ws[i+1:]
	if sub == "exec" || sub == "run" {
		svc := skipFlags(rest, composeCmdValued)
		if sub == "run" {
			out = append(out, invocation{composeBin, "run"})
			if hasFlag(rest[:svc], "--build") {
				out = append(out, invocation{composeBin, "build"})
			}
		}
		if svc+1 < len(rest) {
			out = appendCommand(out, rest[svc+1:])
		}
		return out
	}
	// `up --build` builds and then starts: two invocations, so the class is the build's and
	// the trace says both happened.
	if sub != "build" && hasFlag(rest, "--build") {
		out = append(out, invocation{composeBin, "build"})
	}
	return append(out, invocation{composeBin, sub})
}

// skipFlags returns the index of the first operand in ws, treating each flag named in valued
// as consuming the word after it. `--file=x.yml` is one word and needs no entry.
//
// This is the difference between reading `docker compose -f x.yml exec` as exec and reading
// it as `x.yml`; firstOperand alone cannot tell a flag's value from a subcommand.
func skipFlags(ws []string, valued map[string]bool) int {
	for i := 0; i < len(ws); i++ {
		w := ws[i]
		if w == "" || isRedir(w) {
			continue
		}
		if strings.HasPrefix(w, "-") {
			if valued[w] {
				i++
			}
			continue
		}
		return i
	}
	return len(ws)
}

// dockerGlobalValued are docker's global flags that take a separate value.
var dockerGlobalValued = map[string]bool{
	"-c": true, "--context": true, "-H": true, "--host": true, "--config": true,
	"-l": true, "--log-level": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true,
}

// composeGlobalValued are compose's global flags that take a separate value, from
// `docker compose --help`.
var composeGlobalValued = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true, "--profile": true,
	"--env-file": true, "--project-directory": true, "--ansi": true, "--progress": true,
	"--parallel": true,
}

// composeCmdValued are the value-taking flags of `exec` and `run` together, which stand
// between the subcommand and the service. One set serves both: no flag of one takes a value
// where the same flag of the other does not.
var composeCmdValued = map[string]bool{
	"-e": true, "--env": true, "--env-from-file": true, "-u": true, "--user": true,
	"-w": true, "--workdir": true, "--index": true, "--entrypoint": true, "-l": true,
	"--label": true, "--name": true, "-p": true, "--publish": true, "-v": true,
	"--volume": true, "--cap-add": true, "--cap-drop": true, "--pull": true,
}

// prefixWord introduces a command without being one.
var prefixWord = map[string]bool{
	"sudo": true, "env": true, "nohup": true, "time": true, "command": true,
	"builtin": true, "exec": true, "nice": true, "ionice": true, "stdbuf": true,
	"then": true, "do": true, "else": true, "elif": true, "!": true,
}

// keyword is shell syntax that is never a binary.
var keyword = map[string]bool{
	"if": true, "fi": true, "for": true, "while": true, "until": true, "done": true,
	"case": true, "esac": true, "in": true, "function": true, "select": true,
	"{": true, "}": true, "(": true, ")": true, "[[": true, "]]": true, "[": true, "]": true,
}

func isAssign(w string) bool {
	for i := 0; i < len(w); i++ {
		c := w[i]
		if c == '=' {
			return i > 0
		}
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return false
}

func isRedir(w string) bool {
	if w == "" {
		return false
	}
	i := 0
	for i < len(w) && w[i] >= '0' && w[i] <= '9' {
		i++
	}
	return i < len(w) && (w[i] == '>' || w[i] == '<')
}

func base(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func hasFlag(ws []string, f string) bool {
	for _, w := range ws {
		if w == f {
			return true
		}
	}
	return false
}

// firstOperand is the first word that is neither a flag nor a redirection: the subcommand.
func firstOperand(ws []string) string {
	for _, w := range ws {
		if w == "" || strings.HasPrefix(w, "-") || isRedir(w) {
			continue
		}
		return w
	}
	return ""
}

// firstOperandAfter is the first operand following a given one.
func firstOperandAfter(ws []string, after string) string {
	seen := false
	for _, w := range ws {
		if w == "" || strings.HasPrefix(w, "-") || isRedir(w) {
			continue
		}
		if seen {
			return w
		}
		if w == after {
			seen = true
		}
	}
	return ""
}
