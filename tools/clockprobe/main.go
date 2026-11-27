// Command clockprobe reads the clock this Mac's P-cores are running at, without sudo (GOV-B54).
//
// GOV-B54 measured, with powermetrics run by hand as a one-off instrument, that this M1 Air's
// P-cluster falls from 2988 MHz to about 1700 under saturating load while thermalState stays
// nominal for 24 minutes and never reaches the serious level the budget cuts on. CPU
// utilisation and the run queue cannot see it either: a throttled core is still a busy core.
// This is the candidate sudo-free signal its Done-when clause 2 asks to be evaluated. It is a
// tool and is wired into nothing; clause 4 leaves wiring a signal to a follow-up story.
//
// # Result: it missed (design.md §5.3)
//
// Its readings are right whenever it runs on a P-core, but under saturating load it reached
// one in 1% of ticks: the scheduler keeps a once-a-second thread on the E-cluster while the
// P-cores are busy, whatever its QoS, and macOS has no unprivileged affinity call. It is kept
// as the instrument that measured this, not as a signal to wire in.
//
// # How it reads the clock
//
// Once a tick, on a thread at user-interactive QoS, it spins briefly (which gets the thread
// moved to a P-core; without it, a thread that wakes once a second is left on an E-core) and
// then times pairs of fixed blocks in THREAD CPU time:
//
//   - the chain: dependent integer multiply-adds at a fixed 3 cycles an iteration on both M1
//     core types, so MHz = cycles / thread-CPU-nanoseconds * 1000. An absolute reading, not a
//     ratio against a start-up baseline that could itself have been taken throttled.
//   - the wide block: independent FP adds over sixteen lanes. A P-core has twice an E-core's FP
//     pipes, so the ratio wide/chain says which kind of core ran the pair whatever its clock.
//
// Thread CPU time removes scheduling contention: time preempted by the load is not counted.
// Cache and memory-bandwidth contention it cannot remove, which is why the kernels touch only
// registers.
//
// # Calibration
//
// At start-up it measures the ratio on each kind of core: at background QoS, which Apple
// silicon confines to the E-cluster, and at user-interactive QoS after a spin. Readings are
// then labelled P, E or ? by the nearer ratio. The E-core's implied clock is printed as a
// self-check of the cycles-per-iteration constant: on an M1 it must read about 2064 MHz, the
// E-cluster's cap, and on another chip it says the constant needs remeasuring.
//
// # Output
//
// One JSON object per line on stdout: a "calibration" record, then a "probe" record a tick.
// mhz is the implied clock of the chosen pair: the fastest pair labelled P, else the fastest
// pair. cost is the probe process's CPU over the last tick as a fraction of one core, the
// number to hold against the sampler's 1%-of-a-core budget.
//
//	go run ./tools/clockprobe -for 35m > ~/gov-b54/probe-A2.jsonl
//
// Start it on a quiet machine, so calibration is not competing with the load.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"syscall"
	"time"
)

type calibration struct {
	Kind      string    `json:"kind"`
	At        time.Time `json:"ts"`
	ChainN    uint64    `json:"chain_n"`
	WideN     uint64    `json:"wide_n"`
	Cycles    float64   `json:"cycles_per_iter"`
	Warm      string    `json:"warm"`
	PRatio    float64   `json:"p_ratio"`
	PMHz      float64   `json:"p_mhz"`
	ERatio    float64   `json:"e_ratio"`
	EMHz      float64   `json:"e_mhz"`
	Separable bool      `json:"separable"`
	Threshold float64   `json:"threshold"`
}

type reading struct {
	Kind    string    `json:"kind"`
	At      time.Time `json:"ts"`
	ChainNs int64     `json:"chain_ns"`
	WideNs  int64     `json:"wide_ns"`
	Ratio   float64   `json:"ratio"`
	Core    string    `json:"core"`
	MHz     float64   `json:"mhz"`
	PPairs  int       `json:"p_pairs"`
	Pairs   int       `json:"pairs"`
	Cost    float64   `json:"cost"`
}

// minSeparation is how far apart, as a factor, the two calibrated ratios must be before any
// reading is labelled. Measured on the M1 they are 2x apart; closer than this, a label would be
// a guess, so every reading is "?" and the calibration record says so.
const minSeparation = 1.4

type pair struct{ chainNs, wideNs int64 }

func (p pair) ratio() float64 { return float64(p.wideNs) / float64(p.chainNs) }

func mhz(chainN uint64, chainNs int64, cycles float64) float64 {
	if chainNs <= 0 {
		return 0
	}
	return cycles * float64(chainN) / float64(chainNs) * 1000
}

// classify labels a ratio by which calibrated ratio it is nearer, on a log scale.
func classify(ratio float64, c calibration) string {
	if !c.Separable || ratio <= 0 {
		return "?"
	}
	if (ratio < c.Threshold) == (c.PRatio < c.ERatio) {
		return "P"
	}
	return "E"
}

func newCalibration(chainN, wideN uint64, cycles float64, warm time.Duration, p, e []pair) calibration {
	c := calibration{Kind: "calibration", At: time.Now().UTC(), ChainN: chainN, WideN: wideN, Cycles: cycles, Warm: warm.String()}
	if len(p) == 0 || len(e) == 0 {
		return c
	}
	// Background QoS cannot leave the E-cluster, so its median is the E-core. Interactive QoS
	// can still land a pair on an E-core, so the P-core is its lower quartile: the P ratio is
	// the smaller one by construction (twice the FP pipes), and a stray E pair only ever
	// pushes a reading up.
	c.ERatio = quantile(ratios(e), 0.5)
	c.PRatio = quantile(ratios(p), 0.25)
	c.EMHz = mhz(chainN, medianChain(e), cycles)
	c.PMHz = mhz(chainN, minChainBelow(p, math.Sqrt(c.PRatio*c.ERatio)), cycles)
	lo, hi := math.Min(c.PRatio, c.ERatio), math.Max(c.PRatio, c.ERatio)
	c.Separable = lo > 0 && hi/lo >= minSeparation
	if c.Separable {
		c.Threshold = math.Sqrt(lo * hi)
	}
	return c
}

