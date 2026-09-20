// Package identity carries who is asking for budget.
//
// The seam (design.md section 12.1): v1 is single-user and nothing acts on a principal, but
// every request carries one because retrofitting identity into an already-shipped wire
// format is far more expensive than carrying a field nobody reads. Tier two resolves it
// against an account — tokens, spend and rate limit are per-principal in a way CPU is not.
//
// It lives in its own package rather than beside the wire structs in cmd/governor so that
// internal/budget can name it without importing the command. GOV-26 is why: a budget source
// that cannot be told WHOSE budget it is being asked for cannot govern spend, and until a
// source existed that wanted to know, nothing forced the field past the gate.
package identity

// Principal identifies who is asking. This is the protocol shape of design.md section 7,
// and the json tags are the wire's, not a convenience — cmd/governor aliases this type
// rather than declaring its own, so there is exactly one definition to keep in step.
//
// Team is a pointer because the wire distinguishes "no team" (null) from a team named by
// the empty string. Mapping nil to "" would erase that distinction, and a personal account
// read as a team account is the kind of billing mistake tier two exists to avoid. It is
// read-only once decoded; nothing may mutate what it points at.
type Principal struct {
	User string  `json:"user"`
	Team *string `json:"team"`
}
