#!/bin/sh
# Runs INSIDE a clean container. Proves design.md section 13.2's install path and 13.3's
# byte-identical uninstall on a machine that has never seen this plugin (GOV-35).
#
# Nothing here knows how the artefact is served; it only knows GOVERNOR_BASE_URL, which is
# install.sh's own documented override. That keeps this script a test of the REAL script
# rather than of a copy of its logic.
#
# Expects, in the container:
#   /work/install.sh          the real script, mounted read-only (or INSTALL_SH, below)
#   GOVERNOR_BASE_URL         where the artefact is served from
#   CURL_CA_BUNDLE            the CA for that server, so --proto '=https' stays honest
#
# GOV-19 ALSO RUNS IT ON A CLEAN macOS RUNNER, against the PUBLISHED release, from release.yml.
# There it is given INSTALL_SH (install.sh as fetched from the tag) and NO GOVERNOR_BASE_URL, so
# install.sh downloads from releases/latest exactly as a stranger's would, and HOME is a fresh
# directory. On darwin it additionally refuses a thermally blind binary (see the end).
#
# POSIX sh: the image is deliberately minimal and may not have bash.

set -eu

fail() { echo "FAIL: $*" >&2; exit 1; }
note() { echo "--- $*"; }

# digest renders HOME as content-addressed lines: one per directory, one per file with its
# hash. Paths are relative to HOME and sorted, so the comparison is about what exists and what
# is in it - not about mtimes, which install has every right to move.
#
# NO `find -printf` HERE, DELIBERATELY. busybox find rejects it outright, and the first version
# of this script both used it AND swallowed the error with 2>/dev/null - so on alpine the
# directory half of the digest was silently EMPTY and a leftover directory would have compared
# equal. A byte-identical proof that can pass vacuously is worse than none, because it reports
# success. Everything below is POSIX and works under busybox and coreutils alike, and
# digestIsHonest() checks that it can still see a change.
digest() {
	find "$HOME" -mindepth 1 -type d | sed "s|^$HOME/|d |" | sort
	find "$HOME" -mindepth 1 -type f | sort | while IFS= read -r f; do
		printf 'f %s  %s\n' "$(sha256 "$f")" "${f#"$HOME"/}"
	done
}

# sha256 prints one file's digest. coreutils and busybox have sha256sum; macOS has shasum.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
	else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

INSTALL_SH="${INSTALL_SH:-/work/install.sh}"
[ -r "$INSTALL_SH" ] || fail "$INSTALL_SH is not readable; nothing to run."
if [ -z "${GOVERNOR_BASE_URL:-}" ]; then
	note "GOVERNOR_BASE_URL is unset: installing from the PUBLISHED release, as a stranger would"
fi

# ---------------------------------------------------------------------------------------
note "the machine is clean before anything runs"
# A container that already carried ~/.claude would make every assertion below meaningless,
# so it is checked rather than assumed. This is the one thing the developer's own laptop
# can never test, and the reason this job exists.
[ ! -e "$HOME/.claude" ] || fail "\$HOME/.claude exists before install; the image is not clean."
[ ! -e "$HOME/.local/bin/governor" ] || fail "a governor is already on PATH; the image is not clean."
before=$(digest)

# ---------------------------------------------------------------------------------------
note "install, through the real install.sh"
sh "$INSTALL_SH" || fail "install.sh exited non-zero."

# 13.3 is normative and GOV-35 asserts it against a real install. SIX files: GOV-17 clause 1
# added skills/heavy-build/SKILL.md, after its measurement window was closed in docs/advice.md.
plugin="$HOME/.claude/skills/build-governor"
for f in \
	"$plugin/.claude-plugin/plugin.json" \
	"$plugin/hooks/hooks.json" \
	"$plugin/.mcp.json" \
	"$plugin/commands/governor.md" \
	"$plugin/skills/heavy-build/SKILL.md" \
	"$plugin/bin/governor"
do
	[ -f "$f" ] || fail "install did not write $f, which design.md 13.3 lists."
done
# And nothing else under skills/: a second skill would be a path 13.3 does not list.
[ "$(find "$plugin/skills" -type f | wc -l)" -eq 1 ] || fail \
	"install wrote more under skills/ than skills/heavy-build/SKILL.md; design.md 13.3 must gain the row in the SAME change."

# A fresh container has no settings.json, so install CREATES one. That is the opposite of
# the developer's machine, where there is one to back up and merge, and it is the asymmetry
# no other test exercises: uninstall must REMOVE this file rather than restore a backup of
# something that never existed.
[ -f "$HOME/.claude/settings.json" ] || fail "install did not create settings.json on a machine that had none."
ls "$HOME"/.claude/settings.json.governor-backup-* >/dev/null 2>&1 \
	&& fail "install wrote a settings backup although there was no settings.json to back up."

# install does not conjure ~/.local/bin (13.3): absent means the convenience link is skipped.
[ ! -e "$HOME/.local/bin/governor" ] || fail \
	"install created ~/.local/bin/governor although ~/.local/bin did not exist."

# THE INSTRUMENT IS CHECKED BEFORE IT IS TRUSTED. A digest that returns the same thing whatever
# the disk holds would make the byte-identical assertion at the end pass for the wrong reason,
# and that is not hypothetical: see the note on `find -printf` above. install has just written
# five files, so the digest MUST now differ from the one taken before it.
installed_digest=$(digest)
[ "$installed_digest" != "$before" ] || fail \
	"the digest is unchanged after install wrote five files, so it cannot see the disk. The byte-identical check at the end would pass vacuously."

