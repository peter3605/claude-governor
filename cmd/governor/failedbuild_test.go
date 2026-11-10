package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// postToolUseFailurePayload is the PostToolUseFailure payload Claude Code 2.1.321 sent for a
// failing `go build`, verbatim apart from the paths (docs/hook-surface.md section 25). It
// carries the SAME tool_use_id its PreToolUse did, which is the whole reason a failed build
// can be released exactly rather than by the reaper.
func postToolUseFailurePayload(session, toolUseID string) string {
	return `{"session_id":"` + session + `","cwd":"/w","prompt_id":"p","permission_mode":"default",` +
		`"hook_event_name":"PostToolUseFailure","tool_name":"Bash",` +
		`"tool_input":{"command":"cd broken && go build ./...","description":"build"},` +
		`"tool_use_id":"` + toolUseID + `",` +
		`"error":"Exit code 1\n# broken\n./main.go:4:6: declared and not used: x",` +
		`"is_interrupt":false,"duration_ms":170}`
}

// harnessFires plays the part of Claude Code for one hook event. It reads the hooks.json the
// installer writes and runs whatever governor hook that file registers for the event, if the
// matcher selects the tool. If the file registers nothing for the event, nothing runs.
//
// That indirection is the point. GOV-49's defect was not in `finish`: given a failure payload,
// buildFinish already releases the lease. The defect was that no hook was registered for the
// event that carries the payload, so the release never ran. A test that called buildFinish
// directly would have passed on the broken build. This one goes through the file the harness
// actually reads.
func harnessFires(t *testing.T, event, tool, payload string) (ran bool) {
	t.Helper()
	raw, err := pluginHooks()
	if err != nil {
		t.Fatalf("pluginHooks: %v", err)
	}
	var file struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("hooks.json does not parse: %v", err)
	}
	builders := map[string]builder{"finish": buildFinish, "release": buildRelease, "confirm": buildConfirm}
	for _, entry := range file.Hooks[event] {
		if entry.Matcher != "" && !regexp.MustCompile("^(?:"+entry.Matcher+")$").MatchString(tool) {
			continue
		}
		for _, h := range entry.Hooks {
			fields := strings.Fields(h.Command)
			if len(fields) < 2 {
				t.Fatalf("%s hook command %q has no subcommand", event, h.Command)
			}
			build, ok := builders[fields[1]]
			if !ok {
				t.Fatalf("%s runs `governor %s`, which this harness does not model", event, fields[1])
			}
			lifecycleHook(strings.NewReader(payload), io.Discard, transport.Dial{}, hookBudget, build)
			ran = true
		}
	}
	return ran
}

