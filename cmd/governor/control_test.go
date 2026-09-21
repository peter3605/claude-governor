package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/lease"
)

// The shipped command and the repository's copy are one file.
//
// The bytes are a Go literal because go:embed cannot reach out of its own directory, and a
// second copy of anything is a second copy that drifts. design.md section 9 names
// commands/governor.md as a repository file and section 13.3 names the installed one; this is
// what keeps those two sentences describing the same bytes.
func TestTheShippedCommandMatchesTheRepository(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "commands", commandFileName))
	if err != nil {
		t.Fatalf("reading the repository's copy of the command: %v", err)
	}
	if string(onDisk) != string(pluginCommand()) {
		t.Error("commands/governor.md and the bytes install writes have diverged; " +
			"regenerate cmd/governor/command_md.go from the markdown")
	}
}

// The three things about the command file that hook-surface.md section 22 measured, asserted
// against the file rather than trusted to survive an edit.
//
// EVERY ONE OF THESE IS A SILENT FAILURE IF IT REGRESSES. A `$1` reads the wrong word, an
// inline `!` produces a model narrating a command it never ran, and a missing allowed-tools
// turns the status verb into a command that can never show a status. None of the three fails
// loudly, and none is visible in a diff unless somebody remembers §22 exists.
func TestTheCommandFollowsTheMeasuredSurface(t *testing.T) {
	md := string(pluginCommand())

	// §22.3: positional substitution is off by one and leaks literal "$N" when out of range.
	for _, bad := range []string{"$1", "$2", "$3", "$9"} {
		if strings.Contains(md, bad) {
			t.Errorf("the command uses %s; hook-surface.md §22.3 measured $N as the (N+1)th "+
				"word, and out of range it reaches the model as the literal text %s", bad, bad)
		}
	}
	if !strings.Contains(md, "$ARGUMENTS") {
		t.Error("the command reads no arguments at all")
	}

	// §22.4: the construct only pre-executes at the start of a line.
	found := false
	for _, line := range strings.Split(md, "\n") {
		if i := strings.Index(line, "!`"); i > 0 {
			t.Errorf("an inline !`…` at column %d: §22.4 measured that it is NOT executed "+
				"there and reaches the model as literal text, which is how a model comes to "+
				"report the text of a command as its output: %q", i, line)
		} else if i == 0 {
			found = true
		}
	}
	if !found {
		t.Error("the command never runs the binary, so /…:governor status cannot show a status")
	}

	// §22.5: without allowed-tools the pre-execution is refused outright.
	if !strings.Contains(md, "allowed-tools:") {
		t.Error("no allowed-tools: §22.5 measured that the !`…` line is then refused and " +
			"nothing is substituted")
	}
	if !strings.Contains(md, "${CLAUDE_PLUGIN_ROOT}/bin/"+installedBinaryName) {
		t.Errorf("the command does not name ${CLAUDE_PLUGIN_ROOT}/bin/%s, which is the only "+
			"path to the binary that is one string on every platform", installedBinaryName)
	}
}

// A daemon wired the way a test can drive, with a fixed budget and no controller.
func controlDaemon(t *testing.T, limit int) *daemon {
	t.Helper()
	d := newDaemonWithLeases(limit, io_Discard{}, lease.Options{})
	return d
}

type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

func ctl(d *daemon, verb string, limit int, for_ time.Duration) controlReply {
	r := d.control(request{
		V: protocolVersion, Op: "control", Session: "s1",
		Control: verb, Limit: limit, ForMS: int(for_ / time.Millisecond),
	})
	return r.(controlReply)
}

// `limit N` pins the budget, and the gate admits against the pin rather than against the
// source.
func TestLimitPinsTheBudgetTheGateAdmitsAgainst(t *testing.T) {
	d := controlDaemon(t, 8)

	if got := d.budgetOnly(request{}); got != 8 {
		t.Fatalf("unpinned budget is %d, want the source's 8", got)
	}
	r := ctl(d, "limit", 2, time.Hour)
	if !r.OK || !r.Pinned || r.Budget != 2 {
		t.Fatalf("limit 2 did not pin: %+v", r)
	}
	if got := d.budgetOnly(request{}); got != 2 {
		t.Errorf("the gate would admit against %d, not the pinned 2", got)
	}
	// What the loop would have chosen is reported beside it, or the pin explains nothing.
	if r.SourceBudget != 8 {
		t.Errorf("source_budget is %d, want the 8 the pin is overruling", r.SourceBudget)
	}
}

