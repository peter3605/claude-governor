#!/usr/bin/env bash
# Launch Control - nudge once to close out the bound story before stopping.
# The guard records WHICH story was nudged, so it fires once per story and never loops.
set -uo pipefail
DIR="${CLAUDE_PROJECT_DIR:-$PWD}/.claude"
[ -f "$DIR/launch-control.json" ] || exit 0
[ -f "$DIR/.current-story" ] || exit 0

STORY="$(tr -d '[:space:]' < "$DIR/.current-story" 2>/dev/null || true)"
[ -n "$STORY" ] || exit 0

if [ -f "$DIR/.nudged" ]; then
  PREV="$(tr -d '[:space:]' < "$DIR/.nudged" 2>/dev/null || true)"
  [ "$PREV" = "$STORY" ] && exit 0
fi
printf '%s\n' "$STORY" > "$DIR/.nudged" 2>/dev/null || true

python3 - "$STORY" <<'PY' 2>/dev/null || exit 0
import json, sys
print(json.dumps({
    "decision": "block",
    "reason": (
        f"This session is still bound to Launch Control story {sys.argv[1]}, which is "
        f"marked In Progress in Notion.\n\n"
        f"If the work is finished, run /done - it will check the story's Done-when "
        f"criteria, tick it off, record what this session did, and unblock anything "
        f"waiting on it.\n\n"
        f"If the work is NOT finished, say so in one line and stop. Do not mark it Done. "
        f"This nudge fires only once per story, so stopping now is fine."
    ),
}))
PY
exit 0
