#!/usr/bin/env bash
# Proves the install on a genuinely clean machine (GOV-35). Runs on the HOST; the assertions
# run inside a container, in scripts/clean-install-in-container.sh.
#
#   scripts/clean-install-test.sh [image]
#
# It is a script rather than inline workflow YAML so that it can be run locally against the
# working tree, which is where an install bug is cheapest to find. CI calls it with no
# arguments.
#
# WHY A CONTAINER AND NOT ubuntu-latest. A GitHub runner carries a Go toolchain, a checkout
# and a warm HOME. The bug class this is looking for lives in the difference between that and
# a stranger's machine: a stale binary on PATH, an existing settings.json, a daemon already
# running. Only a clean image has none of them.
#
# WHY HTTPS AND NOT A LOCAL FILE. install.sh fetches with `curl --proto '=https' --tlsv1.2`,
# and that restriction is a property worth keeping rather than working around, so the artefact
# is served over TLS with this script's own certificate trusted through CURL_CA_BUNDLE — the
# same approach cmd/governor/installsh_test.go takes for the same reason. A plain http:// URL
# would be refused by curl, and "relax the flag for the test" would be testing a script the
# user does not run.

set -euo pipefail

IMAGE="${1:-debian:stable-slim}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
SERVER_PID=""

cleanup() {
	[ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

command -v docker >/dev/null 2>&1 || { echo "clean-install-test: docker is required." >&2; exit 1; }

echo "--- building both linux artefacts from this working tree"
# BOTH architectures, because install.sh detects the CONTAINER's arch and the container's arch
# is the host's: an arm64 developer machine runs arm64 images, a CI runner runs amd64. Building
# only one is a test that passes on whoever wrote it. Serving both also matches the shape of a
# real release, which carries all four.
#
# CGO_ENABLED=0 matches the shipped linux targets, which is also what keeps the binary static
# enough to run on a musl image as well as a glibc one.
mkdir -p "$WORK/releases/latest/download"
for arch in amd64 arm64; do
	( cd "$ROOT" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
		go build -o "$WORK/releases/latest/download/governor-linux-$arch" ./cmd/governor )
done

echo "--- generating a certificate for the local release server"
openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes \
	-keyout "$WORK/key.pem" -out "$WORK/cert.pem" \
	-subj "/CN=host.docker.internal" \
	-addext "subjectAltName=DNS:host.docker.internal,DNS:localhost,IP:127.0.0.1" \
	>/dev/null 2>&1

echo "--- serving the artefact over HTTPS"
# Python is on every GitHub runner and on most developer machines; it serves the release
# layout install.sh expects ($BASE_URL/latest/download/<artefact>) and nothing else.
python3 - "$WORK" <<'PY' &
import http.server, functools, ssl, sys, pathlib
root = pathlib.Path(sys.argv[1])
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(root))
# 0.0.0.0, not 127.0.0.1: the container reaches this through the docker gateway rather
# than through the host's loopback, which a VM-backed Docker (macOS) does not share. It
# serves one binary and a certificate, for the few seconds this script runs.
httpd = http.server.HTTPServer(("0.0.0.0", 8443), handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(root / "cert.pem", root / "key.pem")
httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
httpd.serve_forever()
PY
SERVER_PID=$!

# Wait for the port rather than sleeping a guess at it.
for _ in $(seq 1 50); do
	if curl -fsS --cacert "$WORK/cert.pem" \
		https://127.0.0.1:8443/releases/latest/download/governor-linux-amd64 -o /dev/null 2>/dev/null; then
		break
	fi
	sleep 0.2
done

HOSTNAME_FOR_CONTAINER=host.docker.internal
BASE_URL="https://$HOSTNAME_FOR_CONTAINER:8443/releases"
# --add-host maps that name to the host on Linux; Docker Desktop provides it already. This is
# used INSTEAD of --network host, which looks simpler and silently does not work on macOS,
# where the daemon runs in a VM and the container's localhost is not the host's.
DOCKER_NET=(--add-host "$HOSTNAME_FOR_CONTAINER:host-gateway")

echo "--- running the install in a clean $IMAGE container"
# A slim image has neither curl nor wget, and install.sh correctly dies saying so. Installing
# curl here is deliberate: this job tests the HAPPY path on a machine that has a fetcher, which
# is what a stranger has. The no-fetcher diagnosis is a separate, cheaper assertion below.
docker run --rm "${DOCKER_NET[@]}" \
	-v "$ROOT/install.sh:/work/install.sh:ro" \
	-v "$ROOT/scripts/clean-install-in-container.sh:/work/run.sh:ro" \
	-v "$WORK/cert.pem:/work/ca.pem:ro" \
	-e GOVERNOR_BASE_URL="$BASE_URL" \
	-e CURL_CA_BUNDLE=/work/ca.pem \
	"$IMAGE" \
	sh -c 'set -e
		export DEBIAN_FRONTEND=noninteractive
		if command -v apt-get >/dev/null 2>&1; then
			apt-get -qq update && apt-get -qq install -y curl ca-certificates >/dev/null
		elif command -v apk >/dev/null 2>&1; then
			apk add --quiet curl ca-certificates
		fi
		sh /work/run.sh'

# ---------------------------------------------------------------------------------------
# The no-fetcher diagnosis: correct behaviour, not a bug. A machine with neither curl nor wget
# cannot download, and install.sh says which rather than failing three steps later.
#
# ONLY ASSERTABLE ON AN IMAGE THAT ACTUALLY LACKS BOTH. busybox builds wget IN, so on alpine the
# branch is unreachable and asking for it produces a TLS error from wget instead - which is a
# different thing entirely and was mistaken for a failure the first time this ran.
if docker run --rm "$IMAGE" sh -c 'command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1'; then
	echo "--- skipping the no-fetcher diagnosis: $IMAGE ships a fetcher, so the branch is unreachable"
else
	echo "--- asserting the no-fetcher diagnosis, which is install.sh's own error path"
	out=$(docker run --rm "${DOCKER_NET[@]}" \
		-v "$ROOT/install.sh:/work/install.sh:ro" \
		-e GOVERNOR_BASE_URL="$BASE_URL" \
		"$IMAGE" sh /work/install.sh 2>&1 || true)
	case "$out" in
	*"neither curl nor wget is installed"*) ;;
	*) echo "clean-install-test: expected the no-fetcher diagnosis, got:" >&2
	   echo "$out" >&2; exit 1 ;;
	esac
fi

echo
echo "clean-install-test: PASS on $IMAGE"
