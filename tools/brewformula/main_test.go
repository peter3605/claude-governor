package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checksumsFor fakes a release's checksums.txt, one distinct digest per artefact so that a
// formula pairing a URL with the wrong digest is caught.
func checksumsFor(assets ...string) string {
	var b strings.Builder
	for i, a := range assets {
		fmt.Fprintf(&b, "%064x  %s\n", i+1, a)
	}
	return b.String()
}

var allAssets = []string{"governor-darwin-arm64", "governor-darwin-amd64", "governor-linux-arm64", "governor-linux-amd64"}

func TestEveryPlatformGetsItsOwnURLAndDigest(t *testing.T) {
	var out strings.Builder
	if err := write(&out, "0.1.0", strings.NewReader(checksumsFor(allAssets...))); err != nil {
		t.Fatal(err)
	}
	f := out.String()
	for i, a := range allAssets {
		url := "https://github.com/" + repo + "/releases/download/v0.1.0/" + a
		sum := fmt.Sprintf("%064x", i+1)
		at := strings.Index(f, url)
		if at < 0 {
			t.Fatalf("formula has no URL for %s:\n%s", a, f)
		}
		// The digest must be the one that follows ITS url, not merely present somewhere.
		rest := f[at+len(url):]
		if next := strings.Index(rest, `sha256 "`); next < 0 || !strings.HasPrefix(rest[next+len(`sha256 "`):], sum) {
			t.Errorf("%s is not followed by its own digest %s", a, sum)
		}
	}
	// brew reads the version from the URL, and `brew audit --strict` fails a formula that also
	// states it. v0.1.0 shipped with one and failed its release check on exactly that line.
	if strings.Contains(f, "\n  version ") {
		t.Errorf("formula states a version; brew scans it from the URL and audit rejects it as redundant:\n%s", f)
	}
	for _, want := range []string{`license "MIT"`, "governor install", "governor uninstall"} {
		if !strings.Contains(f, want) {
			t.Errorf("formula does not contain %q", want)
		}
	}
}

// brew scans "64" out of a pre-release URL (from the arch, not the tag), so the formula's
// `brew test` would compare "governor 0.1.3-rc.1" against "governor 64" and fail, as it did on the
// v0.1.3-rc.1 release run. A pre-release formula therefore states its version, exactly once,
// and a stable one still does not (the assertion above).
func TestAPrereleaseFormulaStatesItsVersion(t *testing.T) {
	var out strings.Builder
	if err := write(&out, "0.1.3-rc.1", strings.NewReader(checksumsFor(allAssets...))); err != nil {
		t.Fatal(err)
	}
	f := out.String()
	if n := strings.Count(f, "\n  version "); n != 1 {
		t.Fatalf("want exactly one version line, got %d:\n%s", n, f)
	}
	if !strings.Contains(f, "\n  version \"0.1.3-rc.1\"\n") {
		t.Errorf("version line is not version \"0.1.3-rc.1\":\n%s", f)
	}
	if !strings.Contains(f, "/releases/download/v0.1.3-rc.1/governor-darwin-arm64") {
		t.Errorf("URLs do not carry the pre-release tag:\n%s", f)
	}
}

func TestAMissingArtefactRefusesTheFormula(t *testing.T) {
	err := write(&strings.Builder{}, "0.1.0", strings.NewReader(checksumsFor(allAssets[:3]...)))
	if err == nil || !strings.Contains(err.Error(), "governor-linux-amd64") {
		t.Fatalf("want a refusal naming the missing artefact, got %v", err)
	}
}

// The tag is v0.1.0 and the version is 0.1.0. Passing the tag would make every URL
// .../download/vv0.1.0/..., which 404s — so it is refused rather than rendered.
func TestTheTagIsNotAVersion(t *testing.T) {
	if err := write(&strings.Builder{}, "v0.1.0", strings.NewReader(checksumsFor(allAssets...))); err == nil {
		t.Fatal("a version with a leading v was accepted")
	}
}

// TestTheFormulaIsRuby checks the rendered file parses, when a Ruby is available. brew itself is
// not required here; release.yml runs `brew style` and `brew audit` against the real file.
func TestTheFormulaIsRuby(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("no ruby on PATH")
	}
	var out strings.Builder
	if err := write(&out, "0.1.0", strings.NewReader(checksumsFor(allAssets...))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "governor.rb")
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(ruby, "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("ruby -c: %v\n%s", err, b)
	}
}