// Invariant 4 is not negotiable by a person either.
func TestAPinOfZeroIsRefusedRatherThanFloored(t *testing.T) {
	d := controlDaemon(t, 8)
	r := ctl(d, "limit", 0, time.Hour)
	if r.OK {
		t.Fatalf("limit 0 was accepted: %+v", r)
	}
	if r.Reason != "bad_limit" {
		t.Errorf("reason %q, want bad_limit", r.Reason)
	}
	if d.overrides.state().any() {
		t.Error("a refused limit still left an override in force")
	}
}

// THE STORY'S TRAP, END TO END. A pin that outlives its reason is the failure GOV-B01 exists
// to prevent, so the budget going back on its own — with nothing having released it — is
// asserted against the number the gate actually admits with.
func TestAPinnedBudgetReturnsToTheLoopWhenItExpires(t *testing.T) {
	d := controlDaemon(t, 8)
	clock := &fakeClock{t: time.Now()}
	d.overrides.now = clock.now

	ctl(d, "limit", 2, 30*time.Minute)
	if got := d.budgetOnly(request{}); got != 2 {
		t.Fatalf("budget is %d, want the pinned 2", got)
	}

	clock.add(31 * time.Minute)
	if got := d.budgetOnly(request{}); got != 8 {
		t.Errorf("budget is %d after the pin expired, want the source's 8 back", got)
	}
}

// Pausing stops the cap and nothing else. The lease is still taken, so every surface goes on
// reporting what is genuinely running.
func TestPauseStopsTheCapButNotTheBookkeeping(t *testing.T) {
	d := controlDaemon(t, 1)
	d.table.StartSession(lease.Session{ID: "s1", Weight: 1})

	// Fill the one slot the budget allows.
	first := d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1})
	if _, ok := first.(grantReply); !ok {
		t.Fatalf("the first acquire was not granted: %+v", first)
	}
	// A second is refused while the cap is on.
	if r, ok := d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1}).(refuseReply); !ok {
		t.Fatalf("a full budget did not refuse")
	} else if r.Reason != "capacity" {
		t.Fatalf("refused for %q, want capacity", r.Reason)
	}

	ctl(d, "pause", 0, time.Hour)

	before := d.table.InUse()
	got := d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1})
	if _, ok := got.(grantReply); !ok {
		t.Fatalf("gating is paused but the acquire was still refused: %+v", got)
	}
	if after := d.table.InUse(); after != before+1 {
		t.Errorf("in use went %d -> %d: a paused grant must still be recorded, or status "+
			"stops telling the truth about what is running", before, after)
	}
}

// A pause ends on its own, and the cap comes back.
func TestAPauseEndsOnItsOwn(t *testing.T) {
	d := controlDaemon(t, 1)
	clock := &fakeClock{t: time.Now()}
	d.overrides.now = clock.now
	d.table.StartSession(lease.Session{ID: "s1", Weight: 1})

	ctl(d, "pause", 0, 10*time.Minute)
	d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1})
	d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1})

	clock.add(11 * time.Minute)
	got := d.acquire(request{V: protocolVersion, Session: "s1", Kind: "build", Slots: 1})
	if _, ok := got.(grantReply); ok {
		t.Error("the pause expired but the budget is still uncapped")
	}
}

// resume tells "released" from "there was nothing set", because they are different answers to
// the same command.
func TestResumeSaysWhichOfTheTwoThingsHappened(t *testing.T) {
	d := controlDaemon(t, 8)

	if r := ctl(d, "resume", 0, 0); !r.OK || r.Released {
		t.Errorf("resume on a clean daemon reported releasing something: %+v", r)
	}
	ctl(d, "limit", 2, time.Hour)
	if r := ctl(d, "resume", 0, 0); !r.OK || !r.Released {
		t.Errorf("resume did not report releasing the pin: %+v", r)
	}
	if got := d.budgetOnly(request{}); got != 8 {
		t.Errorf("budget is %d after resume, want 8", got)
	}
}

