package main

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/transport"
)

// GOV-B58. Claude Code SIGTERMs a status line that is still running when the next refresh starts,
// then SIGKILLs its group 1.5 s later (hook-surface.md §14.4). These tests do that to the real
// binary, out of process, because only a process can be killed: GOV-33 proved that an
// in-process test of this always passes.
//
// The binary is built with a stall linked into both writers, right after os.CreateTemp, so the
// signal lands while a staging file exists. Without the stall that window is microseconds wide
// and no test could hit it on purpose. The stall is a link-time variable that is empty in every
// real build (statuslineWriteStall).

// stallingGovernor builds the binary with each post-print write paused for stall.
func stallingGovernor(t *testing.T, stall time.Duration) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the governor binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("signals are POSIX")
	}
	out := filepath.Join(t.TempDir(), "governor")
	buildGovernorTo(t, out, "-ldflags", "-X main.statuslineWriteStall="+stall.String())
	return out
}

// killableStatusline is one run of `governor statusline` against its own state directory,
// with no daemon on its socket (so it caches "down", which still writes statusline.json) and a
// payload carrying rate_limits (so it writes rate_limits.json too).
type killableStatusline struct {
	cmd   *exec.Cmd
	state string
	line  chan string
}

func startKillableStatusline(t *testing.T, bin, state string) *killableStatusline {
	t.Helper()
	cmd := exec.Command(bin, "statusline")
	cmd.Env = append(os.Environ(),
		transport.StateDirEnv+"="+state,
		transport.EndpointEnv+"=unix://"+filepath.Join(state, "sock"),
	)
	cmd.Stdin = strings.NewReader(`{"session_id":"gov-b58","version":"2.1.331",` +
		`"rate_limits":{"five_hour":{"used_percentage":27,"resets_at":1795507200}}}`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	k := &killableStatusline{cmd: cmd, state: state, line: make(chan string, 1)}
	go func() {
		l, _ := bufio.NewReader(stdout).ReadString('\n')
		k.line <- l
	}()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return k
}

// printed waits for the line, which is the moment the post-print writes begin.
func (k *killableStatusline) printed(t *testing.T) {
	t.Helper()
	select {
	case l := <-k.line:
		if strings.TrimSpace(l) == "" {
			t.Fatal("the status line printed nothing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the status line never printed")
	}
}

// exited waits for the process, and fails if it outlives what Claude Code would allow it.
func (k *killableStatusline) exited(t *testing.T, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { k.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("the status line was still running %v after the signal", within)
	}
}

func stagingFiles(t *testing.T, state string) []string {
	t.Helper()
	var found []string
	for _, pat := range []string{statuslineCacheFile + ".*", rateLimitsFile + ".*"} {
		m, err := filepath.Glob(filepath.Join(state, pat))
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, m...)
	}
	return found
}

// A SIGTERM during either write waits for that write, and the process then exits without
// leaving a staging file. The two cases put the signal inside each writer's stall: the cache is
// written first, then rate_limits.json.
func TestAStatuslineTerminatedMidWriteLeavesNoStagingFile(t *testing.T) {
	const stall = 400 * time.Millisecond
	bin := stallingGovernor(t, stall)
	for _, tc := range []struct {
		name  string
		after time.Duration
	}{
		{"during the cache write", 150 * time.Millisecond},
		{"during the rate limits write", stall + 250*time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := sockDir(t)
			k := startKillableStatusline(t, bin, state)
			k.printed(t)
			time.Sleep(tc.after)
			if err := k.cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			// Claude Code escalates to SIGKILL 1.5 s after the TERM. A guard that held the
			// process past that would only trade a stray for a SIGKILL.
			k.exited(t, 1500*time.Millisecond)
			if left := stagingFiles(t, state); len(left) > 0 {
				t.Fatalf("a SIGTERM %v after the print left %v behind: the write in flight was "+
					"cut off holding its staging file, which is the stray of 2026-11-22", tc.after, left)
			}
			if _, err := os.Stat(filepath.Join(state, statuslineCacheFile)); err != nil {
				t.Errorf("the cache write the signal waited for did not land: %v", err)
			}
		})
	}
}

// A SIGTERM with no write in flight still ends the process at once: the guard waits for a write
// and for nothing else. The stall makes the run long, and the signal arrives before the print,
// while stdin is still open.
func TestAStatuslineTerminatedBeforeWritingExitsAtOnce(t *testing.T) {
	bin := stallingGovernor(t, 2*time.Second)
	state := sockDir(t)
	cmd := exec.Command(bin, "statusline")
	cmd.Env = append(os.Environ(),
		transport.StateDirEnv+"="+state,
		transport.EndpointEnv+"=unix://"+filepath.Join(state, "sock"),
	)
	stdin, err := cmd.StdinPipe() // never closed: the run cannot get past decoding
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	time.Sleep(20 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	k := &killableStatusline{cmd: cmd}
	k.exited(t, 300*time.Millisecond)
	if left := stagingFiles(t, state); len(left) > 0 {
		t.Fatalf("left %v behind", left)
	}
}

// SIGKILL cannot be waited for, so the staging file it strands is collected by a later run:
// one older than statuslineTempMaxAge goes, one younger stays (it may be another session's
// write in flight).
func TestAStagingFileStrandedBySIGKILLIsSweptByALaterRun(t *testing.T) {
	bin := stallingGovernor(t, 400*time.Millisecond)
	state := sockDir(t)

	k := startKillableStatusline(t, bin, state)
	k.printed(t)
	time.Sleep(150 * time.Millisecond)
	k.cmd.Process.Signal(syscall.SIGKILL)
	k.exited(t, time.Second)
	stranded := stagingFiles(t, state)
	if len(stranded) != 1 {
		t.Fatalf("a SIGKILL mid-write stranded %v, want exactly one staging file", stranded)
	}
	old := time.Now().Add(-2 * statuslineTempMaxAge)
	if err := os.Chtimes(stranded[0], old, old); err != nil {
		t.Fatal(err)
	}
	young := filepath.Join(state, rateLimitsFile+".inflight")
	if err := os.WriteFile(young, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	next := startKillableStatusline(t, bin, state)
	next.printed(t)
	next.exited(t, 5*time.Second)

	if _, err := os.Stat(stranded[0]); !os.IsNotExist(err) {
		t.Errorf("the next run did not sweep %s, stranded by SIGKILL and backdated past %v",
			filepath.Base(stranded[0]), statuslineTempMaxAge)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("the sweep removed %s, which is younger than %v and may be a write in flight: %v",
			filepath.Base(young), statuslineTempMaxAge, err)
	}
}

// The signal guard is installed by main and nowhere else. Anywhere a test can reach would put
// the handler into the test binary, where the next stray signal exits the whole package run:
// the first version of GOV-B58 did that from runStatusline, and CI died with status 141.
func TestTheStatuslineSignalGuardIsInstalledOnlyByMain(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "guardStatuslineSignals" {
							callers = append(callers, fn.Name.Name)
						}
					}
					return true
				})
			}
		}
	}
	if len(callers) != 1 || callers[0] != "main" {
		t.Fatalf("guardStatuslineSignals is called from %v, want exactly [main]", callers)
	}
}
