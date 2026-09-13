#!/bin/sh
# leakcheck.sh — run the test suite and fail if it leaves processes behind.
#
# GOV-B11. Two daemons on scratch sockets and two `go test` binaries, aged 3, 3, 20 and 28
# days, were found running on the dev machine while `governor doctor` reported ten checks
# all passing. They cost nothing and collided with nothing, which is why nothing noticed.
# This script is the thing that notices.
#
# WHERE THOSE FOUR ACTUALLY CAME FROM, because the story that commissioned this script
# assumed they were leaks from this suite and MEASUREMENT SAYS OTHERWISE. All four were
# checked by argv and working directory before anything was killed:
#
#   * the two `governor daemon --budget 4` processes were started BY HAND. No test in this
#     repo passes `--budget` — the flag appears nowhere in any .go file — and their working
#     directories were /tmp/gov28 and the repo root. They are the manual verification runs
#     recorded in docs/hook-surface.md, never cleaned up.
#   * the two `go-build/.../exe/{stub,server}` binaries belonged to a DIFFERENT PROJECT
#     entirely: both had working directories under ~/Repos/concertFinder, and one was still
#     parented to a live `go run stub.go`. Neither is a package in this repo.
#
# So this suite was not, on the day this was written, leaking anything: the checker below ran
# clean twice over `go test ./... -count=2`. The hardening that shipped alongside it is
# therefore defensive rather than corrective, and it is worth knowing which — the reaping bugs
# it fixed were found by reading the exit paths, not by catching them in the act. What this
# script guards is the NEXT one.
#
# It snapshots the process table, runs the suite, and diffs. A process is a leak if it is in
# the after set, not in the before set, and looks like something this repo's tests start:
#
#   * a `governor` binary, anywhere on disk, under any name a test build gives it
#   * a `go-build.../exe/*` binary, which is how `go test` runs a compiled test helper
#
# Usage:  tools/leakcheck.sh [go test arguments...]
# Default:  go test ./... -count=2
#
# Exit 0 means no new survivors. Exit 1 means the suite leaked, and the offenders are listed
# with the pid, elapsed time and full argv of each. A failing suite exits with the suite's
# own status, so a leak and a test failure are never confused for each other.

set -eu

# PATTERN matches what this repo's tests can start.
#
# IT IS TESTED AGAINST THE EXECUTABLE PATH ALONE, never against the whole command line, and
# that distinction was arrived at the hard way. Matching anywhere in the argv produced two
# false positives within minutes: the grep process running the pattern matched its own
# argument, and — less obvious and far worse — so did every shell whose command text merely
# MENTIONED this repo, because the checkout is at ~/Repos/claude-governor and the string is
# right there in the path. A checker whose output depends on what the directory is called is
# not a checker. argv[0] is the executable, and the executable is the question being asked.
PATTERN='governor|go-build[0-9]*/b[0-9]+/exe/'

# snapshot writes one "pid<TAB>command" line per matching process, sorted for comm(1).
#
# The sort is applied AFTER the key is built, not before. Sorting the raw ps output and then
# cutting a column out of it produces lines ordered by a key that is no longer the one comm
# compares — which is the classic way to get comm to silently produce nonsense.
#
# Elapsed time is deliberately NOT part of the key: it ticks upward between the two
# snapshots, so a surviving process would differ from itself and every long-running daemon on
# the machine would be reported as newly leaked. Identity is the pid plus the argv. A
# recycled pid running a different command is correctly seen as new; a recycled pid running
# the same command is the vanishingly rare case this accepts.
snapshot() {
	# A command shown in parentheses is one whose argv the kernel will no longer hand over —
	# a zombie, or a process already on its way out. It is not a leak: by definition nothing
	# is still running. Reporting them produced a "pid N age gone (governor)" line during this
	# script's own development, which is a false positive that teaches a reader to ignore the
	# output.
	ps -Ao pid=,command= 2>/dev/null |
		awk -v pat="$PATTERN" '$2 ~ pat && $2 !~ /^\(/ { pid = $1; $1 = ""; sub(/^ /, ""); print pid "\t" $0 }' |
		sort
}

before=$(mktemp -t govleak-before) || exit 1
after=$(mktemp -t govleak-after) || exit 1
trap 'rm -f "$before" "$after"' EXIT INT TERM

snapshot >"$before"

echo "leakcheck: running the suite" >&2
set +e
if [ "$#" -eq 0 ]; then
	go test ./... -count=2
else
	go test "$@"
fi
suite=$?
set -e

# Reaping is asynchronous in both directions: a daemon sent SIGTERM by a t.Cleanup takes a
# moment to close its listener and exit, and a process orphaned onto PID 1 never will. Wait
# out the first so it is not reported as the second.
sleep 2

snapshot >"$after"

leaked=$(comm -13 "$before" "$after" || true)

if [ -z "$leaked" ]; then
	if [ "$suite" -ne 0 ]; then
		echo "leakcheck: no processes leaked, but the suite itself failed ($suite)" >&2
		exit "$suite"
	fi
	echo "leakcheck: clean — no new governor or test-binary processes survived the run" >&2
	exit 0
fi

echo >&2
echo "leakcheck: the suite left these processes running:" >&2
echo "$leaked" | while IFS="$(printf '\t')" read -r pid cmd; do
	age=$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ')
	echo "  pid $pid  age ${age:-gone}  $cmd" >&2
done
echo >&2
echo "Every test that starts a daemon or execs a built binary must reap it on EVERY exit" >&2
echo "path, including t.Fatal and a panic. Use t.Cleanup, not defer, and kill the process" >&2
echo "GROUP (Setpgid + a negative-pid signal) where the child spawns children of its own." >&2
exit 1
