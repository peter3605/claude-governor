package signals

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The owner walk (GOV-07).
//
// What is being defended here is not "find a pid". It is the difference between binding a
// lease to the session, whose lifetime matches the agent's, and binding it to whatever
// process happened to run the hook. The second one is a silent budget bug: a shell that
// exits the moment the hook does is reported dead by PIDLiveness on the next reaper tick,
// two seconds later, and every confirmed lease on the machine is collected while its agent
// is still running. Nothing errors and the governor simply stops governing.

func TestWalkToOwnerFindsTheSession(t *testing.T) {
	born := time.Unix(1_757_212_800, 0)

	// hook -> sh -> claude -> node -> init. The shell in the middle is the case that
	// matters: os.Getppid() would stop there.
	table := map[int32]procInfo{
		50: {ppid: 40, name: "governor"},
		40: {ppid: 30, name: "sh"},
		30: {ppid: 20, name: "claude", started: born},
		20: {ppid: 1, name: "node"},
		1:  {ppid: 0, name: "launchd"},
	}

	pid, started, ok := walkToOwner(50, "claude", lookupIn(table))
	if !ok {
		t.Fatal("walkToOwner: ok=false, want the claude ancestor found through the shell")
	}
	if pid != 30 {
		t.Errorf("walkToOwner: pid %d, want 30 (the claude process, not the shell at 40)", pid)
	}
	if !started.Equal(born) {
		t.Errorf("walkToOwner: started %v, want %v", started, born)
	}
}

// The direct case, with no shell in between. Both wirings are real: whether the harness
// runs a hook through a shell is not something this binary controls.
func TestWalkToOwnerFindsAnImmediateParent(t *testing.T) {
	table := map[int32]procInfo{
		50: {ppid: 30, name: "governor"},
		30: {ppid: 20, name: "claude"},
		20: {ppid: 1, name: "node"},
	}
	pid, _, ok := walkToOwner(50, "claude", lookupIn(table))
	if !ok || pid != 30 {
		t.Fatalf("walkToOwner: (%d, %v), want (30, true)", pid, ok)
	}
}

// Every way the walk can fail must fail to "unknown", never to a plausible-looking pid.
// A wrong owner is reaped out from under a live agent; an unknown owner just waits out its
// TTL, and those two mistakes are not the same size.
func TestWalkToOwnerReportsUnknownRatherThanGuessing(t *testing.T) {
	deep := map[int32]procInfo{}
	for i := int32(2); i < 40; i++ {
		deep[i] = procInfo{ppid: i + 1, name: "sh"}
	}
	deep[40] = procInfo{ppid: 1, name: "claude"}

	cases := []struct {
		name  string
		self  int32
		table map[int32]procInfo
	}{
		{
			// No claude anywhere: the governor is being run by hand from a terminal.
			name: "no claude ancestor",
			self: 50,
			table: map[int32]procInfo{
				50: {ppid: 40, name: "governor"},
				40: {ppid: 30, name: "zsh"},
				30: {ppid: 1, name: "login"},
			},
		},
		{
			// Reaching init means the assumption was wrong. Binding to pid 1 would be
			// worse than binding to nothing: launchd never exits, so the lease would never
			// be reaped for liveness at all.
			name: "walk reaches init",
			self: 50,
			table: map[int32]procInfo{
				50: {ppid: 1, name: "governor"},
				1:  {ppid: 0, name: "launchd"},
			},
		},
		{
			// pid 1 must be refused even when it carries the name being looked for. This
			// is the guard stated as a claim rather than as an incidental: the walk stops
			// before init because a lease owned by init is a lease no reaper can ever
			// collect for liveness, not because init happens to be called something else.
			// On Linux an orphaned hook really is reparented to pid 1, so the walk does
			// reach it.
			name: "init itself carries the name",
			self: 50,
			table: map[int32]procInfo{
				50: {ppid: 1, name: "governor"},
				1:  {ppid: 0, name: "claude"},
			},
		},
		{
			name:  "ancestry deeper than the hop limit",
			self:  2,
			table: deep,
		},
		{
			// A process that exited mid-walk. Frequent on a busy machine.
			name: "ancestor vanished mid-walk",
			self: 50,
			table: map[int32]procInfo{
				50: {ppid: 40, name: "governor"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pid, _, ok := walkToOwner(tc.self, "claude", lookupIn(tc.table))
			if ok {
				t.Fatalf("walkToOwner: found pid %d, want ok=false", pid)
			}
			if pid != 0 {
				t.Errorf("walkToOwner: pid %d alongside ok=false, want 0", pid)
			}
		})
	}
}

func lookupIn(table map[int32]procInfo) func(int32) (procInfo, bool) {
	return func(pid int32) (procInfo, bool) {
		info, ok := table[pid]
		return info, ok
	}
}

// ---------------------------------------------------------------------------
// The same claim, against the real process table.
//
// The table tests above prove the walk's logic on every platform. They cannot prove that
// liveProcInfo reads argv[0] the way the walk expects, and that is the half that broke in
// GOV-02 — the sampler's logic was fine and its process naming found zero sessions on a
// machine running six. So this builds a real ancestry: a copy of the test binary named
// `claude`, which runs a shell, which runs the test binary again to call OwningSession.
// Exactly the shape the hook sees in production, including the shell hop.

const (
	roleEnv    = "GOVERNOR_OWNER_TEST_ROLE"
	roleClaude = "claude"
	roleHook   = "hook"
)

// TestMain gives the re-executed copies of this binary something to be. Under the default
// (empty) role it just runs the tests.
func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case roleClaude:
		// Stand in for the session process: run the hook through a shell, exactly as a
		// harness that wraps a hook command would, and pass its output back up.
		out, err := exec.Command("/bin/sh", "-c", os.Args[1]).CombinedOutput()
		os.Stdout.Write(out)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case roleHook:
		pid, started, ok := OwningSession(context.Background())
		fmt.Printf("%v %d %d\n", ok, pid, started.Unix())
		os.Exit(0)
	default:
		os.Exit(m.Run())
	}
}

