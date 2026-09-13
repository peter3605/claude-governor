package classify

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// record is one line of testdata/commands.jsonl: a REAL command from this machine's
// transcripts, with the cost that was actually measured while it ran.
//
// The fixture is drawn from the corpus rather than written by hand, because GOV-14 clause 3
// requires exactly that. A hand-written list of commands tests the author's idea of what a
// shell command looks like; this tests the shell commands that happened.
type record struct {
	Group     string  `json:"group"`
	Cmd       string  `json:"cmd"`
	Peak      float64 `json:"peak"`
	CoreSec   float64 `json:"coresec"`
	DurS      float64 `json:"dur_s"`
	Expensive bool    `json:"expensive"`
}

func load(t *testing.T) []record {
	t.Helper()
	f, err := os.Open("testdata/commands.jsonl")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var out []record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return out
}

// TestTheCommonCaseIsNotGated is GOV-14 clause 3, and it is the clause that decides whether
// anyone can bear to have this installed.
//
// The fixture's "cheap" group is every command in the corpus built only of tools that read
// and move bytes around — cat, grep, ls, git, head, sed and their kind. At 20,105 Bash calls
// against 86 gate-matched ones, a predicate that leases these has made the terminal slower for
// no benefit at all.
func TestTheCommonCaseIsNotGated(t *testing.T) {
	var gated []string
	total := 0
	for _, r := range load(t) {
		if r.Group != "cheap" {
			continue
		}
		total++
		if Classify(r.Cmd).Gated {
			gated = append(gated, r.Cmd)
		}
	}
	if total == 0 {
		t.Fatal("fixture carries no cheap commands")
	}
	if len(gated) > 0 {
		for i, c := range gated {
			if i == 5 {
				t.Errorf("... and %d more", len(gated)-5)
				break
			}
			t.Errorf("cheap command gated: %s", firstLine(c))
		}
		t.Fatalf("%d of %d cheap commands would take a lease; clause 3 requires none", len(gated), total)
	}
}

// TestNamedCheapCommandsAreNeverGated pins the five commands GOV-14 names in clause 3 by name.
//
// The fixture covers the general case; this covers the specific promise, so that a future
// table edit that happens to miss the fixture still cannot break it.
func TestNamedCheapCommandsAreNeverGated(t *testing.T) {
	for _, cmd := range []string{
		"cat foo.txt",
		"grep -rn needle .",
		"git status",
		"git status --short",
		"ls -la",
		"echo hello",
		"cd /tmp && ls",
		"sed -n 1,40p file.go",
		"head -20 x.log | tail -5",
		"go version",
		"go env GOPATH",
		"npm ls",
		"docker ps",
		"uv run python -c \"print(1)\"",
	} {
		if got := Classify(cmd); got.Gated {
			t.Errorf("Classify(%q).Gated = true, want false (class %s)", cmd, got.Class)
		}
	}
}

// TestBuildShapedCommandsAreGated is the other half: the predicate must not be decorative.
func TestBuildShapedCommandsAreGated(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want Class
	}{
		{"go test ./...", Medium},
		{"cd ~/repo && go test ./... 2>&1 | tail -20", Medium},
		{"go build ./...", Medium},
		{"cargo build --release", Heavy},
		{"make check", Heavy},
		{"xcodebuild -scheme App test | xcpretty", Heavy},
		{"uv run pytest -q", Medium},
		{".venv/bin/python -m pytest -q 2>&1 | tail -5", Medium},
		{"python3 -m pytest tests/", Medium},
		{"npm run build", Medium},
		{"git clone https://example.com/x.git", Medium},
		{"pip install -r requirements.txt", Medium},
		{"LC_ALL=C time go test -race ./...", Medium},
	} {
		got := Classify(tc.cmd)
		if !got.Gated {
			t.Errorf("Classify(%q).Gated = false, want true", tc.cmd)
			continue
		}
		if got.Class != tc.want {
			t.Errorf("Classify(%q).Class = %s, want %s", tc.cmd, got.Class, tc.want)
		}
	}
}

// TestHeredocBodiesAreNotCommands guards the defect that was visible in the first measurement.
//
// `python3 - <<'PY' ... PY` is one of the commonest shapes in this corpus. Before the scanner
// stripped heredoc bodies, the Python source inside was read as shell, which put a "binary"
// called `1` above `go` in the cost ranking. It is guarded here rather than left to the
// fixture because the failure is silent: the classification of the OUTER command stays
// plausible while the evidence underneath it is wrong.
func TestHeredocBodiesAreNotCommands(t *testing.T) {
	cmd := "python3 - <<'PY'\nimport subprocess\nmake = 1\nsubprocess.run(['cargo','build'])\nPY"
	if got := Classify(cmd); got.Gated {
		t.Errorf("heredoc body leaked into classification: %+v", got)
	}
	// A heredoc that really is followed by a build must still gate on the build.
	cmd2 := "cat > f.py <<'PY'\nimport os\nPY\ngo test ./..."
	if got := Classify(cmd2); !got.Gated {
		t.Errorf("command after a heredoc was missed: %+v", got)
	}
}