// boost writes Session.Weight — the one field fair share has read since GOV-18 — rather than
// introducing a second weighting scheme beside it.
func TestBoostMovesTheOneWeightFairShareReads(t *testing.T) {
	d := controlDaemon(t, 8)
	d.table.StartSession(lease.Session{ID: "s1", Weight: 1})

	r := ctl(d, "boost", 0, time.Hour)
	if !r.OK || !r.Boosted || r.Weight != boostWeight {
		t.Fatalf("boost did not land: %+v", r)
	}
	for _, s := range d.table.Snapshot().Sessions {
		if s.ID == "s1" && s.Weight != boostWeight {
			t.Errorf("session weight is %d, want %d — boost must move the field shareLocked "+
				"reads, not a parallel one", s.Weight, boostWeight)
		}
	}
}

// A boost against a session the daemon has never seen is refused by name, not silently
// reported as success.
//
// It matters because the session id reaching this command is UNMEASURED (hook-surface.md
// §22.7): if the environment does not carry one, this is the path every boost takes, and a
// person told "done" would be waiting for an effect that never arrives.
func TestBoostingAnUnknownSessionSaysSo(t *testing.T) {
	d := controlDaemon(t, 8)
	r := ctl(d, "boost", 0, time.Hour)
	if r.Boosted {
		t.Fatalf("boosted a session the daemon has never seen: %+v", r)
	}
	if r.Reason != "unknown_session" {
		t.Errorf("reason %q, want unknown_session", r.Reason)
	}
}

// A boost expires, and the weight it set goes back to 1 on the path fair share actually reads.
func TestABoostExpiresBackToAnEqualShare(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{t: now}
	table := lease.NewTable(lease.Options{Now: clock.now})
	table.StartSession(lease.Session{ID: "s1", Weight: 1})

	table.Boost("s1", 2, now.Add(30*time.Minute))
	if w := weightSeenBy(table, "s1"); w != 2 {
		t.Fatalf("weight is %v just after the boost, want 2", w)
	}

	clock.add(31 * time.Minute)
	if w := weightSeenBy(table, "s1"); w != 1 {
		t.Errorf("weight is %v after the boost expired, want 1 — a forgotten boost is a "+
			"session quietly outbidding every other one for as long as the daemon lives", w)
	}
}

// weightSeenBy reads the weight through the table's own snapshot, which is what reports it.
func weightSeenBy(t *lease.Table, id string) int {
	// Acquire nothing; Snapshot is enough, and it runs the same expiry the admission path does
	// only if the admission path is what expires it — so drive an admission too.
	t.Fits(lease.Request{Session: id, Weight: 0.01}, 8)
	for _, s := range t.Snapshot().Sessions {
		if s.ID == id {
			return s.Weight
		}
	}
	return 0
}

// An older daemon answering `unknown_op` must read as "no answer", never as a refusal.
//
// Section 7 is explicit about this and it is an upgrade-shaped failure: during an upgrade there
// is a daemon of one version and commands of another, and a person told "refused" goes looking
// for the policy that refused them instead of restarting the daemon.
func TestAnOlderDaemonReadsAsNoAnswerRatherThanRefusal(t *testing.T) {
	d := newDaemonWithLeases(4, io_Discard{}, lease.Options{})
	got := d.dispatch(request{V: protocolVersion, Op: "control", Control: "limit", Limit: 2})
	if _, ok := got.(controlReply); !ok {
		t.Fatalf("a current daemon did not handle control: %T", got)
	}
	// And an unrecognised verb is answered, not ignored.
	r := ctl(d, "teleport", 0, 0)
	if r.OK || r.Reason != "unknown_control" {
		t.Errorf("an unknown verb answered %+v, want a named refusal", r)
	}
}

// The budget the sources reported survives beside the pinned one, and Floor is the answer when
// there are no sources at all.
func TestSourceBudgetFloorsRatherThanReportingZero(t *testing.T) {
	if got := sourceBudget(nil); got != budget.Floor {
		t.Errorf("sourceBudget(nil) = %d, want Floor %d", got, budget.Floor)
	}
}

