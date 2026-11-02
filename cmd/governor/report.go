package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/peter3605/claude-governor/internal/trace"
	"github.com/peter3605/claude-governor/internal/transport"
)

// `governor doctor --report` (GOV-40).
//
// One block a stranger can paste into an issue, holding everything needed to diagnose a
// refusal without a follow-up question. The governor's job is to REFUSE things, so its most
// important bug report is "it refused a build I wanted" — which the user experiences as the
// tool working, and will not report unless the report costs them one command.
//
// THE BLOCK IS MEANT TO BE PASTED IN PUBLIC, AND THAT IS THE HARD PART. The daemon log and
// the gate's decision record exist to debug a classifier that reads real command text, on
// machines where argv can hold repository names, hostnames and credentials. So the report
// is built in two layers, and both are load-bearing:
//
//  1. STRUCTURE. Nothing is copied through wholesale. A build decision contributes its
//     timestamp, outcome, cost class, weight, wait, the classifier's shape names and the
//     governor's own reason text — never its working directory, never its session id in
//     full, and never any field this file does not name. The gate does not record command
//     text at all (buildlog.go), and an unknown field added to a record later is dropped
//     here by construction rather than published by default.
//  2. SCRUBBING. Every line of output, including doctor's own findings, passes through
//     reportScrubber before it is written. The state directory becomes `<state>`; the
//     handful of install paths design.md section 13.3 names are kept, home-relative; every
//     other path becomes `<path>`; uuids are cut to their first segment; and anything
//     shaped like a credential becomes `<redacted>`. This is the layer that catches what
//     structure cannot: free text in the daemon log, an error string carrying a path, a
//     foreign daemon's argv in the strays check.
//
// TestTheReportIsSafeToPasteInPublic plants secrets, a home path and a repository name in
// every source the report reads, and fails if any of them comes out. It also asserts that
// the safe fields DO come out, so that it cannot be satisfied by printing nothing.
//
// NO TELEMETRY. Nothing here sends anything anywhere. The user reads the block and chooses
// to paste it; that is the only way it leaves the machine.

// issuesURL is where a user takes the report. issueChooserURL lands on the template picker.
const (
	issuesURL       = "https://github.com/peter3605/claude-governor/issues"
	issueChooserURL = issuesURL + "/new/choose"
)

// How much of each source the report carries. Enough to see the shape of a problem, few
// enough that the block stays pasteable.
const (
	reportLogLines  = 30
	reportDecisions = 20
	// reportLineMax truncates a single line. A log line is a sentence; one longer than this is
	// almost certainly carrying something it should not, and truncation also bounds what any
	// one leak could carry.
	reportLineMax = 240
	// reportTailBytes is how far back from the end of a file the report reads. samples.jsonl
	// grows without bound (GOV-B26) and has been seen at hundreds of megabytes; the report
	// needs its last few records, not all of it.
	reportTailBytes = 512 << 10
)

// writeReport prints the report for a diagnosis that has already run, and returns doctor's
// exit code, so a script sees the same verdict either way.
func (d *diagnosis) writeReport(stdout io.Writer) int {
	sc := newReportScrubber()
	w := &scrubbedWriter{w: stdout, sc: sc}

	w.line("governor doctor --report")
	w.line("paste everything from this line to the end into the issue")
	w.line("")

	// Header: what is installed, on what, talking to what.
	w.kv("governor", fmt.Sprintf("%s (protocol %d, config schema %d)", binaryVersion, protocolVersion, configSchemaVersion))
	if d.up && d.reply.BinaryVersion != "" && d.reply.BinaryVersion != binaryVersion {
		w.kv("daemon", d.reply.BinaryVersion)
	}
	w.kv("install", installRoute())
	w.kv("host", fmt.Sprintf("%s/%s, %d cores", runtime.GOOS, runtime.GOARCH, runtime.NumCPU()))
	w.kv("thermal", thermalSource(d))
	w.kv("claude code", claudeCodeVersions(d))
	w.kv("budget", reportBudget(d))

	// Doctor, exactly as it prints without --report. Its output is a contract (GOV-30's third
	// outcome, the summary line tests assert), so it is reused rather than reworded.
	w.line("")
	w.line("--- doctor ---")
	var findings bytes.Buffer
	code := d.report(&findings)
	w.block(findings.String())

	w.line("")
	w.line(fmt.Sprintf("--- daemon log (last %d lines) ---", reportLogLines))
	w.block(daemonLogTail())

	w.line("")
	w.line(fmt.Sprintf("--- recent gate decisions (last %d builds) ---", reportDecisions))
	w.block(recentDecisions())
	// Said rather than left for the reader to infer from an absence: the gate records builds,
	// and a refused spawn appears only in the session that was refused.
	w.line("(subagent spawns are not recorded; for a refused spawn, quote the refusal text from the session)")

	w.line("")
	w.line("report it at " + issueChooserURL)
	return code
}

