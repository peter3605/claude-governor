package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/peter3605/claude-governor/internal/config"
)

// The one-time first-run notice (GOV-22).
//
// What is being asked of a stranger here is to let a hook run on every subagent spawn, and
// the first they see of it after install is agents starting slower than they used to. A
// tool that changes when your agents run and does not say so — or says so and does not say
// how to stop it — is the kind of thing people uninstall on principle. So the first session
// after an install is told what is now happening, how to read the status line it is now
// reading, and the exact command that removes it.
//
// It is shown ONCE. This is deliberately not the standing SessionStart instruction of
// design.md section 8 (GOV-17): a notice repeated every session costs context on every turn
// of every session forever, and it is a notice precisely because it only needs saying once.

// onboardMarker records that the notice has been shown. Its existence is its whole meaning;
// the timestamp inside is for a person reading the directory, not for this code.
const onboardMarker = "onboarded"

// onboardPath is the marker's home: beside config.json, which is ${CLAUDE_PLUGIN_DATA} when
// Claude Code has named a data directory for the plugin and the governor's own state
// directory otherwise (design.md section 13.3).
//
// It resolves a name and touches nothing, for config.Path()'s reason: a hook that conjured
// a directory while merely asking where one would be is what section 13.3 exists to prevent.
func onboardPath() (string, error) {
	cfg, err := config.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfg), onboardMarker), nil
}

// claimFirstRun reports whether THIS process is the one that must show the notice, and
// records that it did.
//
// The claim is the file creation itself — O_CREATE|O_EXCL, which the kernel makes atomic —
// rather than a stat followed by a write. Sessions start in parallel all the time, and two
// of them racing a check-then-write would both find no marker and both print. Exactly one
// caller can win a create.
//
// It never creates the directory, which is the rule daemon.start and statusline.json follow
// for the same reason (design.md section 13.3): a hook that ran after uninstall and put a
// directory back would leave residue moments after the one command that promises to remove
// everything. A missing directory therefore means no notice — silence, which is the safe
// direction, because the alternative is a notice that can never be recorded and so repeats
// forever.
func claimFirstRun(diag io.Writer) bool {
	path, err := onboardPath()
	if err != nil {
		fmt.Fprintf(diag, "governor: resolve the onboarding marker: %v\n", err)
		return false
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// Already shown is the overwhelmingly common case and is not worth a line in
		// anybody's log. Anything else is, because it is the reason the notice is missing.
		if !errors.Is(err, fs.ErrExist) {
			fmt.Fprintf(diag, "governor: claim the onboarding marker: %v\n", err)
		}
		return false
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n", time.Now().UTC().Format(time.RFC3339)); err != nil {
		// The claim is already made and the notice is already going to be shown. A marker
		// with no timestamp in it still means what it needs to mean.
		fmt.Fprintf(diag, "governor: stamp the onboarding marker: %v\n", err)
	}
	return true
}

// onboardingNotice is what the first session is told.
//
// Three things, in this order, and the order is the point. The off switch goes first
// because someone who does not want this needs it in the first line, not after two
// paragraphs of what it does for them. Then what it is actually doing to their machine.
// Then how to read the status line, because that is the surface they will see every day and
// it is a row of numbers that means nothing unexplained.
//
// It is short on purpose. Whatever channel carries it, it is competing with the user's own
// work for attention, and a notice nobody finishes reading has told them nothing.
const onboardingNotice = `claude-governor is installed. Turn it off with: governor uninstall

It caps how much building and testing runs at once across every session on this machine,
from live CPU load. Builds are what saturate a laptop — subagents barely register — so a
` + "`go test`" + ` or a ` + "`cargo build`" + ` may be delayed or refused while an idle command never is. A
subagent spawn counts too, at about a twelfth of a build. Nothing is ever altered, only
delayed or refused.

Status line: ` + "`3/5 slots`" + ` is how much of the budget is in use, where one slot is about one
ordinary build, ` + "`(1 yours)`" + ` how many of those are this session's, and ` + "`cpu 61%`" + ` /
` + "`thermal ok`" + ` the readings the budget moves on.
` + "`governor: daemon not running - spawns ungoverned`" + ` means nothing is being capped, and
` + "`governor: ungoverned - restart to enable`" + ` means this session started before the governor
did — hooks are bound when a session opens, so only restarting it will govern this one.`

// firstRunNoticeBudget is the allowance for claiming the marker and rendering the notice.
//
// It is hookBudget's 100 ms, applied a second time rather than shared with the daemon
// exchange that follows, and the SessionStart hook is the one place where spending twice is
// defensible: it runs once as a session opens, where a person is already waiting for a
// session to appear, while confirm and release run per agent inside a fan-out. Those are not
// the same patience. The work under it is a single O_EXCL create, so the budget is a
// backstop against a wedged filesystem and nothing else.
const firstRunNoticeBudget = hookBudget

// sessionNotice is the SessionStart hook's stdout when it has something to tell the user.
//
// systemMessage, NOT hookSpecificOutput.additionalContext, and the difference is measured
// rather than assumed — docs/hook-surface.md section 16. systemMessage is rendered to the
// PERSON, as "SessionStart:startup says: <text>", and contributes literally nothing to the
// model's context. additionalContext is the exact opposite: model only, and invisible to the
// person. This notice is for the person, and the story's own trap is that it must not become
// a standing instruction the model then carries for the rest of the session.
//
// Nothing else goes in this object. An unrecognised key is ignored rather than fatal, but a
// hook that prints parseable JSON and then fails suppresses the error Claude Code would
// otherwise have shown the user (section 16, D1) — so the less this hook says on stdout, the
// less it can hide.
type sessionNotice struct {
	SystemMessage string `json:"systemMessage"`
}

// emitFirstRunNotice shows the notice, once, on the first session after an install.
//
// The claim happens before the daemon exchange rather than after it, because the notice must
// not depend on a daemon being up: the machine a person most needs this explanation on is the
// one where something is already wrong.
//
// One accepted loss, stated because it is a real hole rather than an oversight: if the budget
// expires between the marker being created and this function returning, the marker exists and
// the notice was never printed, so it is never shown. That costs one user one paragraph in the
// case where a single openat(2) took longer than a tenth of a second. The alternative —
// printing first and recording afterwards — trades it for two racing sessions both printing,
// and for a repeat every session until the write succeeds, which is the failure people
// actually notice.
func emitFirstRunNotice(stdout, stderr io.Writer) {
	// Buffered, so the worker can never block on a send this function has stopped waiting
	// for, and so a send that lands after the budget expired is absorbed rather than lost
	// into a deadlock.
	notice := make(chan string, 1)
	withBudget(stderr, firstRunNoticeBudget, func(diag io.Writer, _ *budgetClock) {
		if claimFirstRun(diag) {
			notice <- onboardingNotice
		}
	})
	select {
	case text := <-notice:
		// Marshalled rather than assembled, because the notice contains backticks, percent
		// signs and newlines, and a hand-built JSON string is one edit away from being
		// unparseable — at which point Claude Code would report a hook error instead of the
		// message, on the first session, which is the worst possible moment.
		line, err := json.Marshal(sessionNotice{SystemMessage: text})
		if err != nil {
			fmt.Fprintf(stderr, "governor session: render the first-run notice: %v\n", err)
			return
		}
		fmt.Fprintf(stdout, "%s\n", line)
	default:
	}
}
