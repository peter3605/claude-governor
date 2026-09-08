package budget

import (
	"context"
	"errors"
	"testing"
)

// stubSource is a second implementation for tests only. It is not a second implementation
// of the seam in the sense design.md section 9 forbids: nothing ships it, and its whole
// purpose is to prove that the reducer behaves when there is more than one member — which
// is what tier two will add and what the plural exists for.
type stubSource struct {
	q   Quota
	err error
}

func (s stubSource) Quota(context.Context) (Quota, error) { return s.q, s.err }

func TestLocalSourceReportsItsLimit(t *testing.T) {
	held := 2
	q, err := LocalSource{N: 6, InUse: func() int { return held }}.Quota(context.Background())
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
		q, err := LocalSource{N: n}.Quota(context.Background())
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
		stubSource{q: Quota{Name: "remote", Budget: 2}},
		stubSource{q: Quota{Name: "other", Budget: 9}},
	})
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
		stubSource{err: errors.New("unreachable")},
	})
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
	if got, _ := Min(context.Background(), nil); got != Floor {
		t.Fatalf("min over no sources is %d, want the floor %d", got, Floor)
	}
	got, _ := Min(context.Background(), []Source{stubSource{err: errors.New("down")}})
	if got != Floor {
		t.Fatalf("min over one failed source is %d, want the floor %d", got, Floor)
	}
}

// The v1 configuration: exactly one source, still reduced by min(). The plural is the whole
// of the tier-two seam, so the one-member case has to be the same code path as the many.
func TestMinOverTheSingleV1Source(t *testing.T) {
	got, quotas := Min(context.Background(), []Source{LocalSource{N: 4}})
	if got != 4 {
		t.Fatalf("min over the one v1 source is %d, want 4", got)
	}
	if len(quotas) != 1 || quotas[0].Name != "local" {
		t.Fatalf("quotas %+v, want exactly one local source", quotas)
	}
}