// scrubbedWriter writes lines through a scrubber. There is deliberately no way to write to
// the underlying writer around it.
type scrubbedWriter struct {
	w  io.Writer
	sc *reportScrubber
}

func (s *scrubbedWriter) line(l string) {
	fmt.Fprintln(s.w, s.sc.line(l))
}

func (s *scrubbedWriter) kv(k, v string) {
	s.line(fmt.Sprintf("%-12s %s", k, v))
}

func (s *scrubbedWriter) block(b string) {
	b = strings.TrimRight(b, "\n")
	if b == "" {
		s.line("(none)")
		return
	}
	for _, l := range strings.Split(b, "\n") {
		s.line(l)
	}
}

// ---------------------------------------------------------------------------
// header facts
// ---------------------------------------------------------------------------

// installRoute names how the governor was installed, as far as the machine can say.
//
// The hooked binary is always a copy in the plugin tree, whatever the route, so the route is
// read from what surrounds it: a `governor` on PATH that brew owns, install.sh's link, and a
// marketplace registration pointing at the tree. The marketplace route sits on top of one of
// the other two — it needs the binary first — so it is reported as an addition.
func installRoute() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "unknown"
	}
	base := "unknown (no governor on PATH)"
	if p, err := exec.LookPath(installedBinaryName); err == nil {
		real := p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			real = r
		}
		switch {
		case strings.Contains(real, "/Cellar/") || strings.Contains(real, "homebrew") || strings.Contains(real, "linuxbrew"):
			base = "homebrew"
		case samePath(p, filepath.Join(home, ".local", "bin", installedBinaryName)):
			base = "install.sh"
		default:
			base = "a governor on PATH that neither brew nor install.sh put there"
		}
	}
	tree := filepath.Join(home, ".claude", "skills", pluginName)
	if regs, err := pluginRegistrations(home); err == nil {
		for _, r := range regs {
			if r.linksTo(tree) {
				return base + " + marketplace"
			}
		}
	}
	return base
}

// thermalSource says where thermal state comes from on this platform, and what it reads now.
func thermalSource(d *diagnosis) string {
	src := "none on this platform (reports unknown, treated as nominal)"
	if runtime.GOOS == "darwin" {
		src = "ProcessInfo.thermalState"
	}
	if d.up && d.reply.Thermal != "" {
		return fmt.Sprintf("%s, now %s", src, d.reply.Thermal)
	}
	return src
}

// claudeCodeVersions lists the Claude Code versions of the live sessions the daemon knows.
func claudeCodeVersions(d *diagnosis) string {
	if !d.up {
		return "unknown (no daemon to ask)"
	}
	seen := map[string]bool{}
	for _, s := range d.reply.Sessions {
		if s.Live && s.Version != "" {
			seen[s.Version] = true
		}
	}
	if len(seen) == 0 {
		return "unknown (no live session reported a version)"
	}
	return strings.Join(sortedKeys(seen), ", ")
}

func reportBudget(d *diagnosis) string {
	if !d.up {
		return "unknown (no daemon to ask)"
	}
	r := d.reply
	s := fmt.Sprintf("%s/%d slots held, %d waiting", formatSlots(r.Slots), r.Budget, r.Waiting)
	if r.Ceiling > 0 {
		s += fmt.Sprintf(", ceiling %d", r.Ceiling)
	}
	if r.Cut != "" {
		s += ", cut by " + r.Cut
	}
	if r.CPU != nil {
		s += fmt.Sprintf(", cpu %.0f%%", *r.CPU*100)
	}
	if r.Paused {
		s += ", paused"
	}
	if r.Pinned {
		s += fmt.Sprintf(", pinned to %d", r.Budget)
	}
	return s
}

