// Package live maintains the app host's refreshable daemon snapshot and
// publishes change events to local subscribers. It owns polling cadence,
// source freshness, cloning, and SSE fanout; the cached snapshot is an adapter
// view, not daemon runtime or policy authority.
package live