func TestOwningSessionFindsARealClaudeAncestorThroughAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// The executable is named 2.1.263 and is launched with argv[0] set to "claude". That
	// is not a contrivance — it is exactly how Claude Code runs
	// (~/.local/share/claude/versions/2.1.263, argv[0] "claude"), and it is the shape that
	// cost GOV-02 a whole trace: a sampler keyed on the executable's own name found ZERO
	// sessions on a machine running six, and failed silently while every record still wrote.
	//
	// A copy named "claude" would pass this test whether the walk read argv[0] or the
	// executable name, which would make the test worthless for the one thing it is here to
	// prove.
	dir := t.TempDir()
	fake := filepath.Join(dir, "2.1.263")
	if err := copyExecutable(self, fake); err != nil {
		t.Fatalf("copy test binary to %s: %v", fake, err)
	}
	if filepath.Base(fake) == ClaudeProcessName {
		t.Fatalf("the fake executable is named %q; it must NOT be named %q or this test "+
			"cannot distinguish argv[0] from the executable name", fake, ClaudeProcessName)
	}

	// The grandchild's role is set inline in the shell command rather than inherited, so
	// the two generations can be different things while sharing an environment.
	//
	// Whether a shell hop actually survives into the ancestry is up to /bin/sh: most shells
	// exec the final command of a -c, which would make the hook a direct child of the fake
	// claude. Either shape is a real one — a harness may or may not wrap a hook — and the
	// walk must handle both, so this asserts the answer rather than the path taken. The
	// deterministic shell hop is the table test above.
	hookCmd := fmt.Sprintf("%s=%s %s -test.run=TestMain", roleEnv, roleHook, self)

	// Args[0] is set by hand rather than by exec.Command, which would default it to Path.
	cmd := &exec.Cmd{
		Path: fake,
		Args: []string{ClaudeProcessName, hookCmd},
		Env:  append(os.Environ(), roleEnv+"="+roleClaude),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run fake claude: %v (output: %s)", err, out)
	}

	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 3 {
		t.Fatalf("hook printed %q, want \"<ok> <pid> <started>\"", out)
	}
	if fields[0] != "true" {
		t.Fatalf("OwningSession: ok=%s from a real claude ancestry (output %q)", fields[0], out)
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid <= 1 {
		t.Fatalf("OwningSession: pid %q, want the fake claude's pid", fields[1])
	}
	if pid != cmd.Process.Pid {
		t.Errorf("OwningSession: pid %d, want %d (the process whose argv[0] is %s, not the "+
			"shell between and not its executable name %s)",
			pid, cmd.Process.Pid, ClaudeProcessName, filepath.Base(fake))
	}
	if started, _ := strconv.ParseInt(fields[2], 10, 64); started <= 0 {
		t.Errorf("OwningSession: start time %q, want the ancestor's real start time — "+
			"without it a recycled pid keeps a dead lease alive", fields[2])
	}
}

func copyExecutable(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
