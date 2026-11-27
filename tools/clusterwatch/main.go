// Command clusterwatch reads the P-cluster's clock and temperature once a second, without sudo
// (GOV-B60).
//
// GOV-B54 measured that under sustained load this M1 Air's P-cluster falls to about half its
// 2988 MHz within a minute while thermalState stays nominal, and that tools/clockprobe could not
// see it: a thread has to RUN on a P-core to time one, and under load macOS keeps it on an
// E-core. This reads what the hardware counts instead, which needs no placement:
//
//   - IOReport's "CPU Complex Performance States": per cluster, the 24 MHz ticks spent idle and
//     in each DVFS state since boot. Two samples a tick apart give the interval's residency;
//     weighted by the frequencies in the power manager's voltage-state tables (the IORegistry
//     pmgr entry) that is the average active clock, the number powermetrics prints as "HW
//     active frequency".
//   - IOHID temperature sensors: the hottest "pACC MTR Temp Sensor", the P-cluster's own.
//
// Both are private interfaces, read as an ordinary user with no entitlement, resolved with
// dlsym at run time. Every failure makes that second's reading "unknown" (core "?"), never an
// error and never a guess. It is a tool, wired into nothing.
//
// # Result (design.md §5.3)
//
// Against powermetrics on GOV-B60's run A3: a P-cluster reading in every busy second, median
// error 0 (|error| 2 MHz), correlation 0.995, 0.23% of a core. The temperature pins at ~97 C
// within 21 s while the clock keeps falling, so only the clock is to be wired in (GOV-B61).
//
// Output is one JSON object per line, in the shape tools/throttlecheck -probe scores:
//
//	go run ./tools/clusterwatch -for 36m > ~/gov-b60/watch-A3.jsonl
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

type record struct {
	Kind    string   `json:"kind"`
	At      string   `json:"ts"`
	Core    string   `json:"core"` // "P" when the P-cluster was read, "?" when unknown
	MHz     float64  `json:"mhz"`  // P-cluster average active frequency
	PActive float64  `json:"p_active"`
	EMHz    float64  `json:"e_mhz,omitempty"`
	EActive float64  `json:"e_active,omitempty"`
	PTempC  *float64 `json:"p_temp_c,omitempty"`
	Status  string   `json:"status"` // ok | unknown
	Reason  string   `json:"reason,omitempty"`
	Cost    float64  `json:"cost"`
}

// watcher turns a source into readings. It holds the previous sample, because residency is a
// delta: the first tick has nothing to subtract and is unknown by construction.
type watcher struct {
	src     source
	noTemps bool
	openErr error
	tables  map[string][]float64
	tErr    error
	prev    map[string][]state
}

func newWatcher(src source, openErr error) *watcher {
	w := &watcher{src: src, openErr: openErr}
	if openErr == nil {
		text, err := src.Tables()
		if err == nil {
			w.tables, err = parseTables(text)
		}
		w.tErr = err
	}
	return w
}

func unknown(why error) record {
	return record{Kind: "probe", Core: "?", Status: "unknown", Reason: why.Error()}
}

func (w *watcher) next() record {
	if w.openErr != nil {
		return unknown(w.openErr)
	}
	r := w.frequency()
	if w.noTemps {
		return r
	}
	if text, err := w.src.Temps(); err == nil {
		if c, ok := pClusterTemp(text); ok {
			r.PTempC = &c
		}
	}
	return r
}

func (w *watcher) frequency() record {
	if w.tErr != nil {
		return unknown(w.tErr)
	}
	text, err := w.src.Sample()
	if err != nil {
		w.prev = nil
		return unknown(err)
	}
	cur, err := parseSample(text)
	if err != nil {
		w.prev = nil
		return unknown(err)
	}
	prev := w.prev
	w.prev = cur
	if prev == nil {
		return unknown(failure("first sample: residency needs two"))
	}
	p, err := measure(prev, cur, w.tables, pCluster)
	if err != nil {
		return unknown(fmt.Errorf("P-cluster: %w", err))
	}
	r := record{Kind: "probe", Core: "P", MHz: p.MHz, PActive: p.Active, Status: "ok"}
	if e, err := measure(prev, cur, w.tables, eCluster); err == nil {
		r.EMHz, r.EActive = e.MHz, e.Active
	}
	return r
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func run(w *watcher, interval, dur time.Duration, out io.Writer) error {
	enc := json.NewEncoder(out)
	end := time.Now().Add(dur)
	lastWall, lastCPU := time.Now(), processCPU()
	w.next() // prime the delta
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for now := range tick.C {
		if now.After(end) {
			return nil
		}
		r := w.next()
		cpu, wall := processCPU(), time.Now()
		r.At = wall.UTC().Format(time.RFC3339Nano)
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

func main() {
	interval := flag.Duration("interval", time.Second, "time between readings")
	dur := flag.Duration("for", 35*time.Minute, "how long to run")
	temps := flag.Bool("temps", true, "also read the IOHID temperature sensors (most of the cost)")
	raw := flag.Bool("raw", false, "print what the OS returns (tables, one sample, temperatures) and exit: for fixtures, and for mapping a new chip")
	flag.Parse()
	src, err := newOSSource()
	if *raw {
		if err != nil {
			fmt.Fprintln(os.Stderr, "clusterwatch:", err)
			os.Exit(1)
		}
		for _, f := range []func() (string, error){src.Tables, src.Sample, src.Temps} {
			text, err := f()
			fmt.Printf("%s(err=%v)\n---\n", text, err)
		}
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clusterwatch: every reading will be unknown:", err)
	}
	w := newWatcher(src, err)
	w.noTemps = !*temps
	if err := run(w, *interval, *dur, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "clusterwatch:", err)
		os.Exit(1)
	}
}
