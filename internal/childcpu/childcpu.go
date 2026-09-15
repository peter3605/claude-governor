// Package childcpu attributes CPU across a process tree, bucketed by what kind of work it is.
//
// It is the body of `tools/childcpu`, lifted out of that command's main so that
// `tools/sweep` can take the SAME measurement programmatically. GOV-31 clause 1 asks for
// build-test core-seconds "from tools/childcpu"; a second walk written for the sweep would
// have satisfied the words and broken the intent, because two decompositions that disagree
// are worse than one that is merely imperfect. This package is that one decomposition.
//
// The attribution deliberately mirrors internal/signals/sampler.go rather than improving on
// it: the same argv[0] root rule, the same transitive walk, and the same cpuDelta physical
// bound that keeps a compiler visible for a single tick from scoring zero. A better method
// would measure something the sampler is not measuring, which is not what this is for.
package childcpu

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// DefaultRootName is the process name that roots the tree for the Phase 0 decomposition:
// a Claude Code session. The sweep roots at its own PID instead.
const DefaultRootName = "claude"

// SelfMeasurementBucket holds CPU burnt by the measuring process's own ancestry. It is
// tracked rather than discarded so that a reader can see it was non-zero and was excluded,
// instead of wondering whether it was counted.
const SelfMeasurementBucket = "self-measurement"

// Key identifies one row of the by-name table.
type Key struct{ Bucket, Name string }

// Meter accumulates per-bucket core-seconds over repeated Tick calls.
//
// It is not safe for concurrent use. Both callers drive it from a single sampling goroutine,
// which is also the only way the deltas mean anything: the accounting is differential, so a
// tick taken from two goroutines at once would double-count the interval between them.
type Meter struct {
	// RootName roots the tree at every process with this argv[0] basename. Empty means no
	// name-based rooting, which is what the sweep wants — it names its roots by PID.
	RootName string
	// RootPIDs roots the tree at these PIDs as well. A PID that has exited is simply not
	// found and contributes nothing.
	RootPIDs []int32
	// ExcludeSubtreeOf is a process whose ancestry is attributed to SelfMeasurementBucket
	// rather than to the work under test. Zero disables it.
	//
	// The Phase 0 tool needs this because it runs from a Bash tool call INSIDE a live
	// session and would otherwise measure itself. The sweep does not, because it is its own
	// root rather than a descendant of one.
	ExcludeSubtreeOf int32

	cores    int
	excluded map[int32]bool
	prev     map[int32]float64
	primed   bool
	last     time.Time

	coreSecs map[Key]float64
	rootSecs float64
	ticks    int
	peakDesc float64
}

// Report is what a completed measurement says.
type Report struct {
	// RootCoreSec is CPU burnt in the root processes themselves, not their descendants.
	RootCoreSec float64
	// CoreSecs is descendant CPU by bucket and process name, excluding the measurement's
	// own subtree.
	CoreSecs map[Key]float64
	// PeakDescendantCores is the highest descendant core count observed in any single tick.
	//
	// RECORDED BUT NOT LOAD-BEARING, and GOV-15 is why: it measures how fast the machine is
	// going, not how oversubscribed it is, and it is bounded above by the core count, so a
	// thrashing machine reads LOWER than a machine running one build at full width. See
	// docs/control-proof.md. It is reported so a reader can check that claim, not so a
	// conclusion can be drawn from it.
	PeakDescendantCores float64
	// Ticks is how many samples landed, so a short or interrupted window is visible.
	Ticks int
	// Cores is the logical core count the physical bound was applied against.
	Cores int
}

// ByBucket sums the by-name table into buckets, dropping the self-measurement rows.
func (r Report) ByBucket() map[string]float64 {
	out := map[string]float64{}
	for k, v := range r.CoreSecs {
		if k.Bucket == SelfMeasurementBucket {
			continue
		}
		out[k.Bucket] += v
	}
	return out
}

