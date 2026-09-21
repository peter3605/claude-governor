package classify

import "strings"

// RunKind categorises a RECORDED SHAPE for GOV-17 clause 3, which has to notice a session that
// responded to "ask before you build" by quietly not running its tests.
//
// It is analysis, not hot path. Nothing in the gate calls it: the gate records Shapes and stops,
// and this reads those shapes back at window close. That separation is deliberate and is stated
// on Result.Shapes — what counts as a test run can be revised here without touching the sizing
// decision or reopening the classifier's measured error rate.
//
// WHY IT IS NOT A GREP. docs/advice.md shipped the clause-3 count as
// `grep -E '(^|\s)(test|check|bench)$'`, and on the first four days of real records that query
// returned 93 where the window held 144 test-suite runs. It matched `go test` and silently
// missed all 51 `pytest` invocations, because `pytest` does not end in a separate `test` word.
// Clause 3 is the FIRST gate in that document's reading rule, so a query blind to a third of
// the evidence would have scored a go-test-only finding as the whole answer.
//
// EVERY RULE BELOW WAS PROBED AGAINST Classify RATHER THAN READ OFF THE TABLES, and three
// readings that looked obvious were wrong: `npx jest` does not unwrap to `jest`, `vitest run`
// and `coverage run` keep a subcommand, and `python -m unittest` is not gated at all. The
// TestShapeRunKind cases are those probes, kept so the next revision starts from behaviour.
type RunKind int

const (
	// NotTestRun is a gated invocation that is not a test suite: `go build`, `go` (i.e.
	// `go vet`, `go generate`), `make`, `xcodebuild`, `clang`, `tsc`, and the static
	// checkers `mypy` and `pylint`.
	//
	// The checkers are a judgment call and it is recorded rather than hidden: clause 3 says
	// "test-suite runs", and a type check is not a suite. A session that stopped running
	// mypy would also be worse at its job, but it would be worse in a way this clause does
	// not name, so it is counted separately rather than folded in to make the number bigger.
	NotTestRun RunKind = iota
	// TestRun is a shape that can only be a test suite.
	TestRun
	// AmbiguousRun is a shape that may or may not be one, where what ran is a name the shape
	// deliberately does not carry. Reported as its own bucket and never silently merged:
	// folding it into TestRun inflates the denominator clause 3 is measured against, and
	// folding it into NotTestRun hides a real drop.
	AmbiguousRun
)

func (k RunKind) String() string {
	switch k {
	case TestRun:
		return "test-run"
	case AmbiguousRun:
		return "ambiguous"
	default:
		return "not-test-run"
	}
}

// suiteSub is the subcommand vocabulary that means "a test suite ran", drawn from the verbs
// recordableSub already admits. It catches `go test`, `cargo test`, `npm test`, `dotnet test`
// and `xcodebuild test` without naming any of those binaries.
//
// `check` is deliberately absent although recordableSub carries it. `cargo check` and its kind
// type-check without running anything, so counting them as test runs would report a suite that
// never executed — the precise error clause 3 exists to catch, made by the instrument instead
// of by the session.
var suiteSub = map[string]bool{"test": true, "bench": true}

// suiteBin is the set of binaries whose job IS running tests, so the shape is already proof
// whatever subcommand follows. `coverage` belongs because `coverage run -m pytest` is a suite
// behind a measurement wrapper, and it arrives as the shape `coverage run`.
//
// `unittest` is NOT here, and its absence is a limit of the instrument rather than of this
// table: `python -m unittest` is not gated by the classifier at all, so it never reaches the
// trace and no analysis downstream can see it. Recorded in docs/advice.md among the known
// blind spots.
var suiteBin = map[string]bool{
	"pytest": true, "jest": true, "vitest": true,
	"tox": true, "nox": true, "coverage": true,
}

// pkgRun is a package manager whose `run` subcommand executes a named script. The script name
// is exactly what the shape refuses to carry — it is somebody's repo vocabulary — so
// `npm run test:unit` and `npm run build:prod` are one shape, `npm run`, and which of the two
// happened cannot be recovered.
var pkgRun = map[string]bool{"npm": true, "pnpm": true, "yarn": true}

// ShapeRunKind categorises one shape as internal/trace recorded it — "bin" or "bin sub".
func ShapeRunKind(shape string) RunKind {
	bin, sub := shape, ""
	if i := strings.IndexByte(shape, ' '); i >= 0 {
		bin, sub = shape[:i], shape[i+1:]
	}
	switch {
	case suiteBin[bin], suiteSub[sub]:
		return TestRun
	// A bare `npx` is the ambiguous case, and the reason is a classifier quirk worth naming:
	// npx is a `runner` but takes its command as a bare operand with no `run` keyword, which
	// is the `dispatcher` shape it is not registered as. So unwrapping never fires and BOTH
	// `npx jest` and `npx some-local-bin` record as `npx` — a test suite and a build are
	// indistinguishable here. 14 of this window's first 190 records are bare `npx`.
	case bin == "npx":
		return AmbiguousRun
	case sub == "run" && pkgRun[bin]:
		return AmbiguousRun
	default:
		return NotTestRun
	}
}
