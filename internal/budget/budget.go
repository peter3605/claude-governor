// Package budget models the concurrency budget as a set of sources reduced by min().
//
// The seam (design.md section 12.1): v1 has exactly one source, the local CPU/thermal
// governor. A tier-two broker adds a remote source governing tokens, spend and rate limit —
// resources that genuinely aggregate across machines, unlike CPU. The gate takes the
// minimum over whatever sources are configured, so adding one is not a code change on the
// hot path.
package budget

import "context"

// Quota is one source's answer: how many concurrent agents it will allow right now.
type Quota struct {
	// Name identifies the source in status output and in the "source" wire field.
	Name string
	// Budget is the number of concurrent agents this source permits.
	Budget int
	// InUse is how many of them this source believes are held.
	InUse int
}

// Source is one contributor to the budget.
//
// Quota MUST NOT block indefinitely: a source that cannot answer within the caller's
// deadline is skipped, never waited on, because a stalled budget lookup on the hot path is
// a stalled session.
type Source interface {
	Quota(ctx context.Context) (Quota, error)
}
