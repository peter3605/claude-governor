// Command shapecost measures what one command costs in cores, in two ways at once, so the
// gap between them is visible rather than argued about.
//
// GOV-B33 clause 2: Heavy's 4.5 expected peak cores (docs/bash-corpus.md section 5) came from
// the 1 Hz sampler's CHILD-TREE cores, attributed to a Bash call. That instrument can only see
// work that descends from the command. Two of the shapes that dominate Heavy on macOS do most
// of their work somewhere else:
//
//   - docker build / docker compose: in Docker Desktop's VM
//     (com.apple.Virtualization.VirtualMachine and com.docker.*), which no docker client
//     process is a parent of (bash-corpus.md section 11).
//   - xcodebuild test: the test host runs inside the iOS Simulator, under launchd_sim, which
//     CoreSimulator starts. xcodebuild is not its ancestor either.
//
// So per run this reports:
//
//  1. child: the command's process tree at a 1 s tick, through internal/childcpu, the same
//     walk and the same delta arithmetic the corpus figure came from. Peak and mean.
//  2. outside the tree, at a 250 ms tick, with an idle baseline subtracted: the Docker VM
//     group, the Simulator group (launchd_sim and everything under it), and host total
//     (every core that was not idle). Host total minus baseline is an upper bound, because
//     another session can still land inside the window; -daemon-log flags runs that
//     overlapped another session's build lease, so they can be discarded rather than
//     averaged in.
//
// Peaks outside the tree are reported over 1 s windows (the mean of four 250 ms samples), so
// they are comparable with the 1 Hz child peak. A raw 250 ms peak inflates sub-second bursts,
// which section 11 already records.
//
// This is a measurement tool, not product code. It is never on the hot path.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/peter3605/claude-governor/internal/childcpu"
)

const (
	groupVM   = "docker-vm"
	groupSim  = "simulator"
	groupHost = "host"
)

var groups = []string{groupVM, groupSim, groupHost}

type stat struct {
	Mean   float64 `json:"mean"`
	Peak1s float64 `json:"peak_1s"`
}

type run struct {
	Label    string             `json:"label"`
	Rep      int                `json:"rep"`
	Start    time.Time          `json:"start"`
	Wall     float64            `json:"wall_s"`
	Exit     int                `json:"exit"`
	Settled  bool               `json:"settled"`
	Child    stat               `json:"child"`
	ChildCS  float64            `json:"child_core_s"`
	Baseline map[string]float64 `json:"baseline"`
	Outside  map[string]stat    `json:"outside_minus_baseline"`
	Overlaps []string           `json:"overlapping_leases,omitempty"`
}

func main() {
	var (
		label      = flag.String("label", "", "label for the report")
		reps       = flag.Int("reps", 3, "repetitions")
		dir        = flag.String("dir", "", "working directory for the command")
		baseline   = flag.Duration("baseline", 4*time.Second, "idle baseline measured before each rep")
		settleBelw = flag.Float64("settle-below", 3.0, "host busy cores the machine must be under before a rep starts; a desktop with Docker Desktop and several Claude sessions idles around 2-3")
		settleFor  = flag.Duration("settle-for", 10*time.Second, "how long it must stay under it")
		settleWait = flag.Duration("settle-wait", 5*time.Minute, "give up settling after this long; the rep runs and is marked unsettled")
		pre        = flag.String("pre", "", "shell command run before each rep, outside the measurement (e.g. clearing a cache)")
		daemonLog  = flag.String("daemon-log", filepath.Join(os.Getenv("HOME"), ".claude/governor/daemon.log"), "daemon.log to check for other sessions' leases overlapping a rep; empty disables")
		ownSession = flag.String("own-session", "", "session ID whose leases are this measurement's own and do not count as overlap")
		out        = flag.String("out", "", "append each rep as a JSON line here as well")
	)
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: shapecost [flags] -- command...")
		os.Exit(2)
	}
	command := strings.Join(flag.Args(), " ")
	ctx := context.Background()

	var runs []run
	for i := 1; i <= *reps; i++ {
		if *pre != "" {
			c := exec.Command("/bin/sh", "-c", *pre)
			c.Dir = *dir
			c.Stdout, c.Stderr = os.Stderr, os.Stderr
			_ = c.Run()
		}
		settled := settle(*settleBelw, *settleFor, *settleWait)
		base := measureBaseline(ctx, *baseline)
		r := measure(ctx, command, *dir)
		r.Label, r.Rep, r.Settled, r.Baseline = *label, i, settled, base
		raw := r.Outside
		r.Outside = map[string]stat{}
		for _, g := range groups {
			r.Outside[g] = stat{
				Mean:   clamp0(raw[g].Mean - base[g]),
				Peak1s: clamp0(raw[g].Peak1s - base[g]),
			}
		}
		if *daemonLog != "" {
			r.Overlaps = overlaps(*daemonLog, *ownSession, r.Start, r.Start.Add(time.Duration(r.Wall*float64(time.Second))))
		}
		runs = append(runs, r)
		printRun(r)
		if *out != "" {
			if f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				b, _ := json.Marshal(r)
				fmt.Fprintln(f, string(b))
				f.Close()
			}
		}
	}
}

