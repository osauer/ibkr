// Package corestore owns the daemon's authoritative SQLite state in daemon.db.
//
// The store exposes typed transactions for mutable state, append-only evidence,
// broker-scoped order safety, retained observations, and statement projections.
// Every durable mutation advances a monotonic authority head. Opening,
// inspection, backup, and upgrade paths validate schema, integrity, content
// hashes, and any caller-supplied rollback floor; they never repair or recreate
// an existing authority after a validation failure.
//
// Store serializes mutations internally. Callers never receive the underlying
// SQL handle and must use the combined operations when state and evidence need
// to become visible atomically.
package corestore