// DescendantCoreSec is total descendant CPU, excluding the self-measurement rows.
func (r Report) DescendantCoreSec() float64 {
	t := 0.0
	for _, v := range r.ByBucket() {
		t += v
	}
	return t
}

// BuildTestCoreSec is the number GOV-31 clause 1 reports per arm: CPU spent compiling and
// testing, which is the work the governor is admitting or refusing.
func (r Report) BuildTestCoreSec() float64 { return r.ByBucket()["build-test"] }

// New prepares a meter. It resolves the excluded ancestry once, because walking up from the
// measuring process on every tick would cost more than the thing being measured.
func New(ctx context.Context, m Meter) *Meter {
	m.cores = runtime.NumCPU()
	m.prev = map[int32]float64{}
	m.coreSecs = map[Key]float64{}
	m.excluded = map[int32]bool{}
	if m.ExcludeSubtreeOf > 0 {
		m.excluded = ancestryOf(ctx, m.ExcludeSubtreeOf)
	}
	m.last = time.Now()
	return &m
}

// Tick takes one sample. The first one only primes the deltas and contributes no CPU, which
// is why a one-tick window correctly reports nothing rather than reporting the whole
// lifetime of every process it found.
func (m *Meter) Tick(ctx context.Context) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return
	}
	byPID := map[int32]*process.Process{}
	children := map[int32][]int32{}
	var roots []int32
	for _, p := range procs {
		byPID[p.Pid] = p
		ppid, err := p.PpidWithContext(ctx)
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], p.Pid)
		if m.RootName != "" && isNamed(ctx, p, m.RootName) {
			roots = append(roots, p.Pid)
		}
	}
	for _, pid := range m.RootPIDs {
		if _, ok := byPID[pid]; ok {
			roots = append(roots, pid)
		}
	}
	rootSet := map[int32]bool{}
	for _, r := range roots {
		rootSet[r] = true
	}
	members := collect(roots, children)

	elapsed := time.Since(m.last).Seconds()
	next := map[int32]float64{}
	descThisTick := 0.0
	for _, pid := range members {
		p := byPID[pid]
		if p == nil {
			continue
		}
		t, err := p.TimesWithContext(ctx)
		if err != nil {
			continue
		}
		total := t.User + t.System
		next[pid] = total
		if !m.primed || elapsed <= 0 {
			continue
		}
		prev, hadPrev := m.prev[pid]
		d, ok := cpuDelta(total, prev, hadPrev, elapsed, m.cores)
		if !ok {
			continue
		}
		c := d / elapsed
		if rootSet[pid] {
			m.rootSecs += c * elapsed
			continue
		}
		if m.excluded[pid] {
			m.coreSecs[Key{SelfMeasurementBucket, ProcName(ctx, p)}] += c * elapsed
			continue
		}
		m.coreSecs[Key{BucketOf(ProcName(ctx, p), Cmdline(ctx, p)), ProcName(ctx, p)}] += c * elapsed
		descThisTick += c
	}
	if descThisTick > m.peakDesc {
		m.peakDesc = descThisTick
	}
	m.prev = next
	m.last = time.Now()
	m.primed = true
	m.ticks++
}

// Report returns what has been accumulated so far.
func (m *Meter) Report() Report {
	out := map[Key]float64{}
	for k, v := range m.coreSecs {
		out[k] = v
	}
	return Report{
		RootCoreSec:         m.rootSecs,
		CoreSecs:            out,
		PeakDescendantCores: m.peakDesc,
		Ticks:               m.ticks,
		Cores:               m.cores,
	}
}

