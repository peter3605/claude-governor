package budget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/identity"
)

// stubSource is a second implementation for tests only. It is not a second implementation
// of the seam in the sense design.md section 9 forbids: nothing ships it, and its whole
// purpose is to prove that the reducer behaves when there is more than one member — which
// is what tier two will add and what the plural exists for.
type stubSource struct {
	q   Quota
	err error
	// block holds Quota until it is closed or the context expires, so a test can drive the
	// one failure shape a dead remote has that a refused connection does not: accepted, and
	// then silent.
	block <-chan struct{}

	mu   sync.Mutex
	sawN int
	saw  identity.Principal
}

func (s *stubSource) Quota(ctx context.Context, who identity.Principal) (Quota, error) {
	s.mu.Lock()
	s.saw, s.sawN = who, s.sawN+1
	s.mu.Unlock()
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return Quota{}, ctx.Err()
		}
	}
	return s.q, s.err
}

func (s *stubSource) seen() (identity.Principal, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saw, s.sawN
}

func team(name string) *string { return &name }

func TestLocalSourceReportsItsLimit(t *testing.T) {
	held := 2
	q, err := LocalSource{N: 6, InUse: func() int { return held }}.Quota(context.Background(), identity.Principal{})
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if q.Name != "local" || q.Budget != 6 || q.InUse != 2 {
		t.Fatalf("quota %+v, want local/6/2", q)
	}
}

// Invariant 4: the budget never reaches zero. A machine under thermal duress still makes
// progress, one agent at a time.
func TestLocalSourceNeverReportsBelowTheFloor(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		q, err := LocalSource{N: n}.Quota(context.Background(), identity.Principal{})
		if err != nil {
			t.Fatalf("Quota(%d): %v", n, err)
		}
		if q.Budget != Floor {
			t.Fatalf("a source configured with %d reported budget %d, want the floor %d", n, q.Budget, Floor)
		}
	}
}

func TestMinTakesTheTightestSource(t *testing.T) {
	got, quotas := Min(context.Background(), []Source{
		LocalSource{N: 6},
		&stubSource{q: Quota{Name: "remote", Budget: 2}},
		&stubSource{q: Quota{Name: "other", Budget: 9}},
	}, identity.Principal{})
	if got != 2 {
		t.Fatalf("min over 6, 2 and 9 is %d, want 2", got)
	}
	if len(quotas) != 3 {
		t.Fatalf("reported %d quotas, want all 3 for status", len(quotas))
	}
}

// A source that cannot answer is skipped, never treated as zero. Treating an unreachable
// source as zero would let a partition halt every agent on every machine, which is the
// failure mode invariant 1 exists to prevent.
func TestMinSkipsASourceThatCannotAnswer(t *testing.T) {
	got, quotas := Min(context.Background(), []Source{
		LocalSource{N: 5},
		&stubSource{err: errors.New("unreachable")},
	}, identity.Principal{})
	if got != 5 {
		t.Fatalf("min is %d, want 5 — an unreachable source must not pull the budget down", got)
	}
	if len(quotas) != 1 || quotas[0].Name != "local" {
		t.Fatalf("quotas %+v, want only the source that answered", quotas)
	}
}

// With nothing usable at all the answer is the floor: one agent at a time is the correct
// amount of progress for a governor that has lost its inputs.
func TestMinWithNoUsableSourceFallsToTheFloor(t *testing.T) {
	if got, _ := Min(context.Background(), nil, identity.Principal{}); got != Floor {
		t.Fatalf("min over no sources is %d, want the floor %d", got, Floor)
	}
	got, _ := Min(context.Background(), []Source{&stubSource{err: errors.New("down")}}, identity.Principal{})
	if got != Floor {
		t.Fatalf("min over one failed source is %d, want the floor %d", got, Floor)
	}
}

// The v1 configuration: exactly one source, still reduced by min(). The plural is the whole
// of the tier-two seam, so the one-member case has to be the same code path as the many.
func TestMinOverTheSingleV1Source(t *testing.T) {
	got, quotas := Min(context.Background(), []Source{LocalSource{N: 4}}, identity.Principal{})
	if got != 4 {
		t.Fatalf("min over the one v1 source is %d, want 4", got)
	}
	if len(quotas) != 1 || quotas[0].Name != "local" {
		t.Fatalf("quotas %+v, want exactly one local source", quotas)
	}
}

