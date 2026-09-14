package budget

import "context"

// Floor is the smallest budget any source may report.
//
// Invariant 4 (design.md section 3): the budget never reaches zero. A machine under
// thermal duress still makes progress, one agent at a time. A source that computes zero
// has computed a wedged machine, not a budget, so the floor is applied here rather than
// left to each caller to remember.
const Floor = 1

// LocalSource is the v1 budget: this host's own concurrency limit, in build slots.
//
// IT USED TO BE A CONSTANT AND IS NOT ANY MORE (GOV-15). GOV-06 fixed it deliberately, so
// that a moving budget was not being debugged against a moving lease table, and GOV-12's
// sampler was wired to write its readings and act on none of them. This is where that
// changes: Live is the AIMD controller's current answer, and when it is set N is ignored.
//
// What is deliberately unchanged is the *shape*: the daemon holds a slice of Source and
// reduces it with Min even though the slice has one member, because that plural is the entire
// tier-two seam (design.md section 12.1). Adding a remote source later is a line at
// construction, not a change on the hot path — and note that the loop below did NOT become a
// second Source implementation. The budget moving is a property of this one source's answer,
// not a new kind of source.
type LocalSource struct {
	// N is the fixed budget in slots, used when Live is nil. It remains the answer for a
	// daemon with no sampler — a host whose load cannot be read is a host whose budget must
	// not be inferred from silence.
	N int
	// Live is the controller's current budget in slots. Nil means the budget is fixed at N.
	//
	// A func rather than a value because Quota is called on the hot path and the controller
	// is stepped by the sampling goroutine: the indirection is what lets the daemon hand out
	// this source once at construction and still have it answer with today's number.
	Live func() int
	// InUse is read when the quota is taken, so status can report what this source
	// believes is held. Nil means the source does not track holders.
	InUse func() int
}

// Quota reports this host's limit. It never blocks and never fails: everything it needs is
// already in memory, which is what makes it safe on the path a session is waiting on.
func (s LocalSource) Quota(_ context.Context) (Quota, error) {
	n := s.N
	if s.Live != nil {
		n = s.Live()
	}
	if n < Floor {
		n = Floor
	}
	inUse := 0
	if s.InUse != nil {
		inUse = s.InUse()
	}
	return Quota{Name: "local", Budget: n, InUse: inUse}, nil
}

// Min reduces a set of sources to the budget the tightest of them allows, and returns each
// source's answer for status reporting.
//
// A source that errors or exceeds ctx is *skipped*, not waited on and not treated as zero.
// Both alternatives are worse than they look: waiting stalls the session that is blocked on
// this call, and treating an unreachable source as zero would let a network partition halt
// every agent on every machine — precisely the failure mode invariant 1 exists to prevent.
// Skipping degrades to the budget the reachable sources allow, which is the same direction
// the gate fails in.
//
// With no usable source at all the answer is Floor rather than an error: one agent at a
// time is the correct amount of progress for a governor that has lost its inputs.
func Min(ctx context.Context, sources []Source) (int, []Quota) {
	quotas := make([]Quota, 0, len(sources))
	best := 0
	for _, s := range sources {
		q, err := s.Quota(ctx)
		if err != nil {
			continue
		}
		quotas = append(quotas, q)
		if best == 0 || q.Budget < best {
			best = q.Budget
		}
	}
	if best < Floor {
		best = Floor
	}
	return best, quotas
}
