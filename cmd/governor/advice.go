package main

// GOV-17 clause 1: the standing instruction every session carries.
//
// design.md section 8 divides the advice across three channels, by WHEN the model sees each
// (hook-surface.md sections 16.1 and 21.2). This is the first of them: SessionStart
// additionalContext, which is in context from the first turn, before any decision to build has
// been made. The skill's description is the second and its body the third (pluginSkill).
//
// SHIPPED AS A CONVENIENCE, NOT AS A PROVEN SAVING. docs/advice.md measured whether advice like
// this lowers the machine's load and found the question unscoreable on one laptop — the
// baseline's own halves differed by twice the pre-registered bar. Nothing in this file is
// evidence that it helps. It ships because a session that does not know the tool exists cannot
// ask, and a refusal it was not warned about costs it a round trip to learn what it could have
// been told.

// sessionStartAdvice is the standing instruction.
//
// KEEP IT SHORT AND NEGATIVE-SCOPED, and the two are the story's own trap. It is paid for in
// context on every turn of every session, and "before an expensive build" invites a model that
// is unsure to ask before everything — at which point the habit is abandoned. So it says what
// NOT to ask about as plainly as what to ask about, and makes "unsure" resolve to not asking,
// which is safe because the gate classifies every command itself.
//
// IT SAYS THE ANSWER NEVER DECIDES WHETHER A TEST RUNS. That is clause 3: a session told to
// check before building must not respond by not building, because a lower load bought by
// skipping the test suite is a worse machine reported as a better one.
//
// The tool name is the one a session actually sees, measured in hook-surface.md section 20.5,
// stutter and all. TestTheAdviceNamesTheToolASessionSees holds it to pluginMCP and toolName.
const sessionStartAdvice = `claude-governor shares one CPU budget between every Claude Code session on this machine. ` +
	`Before an expensive build or test-suite run (go build/test, cargo, make, pytest, xcodebuild, npm run build/test, docker build), ` +
	`call mcp__plugin_build-governor_governor__governor__request_build. ` +
	`If it is not granted, wait about wait_hint_ms and ask again. ` +
	`The answer decides when a build or test runs, never whether: do not skip, shrink or drop a test run because of it. ` +
	`Do NOT call it for anything else: reading or searching files, git, ls, grep, edits, a dev server, a quick script. ` +
	`If unsure, do not call; the hook gates expensive commands either way. Details: the build-governor:heavy-build skill.`

// sessionStartOutput is the SessionStart hook's whole stdout, as one JSON object.
//
// ONE OBJECT, NOT TWO LINES. The first-run notice (onboard.go) and the standing instruction go
// to disjoint audiences — systemMessage to the person, additionalContext to the model — and
// hook-surface.md section 16.1 measured that the two fields compose inside one object, each
// unaware of the other. Two JSON documents on one stdout is a shape nobody measured, and a hook
// whose stdout Claude Code cannot parse reports an error instead of either message.
type sessionStartOutput struct {
	// SystemMessage is the first-run notice, and is absent on every session but the first.
	SystemMessage      string             `json:"systemMessage,omitempty"`
	HookSpecificOutput sessionStartAdvise `json:"hookSpecificOutput"`
}

type sessionStartAdvise struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}
