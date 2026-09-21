package classify

import "testing"

// TestShapeRunKind goes from a real command line through Classify to a category, rather than
// asserting against hand-written shapes. Three of these cases were wrong in the first draft of
// testrun.go, and they were wrong because the shape was assumed instead of probed.
func TestShapeRunKind(t *testing.T) {
	for _, c := range []struct {
		cmd   string
		shape string
		want  RunKind
	}{
		// The two the shipped grep disagreed about. `go test` it caught; `pytest` it missed,
		// and pytest is 51 of this window's 190 records.
		{"go test ./...", "go test", TestRun},
		{"pytest -q", "pytest", TestRun},
		{"python -m pytest tests/", "pytest", TestRun},
		// Suite binaries keep their category through a subcommand.
		{"vitest run", "vitest run", TestRun},
		{"coverage run -m pytest", "coverage run", TestRun},
		{"tox", "tox", TestRun},
		// The sub vocabulary carries every other ecosystem without naming it.
		{"cargo test", "cargo test", TestRun},
		{"npm test", "npm test", TestRun},
		{"dotnet test", "dotnet test", TestRun},
		{"xcodebuild test -scheme App", "xcodebuild test", TestRun},
		{"go test -bench=. ./...", "go test", TestRun},
		// Builds and checks are not suites.
		{"go build ./...", "go build", NotTestRun},
		{"go vet ./...", "go", NotTestRun},
		{"mypy .", "mypy", NotTestRun},
		{"make", "make", NotTestRun},
		{"xcodebuild -scheme App", "xcodebuild", NotTestRun},
		// `cargo check` type-checks and runs nothing. Counting it would report a suite that
		// never executed, which is the error clause 3 exists to catch.
		{"cargo check", "cargo check", NotTestRun},
		// Ambiguous: what ran is a name the shape deliberately does not carry.
		{"npx jest", "npx", AmbiguousRun},
		{"npx some-local-bin", "npx", AmbiguousRun},
		{"npm run test:unit", "npm run", AmbiguousRun},
	} {
		r := Classify(c.cmd)
		if !r.Gated || len(r.Shapes) == 0 {
			t.Errorf("%q: not gated, so it never reaches the trace (shapes %v)", c.cmd, r.Shapes)
			continue
		}
		if r.Shapes[0] != c.shape {
			t.Errorf("%q: shape = %q, want %q", c.cmd, r.Shapes[0], c.shape)
		}
		if got := ShapeRunKind(r.Shapes[0]); got != c.want {
			t.Errorf("%q: shape %q is %v, want %v", c.cmd, r.Shapes[0], got, c.want)
		}
	}
}

// TestUnittestIsInvisibleToClauseThree pins a blind spot rather than a behaviour. If this ever
// fails, `python -m unittest` has become gated and suiteBin must gain "unittest" in the same
// change — otherwise clause 3 starts undercounting again in a new way.
func TestUnittestIsInvisibleToClauseThree(t *testing.T) {
	if r := Classify("python -m unittest discover"); r.Gated {
		t.Fatalf("python -m unittest is now gated (shapes %v) — add it to suiteBin and to the "+
			"blind-spot list in docs/advice.md", r.Shapes)
	}
}

// TestRunKindVocabularyIsReachable keeps the analysis vocabulary honest against the classifier's
// own tables. An entry that cannot be produced is worse than a missing one: it reads as coverage
// the instrument does not have.
func TestRunKindVocabularyIsReachable(t *testing.T) {
	for sub := range suiteSub {
		if !recordableSub[sub] {
			t.Errorf("suiteSub has %q but recordableSub does not, so no shape can carry it", sub)
		}
	}
	for bin := range suiteBin {
		if _, ok := classOf(bin, ""); !ok {
			t.Errorf("suiteBin has %q but it is not gated, so it never reaches the trace", bin)
		}
	}
	for bin := range pkgRun {
		if _, ok := classOf(bin, ""); !ok {
			t.Errorf("pkgRun has %q but it is not gated, so it never reaches the trace", bin)
		}
	}
	if _, ok := classOf("npx", ""); !ok {
		t.Error("npx is not gated, so the ambiguous bucket is unreachable")
	}
	// The quirk the ambiguous bucket exists for: npx is a runner, so it is expected to unwrap,
	// and it does not, because unwrapping needs a `run`/`exec` keyword npx does not use.
	if !runner["npx"] {
		t.Error("npx left the runner set — recheck whether `npx jest` now unwraps to `jest`")
	}
	if dispatcher["npx"] {
		t.Error("npx became a dispatcher, so `npx jest` now unwraps and is no longer ambiguous")
	}
}
