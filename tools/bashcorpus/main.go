// Command bashcorpus collects and cost-labels every Bash tool call on this machine.
//
// GOV-14 asks which `Bash` commands are build-shaped, and forbids answering it from a
// hand-written list of binary names that looks plausible. This command builds the evidence
// the answer has to come from: one record per Bash tool call, carrying the command text and
// what the machine was actually doing while it ran.
//
// The cost label is the same quantity docs/phase-0-findings.md section 5 turns on — child
// cores, claude_cpu minus claude_cpu_roots, the CPU burnt by descendants of a session rather
// than by the session process itself. A build or test run is spawned as a child and lands
// there; a subagent executes inside the session process and lands in roots. So child cores
// during a Bash call is, modulo the confound below, what that command cost.
//
// THE CONFOUND IS THE HARD PART AND IT IS NOT HIDDEN HERE. The sampler is host-wide and this
// machine ran five or six concurrent sessions for 98.9% of the Phase 0 window. Child cores in
// the seconds a command ran are not necessarily THAT command's — another session may have
// been building at the same time.
//
// EXCLUSIVITY IS PER SECOND, NOT PER COMMAND, AND THE DIFFERENCE IS THE WHOLE MEASUREMENT.
// The obvious rule — keep only commands that overlapped no other command — was tried first
// and is biased in exactly the direction that matters. Measured over this corpus, the share
// of calls that ran alone falls from 54% at 1-5 seconds to 26% at 20-60 seconds, because a
// long command on a five-session machine is almost certain to overlap something. It discards
// the expensive tail, which is the only part of the distribution this story is about.
//
// So attribution is computed per sample instead. A 1 Hz sample is ATTRIBUTABLE to a call when
// that call is the only Bash call in flight anywhere on the machine during that second, and a
// call is labelled from its attributable seconds alone. A 200-second test run that overlapped
// a 3-second `grep` loses three seconds of evidence rather than all of it.
//
//	AttrSamplesN   attributable seconds this call got
//	AttrFrac       what share of its window those were: how much of the call was observed
//	AttrChildMean  child cores averaged over them — the cost label
//
// The baseline is taken the same way, from pre-window seconds when NOTHING was in flight, so
// the marginal is against a genuinely idle machine rather than against whatever else was
// running. That mirrors the population marginal in phase-0-findings.md section 3 rather than
// inventing a second method.
//
// BACKGROUNDED COMMANDS ARE THE RESIDUAL HOLE, and they are bounded rather than ignored. A
// `run_in_background` call returns its tool_result immediately and keeps running, so the
// transcript records when it started and never when it stopped — and nothing in this corpus
// polls BashOutput, so there is no second source to recover the end from. Such a call is
// therefore treated as in flight for backgroundHorizon from its start: it cannot be labelled,
// and it suppresses attribution for anything it overlaps. The horizon is a guess, so the
// analysis is run at more than one value and the sensitivity is reported.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peter3605/claude-governor/internal/trace"
)

// baselineWindow is how far back of a call's start the baseline is sampled.
//
// Long enough to average out a single busy second, short enough that it is still the same
// machine state the call began in. A baseline taken from minutes earlier would be measuring a
// different workload and subtracting it would be noise, not correction.
const baselineWindow = 15 * time.Second

// backgroundHorizon is how long a `run_in_background` command is assumed to still be running.
//
// It is not a measurement — the transcript carries no end for these — and it is deliberately
// generous, because the cost of overstating it is lost evidence while the cost of
// understating it is a wrong cost label. Its sensitivity is reported rather than assumed: see
// the -bg-horizon flag.
var backgroundHorizon = 10 * time.Minute

// minAttrSamples is how many attributable seconds a call needs before its label is used.
//
// One second is a single 1 Hz reading and the sampler's own jitter is of that order, so a
// one-sample label is a coin flip about a quantity whose median is zero. Two is the smallest
// number that is not one reading.
const minAttrSamples = 2