// ---------------------------------------------------------------------------
// sources
// ---------------------------------------------------------------------------

// tail returns the complete lines in the last reportTailBytes of a file.
func tail(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := fi.Size() - reportTailBytes
	partial := off > 0
	if !partial {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if partial && len(lines) > 0 {
		// The first line starts wherever the seek landed, mid-record.
		lines = lines[1:]
	}
	return lines, sc.Err()
}

func daemonLogTail() string {
	dir, err := transport.StateDir()
	if err != nil {
		return "(cannot resolve the state directory)"
	}
	lines, err := tail(filepath.Join(dir, daemonLogFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "(no daemon log)"
		}
		return fmt.Sprintf("(cannot read the daemon log: %v)", err)
	}
	if len(lines) > reportLogLines {
		lines = lines[len(lines)-reportLogLines:]
	}
	return strings.Join(lines, "\n")
}

// recentDecisions renders the last gated builds from the trace log, by allowlisted field.
//
// It decodes into its own struct rather than trace.Build on purpose: a field added to the
// record later (a working directory, a command, anything) is then absent here until somebody
// decides, in this file and next to TestTheReportIsSafeToPasteInPublic, that it is safe.
func recentDecisions() string {
	path, err := trace.LogPath()
	if err != nil {
		return "(cannot resolve the trace log)"
	}
	lines, err := tail(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "(no gate decisions recorded)"
		}
		return fmt.Sprintf("(cannot read the trace log: %v)", err)
	}
	type safeBuild struct {
		SessionID string   `json:"session_id"`
		Shapes    []string `json:"shapes"`
		Class     string   `json:"class"`
		Weight    float64  `json:"weight"`
		Outcome   string   `json:"outcome"`
		WaitedMS  float64  `json:"waited_ms"`
		Why       string   `json:"why"`
	}
	type safeRecord struct {
		TS    string     `json:"ts"`
		Kind  string     `json:"kind"`
		Build *safeBuild `json:"build"`
	}
	var out []string
	for _, l := range lines {
		var r safeRecord
		if json.Unmarshal([]byte(l), &r) != nil || r.Kind != trace.KindBuild || r.Build == nil {
			continue
		}
		b := r.Build
		ts := r.TS
		if len(ts) > 19 {
			ts = ts[:19] + "Z"
		}
		shapes := strings.Join(b.Shapes, ",")
		if shapes == "" {
			shapes = "-"
		}
		s := fmt.Sprintf("%s  %-11s %-6s w%s  %s  %s", ts, b.Outcome, b.Class, formatSlots(b.Weight),
			strings.TrimPrefix(sessionLabel(b.SessionID), "session "), shapes)
		if b.WaitedMS > 0 {
			s += fmt.Sprintf("  waited %.0fms", b.WaitedMS)
		}
		if b.Why != "" {
			s += "  why: " + b.Why
		}
		out = append(out, s)
	}
	if len(out) > reportDecisions {
		out = out[len(out)-reportDecisions:]
	}
	if len(out) == 0 {
		return "(no gate decisions recorded)"
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// scrubbing
// ---------------------------------------------------------------------------

// reportScrubber rewrites one line so it is safe to publish.
type reportScrubber struct {
	// prefixes are rewritten in order, longest first: the state directory to <state>, and the
	// home directory to ~ (after which pathRE decides what survives).
	prefixes []struct{ from, to string }
	// tildeState is the state directory spelled from ~, when it is under home.
	tildeState string
}

func newReportScrubber() *reportScrubber {
	sc := &reportScrubber{}
	add := func(from, to string) {
		if from == "" || from == "/" {
			return
		}
		sc.prefixes = append(sc.prefixes, struct{ from, to string }{from, to})
		// macOS's /tmp is /private/tmp, and either spelling can reach a log line.
		if real, err := filepath.EvalSymlinks(from); err == nil && real != from {
			sc.prefixes = append(sc.prefixes, struct{ from, to string }{real, to})
		}
	}
	if dir, err := transport.StateDir(); err == nil {
		add(filepath.Clean(dir), "<state>")
		if t := short(filepath.Clean(dir)); strings.HasPrefix(t, "~/") {
			sc.tildeState = t
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Clean(home), "~")
	}
	sort.SliceStable(sc.prefixes, func(i, j int) bool {
		return len(sc.prefixes[i].from) > len(sc.prefixes[j].from)
	})
	return sc
}

// keptPaths are the home-relative paths that may appear in a report: the files design.md
// section 13.3 says install writes. They identify nothing about the user and are exactly what
// a maintainer needs to read a hooks or binary finding. Anything else is a path into the
// user's own files, and becomes <path>.
var keptPaths = []string{
	"~/.claude/skills/" + pluginName,
	"~/.claude/skills/" + legacyPluginName,
	"~/.claude/settings.json",
	"~/.claude/settings.local.json",
	"~/.claude/plugins/installed_plugins.json",
	"~/.claude/plugins/data/" + pluginName + "-",
	"~/.local/bin/" + installedBinaryName,
}

var (
	// uuidRE matches a session or agent id. Cut to the first segment, as sessionLabel does.
	uuidRE = regexp.MustCompile(`\b([0-9a-fA-F]{8})-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	// pathRE matches a path token: one starting with / or ~/ at a token boundary (a backtick
	// counts: doctor quotes its fix commands in them). A path ends at a colon, because doctor
	// and the daemon both write "<path>: <what is wrong with it>". "3/6" and
	// "darwin/arm64" do not start at a boundary, and in a URL the second slash of "//" follows
	// a slash, so https://... survives.
	pathRE = regexp.MustCompile("(^|[\\s\"'`=(\\[,:])(~?/[^\\s\"'`<>()\\[\\],;:/][^\\s\"'`<>()\\[\\],;:]*)")

	// secretREs are credential shapes. Each match becomes <redacted>.
	secretREs = []*regexp.Regexp{
		// key=value and key: value where the key names a secret.
		regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|auth|credential|private[_-]?key)[a-z0-9_.-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`),
		// Authorization headers.
		regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`),
		// Credentials in a URL: scheme://user:pass@host.
		regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`),
		// Vendor token prefixes.
		regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_|glpat-|xox[abprs]-|sk-|sk_live_|rk_live_|AKIA|ASIA|AIza|npm_|hf_)[A-Za-z0-9_-]{8,}`),
		// PEM blocks.
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		// Long opaque runs: anything of 32+ characters from a token alphabet that mixes letters
		// and digits. Run after uuidRE, so an id has already been shortened.
		regexp.MustCompile(`\b[A-Za-z0-9+/_=]{32,}\b`),
	}
)

func (sc *reportScrubber) line(l string) string {
	for _, p := range sc.prefixes {
		l = strings.ReplaceAll(l, p.from, p.to)
	}
	// Doctor abbreviates home to ~ itself (short), so the state directory can arrive in that
	// spelling too, and only after the pass above has turned home into ~ in everything else.
	if sc.tildeState != "" {
		l = strings.ReplaceAll(l, sc.tildeState, "<state>")
	}
	l = uuidRE.ReplaceAllString(l, "$1")

	// Paths BEFORE secrets. A credential rule that fires inside a path leaves a fragment the
	// path rule no longer recognises as a path — `/<redacted>/Repos/x` — and the rest of the
	// path, the part naming the user's files, survives. Found on the first real run, where a
	// stray daemon's argv under a test temp dir came out exactly like that.
	l = pathRE.ReplaceAllStringFunc(l, func(m string) string {
		sub := pathRE.FindStringSubmatch(m)
		lead, p := sub[1], sub[2]
		if keptPath(p) {
			return m
		}
		return lead + "<path>"
	})

	for i, re := range secretREs {
		switch i {
		case 0:
			l = re.ReplaceAllString(l, "$1$2<redacted>")
		case len(secretREs) - 1:
			l = re.ReplaceAllStringFunc(l, func(m string) string {
				if strings.ContainsAny(m, "0123456789") && strings.IndexFunc(m, isLetter) >= 0 {
					return "<redacted>"
				}
				return m
			})
		default:
			l = re.ReplaceAllString(l, "<redacted>")
		}
	}

	if len(l) > reportLineMax {
		l = strings.ToValidUTF8(l[:reportLineMax], "") + "…"
	}
	return l
}

func keptPath(p string) bool {
	for _, k := range keptPaths {
		if p == k || strings.HasPrefix(p, k) && (strings.HasSuffix(k, "-") || p[len(k)] == '/') {
			return true
		}
	}
	return false
}

func isLetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }
