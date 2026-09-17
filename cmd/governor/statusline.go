package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The statusline (GOV-10, design.md sections 7 and 9).
//
// This is the client half of the status operation, and it is Phase 1 rather than polish.
// A governor that silently changes behaviour is untrustworthy: `2/4 slots` moving in every
// session is what turns an invisible policy into something a person can watch, and being
// able to watch it is most of what makes it acceptable to run at all. It is also the
// fastest instrument available for debugging the control loop when GOV-14 lands, which is
// why it is built before the loop and not after.
//
// It is NOT a hook, and the difference from gate.go matters in one direction: a hook that
// fails open produces nothing and blocks nobody, whereas this command's failure mode is a
// line of text a person reads. So every failure here still ends in exit 0 — a non-zero
// status makes Claude Code discard the output entirely (section 3 of the surface notes
// below) — but it ends in a line that SAYS what went wrong rather than in silence.
//
// What it must not become is a second hot path. Claude Code runs it on every assistant
// message, in every session on the machine, so a naive implementation turns one daemon
// round trip per message per session into the busiest thing the governor does. The cache
// below is the answer, and it is a file rather than a variable because each invocation is
// a fresh process.

// statuslineBudget is the whole wall-clock allowance for one invocation, covering reading
// stdin, the cache, the socket and the render.
//
// A var, not a const, so tests can shrink it. Nothing else may write it.
var statuslineBudget = 50 * time.Millisecond

// statuslineCacheTTL is how long a status reply is reused before the daemon is asked again.
//
// One second is chosen against what the line reports rather than against the cost of the
// call: the budget moves on GOV-14's control interval and lease counts move on spawns, so a
// second-old answer is still an answer, and no person reads a status line faster than that.
// The saving is per message per session — on a machine with six sessions open this is the
// difference between six daemon round trips per assistant message and roughly one per
// second in total.
const statuslineCacheTTL = time.Second

// statuslineCacheFile is the cached status, in the state directory beside the socket. It is
// listed in design.md section 13.3, which is normative: a path written but not listed is a
// path uninstall silently stops covering.
const statuslineCacheFile = "statusline.json"

// statusLinePayload is the JSON Claude Code writes to this command's stdin, decoded for the
// two fields this command has any business with.
//
// Measured against 2.1.265 in docs/hook-surface.md section 14. Decoded loosely, like the
// hook payloads: a harness upgrade that renames a field must degrade to a thinner line,
// never to a failed render. Everything else in that payload — the transcript path, the cost
// totals, the context window, the model — is deliberately not read. This process reports on
// the fleet budget and has no business with the contents of anyone's session.
type statusLinePayload struct {
	// SessionID is what separates "the machine is holding 3 slots" from "you are holding
	// 2 of them", which is the number a person in this session actually wants.
	SessionID string `json:"session_id"`
}

// statusRequest is the status message of design.md section 7.
//
// Section 7 shows this op without a principal, but the prose above it says every request
// carries one, and GOV-06 made that a Done-when clause. It is sent for the same reason the
// gate sends it: nothing reads it in v1, and retrofitting identity into a shipped wire
// format costs far more than carrying a field that is currently ignored.
type statusRequest struct {
	V         int       `json:"v"`
	Op        string    `json:"op"`
	Principal principal `json:"principal"`
}

