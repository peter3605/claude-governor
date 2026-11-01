package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for GOV-45: install's ~/.local/bin link must never shadow a governor that is
// already on PATH.
//
// THE CASE THAT FOUND IT. The real v0.1.2 upgrade on 2026-10-31 (design.md section 13.4):
// `brew upgrade` replaced /opt/homebrew/bin/governor, brew warned it was shadowed by
// ~/.local/bin/governor, and that path was install's own link to the hooked 0.1.1 binary. Bare
// `governor install` therefore ran 0.1.1, reported every path unchanged, and left the 0.1.1
// daemon. Nothing failed; the upgrade just did not happen.

// pathWithoutGovernor drops every PATH entry that holds a `governor`, so a staged home starts
// from a machine with no governor installed whatever the developer's own machine has.
func pathWithoutGovernor(path string) string {
	var keep []string
	for _, dir := range filepath.SplitList(path) {
		if _, err := os.Stat(filepath.Join(dir, "governor")); err == nil {
			continue
		}
		keep = append(keep, dir)
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// onPath stages a directory holding an executable `governor` and puts it on PATH, AFTER
// ~/.local/bin. That order is the one that made the link shadow brew on the real machine:
// ~/.local/bin earlier, /opt/homebrew/bin later.
//
// target is what `governor` is: a symlink to it, the way brew links /opt/homebrew/bin into
// the Cellar. "" stages a stand-in script, enough for the in-process tests that only need
// something to be there.
func onPath(t *testing.T, p paths, target string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "brew-bin")
	mkdirAll(t, dir)
	bin := filepath.Join(dir, "governor")
	if target == "" {
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho brew governor\n"), 0o755); err != nil {
			t.Fatalf("stage %s: %v", bin, err)
		}
	} else if err := os.Symlink(target, bin); err != nil {
		t.Fatalf("stage %s: %v", bin, err)
	}
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(p.link), dir, os.Getenv("PATH")},
		string(os.PathListSeparator)))
	return bin
}

// The install.sh route, which is the one the link exists for: the binary running install is
// in a temp directory and nothing else is on PATH, so the link is what puts `governor` there.
func TestInstallLinksGovernorOntoPathWhenNothingElseIs(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.link))

	out := installOK(t)
	target, err := os.Readlink(p.link)
	if err != nil {
		t.Fatalf("install wrote no link at %s: %v\n%s", p.link, err, out)
	}
	if target != p.binary {
		t.Errorf("link points at %s, want the hooked binary %s", target, p.binary)
	}
}

// The brew route: `governor` is already on PATH, so the link would only shadow it.
func TestInstallDoesNotShadowAGovernorAlreadyOnPath(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.link))
	brew := onPath(t, p, "")

	out := installOK(t)
	if _, err := os.Lstat(p.link); err == nil {
		t.Fatalf("install wrote %s although %s is already on PATH\n%s", p.link, brew, out)
	}
	if !strings.Contains(out, brew) {
		t.Errorf("install did not say which governor is already on PATH (%s):\n%s", brew, out)
	}
	// The dry run reaches the same decision, or --dry-run is describing a different install.
	_, dry, _ := governor(t, "install", "--dry-run", "--no-daemon")
	if strings.Contains(dry, "→ "+p.binary) || !strings.Contains(dry, brew) {
		t.Errorf("--dry-run disagrees with install about the link:\n%s", dry)
	}
}

