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
// The walk itself lives in internal/childcpu, so that tools/sweep takes the same measurement
// through the same code rather than through a second implementation of it (GOV-31 clause 1).
// This file is the command-line face of that package and nothing else.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/peter3605/claude-governor/internal/childcpu"
)

func main() {
	dur := flag.Duration("for", 3*time.Minute, "how long to measure")
	tick := flag.Duration("tick", time.Second, "sampling cadence; 1s matches the Phase 0 sampler")
	label := flag.String("label", "", "label printed with the report")
	root := flag.String("root", childcpu.DefaultRootName, "process name that roots the tree")
	flag.Parse()

	ctx := context.Background()

	// Exclude this measurement's own subtree. It runs from a Bash tool call inside a live
	// session, so without this it would be a descendant of a root and would measure itself.
	m := childcpu.New(ctx, childcpu.Meter{
		RootName:         *root,
		ExcludeSubtreeOf: int32(os.Getpid()),
	})

	deadline := time.Now().Add(*dur)
	for time.Now().Before(deadline) {
		time.Sleep(*tick)
		m.Tick(ctx)
	}

	report(*label, *dur, m.Report())
}

func report(label string, dur time.Duration, r childcpu.Report) {
	byBucket := r.ByBucket()
	desc := r.DescendantCoreSec()
	if label != "" {
		fmt.Printf("== %s ==\n", label)
	}
	fmt.Printf("window %s, %d ticks, %d cores\n\n", dur, r.Ticks, r.Cores)
	total := r.RootCoreSec + desc
	fmt.Printf("%-16s %10s %8s\n", "", "core-sec", "share")
	fmt.Printf("%-16s %10.2f %7.1f%%\n", "session roots", r.RootCoreSec, pct(r.RootCoreSec, total))
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
		k childcpu.Key
		v float64
	}
	var kl []ks
	for k, v := range r.CoreSecs {
		kl = append(kl, ks{k, v})
	}
	sort.Slice(kl, func(i, j int) bool { return kl[i].v > kl[j].v })
	for _, e := range kl {
		if e.v <= 0 {
			continue
		}
		fmt.Printf("  %-14s %-22s %9.3f core-sec\n", e.k.Bucket, e.k.Name, e.v)
	}
	fmt.Printf("\npeak descendant cores in any single tick: %.2f\n", r.PeakDescendantCores)
}

func pct(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * a / b
}
