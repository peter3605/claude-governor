// Package budget models the concurrency budget as a set of sources reduced by min().
//
// The seam (design.md section 12.1): v1 has exactly one source, the local CPU/thermal
// governor. A tier-two broker adds a remote source governing tokens, spend and rate limit —
// resources that genuinely aggregate across machines, unlike CPU. The gate takes the
// minimum over whatever sources are configured, so adding one is not a code change on the
// hot path.
package budget

import (
	"context"

	"github.com/peter3605/claude-governor/internal/identity"
)

// Quota is one source's answer: how much concurrency it will allow right now, in slots.
type Quota struct {
	// Name identifies the source in status output and in the "source" wire field.
	Name string
	// Budget is how many slots of concurrency this source permits. One slot is one Medium
	// build; a subagent weighs about a twelfth of one (see weight.go).
	Budget int
	// InUse is how many of them this source believes are held.
	InUse int
}

// Source is one contributor to the budget.
//
// Quota MUST NOT block indefinitely: a source that cannot answer within the caller's
// deadline is skipped, never waited on, because a stalled budget lookup on the hot path is
// a stalled session. A caller that passes a context with no deadline is therefore asking
// for a stall, and Min refuses to do it — see there.
//
// WHO IS ASKING IS A PARAMETER BECAUSE TIER TWO CANNOT WORK WITHOUT IT (GOV-26). The local
// source ignores it: one machine's CPU is not divided by who wants it. A remote source
// governing tokens or spend is asking a question that has no answer without a principal,
// and before this the interface had no way to be told. The field had been carried on the
// wire since v1 and read by nothing, which is exactly the shape of seam the story existed
// to find: present, plausible, and not actually connected to anything.
type Source interface {
	Quota(ctx context.Context, who identity.Principal) (Quota, error)
}