// statusResponse is the status reply of design.md section 7.
//
// It mirrors the daemon's statusReply rather than sharing the type, because these are two
// halves of a wire protocol that will at times be different binary versions (invariant 8),
// and a shared struct hides the moment they diverge.
type statusResponse struct {
	V       int     `json:"v"`
	Budget  int     `json:"budget"`
	InUse   int     `json:"in_use"`
	Slots   float64 `json:"slots"`
	Waiting int     `json:"waiting"`
	// CPU is a pointer because the daemon sends null when it has not measured, rather than
	// a fabricated 0.0. Null means "not measured"; rendering it as "cpu 0%" would tell the
	// reader the machine is idle, which is a claim and not a reading.
	//
	// GOV-12 gave the daemon a sampler, so this is now populated on any daemon that has run
	// a tick — but null is still the honest answer before the first one lands, and on a
	// daemon started without one.
	CPU     *float64        `json:"cpu"`
	Thermal signals.Thermal `json:"thermal"`
	Sources []sourceStat    `json:"sources"`
	// The governance fields (GOV-29). A daemon from before that story sends none of them,
	// which reads as SessionsKnown false — "this daemon cannot tell me" — and every consumer
	// then says nothing rather than guessing. That is the whole reason they are additive
	// rather than a protocol bump.
	DaemonStarted  string        `json:"daemon_started"`
	HooksInstalled string        `json:"hooks_installed"`
	SessionsKnown  bool          `json:"sessions_known"`
	Sessions       []sessionStat `json:"sessions"`
	// SpawnSurface is GOV-30's ledger of spawn tool names the matcher has actually been seen
	// to catch, per Claude Code version. Only doctor reads it; the status line does not
	// render it. Absent from an older daemon, which reads as "nothing confirmed".
	SpawnSurface      []spawnSighting `json:"spawn_surface,omitempty"`
	SpawnSurfaceKnown bool            `json:"spawn_surface_known"`
}

// cachedStatus is one status reply plus when it was taken.
//
// The whole reply is cached rather than the rendered line, and that is load-bearing: the
// cache is shared by every session on the machine, and the per-session part of the line is
// derived from Sessions at render time. Caching the finished string would show one
// session's slot count in another session's status line for up to a second.
//
// Down is cached too. A machine with no daemon running would otherwise pay a failed dial on
// every assistant message in every session, and "not running" is exactly as true a second
// later as it was when it was measured.
type cachedStatus struct {
	At    time.Time      `json:"at"`
	Down  bool           `json:"down"`
	Reply statusResponse `json:"reply"`
}

func runStatusline(_ []string, stdout, stderr io.Writer) int {
	return statuslineCommand(os.Stdin, stdout, stderr, transport.Dial{}, statuslineBudget, terminalWidth())
}

// statuslineCommand renders the line, prints it, and only then writes the cache.
//
// That order is the whole of GOV-33 and it is the same ordering argument trace.Append makes
// on the gate's hot path: the disk is touched after the reader has what they came for, never
// before. Two things follow from it, and both are the point.
//
// A write that happens here CANNOT BE ABANDONED. statusline's budget is enforced by returning
// while its worker goroutine is still running, and this command exits as soon as it returns —
// so a cache write inside that goroutine is a write the process can exit in the middle of.
// Deferred functions in an abandoned goroutine do not run at exit, which is how writeCache's
// `defer os.Remove(tmp.Name())` came to leave 133 statusline.json.<random> files in one
// person's state directory over six days, and with them a directory uninstall could never
// empty. Nothing here outlives the call, so nothing here can be left half-done.
//
// A wedged disk CANNOT DELAY THE LINE, which is what moving the write past the render buys
// and why the budget not covering it is acceptable rather than merely tolerable. The budget
// exists for the failures a socket deadline cannot see — a stdin that never closes, a daemon
// that never answers — and both of those are behind us by the time the line is printed.
func statuslineCommand(stdin io.Reader, stdout, stderr io.Writer, dialer transport.Dialer, budget time.Duration, width int) int {
	line, pending := statusline(stdin, stderr, dialer, budget, width)
	fmt.Fprintln(stdout, line)
	if pending != nil {
		writeCache(stderr, *pending)
	}
	return 0
}

