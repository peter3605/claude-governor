#!/bin/sh
# claude-governor bootstrap (design.md section 13.2).
#
#   curl -fsSL https://raw.githubusercontent.com/peter3605/claude-governor/main/install.sh | sh
#
# Detects the OS and architecture, fetches the matching release binary, and hands off to
# `governor install`, which does everything that touches the machine. This script deliberately
# knows almost nothing: it downloads one file and runs it. Every decision about what to write
# and how to undo it lives in the binary, where it is tested — a shell script that also
# edited settings.json would be a second implementation of the one thing that must not be got
# wrong, and it would not be covered by the byte-identical uninstall test.
#
# POSIX sh, not bash: this is the first thing a stranger runs, and /bin/sh is what every
# target has.
#
# Remove everything it installs with:  governor uninstall

set -eu

REPO="${GOVERNOR_REPO:-peter3605/claude-governor}"
VERSION="${GOVERNOR_VERSION:-latest}"
# Where releases are served from. Overridable for a mirror, a private fork, or a test that
# needs to exercise this script without publishing a release to do it.
BASE_URL="${GOVERNOR_BASE_URL:-https://github.com/$REPO/releases}"

die() {
	echo "install: $*" >&2
	exit 1
}

# detect names the release artefact for this host.
#
# The four targets of design.md section 13.5, and nothing else. An unrecognised host is an
# error rather than a guess: downloading the wrong architecture produces "cannot execute
# binary file" three steps later, which reads like a corrupt download rather than an
# unsupported platform.
detect() {
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported operating system: $os. claude-governor supports macOS and Linux." ;;
	esac
	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch. claude-governor supports arm64 and amd64." ;;
	esac
	echo "governor-$os-$arch"
}

fetch() {
	url=$1
	out=$2
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https' --tlsv1.2 -o "$out" "$url"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$out" "$url"
	else
		die "neither curl nor wget is installed, so there is nothing to download with."
	fi
}

main() {
	artefact=$(detect)

	if [ "$VERSION" = latest ]; then
		url="$BASE_URL/latest/download/$artefact"
	else
		url="$BASE_URL/download/$VERSION/$artefact"
	fi

	# A directory of our own, removed on the way out however this exits. The binary is NOT
	# downloaded over any existing install: `governor install` is what puts it in place, and
	# it renames rather than overwrites for the reason design.md section 13.4 measured —
	# writing onto a running binary on macOS invalidates its code signature and the kernel
	# SIGKILLs every later execution.
	tmp=$(mktemp -d) || die "cannot create a temporary directory."
	trap 'rm -rf "$tmp"' EXIT INT TERM

	echo "Downloading $artefact"
	fetch "$url" "$tmp/governor" || die "download failed: $url"
	[ -s "$tmp/governor" ] || die "downloaded an empty file from $url"
	chmod +x "$tmp/governor"

	# Prove it runs before it is asked to install anything. A wrong architecture or a
	# truncated download fails here, with one line about the download, rather than halfway
	# through writing files.
	"$tmp/governor" version >/dev/null 2>&1 || die "the downloaded binary does not run on this machine."

	echo
	exec "$tmp/governor" install "$@"
}

main "$@"
