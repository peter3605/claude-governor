package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// The client half of the manual overrides (design.md section 9.1, GOV-B01).
//
// These are what `/build-governor:governor` actually runs. They are ordinary subcommands
// rather than anything the slash command owns, so that the override is equally available from
// a script, from a second terminal, and to somebody debugging a machine whose sessions are all
// wedged — which is exactly when a person most wants to say "stop capping me".
//
// THEY ARE NOT HOOKS AND MUST NOT BEHAVE LIKE ONE. A hook fails open in silence because the
// thing waiting on it is a session; these are typed by a person, and their failure mode is a
// sentence. So a daemon that cannot be reached is reported as such and exits non-zero, where
// the gate would have exited 0 and said nothing.

// controlBudget is the wall-clock allowance for one override command.
//
// It matches `governor status` rather than the gate: a person who has just typed a command has
// already decided to wait for an answer. It is still bounded, because a command that hangs
// against a wedged daemon gets reported as a hung terminal rather than as a wedged daemon.
const controlBudget = time.Second

// controlClientRequest is the control message of design.md section 7.
//
// Its keys are deliberately not `slots` or `action`: the daemon decodes every operation into
// one struct, where both of those names are already taken with other types and other meanings.
// See the request struct in daemon.go for the full account.
type controlClientRequest struct {
	V         int       `json:"v"`
	Op        string    `json:"op"`
	Principal principal `json:"principal"`
	Session   string    `json:"session"`
	Control   string    `json:"control"`
	Limit     int       `json:"limit,omitempty"`
	ForMS     int       `json:"for_ms,omitempty"`
}

// controlClientReply mirrors the daemon's controlReply.
//
// A separate type for the same reason statusResponse is separate from statusReply: these are
// two halves of a wire protocol that will at times be different binary versions (invariant 8),
// and a shared struct hides the moment they diverge.
type controlClientReply struct {
	V            int    `json:"v"`
	OK           bool   `json:"ok"`
	Reason       string `json:"reason"`
	Budget       int    `json:"budget"`
	SourceBudget int    `json:"source_budget"`
	Pinned       bool   `json:"pinned"`
	PinnedUntil  string `json:"pinned_until"`
	Paused       bool   `json:"paused"`
	PausedUntil  string `json:"paused_until"`
	Weight       int    `json:"weight"`
	Boosted      bool   `json:"boosted"`
	BoostedUntil string `json:"boosted_until"`
	Released     bool   `json:"released"`
	InUse        int    `json:"in_use"`
}

func runLimit(args []string, stdout, stderr io.Writer) int {
	n, rest, ok := parseLimitArgs(args)
	if !ok {
		fmt.Fprintln(stderr, "usage: governor limit <slots> [for <duration>]")
		return 2
	}
	d, ok := parseFor(rest)
	if !ok {
		fmt.Fprintln(stderr, "usage: governor limit <slots> [for <duration>]")
		return 2
	}
	return control(stdout, stderr, controlClientRequest{Control: "limit", Limit: n, ForMS: ms(d)})
}

func runPause(args []string, stdout, stderr io.Writer) int {
	d, ok := parseFor(args)
	if !ok {
		fmt.Fprintln(stderr, "usage: governor pause [for <duration>]")
		return 2
	}
	return control(stdout, stderr, controlClientRequest{Control: "pause", ForMS: ms(d)})
}

func runResume(_ []string, stdout, stderr io.Writer) int {
	return control(stdout, stderr, controlClientRequest{Control: "resume"})
}

func runBoost(args []string, stdout, stderr io.Writer) int {
	d, ok := parseFor(args)
	if !ok {
		fmt.Fprintln(stderr, "usage: governor boost [for <duration>]")
		return 2
	}
	// The session id comes from the environment Claude Code sets for the command it runs.
	// Empty is not fatal here — the daemon answers `unknown_session` and control() prints a
	// sentence about it, which is a better failure than this process guessing at an id.
	return control(stdout, stderr, controlClientRequest{
		Control: "boost", Session: os.Getenv(sessionIDEnv), ForMS: ms(d),
	})
}

// sessionIDEnv is where a command run inside a session learns which session it is.
//
// UNMEASURED, AND HANDLED AS SUCH. hook-surface.md §22.7 records that the environment a slash
// command's subprocess inherits was not probed, so this may well be empty in practice. That is
// why `boost` degrades to a named refusal rather than to a wrong answer: a boost applied to the
// wrong session would hand somebody else's window a double share and expire an hour later,
// which is far worse than being told it did not work.
const sessionIDEnv = "CLAUDE_SESSION_ID"