// TestQuotingDoesNotCreateCommands: `echo "a && cargo build"` runs echo, and nothing else.
func TestQuotingDoesNotCreateCommands(t *testing.T) {
	for _, cmd := range []string{
		`echo "a && cargo build"`,
		`echo 'go test ./...'`,
		`grep -n "make check" Makefile`,
	} {
		if got := Classify(cmd); got.Gated {
			t.Errorf("Classify(%q).Gated = true, want false", cmd)
		}
	}
}

// TestSubstitutionsAreScanned: the build inside $(...) is still a build.
func TestSubstitutionsAreScanned(t *testing.T) {
	if got := Classify("echo $(go build ./... && echo done)"); !got.Gated {
		t.Errorf("command substitution not scanned: %+v", got)
	}
}

// TestErrorRatesDoNotRegress holds the predicate to the numbers GOV-14 shipped.
//
// Both directions are asserted, because a predicate with only one of them is trivially gamed:
// gate everything and the catch rate is perfect, gate nothing and the false-positive rate is.
// The thresholds are deliberately a little looser than the measured figures so that ordinary
// table edits do not fail the build, but tight enough that losing a whole ecosystem does.
//
// Measured on the full corpus at the time of writing (docs/bash-corpus.md): catch 53.0%,
// false positives 35.7%. The fixture is a stratified sample rather than the whole corpus — it
// over-samples expensive commands so that the catch rate is measured on more than a handful —
// so its figures sit near but not on the document's: catch 55.8%, false positives 37.8%.
//
// THE FALSE-POSITIVE CEILING IS HIGH BECAUSE THE MEASURED RATE IS HIGH, and that is recorded
// as a known weakness rather than tuned away. Most of it is real build tools invoked cheaply:
// `go` is gated 303 times in the labelled set and only 13.5% of those were expensive, `pytest`
// 189 times at 2.6%. Lowering it means either dropping a whole ecosystem from the table —
// which fails on the next machine, where that ecosystem is the expensive one — or learning
// per-host cost, which is GOV-15's job and not a static table's.
func TestErrorRatesDoNotRegress(t *testing.T) {
	var tp, fn, fp, tn int
	var costCaught, costMissed float64
	for _, r := range load(t) {
		if r.Group != "labelled" {
			continue
		}
		hit := Classify(r.Cmd).Gated
		switch {
		case r.Expensive && hit:
			tp++
			costCaught += r.CoreSec
		case r.Expensive && !hit:
			fn++
			costMissed += r.CoreSec
		case !r.Expensive && hit:
			fp++
		default:
			tn++
		}
	}
	if tp+fn == 0 || fp+tn == 0 {
		t.Fatal("fixture is missing one of the two classes")
	}
	catch := float64(tp) / float64(tp+fn)
	fpr := float64(fp) / float64(fp+tn)
	cost := costCaught / (costCaught + costMissed)
	t.Logf("catch %.1f%% (%d/%d), false positives %.1f%% (%d/%d), cost caught %.1f%%",
		100*catch, tp, tp+fn, 100*fpr, fp, fp+tn, 100*cost)

	if catch < 0.40 {
		t.Errorf("catch rate %.1f%% below floor of 40%%: the predicate is becoming decorative", 100*catch)
	}
	if fpr > 0.45 {
		t.Errorf("false-positive rate %.1f%% above ceiling of 45%%: the predicate is becoming unusable", 100*fpr)
	}
	if cost < 0.65 {
		t.Errorf("cost captured %.1f%% below floor of 65%%: the predicate is missing the tail it exists for", 100*cost)
	}
}

// TestClassifyIsCheapEnoughToRunOnEveryCall.
//
// The gate calls this before it contacts the daemon, on every Bash call — between 184 and
// 2,221 times a day on this machine. If classification is not free the latency budget is spent
// before the decision is made, and the whole reason it lives gate-side rather than in the
// daemon disappears.
//
// A generous ceiling rather than a tight one: this asserts the right ORDER OF MAGNITUDE, not a
// benchmark. Wall-clock assertions that measure the runner's spare capacity are how GOV-B04,
// GOV-B08 and GOV-B10 all went, and a per-call ceiling of 50µs is far enough above the real
// figure (single-digit µs) that a loaded CI box does not trip it.
func TestClassifyIsCheapEnoughToRunOnEveryCall(t *testing.T) {
	recs := load(t)
	res := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			Classify(recs[i%len(recs)].Cmd)
		}
	})
	per := res.T.Nanoseconds() / int64(res.N)
	t.Logf("%d ns/op over %d real commands", per, len(recs))
	if per > 50_000 {
		t.Errorf("Classify costs %d ns/op; it runs on every Bash call and must stay cheap", per)
	}
}

func BenchmarkClassify(b *testing.B) {
	f, err := os.Open("testdata/commands.jsonl")
	if err != nil {
		b.Skip("no fixture")
	}
	defer f.Close()
	var cmds []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			cmds = append(cmds, r.Cmd)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Classify(cmds[i%len(cmds)])
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
