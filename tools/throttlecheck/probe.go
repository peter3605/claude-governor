package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"
)

// Scoring a sudo-free clock reading against powermetrics (GOV-B54 Done-when clause 2).
//
// The candidate is tools/clockprobe. The questions the clause asks of it, answered here from
// the same run: did it track the frequency drop, how soon did it see it, and what did it cost.
// powermetrics is the ground truth only because it was run by hand for this measurement.

type probeReading struct {
	At   time.Time `json:"ts"`
	Kind string    `json:"kind"`
	Core string    `json:"core"`
	MHz  float64   `json:"mhz"`
	Cost float64   `json:"cost"`
	// PTempC is the P-cluster temperature, when the probe reads one (tools/clusterwatch does,
	// GOV-B60). Scored separately: a temperature is not a clock, so it is reported against
	// the clock's drop rather than as an error in MHz.
	PTempC *float64 `json:"p_temp_c"`
}

func readProbe(path string, from, to time.Time) ([]probeReading, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []probeReading
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r probeReading
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Kind != "probe" || r.At.Before(from) || r.At.After(to) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, sc.Err()
}

func nearestProbe(ps []probeReading, t time.Time, maxGap time.Duration) (probeReading, bool) {
	i := sort.Search(len(ps), func(i int) bool { return !ps[i].At.Before(t) })
	best, ok, bestD := probeReading{}, false, maxGap+1
	for _, j := range []int{i - 1, i} {
		if j < 0 || j >= len(ps) {
			continue
		}
		d := ps[j].At.Sub(t)
		if d < 0 {
			d = -d
		}
		if d <= maxGap && d < bestD {
			best, ok, bestD = ps[j], true, d
		}
	}
	return best, ok
}

type probeScore struct {
	Readings, Joined, BusyP int     // probe readings in window; joined to a busy pm second; of those labelled P
	PShare                  float64 // BusyP / Joined
	MedianErrMHz            float64 // probe minus powermetrics, signed, over BusyP
	MedianAbsErrMHz         float64
	Correlation             float64 // Pearson, probe vs powermetrics, over BusyP
	ProbeRefMHz             float64
	ProbeFirstBelow         time.Duration
	ProbeConfirmed          time.Duration
	ProbeThrottled          bool
	MeanCost, MaxCost       float64

	Temps    []tempAt // busy seconds with a temperature, any core label
	TempCorr float64  // Pearson, temperature vs powermetrics frequency (a throttle reads negative)
}

type tempAt struct {
	Off time.Duration // since the first busy second
	C   float64
}

// scoreProbe joins each busy powermetrics second to the nearest probe reading and applies the
// SAME throttle definition analyse() uses, to the probe's P-labelled readings alone. Seconds
// where the P-cluster was not busy are excluded on both sides: an idle P-cluster clocks itself
// down by design, and a low reading there is power management, not heat.
func scoreProbe(rows []row, probes []probeReading, th thresholds) probeScore {
	sc := probeScore{Readings: len(probes)}
	for _, p := range probes {
		sc.MeanCost += p.Cost
		sc.MaxCost = math.Max(sc.MaxCost, p.Cost)
	}
	if len(probes) > 0 {
		sc.MeanCost /= float64(len(probes))
	}

	type pt struct {
		at        time.Time
		pm, probe float64
	}
	var pts []pt
	var tf, tc []float64
	var start time.Time
	for _, r := range rows {
		if !(r.HasP && r.HasPRes && r.PResidency >= th.BusyResidency) {
			continue
		}
		if start.IsZero() {
			start = r.At
		}
		p, ok := nearestProbe(probes, r.At, 700*time.Millisecond)
		if !ok {
			continue
		}
		sc.Joined++
		if p.PTempC != nil {
			sc.Temps = append(sc.Temps, tempAt{r.At.Sub(start), *p.PTempC})
			tf, tc = append(tf, r.PFreqMHz), append(tc, *p.PTempC)
		}
		if p.Core != "P" {
			continue
		}
		pts = append(pts, pt{r.At, r.PFreqMHz, p.MHz})
	}
	sc.TempCorr = pearson(tf, tc)
	sc.BusyP = len(pts)
	if sc.Joined > 0 {
		sc.PShare = float64(sc.BusyP) / float64(sc.Joined)
	}
	if len(pts) < 2 {
		return sc
	}

	errs, abs := make([]float64, len(pts)), make([]float64, len(pts))
	xs, ys := make([]float64, len(pts)), make([]float64, len(pts))
	for i, p := range pts {
		errs[i] = p.probe - p.pm
		abs[i] = math.Abs(errs[i])
		xs[i], ys[i] = p.pm, p.probe
	}
	sc.MedianErrMHz, sc.MedianAbsErrMHz = pct(errs, 0.5), pct(abs, 0.5)
	sc.Correlation = pearson(xs, ys)

	// The probe's own verdict, by the rule analyse() applies to powermetrics: reference is the
	// peak short median in the opening window, a throttle is a Sustain-long median under the cut.
	k, w := int(th.RefSmooth/time.Second), int(th.Sustain/time.Second)
	for i := 0; i+k <= len(pts) && pts[i+k-1].at.Sub(start) <= th.RefWindow; i++ {
		win := make([]float64, 0, k)
		for _, p := range pts[i : i+k] {
			win = append(win, p.probe)
		}
		sc.ProbeRefMHz = math.Max(sc.ProbeRefMHz, pct(win, 0.5))
	}
	cut := th.DropFraction * sc.ProbeRefMHz
	for i := 0; i+w <= len(pts) && !sc.ProbeThrottled; i++ {
		win := make([]float64, 0, w)
		for _, p := range pts[i : i+w] {
			win = append(win, p.probe)
		}
		if pct(win, 0.5) < cut {
			sc.ProbeThrottled = true
			sc.ProbeConfirmed = pts[i+w-1].at.Sub(start)
			for _, p := range pts[i : i+w] {
				if p.probe < cut {
					sc.ProbeFirstBelow = p.at.Sub(start)
					break
				}
			}
		}
	}
	return sc
}