// TestAFailedBuildDoesNotBlockItsOwnRetry replays Day 7 of docs/dogfooding.md (GOV-49).
//
// On 2026-11-09 a muncheez session was granted a Heavy `xcodebuild test` at 13:12:18. The
// command failed. Its four retries, at +50s, +78s, +231s and +260s, were each parked twenty
// seconds and refused at `slots=3.75/4`, which was its own lease and nothing else. The first
// grant came at 13:21:49, after the 300 s reaper had collected the lease. A failed command
// raised no PostToolUse, the only event `finish` was registered on, so the lease was released
// by the TTL. Nothing released it when the command actually ended.
//
// The loop below is that session's fail, fix, retry cycle. Every attempt fails, and every
// retry must be admitted, because by then the session's only lease belonged to a command
// that had already returned.
func TestAFailedBuildDoesNotBlockItsOwnRetry(t *testing.T) {
	endpoint, d := startTestDaemon(t, 4)
	t.Setenv(transport.EndpointEnv, endpoint)

	acquireHeavy := func(toolUseID string) refuseReply {
		t.Helper()
		line, err := callRaw(t, endpoint, `{"v":1,"op":"acquire","session":"muncheez","kind":"build",`+
			`"class":"heavy","slots":3.75,"tool_use_id":"`+toolUseID+`","wait_ms":0}`+"\n")
		if err != nil {
			t.Fatalf("acquire %s: %v", toolUseID, err)
		}
		var r refuseReply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("acquire %s reply %q: %v", toolUseID, line, err)
		}
		return r
	}

	if r := acquireHeavy("toolu_1312"); !r.OK {
		t.Fatalf("the first Heavy was refused against an empty table: %+v", r)
	}
	// The control. While the first build's lease is held, a second Heavy from the same session
	// does not fit (3.75 + 3.75 > 4), and section 6.1a's escape does not apply because the
	// session holds something. If this were admitted, the loop below would pass whether or
	// not anything released the failed build.
	if r := acquireHeavy("toolu_while_running"); r.OK {
		t.Fatalf("a second Heavy was admitted beside the first; the test cannot tell a released " +
			"lease from a leaked one")
	} else if r.Own != 3.75 {
		t.Errorf("the refusal says the session holds %.2f of %.2f slots, want 3.75: it is the only holder",
			r.Own, r.Slots)
	}
	for i, at := range []string{"+50s", "+78s", "+231s", "+260s"} {
		failed := "toolu_1312"
		if i > 0 {
			failed = fmt.Sprintf("toolu_retry%d", i-1)
		}
		if !harnessFires(t, "PostToolUseFailure", "Bash", postToolUseFailurePayload("muncheez", failed)) {
			t.Fatalf("hooks.json registers nothing for PostToolUseFailure on Bash, so the failed "+
				"build %s keeps its 3.75 slots until the %v reaper", failed, lease.DefaultBuildTTL)
		}
		r := acquireHeavy(fmt.Sprintf("toolu_retry%d", i))
		if !r.OK {
			t.Fatalf("the retry at %s was refused (slots=%.2f/%d, cause=%q): the session's own "+
				"failed build is still holding its lease", at, r.Slots, r.Budget, r.Cause)
		}
	}
	if got := d.table.Slots(); got != 3.75 {
		t.Errorf("table holds %.2f slots after the loop, want 3.75: only the last retry is running", got)
	}
}

// TestARefusalAgainstItsOwnLeaseSaysSo holds clause 4 of GOV-49.
//
// On 2026-11-08 01:44 a session was refused three times while it held every slot in use. The
// third refusal told it "Something on this machine has been holding the slots this build
// needs ... tell the user the machine is contended", and it told its user "another session is
// holding the slots". No other session held any. With the daemon now sending Own, each
// escalation step has to name the session itself and nobody else.
func TestARefusalAgainstItsOwnLeaseSaysSo(t *testing.T) {
	// Phrases that put the slots in someone else's hands. "No other session is holding" is the
	// correct sentence here, so the bare words "other session" are not on the list.
	blame := []string{"another session", "Other sessions", "other sessions are", "Other builds",
		"contended", "Something on this machine"}
	for denials := 1; denials <= 3; denials++ {
		text := denyReason(acquireResponse{
			Reason: "capacity", Cause: "capacity", Budget: 4, Slots: 3.75, Asking: 3.75, Own: 3.75,
			Denials: denials,
		}, intent{kind: "build"})
		if !strings.HasPrefix(text, "This session's own earlier build is holding the slots") {
			t.Errorf("refusal %d does not lead with the session's own build:\n%s", denials, text)
		}
		for _, b := range blame {
			if strings.Contains(text, b) {
				t.Errorf("refusal %d says %q, but this session holds every slot in use:\n%s", denials, b, text)
			}
		}
	}

	// The control. Holding some of the slots is not holding all of them: with 1 of the 3.75
	// held by this session, someone else holds the rest, and the existing sentence is true.
	text := denyReason(acquireResponse{
		Reason: "capacity", Cause: "capacity", Budget: 4, Slots: 3.75, Asking: 3.75, Own: 1, Denials: 1,
	}, intent{kind: "build"})
	if strings.HasPrefix(text, "This session's own") {
		t.Errorf("a session holding 1 of 3.75 held slots was told they are all its own:\n%s", text)
	}
	// An older daemon sends no Own, and the gate prints what it always printed.
	if old := denyReason(acquireResponse{Reason: "capacity", Budget: 4, Slots: 3.75, Asking: 3.75, Denials: 1},
		intent{kind: "build"}); !strings.HasPrefix(old, "Not enough free build slots") {
		t.Errorf("without Own the refusal changed:\n%s", old)
	}
}
