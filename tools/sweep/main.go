// Command sweep measures what a run-queue cut threshold costs and what it buys.
//
// GOV-31 clause 1: the governed-vs-ungoverned fan-out of docs/control-proof.md, turned into
// something a person who was not there can re-run, taking the threshold as a parameter.
// GOV-15 ran that experiment by hand through `claude -p` and wrote the numbers into a
// document; nothing was committed that would let anyone reproduce it, which is why the
// threshold it diagnosed could not simply be corrected in the same story.
//
// # What it does
//
// For each arm — one per -thresholds value, plus an ungoverned reference — it settles the
// machine, clears the build cache, starts an isolated daemon at that threshold, then fires a
// fixed batch of build requests through the REAL gate binary at the REAL daemon and measures
// the machine for a fixed window. Per arm it reports builds admitted, build-test
// core-seconds through internal/childcpu, run-queue-per-core p50/p90/max, and the wall time
// to drain the batch.
//
// # What it deliberately does NOT do
//
// IT DOES NOT DRIVE A CLAUDE SESSION, and that is the single biggest difference from
// GOV-15's run. The load here is launched by this process and admitted through the same
// `governor gate` binary Claude Code would invoke, with the same PreToolUse payload shape
// measured in docs/hook-surface.md section 19. What is lost is the model's own behaviour;
// what is gained is that both of GOV-15's experiment traps stop existing rather than having
// to be worked around — `claude -p` cannot kill the load early because there is no session
// to exit, and the model cannot stop on a refusal because there is no model. Those two traps
// cost GOV-15 a discarded run and made its first comparison worthless. A sweep of four
// thresholds would have had four chances to hit them.
//
// The cost of that choice is stated in the method section of docs/run-queue-sweep.md rather
// than hidden here: this measures the CONTROLLER against build load, not the end-to-end
// product against an agent. GOV-15 already measured the latter once, and its finding — that
// the governor controls — is not what this story reopens.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/childcpu"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sweep: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	thresholds   []float64
	ungoverned   bool
	builds       int
	modules      int
	funcs        int
	window       time.Duration
	retry        time.Duration
	settleBelow  float64
	settleFor    time.Duration
	settleWait   time.Duration
	governor     string
	workdir      string
	out          string
	keepWorkdir  bool
	buildCommand string
	reverse      bool
	canary       bool
	concurrency  []int
	recovery     int
}

func run() error {
	var (
		thresholds  = flag.String("thresholds", "1.2,2.0,3.0,4.5", "comma-separated run-queue-per-core thresholds to measure; 1.2 is the shipped value")
		ungoverned  = flag.Bool("ungoverned", true, "also measure an arm with no gate at all, as the reference")
		builds      = flag.Int("builds", 10, "build requests in the batch")
		modules     = flag.Int("modules", 8, "generated Go modules the builds compile")
		funcs       = flag.Int("funcs", 9000, "functions per generated module")
		window      = flag.Duration("window", 150*time.Second, "measurement window; every arm gets the same one")
		retry       = flag.Duration("retry", 5*time.Second, "how long a refused request waits before asking again; 0 means never retry")
		below       = flag.Float64("settle-below", 0.8, "run-queue per core the machine must be under before an arm starts")
		settleFor   = flag.Duration("settle-for", 5*time.Second, "how long it must stay under it")
		settleWait  = flag.Duration("settle-wait", 5*time.Minute, "give up settling after this long")
		governor    = flag.String("governor", "bin/governor", "the governor binary under test")
		workdir     = flag.String("workdir", "", "where to generate the workload; default is a temp directory")
		out         = flag.String("out", "", "write the raw results as JSON here as well as to stdout")
		keep        = flag.Bool("keep-workdir", false, "do not delete the generated workload on exit")
		buildCmd    = flag.String("build-command", "go build -a ./...", "the command one build request runs")
		concurrency = flag.String("concurrency", "", "instead of sweeping thresholds, measure the run queue produced by each of these concurrent build counts, ungoverned")
		reverse     = flag.Bool("reverse", false, "run the arms in reverse order, to separate the threshold's effect from the arm order's")
		canary      = flag.Bool("canary", true, "time one build alone before each arm, as a check on whether the machine itself is drifting")
		recovery    = flag.Int("recovery", 0, "instead of a fan-out, hold this many builds running continuously and record what the daemon's budget does; the measurement of whether the budget can climb while a build runs")
	)
	flag.Parse()

	o := options{
		ungoverned: *ungoverned, builds: *builds, modules: *modules, funcs: *funcs,
		window: *window, retry: *retry, settleBelow: *below, settleFor: *settleFor,
		settleWait: *settleWait, workdir: *workdir, out: *out, keepWorkdir: *keep,
		buildCommand: *buildCmd, reverse: *reverse, canary: *canary, recovery: *recovery,
	}
	for _, f := range strings.Split(*concurrency, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 {
			return fmt.Errorf("bad concurrency %q: want a positive whole number", f)
		}
		o.concurrency = append(o.concurrency, n)
	}
	for _, f := range strings.Split(*thresholds, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || v <= 0 {
			return fmt.Errorf("bad threshold %q: want a positive number", f)
		}
		o.thresholds = append(o.thresholds, v)
	}
	if len(o.thresholds) == 0 && !o.ungoverned && len(o.concurrency) == 0 && o.recovery == 0 {
		return fmt.Errorf("nothing to measure: pass -thresholds, -concurrency, -recovery, or -ungoverned")
	}
	abs, err := filepath.Abs(*governor)
	if err != nil {
		return err
	}
	o.governor = abs
	if _, err := os.Stat(o.governor); err != nil {
		return fmt.Errorf("governor binary %s: %w (run `make build` first)", o.governor, err)
	}

	ctx := context.Background()
	work, cleanup, err := prepareWorkload(ctx, o)
	if err != nil {
		return err
	}
	defer cleanup()

	res := results{
		Host:         describeHost(),
		StartedAt:    time.Now().Format(time.RFC3339),
		Builds:       o.builds,
		Modules:      o.modules,
		Funcs:        o.funcs,
		Window:       o.window.String(),
		Retry:        o.retry.String(),
		BuildCommand: o.buildCommand,
	}

	if o.recovery > 0 {
		return runRecovery(ctx, o, work, &res)
	}
	if len(o.concurrency) > 0 {
		return runCurve(ctx, o, work, &res)
	}

	// ARM ORDER IS A CONFOUND AND -reverse IS HOW IT IS SEPARATED FROM THE THRESHOLD.
	//
	// The first run of this sweep produced build CPU falling monotonically — 323.6, 212.5,
	// 193.7, 147.3 core-seconds — as the threshold ROSE, which is backwards from the
	// mechanism: a more permissive threshold cannot admit less work. What it tracked was the
	// order the arms ran in, on a fanless laptop that had been compiling flat out for half an
	// hour. Thermal and clock drift across a long sweep is confounded with arm order, and a
	// fixed order cannot tell the two apart.
	//
	// So the order is a parameter. Running the sweep both ways and comparing is what settles
	// whether an effect belongs to the threshold or to the clock: an effect that follows the
	// threshold reverses with it, an effect that follows the machine does not.
	var plan []arm
	for _, t := range o.thresholds {
		plan = append(plan, arm{
			Name:        fmt.Sprintf("RunQueueMax=%g", t),
			Governed:    true,
			RunQueueMax: t,
		})
	}
	// The ungoverned arm is the noisiest and the one most likely to leave the machine hot,
	// so it sits at the head of the forward order and the tail of the reverse one.
	if o.ungoverned {
		plan = append([]arm{{Name: "ungoverned", Governed: false}}, plan...)
	}
	if o.reverse {
		for i, j := 0, len(plan)-1; i < j; i, j = i+1, j-1 {
			plan[i], plan[j] = plan[j], plan[i]
		}
	}
	res.Order = "forward"
	if o.reverse {
		res.Order = "reverse"
	}
	for i := range plan {
		plan[i].Position = i + 1
		a, err := runArm(ctx, o, work, plan[i])
		if err != nil {
			// THE ARMS THAT DID FINISH ARE WRITTEN OUT ANYWAY, and the reason is a run that
			// was lost. A sweep's last arm failed to settle — the previous arm had left the
			// machine at loadavg 243 and five minutes was not enough decay — and because the
			// results were serialised only after the whole plan completed, the completed arm's
			// four minutes of measurement went with it. An arm is expensive and irreproducible;
			// the failure that follows it is neither.
			if werr := writeResults(o, res); werr != nil {
				fmt.Fprintf(os.Stderr, "sweep: also failed to write partial results: %v\n", werr)
			}
			return err
		}
		res.Arms = append(res.Arms, a)
		printArm(a)
		// Written after every arm, not only at the end, for the same reason.
		if err := writeResults(o, res); err != nil {
			return err
		}
	}

	fmt.Println()
	printTable(res)
	return writeResults(o, res)
}

