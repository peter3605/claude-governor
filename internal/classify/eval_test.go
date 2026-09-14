package classify

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
)

// TestEvaluateAgainstFullCorpus recomputes every number in docs/bash-corpus.md from the
// shipped classifier.
//
// It is skipped unless GOVERNOR_CORPUS points at the output of tools/bashcorpus, because the
// corpus is 20,000 commands off a real machine and does not belong in the repository. The
// checked-in fixture in testdata/ is a stratified sample of it and is what CI asserts against;
// this is how the document's figures are produced and re-produced.
//
//	go build -o /tmp/bashcorpus ./tools/bashcorpus && /tmp/bashcorpus -out /tmp/corpus.jsonl
//	GOVERNOR_CORPUS=/tmp/corpus.jsonl go test ./internal/classify/ -run FullCorpus -v
//
// The reason it exists at all rather than a throwaway script: the first pass of this analysis
// was done in a prototype whose runner-unwrapping silently mapped `uv run pytest` to a binary
// called `run`, which is in no table, so 431 calls quietly stopped being gated and every error
// rate came out flattering. Numbers in the document have to come from the code that ships.
func TestEvaluateAgainstFullCorpus(t *testing.T) {
	path := os.Getenv("GOVERNOR_CORPUS")
	if path == "" {
		t.Skip("set GOVERNOR_CORPUS to the output of tools/bashcorpus")
	}
	rows := readCorpus(t, path)

	// The threshold that defines "expensive", and the reasoning, are in the document: peak
	// child cores >= 1.0 selects 7.2% of labelled calls and is the knee of the distribution.
	const expensiveAt = 1.0

	var all, fittable int
	var gatedAll int
	var tp, fn, fp, tn int
	var costCaught, costMissed float64
	byShape := map[string][]corpusRow{}

	for _, r := range rows {
		if r.synthetic() {
			continue
		}
		all++
		hit := Classify(r.Cmd).Gated
		if hit {
			gatedAll++
		}
		if !r.Labelled || r.AttrSamplesN < 2 {
			continue
		}
		fittable++
		exp := r.AttrChildPeak >= expensiveAt
		switch {
		case exp && hit:
			tp++
			costCaught += r.EstCoreSec
		case exp && !hit:
			fn++
			costMissed += r.EstCoreSec
		case !exp && hit:
			fp++
		default:
			tn++
		}
		byShape[shapeOf(r.Cmd)] = append(byShape[shapeOf(r.Cmd)], r)
	}

	fmt.Printf("\ncorpus: %d Bash calls, %d fittable (labelled with >=2 attributable seconds)\n", all, fittable)
	fmt.Printf("expensive = peak child cores >= %.1f: %d of %d fittable (%.1f%%)\n",
		expensiveAt, tp+fn, fittable, pct(tp+fn, fittable))
	fmt.Printf("\n  catch rate        %5.1f%%  (%d of %d expensive commands)\n", pct(tp, tp+fn), tp, tp+fn)
	fmt.Printf("  false positives   %5.1f%%  (%d of %d cheap commands)\n", pct(fp, fp+tn), fp, fp+tn)
	fmt.Printf("  precision         %5.1f%%\n", pct(tp, tp+fp))
	fmt.Printf("  gated share       %5.1f%%  (%d of ALL %d Bash calls)\n", pct(gatedAll, all), gatedAll, all)
	fmt.Printf("  cost captured     %5.1f%%  of measured child core-seconds\n",
		100*costCaught/(costCaught+costMissed))

	// A held-out split, because GOV-14 clause 2 asks for the error rates on one.
	//
	// Chronological rather than random, and that choice matters: a random split puts commands
	// from the same session on both sides, and sessions repeat commands nearly verbatim, so a
	// random hold-out scores its own training data. Splitting by time simulates the only thing
	// that is actually being claimed — that a predicate built from what this machine has done
	// keeps working on what it does next.
	//
	// The shipped predicate is a table rather than a fitted model, so nothing was estimated from
	// the training half; the split still earns its place because the table WAS informed by the
	// corpus (dropping ruff, unwrapping runners) and a figure quoted from the data that shaped
	// it is not evidence of anything.
	var labelled []corpusRow
	for _, r := range rows {
		if !r.synthetic() && r.Labelled && r.AttrSamplesN >= 2 {
			labelled = append(labelled, r)
		}
	}
	sort.Slice(labelled, func(i, j int) bool { return labelled[i].Start < labelled[j].Start })
	cut := 7 * len(labelled) / 10
	for _, half := range []struct {
		name string
		rs   []corpusRow
	}{{"earlier 70%", labelled[:cut]}, {"HELD OUT, last 30%", labelled[cut:]}} {
		var htp, hfn, hfp, htn int
		for _, r := range half.rs {
			hit := Classify(r.Cmd).Gated
			exp := r.AttrChildPeak >= expensiveAt
			switch {
			case exp && hit:
				htp++
			case exp && !hit:
				hfn++
			case !exp && hit:
				hfp++
			default:
				htn++
			}
		}
		fmt.Printf("  %-20s n=%4d  catch %5.1f%% (%d/%d)  false positives %5.1f%% (%d/%d)\n",
			half.name, len(half.rs), pct(htp, htp+hfn), htp, htp+hfn, pct(hfp, hfp+htn), hfp, hfp+htn)
	}

	// The ceiling, stated as a split rather than as an average. It is the single most useful
	// number for GOV-15: it says the predicate is not failing to recognise build tools, it is
	// failing on commands that contain none, and no table edit can fix that.
	var expWith, caughtWith, expWithout, caughtWithout int
	for _, r := range rows {
		if r.synthetic() || !r.Labelled || r.AttrSamplesN < 2 || r.AttrChildPeak < expensiveAt {
			continue
		}
		hit := Classify(r.Cmd).Gated
		if shapeOf(r.Cmd) == "(no toolchain)" {
			expWithout++
			if hit {
				caughtWithout++
			}
			continue
		}
		expWith++
		if hit {
			caughtWith++
		}
	}
	fmt.Printf("\nthe ceiling:\n")
	fmt.Printf("  expensive commands naming a build tool:  %3d, caught %3d (%.1f%%)\n",
		expWith, caughtWith, pct(caughtWith, expWith))
	fmt.Printf("  expensive commands naming none:          %3d, caught %3d (%.1f%%)\n",
		expWithout, caughtWithout, pct(caughtWithout, expWithout))

	fmt.Printf("\ncost classes, measured peak child cores by shape:\n")
	fmt.Printf("  %-22s %6s %9s %9s %9s %8s\n", "shape", "n", "med", "p90", "max", "exp%")
	type se struct {
		name string
		rs   []corpusRow
	}
	var shapes []se
	for k, v := range byShape {
		if len(v) >= 5 {
			shapes = append(shapes, se{k, v})
		}
	}
	sort.Slice(shapes, func(i, j int) bool {
		return quantile(shapes[i].rs, 0.9) > quantile(shapes[j].rs, 0.9)
	})
	for _, s := range shapes {
		e := 0
		for _, r := range s.rs {
			if r.AttrChildPeak >= expensiveAt {
				e++
			}
		}
		fmt.Printf("  %-22s %6d %9.2f %9.2f %9.2f %7.1f%%\n", s.name, len(s.rs),
			quantile(s.rs, 0.5), quantile(s.rs, 0.9), quantile(s.rs, 1.0), pct(e, len(s.rs)))
	}
}