// The principal reaches every source, intact.
//
// It had been carried on the wire since v1 and read by nothing, so "identity is a seam" was
// a claim about a struct field rather than about a path (GOV-26). A source that governs an
// account's tokens or spend cannot answer without it, and before the parameter existed there
// was nowhere for it to arrive.
func TestMinHandsThePrincipalToEverySource(t *testing.T) {
	a := &stubSource{q: Quota{Name: "a", Budget: 3}}
	b := &stubSource{q: Quota{Name: "b", Budget: 9}}
	who := identity.Principal{User: "peter", Team: team("platform")}

	if got, _ := Min(context.Background(), []Source{LocalSource{N: 6}, a, b}, who); got != 3 {
		t.Fatalf("min is %d, want 3", got)
	}

	for name, s := range map[string]*stubSource{"a": a, "b": b} {
		saw, n := s.seen()
		if n != 1 {
			t.Fatalf("source %s asked %d times, want exactly 1", name, n)
		}
		if saw.User != who.User {
			t.Errorf("source %s saw user %q, want %q", name, saw.User, who.User)
		}
		if saw.Team == nil {
			t.Fatalf("source %s saw a nil team, want %q — a team account read as a personal one is a billing mistake, not a rounding one", name, *who.Team)
		}
		if *saw.Team != *who.Team {
			t.Errorf("source %s saw team %q, want %q", name, *saw.Team, *who.Team)
		}
	}
}

// A nil team survives as nil rather than arriving as the empty string. The wire distinguishes
// "no team" from "a team named by the empty string" and so must everything below it.
func TestMinPreservesTheAbsenceOfATeam(t *testing.T) {
	s := &stubSource{q: Quota{Name: "s", Budget: 2}}
	Min(context.Background(), []Source{s}, identity.Principal{User: "peter"})
	if saw, _ := s.seen(); saw.Team != nil {
		t.Fatalf("a principal with no team arrived with team %q, want nil", *saw.Team)
	}
}

// THE FAILURE SHAPE A REFUSED CONNECTION DOES NOT COVER: a source that accepts and then goes
// quiet. Min's contract says such a source "is skipped, never waited on" — but every caller
// in the daemon passed context.Background(), which has no deadline for that promise to rest
// on, so the honest answer before GOV-26 was that it would have waited forever.
//
// The assertion is therefore two-sided: the budget degrades to what the reachable sources
// allow, AND the call returns in bounded time rather than hanging.
func TestMinDoesNotWaitForeverOnASourceThatNeverAnswers(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	dead := &stubSource{q: Quota{Name: "remote", Budget: 1}, block: stuck}

	start := time.Now()
	got, quotas := Min(context.Background(), []Source{LocalSource{N: 5}, dead}, identity.Principal{})
	elapsed := time.Since(start)

	if got != 5 {
		t.Errorf("budget is %d, want the local 5 — a silent remote must degrade, not decide", got)
	}
	if len(quotas) != 1 || quotas[0].Name != "local" {
		t.Errorf("quotas %+v, want only the source that answered", quotas)
	}
	// Generous against a loaded CI box; the defect it guards is unbounded, not slow.
	if limit := 10 * MaxWait; elapsed > limit {
		t.Fatalf("Min took %v against a silent source, want under %v — the hot path must not stall", elapsed, limit)
	}
}

// A caller that has already promised an answer sooner keeps its own, tighter deadline: the
// cap is a ceiling, not a replacement.
func TestMinKeepsACallersTighterDeadline(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	dead := &stubSource{q: Quota{Name: "remote", Budget: 1}, block: stuck}

	ctx, cancel := context.WithTimeout(context.Background(), MaxWait/10)
	defer cancel()

	start := time.Now()
	if got, _ := Min(ctx, []Source{LocalSource{N: 5}, dead}, identity.Principal{}); got != 5 {
		t.Errorf("budget is %d, want the local 5", got)
	}
	if elapsed := time.Since(start); elapsed >= MaxWait {
		t.Fatalf("Min took %v, want well under MaxWait %v — the caller's shorter deadline must win", elapsed, MaxWait)
	}
}