func clamp0(x float64) float64 {
	if x < 0 {
		return 0
	}
	return x
}

func printRun(r run) {
	o := r.Outside
	flags := ""
	if !r.Settled {
		flags += " UNSETTLED"
	}
	if len(r.Overlaps) > 0 {
		flags += fmt.Sprintf(" OVERLAP(%d)", len(r.Overlaps))
	}
	if r.Exit != 0 {
		flags += fmt.Sprintf(" EXIT=%d", r.Exit)
	}
	fmt.Printf("%s rep %d  wall %.1fs | child mean %.2f peak %.2f (%.1f core-s) | vm mean %.2f peak %.2f | sim mean %.2f peak %.2f | host mean %.2f peak %.2f%s\n",
		r.Label, r.Rep, r.Wall, r.Child.Mean, r.Child.Peak1s, r.ChildCS,
		o[groupVM].Mean, o[groupVM].Peak1s, o[groupSim].Mean, o[groupSim].Peak1s,
		o[groupHost].Mean, o[groupHost].Peak1s, flags)
}

// hostBusy returns busy cores over the interval since the previous call's times.
func hostTimes() (busy, total float64) {
	ts, err := cpu.Times(false)
	if err != nil || len(ts) == 0 {
		return 0, 0
	}
	t := ts[0]
	idle := t.Idle + t.Iowait
	all := t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
	return all - idle, all
}

func settle(below float64, need, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	quietSince := time.Time{}
	b0, _ := hostTimes()
	t0 := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		b1, _ := hostTimes()
		cores := (b1 - b0) / time.Since(t0).Seconds()
		b0, t0 = b1, time.Now()
		if time.Now().Second()%10 == 0 {
			fmt.Fprintf(os.Stderr, "settling: host busy %.2f cores (need < %.2f for %s)\n", cores, below, need)
		}
		if cores < below {
			if quietSince.IsZero() {
				quietSince = time.Now()
			}
			if time.Since(quietSince) >= need {
				return true
			}
		} else {
			quietSince = time.Time{}
		}
	}
	return false
}

// outsideSampler tracks CPU in the groups that are not descendants of the command.
type outsideSampler struct {
	exclude  int32 // root of the command's tree; its descendants are never counted here
	prev     map[int32]float64
	prevHost float64
	last     time.Time
	primed   bool
	samples  []map[string]float64 // cores per group, one per tick
}

func newOutside(exclude int32) *outsideSampler {
	return &outsideSampler{exclude: exclude, prev: map[int32]float64{}, last: time.Now()}
}

func (s *outsideSampler) tick(ctx context.Context) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return
	}
	children := map[int32][]int32{}
	name := map[int32]string{}
	byPID := map[int32]*process.Process{}
	var simRoots []int32
	for _, p := range procs {
		byPID[p.Pid] = p
		ppid, err := p.PpidWithContext(ctx)
		if err == nil {
			children[ppid] = append(children[ppid], p.Pid)
		}
		n := childcpu.ProcName(ctx, p)
		name[p.Pid] = n
		if n == "launchd_sim" {
			simRoots = append(simRoots, p.Pid)
		}
	}
	excluded := map[int32]bool{}
	if s.exclude > 0 {
		for _, pid := range descendants([]int32{s.exclude}, children) {
			excluded[pid] = true
		}
	}
	member := map[int32]string{}
	for pid, n := range name {
		if excluded[pid] {
			continue
		}
		if n == "com.apple.Virtualization.VirtualMachine" || strings.HasPrefix(n, "com.docker") {
			member[pid] = groupVM
		}
	}
	for _, pid := range descendants(simRoots, children) {
		if !excluded[pid] {
			member[pid] = groupSim
		}
	}

	elapsed := time.Since(s.last).Seconds()
	s.last = time.Now()
	next := map[int32]float64{}
	cores := map[string]float64{}
	for pid, g := range member {
		t, err := byPID[pid].TimesWithContext(ctx)
		if err != nil {
			continue
		}
		total := t.User + t.System
		next[pid] = total
		if prev, ok := s.prev[pid]; ok && s.primed && elapsed > 0 && total >= prev {
			cores[g] += (total - prev) / elapsed
		}
	}
	busy, _ := hostTimes()
	if s.primed && elapsed > 0 {
		cores[groupHost] = clamp0((busy - s.prevHost) / elapsed)
		s.samples = append(s.samples, cores)
	}
	s.prevHost = busy
	s.prev = next
	s.primed = true
}

