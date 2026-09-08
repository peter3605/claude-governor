package signals

import (
	"context"
	"path/filepath"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// ClaudeProcessName is the command name of a Claude Code session process.
//
// It is matched against argv[0] and NOT against the executable's own name, which is the
// non-obvious part and was measured rather than assumed. Claude Code installs itself as a
// version-numbered file — /Users/<u>/.local/share/claude/versions/2.1.263 — and launches it
// with argv[0] set to "claude". So `ps -o comm=` says "claude" while the executable's base
// name says "2.1.263", and a sampler keyed on the executable name finds zero sessions on a
// machine running six. That failure is silent: every record still writes, every field still
// parses, and the whole trace reads as a permanently idle machine.
//
// The comparison is case-sensitive and exact for the reasons given on Tree.Roots — "Claude"
// and "Claude Helper" are the desktop app, and matching them would feed an Electron UI's CPU
// into the governor. It also excludes the browser native host, which runs the same
// executable but with argv[0] set to the full versioned path instead.
const ClaudeProcessName = "claude"

// hostSampler is the one implementation of Sampler (design.md section 12.1: one interface,
// one implementation, and a second one is premature until tier two exists). It is not
// per-platform: gopsutil covers CPU, load, memory and process introspection identically on
// darwin and linux, and the single genuinely platform-specific reading — thermal state —
// already has its _darwin/_other pair behind ReadThermal.
//
// It is stateful because the readings that matter are rates, not levels. CPU utilisation
// and per-process CPU are both deltas between consecutive ticks, so a sampler is a thing
// that must be kept and ticked rather than a function that can be called once.
//
// Not safe for concurrent use. One sampler, one goroutine, one tick at a time.
type hostSampler struct {
	// isRoot caches pid -> "is this a tree root". Deciding it costs a per-process syscall
	// (measured ~15 ms for CmdlineSlice across ~740 processes, ~26 ms for Name), and the
	// answer never changes for a live pid, so it is computed once. This cache is what keeps
	// the steady-state tick near 15 ms rather than paying the full scan every second.
	//
	// Entries for pids not seen on a tick are evicted, so the map tracks the process table
	// rather than growing forever. A pid recycled within one second onto a different process
	// would carry the stale answer for one sample; that is an acceptable error for
	// instrumentation and not worth a CreateTime syscall per process to rule out.
	isRoot map[int32]bool
	// cpuSecs caches pid -> cumulative CPU seconds as of the previous tick.
	cpuSecs map[int32]float64
	// at is when the previous tick's cpuSecs were read.
	at time.Time
	// primed reports whether a previous tick exists to difference against.
	primed bool
	// rootName is the command name that starts a tree. It is a field rather than a direct
	// use of the constant so that a test can point the walk at a process tree it actually
	// controls: descendant accounting is the load-bearing part of this file, and a test
	// that cannot spawn its own children can only assert against whatever happens to be
	// running.
	rootName string
}

// NewSampler returns a Sampler for this host.
//
// The first Sample it returns is a priming read: CPU utilisation and every process-tree
// rate are differences against a previous tick that does not yet exist, so they come back
// zero. Callers that record samples MUST discard the first one rather than write a row of
// false zeroes into the trace.
func NewSampler() Sampler {
	return newSamplerNamed(ClaudeProcessName)
}

func newSamplerNamed(rootName string) *hostSampler {
	return &hostSampler{
		isRoot:   make(map[int32]bool),
		cpuSecs:  make(map[int32]float64),
		rootName: rootName,
	}
}

// collectTree returns the roots and all their transitive descendants, breadth first.
//
// visited guards against a cycle in the parent map. A cycle should be impossible — a
// process table is a forest — but the sampler runs unattended for hours and an infinite
// loop inside it would be a hang with no error and no output.
func collectTree(roots []int32, children map[int32][]int32) []int32 {
	visited := make(map[int32]bool, len(roots)*4)
	queue := append([]int32(nil), roots...)
	var members []int32
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if visited[pid] {
			continue
		}
		visited[pid] = true
		members = append(members, pid)
		queue = append(queue, children[pid]...)
	}
	return members
}

// Host reports the fixed shape of the machine.
//
// PerformanceCores is not distinguishable from gopsutil on Apple silicon, so it is reported
// equal to Cores here and left for `governor calibrate` (GOV-16) to measure properly. B_max
// depends on it (section 5.2) and the control loop does not exist yet, so guessing a split
// now would bake a number nobody measured into a trace nobody can correct.
func (s *hostSampler) Host(ctx context.Context) (Host, error) {
	h := Host{Cores: runtime.NumCPU()}
	h.PerformanceCores = h.Cores
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		h.MemoryBytes = v.Total
	}
	return h, nil
}

