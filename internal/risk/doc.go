// Package risk provides pure risk-policy evaluation and related domain
// contracts. It performs no I/O and owns no broker or runtime state; callers
// supply typed observations and policies, and retain responsibility for state
// mutation, persistence, transport, and enforcement.
//
// Evaluators preserve missing, stale, and unapproved inputs as explicit
// non-passing outcomes. Fingerprint helpers identify the complete normalized
// policy projections used by callers.
package risk