// statusline renders the whole line. It never returns an error and never returns an empty
// string: whatever happened, the reader gets a sentence about it.
//
// It returns the reply to cache rather than caching it, and touches no file at all. This is
// the function whose work can be abandoned mid-flight, so it is the function that must not
// be holding a temp file when that happens — see statuslineCommand. A nil second return is
// "nothing to cache": the cache was fresh, or the budget expired before an answer arrived.
func statusline(stdin io.Reader, stderr io.Writer, dialer transport.Dialer, budget time.Duration, width int) (string, *cachedStatus) {
	deadline := time.Now().Add(budget)

	// Diagnostics go to stderr, which Claude Code captures into its debug log and never
	// shows the user. It is the same bargain the hooks make: say what went wrong somewhere
	// a person can find it, and never on the surface being rendered.
	diag := &cutoffWriter{w: stderr}
	defer diag.off()

	type result struct {
		line string
		// pending is the reply the caller should cache, or nil if there is nothing to
		// cache. It is carried out rather than written here on purpose; see the doc
		// comment on statuslineCommand.
		pending *cachedStatus
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(diag, "governor statusline: recovered: %v\n", r)
				done <- result{line: render(statusView{}, width)}
			}
		}()
		var p statusLinePayload
		// A payload this command cannot read is not a reason to render nothing: every
		// field of the line except the session's own slot count is machine-wide.
		if !decodePayload(stdin, diag, &p) {
			fmt.Fprintf(diag, "governor statusline: continuing without a session id\n")
		}
		v, pending := fetch(diag, dialer, deadline, p.SessionID)
		done <- result{line: render(v, width), pending: pending}
	}()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case r := <-done:
		return r.line, r.pending
	case <-timer.C:
		// The backstop, for the failures no socket deadline can see — chiefly a stdin that
		// is never closed, which happens before there is a socket to put a deadline on.
		//
		// Nothing is cached on this path. The answer either never arrived or arrived too
		// late to be worth a second of every session's status line, and the goroutine that
		// might still be carrying one is about to be abandoned.
		fmt.Fprintf(diag, "governor statusline: budget of %v expired\n", budget)
		return render(statusView{}, width), nil
	}
}

// statusView is what the renderer needs, after the wire reply and the session id have been
// reconciled. The zero value is "the daemon did not answer", which is what every failure
// path in this file degrades to.
type statusView struct {
	// OK is whether the daemon answered at all. Everything below is meaningless without it.
	OK     bool
	Budget int
	// InUse is the number of leases held; Slots is how much of the budget they weigh. Since
	// GOV-15 those are different numbers — a subagent weighs about 0.08 slots and a Heavy
	// build 3.75 — and only Slots is comparable with Budget.
	InUse   int
	Slots   float64
	Waiting int
	CPU     *float64
	Thermal signals.Thermal
	// Mine is the slots held by the session this line is being rendered for, tentative and
	// confirmed together. A tentative lease is a spawn in flight and counting it is right:
	// it is a slot this session is currently consuming.
	Mine int
	// Ungoverned is set when THIS session's hooks are not loaded, so nothing it spawns is
	// being capped (GOV-29).
	//
	// It is the difference between a truthful number and a useful one. A session whose hooks
	// never loaded holds no leases, so it renders "0/4 slots" — accurate, and exactly what a
	// fully governed idle session shows. On 2026-09-09 that reading is what a person saw
	// while two subagents ran, and the reasonable conclusion drawn from it was that the
	// governor was working and the machine was quiet.
	Ungoverned bool
}

// fetch resolves the current status for the statusline, from the cache if it is fresh and
// from the daemon if it is not.
//
// It returns the reply to cache alongside the view, and does not cache it itself: a cache
// hit has nothing to write, and a fresh answer is written by the caller after the line has
// been rendered. Reading is safe here — a read holds no temp file and cannot leave residue
// if it is abandoned partway.
func fetch(diag io.Writer, dialer transport.Dialer, deadline time.Time, session string) (statusView, *cachedStatus) {
	if c, ok := readCache(time.Now()); ok {
		return view(c, session), nil
	}
	c := query(diag, dialer, deadline)
	return view(c, session), &c
}

