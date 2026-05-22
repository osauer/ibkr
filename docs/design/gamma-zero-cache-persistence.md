# Gamma zero-cache persistence

**Status:** Implemented with scope-keying (one slot + one file per scope). Tests green; smoke green; live exercise pending the dashboard's combined fan-out completing.
**Created:** 2026-05-22 07:30 CEST
**Last update:** 2026-05-22 08:10 CEST
**Owner:** osauer
**Related:** [internal/breadth/spx/store.go](../../internal/breadth/spx/store.go), [docs/specs/regime-v2-design.md](../specs/regime-v2-design.md), [internal/daemon/gamma_zero_cache.go](../../internal/daemon/gamma_zero_cache.go), [internal/daemon/gamma_zero_store.go](../../internal/daemon/gamma_zero_store.go)

## Why now

`gammaZeroCache` was deliberately in-memory per [regime-v2-design.md:107](../specs/regime-v2-design.md#L107): *"Per-day persistence would add complexity for negligible gain — the compute is cheap enough at 2-4 min that re-running on cold-start is fine."*

The 2-4 min assumption no longer holds. Today's measurements after the slot-anchored picker landed (commit `0145bed`):

- Cold off-hours fan-out: 5m24s (1033/1076 legs).
- Warm follow-up: 1m38s (1040/1076).

5+ min crosses the "operator attention" threshold. A `pkill && make install && relaunch` cycle during NY-session hours now costs the dashboard a full ~5 min "computing" state. The TTL is one NY trading session anyway; persistence is the obvious move.

## Unify with breadth's store, or distinct?

**Decision: distinct.** Three reasons.

1. [regime-v2-design.md:96-100](../specs/regime-v2-design.md#L96) explicitly rejects unification: *"different cadence and invalidation rules; cataloguing them under one persistence layer would over-engineer the shared piece. New persistence … own store, own version field, own load/save lifecycle."* The convention was set deliberately; gamma should follow it.

2. Breadth's `Store` has three file methods (`SaveSnapshot` / `SaveWindows` / `SaveHistory`), each with a domain-specific shape. Gamma has one cache file. Genericising breadth's `Store` to also serve gamma would add a layer (`KV.Set/Get`) that adds more complexity than it removes.

3. The only meaningfully shareable code is `writeAtomic` (~30 LoC). Two copies is below the threshold where extraction earns its keep.

The conventions worth mirroring exactly:

- **Atomic temp + rename** for every write (mkdir-on-first-write).
- **Pretty-printed JSON** (indent=2) so debugging with `cat` works.
- **Version field** on the persisted struct; mismatch = treat as no-cache, do NOT migrate or error.
- **Method-token gate** alongside version: a persisted struct whose `Method` differs from the current `Method` constant is also no-cache. Catches silent methodology bumps the version field might miss.
- **Cold-start = no error.** Returning `(nil, nil)` on missing file or any version/method mismatch lets the engine cold-rebuild without a startup error.
- **`DefaultDir()` resolves XDG_CACHE_HOME** with fallback to `$HOME/.cache/ibkr/...`.
- **Persistence errors are warnings, not failures.** A compute that succeeds but fails to persist still serves callers from the in-memory cache.

## Shape — as shipped

`internal/daemon/gamma_zero_store.go` — ~180 LoC after scope-keying.

```go
type gammaZeroStore struct{ dir string }

func newGammaZeroStore(dir string) *gammaZeroStore { … }

// Load returns the persisted entry for scope or (nil, nil) on cold /
// mismatch. Independent gates: version, session_key, envelope's own
// scope, and result.method.
func (s *gammaZeroStore) Load(scope string, nyNow time.Time) (*rpc.GammaZeroComputed, error)

// Save writes the result atomically to the scope's canonical file.
func (s *gammaZeroStore) Save(scope, sessionKey string, r *rpc.GammaZeroComputed) error

func gammaZeroStoreDefaultDir() (string, error) // $XDG_CACHE_HOME/ibkr/gamma-zero
```

**Per-scope filename:** `gamma-zero-{scope}.json` — one of `gamma-zero-spy+spx.json` (combined / dashboard / regime), `gamma-zero-spy.json` (--only=spy), `gamma-zero-spx.json` (--only=spx).

Persistence envelope:

```json
{
  "version": 1,
  "session_key": "2026-05-22",
  "scope": "spy+spx",
  "method": "perfiliev-bs-sweep-v2-stickymoneyness",
  "result": { … rpc.GammaZeroComputed … }
}
```

Four independent reasons to treat as cold: `version != 1`, `session_key != today's NY date`, `scope != requested scope` (defense against renamed/linked files), `method != current method constant`. Each is a write-through gate — the store never tries to coerce or migrate.

## Scope-keying — why it landed in the same PR

The original design described one cache slot keyed by NY session date. While implementing, I noticed the existing in-memory cache had the same property — a `--only=spy` call without `--force` would overwrite the combined cache with SPY-only data. The handler's comment at `gamma_handler.go:50-51` described `--only` paths as "bypass via force()" but the code didn't enforce that.

Persistence makes this wart materially worse: today a daemon restart wipes a wrong-scope cache; with persistence on, the wrong-scope value would survive until the soft-TTL refresh (5 min) replaced it. The user's call was to fix scope-mixing in the same PR rather than carry the tech debt forward.

**Shape:** the cache became a `map[scope]*gammaSlot`. Each slot owns its own `current` / `refresh` / `lastErr*` lifecycle. `kickOrJoin` / `force` / `snapshot` take scope as their first parameter (after the parent ctx); the dispatch is a per-slot lookup. `IsComputing` iterates all slots. Persistence is keyed the same way: one file per scope, independent atomic writes.

**Known scopes** are enumerated as `knownGammaScopes` (constants from `rpc.GammaZeroScope*`). The cache treats scope as opaque at the data layer — adding a new scope means appending one entry. Load on boot iterates this list and seeds whichever slots have a valid persisted file.

## Wiring

- `gammaZeroCache` gains an optional `store *gammaZeroStore` field. Nil store = pure in-memory (used by tests).
- `Server.installGammaZeroCache()` (new helper, mirrors `installBreadthEngine`) resolves `DefaultDir()` and constructs the cache with the store; falls back to in-memory + warning on dir-resolve failure.
- **Load path:** at cache construction time, attempt `store.Load(time.Now())`. If a result lands, install as `current` with a synthetic `gammaComputation` whose `done` channel is already closed and `result` populated. `startedAt` is the persisted `AsOf` (or zero — let the existing soft-TTL refresh trigger if stale).
- **Save path:** wherever a fresh compute lands cleanly (`spawnJob`'s goroutine where `job.result = res` is assigned). Persistence happens once per successful compute, off the cache mutex. Errors logged as warnings.
- **Refresh promotion:** when a soft-TTL refresh promotes to current in `kickOrJoin`, that's already a successful compute — the save in `spawnJob` already fired. No additional save needed.
- **Failed computes never persist.** Mirrors breadth's `MinCoverageFraction` gate: only successful results land on disk.

## What's intentionally NOT covered

- **No snapshot log.** [regime-v2-design.md:99](../specs/regime-v2-design.md#L99) anticipates a future "gamma snapshot log" for historical analysis. That's a separate domain (append-only series) and belongs in its own store when someone needs it.
- **No background eviction.** Stale files from prior sessions linger until the daemon next writes (`os.Rename` replaces). Negligible disk footprint (~10-50KB per file, one file).
- **No multi-underlying.** The combined SPY+SPX result envelope already lives inside one `rpc.GammaZeroComputed` from the daemon's perspective; the store doesn't need to know about the underlying split.

## Test plan — what landed

- `TestGammaZeroStore_RoundTrip` — Save then Load returns the same `GammaZeroComputed` values.
- `TestGammaZeroStore_ScopeIsolation` — three scopes save to three distinct files; each Load returns its own data, no bleeding.
- `TestGammaZeroStore_ColdMissingFile` — empty dir → Load returns nil-nil.
- `TestGammaZeroStore_VersionMismatch` — `version=99` → Load returns nil-nil.
- `TestGammaZeroStore_SessionKeyMismatch` — yesterday's session → Load returns nil-nil today.
- `TestGammaZeroStore_ScopeMismatch` — envelope claims a different scope than the file path → Load returns nil-nil.
- `TestGammaZeroStore_MethodMismatch` — stale `method` token → Load returns nil-nil.
- `TestGammaZeroStore_AtomicReplace` — Save replaces existing file; no temp leftover.
- `TestGammaZeroStore_DefaultDirHonoursXDG` — env-var resolution path.
- `TestNewGammaZeroCacheWithStore_LoadsPersistedScopes` — constructor seeds each persisted scope's slot independently.
- `TestNewGammaZeroCacheWithStore_IgnoresYesterdaysSession` — session rollover invalidates persisted scopes.

## Stop-loss

If implementing this hits >150 LoC across store + wiring (excluding tests), pause. The "small piece" intent is the load-bearing constraint — the moment it grows past that, the cost/benefit shifts back toward leaving the cache in-memory and just accepting the 5-min restart cost.