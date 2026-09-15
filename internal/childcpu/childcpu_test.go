package childcpu

import "testing"

// The bucketing is the part of this package that decides what a number MEANS, and it was
// moved here out of tools/childcpu so that tools/sweep could take the same measurement
// (GOV-31 clause 1). A move is exactly when a pure function should acquire a test: the
// build-test bucket is what both the Phase 0 decomposition and the sweep's core-second
// figure are read from, so a silent change to it would rewrite conclusions in two documents
// at once.
func TestBucketOfSortsTheWorkTheGovernorCaresAbout(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		// MCP is checked first and wins over anything else in the arguments, because a
		// stdio server launched by a session IS a descendant and the distinction from a
		// compiler is the whole Phase 0 finding.
		{"node", "node /x/server-mcp.js", "mcp"},
		{"python3", "python3 -m modelcontextprotocol.server", "mcp"},

		// Compilers, linkers, test runners and build drivers.
		{"go", "go build -a ./...", "build-test"},
		{"compile", "compile -p main", "build-test"},
		{"clang", "clang -c foo.c", "build-test"},
		{"pytest", "pytest -q", "build-test"},
		{"cargo", "cargo build", "build-test"},
		// Matched on arguments rather than name: the go toolchain's work often appears
		// under a cache path rather than under a recognisable binary name.
		{"cc1", "/x/go-build123/b001/exe", "build-test"},
		{"sh", "sh -c 'go test ./...'", "build-test"},

		{"git", "git status", "vcs-search"},
		{"rg", "rg needle", "vcs-search"},
		{"zsh", "zsh -i", "shell"},
		{"ssh", "ssh host", "other"},
	}
	for _, c := range cases {
		if got := BucketOf(c.name, c.args); got != c.want {
			t.Errorf("BucketOf(%q, %q) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

// A report with nothing in it must read as nothing, not as a division by zero. The sweep
// calls BuildTestCoreSec on every arm including ones where the window was too short to
// collect a second sample.
func TestAnEmptyReportIsZeroRatherThanAPanic(t *testing.T) {
	var r Report
	if got := r.BuildTestCoreSec(); got != 0 {
		t.Errorf("BuildTestCoreSec on an empty report = %v, want 0", got)
	}
	if got := r.DescendantCoreSec(); got != 0 {
		t.Errorf("DescendantCoreSec on an empty report = %v, want 0", got)
	}
}

// The measuring process's own subtree is excluded from the work under test, and it is
// bucketed separately rather than dropped so that a reader can see it was non-zero.
func TestSelfMeasurementIsExcludedFromTheTotals(t *testing.T) {
	r := Report{CoreSecs: map[Key]float64{
		{"build-test", "go"}:                12.5,
		{SelfMeasurementBucket, "childcpu"}: 3.0,
	}}
	if got := r.BuildTestCoreSec(); got != 12.5 {
		t.Errorf("BuildTestCoreSec = %v, want 12.5", got)
	}
	if got := r.DescendantCoreSec(); got != 12.5 {
		t.Errorf("DescendantCoreSec = %v, want 12.5; the measurement must not count itself", got)
	}
}