// summary returns mean and peak over 1 s windows for each group.
func (s *outsideSampler) summary(perSecond int) map[string]stat {
	out := map[string]stat{}
	for _, g := range groups {
		var sum float64
		for _, c := range s.samples {
			sum += c[g]
		}
		st := stat{}
		if n := len(s.samples); n > 0 {
			st.Mean = sum / float64(n)
		}
		for i := 0; i+perSecond <= len(s.samples); i++ {
			w := 0.0
			for _, c := range s.samples[i : i+perSecond] {
				w += c[g]
			}
			if w/float64(perSecond) > st.Peak1s {
				st.Peak1s = w / float64(perSecond)
			}
		}
		out[g] = st
	}
	return out
}

func descendants(roots []int32, children map[int32][]int32) []int32 {
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

const outsideTick = 250 * time.Millisecond

func measureBaseline(ctx context.Context, d time.Duration) map[string]float64 {
	s := newOutside(0)
	s.tick(ctx)
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		time.Sleep(outsideTick)
		s.tick(ctx)
	}
	base := map[string]float64{}
	for g, st := range s.summary(int(time.Second / outsideTick)) {
		base[g] = st.Mean
	}
	return base
}

func measure(ctx context.Context, command, dir string) run {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	r := run{Start: time.Now()}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		r.Exit = -1
		return r
	}
	pid := int32(cmd.Process.Pid)

	child := childcpu.New(ctx, childcpu.Meter{RootPIDs: []int32{pid}})
	outside := newOutside(pid)

	done := make(chan struct{})
	var wg sync.WaitGroup
	var childSamples []float64
	wg.Add(2)
	go func() { // 1 s child tick: the corpus instrument
		defer wg.Done()
		child.Tick(ctx)
		prevCS := 0.0
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Second):
			}
			child.Tick(ctx)
			rep := child.Report()
			cs := rep.DescendantCoreSec() + rep.RootCoreSec
			childSamples = append(childSamples, cs-prevCS)
			prevCS = cs
		}
	}()
	go func() {
		defer wg.Done()
		outside.tick(ctx)
		for {
			select {
			case <-done:
				return
			case <-time.After(outsideTick):
			}
			outside.tick(ctx)
		}
	}()

	err := cmd.Wait()
	r.Wall = time.Since(r.Start).Seconds()
	close(done)
	wg.Wait()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.Exit = ee.ExitCode()
		} else {
			r.Exit = -1
		}
	}

	rep := child.Report()
	r.ChildCS = rep.DescendantCoreSec() + rep.RootCoreSec
	if r.Wall > 0 {
		r.Child.Mean = r.ChildCS / r.Wall
	}
	for _, c := range childSamples { // each sample covers ~1 s, so core-s per sample is cores
		if c > r.Child.Peak1s {
			r.Child.Peak1s = c
		}
	}
	r.Outside = outside.summary(int(time.Second / outsideTick))
	return r
}

// overlaps returns other sessions' build leases from daemon.log whose held interval overlaps
// [from, to]. A release line carries its end time and held duration, which is the interval.
func overlaps(path, own string, from, to time.Time) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, " budget release ") || !strings.Contains(line, "kind=build") {
			continue
		}
		fields := strings.Fields(line)
		end, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		var session string
		var held time.Duration
		for _, kv := range fields {
			if v, ok := strings.CutPrefix(kv, "session="); ok {
				session = v
			}
			if v, ok := strings.CutPrefix(kv, "held="); ok {
				held, _ = time.ParseDuration(v)
			}
		}
		if session == own {
			continue
		}
		start := end.Add(-held)
		if start.Before(to) && end.After(from) {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

// The meter must not compete with the command it measures. Two threads are enough for the
// two samplers, and they keep the measurement's own footprint off the cores under test.
func init() { runtime.GOMAXPROCS(2) }