// query asks the daemon, once, with no cache on either side of it.
//
// `governor status` uses this rather than fetch, and the distinction is not fussiness: that
// command is typed by a person who has just been told by the status line that something is
// wrong, and answering them from a cache written by the very line that sent them there is
// how a stale reading gets confirmed twice and believed. It does not write the cache
// either — a command run by hand should not change what every session on the machine
// displays for the next second.
func query(diag io.Writer, dialer transport.Dialer, deadline time.Time) cachedStatus {
	c := cachedStatus{At: time.Now()}
	var resp statusResponse
	if !exchange(diag, dialer, deadline, statusRequest{
		V: protocolVersion, Op: "status", Principal: localPrincipal(),
	}, &resp) {
		c.Down = true
		return c
	}
	if resp.V != protocolVersion {
		// A version this binary does not speak is answered, not ignored (invariant 8), and
		// the answer is treated exactly as the gate treats it: as no answer at all. The
		// numbers in a reply from a protocol this binary does not know are not numbers it
		// can put on a status line.
		fmt.Fprintf(diag, "governor: daemon speaks protocol v%d, this binary speaks v%d\n",
			resp.V, protocolVersion)
		c.Down = true
		return c
	}
	c.Reply = resp
	return c
}

// view reconciles a cached reply with the session the line is being rendered for.
func view(c cachedStatus, session string) statusView {
	if c.Down {
		return statusView{}
	}
	v := statusView{
		OK:      true,
		Budget:  c.Reply.Budget,
		InUse:   c.Reply.InUse,
		Slots:   c.Reply.Slots,
		Waiting: c.Reply.Waiting,
		CPU:     c.Reply.CPU,
		Thermal: c.Reply.Thermal,
	}
	if session == "" {
		return v
	}
	for _, s := range c.Reply.Sessions {
		if s.Session != session {
			continue
		}
		v.Mine = s.Tentative + s.Confirmed
		// Only this session's own verdict is rendered. The line has room for one machine and
		// one session, and a person reading it is asking about the window in front of them.
		// Whether some other session is ungoverned is doctor's question to answer.
		v.Ungoverned = classifyGovernance(c.Reply, s).ungoverned()
		break
	}
	return v
}

// render turns a view into the one line, fitted to width.
//
// The shape is design.md section 9's: slots first, then the load signals that explain the
// slot count. Every clause after the first is conditional, for the same reason the gate's
// deny text is — a clause that states a fact the daemon did not send is a fabrication, and
// on a surface a person reads continuously a fabricated number is worse than a missing one.
// CPU is null until the daemon's first sample lands, and thermal is "unknown" on every Linux
// host by design, so both of those omissions are ordinary rather than errors.
func render(v statusView, width int) string {
	if !v.OK {
		// Says so rather than erroring. The line is the only channel this command has, so
		// an unreachable daemon is reported on it in the words a person needs: not that a
		// dial failed, but that nothing is being governed right now.
		line, _ := fit([]string{"governor: daemon not running", "spawns ungoverned"}, width)
		return line
	}

	if v.Ungoverned {
		// Said instead of the slot count, not beside it. The count is the thing that would be
		// misread, so leaving it on the line and appending a warning would keep the misreading
		// available; and this is the one state where what a person needs is not a number but
		// an instruction. The daemon-down line above is worded the same way for the same
		// reason — both mean "nothing is capping this", and both name what to do about it.
		line, _ := fit([]string{"governor: ungoverned", "restart to enable"}, width)
		return line
	}

	line, cut := fit(clauses(v, v.Mine > 0), width)
	if cut && v.Mine > 0 {
		// The slot count itself had to be cut, which on a real terminal produced
		// "2/5 slots (1" — a count sliced through the middle, which is worse than no count
		// at all because it still looks like a number. The session-local suffix is the part
		// worth losing: it is an addition to the slot count rather than part of it, so
		// dropping it whole leaves "2/5 slots", which is the point of the line.
		line, _ = fit(clauses(v, false), width)
	}
	return line
}

