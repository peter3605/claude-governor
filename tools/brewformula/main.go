// Command brewformula writes the Homebrew formula for one release, from that release's own
// checksums.txt.
//
// It runs in release.yml after the four binaries are built, and its output is attached to the
// release as governor.rb, which is what the tap carries. Generating it there rather than editing
// it by hand is the whole point: GOV-19's Done-when asks that the tap and the marketplace install
// the SAME version, and a formula whose version and digests are typed in separately is one release
// away from naming a binary that no longer matches its checksum — which brew reports as a
// download failure, on a stranger's machine, after the release has shipped.
//
// The marketplace needs no equivalent. Its entry is a command source, so it carries no version of
// its own: it loads whatever tree `governor install` wrote, from whatever binary this formula
// installed (design.md section 13.2a).
//
// Usage: brewformula -version 0.1.0 -checksums dist/checksums.txt > governor.rb
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/template"
)

// repo is where the release lives. The formula downloads from its releases page, so it has to
// be public for anyone but its owner to install from it.
const repo = "peter3605/claude-governor"

// targets are the four release artefacts, in the order the formula lists them. Each must appear
// in checksums.txt or the formula is refused: a platform block with no digest would install
// whatever the URL returns.
var targets = []struct{ os, arch, brewOS, brewArch string }{
	{"darwin", "arm64", "on_macos", "on_arm"},
	{"darwin", "amd64", "on_macos", "on_intel"},
	{"linux", "arm64", "on_linux", "on_arm"},
	{"linux", "amd64", "on_linux", "on_intel"},
}

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

func main() {
	version := flag.String("version", "", "release version, without the leading v")
	checksums := flag.String("checksums", "", "path to the release's checksums.txt")
	flag.Parse()
	f, err := os.Open(*checksums)
	if err != nil {
		fmt.Fprintf(os.Stderr, "brewformula: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	if err := write(os.Stdout, *version, f); err != nil {
		fmt.Fprintf(os.Stderr, "brewformula: %v\n", err)
		os.Exit(1)
	}
}

// parseChecksums reads sha256sum's output: "<64 hex>  <name>" per line.
func parseChecksums(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("not a sha256sum line: %q", sc.Text())
		}
		sums[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	return sums, sc.Err()
}

type platform struct {
	BrewOS, BrewArch, Asset, SHA256 string
}

// write renders the formula for version from the digests in checksums.
func write(w io.Writer, version string, checksums io.Reader) error {
	if !semver.MatchString(version) {
		return fmt.Errorf("version %q is not MAJOR.MINOR.PATCH (pass it without the leading v)", version)
	}
	sums, err := parseChecksums(checksums)
	if err != nil {
		return err
	}
	var plats []platform
	for _, t := range targets {
		asset := "governor-" + t.os + "-" + t.arch
		sum, ok := sums[asset]
		if !ok {
			return fmt.Errorf("checksums.txt has no entry for %s", asset)
		}
		plats = append(plats, platform{t.brewOS, t.brewArch, asset, sum})
	}
	return formula.Execute(w, struct {
		Repo, Version string
		Mac, Linux    []platform
	}{repo, version, plats[:2], plats[2:]})
}

// The caveat carries the step brew cannot do for the user. Installing the formula puts a binary
// on PATH and governs nothing: the hooks, the status line and the daemon are `governor install`,
// which merges into the user's settings and so is not something a package manager should do
// unasked.
var formula = template.Must(template.New("formula").Parse(`class Governor < Formula
  desc "Machine-wide build and subagent budget for Claude Code sessions"
  homepage "https://github.com/{{.Repo}}"
  version "{{.Version}}"
  license "MIT"

  on_macos do
{{- range .Mac}}
    {{.BrewArch}} do
      url "https://github.com/{{$.Repo}}/releases/download/v{{$.Version}}/{{.Asset}}"
      sha256 "{{.SHA256}}"
    end
{{- end}}
  end

  on_linux do
{{- range .Linux}}
    {{.BrewArch}} do
      url "https://github.com/{{$.Repo}}/releases/download/v{{$.Version}}/{{.Asset}}"
      sha256 "{{.SHA256}}"
    end
{{- end}}
  end

  def install
    bin.install Dir["governor-*"].first => "governor"
  end

  def caveats
    <<~EOS
      This installs the binary only. To govern Claude Code sessions, run:
        governor install
      It writes a plugin to ~/.claude/skills/build-governor/, merges a statusLine key into
      ~/.claude/settings.json (after backing it up), and starts the daemon.
      Undo all of it with: governor uninstall
    EOS
  end

  test do
    assert_match "governor #{version}", shell_output("#{bin}/governor version")
  end
end
`))
