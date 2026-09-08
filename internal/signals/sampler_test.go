package signals

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCollectTreeGathersDescendants(t *testing.T) {
	// 1 ─ 2 ─ 4
	//   └ 3 ─ 5 ─ 6
	// 7 ─ 8   (not under a root)
	children := map[int32][]int32{
		1: {2, 3},
		2: {4},
		3: {5},
		5: {6},
		7: {8},
	}
	got := collectTree([]int32{1}, children)
	if len(got) != 6 {
		t.Fatalf("want 6 members, got %d: %v", len(got), got)
	}
	in := map[int32]bool{}
	for _, pid := range got {
		in[pid] = true
	}
	for _, pid := range []int32{1, 2, 3, 4, 5, 6} {
		if !in[pid] {
			t.Errorf("pid %d missing from tree: %v", pid, got)
		}
	}
	for _, pid := range []int32{7, 8} {
		if in[pid] {
			t.Errorf("pid %d is not under the root but was collected: %v", pid, got)
		}
	}
}

func TestCollectTreeTerminatesOnACycle(t *testing.T) {
	// A process table cannot really contain a cycle, but the sampler runs unattended for
	// hours and a hang inside it would produce no error and no output — only a trace that
	// silently stops. The guard is cheap; this proves it works.
	children := map[int32][]int32{1: {2}, 2: {3}, 3: {1}}
	done := make(chan []int32, 1)
	go func() { done <- collectTree([]int32{1}, children) }()
	select {
	case got := <-done:
		if len(got) != 3 {
			t.Fatalf("want 3 members, got %d: %v", len(got), got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collectTree did not terminate on a cyclic parent map")
	}
}

// TestSamplerCountsDescendantCPU is the story's central trap, tested end to end against
// real processes: the CPU an agent causes is mostly burnt by what the agent launched, not
// by the agent itself, so a tree walk that stops at the root measures the wrong thing.
//
// The sampler is pointed at this test binary's own process name, and a chain of real child
// processes is spawned beneath it with a busy grandchild. If descendants are being counted,
// the tree CPU rises well above the root-only CPU.
func TestSamplerCountsDescendantCPU(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes and burns a second of CPU")
	}
	name := selfRootName(t)

	s := newSamplerNamed(name)
	ctx := context.Background()

	// Prime. The first reading has no previous tick to difference against.
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// A grandchild, so the walk has to recurse rather than just look at direct children:
	// sh -c spawns a subshell that spins.
	busy := exec.Command("/bin/sh", "-c", "sh -c 'while :; do :; done' & sleep 3; kill %1 2>/dev/null")
	if err := busy.Start(); err != nil {
		t.Fatalf("start busy child: %v", err)
	}
	defer func() {
		_ = busy.Process.Kill()
		_, _ = busy.Process.Wait()
	}()

	time.Sleep(1200 * time.Millisecond)

	got, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}

	if got.Tree.Roots < 1 {
		t.Fatalf("expected at least one root named %q, got %d", name, got.Tree.Roots)
	}
	if got.Tree.Procs <= got.Tree.Roots {
		t.Fatalf("descendants were not collected: Procs=%d Roots=%d", got.Tree.Procs, got.Tree.Roots)
	}
	// The spinning grandchild saturates a core. Root-only accounting cannot see it.
	if got.Tree.CPU <= got.Tree.CPURoots {
		t.Fatalf("tree CPU (%.3f) did not exceed root-only CPU (%.3f) with a busy grandchild running;"+
			" descendant CPU is not being summed", got.Tree.CPU, got.Tree.CPURoots)
	}
	if got.Tree.CPU < 0.5 {
		t.Fatalf("tree CPU %.3f is implausibly low with a core-saturating grandchild", got.Tree.CPU)
	}
	if got.Tree.RSSBytes == 0 {
		t.Error("tree RSS is zero, which no live process tree is")
	}
}