// The override commands are bounded against a daemon that never answers.
//
// THEY ARE NOT HOOKS AND MUST NOT BE HELD TO THE HOOK CONTRACT — a hook fails open in silence
// because a session is blocked on it, while these are typed by a person and their failure is a
// sentence. What they share is the bound: a command that hangs against a wedged daemon gets
// reported as a hung terminal, and the person who typed `governor pause` because the machine
// was already struggling is the last person who should be made to wait.
func TestOverrideCommandsAreBoundedAgainstAWedgedDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
		args []string
	}{
		{"limit", runLimit, []string{"2"}},
		{"pause", runPause, nil},
		{"resume", runResume, nil},
		{"boost", runBoost, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No daemon: DefaultEndpoint resolves into a directory with no socket in it.
			t.Setenv("HOME", t.TempDir())

			done := make(chan int, 1)
			go func() {
				var out, errw strings.Builder
				done <- tc.run(tc.args, &out, &errw)
			}()
			select {
			case code := <-done:
				// Non-zero, unlike a hook: the person needs to know it did not happen.
				if code == 0 {
					t.Errorf("reported success against a daemon that is not running")
				}
			case <-time.After(5 * controlBudget):
				t.Fatalf("did not return within %v of its %v budget", 5*controlBudget, controlBudget)
			}
		})
	}
}

// A bad duration is refused rather than silently taking the default.
//
// `governor pause 30x` applying a one-hour pause would be the command doing something other
// than what was typed, on a surface whose entire job is to do exactly what was typed.
func TestABadDurationIsRefusedRatherThanDefaulted(t *testing.T) {
	for _, args := range [][]string{{"30x"}, {"for", "banana"}, {"-5m"}} {
		if _, ok := parseFor(args); ok {
			t.Errorf("parseFor(%q) accepted an unusable duration", args)
		}
	}
	// Both spellings of the good case work, and so does omitting it.
	for _, args := range [][]string{nil, {"30m"}, {"for", "30m"}, {"FOR", "2h"}} {
		if _, ok := parseFor(args); !ok {
			t.Errorf("parseFor(%q) rejected a usable argument", args)
		}
	}
}

// Every override the command reports names when it ends.
func TestEveryOverrideMessageNamesItsDeadline(t *testing.T) {
	now := time.Now()
	in30 := now.Add(30 * time.Minute).UTC().Format(time.RFC3339)

	for _, tc := range []struct {
		verb  string
		reply controlClientReply
	}{
		{"limit", controlClientReply{OK: true, Budget: 2, SourceBudget: 8, Pinned: true, PinnedUntil: in30}},
		{"pause", controlClientReply{OK: true, Paused: true, PausedUntil: in30}},
		{"boost", controlClientReply{OK: true, Boosted: true, Weight: 2, BoostedUntil: in30}},
	} {
		got := strings.Join(describeControl(tc.verb, tc.reply, now), "\n")
		if !strings.Contains(got, "until") {
			t.Errorf("%s does not say when it ends, which is the whole of GOV-B01's trap:\n%s",
				tc.verb, got)
		}
	}

	// And a reply with no deadline says nothing about one rather than inventing it.
	got := strings.Join(describeControl("limit", controlClientReply{OK: true, Budget: 2, Pinned: true}, now), "\n")
	if strings.Contains(got, "until") {
		t.Errorf("invented a deadline the daemon never sent:\n%s", got)
	}
}

// `limit` says what it is overruling, or the number it printed explains nothing.
func TestLimitReportsWhatTheLoopWouldHaveChosen(t *testing.T) {
	now := time.Now()
	got := strings.Join(describeControl("limit", controlClientReply{
		OK: true, Budget: 2, SourceBudget: 8, Pinned: true,
	}, now), "\n")
	if !strings.Contains(got, "8") {
		t.Errorf("pinned to 2 without saying the loop would have chosen 8; the person has "+
			"changed a number and cannot tell whether they raised it or lowered it:\n%s", got)
	}
}

