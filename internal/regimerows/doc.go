// Package regimerows builds presentation-oriented regime rows, bands, and
// provenance labels from typed RPC results.
//
// The package performs no I/O and owns no daemon state or broker authority. Its
// shared classifications keep Canary and CLI presentation consistent; callers
// must continue to use daemon- and risk-owned verdicts for policy decisions.
package regimerows
