package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// failure is every way a reading can fail. Each one makes the reading "unknown": never an
// error that stops the tool, and never a guessed number. An unknown reading is visible as
// such; a guessed one would look exactly like a cool machine.
type failure string

func (f failure) Error() string { return string(f) }

// source is what the OS hands over, as text. osSource is the real one (source_darwin.go);
// tests substitute their own to drive every failure shape.
type source interface {
	Sample() (string, error) // one line per CPU complex: name, then TAB-separated state=cumulative ticks
	Tables() (string, error) // one line per pmgr voltage-state table: name, TAB, raw bytes in hex
	Temps() (string, error)  // one line per temperature sensor: name, TAB, degrees C
}

type state struct {
	Name  string
	Ticks int64
}

// parseSample reads cwSample's text. Anything it does not recognise is a failure: a format
// change must surface as unknown, not as a cluster with no states.
func parseSample(text string) (map[string][]state, error) {
	out := map[string][]state{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if f[0] == "" || len(f) < 2 {
			return nil, failure("unparseable channel line")
		}
		for _, kv := range f[1:] {
			name, val, ok := strings.Cut(kv, "=")
			n, err := strconv.ParseInt(val, 10, 64)
			if !ok || name == "" || err != nil {
				return nil, failure("unparseable state " + strconv.Quote(kv))
			}
			out[f[0]] = append(out[f[0]], state{name, n})
		}
	}
	if len(out) == 0 {
		return nil, failure("no channels in sample")
	}
	return out, nil
}

// parseTables reads cwTables' text into MHz lists. Only the "-sram" tables carry frequencies
// on the chips measured (their non-sram twins are all zero); a table is kept only if every
// entry is a real, strictly rising frequency, so a zeroed or reordered table cannot be used.
func parseTables(text string) (map[string][]float64, error) {
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
		return nil, failure("no usable voltage-state table")
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

var (
	pCluster = regexp.MustCompile(`^P(CPU|ACC)\d*$`)
	eCluster = regexp.MustCompile(`^E(CPU|ACC)\d*$`)
)

func isIdle(name string) bool { return name == "IDLE" || name == "OFF" || name == "DOWN" }

// tableFor finds the frequency table for a cluster with n active states. Channel names and
// table names both vary by chip, so the join is by count: exactly one table with n entries.
// None, or more than one, is a chip this tool cannot map, and says so.
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
		return nil, failure(fmt.Sprintf("no voltage table with %d states", n))
	default:
		return nil, failure(fmt.Sprintf("%d-state tables are ambiguous: %s", n, strings.Join(hit, ", ")))
	}
}

type cluster struct {
	MHz    float64 // residency-weighted average frequency while active; 0 if never active
	Active float64 // fraction of the interval not idle
}

// measure reads one cluster kind from two cumulative samples. Every channel of that kind
// (an M1 has one P-cluster, larger chips have two) is pooled, tick for tick.
func measure(prev, cur map[string][]state, tables map[string][]float64, kind *regexp.Regexp) (cluster, error) {
	var idle, active, weighted float64
	found := false
	names := make([]string, 0, len(cur))
	for name := range cur {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !kind.MatchString(name) {
			continue
		}
		found = true
		c, p := cur[name], prev[name]
		if len(c) != len(p) {
			return cluster{}, failure(name + ": state layout changed between samples")
		}
		var act []float64
		for i := range c {
			if c[i].Name != p[i].Name {
				return cluster{}, failure(name + ": state layout changed between samples")
			}
			d := c[i].Ticks - p[i].Ticks
			if d < 0 {
				return cluster{}, failure(name + ": counter went backwards")
			}
			if isIdle(c[i].Name) {
				idle += float64(d)
			} else {
				act = append(act, float64(d))
			}
		}
		t, err := tableFor(len(act), tables)
		if err != nil {
			return cluster{}, failure(name + ": " + err.Error())
		}
		for i, d := range act {
			active += d
			weighted += d * t[i]
		}
	}
	if !found {
		return cluster{}, failure("no channel for this cluster")
	}
	if idle+active == 0 {
		return cluster{}, failure("no ticks elapsed")
	}
	c := cluster{Active: active / (idle + active)}
	if active > 0 {
		c.MHz = weighted / active
	}
	return c, nil
}

// pSensorPrefix narrows the sensors read at all (source_darwin.go keeps only these, once);
// pSensor is the exact rule a reading must pass.
const pSensorPrefix = "pACC MTR Temp Sensor"

var pSensor = regexp.MustCompile(`^` + pSensorPrefix + `\d+$`)

// pClusterTemp is the hottest P-cluster sensor. Some sensors report placeholders (-21.6 C was
// seen on idle ones), so only readings in a physically plausible range count; none left is
// unknown, reported as ok=false.
func pClusterTemp(text string) (float64, bool) {
	best, ok := math.Inf(-1), false
	for _, line := range strings.Split(text, "\n") {
		name, val, cut := strings.Cut(line, "\t")
		c, err := strconv.ParseFloat(val, 64)
		if !cut || err != nil || !pSensor.MatchString(name) || c <= 0 || c >= 130 {
			continue
		}
		best, ok = math.Max(best, c), true
	}
	return best, ok
}