// Call is one Bash tool call and what the machine did while it ran.
type Call struct {
	Session string `json:"session"`
	Project string `json:"project"`
	// Sidechain is true when the call was made by a subagent rather than the main
	// conversation. Recorded because a corpus that is all main-session traffic is a limit on
	// what can be claimed, and the only way to know that is to count.
	Sidechain bool `json:"sidechain"`
	// Background is tool_input.run_in_background. It matters more than it looks: a
	// backgrounded command returns its tool_result IMMEDIATELY and keeps running, so End
	// does not bound its execution and its cost label would be measured over a window the
	// command had barely started. These are collected and flagged, never labelled.
	Background bool    `json:"background"`
	Start      string  `json:"start"`
	End        string  `json:"end"`
	DurS       float64 `json:"dur_s"`
	CWD        string  `json:"cwd,omitempty"`
	Cmd        string  `json:"cmd"`

	// Labelled reports whether the call's window falls inside the sampler's coverage and
	// carries enough samples to say anything. Everything below is meaningless when false.
	Labelled bool `json:"labelled"`
	// SamplesN is how many 1 Hz samples landed inside the window. A sub-second command can
	// legitimately have one or none, and a predicate fitted without checking this would be
	// fitting to the sampler's cadence rather than to the command.
	SamplesN int `json:"samples_n"`
	// Coverage is SamplesN over the seconds the window spans: the sampler drops ticks, and a
	// window it half-covered is weaker evidence than one it covered fully.
	Coverage float64 `json:"coverage"`

	ChildPeak    float64 `json:"child_peak"`
	ChildMean    float64 `json:"child_mean"`
	ChildCoreSec float64 `json:"child_coresec"`
	RootPeak     float64 `json:"root_peak"`
	RootMean     float64 `json:"root_mean"`
	HostCPUMean  float64 `json:"host_cpu_mean"`

	// The attributable figures are the ones a predicate may be fitted on. They are computed
	// over the seconds this call was the only Bash call in flight on the machine; everything
	// above is computed over the whole window and includes other sessions' work.
	AttrSamplesN  int     `json:"attr_samples_n"`
	AttrFrac      float64 `json:"attr_frac"`
	AttrChildPeak float64 `json:"attr_child_peak"`
	AttrChildMean float64 `json:"attr_child_mean"`
	// AttrCoreSec is the observed attributable core-seconds: the cost actually seen, not
	// extrapolated. EstCoreSec scales AttrChildMean over the full duration, which is the
	// better estimate of what the command cost in total but rests on the unobserved seconds
	// looking like the observed ones.
	AttrCoreSec float64 `json:"attr_coresec"`
	EstCoreSec  float64 `json:"est_coresec"`

	BaseChildMean float64 `json:"base_child_mean"`
	BaseSamplesN  int     `json:"base_samples_n"`
	// MargChildMean is AttrChildMean minus BaseChildMean. Negative values are kept: a command
	// that ran while the machine was getting quieter is a real observation and clipping it at
	// zero would bias every summary computed from this file upward.
	MargChildMean float64 `json:"marg_child_mean"`
	MargCoreSec   float64 `json:"marg_coresec"`

	// OverlapN is how many other Bash calls overlapped this one's window at all. It is kept
	// as a description of the traffic, NOT as the attribution rule — see the package comment
	// for why per-command exclusivity was rejected in favour of per-second.
	OverlapN int `json:"overlap_n"`
}

// sample is the subset of a trace sample this analysis reads.
type sample struct {
	t     time.Time
	child float64
	root  float64
	cpu   float64
}