# ---------------------------------------------------------------------------------------
note "governor doctor, and what 'passes' means where there is no Claude Code"
gov="$plugin/bin/governor"
set +e
"$gov" doctor > /tmp/doctor.out 2>&1
doctor_rc=$?
set -e
cat /tmp/doctor.out

# Exit codes are a contract (doctor.go): 0 all clear, 1 something FAILED, 2 usage,
# 3 nothing failed but something could not verify. In a container `spawn` can never verify —
# no subagent has ever run — so 3 is the expected success here and 0 would be a surprise.
case "$doctor_rc" in
0 | 3) ;;
1) fail "doctor reported a FAILED check on a fresh install. See the output above." ;;
2) fail "doctor exited 2 (usage error): this script called it wrongly." ;;
*) fail "doctor exited $doctor_rc, which its own contract does not define." ;;
esac

# GOV-30 exists because doctor once said ok for a check that could not verify. So it is not
# enough that nothing FAILED: the checks that MUST be ok are named, and the set that may be
# unverified is named EXACTLY. A count would let a new unknown in unnoticed, which is the
# same hole GOV-30 closed.
for check in daemon socket hooks matcher protocol binary config budget leases strays; do
	grep -Eq "^ok +${check}( |$)" /tmp/doctor.out \
		|| fail "doctor did not report 'ok $check' on a fresh install."
done

# calibration is ok, or it is exactly ONE warn: GOV-47's "b_max was not measured". install's
# calibrate skips the b_max probe when the machine is already loaded, and right after install.sh
# on a CI runner it often is, so whether the probe runs depends on the runner, not the install.
# v0.1.3's clean-macos failed on exactly this, on a 3-core runner, with everything else sound.
# Any other calibration line (a failure, an unknown, a different warn) is still a finding.
grep -Eq "^ok +calibration( |$)" /tmp/doctor.out \
	|| grep -Eq "^warn +calibration .*b_max was not measured" /tmp/doctor.out \
	|| fail "doctor reported neither 'ok calibration' nor the one accepted warn (b_max was not measured) on a fresh install."

# EXACTLY TWO checks cannot verify on a machine with no Claude Code, and each is a fact about
# the container rather than about the install:
#
#   spawn    — no subagent has ever run here, so nothing confirms the PreToolUse matcher fires
#              on this Claude Code version. There is no Claude Code version at all.
#   sessions — the session registry is populated by SessionStart hooks firing in real
#              sessions. A container has none, so the daemon cannot say which are live.
#
# Anything else unverified is a finding. Do not widen this set to make a run green: an
# unverified check is doctor saying it could not see, and the whole point of GOV-30 is that
# "could not see" is never quietly counted as "fine".
expected_unknown="sessions spawn"
actual_unknown=$(awk '/^unknown/ {print $2}' /tmp/doctor.out | sort | tr '\n' ' ' | sed 's/ $//')
if [ "$actual_unknown" != "$expected_unknown" ]; then
	fail "doctor's unverified checks are [$actual_unknown]; a clean container expects exactly [$expected_unknown]. A new unknown is a finding, and widening this set would re-open GOV-30."
fi

# ---------------------------------------------------------------------------------------
note "reinstall over a RUNNING install (design.md 13.4: rename(2), never overwrite)"
# The case the clean-machine test deliberately does not cover. Writing onto a running binary
# is what GOV-02 measured as fatal on macOS; the hazard is placement, not the platform, so
# the ordering is asserted wherever the job can run.
"$gov" status >/dev/null 2>&1 || fail "the daemon does not answer after install."
sh "$INSTALL_SH" || fail "the SECOND install.sh exited non-zero (reinstall over a running install)."
rc=0
"$gov" status >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 0 ] || fail \
	"the daemon does not answer after a reinstall (status exited $rc). Exit 137 here is the SIGKILL of design.md 13.4: the binary was overwritten in place rather than renamed."

# ---------------------------------------------------------------------------------------
if [ "$(uname -s)" = "Darwin" ]; then
	note "the darwin binary can see thermal state (GOV-13: built with cgo, on macOS)"
	# A darwin binary built without cgo runs perfectly and reports thermal "unknown", which the
	# control loop treats as nominal — so a throttling Mac looks idle and the budget never floors.
	# Nothing else in this script, in install, or in doctor would notice. This line is the
	# acceptance check GOV-19's traps name for exactly that artefact.
	thermal=$("$gov" status 2>/dev/null | awk '$1 == "thermal" {print $2}')
	[ -n "$thermal" ] || fail "governor status printed no thermal line."
	[ "$thermal" != "unknown" ] || fail \
		"governor status reports 'thermal unknown' on macOS: this release's darwin binary was built without cgo and is thermally blind."
fi

# ---------------------------------------------------------------------------------------
note "uninstall leaves the machine byte-identical"
"$gov" uninstall || fail "uninstall exited non-zero."
after=$(digest)

if [ "$before" != "$after" ]; then
	echo "the container is NOT byte-identical after uninstall:" >&2
	printf '%s\n' "$before" > /tmp/before.txt
	printf '%s\n' "$after" > /tmp/after.txt
	diff /tmp/before.txt /tmp/after.txt >&2 || true
	fail "uninstall left residue, or removed something it did not install. design.md 13.3 is the list it must match."
fi

note "clean install, doctor, reinstall and byte-identical uninstall all pass"