// A machine that already has the shadowing link, from an install before this change, has it
// retired by the next brew-route install. Then uninstall still leaves the machine as it was
// before ANY install: the link was ours, so it is not a file a user had.
func TestABrewInstallRetiresTheLinkThatShadowedIt(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.link))
	before := snapshot(t, p.home)

	installOK(t) // the old shape: nothing else on PATH, so the link is written
	if target, err := os.Readlink(p.link); err != nil || target != p.binary {
		t.Fatalf("staging: no link to the hooked binary (%q, %v)", target, err)
	}

	brew := onPath(t, p, "")
	if _, dry, _ := governor(t, "install", "--dry-run", "--no-daemon"); !strings.Contains(dry, "removed") {
		t.Errorf("--dry-run does not say the link would be removed:\n%s", dry)
	}
	if _, err := os.Lstat(p.link); err != nil {
		t.Fatalf("--dry-run removed the link: %v", err)
	}

	out := installOK(t)
	if _, err := os.Lstat(p.link); err == nil {
		t.Fatalf("install left the link that shadows %s\n%s", brew, out)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("install did not report removing the link:\n%s", out)
	}

	uninstallOK(t)
	if diff := diffSnapshots(before, snapshot(t, p.home)); len(diff) > 0 {
		t.Errorf("uninstall did not leave %s byte-identical:\n  %s", p.home, strings.Join(diff, "\n  "))
	}
}

// Somebody else's file at the link's name is left alone on the brew route as well: retiring
// is for the link install wrote, not for whatever happens to be called governor.
func TestABrewInstallLeavesSomebodyElsesLinkAlone(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.link))
	theirs := filepath.Join(t.TempDir(), "governor")
	writeFileT(t, theirs, "#!/bin/sh\necho theirs\n")
	if err := os.Symlink(theirs, p.link); err != nil {
		t.Fatal(err)
	}
	onPath(t, p, "")

	installOK(t)
	uninstallOK(t)
	if target, err := os.Readlink(p.link); err != nil || target != theirs {
		t.Errorf("their link at %s became %q (%v), want it untouched -> %s", p.link, target, err, theirs)
	}
}

// TestABrewUpgradeIsActuallyInstalled is Done-when clause 1, the v0.1.2 upgrade as it went,
// end to end with real binaries and a real daemon.
//
// brew bin on PATH with ~/.local/bin earlier. `<brew bin>/governor install`; brew upgrade
// (the brew symlink now points at a different build); then BARE `governor install`, resolved
// through PATH exactly as a shell would. It must end with the new build hooked and the new
// build serving.
func TestABrewUpgradeIsActuallyInstalled(t *testing.T) {
	p := stagedHome(t)
	mkdirAll(t, filepath.Dir(p.link))

	// Cellar copies, so the brew symlink can be repointed without touching the shared builds.
	cellar := t.TempDir()
	oldBuild := filepath.Join(cellar, "0.1.1", "governor")
	newBuild := filepath.Join(cellar, "0.1.2", "governor")
	copyExecutable(t, realGovernor(t), oldBuild)
	copyExecutable(t, governorRebuilt(t), newBuild)
	brew := onPath(t, p, oldBuild)

	if code, out, errOut := runRealBinary(t, brew, p, "install"); code != 0 {
		t.Fatalf("%s install: exit %d\n%s\n%s", brew, code, out, errOut)
	}
	t.Cleanup(func() { runRealBinary(t, newBuild, p, "uninstall", "--purge") })

	// brew upgrade: the same name, now linked to the new Cellar entry.
	if err := os.Remove(brew); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newBuild, brew); err != nil {
		t.Fatal(err)
	}

	bare, err := exec.LookPath("governor")
	if err != nil {
		t.Fatalf("bare governor does not resolve on PATH: %v", err)
	}
	if bare != brew {
		t.Fatalf("bare governor resolves to %s, not the brew binary %s: the link shadows it", bare, brew)
	}
	code, out, errOut := runRealBinary(t, bare, p, "install")
	if code != 0 {
		t.Fatalf("bare governor install: exit %d\n%s\n%s", code, out, errOut)
	}

	want, err := digest(newBuild)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := digest(p.binary); err != nil || got != want {
		t.Errorf("the hooked binary is %s (%v), want the new build %s\n%s", shortBuild(got), err, shortBuild(want), out)
	}
	endpoint := "unix://" + filepath.Join(p.state, "sock")
	if got := daemonBuild(t, endpoint); got != want {
		t.Errorf("the daemon reports build %s, want the new build %s\n%s\n%s",
			shortBuild(got), shortBuild(want), out, daemonLog(t, p))
	}
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	mkdirAll(t, filepath.Dir(dst))
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}