// choose picks the pair a tick reports: the fastest chain among pairs labelled P, because the
// question is the P-cluster's clock and every source of error only adds time; failing any P
// pair, the fastest pair of all, labelled for what it was.
func choose(ps []pair, c calibration) (pair, string, int) {
	var best pair
	bestCore, nP := "", 0
	for _, p := range ps {
		core := classify(p.ratio(), c)
		if core == "P" {
			nP++
		}
		better := best.chainNs == 0 ||
			(core == "P" && bestCore != "P") ||
			((core == "P") == (bestCore == "P") && p.chainNs < best.chainNs)
		if better {
			best, bestCore = p, core
		}
	}
	return best, bestCore, nP
}

func ratios(ps []pair) []float64 {
	r := make([]float64, len(ps))
	for i, p := range ps {
		r[i] = p.ratio()
	}
	return r
}

func medianChain(ps []pair) int64 {
	c := make([]float64, len(ps))
	for i, p := range ps {
		c[i] = float64(p.chainNs)
	}
	return int64(quantile(c, 0.5))
}

func minChainBelow(ps []pair, threshold float64) int64 {
	var m int64
	for _, p := range ps {
		if p.ratio() < threshold && (m == 0 || p.chainNs < m) {
			m = p.chainNs
		}
	}
	return m
}

func quantile(a []float64, q float64) float64 {
	if len(a) == 0 {
		return 0
	}
	c := append([]float64(nil), a...)
	sort.Float64s(c)
	return c[int(q*float64(len(c)-1)+0.5)]
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

type options struct {
	chainN, wideN uint64
	cycles        float64
	pairs, rounds int
	warm          time.Duration
	interval, dur time.Duration
}

func main() {
	var o options
	flag.Uint64Var(&o.chainN, "n", 1<<19, "chain iterations per pair; about half a millisecond on a full-speed M1 P-core")
	flag.Uint64Var(&o.wideN, "wide", 1<<19, "FP adds per wide block")
	flag.Float64Var(&o.cycles, "cycles", 3, "cycles per chain iteration (3 on both M1 core types, measured)")
	flag.IntVar(&o.pairs, "pairs", 3, "pairs timed per tick")
	flag.IntVar(&o.rounds, "calib", 20, "calibration pairs per core type")
	flag.DurationVar(&o.warm, "warm", 2*time.Millisecond, "spin before each tick's pairs, to be moved to a P-core")
	flag.DurationVar(&o.interval, "interval", time.Second, "time between readings")
	flag.DurationVar(&o.dur, "for", 35*time.Minute, "how long to run after calibrating")
	flag.Parse()
	if err := run(o, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "clockprobe:", err)
		os.Exit(1)
	}
}

func sample(o options, n int) ([]pair, error) {
	spin(o.warm)
	ps := make([]pair, 0, n)
	for i := 0; i < n; i++ {
		c, w, err := timePair(o.chainN, o.wideN)
		if err != nil {
			return nil, err
		}
		ps = append(ps, pair{c, w})
	}
	return ps, nil
}

func calibrateOn(o options, interactive bool) ([]pair, error) {
	if err := setQoS(interactive); err != nil {
		return nil, err
	}
	var all []pair
	for i := 0; i < o.rounds; i++ {
		ps, err := sample(o, 1)
		if err != nil {
			return nil, err
		}
		all = append(all, ps...)
		time.Sleep(100 * time.Millisecond)
	}
	return all, nil
}

func run(o options, out, logw io.Writer) error {
	if !supported {
		return fmt.Errorf("unsupported on %s/%s (needs darwin with cgo)", runtime.GOOS, runtime.GOARCH)
	}
	// Every QoS change and every timing must happen on one OS thread, or the QoS is set on one
	// thread and the work timed on another.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	e, err := calibrateOn(o, false)
	if err != nil {
		return err
	}
	p, err := calibrateOn(o, true)
	if err != nil {
		return err
	}
	cal := newCalibration(o.chainN, o.wideN, o.cycles, o.warm, p, e)
	enc := json.NewEncoder(out)
	if err := enc.Encode(cal); err != nil {
		return err
	}
	fmt.Fprintf(logw, "calibrated: P ratio %.3f (%.0f MHz), E ratio %.3f (%.0f MHz, expect ~2064 on an M1), separable %v\n",
		cal.PRatio, cal.PMHz, cal.ERatio, cal.EMHz, cal.Separable)

	end := time.Now().Add(o.dur)
	lastWall, lastCPU := time.Now(), processCPU()
	tick := time.NewTicker(o.interval)
	defer tick.Stop()
	for now := range tick.C {
		if now.After(end) {
			return nil
		}
		ps, err := sample(o, o.pairs)
		if err != nil {
			return err
		}
		best, core, nP := choose(ps, cal)
		cpu, wall := processCPU(), time.Now()
		r := reading{
			Kind: "probe", At: wall.UTC(), ChainNs: best.chainNs, WideNs: best.wideNs, Ratio: best.ratio(),
			Core: core, MHz: mhz(o.chainN, best.chainNs, o.cycles), PPairs: nP, Pairs: len(ps),
		}
		if d := wall.Sub(lastWall); d > 0 {
			r.Cost = float64(cpu-lastCPU) / float64(d)
		}
		lastWall, lastCPU = wall, cpu
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
