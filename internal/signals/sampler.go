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
// one implementation, and a second one is premature until tier two exists).
//
// Most of it is platform-neutral: gopsutil covers CPU, load, memory and process
// introspection identically on darwin and linux. Two readings are not, and each has its own
// build-tagged pair rather than a runtime branch — thermal state behind ReadThermal, and CPU
// capacity behind cpuCapacity, where Linux prefers the cgroup quota to the host's core count
// (GOV-12). This is the darwin/linux split section 12.1's Signals row calls for; it is one
// implementation selected at compile time, not two implementations of the seam.
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

	// treeEvery is how often the process-tree walk is actually performed. Zero means every
	// tick, which is what Phase 0 uses. See NewSamplerEvery for why the daemon does not.
	treeEvery time.Duration
	// lastTree is the most recent walk, returned unchanged on ticks that skip one.
	lastTree Tree
	// lastTreeAt is when lastTree was walked.
	lastTreeAt time.Time

	// swapIn caches the cumulative bytes-swapped-in counter as of the previous tick, and
	// swapAt is when it was read. Like CPU, the useful signal is the rate, and the counter
	// is monotonic since boot.
	swapIn   uint64
	swapAt   time.Time
	swapSeen bool
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

// NewSamplerEvery returns a Sampler that refreshes the process-tree walk no more often than
// treeEvery, carrying the previous walk forward on the ticks in between.
//
// This exists because the tree walk is the whole cost of sampling and the daemon cannot
// afford it every second. MEASURED, not estimated: the Phase 0 sampler has run for 46 hours
// at 1 Hz and cost 2.30% of one core — 23.0 ms of CPU per tick, nearly all of it the
// per-process syscalls the walk makes across ~700 processes. The host signals either side of
// it cost 1-2 ms together. Refreshing the tree every 5 s brings the average to roughly 0.6%
// of a core, which is the bar GOV-12 sets.
//
// Nothing is lost from the v1 control loop by doing this. design.md section 5.1 lists the
// per-agent tree cost as a "Phase 2 refinement"; the signals the loop actually acts on —
// CPU utilisation, run queue, thermal, memory — are the cheap ones and still tick at 1 Hz.
//
// Phase 0 keeps the full-rate walk. NewSampler is unchanged and still walks every tick, so
// the trace the setpoint will be tuned against is collected exactly as it has been since
// GOV-02. Samples carry TreeFresh so a slower-tree consumer can tell a fresh walk from a
// carried-over one.
func NewSamplerEvery(treeEvery time.Duration) Sampler {
	s := newSamplerNamed(ClaudeProcessName)
	s.treeEvery = treeEvery
	return s
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

// cpuDelta returns how much CPU time a process accrued during the interval, and whether it
// could be determined at all.
//
// Two cases. A process seen on the previous tick differences against its own last reading —
// the ordinary path. A process NOT seen before is treated as new, and its whole cumulative
// time counted, but only when that total could physically have been accrued inside the
// interval: no machine burns more than one core-second per core per second. That test is what
// keeps short-lived children — a compiler, a test process, a git subprocess, each visible for
// a single tick — from scoring zero, which would make the descendant CPU this phase exists to
// measure read far too low.
//
// The bound is physical rather than temporal on purpose, and that was learnt the hard way.
// The obvious version asks CreateTime whether the process was born after the last tick. It
// worked on macOS and failed silently on Linux, where gopsutil derives CreateTime from
// /proc/stat's btime, which has ONE SECOND of granularity — so a process created 1.2 s after
// the previous tick can read as older than it, and every short-lived child scores zero. CI
// caught it: tree CPU and root-only CPU came back bit-identical with a core-saturating
// grandchild running. Comparing against elapsed*cores needs no clock, no boot time and no
// syscall, and is the same arithmetic on both platforms.
//
// The residual error is an older process that only now entered the tree, because its parent
// became a root between ticks, and whose accumulated CPU happens to sit under the interval's
// physical ceiling. It is counted once, in full, on that tick. Rare, bounded, and far cheaper
// than missing every short-lived process on one of the two supported platforms.
func cpuDelta(total, prev float64, hadPrev bool, elapsed float64, cores int) (float64, bool) {
	if elapsed <= 0 || cores <= 0 {
		return 0, false
	}
	var delta float64
	switch {
	case hadPrev:
		delta = total - prev
	case total <= elapsed*float64(cores):
		delta = total
	default:
		// Present before this sampler started watching it. Counting its lifetime would be a
		// spike that never happened; it is tracked from the next tick instead.
		return 0, false
	}
	if delta < 0 {
		// Negative means pid reuse: the counter belongs to a different process now.
		return 0, false
	}
	return delta, true
}

// Host reports the fixed shape of the machine.
//
// THE PERFORMANCE-CORE SPLIT IS NOW MEASURED WHERE THE PLATFORM PUBLISHES IT (GOV-25). This
// function used to set PerformanceCores equal to Cores unconditionally, with a note deferring
// the split to `governor calibrate` on the grounds that guessing one would bake a number
// nobody measured into a trace nobody could correct. That reasoning was right and the
// conclusion is now reachable: macOS publishes the split through `hw.perflevelN.*` with no
// privilege at all, so it does not have to be guessed. See perfcores_darwin.go.
//
// Where no platform publishes it, PerformanceCores still equals Cores — but
// PerformanceCoresKnown is false, which is the difference between defaulting and measuring
// and is the bit the old code had no way to express.
func (s *hostSampler) Host(ctx context.Context) (Host, error) {
	h := Host{Cores: runtime.NumCPU()}
	h.PerformanceCores = h.Cores
	if n, ok := performanceCores(); ok && n > 0 && n <= h.Cores {
		// The upper bound is not defensive clutter. A perflevel count above the visible core
		// count means the two readings disagree about what machine this is — a VM exposing a
		// subset of host CPUs is the ordinary way that happens — and a "performance core"
		// count larger than the cores available to run on is not a number to calibrate from.
		h.PerformanceCores, h.PerformanceCoresKnown = n, true
	}
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
// CPUCapacity is the same capacity reading a Sample carries, without taking a sample.
//
// It exists for the daemon's first second (GOV-41). A Sample is a delta against the previous
// one, so the first reading lands a full tick after start — and until it does, a controller
// with no capacity has no ceiling and answers the floor. Capacity is a level, not a rate, so
// it can be read at construction; this is that read, through the same build-tagged pair the
// sampler uses, so the two can never disagree about what the host has.
func CPUCapacity() (float64, string) { return cpuCapacity() }

func (s *hostSampler) Sample(ctx context.Context) (Sample, error) {
	start := time.Now()
	out := Sample{Thermal: ReadThermal(), Cores: runtime.NumCPU()}
	out.CPUCapacity, out.CPUCapacitySource = cpuCapacity()

	// cpu.Percent with a zero interval differences against gopsutil's own last call, which
	// is why the first tick must be discarded rather than trusted.
	if pct, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pct) > 0 {
		out.CPU = pct[0] / 100
	}
	if avg, err := load.AvgWithContext(ctx); err == nil {
		out.LoadAvg1 = avg.Load1
		if out.CPUCapacity > 0 {
			out.RunQueue = avg.Load1 / out.CPUCapacity
		}
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		out.MemTotalBytes = v.Total
		out.MemAvailableBytes = v.Available
		if v.Total > 0 {
			out.MemoryPressure = 1 - float64(v.Available)/float64(v.Total)
		}
	}
	out.SwapInBytesPerSec = s.swapRate(ctx, start)
	out.Tree, out.TreeFresh = s.treeCached(ctx, start)

	out.CostMS = float64(time.Since(start).Microseconds()) / 1000
	return out, nil
}

// swapRate returns bytes per second faulted back in from swap since the previous tick.
//
// Zero on the priming tick, and zero again if the counter goes backwards — which it does on
// a machine that has swapoff'd, and would otherwise present as a large negative rate or, once
// stored in a uint64 difference, an enormous positive one.
func (s *hostSampler) swapRate(ctx context.Context, now time.Time) float64 {
	sw, err := mem.SwapMemoryWithContext(ctx)
	if err != nil {
		return 0
	}
	prev, at, seen := s.swapIn, s.swapAt, s.swapSeen
	s.swapIn, s.swapAt, s.swapSeen = sw.Sin, now, true
	if !seen {
		return 0
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 || sw.Sin < prev {
		return 0
	}
	return float64(sw.Sin-prev) / elapsed
}

// treeCached walks the process tree, or returns the previous walk when one is recent enough.
//
// The freshness test is against the time of the last walk rather than a tick count, for the
// same reason the EWMA is time-aware: ticks are not evenly spaced on a loaded machine, and
// "every fifth tick" on a machine that skipped four of them is not every five seconds.
func (s *hostSampler) treeCached(ctx context.Context, now time.Time) (Tree, bool) {
	if s.treeEvery > 0 && !s.lastTreeAt.IsZero() && now.Sub(s.lastTreeAt) < s.treeEvery {
		return s.lastTree, false
	}
	// Whether the walk about to happen can produce CPU figures at all. The first walk of a
	// sampler's life has nothing to difference against, so every rate in it is zero by
	// construction — and caching THAT would republish a manufactured zero on every tick until
	// the interval elapsed. On a 5 s interval that is five consecutive samples claiming the
	// claude tree is using no CPU, which is the "reports an idle machine" failure this file
	// and statusReply's null both exist to prevent, arriving from inside the cache.
	//
	// So an unprimed walk is returned but not retained, and the next tick walks again — one
	// extra walk, once, at startup.
	wasPrimed := s.primed
	t := s.tree(ctx)
	if wasPrimed {
		s.lastTree, s.lastTreeAt = t, now
	}
	return t, true
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
		prev, hadPrev := s.cpuSecs[pid]
		delta, ok := cpuDelta(total, prev, hadPrev, elapsed, runtime.NumCPU())
		if !ok {
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
