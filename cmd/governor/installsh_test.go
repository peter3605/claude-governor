package main

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for install.sh (GOV-21, design.md section 13.2).
//
// The bootstrap is the first thing a stranger runs, and it is the one piece of this story
// that is not Go. Its two jobs are to name the right artefact for the host and to hand off
// to `governor install` — and the failure mode of getting the first wrong is a download that
// succeeds and a binary that cannot execute, reported three steps later as something that
// looks like a corrupt file rather than an unsupported platform.
//
// `uname` and `curl` are stubbed on PATH rather than mocked in the script, so what runs is
// the shipped file with nothing conditional compiled into it for testing.

// stubPATH builds a directory of stub executables and returns a PATH with it in front.
func stubPATH(t *testing.T, stubs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range stubs {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// runInstallSh runs the real install.sh with a stubbed uname and a served artefact.
//
// The artefact is served over HTTPS by the test rather than stubbed into curl, so the
// script's actual download path runs — including its "did we get an empty file" check.
func runInstallSh(t *testing.T, unameS, unameM, script string) (int, string, string) {
	t.Helper()

	// The served "binary" is a shell script that records the arguments it was handed. That
	// is the whole assertion for the handoff: install.sh must exec the downloaded file with
	// `install`, and must have proved it runs first.
	served := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, script)
	}))
	t.Cleanup(served.Close)

	// TLS, and with the test's own certificate trusted rather than with verification turned
	// off. install.sh passes `--proto '=https'` to curl, and that is a property worth
	// keeping: it is the flag that stops a redirect or a mangled base URL downgrading the
	// download of an executable to plaintext. A test that served plain HTTP would have to
	// weaken the script to pass, which is the tail wagging the dog.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	block, err := certPEM(served.Certificate().Raw)
	if err != nil {
		t.Fatalf("encode test certificate: %v", err)
	}
	if err := os.WriteFile(caFile, block, 0o600); err != nil {
		t.Fatalf("write test certificate: %v", err)
	}

	cmd := exec.Command("sh", filepath.Join("..", "..", "install.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+stubPATH(t, map[string]string{
			"uname": fmt.Sprintf(`case "$1" in -s) echo %q ;; -m) echo %q ;; esac`, unameS, unameM),
		}),
		"GOVERNOR_BASE_URL="+served.URL,
		"HOME="+t.TempDir(),
		"CURL_CA_BUNDLE="+caFile,
		"SSL_CERT_FILE="+caFile,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run install.sh: %v", err)
	}
	return code, stdout.String(), stderr.String()
}

// certPEM renders a DER certificate as the PEM file curl and wget want.
func certPEM(der []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// recordingGovernor is a stand-in for the release binary. It answers `version` so the
// script's "does it run" check passes, and echoes whatever else it is given.
const recordingGovernor = `#!/bin/sh
case "$1" in
version) echo "governor 0.0.0-stub"; exit 0 ;;
esac
echo "HANDOFF: $*"
`

// install.sh names the right artefact for each of the four supported targets.
//
// design.md section 13.5: macOS and Linux, arm64 and amd64. The aliases matter as much as
// the canonical names — `uname -m` says x86_64 on most Linux distributions and aarch64 on
// several arm ones, and a table that only knew amd64 and arm64 would reject the majority of
// real Linux machines.
func TestInstallShNamesTheRightArtefact(t *testing.T) {
	for _, tc := range []struct{ unameS, unameM, want string }{
		{"Darwin", "arm64", "governor-darwin-arm64"},
		{"Darwin", "x86_64", "governor-darwin-amd64"},
		{"Linux", "x86_64", "governor-linux-amd64"},
		{"Linux", "amd64", "governor-linux-amd64"},
		{"Linux", "aarch64", "governor-linux-arm64"},
		{"Linux", "arm64", "governor-linux-arm64"},
	} {
		t.Run(tc.unameS+"/"+tc.unameM, func(t *testing.T) {
			code, stdout, stderr := runInstallSh(t, tc.unameS, tc.unameM, recordingGovernor)
			if code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("did not download %s:\n%s", tc.want, stdout)
			}
		})
	}
}

// install.sh hands off to `governor install` and does nothing to the machine itself.
//
// The division of labour is the point: everything that touches settings.json or writes a
// plugin lives in the binary, where the byte-identical uninstall test covers it. A shell
// script that also edited the user's configuration would be a second implementation of the
// one thing that must not go wrong, and an untested one.
func TestInstallShHandsOffToGovernorInstall(t *testing.T) {
	code, stdout, stderr := runInstallSh(t, "Darwin", "arm64", recordingGovernor)
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "HANDOFF: install") {
		t.Errorf("install.sh did not run `governor install`:\n%s", stdout)
	}
}

// An unsupported host is named, not guessed at.
func TestInstallShRefusesAnUnsupportedHost(t *testing.T) {
	for _, tc := range []struct{ unameS, unameM, want string }{
		{"MINGW64_NT-10.0", "x86_64", "unsupported operating system"},
		{"Linux", "riscv64", "unsupported architecture"},
	} {
		t.Run(tc.unameS+"/"+tc.unameM, func(t *testing.T) {
			code, _, stderr := runInstallSh(t, tc.unameS, tc.unameM, recordingGovernor)
			if code == 0 {
				t.Error("exit 0 on an unsupported host")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not say %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// A binary that does not run on this machine is caught before anything is installed.
//
// This is what turns "wrong architecture" into one line about the download instead of a
// half-finished install. The stub here exits non-zero for `version`, which is what a binary
// for another architecture does — modulo the shell's own message about it.
func TestInstallShChecksTheBinaryRunsFirst(t *testing.T) {
	code, stdout, stderr := runInstallSh(t, "Darwin", "arm64", "#!/bin/sh\nexit 1\n")
	if code == 0 {
		t.Error("exit 0 with a binary that does not run")
	}
	if !strings.Contains(stderr, "does not run on this machine") {
		t.Errorf("stderr does not name the problem:\n%s", stderr)
	}
	if strings.Contains(stdout, "HANDOFF") {
		t.Error("install.sh handed off to a binary it could not run")
	}
}
