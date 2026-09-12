// Command childcpu decomposes the descendant half of the Phase 0 CPU measurement.
//
// internal/signals.Sampler reports two numbers per tick: claude_cpu, the cores busy across
// every Claude Code session process AND its transitive descendants, and claude_cpu_roots,
// the cores busy in the session processes alone. The gap between them is the load agents
// caused but did not themselves burn, and over the Phase 0 window it is 47.7% of the total.
//
// Undecomposed that number supports two opposite products. If the descendants are attached
// MCP servers, agents and their children are both cheap and agent count is the wrong control
// variable. If they are compilers and test runners, the right product is a build queue. This
// command answers which by bucketing the same tree the sampler sums, and reporting cores by
// bucket and by process name.
//
// The attribution deliberately mirrors internal/signals/sampler.go rather than improving on
// it: the same argv[0] root rule, the same transitive walk, and the same cpuDelta physical
// bound that keeps a compiler visible for a single tick from scoring zero. A better method
// would measure something the sampler is not measuring, which is not what this is for.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

const rootName = "claude"

// bucketOf classifies one descendant by what kind of work it is.
//
// The buckets are the three the pivot turns on, plus the two that would otherwise hide
// inside "other" and be mistaken for one of them. Every process is also reported by name, so
// a reader who disagrees with a call here can re-bucket from the raw table rather than
// having to re-run the measurement.
func bucketOf(name, args string) string {
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

type key struct{ bucket, name string }

func main() {
	dur := flag.Duration("for", 3*time.Minute, "how long to measure")
	tick := flag.Duration("tick", time.Second, "sampling cadence; 1s matches the Phase 0 sampler")
	label := flag.String("label", "", "label printed with the report")
	flag.Parse()

	ctx := context.Background()
	cores := runtime.NumCPU()

	// Exclude this measurement's own subtree. It runs from a Bash tool call inside a live
	// session, so without this it would be a descendant of a root and would measure itself.
	selfExcluded := ancestryOf(ctx, int32(os.Getpid()))

	coreSecs := map[key]float64{}
	rootSecs := 0.0
	prev := map[int32]float64{}
	primed := false
	last := time.Now()
	ticks := 0
	maxDescCores := 0.0

	deadline := time.Now().Add(*dur)
	for time.Now().Before(deadline) {
		time.Sleep(*tick)
		procs, err := process.ProcessesWithContext(ctx)
		if err != nil {
			continue
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
			if isRoot(ctx, p) {
				roots = append(roots, p.Pid)
			}
		}
		rootSet := map[int32]bool{}
		for _, r := range roots {
			rootSet[r] = true
		}
		members := collect(roots, children)

		elapsed := time.Since(last).Seconds()
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
			if !primed || elapsed <= 0 {
				continue
			}
			d, ok := cpuDelta(total, prev[pid], hasKey(prev, pid), elapsed, cores)
			if !ok {
				continue
			}
			c := d / elapsed
			if rootSet[pid] {
				rootSecs += c * elapsed
				continue
			}
			if selfExcluded[pid] {
				coreSecs[key{"self-measurement", procName(ctx, p)}] += c * elapsed
				continue
			}
			name := procName(ctx, p)
			args := cmdline(ctx, p)
			coreSecs[key{bucketOf(name, args), name}] += c * elapsed
			descThisTick += c
		}
		if descThisTick > maxDescCores {
			maxDescCores = descThisTick
		}
		prev = next
		last = time.Now()
		primed = true
		ticks++
	}

	report(*label, *dur, ticks, cores, rootSecs, maxDescCores, coreSecs)
}

func report(label string, dur time.Duration, ticks, cores int, rootSecs, maxDesc float64, coreSecs map[key]float64) {
	byBucket := map[string]float64{}
	desc := 0.0
	for k, v := range coreSecs {
		if k.bucket == "self-measurement" {
			continue
		}
		byBucket[k.bucket] += v
		desc += v
	}
	if label != "" {
		fmt.Printf("== %s ==\n", label)
	}
	fmt.Printf("window %s, %d ticks, %d cores\n\n", dur, ticks, cores)
	total := rootSecs + desc
	fmt.Printf("%-16s %10s %8s\n", "", "core-sec", "share")
	fmt.Printf("%-16s %10.2f %7.1f%%\n", "session roots", rootSecs, pct(rootSecs, total))
	fmt.Printf("%-16s %10.2f %7.1f%%   <- the number being decomposed\n", "descendants", desc, pct(desc, total))
	fmt.Printf("%-16s %10.2f\n\n", "TOTAL", total)

	fmt.Println("descendants by bucket:")
	type bs struct {
		b string
		v float64
	}
	var bl []bs
	for b, v := range byBucket {
		bl = append(bl, bs{b, v})
	}
	sort.Slice(bl, func(i, j int) bool { return bl[i].v > bl[j].v })
	for _, e := range bl {
		fmt.Printf("  %-14s %10.3f core-sec  %6.1f%% of descendants  %5.1f%% of tree\n",
			e.b, e.v, pct(e.v, desc), pct(e.v, total))
	}
	if len(bl) == 0 {
		fmt.Println("  (none measured)")
	}

	fmt.Println("\ndescendants by process name:")
	type ks struct {
		k key
		v float64
	}
	var kl []ks
	for k, v := range coreSecs {
		kl = append(kl, ks{k, v})
	}
	sort.Slice(kl, func(i, j int) bool { return kl[i].v > kl[j].v })
	for _, e := range kl {
		if e.v <= 0 {
			continue
		}
		fmt.Printf("  %-14s %-22s %9.3f core-sec\n", e.k.bucket, e.k.name, e.v)
	}
	fmt.Printf("\npeak descendant cores in any single tick: %.2f\n", maxDesc)
}

func pct(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * a / b
}

func hasKey(m map[int32]float64, k int32) bool { _, ok := m[k]; return ok }

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

func isRoot(ctx context.Context, p *process.Process) bool {
	if argv, err := p.CmdlineSliceWithContext(ctx); err == nil && len(argv) > 0 && argv[0] != "" {
		if filepath.Base(argv[0]) == rootName {
			return true
		}
	}
	n, err := p.NameWithContext(ctx)
	return err == nil && n == rootName
}

func procName(ctx context.Context, p *process.Process) string {
	if argv, err := p.CmdlineSliceWithContext(ctx); err == nil && len(argv) > 0 && argv[0] != "" {
		return filepath.Base(argv[0])
	}
	if n, err := p.NameWithContext(ctx); err == nil {
		return n
	}
	return "?"
}

func cmdline(ctx context.Context, p *process.Process) string {
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