type corpusRow struct {
	Cmd           string  `json:"cmd"`
	Start         string  `json:"start"`
	Labelled      bool    `json:"labelled"`
	AttrSamplesN  int     `json:"attr_samples_n"`
	AttrChildPeak float64 `json:"attr_child_peak"`
	EstCoreSec    float64 `json:"est_coresec"`
	DurS          float64 `json:"dur_s"`
}

// synthetic excludes this project's own thermal stress probes.
//
// They are `for i in $(seq 1 16); do yes > /dev/null & done` from GOV-13, and they are the two
// most expensive commands in the whole corpus — together 59% of all measured core-seconds.
// They are instrumentation rather than workload: nobody else's machine runs them, and fitting
// a predicate to them would teach it `yes` and `seq`. Excluded, and said out loud rather than
// filtered quietly.
func (r corpusRow) synthetic() bool {
	c := r.Cmd
	return (contains(c, "yes >") || contains(c, "yes>")) && contains(c, "seq")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// shapeOf names a command by the gated binaries it invokes, for the cost-class table.
func shapeOf(cmd string) string {
	seen := map[string]bool{}
	var names []string
	for _, inv := range invocations(cmd) {
		if light[pair{inv.bin, inv.sub}] {
			continue
		}
		if _, ok := classOf(inv.bin, inv.sub); ok {
			if !seen[inv.bin] {
				seen[inv.bin] = true
				names = append(names, inv.bin)
			}
		}
	}
	if len(names) == 0 {
		return "(no toolchain)"
	}
	sort.Strings(names)
	out := names[0]
	for _, n := range names[1:] {
		out += "+" + n
	}
	return out
}

func quantile(rs []corpusRow, q float64) float64 {
	v := make([]float64, len(rs))
	for i, r := range rs {
		v[i] = r.AttrChildPeak
	}
	sort.Float64s(v)
	i := int(q * float64(len(v)))
	if i >= len(v) {
		i = len(v) - 1
	}
	return v[i]
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func readCorpus(t *testing.T, path string) []corpusRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()
	var out []corpusRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		var r corpusRow
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	return out
}
