// Package corestore owns the daemon's authoritative SQLite state.
//
// Unlike history.db, this database is not a rebuildable index. Open failures,
// migration drift, integrity failures, and newer schema versions are returned
// to the caller without deleting or recreating the database or its sidecars.
// Callers receive typed transactional operations, never the underlying SQL
// handle.
package corestore