// writeResults serialises what has been measured so far. It is a no-op without -out.
func writeResults(o options, res results) error {
	if o.out == "" {
		return nil
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------------

type results struct {
	Host         string `json:"host"`
	StartedAt    string `json:"started_at"`
	Builds       int    `json:"builds"`
	Modules      int    `json:"modules"`
	Funcs        int    `json:"funcs_per_module"`
	Window       string `json:"window"`
	Retry        string `json:"retry"`
	BuildCommand string `json:"build_command"`
	Order        string `json:"order"`
	Arms         []arm  `json:"arms"`
}

type arm struct {
	Name        string  `json:"name"`
	Governed    bool    `json:"governed"`
	RunQueueMax float64 `json:"run_queue_max,omitempty"`
	// Position is where this arm fell in the running order, counting from 1. It is recorded
	// because the first run of this sweep found an effect that tracked position rather than
	// threshold; anything that correlates with it is suspect.
	Position int `json:"position"`
	// CanarySeconds is the wall time of ONE build run alone, immediately before this arm.
	// It is the machine's own speed at that moment, and it is the direct check on drift:
	// if it grows down the table, later arms were measured on a slower machine and the
	// comparison across arms is not clean.
	CanarySeconds float64 `json:"canary_seconds"`

	// Admitted is how many build requests the gate let through. With -retry non-zero a
	// single request can be refused several times and admitted once; this counts admissions,
	// not requests.
	Admitted int `json:"admitted"`
	// Refusals is how many times the gate said no across the whole arm.
	Refusals int `json:"refusals"`
	// Completed is how many of the batch's builds actually finished inside the window.
	Completed int `json:"completed"`
	// DrainSeconds is the wall time from the first request to the last build finishing, or
	// -1 if the batch did not drain inside the window. This is the throughput half of the
	// trade clause 3 asks about.
	DrainSeconds float64 `json:"drain_seconds"`

	BuildTestCoreSec float64 `json:"build_test_core_sec"`
	PeakChildCores   float64 `json:"peak_child_cores"`

	// The reported percentiles cover only the samples taken while the batch was still
	// outstanding. See runQueue for why the whole-window series would invert the result.
	RunQueueP50         float64 `json:"run_queue_p50"`
	RunQueueP90         float64 `json:"run_queue_p90"`
	RunQueueMaxObserved float64 `json:"run_queue_max_observed"`
	RunQueueSamples     int     `json:"run_queue_samples"`

	// The same statistics over the whole fixed window, reported so the dilution is visible
	// rather than taken on trust.
	WindowRunQueueP50     float64 `json:"window_run_queue_p50"`
	WindowRunQueueP90     float64 `json:"window_run_queue_p90"`
	WindowRunQueueMax     float64 `json:"window_run_queue_max"`
	WindowRunQueueSamples int     `json:"window_run_queue_samples"`

	// CPUP50 and CPUAboveCut are the other half of the cut condition: host utilisation over
	// the busy window, and the share of those ticks already above Setpoint+SetpointBand.
	// A value near 1.0 means the run-queue threshold decided nothing on this arm.
	CPUP50      float64 `json:"cpu_p50"`
	CPUAboveCut float64 `json:"cpu_share_above_cut"`

	// CutShare and ClimbShare are budget.Cutting and budget.Climbing evaluated over the
	// busy window on the SMOOTHED CPU series — the loop's own conditions, over what the loop
	// would actually have seen.
	//
	// CLIMBSHARE IS THE NUMBER GOV-32 CLAUSE 1 ASKS FOR. GOV-31 measured the cut side at
	// 72-99% and left the increase side unmeasured, which is what let "the budget cannot
	// climb back" stand as a reading of the code rather than as a measurement.
	CPUEWMAP50 float64 `json:"cpu_ewma_p50"`
	CutShare   float64 `json:"cut_condition_share"`
	ClimbShare float64 `json:"climb_condition_share"`
	// DroppedTicks is busy ticks discarded because host CPU would not read; see
	// runQueue.sample. Non-zero here means the shares cover fewer ticks than the run-queue
	// percentiles do, and it is reported rather than silently absorbed.
	DroppedTicks int `json:"dropped_ticks,omitempty"`

	// Series is the per-tick busy window, kept so that a later story can evaluate a NEW
	// condition over this arm without re-running it.
	//
	// GOV-32 had to re-run GOV-31's entire sweep to answer a one-line question about the
	// same load, because pass1.json and pass2.json recorded only summary statistics. A
	// fan-out arm costs four minutes of a settled machine and cannot be reconstructed
	// afterwards; the summary it collapses to costs nothing to keep in full.
	Series *series `json:"series,omitempty"`

	SettleSeconds float64 `json:"settle_seconds"`
	WindowSeconds float64 `json:"window_seconds"`

	// DaemonLog is the governed arm's budget event log, kept verbatim so the trajectory of
	// the loop can be read back rather than reasoned about from the outside.
	DaemonLog string `json:"daemon_log,omitempty"`

	// Budgets is the daemon's budget read once per second, and BudgetMin/Max/Final summarise
	// it. Recorded by -recovery only; the fan-out arms leave it empty.
	//
	// IT IS READ FROM `governor status` RATHER THAN INFERRED FROM THE EVENT LOG, because the
	// log only prints a budget when something happens — a park, a denial, a new peak — so a
	// budget that changed quietly between events is invisible in it. GOV-32 spent a
	// measurement discovering that two arms with identical event logs could not be told apart.
	Budgets     []int `json:"budgets,omitempty"`
	BudgetMin   int   `json:"budget_min,omitempty"`
	BudgetMax   int   `json:"budget_max,omitempty"`
	BudgetFinal int   `json:"budget_final,omitempty"`
}

// series is one arm's busy window, tick by tick, at 1 Hz.
//
// RunQueue and CPU are index-aligned by construction (runQueue.sample drops a tick from both
// or neither), so entry i of each is one moment on the machine. CPUEWMA is CPU after the 5 s
// window the daemon applies, and is the series any condition of the loop's should be
// evaluated against.
type series struct {
	RunQueue []float64 `json:"run_queue"`
	CPU      []float64 `json:"cpu_raw"`
	CPUEWMA  []float64 `json:"cpu_ewma"`
}

func printArm(a arm) {
	drain := "did not drain"
	if a.DrainSeconds >= 0 {
		drain = fmt.Sprintf("%.1fs", a.DrainSeconds)
	}
	fmt.Printf("  -> admitted %d, refusals %d, completed %d, build CPU %.1f core-sec, "+
		"run queue p50 %.2f p90 %.2f max %.2f, drain %s\n"+
		"     cut condition on %.0f%% of busy ticks, climb condition on %.0f%%\n\n",
		a.Admitted, a.Refusals, a.Completed, a.BuildTestCoreSec,
		a.RunQueueP50, a.RunQueueP90, a.RunQueueMaxObserved, drain,
		100*a.CutShare, 100*a.ClimbShare)
}

func printTable(r results) {
	fmt.Printf("order: %s\n\n", r.Order)
	fmt.Printf("%-18s %4s %8s %9s %12s %8s %8s %8s %8s %7s %7s %9s\n",
		"arm", "pos", "canary s", "refusals", "build core-s",
		"rq p50", "rq p90", "rq max", "cpu p50", "cut", "climb", "drain s")
	for _, a := range r.Arms {
		drain := "-"
		if a.DrainSeconds >= 0 {
			drain = fmt.Sprintf("%.1f", a.DrainSeconds)
		}
		fmt.Printf("%-18s %4d %8.1f %9d %12.1f %8.2f %8.2f %8.2f %8.2f %6.0f%% %6.0f%% %9s\n",
			a.Name, a.Position, a.CanarySeconds, a.Refusals, a.BuildTestCoreSec,
			a.RunQueueP50, a.RunQueueP90, a.RunQueueMaxObserved,
			a.CPUP50, 100*a.CutShare, 100*a.ClimbShare, drain)
	}
}

// ---------------------------------------------------------------------------
// One arm
// ---------------------------------------------------------------------------

func runArm(ctx context.Context, o options, work *workload, a arm) (arm, error) {
	fmt.Printf("== %s ==\n", a.Name)

	settled, err := settle(ctx, o)
	if err != nil {
		return a, err
	}
	a.SettleSeconds = settled.Seconds()

	// THE CANARY IS RUN BEFORE THE CACHE IS CLEARED, not after, so that it is not itself
	// warming the cache the arm is supposed to start cold against. It runs on a module the
	// batch never touches, alone, on a machine that has just been declared settled — so its
	// wall time is a reading of how fast this machine is RIGHT NOW, and nothing else.
	if o.canary && len(work.modules) > 0 {
		t0 := time.Now()
		runBuild(ctx, o, work.canary)
		a.CanarySeconds = time.Since(t0).Seconds()
		fmt.Printf("  canary build %.1fs\n", a.CanarySeconds)
	}

	// A cold cache on every arm, so the arms differ in the threshold and not in how much of
	// the compile was already done. `go build -a` rebuilds regardless, but the clean also
	// clears whatever the PREVIOUS arm left warm in the module cache and the linker cache.
	if err := work.cleanCache(ctx); err != nil {
		return a, err
	}

	var dm *daemon
	if a.Governed {
		dm, err = startDaemon(ctx, o.governor, a.RunQueueMax)
		if err != nil {
			return a, err
		}
		// Stopped explicitly at the end of the arm so its log can be kept; this defer is
		// only the safety net for an early return, and stop is idempotent enough for both.
		defer func() { _ = dm.stop() }()
	}

	wctx, cancel := context.WithTimeout(ctx, o.window)
	defer cancel()

	meter := childcpu.New(wctx, childcpu.Meter{RootPIDs: []int32{int32(os.Getpid())}})

	// outstanding is how many of the batch's requests have neither completed nor given up.
	// It gates the run-queue series the report uses; see runQueue for why that matters.
	var outstanding atomic.Int64
	outstanding.Store(int64(o.builds))
	var rq runQueue
	var mu sync.Mutex
	sampling := make(chan struct{})
	go func() {
		defer close(sampling)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				mu.Lock()
				meter.Tick(wctx)
				rq.sample(outstanding.Load() > 0)
				mu.Unlock()
			}
		}
	}()

	start := time.Now()
	var admitted, refusals, completed atomic.Int64
	var lastDone atomic.Int64 // nanoseconds since start

	var wg sync.WaitGroup
	for i := 0; i < o.builds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mod := work.modules[i%len(work.modules)]
			done := false
			defer func() {
				if !done {
					outstanding.Add(-1)
				}
			}()
			for {
				if wctx.Err() != nil {
					return
				}
				ok := true
				var release func()
				if dm != nil {
					ok, release = dm.acquire(wctx, o, i)
				}
				if !ok {
					refusals.Add(1)
					if o.retry <= 0 {
						return
					}
					select {
					case <-wctx.Done():
						return
					case <-time.After(o.retry):
						continue
					}
				}
				admitted.Add(1)
				runBuild(wctx, o, mod)
				if release != nil {
					release()
				}
				completed.Add(1)
				lastDone.Store(int64(time.Since(start)))
				outstanding.Add(-1)
				done = true
				return
			}
		}(i)
	}

	// Every arm is held open for the WHOLE window even if its batch drained early, so the
	// run-queue percentiles and the core-second totals cover identical spans. Comparing a
	// 40-second arm's p90 against a 150-second arm's p90 would be comparing how long each
	// was busy, not how contended it was.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		<-wctx.Done()
	case <-wctx.Done():
		wg.Wait()
	}
	<-sampling

	if dm != nil {
		a.DaemonLog = dm.stop()
	}

	mu.Lock()
	rep := meter.Report()
	p50, p90, max := stats(rq.busy)
	n := len(rq.busy)
	wp50, wp90, wmax := stats(rq.all)
	wn := len(rq.all)
	cpuP50, _, _ := stats(rq.cpu)
	above := 0
	for _, c := range rq.cpu {
		if c > budget.Setpoint+budget.SetpointBand {
			above++
		}
	}
	cpuShare := 0.0
	if len(rq.cpu) > 0 {
		cpuShare = float64(above) / float64(len(rq.cpu))
	}
	smP50, _, _ := stats(rq.smoothed)
	cutShare, climbShare, _ := rq.conditions(armRunQueueMax(a), armClimbRunQueue(a))
	dropped := rq.dropped
	ser := &series{
		RunQueue: append([]float64(nil), rq.busy...),
		CPU:      append([]float64(nil), rq.cpu...),
		CPUEWMA:  append([]float64(nil), rq.smoothed...),
	}
	mu.Unlock()

	a.Admitted = int(admitted.Load())
	a.Refusals = int(refusals.Load())
	a.Completed = int(completed.Load())
	a.DrainSeconds = -1
	if a.Completed == o.builds {
		a.DrainSeconds = time.Duration(lastDone.Load()).Seconds()
	}
	a.BuildTestCoreSec = rep.BuildTestCoreSec()
	a.PeakChildCores = rep.PeakDescendantCores
	a.RunQueueP50, a.RunQueueP90, a.RunQueueMaxObserved = p50, p90, max
	a.RunQueueSamples = n
	a.WindowRunQueueP50, a.WindowRunQueueP90, a.WindowRunQueueMax = wp50, wp90, wmax
	a.WindowRunQueueSamples = wn
	a.CPUP50, a.CPUAboveCut = cpuP50, cpuShare
	a.CPUEWMAP50, a.CutShare, a.ClimbShare = smP50, cutShare, climbShare
	a.DroppedTicks = dropped
	a.Series = ser
	a.WindowSeconds = time.Since(start).Seconds()
	return a, nil
}