// BucketOf classifies one descendant by what kind of work it is.
//
// The buckets are the three the pivot turns on, plus the two that would otherwise hide
// inside "other" and be mistaken for one of them. Every process is also reported by name, so
// a reader who disagrees with a call here can re-bucket from the raw table rather than
// having to re-run the measurement.
func BucketOf(name, args string) string {
	a := strings.ToLower(args)
	n := strings.ToLower(name)

	// MCP servers first: the hypothesis under test. A stdio server is launched by the
	// session and IS a descendant; an HTTP or native-host server is not, and the difference
	// is the whole finding.
	if strings.Contains(a, "mcp") || strings.Contains(a, "--chrome-native-host") ||
		strings.Contains(a, "modelcontextprotocol") {
		return "mcp"
	}
	// Compilers, linkers, test runners and the build drivers that spawn them.
	switch n {
	case "go", "cc", "c++", "clang", "clang++", "gcc", "g++", "ld", "as", "compile", "link",
		"cargo", "rustc", "swift", "swiftc", "xcodebuild", "gradle", "java", "javac",
		"make", "cmake", "ninja", "tsc", "jest", "vitest", "pytest", "mypy", "ruff",
		"npm", "npx", "pnpm", "yarn", "uv", "pip", "pip3", "bazel", "dotnet":
		return "build-test"
	}
	if strings.Contains(a, "go-build") || strings.Contains(a, "pytest") ||
		strings.Contains(a, "-m unittest") || strings.Contains(a, "go test") {
		return "build-test"
	}
	// Version control and search: the ordinary tool-call traffic of an agent reading a repo.
	switch n {
	case "git", "rg", "grep", "egrep", "fgrep", "find", "fd", "ag", "sed", "awk", "jq":
		return "vcs-search"
	}
	switch n {
	case "zsh", "bash", "sh", "dash", "fish":
		return "shell"
	}
	return "other"
}

// cpuDelta is internal/signals.cpuDelta, copied rather than exported.
//
// Exporting it would widen a package the seams rule keeps narrow, to serve a tool that is
// not part of the product. The copy is small, and it is the arithmetic being reproduced on
// purpose: a decomposition that used a different rule would not add up to the number it is
// decomposing.
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
		return 0, false
	}
	if delta < 0 {
		return 0, false
	}
	return delta, true
}

func collect(roots []int32, children map[int32][]int32) []int32 {
	seen := map[int32]bool{}
	q := append([]int32(nil), roots...)
	var out []int32
	for len(q) > 0 {
		pid := q[0]
		q = q[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
		q = append(q, children[pid]...)
	}
	return out
}

func isNamed(ctx context.Context, p *process.Process, want string) bool {
	if argv, err := p.CmdlineSliceWithContext(ctx); err == nil && len(argv) > 0 && argv[0] != "" {
		if filepath.Base(argv[0]) == want {
			return true
		}
	}
	n, err := p.NameWithContext(ctx)
	return err == nil && n == want
}

// ProcName is the name a process is reported under: argv[0]'s basename where there is one.
func ProcName(ctx context.Context, p *process.Process) string {
	if argv, err := p.CmdlineSliceWithContext(ctx); err == nil && len(argv) > 0 && argv[0] != "" {
		return filepath.Base(argv[0])
	}
	if n, err := p.NameWithContext(ctx); err == nil {
		return n
	}
	return "?"
}

// Cmdline is the full command line, used only to bucket by argument text.
func Cmdline(ctx context.Context, p *process.Process) string {
	s, err := p.CmdlineWithContext(ctx)
	if err != nil {
		return ""
	}
	return s
}

// ancestryOf returns this process and every ancestor up to a root, so the measurement can
// exclude itself from the tree it is measuring.
func ancestryOf(ctx context.Context, self int32) map[int32]bool {
	out := map[int32]bool{}
	cur := self
	for i := 0; i < 12; i++ {
		out[cur] = true
		p, err := process.NewProcessWithContext(ctx, cur)
		if err != nil {
			break
		}
		ppid, err := p.PpidWithContext(ctx)
		if err != nil || ppid <= 1 {
			break
		}
		cur = ppid
	}
	return out
}