// Sample takes one reading of the host.
//
// It is deliberately forgiving: a failure in any single signal leaves that field at its
// zero value and the rest of the sample is still returned. The sampler runs unattended for
// hours and a transient failure to read, say, load average is not a reason to lose the CPU
// trace for that second. The error return is reserved for a failure that invalidates the
// whole reading, of which there are currently none.
func (s *hostSampler) Sample(ctx context.Context) (Sample, error) {
	start := time.Now()
	out := Sample{Thermal: ReadThermal(), Cores: runtime.NumCPU()}

	// cpu.Percent with a zero interval differences against gopsutil's own last call, which
	// is why the first tick must be discarded rather than trusted.
	if pct, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pct) > 0 {
		out.CPU = pct[0] / 100
	}
	if avg, err := load.AvgWithContext(ctx); err == nil {
		out.LoadAvg1 = avg.Load1
		if out.Cores > 0 {
			out.RunQueue = avg.Load1 / float64(out.Cores)
		}
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		out.MemTotalBytes = v.Total
		out.MemAvailableBytes = v.Available
		if v.Total > 0 {
			out.MemoryPressure = 1 - float64(v.Available)/float64(v.Total)
		}
	}
	out.Tree = s.tree(ctx)

	out.CostMS = float64(time.Since(start).Microseconds()) / 1000
	return out, nil
}

// tree walks the process table and sums the cost of the claude process tree.
//
// The descendants are the point. The question Phase 0 answers is whether the load comes
// from the agents or from what they spawn, and a build or a test suite launched by an agent
// is a child of the claude process, not a claude process itself. Summing only the roots
// would measure the harness and miss the work.
func (s *hostSampler) tree(ctx context.Context) Tree {
	var t Tree

	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return t
	}

	// children maps a pid to its direct children, and seen collects the live pids so the
	// name cache can be trimmed to the current process table.
	children := make(map[int32][]int32, len(procs))
	byPID := make(map[int32]*process.Process, len(procs))
	seen := make(map[int32]bool, len(procs))
	var roots []int32

	for _, p := range procs {
		ppid, err := p.PpidWithContext(ctx)
		if err != nil {
			// The process exited between enumeration and now. Normal, and frequent on a
			// busy machine; it is not an error worth reporting.
			continue
		}
		seen[p.Pid] = true
		byPID[p.Pid] = p
		children[ppid] = append(children[ppid], p.Pid)

		root, ok := s.isRoot[p.Pid]
		if !ok {
			root = s.looksLikeRoot(ctx, p)
			s.isRoot[p.Pid] = root
		}
		if root {
			roots = append(roots, p.Pid)
		}
	}

	for pid := range s.isRoot {
		if !seen[pid] {
			delete(s.isRoot, pid)
		}
	}

	members := collectTree(roots, children)

	rootSet := make(map[int32]bool, len(roots))
	for _, pid := range roots {
		rootSet[pid] = true
	}

	elapsed := time.Since(s.at).Seconds()
	next := make(map[int32]float64, len(members))

	for _, pid := range members {
		p := byPID[pid]
		if p == nil {
			continue
		}
		times, err := p.TimesWithContext(ctx)
		if err != nil {
			continue
		}
		total := times.User + times.System
		next[pid] = total

		if mi, err := p.MemoryInfoWithContext(ctx); err == nil && mi != nil {
			t.RSSBytes += mi.RSS
		}

		if !s.primed || elapsed <= 0 {
			continue
		}
		delta, known := 0.0, false
		if prev, ok := s.cpuSecs[pid]; ok {
			delta, known = total-prev, true
		} else if created, err := p.CreateTimeWithContext(ctx); err == nil &&
			time.UnixMilli(created).After(s.at) {
			// A process that did not exist on the previous tick but was created after it:
			// all of its CPU time was accrued inside this interval, so all of it counts.
			// Without this, every short-lived child of an agent — which is most of what a
			// build or a test run is — would contribute nothing on the one tick it is
			// visible for, and the descendant CPU this story exists to measure would read
			// far too low.
			delta, known = total, true
		}
		if !known || delta < 0 {
			// Negative means pid reuse: the counter belongs to a different process now.
			continue
		}
		cores := delta / elapsed
		t.CPU += cores
		if rootSet[pid] {
			t.CPURoots += cores
		}
	}

	s.cpuSecs = next
	s.at = time.Now()
	s.primed = true

	t.Roots = len(roots)
	t.Procs = len(members)
	return t
}

// looksLikeRoot decides whether one process starts a tree.
//
// argv[0] is checked first because it is what actually identifies a Claude Code session (see
// ClaudeProcessName) and because it is the cheaper of the two calls. The executable name is
// the fallback, which covers a build or a platform where the binary really is named claude
// rather than launched under that name — and is what makes the check work for a test that
// points the sampler at its own process tree.
func (s *hostSampler) looksLikeRoot(ctx context.Context, p *process.Process) bool {
	if argv, err := p.CmdlineSliceWithContext(ctx); err == nil && len(argv) > 0 && argv[0] != "" {
		if filepath.Base(argv[0]) == s.rootName {
			return true
		}
	}
	name, err := p.NameWithContext(ctx)
	return err == nil && name == s.rootName
}