// armRunQueueMax is the threshold this arm's condition shares are evaluated against.
//
// An ungoverned arm has no threshold of its own, and the shipped constant is used. The
// question that answers — "what would the loop have done on this load" — is the only reading
// of an ungoverned arm that says anything about the controller rather than about the machine.
func armRunQueueMax(a arm) float64 {
	if a.RunQueueMax > 0 {
		return a.RunQueueMax
	}
	return budget.RunQueueMax
}

// armClimbRunQueue is the level this arm's loop climbs at: one band below where it cuts, which
// is how internal/budget derives it so that sweeping the cut threshold moves both together.
func armClimbRunQueue(a arm) float64 {
	at := armRunQueueMax(a) - budget.RunQueueBand
	if at <= 0 {
		at = armRunQueueMax(a) / 2
	}
	return at
}

// runBuild runs one build request's command in one module, to completion or to the window's
// end. A build that fails is still load and is still counted: the machine did the work.
func runBuild(ctx context.Context, o options, dir string) {
	cmd := exec.CommandContext(ctx, "sh", "-c", o.buildCommand)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// acquire asks the real gate binary for permission, exactly as Claude Code's PreToolUse hook
// would. It returns whether the build may proceed, and the function that returns the lease.
//
// THE DECISION IS READ THE WAY CLAUDE CODE READS IT: empty stdout means no opinion and the
// command proceeds; a permissionDecision of "deny" means it does not. Nothing here inspects
// the daemon's own logs to decide whether a build was admitted, because a measurement that
// trusted the loop's accounting could not have found what GOV-15 found.
func (d *daemon) acquire(ctx context.Context, o options, i int) (bool, func()) {
	id := fmt.Sprintf("sweep-%d-%d", os.Getpid(), i)
	payload := map[string]any{
		"session_id":  d.session,
		"tool_name":   "Bash",
		"tool_use_id": id,
		"tool_input":  map[string]any{"command": o.buildCommand},
	}
	out, err := d.hook(ctx, "gate", payload)
	if err != nil {
		// A gate that could not be run is not a refusal. Treating it as one would score a
		// harness bug as governance, which is the error most likely to produce a confident
		// wrong threshold.
		fmt.Fprintf(os.Stderr, "sweep: gate: %v\n", err)
		return true, nil
	}
	if denied(out) {
		return false, nil
	}
	return true, func() {
		_, _ = d.hook(context.WithoutCancel(ctx), "finish", map[string]any{
			"session_id":    d.session,
			"tool_name":     "Bash",
			"tool_use_id":   id,
			"tool_response": map[string]any{},
		})
	}
}

func denied(stdout []byte) bool {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return false
	}
	var v struct {
		HookSpecificOutput struct {
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout, &v); err != nil {
		return false
	}
	return v.HookSpecificOutput.PermissionDecision == "deny"
}

func (d *daemon) hook(ctx context.Context, sub string, payload map[string]any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, d.bin, sub)
	cmd.Stdin = bytes.NewReader(b)
	cmd.Env = append(os.Environ(), d.env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	err = cmd.Run()
	return out.Bytes(), err
}

// ---------------------------------------------------------------------------
// The daemon
// ---------------------------------------------------------------------------

type daemon struct {
	bin     string
	env     []string
	dir     string
	session string
	proc    *exec.Cmd
	log     *os.File
}

// logPath is where this daemon's diagnostics are being written.
func (d *daemon) logPath() string { return filepath.Join(d.dir, "daemon.log") }

// startDaemon runs a governor daemon ISOLATED FROM THE MACHINE'S OWN: its own state
// directory, so its own socket, so it neither collides with an installed governor nor
// governs the developer's live sessions while the sweep runs.
//
// It is restarted for every arm rather than reconfigured, and that is not tidiness. The AIMD
// loop's budget is state: an arm that inherited the previous arm's floored budget would
// begin already cut, and would measure the previous threshold for its first several ticks.
// GOV-15 restarted the daemon between arms for exactly this reason.
func startDaemon(ctx context.Context, bin string, threshold float64) (*daemon, error) {
	dir, err := os.MkdirTemp("", "sweep-state-")
	if err != nil {
		return nil, err
	}
	d := &daemon{
		bin:     bin,
		dir:     dir,
		session: fmt.Sprintf("sweep-%d", time.Now().UnixNano()),
		env: []string{
			"GOVERNOR_STATE_DIR=" + dir,
			"CLAUDE_PLUGIN_DATA=" + dir,
			fmt.Sprintf("GOVERNOR_RUNQUEUE_MAX=%g", threshold),
		},
	}
	cmd := exec.Command(bin, "daemon")
	cmd.Env = append(os.Environ(), d.env...)
	// THE DAEMON'S DIAGNOSTICS ARE KEPT, not discarded. They carry the budget event log
	// (design.md section 4.5) — the peaks, the refusals and the parks — which is the only
	// way to see what the loop actually did during an arm as opposed to what the machine
	// looked like while it did it. Discarding them is how a run that made no sense stayed
	// unexplainable for an hour.
	if lf, err := os.Create(filepath.Join(dir, "daemon.log")); err == nil {
		d.log = lf
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	d.proc = cmd

	// Wait for it to answer rather than sleeping a guessed interval: an arm that began
	// before the daemon was listening would fail open on its first few requests and read as
	// a more permissive threshold than it is.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		out, err := d.hook(ctx, "status", nil)
		if err == nil && len(out) > 0 {
			return d, nil
		}
	}
	_ = d.stop()
	return nil, fmt.Errorf("daemon did not start listening within 10s")
}

// stop kills the daemon and returns its diagnostics, so the caller can keep them beside the
// arm they belong to before the state directory goes away.
func (d *daemon) stop() string {
	if d.proc != nil && d.proc.Process != nil {
		_ = d.proc.Process.Kill()
		_, _ = d.proc.Process.Wait()
	}
	var out string
	if b, err := os.ReadFile(d.logPath()); err == nil {
		out = string(b)
	}
	if d.log != nil {
		_ = d.log.Close()
	}
	if d.dir != "" {
		_ = os.RemoveAll(d.dir)
	}
	return out
}

// ---------------------------------------------------------------------------
// Settling
// ---------------------------------------------------------------------------

// settle waits for the machine to go quiet before an arm starts.
//
// loadavg1 IS A DECAYING ONE-MINUTE AVERAGE, which is the whole reason this exists. An arm
// started straight after another begins with the previous arm's contention still in the
// signal, so the controller cuts the budget before the new arm's first build arrives — and
// the arm measures the tail of its predecessor. GOV-15 hit this and waited for run queue per
// core below 0.8.
func settle(ctx context.Context, o options) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(o.settleWait)
	quietSince := time.Time{}
	for {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("machine did not settle below %.2f run queue per core within %s; "+
				"something else is loading it", o.settleBelow, o.settleWait)
		}
		v := runQueuePerCore()
		if v >= 0 && v < o.settleBelow {
			if quietSince.IsZero() {
				quietSince = time.Now()
			}
			if time.Since(quietSince) >= o.settleFor {
				d := time.Since(start)
				fmt.Printf("  settled at %.2f run queue per core after %.0fs\n", v, d.Seconds())
				return d, nil
			}
		} else {
			quietSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// The run-queue watcher
// ---------------------------------------------------------------------------

// runQueue collects loadavg1 over usable cores at 1 Hz.
//
// It is an INDEPENDENT measurement, taken in this process rather than read out of the
// daemon's own signals. That independence is what let GOV-15's experiment disagree with the
// loop it was measuring, and it is the property worth keeping most.
//
// IT KEEPS TWO SERIES, AND THE REASON IS A CONFOUND THAT WOULD OTHERWISE INVERT THE RESULT.
// Every arm is held open for the same fixed window, but the arms do not stay busy for the
// same share of it: a batch that drains in 90 seconds leaves 60 seconds of an idle machine
// in the window, and those idle samples drag its percentiles down. The arm that drains
// FASTEST collects the MOST idle samples, so scoring contention over the whole window would
// systematically flatter whichever arm admitted the most work — which is the ungoverned one.
// That is backwards, and it is the sort of artefact that produces a confident wrong
// threshold.
//
// So `busy` is the series used for the reported percentiles: samples taken while at least
// one build request of the batch is still outstanding. It answers "how contended was this
// machine while it did the same fixed work", which is the question the trade turns on.
// `all` is kept beside it so a reader can see the difference rather than take it on trust.
type runQueue struct {
	all  []float64
	busy []float64
	// cpu is host utilisation on the same ticks as busy.
	//
	// IT IS RECORDED BECAUSE THE RUN QUEUE IS NOT THE ONLY TRIGGER, and the first sweep was
	// unreadable without it. The loop cuts on `CPU > Setpoint+Band` OR `RunQueue > max`, so
	// an arm where CPU sits above 0.80 throughout is one where the run-queue threshold could
	// not have decided anything — and every arm of a build fan-out is that arm. Measuring
	// only the quantity under test would have left that invisible.
	cpu []float64
	// smoothed is the same tick's CPU after the 5 s EWMA the daemon applies before the loop
	// ever sees it (section 5.1: signals.EWMA owns the window, and it is set in one place).
	//
	// IT IS THE SERIES THE CONDITION SHARES ARE COMPUTED ON, and `cpu` is kept beside it
	// only for continuity with pass1 and pass2, which reported raw. The distinction did not
	// matter for the CUT, because a fan-out pegs both at 1.00. It matters for the CLIMB,
	// which is the condition GOV-32 exists to measure: the climb needs CPU below 0.65, and a
	// raw series dips there on brief scheduling gaps that a 5 s average absorbs. Measuring
	// the climb on raw CPU would therefore report a loop climbing on ticks the real loop
	// never climbs on, which is the exact direction that would have made this story's answer
	// wrong in the reassuring direction.
	smoothed []float64
	ewma     *signals.EWMA
	// dropped counts busy ticks discarded because host CPU would not read. See sample.
	dropped int
}

func (r *runQueue) sample(outstanding bool) {
	v := runQueuePerCore()
	if v < 0 {
		return
	}
	r.all = append(r.all, v)
	if !outstanding {
		return
	}
	c := hostCPU()
	if c < 0 {
		// A tick whose CPU will not read is dropped from BOTH busy series rather than from
		// one of them. The cut and climb conditions are JOINT over CPU and the run queue, so
		// an unpaired append would silently shift one series against the other from that
		// tick on, and every joint share computed afterwards would be pairing readings taken
		// at different moments. The previous version appended to `busy` unconditionally and
		// to `cpu` only on success, which was harmless while nothing read the two together.
		r.dropped++
		return
	}
	if r.ewma == nil {
		r.ewma = signals.NewEWMA(signals.DefaultCPUWindow)
	}
	r.busy = append(r.busy, v)
	r.cpu = append(r.cpu, c)
	r.smoothed = append(r.smoothed, r.ewma.Update(c, time.Now()))
}

// conditions evaluates the loop's OWN predicates over the paired busy series.
//
// It calls budget.Cutting and budget.Climbing rather than restating them, so that a change to
// the loop cannot leave this harness measuring the previous policy and reporting it as the
// current one. That is not hypothetical: this file already carried a hand-copied cut
// condition, and it was evaluated against a different CPU series than the loop uses.
// THE TWO THRESHOLDS ARE PASSED SEPARATELY BECAUSE THEY ARE NOT THE SAME NUMBER. The loop cuts
// above runQueueMax and climbs at or below runQueueMax-RunQueueBand, and a first version of
// this function handed runQueueMax to both. That reported the climb firing on every tick with
// a queue under 2.0 when the loop only climbs under 1.5 — an over-count, in the direction that
// would have made the fix look better than it is.
func (r *runQueue) conditions(runQueueMax, climbAt float64) (cut, climb float64, n int) {
	n = len(r.smoothed)
	if n == 0 {
		return 0, 0, 0
	}
	cuts, climbs := 0, 0
	for i := 0; i < n; i++ {
		rd := budget.Reading{CPU: r.smoothed[i], RunQueue: r.busy[i]}
		if budget.Cutting(rd, runQueueMax) {
			cuts++
		}
		if budget.Climbing(rd, climbAt) {
			climbs++
		}
	}
	return float64(cuts) / float64(n), float64(climbs) / float64(n), n
}

func stats(in []float64) (p50, p90, max float64) {
	if len(in) == 0 {
		return 0, 0, 0
	}
	s := append([]float64(nil), in...)
	sort.Float64s(s)
	return pct(s, 0.50), pct(s, 0.90), s[len(s)-1]
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// hostCPU mirrors internal/signals.Sampler's CPU reading: whole-host utilisation in [0,1],
// differenced against gopsutil's own previous call. It returns -1 if the platform will not
// answer.
func hostCPU() float64 {
	pct, err := cpu.Percent(0, false)
	if err != nil || len(pct) == 0 {
		return -1
	}
	return pct[0] / 100
}

// runQueuePerCore mirrors internal/signals.Sampler: loadavg1 divided by the cores this
// process may use. It returns -1 if the platform will not answer.
func runQueuePerCore() float64 {
	avg, err := load.Avg()
	if err != nil {
		return -1
	}
	c := capacity()
	if c <= 0 {
		return -1
	}
	return avg.Load1 / c
}

// ---------------------------------------------------------------------------
// The workload
// ---------------------------------------------------------------------------

type workload struct {
	root    string
	modules []string
	// canary is a module the measured batch never builds, reserved for the per-arm timing
	// check so that running it cannot change what the batch finds in the cache.
	canary string
}

// prepareWorkload generates the Go modules the builds compile.
//
// GENERATED RATHER THAN POINTED AT A REPO, because "re-runnable by a person who was not
// there" rules out a workload that only exists on one laptop. The shape is GOV-15's: eight
// modules of nine thousand functions each, about nine seconds of wall time and thirty-odd
// core-seconds for one `go build -a` on an M1 Air.
//
// THE STDLIB IMPORTS ARE MOST OF THE COST AND ARE NOT DECORATION. Nine thousand trivial
// functions on their own compile in half a second for 1.5 core-seconds, because the package
// imports nothing and there is nothing else to build. With these imports `-a` rebuilds the
// whole transitive dependency tree, which is what takes the build to ~36 core-seconds at
// ~420% — and the parallelism is the point as much as the total, because a single-package
// compile cannot put the machine under the kind of contention this experiment measures.
// Reproducing GOV-15's per-build cost is what makes these numbers comparable to its.
func prepareWorkload(ctx context.Context, o options) (*workload, func(), error) {
	root := o.workdir
	cleanup := func() {}
	if root == "" {
		d, err := os.MkdirTemp("", "sweep-work-")
		if err != nil {
			return nil, cleanup, err
		}
		root = d
	}
	if !o.keepWorkdir {
		cleanup = func() { _ = os.RemoveAll(root) }
	}
	w := &workload{root: root}
	fmt.Fprintf(os.Stderr, "generating %d modules of %d functions in %s\n", o.modules+1, o.funcs, root)
	for i := 0; i < o.modules+1; i++ {
		dir := filepath.Join(root, fmt.Sprintf("mod%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, cleanup, err
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte(fmt.Sprintf("module sweep/mod%d\n\ngo 1.21\n", i)), 0o644); err != nil {
			return nil, cleanup, err
		}
		var b strings.Builder
		b.WriteString(workloadPreamble)
		for f := 0; f < o.funcs; f++ {
			fmt.Fprintf(&b, "func F%d(x int) int { return x*%d + %d }\n", f, f%97+1, f%31)
		}
		if err := os.WriteFile(filepath.Join(dir, "gen.go"), []byte(b.String()), 0o644); err != nil {
			return nil, cleanup, err
		}
		// The last module generated is the canary's and is kept out of the batch.
		if i == o.modules {
			w.canary = dir
			break
		}
		w.modules = append(w.modules, dir)
	}
	return w, cleanup, nil
}

// workloadPreamble pulls a broad slice of the standard library into the dependency tree, so
// that `go build -a` has a real graph to rebuild. The packages are chosen for weight and for
// breadth — crypto, net, reflection-heavy encoders, the go/ tooling tree — rather than for
// anything they do here; nothing calls them.
const workloadPreamble = `package mod

import (
	"compress/gzip"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"go/parser"
	"net/http"
	"regexp"
	"text/template"
)

var (
	_ = http.NewRequest
	_ = json.Marshal
	_ = sql.Open
	_ = template.New
	_ = tls.Client
	_ = regexp.MustCompile
	_ = parser.ParseFile
	_ = gzip.NewReader
)

`

func (w *workload) cleanCache(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "go", "clean", "-cache")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

// ---------------------------------------------------------------------------
// Host
// ---------------------------------------------------------------------------

// usableCores is the denominator of "run queue per core", read ONCE from the product's own
// sampler rather than from runtime.NumCPU().
//
// The two agree on a Mac and disagree inside a container, where internal/signals reads the
// cgroup quota. Dividing by the wrong one would make every number in this report
// incomparable with the threshold it is measuring, which is defined against the sampler's
// figure — so it is read from the sampler, once, rather than reimplemented here.
var usableCores = func() float64 {
	s, err := signals.NewSampler().Sample(context.Background())
	if err != nil || s.CPUCapacity <= 0 {
		return float64(runtime.NumCPU())
	}
	return s.CPUCapacity
}()

func capacity() float64 { return usableCores }

func describeHost() string {
	return fmt.Sprintf("%s/%s, %.0f usable cores", runtime.GOOS, runtime.GOARCH, capacity())
}

// ---------------------------------------------------------------------------
// The concurrency curve
// ---------------------------------------------------------------------------

// runCurve measures what N concurrent builds actually do to the machine, with no governor in
// the picture at all.
//
// WHY THIS EXISTS, AND WHY THE THRESHOLD SWEEP COULD NOT ANSWER IT. Sweeping RunQueueMax
// under a ten-way fan-out found no effect between 1.2 and 4.5, and the daemon's own budget
// log says why: a fan-out pegs host CPU, so `CPU > Setpoint+SetpointBand` fires on three
// ticks in four and the budget floors whatever the run-queue threshold is. The run-queue
// threshold is only reachable as a decision in the regime where CPU is NOT pegged — one or
// two builds on an otherwise quiet machine — which is exactly the regime GOV-31 was filed
// about and exactly the regime a ten-way fan-out cannot observe.
//
// So this measures the curve directly: run queue and host CPU as a function of how many
// builds are running. A threshold is then a statement about where on that curve the budget
// should stop climbing, which is an argument from the trade rather than from a percentile.
func runCurve(ctx context.Context, o options, work *workload, res *results) error {
	res.Order = "concurrency curve"
	for i, n := range o.concurrency {
		a := arm{Name: fmt.Sprintf("builds=%d", n), Position: i + 1}
		fmt.Printf("== %s ==\n", a.Name)

		settled, err := settle(ctx, o)
		if err != nil {
			return err
		}
		a.SettleSeconds = settled.Seconds()

		if o.canary {
			t0 := time.Now()
			runBuild(ctx, o, work.canary)
			a.CanarySeconds = time.Since(t0).Seconds()
			fmt.Printf("  canary build %.1fs\n", a.CanarySeconds)
		}
		if err := work.cleanCache(ctx); err != nil {
			return err
		}

		wctx, cancel := context.WithTimeout(ctx, o.window)
		meter := childcpu.New(wctx, childcpu.Meter{RootPIDs: []int32{int32(os.Getpid())}})
		var outstanding atomic.Int64
		outstanding.Store(int64(n))
		var rq runQueue
		var mu sync.Mutex
		sampling := make(chan struct{})
		go func() {
			defer close(sampling)
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-wctx.Done():
					return
				case <-t.C:
					mu.Lock()
					meter.Tick(wctx)
					rq.sample(outstanding.Load() > 0)
					mu.Unlock()
				}
			}
		}()

		start := time.Now()
		var wg sync.WaitGroup
		for j := 0; j < n; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				runBuild(wctx, o, work.modules[j%len(work.modules)])
				outstanding.Add(-1)
			}(j)
		}
		wg.Wait()
		// The window ENDS WITH THE WORK here, unlike the threshold arms. There is no budget
		// to observe settling and no batch that might never drain, so holding an idle machine
		// open would only add idle ticks to a series that is already gated on outstanding
		// work.
		elapsed := time.Since(start)
		cancel()
		<-sampling

		mu.Lock()
		rep := meter.Report()
		p50, p90, max := stats(rq.busy)
		cpuP50, _, cpuMax := stats(rq.cpu)
		above := 0
		for _, c := range rq.cpu {
			if c > budget.Setpoint+budget.SetpointBand {
				above++
			}
		}
		share := 0.0
		if len(rq.cpu) > 0 {
			share = float64(above) / float64(len(rq.cpu))
		}
		samples := len(rq.busy)
		smP50, _, _ := stats(rq.smoothed)
		cutShare, climbShare, _ := rq.conditions(budget.RunQueueMax, budget.RunQueueMax-budget.RunQueueBand)
		dropped := rq.dropped
		ser := &series{
			RunQueue: append([]float64(nil), rq.busy...),
			CPU:      append([]float64(nil), rq.cpu...),
			CPUEWMA:  append([]float64(nil), rq.smoothed...),
		}
		mu.Unlock()

		// Every build ran: there is no gate in this mode, so admitted and completed are the
		// concurrency itself and refusals are meaningless.
		a.Admitted, a.Completed = n, n
		a.BuildTestCoreSec = rep.BuildTestCoreSec()
		a.PeakChildCores = rep.PeakDescendantCores
		a.RunQueueP50, a.RunQueueP90, a.RunQueueMaxObserved = p50, p90, max
		a.RunQueueSamples = samples
		a.CPUP50, a.CPUAboveCut = cpuP50, share
		a.CPUEWMAP50, a.CutShare, a.ClimbShare = smP50, cutShare, climbShare
		a.DroppedTicks = dropped
		a.Series = ser
		a.DrainSeconds = elapsed.Seconds()
		a.WindowSeconds = elapsed.Seconds()
		fmt.Printf("  -> run queue p50 %.2f p90 %.2f max %.2f, cpu p50 %.2f (max %.2f, %.0f%% above cut), "+
			"loop would cut on %.0f%% of ticks and climb on %.0f%%, "+
			"build CPU %.1f core-sec, wall %.1fs\n\n",
			p50, p90, max, cpuP50, cpuMax, 100*share, 100*cutShare, 100*climbShare,
			a.BuildTestCoreSec, elapsed.Seconds())
		res.Arms = append(res.Arms, a)
	}

	fmt.Println()
	printCurve(*res)
	if o.out != "" {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(o.out, append(b, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nraw results written to %s\n", o.out)
	}
	return nil
}

func printCurve(r results) {
	fmt.Printf("%-12s %8s %8s %8s %8s %9s %12s %8s\n",
		"builds", "rq p50", "rq p90", "rq max", "cpu p50", "cpu>cut", "build core-s", "wall s")
	for _, a := range r.Arms {
		fmt.Printf("%-12s %8.2f %8.2f %8.2f %8.2f %8.0f%% %12.1f %8.1f\n",
			a.Name, a.RunQueueP50, a.RunQueueP90, a.RunQueueMaxObserved,
			a.CPUP50, 100*a.CPUAboveCut, a.BuildTestCoreSec, a.DrainSeconds)
	}
}

// ---------------------------------------------------------------------------
// Recovery: can the budget climb back while a build is running?
// ---------------------------------------------------------------------------

// runRecovery holds a fixed build load on the machine and records what the daemon's budget
// does, once per second, for the whole window.
//
// WHY THE FAN-OUT ARMS COULD NOT ANSWER THIS, WHICH IS THE REASON THIS MODE EXISTS. GOV-32
// measured the shipped loop and a candidate loop under the same ten-way fan-out and got
// IDENTICAL budget trajectories: every logged value in both arms was the floor. The arms were
// not discriminating between two controllers, they were both reporting that ten concurrent
// builds oversubscribe an eight-core laptop — which is true, and which both loops are supposed
// to say. The batch is admitted at B_max, six builds start at once, and `loadavg1` is a
// one-minute average, so the run queue stays above any plausible cut threshold for the whole
// 150-second window and no controller gets to demonstrate anything else.
//
// That is the same shape of trap GOV-31 hit and named: a measurement whose answer is fixed by
// something upstream of the quantity under test. There it was the CPU disjunct deciding before
// the run-queue threshold was consulted; here it is the initial burst deciding before the loop
// is consulted.
//
// SO THIS MEASURES THE OTHER REGIME, WHICH IS THE ONE THE STORY IS ACTUALLY ABOUT: a machine
// with builds running on it continuously but NOT oversubscribed. One `go build -a` alone holds
// host CPU at 0.77-1.00 and the run queue at about 1.22 per core. A loop that cuts on
// utilisation must floor there and stay floored; a loop that cuts on saturation must not.
//
// THE LOAD IS OPEN-LOOP AND DELIBERATELY DOES NOT GO THROUGH THE GATE. The question is what
// the CONTROLLER does given a load, not what the gate admits, and routing the load through the
// budget would make the load a function of the answer. So the builds run regardless of what
// the budget says, the budget is read from `governor status`, and the two are independent.
func runRecovery(ctx context.Context, o options, work *workload, res *results) error {
	a := arm{Name: fmt.Sprintf("recovery, %d build(s) held", o.recovery), Governed: true, Position: 1}
	fmt.Printf("== %s ==\n", a.Name)

	settled, err := settle(ctx, o)
	if err != nil {
		return err
	}
	a.SettleSeconds = settled.Seconds()

	if o.canary && len(work.modules) > 0 {
		t0 := time.Now()
		runBuild(ctx, o, work.canary)
		a.CanarySeconds = time.Since(t0).Seconds()
		fmt.Printf("  canary build %.1fs\n", a.CanarySeconds)
	}
	if err := work.cleanCache(ctx); err != nil {
		return err
	}

	// THE THRESHOLD IS A PARAMETER HERE TOO, and it is the difference between an experiment
	// that isolates the change and one that is swamped by the machine it runs on. Measured on
	// this host, holding ONE build produces a run queue of 2.91 per core rather than the 1.22
	// the concurrency curve records for a build alone, because the developer's own session is
	// on the box. 2.91 is over the shipped cut of 2.0, so both loops cut and the arm answers
	// nothing — the ambient load, not the controller, decides.
	//
	// Passing -thresholds moves the cut out of the way of the ambient load, which leaves the
	// CPU disjunct as the only thing the two binaries disagree about. That is the quantity
	// under test, so isolating it is the point rather than a convenience.
	threshold := budget.RunQueueMax
	if len(o.thresholds) > 0 {
		threshold = o.thresholds[0]
	}
	a.RunQueueMax = threshold
	dm, err := startDaemon(ctx, o.governor, threshold)
	if err != nil {
		return err
	}

	wctx, cancel := context.WithTimeout(ctx, o.window)
	defer cancel()

	// The load: o.recovery builds, each relaunched as soon as it finishes, so the machine
	// carries the same concurrency for the whole window rather than tailing off.
	var load sync.WaitGroup
	for i := 0; i < o.recovery; i++ {
		load.Add(1)
		go func(i int) {
			defer load.Done()
			for wctx.Err() == nil {
				runBuild(wctx, o, work.modules[i%len(work.modules)])
			}
		}(i)
	}

	var mu sync.Mutex
	rq := &runQueue{}
	var budgets []int
	sampling := make(chan struct{})
	go func() {
		defer close(sampling)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				b := readBudget(ctx, dm)
				mu.Lock()
				rq.sample(true)
				if b > 0 {
					budgets = append(budgets, b)
				}
				mu.Unlock()
			}
		}
	}()

	<-wctx.Done()
	load.Wait()
	<-sampling
	a.DaemonLog = dm.stop()

	mu.Lock()
	p50, p90, max := stats(rq.busy)
	cpuP50, _, _ := stats(rq.smoothed)
	cutShare, climbShare, _ := rq.conditions(armRunQueueMax(a), armClimbRunQueue(a))
	ser := &series{
		RunQueue: append([]float64(nil), rq.busy...),
		CPU:      append([]float64(nil), rq.cpu...),
		CPUEWMA:  append([]float64(nil), rq.smoothed...),
	}
	trace := append([]int(nil), budgets...)
	mu.Unlock()

	a.RunQueueP50, a.RunQueueP90, a.RunQueueMaxObserved = p50, p90, max
	a.RunQueueSamples = len(ser.RunQueue)
	a.CPUEWMAP50, a.CutShare, a.ClimbShare = cpuP50, cutShare, climbShare
	a.Series = ser
	a.Budgets = trace
	a.WindowSeconds = o.window.Seconds()

	lo, hi, final := budgetSpan(trace)
	a.BudgetMin, a.BudgetMax, a.BudgetFinal = lo, hi, final
	fmt.Printf("  -> budget over %d ticks: min %d, max %d, final %d\n", len(trace), lo, hi, final)
	fmt.Printf("     run queue p50 %.2f p90 %.2f max %.2f, smoothed cpu p50 %.2f\n",
		p50, p90, max, cpuP50)
	fmt.Printf("     cut condition on %.0f%% of ticks, climb condition on %.0f%%\n\n",
		100*cutShare, 100*climbShare)

	res.Arms = append(res.Arms, a)
	return writeResults(o, *res)
}

// readBudget asks the daemon what the budget is, through the same `governor status` a user
// would run. It returns 0 if the answer cannot be read, and the caller drops that tick rather
// than recording a zero that would look like a floor.
func readBudget(ctx context.Context, dm *daemon) int {
	out, err := dm.hook(ctx, "status", nil)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "budget ") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "budget")))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

func budgetSpan(b []int) (lo, hi, final int) {
	if len(b) == 0 {
		return 0, 0, 0
	}
	lo, hi = b[0], b[0]
	for _, v := range b {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return lo, hi, b[len(b)-1]
}