func main() {
	home, _ := os.UserHomeDir()
	transcripts := flag.String("transcripts", filepath.Join(home, ".claude", "projects"),
		"directory of Claude Code transcript directories")
	samplesPath := flag.String("samples", "", "path to samples.jsonl (default: the governor state dir)")
	out := flag.String("out", "", "write JSONL corpus here (default: stdout)")
	bgHorizon := flag.Duration("bg-horizon", backgroundHorizon,
		"how long a run_in_background command is assumed to keep running; vary it to check sensitivity")
	flag.Parse()
	backgroundHorizon = *bgHorizon

	if *samplesPath == "" {
		p, err := trace.LogPath()
		if err != nil {
			fatal(err)
		}
		*samplesPath = p
	}

	calls, files, err := collect(*transcripts)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "read %d transcripts, %d Bash calls\n", files, len(calls))

	samples, err := readSamples(*samplesPath)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "read %d samples, %s .. %s\n", len(samples),
		stamp(samples, 0), stamp(samples, len(samples)-1))

	markOverlaps(calls)
	label(calls, samples)

	w := io.Writer(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		w = f
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, c := range calls {
		if err := enc.Encode(c); err != nil {
			fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		fatal(err)
	}
	summarise(calls)
}

func stamp(s []sample, i int) string {
	if len(s) == 0 {
		return "-"
	}
	return s[i].t.UTC().Format(time.RFC3339)
}

// --- collection -------------------------------------------------------------------------

// entry is the subset of a transcript line this analysis reads.
type entry struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	SessionID   string `json:"sessionId"`
	IsSidechain bool   `json:"isSidechain"`
	CWD         string `json:"cwd"`
	Message     struct {
		Content []struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input struct {
				Command    string `json:"command"`
				Background bool   `json:"run_in_background"`
			} `json:"input"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	} `json:"message"`
}

func collect(root string) ([]*Call, int, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable directory is not a reason to abandon the corpus
		}
		if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(files)

	var all []*Call
	for _, f := range files {
		cs, err := collectFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", f, err)
			continue
		}
		all = append(all, cs...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Start < all[j].Start })
	return all, len(files), nil
}

// collectFile pairs each tool_use(Bash) with its tool_result inside one transcript.
//
// The pair is the whole point: the tool_use timestamp is when the command was issued and the
// tool_result timestamp is when its output came back, so the two bracket the seconds the
// command was on the CPU. An unpaired tool_use is a call that never returned in this
// transcript — an interrupted session, usually — and is dropped rather than given an invented
// end.
func collectFile(path string) ([]*Call, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	project := filepath.Base(filepath.Dir(path))
	pending := map[string]*Call{}
	var done []*Call

	// ReadString rather than a Scanner: transcript lines carry tool output and images and
	// routinely exceed any buffer size worth guessing at.
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			var e entry
			if json.Unmarshal([]byte(line), &e) == nil {
				for _, b := range e.Message.Content {
					switch {
					case b.Type == "tool_use" && b.Name == "Bash":
						pending[b.ID] = &Call{
							Session:    e.SessionID,
							Project:    project,
							Sidechain:  e.IsSidechain,
							Background: b.Input.Background,
							Start:      e.Timestamp,
							CWD:        e.CWD,
							Cmd:        b.Input.Command,
						}
					case b.Type == "tool_result":
						c, ok := pending[b.ToolUseID]
						if !ok {
							continue
						}
						delete(pending, b.ToolUseID)
						c.End = e.Timestamp
						if c.CWD == "" {
							c.CWD = e.CWD
						}
						t0, err0 := parse(c.Start)
						t1, err1 := parse(c.End)
						if err0 != nil || err1 != nil || !t1.After(t0) {
							continue
						}
						c.DurS = t1.Sub(t0).Seconds()
						done = append(done, c)
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return done, err
		}
	}
	return done, nil
}

func parse(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// --- labelling --------------------------------------------------------------------------

func readSamples(path string) ([]sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []sample
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			var rec trace.Record
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Kind == trace.KindSample && rec.Sample != nil {
				t, perr := parse(rec.TS)
				if perr == nil {
					child := rec.Sample.ClaudeCPU - rec.Sample.ClaudeCPURoots
					if child < 0 {
						// The two figures come from one walk, so roots exceeding the tree is
						// not physically meaningful; it is float noise on a near-zero gap.
						child = 0
					}
					out = append(out, sample{t: t, child: child, root: rec.Sample.ClaudeCPURoots, cpu: rec.Sample.CPU})
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t.Before(out[j].t) })
	return out, nil
}

// span returns the half-open interval of a call for in-flight purposes.
//
// A foreground call occupies the window between its tool_use and its tool_result. A
// backgrounded one has no recorded end, so it occupies backgroundHorizon from its start —
// long enough to cover the builds and servers these are typically used for, and deliberately
// generous because the failure it prevents (silently attributing a background build's CPU to
// whatever short command ran alongside it) is worse than the evidence it costs.
func (c *Call) window() (time.Time, time.Time, bool) {
	t0, err := parse(c.Start)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	if c.Background {
		return t0, t0.Add(backgroundHorizon), true
	}
	t1, err := parse(c.End)
	if err != nil || !t1.After(t0) {
		return time.Time{}, time.Time{}, false
	}
	return t0, t1, true
}

// markOverlaps counts, for every call, how many other Bash calls overlapped its window.
//
// This is descriptive only. It is NOT the attribution rule — see label, and the package
// comment for why per-command exclusivity was measured to be biased against long commands and
// rejected.
func markOverlaps(calls []*Call) {
	type iv struct {
		lo, hi time.Time
		c      *Call
	}
	ivs := make([]iv, 0, len(calls))
	for _, c := range calls {
		lo, hi, ok := c.window()
		if !ok {
			continue
		}
		ivs = append(ivs, iv{lo, hi, c})
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].lo.Before(ivs[j].lo) })
	for i := range ivs {
		for j := i + 1; j < len(ivs); j++ {
			if ivs[j].lo.After(ivs[i].hi) {
				break
			}
			ivs[i].c.OverlapN++
			ivs[j].c.OverlapN++
		}
	}
}

// label attributes each sample to at most one call and derives the cost labels.
//
// The sweep walks samples in time order maintaining the set of calls in flight. A sample with
// exactly one call in flight is that call's evidence; a sample with none is baseline material;
// a sample with two or more is discarded, because nothing in this instrument can say which of
// them the CPU belonged to.
func label(calls []*Call, samples []sample) {
	if len(samples) == 0 {
		return
	}
	type iv struct {
		lo, hi time.Time
		c      *Call
	}
	ivs := make([]iv, 0, len(calls))
	for _, c := range calls {
		lo, hi, ok := c.window()
		if !ok {
			continue
		}
		ivs = append(ivs, iv{lo, hi, c})
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].lo.Before(ivs[j].lo) })

	// idle[i] is true when no call at all was in flight during samples[i]: the baseline pool.
	idle := make([]bool, len(samples))
	active := map[*Call]time.Time{} // call -> its end
	next := 0
	for i, s := range samples {
		for next < len(ivs) && !ivs[next].lo.After(s.t) {
			active[ivs[next].c] = ivs[next].hi
			next++
		}
		for c, end := range active {
			if end.Before(s.t) {
				delete(active, c)
			}
		}
		switch len(active) {
		case 0:
			idle[i] = true
		case 1:
			for c := range active {
				c.AttrSamplesN++
				c.AttrChildMean += s.child
				c.AttrCoreSec += s.child
				if s.child > c.AttrChildPeak {
					c.AttrChildPeak = s.child
				}
			}
		}
	}

	lo, hi := samples[0].t, samples[len(samples)-1].t
	for _, c := range calls {
		if c.AttrSamplesN > 0 {
			c.AttrChildMean /= float64(c.AttrSamplesN)
		}
		t0, t1, ok := c.window()
		if !ok || c.Background || t0.Before(lo) || t1.After(hi) {
			continue
		}
		in := window(samples, t0, t1)
		if len(in) == 0 {
			continue
		}
		c.Labelled = true
		c.SamplesN = len(in)
		if c.DurS > 0 {
			c.Coverage = float64(len(in)) / c.DurS
			c.AttrFrac = float64(c.AttrSamplesN) / c.DurS
		}
		for _, s := range in {
			if s.child > c.ChildPeak {
				c.ChildPeak = s.child
			}
			if s.root > c.RootPeak {
				c.RootPeak = s.root
			}
			c.ChildMean += s.child
			c.RootMean += s.root
			c.HostCPUMean += s.cpu
		}
		n := float64(len(in))
		c.ChildMean /= n
		c.RootMean /= n
		c.HostCPUMean /= n
		c.ChildCoreSec = c.ChildMean * c.DurS
		c.EstCoreSec = c.AttrChildMean * c.DurS

		// The baseline is the idle seconds before the call started. Idle rather than merely
		// earlier: subtracting a window that had another build running in it would report a
		// negative cost for a command that genuinely cost something.
		lo2 := sort.Search(len(samples), func(i int) bool { return !samples[i].t.Before(t0.Add(-baselineWindow)) })
		hi2 := sort.Search(len(samples), func(i int) bool { return samples[i].t.After(t0) })
		for i := lo2; i < hi2; i++ {
			if idle[i] {
				c.BaseChildMean += samples[i].child
				c.BaseSamplesN++
			}
		}
		if c.BaseSamplesN > 0 {
			c.BaseChildMean /= float64(c.BaseSamplesN)
		}
		c.MargChildMean = c.AttrChildMean - c.BaseChildMean
		c.MargCoreSec = c.MargChildMean * c.DurS
	}
}

// window returns the samples in [lo,hi].
func window(samples []sample, lo, hi time.Time) []sample {
	i := sort.Search(len(samples), func(i int) bool { return !samples[i].t.Before(lo) })
	j := sort.Search(len(samples), func(j int) bool { return samples[j].t.After(hi) })
	if i >= j {
		return nil
	}
	return samples[i:j]
}

// --- summary ----------------------------------------------------------------------------

func summarise(calls []*Call) {
	var labelled, attributable, fittable, side, bg int
	var attrSecs float64
	for _, c := range calls {
		if c.Sidechain {
			side++
		}
		if c.Background {
			bg++
		}
		if c.Labelled {
			labelled++
		}
		if c.AttrSamplesN > 0 {
			attributable++
			attrSecs += float64(c.AttrSamplesN)
		}
		if c.Labelled && c.AttrSamplesN >= minAttrSamples {
			fittable++
		}
	}
	fmt.Fprintf(os.Stderr, "\ncalls             %6d\n", len(calls))
	fmt.Fprintf(os.Stderr, "  sidechain       %6d\n", side)
	fmt.Fprintf(os.Stderr, "  background      %6d  (never labelled: window is not runtime)\n", bg)
	fmt.Fprintf(os.Stderr, "  labelled        %6d  (window inside sampler coverage)\n", labelled)
	fmt.Fprintf(os.Stderr, "  attributable    %6d  (>=1 second alone on the machine, %.0f seconds total)\n", attributable, attrSecs)
	fmt.Fprintf(os.Stderr, "  fittable        %6d  <- labelled with >=%d attributable seconds\n", fittable, minAttrSamples)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bashcorpus:", err)
	os.Exit(1)
}