// control sends one override and prints what came back.
func control(stdout, stderr io.Writer, req controlClientRequest) int {
	req.V = protocolVersion
	req.Op = "control"
	req.Principal = localPrincipal()

	var resp controlClientReply
	if !exchange(stderr, transport.Dial{}, time.Now().Add(controlBudget), req, &resp) {
		fmt.Fprintln(stdout, "governor: daemon not running, so there is nothing to override")
		return 1
	}
	if resp.V != protocolVersion {
		// Answered, not ignored (invariant 8) — and reported as no answer, which is how every
		// other client in this binary treats a version it does not speak.
		fmt.Fprintf(stdout, "governor: daemon speaks protocol v%d, this binary speaks v%d\n",
			resp.V, protocolVersion)
		return 1
	}
	if !resp.OK {
		// An older daemon answers `unknown_op`, and it MUST read as "no answer" rather than as
		// a refusal (section 7). During an upgrade there will be a daemon of one version and
		// commands of another, and a person told "refused" would go looking for the policy
		// that refused them.
		switch resp.Reason {
		case "unknown_op", "unknown_control":
			fmt.Fprintln(stdout, "governor: this daemon does not support overrides; restart it with a current binary")
		case "bad_limit":
			fmt.Fprintln(stdout, "governor: a limit must be at least 1 slot — the budget never reaches zero")
		default:
			fmt.Fprintf(stdout, "governor: refused (%s)\n", resp.Reason)
		}
		return 1
	}

	for _, line := range describeControl(req.Control, resp, time.Now()) {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// describeControl is what the person reads. It is a pure function of the reply so that the
// wording is testable without a daemon.
//
// EVERY LINE NAMES WHEN IT ENDS, and that is the whole of this story's trap rather than
// politeness. An override whose expiry is invisible is one nobody can reason about later, and
// "later" is precisely when a forgotten override does its damage.
func describeControl(verb string, r controlClientReply, now time.Time) []string {
	var out []string
	switch verb {
	case "limit":
		out = append(out, fmt.Sprintf("budget pinned at %d slots%s", r.Budget, untilClause(r.PinnedUntil, now)))
		if r.SourceBudget > 0 && r.SourceBudget != r.Budget {
			// What the pin is overruling. Without it the person has changed a number and has
			// no idea whether they raised it or lowered it.
			out = append(out, fmt.Sprintf("the control loop would have chosen %d", r.SourceBudget))
		}
	case "pause":
		out = append(out, "gating paused"+untilClause(r.PausedUntil, now))
		out = append(out, "leases are still recorded, so `governor status` still reports what is running")
	case "resume":
		if r.Released {
			out = append(out, "override released")
		} else {
			out = append(out, "nothing was overridden")
		}
		out = append(out, fmt.Sprintf("budget %d slots, %d in use", r.Budget, r.InUse))
	case "boost":
		if !r.Boosted {
			out = append(out, "boost not applied: this daemon has no record of this session")
			out = append(out, "a session registers at start-up, so a session older than the daemon has to be restarted")
			break
		}
		out = append(out, fmt.Sprintf("this session's share weight is now %d%s", r.Weight, untilClause(r.BoostedUntil, now)))
		out = append(out, "weight decides who refills a contended budget, not who reaches it first")
	}
	return out
}

// untilClause renders " until <time> (<d> from now)", or nothing at all when the deadline is
// absent or unparseable.
//
// Nothing rather than a placeholder: a surface that printed "until unknown" would be stating a
// fact the daemon did not send, which is the rule the status line and the gate's refusal text
// both follow.
func untilClause(stamp string, now time.Time) string {
	if stamp == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	left := t.Sub(now)
	if left <= 0 {
		return ""
	}
	return fmt.Sprintf(" until %s (%s from now)", t.Local().Format("15:04"), roundDuration(left))
}

// roundDuration renders a remaining time the way a person would say it.
func roundDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	case d >= time.Minute:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Second).String()
	}
}

func ms(d time.Duration) int { return int(d / time.Millisecond) }

// parseLimitArgs pulls the slot count off the front of the arguments.
func parseLimitArgs(args []string) (int, []string, bool) {
	if len(args) == 0 {
		return 0, nil, false
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, nil, false
	}
	return n, args[1:], true
}

// parseFor reads an optional `for <duration>` suffix.
//
// The `for` word is optional so that both `governor pause 30m` and `governor pause for 30m`
// work; a person typing this into a terminal should not have to remember which one it wants.
// An empty tail is a valid answer — it means "use the default" — and only an unparseable
// duration is an error, because silently taking the default for `pause 30x` would apply an
// override for a length nobody asked for.
func parseFor(args []string) (time.Duration, bool) {
	if len(args) > 0 && strings.EqualFold(args[0], "for") {
		args = args[1:]
	}
	if len(args) == 0 {
		return 0, true
	}
	d, err := time.ParseDuration(args[0])
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