func pearson(xs, ys []float64) float64 {
	if len(xs) < 2 || len(xs) != len(ys) {
		return 0
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx, my = mx/float64(len(xs)), my/float64(len(xs))
	var sxy, sxx, syy float64
	for i := range xs {
		sxy += (xs[i] - mx) * (ys[i] - my)
		sxx += (xs[i] - mx) * (xs[i] - mx)
		syy += (ys[i] - my) * (ys[i] - my)
	}
	if sxx == 0 || syy == 0 {
		return 0
	}
	return sxy / math.Sqrt(sxx*syy)
}

// tempNear is the temperature reading closest to an offset into the run, or NaN with none.
func tempNear(ts []tempAt, off time.Duration) float64 {
	best, bestD := math.NaN(), time.Duration(math.MaxInt64)
	for _, t := range ts {
		d := t.Off - off
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = t.C, d
		}
	}
	return best
}

func printTemps(w io.Writer, sc probeScore, s summary) {
	if len(sc.Temps) == 0 {
		return
	}
	peak := math.Inf(-1)
	for _, t := range sc.Temps {
		peak = math.Max(peak, t.C)
	}
	fmt.Fprintf(w, "probe temp      %d busy seconds; %.1f C at start", len(sc.Temps), sc.Temps[0].C)
	if s.Throttled {
		fmt.Fprintf(w, ", %.1f C at first below, %.1f C at confirmed", tempNear(sc.Temps, s.FirstBelow), tempNear(sc.Temps, s.Confirmed))
	}
	fmt.Fprintf(w, ", peak %.1f C; correlation with pm frequency %.3f\n", peak, sc.TempCorr)
}

func printProbe(w io.Writer, sc probeScore, s summary) {
	defer printTemps(w, sc, s)
	fmt.Fprintf(w, "probe           %d readings, %d joined to busy seconds, %.0f%% of those on a P-core\n",
		sc.Readings, sc.Joined, 100*sc.PShare)
	if sc.BusyP < 2 {
		fmt.Fprintln(w, "probe verdict   NO SCORE: too few P-core readings during busy seconds")
		return
	}
	fmt.Fprintf(w, "probe vs pm     median error %+.0f MHz, median |error| %.0f MHz, correlation %.3f over %d seconds\n",
		sc.MedianErrMHz, sc.MedianAbsErrMHz, sc.Correlation, sc.BusyP)
	fmt.Fprintf(w, "probe cost      mean %.2f%% of a core, max %.2f%%  (sampler budget: 1%%)\n", 100*sc.MeanCost, 100*sc.MaxCost)
	switch {
	case sc.ProbeThrottled && s.Throttled:
		fmt.Fprintf(w, "probe verdict   TRACKS: probe under 90%% of its %.0f MHz reference from %s, confirmed by %s (powermetrics: %s, %s)\n",
			sc.ProbeRefMHz, sc.ProbeFirstBelow.Round(time.Second), sc.ProbeConfirmed.Round(time.Second),
			s.FirstBelow.Round(time.Second), s.Confirmed.Round(time.Second))
	case sc.ProbeThrottled:
		fmt.Fprintf(w, "probe verdict   FALSE ALARM: probe saw a drop from %s that powermetrics did not\n", sc.ProbeFirstBelow.Round(time.Second))
	case s.Throttled:
		fmt.Fprintln(w, "probe verdict   MISSED: powermetrics saw a throttle the probe did not")
	default:
		fmt.Fprintln(w, "probe verdict   AGREES: neither saw a throttle")
	}
}