// formatSlots renders held slots without a pointless decimal.
//
// Whole numbers print whole — "3/6 slots" — because that is the common case once builds are
// what fill the budget, and a line that said "3.0/6" on every render would look like a
// precision nobody asked for. A fraction prints to one place, so a machine holding only
// agents reads "0.2/6 slots" and is visibly nearly empty rather than rounding to a
// reassuring zero.
func formatSlots(s float64) string {
	if r := math.Round(s); math.Abs(s-r) < 0.05 {
		return strconv.FormatFloat(r, 'f', 0, 64)
	}
	return strconv.FormatFloat(s, 'f', 1, 64)
}

// clauses builds the parts of the line, most important first, optionally including the
// session-local slot count.
func clauses(v statusView, mine bool) []string {
	out := make([]string, 0, 4)
	// SLOTS, NOT LEASES, and the two stopped being the same number in GOV-15. A subagent
	// weighs about 0.08 slots against a Heavy build's 3.75, so rendering the lease COUNT
	// beside the slot BUDGET would put two different units either side of the slash: four
	// agents would read "4/6 slots" on a machine that is 5% full. Budget is a quantity of
	// build capacity and this has to be the same quantity.
	slots := fmt.Sprintf("%s/%d slots", formatSlots(v.Slots), v.Budget)
	if mine {
		// Whose slots. The machine-wide figure alone does not answer the question a person
		// in this session is actually asking, which is whether what fills the budget is
		// theirs. This one stays a COUNT of leases, and says so, because "0.16 yours" is
		// not a thing anyone wants to read and the question behind it is "how many of those
		// are mine". Only when non-zero: a session holding nothing is the common case, and
		// "(0 yours)" on every line would be noise on the surface with least room for it.
		slots += fmt.Sprintf(" (%d yours)", v.Mine)
	}
	out = append(out, slots)
	if v.Waiting > 0 {
		// Only when non-zero, and it is here because a full budget and a full budget with a
		// queue behind it are different situations that read identically without it. It is
		// also the signal GOV-14 will be debugged against: a wait queue that never drains
		// is visible here and nowhere else a person looks.
		out = append(out, fmt.Sprintf("%d waiting", v.Waiting))
	}
	if v.CPU != nil {
		out = append(out, fmt.Sprintf("cpu %.0f%%", *v.CPU*100))
	}
	if t := thermalWord(v.Thermal); t != "" {
		out = append(out, "thermal "+t)
	}
	return out
}

// thermalWord renders a thermal state for a human, or "" for one not worth a clause.
//
// Nominal becomes "ok", which is the word both design.md section 9 and this story's
// acceptance criterion use. That is a rendering and not a re-labelling: `governor status`
// prints the wire value verbatim, and this is the one surface where the audience is a
// person reading a strip of text between two other tools' output rather than a program
// parsing a field.
//
// Nothing above nominal is softened. fair, serious and critical are the states the control
// loop cuts the budget on, and they appear here in the daemon's own words precisely because
// a person seeing an unfamiliar one should go and look it up rather than read a reassuring
// synonym. Unknown produces no clause at all: it is what every Linux host reports and it is
// a statement about the measurement rather than about the machine, so putting "thermal
// unknown" on every Linux status line forever would spend the width on nothing.
func thermalWord(t signals.Thermal) string {
	switch t {
	case signals.Nominal:
		return "ok"
	case "", signals.Unknown:
		return ""
	default:
		return string(t)
	}
}

// clauseSep joins the clauses of the line.
const clauseSep = " - "

