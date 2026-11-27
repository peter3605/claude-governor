package signals

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The P-cluster clock (GOV-B61, design.md section 5.3).
//
// GOV-B54 measured that under sustained load this class of machine runs its performance
// cluster at about half speed within a minute while thermalState stays nominal and CPU, load
// average and the run queue all read exactly as they would on a cool machine: a throttled core
// is still a busy core. GOV-B60 found the one sudo-free reading that sees it — IOReport's
// per-cluster DVFS-state residency, the counters powermetrics itself reads — and matched it to
// powermetrics within 2 MHz in every busy second (tools/clusterwatch). This is that method,
// moved into the sampler.
//
// THE SPLIT. pclock_darwin.go only FETCHES, as text: the residency counters and the power
// manager's voltage-state tables. Everything that interprets them is here, platform-neutral,
// so every failure shape is testable on any host without the private framework.
//
// FAILURE IS UNKNOWN, AND UNKNOWN IS NOMINAL. Every way this can fail — library or symbol
// missing (it is private API), no channel, a chip whose tables cannot be mapped, a counter
// that went backwards, output that does not parse — yields Known=false with a reason, never an
// error that stops the sample and never a guessed number. A guessed clock would look exactly
// like a cool machine or exactly like a hot one, and either is worse than "this machine would
// not say".

// pclockSource is what the OS hands over, as text. The darwin implementation is in
// pclock_darwin.go; tests substitute their own to drive every failure shape. It is internal
// plumbing of the one Sampler implementation, not a seam.
type pclockSource interface {
	// Sample returns one line per CPU complex: its channel name, then TAB-separated
	// state=ticks pairs, the ticks cumulative since boot at 24 MHz.
	Sample() (string, error)
	// Tables returns one line per power-manager voltage-state table: its name, TAB, the raw
	// bytes in hex — little-endian uint32 (frequency Hz, voltage) pairs.
	Tables() (string, error)
}

// PClock is one reading of the performance cluster.
type PClock struct {
	// MHz is the residency-weighted average frequency the cluster ran at while active over
	// the interval: the figure powermetrics prints as "HW active frequency". Zero when the
	// cluster was never active, or when Known is false.
	MHz float64
	// Active is the fraction of the interval the cluster was not idle, in [0,1].
	//
	// It is what makes MHz interpretable. An idle P-cluster clocks itself down to 600-1400
	// MHz by design, so a low MHz on a mostly idle cluster is power management, not heat.
	// Only a busy cluster's clock says anything about throttling.
	Active float64
	// TopMHz is the top of the P-cluster's DVFS table on this machine, read from the silicon
	// (the IORegistry pmgr voltage-state table), not a constant. Frac is MHz over it.
	//
	// IT IS NOT THE ALL-CORE CEILING, AND THAT IS MEASURED (GOV-B61). On the M1 the top state
	// is 3204 MHz, which the hardware delivers only with one or two P-cores busy; with all four
	// it caps the cluster at 2988 — and IOReport still reports the REQUESTED top state. Against
	// powermetrics in GOV-B60's run A3, the first 21 fully-busy seconds read 3204 here and 2988
	// there, a steady +216 MHz; once the clock fell below 2988 the two agreed within 2-3 MHz
	// for the remaining 29 minutes. No unprivileged source reports the multi-core cap, so the
	// reference is the table top and a cool, fully busy M1 reads Frac 1.00 where the truth is
	// 0.93. Everything below the cap is exact, and a throttle is by definition below it: the
	// cut threshold (design.md section 5.3) sits under the cap, where this error does not reach.
	TopMHz float64
	// Frac is MHz / TopMHz, in [0,1]; zero when the cluster was never active or Known is false.
	Frac float64
	// Known is whether the reading was taken. False is nominal: it never cuts.
	Known bool
	// Reason says why Known is false, for the trace and for doctor.
	Reason string
}

type pclockFailure string

func (f pclockFailure) Error() string { return string(f) }

type pclockState struct {
	Name  string
	Ticks int64
}

// parsePClockSample reads the source's sample text. Anything it does not recognise is a
// failure: a format change must surface as unknown, not as a cluster with no states.
func parsePClockSample(text string) (map[string][]pclockState, error) {
	out := map[string][]pclockState{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if f[0] == "" || len(f) < 2 {
			return nil, pclockFailure("unparseable channel line")
		}
		for _, kv := range f[1:] {
			name, val, ok := strings.Cut(kv, "=")
			n, err := strconv.ParseInt(val, 10, 64)
			if !ok || name == "" || err != nil {
				return nil, pclockFailure("unparseable state " + strconv.Quote(kv))
			}
			out[f[0]] = append(out[f[0]], pclockState{name, n})
		}
	}
	if len(out) == 0 {
		return nil, pclockFailure("no channels in sample")
	}
	return out, nil
}