// `governor status` prints every fact GOV-B01's acceptance criteria name.
//
// The per-session shares are the clause that was missing when this test was written, and the
// gap is the interesting part: the machine-wide totals answer "is the budget full", and only
// the per-session lines answer "full of WHOSE work" — which is the question somebody runs
// status to settle when their own session is the one being refused.
func TestStatusPrintsEveryFactTheStoryNames(t *testing.T) {
	boosted := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	cpu := 0.61
	c := cachedStatus{At: time.Now(), Reply: statusResponse{
		V: protocolVersion, Budget: 6, InUse: 3, Slots: 3, Waiting: 1,
		CPU: &cpu, Thermal: "nominal",
		Sessions: []sessionStat{
			{Session: "sess-a", Weight: 2, Tentative: 1, Confirmed: 1, Registered: true, BoostedUntil: boosted},
			{Session: "sess-b", Weight: 1, Confirmed: 1, Registered: true},
		},
	}}

	var out strings.Builder
	printStatus(&out, c)
	got := out.String()

	for _, want := range []string{
		"budget 6", "in use 3", "waiting 1", "cpu 0.61", "thermal nominal",
		"sess-a", "sess-b", "weight 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("`governor status` does not report %q, which the story's Done-when "+
				"names:\n%s", want, got)
		}
	}
}

// A session the daemon has no weight for reads as the default 1, never as 0.
//
// Zero is what the wire carries for "this daemon recorded no weight", and the lease table
// already reads that as 1 — so printing 0 would show a session as entitled to nothing on a
// line whose whole purpose is to explain who is entitled to what.
func TestAnUnknownWeightPrintsAsTheDefaultShare(t *testing.T) {
	if got := sessionWeight(0); got != 1 {
		t.Errorf("sessionWeight(0) = %d, want the default 1 the table itself uses", got)
	}
	if got := sessionWeight(-3); got != 1 {
		t.Errorf("sessionWeight(-3) = %d, want 1", got)
	}
	if got := sessionWeight(2); got != 2 {
		t.Errorf("sessionWeight(2) = %d, want 2", got)
	}
}

// A live session that never registered is marked on its own line.
//
// Same misreading GOV-29 was filed about — it holds no leases and so looks identical to an idle
// governed session — arriving in the place a person goes to check after the status line has
// already sent them there.
func TestStatusMarksALiveSessionThatNeverRegistered(t *testing.T) {
	if got := liveSuffix(sessionStat{Live: true, Registered: false}); got == "" {
		t.Error("a running, unregistered session is indistinguishable from an idle governed one")
	}
	if got := liveSuffix(sessionStat{Live: true, Registered: true}); got != "" {
		t.Errorf("marked a properly registered session as ungoverned: %q", got)
	}
}

// GOV-34: `governor status` says why the budget is what it is, when something is holding it
// below what this host could allow.
//
// This is the surface the 2026-09-21 incident report quoted — "budget 4, in use 1, waiting 0,
// cpu 0.54, thermal nominal" — and its problem was not that any line was wrong. It was that
// the person reading it could not tell a four-slot machine from a six-slot machine cut to
// four, and neither could the refusal that sent them here.
func TestStatusSaysWhatIsCuttingTheBudget(t *testing.T) {
	cut := cachedStatus{At: time.Now(), Reply: statusResponse{
		V: protocolVersion, Budget: 3, InUse: 1, Slots: 1, Thermal: "nominal",
		Ceiling: 6, Cut: "run queue",
	}}
	var out strings.Builder
	printStatus(&out, cut)
	for _, want := range []string{"budget 3", "run queue", "ceiling 6"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("`governor status` does not report %q:\n%s", want, out.String())
		}
	}

	// And it stays quiet when nothing is cutting anything, rather than printing a line about
	// a ceiling the budget is already at. A status line that always says something about a
	// cut is one nobody reads when there is a cut.
	for _, quiet := range []statusResponse{
		{V: protocolVersion, Budget: 6, Thermal: "nominal", Ceiling: 6, Cut: ""},
		// A daemon too old to send either field. Not a cut; not anything.
		{V: protocolVersion, Budget: 6, Thermal: "nominal"},
	} {
		var out strings.Builder
		printStatus(&out, cachedStatus{At: time.Now(), Reply: quiet})
		if strings.Contains(out.String(), "cut") {
			t.Errorf("`governor status` claims a cut nobody reported:\n%s", out.String())
		}
	}
}