func TestSampleReportsPlausibleHostSignals(t *testing.T) {
	s := NewSampler()
	ctx := context.Background()
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	got, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got.CPU < 0 || got.CPU > 1 || math.IsNaN(got.CPU) {
		t.Errorf("CPU %v is not a utilisation in [0,1]", got.CPU)
	}
	if got.Cores != runtime.NumCPU() {
		t.Errorf("Cores = %d, want %d", got.Cores, runtime.NumCPU())
	}
	if got.MemTotalBytes == 0 {
		t.Error("MemTotalBytes is zero")
	}
	if got.MemAvailableBytes > got.MemTotalBytes {
		t.Errorf("MemAvailableBytes %d exceeds MemTotalBytes %d", got.MemAvailableBytes, got.MemTotalBytes)
	}
	if got.MemoryPressure < 0 || got.MemoryPressure > 1 {
		t.Errorf("MemoryPressure %v is out of [0,1]", got.MemoryPressure)
	}
	if got.LoadAvg1 < 0 {
		t.Errorf("LoadAvg1 %v is negative", got.LoadAvg1)
	}
	switch got.Thermal {
	case Nominal, Fair, Serious, Critical, Unknown:
	default:
		t.Errorf("Thermal %q is not one of the defined states", got.Thermal)
	}
	if got.CostMS <= 0 {
		t.Errorf("CostMS %v was not measured", got.CostMS)
	}
}

// TestSamplerStaysCheap holds the "must be invisible" trap to a number.
//
// GOV-02's traps say a sampler that costs anything measurable defeats the purpose of
// measuring a baseline, so the cost is asserted rather than hoped for. The budget is
// generous against the ~15 ms measured on this machine, because CI runners are slower and a
// flaky performance test gets deleted rather than fixed — but a regression that makes the
// tick cost a tenth of its own interval will trip it.
func TestSamplerStaysCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("timing sensitive")
	}
	s := NewSampler()
	ctx := context.Background()
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// A second call populates the name cache for the whole process table; steady-state cost
	// is what the sampler actually pays, and what is being bounded.
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}

	const budgetMS = 100.0
	best := math.MaxFloat64
	for i := 0; i < 5; i++ {
		got, err := s.Sample(ctx)
		if err != nil {
			t.Fatalf("sample: %v", err)
		}
		best = math.Min(best, got.CostMS)
	}
	if best > budgetMS {
		t.Errorf("cheapest of 5 warm samples cost %.1f ms, over the %.0f ms budget for a 1 Hz tick", best, budgetMS)
	}
	t.Logf("cheapest warm sample: %.1f ms", best)
}

// TestRootCacheIsTrimmed proves the cache tracks the process table rather than growing
// without bound. A sampler that runs for days on a machine that churns processes would
// otherwise leak one map entry per process ever seen.
func TestRootCacheIsTrimmed(t *testing.T) {
	s := newSamplerNamed("no-such-process-name")
	ctx := context.Background()
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	s.isRoot[999999] = true
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if _, ok := s.isRoot[999999]; ok {
		t.Error("root cache kept an entry for a pid that is not in the process table")
	}
}

// TestRootIsMatchedOnArgv0 is a regression test for a bug that silently produced a trace of
// all zeros on a machine running six sessions.
//
// Claude Code runs from a version-numbered executable with argv[0] set to "claude", so a
// sampler that matches the executable's own name finds nothing and reports a permanently
// idle machine. Nothing errors, every field parses, and the trace is worthless. This spawns
// a process whose argv[0] differs from its executable and asserts argv[0] wins.
func TestRootIsMatchedOnArgv0(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a process")
	}
	// exec with argv[0] set to something other than the binary's name, which is exactly
	// what Claude Code's launcher does.
	const argv0 = "governor-argv0-probe"
	cmd := exec.Command("/bin/sh")
	// Two commands, not one: a shell exec's a lone final command in place, which would
	// replace argv[0] with "sleep" and quietly test nothing.
	cmd.Args = []string{argv0, "-c", "sleep 5; :"}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	s := newSamplerNamed(argv0)
	ctx := context.Background()
	if _, err := s.Sample(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	got, err := s.Sample(ctx)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got.Tree.Roots < 1 {
		t.Fatalf("a process whose argv[0] is %q was not found as a root; "+
			"root detection has regressed to matching the executable name", argv0)
	}
}

// selfRootName is the name the sampler will match this test binary on.
//
// It is derived from argv[0] rather than from ps(1) because argv[0] is precisely what
// looksLikeRoot checks first, and because Linux truncates the kernel's comm field at 15
// characters while argv[0] is untruncated — a difference that would make this test's premise
// depend on how long the temporary test binary's name happened to be.
func selfRootName(t *testing.T) string {
	t.Helper()
	name := filepath.Base(os.Args[0])
	if name == "" || name == "." {
		t.Fatalf("could not derive a process name from argv[0] %q", os.Args[0])
	}
	return name
}