// parsePClockTables reads the voltage-state tables into MHz lists. Only the "-sram" tables
// carry frequencies on the chips measured (their non-sram twins are all zero), and a table is
// kept only if every entry is a real, strictly rising frequency, so a zeroed or reordered
// table cannot be used.
func parsePClockTables(text string) (map[string][]float64, error) {
	out := map[string][]float64{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		name, hx, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasSuffix(name, "-sram") {
			continue
		}
		b, err := hex.DecodeString(hx)
		if err != nil || len(b) == 0 || len(b)%8 != 0 {
			continue
		}
		var mhz []float64
		for i := 0; i < len(b); i += 8 {
			mhz = append(mhz, float64(binary.LittleEndian.Uint32(b[i:]))/1e6)
		}
		if rising(mhz) {
			out[name] = mhz
		}
	}
	if len(out) == 0 {
		return nil, pclockFailure("no usable voltage-state table")
	}
	return out, nil
}

func rising(f []float64) bool {
	for i, v := range f {
		if v <= 0 || (i > 0 && v <= f[i-1]) {
			return false
		}
	}
	return true
}

// pClusterChannel matches the performance cluster's channel by name family, not by a list of
// M1 names: "PCPU" on the M1, numbered or "PACC" on chips with two P-clusters.
var pClusterChannel = regexp.MustCompile(`^P(CPU|ACC)\d*$`)

func idleState(name string) bool { return name == "IDLE" || name == "OFF" || name == "DOWN" }

// tableFor finds the frequency table for a cluster with n active states. Channel and table
// names both vary by chip, so the join is by count: exactly one table with n entries. None, or
// more than one, is a chip this cannot map, and it says so rather than picking.
func tableFor(n int, tables map[string][]float64) ([]float64, error) {
	var hit []string
	for name, t := range tables {
		if len(t) == n {
			hit = append(hit, name)
		}
	}
	sort.Strings(hit)
	switch len(hit) {
	case 1:
		return tables[hit[0]], nil
	case 0:
		return nil, pclockFailure(fmt.Sprintf("no voltage table with %d states", n))
	default:
		return nil, pclockFailure(fmt.Sprintf("%d-state tables are ambiguous: %s", n, strings.Join(hit, ", ")))
	}
}

// measurePCluster reads the performance cluster from two cumulative samples. Every
// P-cluster channel is pooled tick for tick (an M1 has one, larger chips two).
func measurePCluster(prev, cur map[string][]pclockState, tables map[string][]float64) (PClock, error) {
	var idle, active, weighted, top float64
	found := false
	names := make([]string, 0, len(cur))
	for name := range cur {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !pClusterChannel.MatchString(name) {
			continue
		}
		found = true
		c, p := cur[name], prev[name]
		if len(c) != len(p) {
			return PClock{}, pclockFailure(name + ": state layout changed between samples")
		}
		var act []float64
		for i := range c {
			if c[i].Name != p[i].Name {
				return PClock{}, pclockFailure(name + ": state layout changed between samples")
			}
			d := c[i].Ticks - p[i].Ticks
			if d < 0 {
				return PClock{}, pclockFailure(name + ": counter went backwards")
			}
			if idleState(c[i].Name) {
				idle += float64(d)
			} else {
				act = append(act, float64(d))
			}
		}
		t, err := tableFor(len(act), tables)
		if err != nil {
			return PClock{}, pclockFailure(name + ": " + err.Error())
		}
		for i, d := range act {
			active += d
			weighted += d * t[i]
		}
		if t[len(t)-1] > top {
			top = t[len(t)-1]
		}
	}
	if !found {
		return PClock{}, pclockFailure("no P-cluster channel")
	}
	if idle+active == 0 {
		return PClock{}, pclockFailure("no ticks elapsed")
	}
	r := PClock{Active: active / (idle + active), TopMHz: top, Known: true}
	if active > 0 {
		r.MHz = weighted / active
		r.Frac = r.MHz / top
	}
	return r, nil
}

// pclock turns a source into readings. It holds the previous sample, because residency is a
// delta; the first reading, and the first after any failed sample, has nothing valid to
// subtract from and is unknown by construction.
type pclock struct {
	src     pclockSource
	openErr error
	tables  map[string][]float64
	tErr    error
	prev    map[string][]pclockState
}

// newPClock wraps a source. An open error makes every reading unknown for the life of the
// sampler: a framework that is not there will not appear, and retrying a dlopen every second
// would spend the sampler's budget learning nothing. The tables are read once; they describe
// the silicon and do not change while the machine runs.
func newPClock(src pclockSource, openErr error) *pclock {
	p := &pclock{src: src, openErr: openErr}
	if openErr == nil {
		text, err := src.Tables()
		if err == nil {
			p.tables, err = parsePClockTables(text)
		}
		p.tErr = err
	}
	return p
}

func unknownPClock(why error) PClock { return PClock{Reason: why.Error()} }

func (p *pclock) next() PClock {
	if p.openErr != nil {
		return unknownPClock(p.openErr)
	}
	if p.tErr != nil {
		return unknownPClock(p.tErr)
	}
	text, err := p.src.Sample()
	if err != nil {
		p.prev = nil
		return unknownPClock(err)
	}
	cur, err := parsePClockSample(text)
	if err != nil {
		p.prev = nil
		return unknownPClock(err)
	}
	prev := p.prev
	p.prev = cur
	if prev == nil {
		return unknownPClock(pclockFailure("first sample: residency needs two"))
	}
	r, err := measurePCluster(prev, cur, p.tables)
	if err != nil {
		return unknownPClock(err)
	}
	return r
}
