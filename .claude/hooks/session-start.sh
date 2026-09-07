#!/usr/bin/env bash
# Launch Control - inject the current story binding at session start.
# Never fails the session: any error exits 0 silently.
set -uo pipefail
DIR="${CLAUDE_PROJECT_DIR:-$PWD}/.claude"
[ -f "$DIR/launch-control.json" ] || exit 0

python3 - "$DIR" <<'PY' 2>/dev/null || exit 0
import json, os, sys
d = sys.argv[1]
try:
    cfg = json.load(open(os.path.join(d, "launch-control.json")))
except Exception:
    sys.exit(0)

cur = ""
p = os.path.join(d, ".current-story")
if os.path.exists(p):
    cur = open(p).read().strip()

notice = cfg.get("notice", "").strip()

lines = [
    "## Launch Control",
    "",
    f"This repo is tracked in Notion as project **{cfg.get('project','?')}** "
    f"(story IDs `{cfg.get('prefix','?')}...`). The backlog is the source of truth "
    "for what to work on and what counts as finished.",
    "",
]
if cur:
    lines += [
        f"**This session is bound to story `{cur}`.**",
        "",
        "Before doing anything else, fetch that story from Notion and re-read its "
        "**Done when** and **Notes and traps** fields. When the work is finished, run "
        "`/done` so it is ticked off and the next stories are unblocked. Put the story "
        "ID in every commit subject.",
    ]
else:
    lines += [
        "**No story is bound to this session yet.**",
        "",
        "If the user asks what to work on, run `/next`. If they name specific work, "
        "check with `/next` or a Notion search whether it is already a story and run "
        "`/start <ID>`; if it genuinely is not on the backlog, run `/groom` to file it "
        "first. Untracked work is how the last tracker went stale.",
    ]

if notice:
    lines += ["", "### Read this before you start", "", notice]

print(json.dumps({
    "hookSpecificOutput": {
        "hookEventName": "SessionStart",
        "additionalContext": "\n".join(lines),
    }
}))
PY