// fit joins the clauses into one line no wider than width, and reports whether it had to
// cut text to get there rather than dropping whole clauses.
//
// The caller uses that flag to offer a shorter first clause before accepting a cut, which
// is the difference between "2/5 slots" and "2/5 slots (1" on a narrow terminal.
//
// Clauses are dropped from the end rather than the line being cut mid-word, because they
// are ordered by how much a reader needs them: the slot count is the point of the line and
// the thermal state is context for it. A narrow terminal should lose the context and keep
// the point.
//
// Only when even the first clause does not fit is the text cut, and then by runes rather
// than bytes. That cut is not reachable from render() today — every first clause it builds
// is ASCII, so a multi-byte thermal value is dropped whole long before anything is cut, and
// mutating the cut to bytes leaves the suite green. It is written rune-safe anyway because
// the multi-byte case is one edit away rather than hypothetical: design.md section 9
// illustrated this line with a leading spinner glyph and middle-dot separators until GOV-B09
// deleted that illustration, and the moment a glyph returns — or a thermal word arrives that
// is not ASCII — a byte cut splits a character on the one part of the screen reserved for the
// governor's own report. TestFit covers the branch directly.
//
// width <= 0 means "unknown", and is not a constraint. COLUMNS is frequently unset for a
// child process, and treating an absent width as a width of zero would render every status
// line on such a machine as the empty string.
func fit(parts []string, width int) (string, bool) {
	line := strings.Join(parts, clauseSep)
	if width <= 0 || len(line) <= width {
		// len is a safe fast path only because it over-estimates: any multi-byte character
		// makes len exceed the rune count, so a line that passes this test fits for certain.
		return line, false
	}
	for {
		if runeLen(line) <= width {
			return line, false
		}
		if len(parts) <= 1 {
			break
		}
		parts = parts[:len(parts)-1]
		line = strings.Join(parts, clauseSep)
	}
	return string([]rune(line)[:width]), true
}

func runeLen(s string) int { return len([]rune(s)) }

// terminalWidth is the width to fit the line to, from COLUMNS.
//
// Claude Code was not observed to set COLUMNS for the statusline child process on 2.1.265,
// so the usual answer here is 0 — meaning unknown, and no truncation. It is read anyway
// because a person who wires this command up by hand, or runs it in a pipeline, sets it,
// and because the harness may start setting it at any version.
func terminalWidth() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS")))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// statuslineCachePath is where the cache lives, or false if there is no state directory to
// put it in.
func statuslineCachePath() (string, bool) {
	dir, err := transport.StateDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, statuslineCacheFile), true
}

// readCache returns the cached status if it is younger than the TTL.
//
// Every failure is a miss: a missing file, a truncated one, a half-written one, a clock
// that moved. There is nothing to report about any of them — the answer is to ask the
// daemon, which is what a miss does.
func readCache(now time.Time) (cachedStatus, bool) {
	path, ok := statuslineCachePath()
	if !ok {
		return cachedStatus{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cachedStatus{}, false
	}
	var c cachedStatus
	if err := json.Unmarshal(raw, &c); err != nil {
		return cachedStatus{}, false
	}
	age := now.Sub(c.At)
	// A cache stamped in the future is a clock that moved, not a fresh entry. Treating it
	// as fresh would pin the status line at a stale reading until the clock caught up.
	if age < 0 || age > statuslineCacheTTL {
		return cachedStatus{}, false
	}
	return c, true
}

// writeCache stores the status for the next invocation, atomically.
//
// Atomic because every session on the machine writes this file and all of them read it: a
// reader that catches a partial write gets a parse failure, which is a miss, which is a
// daemon round trip — harmless but pointless, and trivially avoided by renaming into place.
//
// The state directory is never created. A governor that is not installed has no state
// directory, and a statusline that conjured one would leave a file behind after uninstall,
// which design.md section 13.3 exists to prevent.
func writeCache(diag io.Writer, c cachedStatus) {
	path, ok := statuslineCachePath()
	if !ok {
		return
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintf(diag, "governor statusline: no state directory at %s, not caching\n", dir)
		return
	}
	raw, err := json.Marshal(c)
	if err != nil {
		fmt.Fprintf(diag, "governor statusline: encode cache: %v\n", err)
		return
	}
	// Same directory as the target, so the rename is on one filesystem and therefore atomic.
	tmp, err := os.CreateTemp(dir, statuslineCacheFile+".*")
	if err != nil {
		fmt.Fprintf(diag, "governor statusline: create cache: %v\n", err)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		fmt.Fprintf(diag, "governor statusline: write cache: %v\n", err)
		return
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintf(diag, "governor statusline: close cache: %v\n", err)
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		fmt.Fprintf(diag, "governor statusline: install cache: %v\n", err)
	}
}
