# History index (history.db)

**Status:** Implemented (Phase 2): all evidence journals + retained statements indexed; rotation/retention engine for the three decision journals; RPCs `regime.history` / `rules.history` / `canary.history` / `recon.equity`; indexed order reads with automatic journal-scan fallback.
**Created:** 2026-07-20 15:41 CEST · **Updated:** 2026-07-20 18:38 CEST
**Owner:** osauer
**Related:** [internal/daemon/history](../../internal/daemon/history), [internal/daemon/history_index.go](../../internal/daemon/history_index.go), [internal/daemon/order_index_read.go](../../internal/daemon/order_index_read.go), [internal/rpc/history_index.go](../../internal/rpc/history_index.go), [docs/design/regime-calibration.md](regime-calibration.md), [docs/design/post-trade-truth.md](post-trade-truth.md)

## Why this exists

The append-only evidence journals (regime/rules/canary decisions, capital
events, risk-policy governance, proposal outcomes, the order journal) are the
record of truth: tens of megabytes of JSONL that, before this index, could
only be queried with `jq` full-file scans — and the regime journal alone grew
55 MB in two months with no retention story. Operator questions ("when did
the stage last change?", "plot statement equity against capital flows"),
calibration questions ("canary time-in-action per week"), and the order
read model's O(file) hot paths all need indexed access — without ever
moving, rewriting, or reinterpreting the evidence itself.

`history.db` is a **derived, always-rebuildable SQLite index** over those
journals plus the retained Flex statements. The journals stay the record of
truth; the index is a read model. Phase 2 adds the rotation engine that
bounds raw-journal growth by compressing fully-indexed history into
immutable archives — rotation relocates evidence, it never deletes it.

## Ownership

- **The daemon is the sole writer and the sole runtime opener.** It opens
  `$XDG_STATE_HOME/ibkr/history.db` (default
  `~/.local/state/ibkr/history.db`) only after winning the single-instance
  flock — autospawn race losers construct a Server but never touch the DB.
- **The CLI and the app never open the DB.** `ibkr regime history`,
  `ibkr rules history`, `ibkr canary history`, and `ibkr recon equity` are
  RPC clients of the daemon; the paired app has no history surface.
- The only sanctioned second reader is a human (or offline analysis script)
  using `sqlite3` in read-only mode — see "Offline analysis" below. The
  daemon's connection sets `busy_timeout` so such readers never wedge ingest.
- Pure Go driver (`modernc.org/sqlite`): no CGO, no release-matrix impact.

## Derived and rebuildable — delete-safety

The DB is not evidence. Every evidence row mirrors a journal line and
carries the line verbatim in `raw_json`, so:

- **`rm ~/.local/state/ibkr/history.db*` is always safe.** The next daemon
  start re-ingests everything from byte 0 — rotated archives first (in file
  name order), then the live journals, then the retained statements.
  Backfill is simply "first ingest"; there is no backfill command.
- A corrupt or future-versioned DB file is deleted and recreated
  automatically (one retry; a second failure disables the index for the run,
  leaving the history RPCs returning a classified error, every order read on
  the journal-scan path, and journaling untouched).
- Older binaries: a phase-1 binary reopening a post-rotation state dir sees
  a genesis mismatch on the rotated journals and rebuilds its v1 index
  cleanly from the (shorter) live files.

Evidence-mirroring tables (`regime_decisions`, `regime_indicators`,
`rule_transitions`, `canary_transitions`, `capital_events`,
`risk_policy_events`, `proposal_outcomes`, `order_events`) carry `BEFORE
UPDATE` / `BEFORE DELETE` ABORT triggers: append-only at the SQL layer too.
Bookkeeping tables (`ingest_sources`, `rotation_log`, `archive_files`,
`statement_files`) and the derived `statement_equity_days` are updated in
place.

Schema is `PRAGMA user_version` 2. A v1 file migrates in place with a
single-transaction delta (`ALTER TABLE ingest_sources ADD COLUMN base`,
create the new tables) and zero row rewrites.

## Ingest and offset mechanics

One code path serves backfill, tail-ingest, and crash reconcile; they differ
only in the stored offset.

- **Offsets are logical-stream offsets.** `src_offset` is a line's position
  in the journal's complete append stream: (bytes rotated into archives) +
  (physical offset in the live file). `ingest_sources.base` records the
  rotated byte count; the stored `offset` is the logical high-water mark
  (never decreases) and `offset - base` is the physical resume point.
  Archives-in-name-order ++ live file reconstruct the original stream
  byte-for-byte, so rotation needs zero row updates and a full rebuild
  reproduces identical rows including ids.
- **Idempotency is the per-source offset**, advanced in the same SQLite
  transaction as the rows it covers (batches of 2000 lines). A crash replays
  from the last committed boundary; `UNIQUE(src_offset)` is a loud backstop
  (archive backfill uses `INSERT OR IGNORE` for idempotent resume; live
  ingest keeps the plain-INSERT backstop). No content-hash dedupe:
  byte-identical heartbeat lines are legitimate evidence.
- **Evidence-before-index by construction:** journal writers append to the
  JSONL file, then send a data-free kick; the ingester reads only the file.
  Every writer now kicks: regime/rules/canary decisions, capital events,
  risk-policy governance, proposal outcomes, order-journal appends
  (`onAppend` hook), and the Flex fetch success path.
- **Complete lines only.** A trailing line without `\n` is left for the next
  pass. A complete but unparseable line is logged, counted, and skipped —
  except in `order_events`, where it is **stored verbatim with
  `parse_ok = 0`** so the indexed order-read path refuses to serve exactly
  when the legacy scan would hard-fail (see "Order read model").
- **Truncation/replacement detection:** physical shrinkage
  (`size < offset - base`) or a changed first-line SHA-256 (`genesis`)
  triggers a loud per-source rebuild: drop the source's tables, clear its
  `archive_files` rows, re-stream archives, re-ingest the live file from 0.
- **Time is stored twice:** `at` verbatim (evidence), `at_unix_ms` canonical.
- Ingest failures are warned and swallowed. Nothing in journaling,
  snapshots, or any trading path waits on — or can be failed by — the index.

Every history result carries an `index` health block (`last_ingest_at`,
`ingested_bytes`, `journal_bytes`). `ingested_bytes` reports **physical**
live-file bytes (`offset - base`), so the catching-up comparison against the
on-disk size stays exact after rotation.

## Rotation and retention (phase 2)

Only three journals are rotatable — `regime-decisions.jsonl`,
`rules-decisions.jsonl`, `canary-decisions.jsonl` — nothing else, ever,
without a new spec. A daily maintenance pass (plus one pass at startup)
moves each journal's fully-ingested prefix older than the keep window into
`$XDG_STATE_HOME/ibkr/rotated/<journal>-YYYY-MM.jsonl.gz` (dir 0700, files
0600, one gzip member per file, exact original bytes). **Archives are
immutable evidence, kept forever: rotation compresses and relocates, it
never deletes.** The keep window is `history.rotation.keep_raw_months`
calendar months (current month counts as month 1; default 2, minimum 1);
`history.rotation.enabled=false` turns the pass off.

Mechanics and guarantees:

- **Only ingested bytes rotate.** The precondition (`offset - base == size`,
  after at most one synchronous inline catch-up) guarantees every archived
  byte is already mirrored in history.db and the cut lands on a complete
  line. The maintenance goroutine may block on that inline ingest; hot
  paths never do.
- **Cut rule.** The cut is the byte offset of the first line whose
  timestamp (`ts` for regime/canary, `at` for rules) falls inside the keep
  window; the prefix partitions into contiguous same-month runs, one
  archive per run. A line with an unparseable timestamp inherits the
  previous line's month; an unparseable first line aborts the source's
  rotation (safe direction). `cut == 0` is a quiet no-op.
- **Name order is stream order.** Every new archive's name must sort
  lexically after every existing archive of its source — that is what makes
  name-order concatenation reproduce the stream. A re-touched month takes
  `.part2` … `.part9` (a month beyond part9 is skipped with a warning); a
  stray out-of-order month that cannot satisfy the order rule truncates the
  rotation at its start with a warning and stays raw.
- **Writer quiescence.** All three writers are open-per-append and hold a
  writer lock across the whole append (regime/canary journal mutexes,
  `Server.rulesJournalMu` for rules); rotation holds the same lock across
  the swap, so a live-file rename is invisible to writers and needs no fd
  repair. The live file's mode is preserved (rules stays 0644; regime and
  canary stay 0600). Rotation also holds the store's ingest lock so the
  tail ingester can never misread the swap as a truncation.
- **Crash-recovery contract.** The sequence is: write archives as temps →
  fsync → intent row (`rotation_log`, `state='pending'`, with per-archive
  sizes and raw-content SHA-256) → rename temps to finals → rewrite the
  live tail atomically → finalize transaction (`base += cut`, genesis reset
  to the tail's first-line hash, `archive_files` rows, `state='done'`). On
  the next start, before writer traffic, every pending row is discriminated
  on the live file's first-line hash: pre-rotation hash ⇒ **roll back**
  (each archive is verified byte-equal to the untouched journal prefix and
  then deleted — the only sanctioned archive deletion anywhere; a
  verification mismatch quarantines the file instead); post-rotation hash
  (or a planned-empty tail whose pre-hash no longer matches) ⇒ **roll
  forward** (archives now hold the only raw copy; the finalize transaction
  runs). Orphan `.tmp-*` files are deleted (temps are copies, not
  evidence). The evidence multiset is invariant through every crash state.
- Each completed rotation logs one info line: source, months, bytes,
  archive names, new live size.

### Per-file retention policy

| File | Policy | Rationale / trigger |
|---|---|---|
| `regime-decisions.jsonl` | Rotate monthly → `rotated/regime-decisions-YYYY-MM.jsonl.gz`; keep `history.rotation.keep_raw_months` (default 2) months raw; archives kept forever | 55 MB in 2 months; only rotated over fully-ingested bytes |
| `rules-decisions.jsonl` | Same rotation | Same engine; small today, same policy for uniformity |
| `canary-decisions.jsonl` (new) | Same rotation | Born with a retention policy |
| `order-journal.jsonl` | **NEVER rotated or deleted** | Trading evidence; token-redemption and order-ID floors read it; phase 2 indexes it in place |
| `statements/flex-*.xml` | **NEVER rotated or deleted** | Broker-statement truth; `statement_equity_days` is derived from them |
| `capital-events.jsonl` | **NEVER rotated or deleted** | Capital ledger truth |
| `risk-policy-journal.jsonl` | **NEVER rotated or deleted** | Governance audit trail |
| `trade-proposals.jsonl` | **NEVER rotated or deleted** | Proposal evidence (19 MB; revisit only via a new spec) |
| `trade-proposal-outcomes.jsonl` | **NEVER rotated or deleted** | Outcome marks feed calibration |
| `gamma-skew-diagnostics.jsonl` | **NEVER rotated or deleted**; documented revisit trigger at 100 MB (1.1 MB today) | Diagnostics corpus |
| `purge-ledger.json` | Never touched by this feature | Restore authority for purge |
| `history.db` (+`-wal`/`-shm`) | Derived; delete-safe at any time | Rebuilds from journals + archives + statements |
| `rotated/*.jsonl.gz` | Immutable evidence archives, kept forever, 0600/0700 | Created only by rotation; only deletion ever permitted is recovery's verified-duplicate rollback |

## Statement equity derivation (recon.equity)

Retained Flex statements are a **file-set ingest** (not offset-based): each
`*.xml` in `statements/` missing from `statement_files` (or whose size
changed) parses through `internal/flexstmt` and upserts one row per
`(account_id, day)` into `statement_equity_days`. The restatement rule is
exactly the recon engine's: **the statement with the newest
`whenGenerated` wins a day** (`ON CONFLICT … WHERE excluded.when_generated >
…`). Statements are never modified or pruned; a parse failure warns and
retries next pass. The first pass backfills all retained statements, so the
runtime 45-day `DailyEquity` prune (untouched) stops being the only
intraday-equity memory. `recon.equity` / `ibkr recon equity` serves the
day series (default 90-day lookback, limit 200, max 1000) joined with the
declared capital-event ledger (newest 500, disclosed truncation) and two
health blocks: the capital journal and the statement file set (bytes =
summed retained XML sizes). `recon.backtest`, `recon.snapshot`, and every
other existing RPC keep reading raw statements/journals unchanged.

## Canary evidence journal (canary-decisions.jsonl)

The daemon owns `canary-decisions.jsonl` (0600, `canary.journal.enabled`
default true), mirroring the regime journal exactly: append-only, deduped
on the canary fingerprint with an hourly heartbeat, never read at runtime,
safe to delete. Two producers journal through one writer:

1. **Brief hook** — every brief render journals the exact canary the brief
   row displayed.
2. **Cadence loop** — every **5 minutes** while the gateway is connected,
   the daemon composes the canary exactly as the brief does (account
   snapshot without capital observation, positions, the cached regime
   snapshot, held-symbol market events) and journals it. Five minutes is a
   deliberate cadence: each tick includes a broker round-trip, so matching
   the app's 1-minute poll would add gateway load whenever the app is not
   attached. Consequence: a sub-5-minute canary flap can be alerted by the
   app but may not appear as a daemon journal line; dedupe plus the hourly
   heartbeat make the journal calibration-sufficient, not tick-complete.

`canary.history` / `ibkr canary history` serves the indexed timeline with
severity/action filters (7-day default lookback, limit 50, max 500).

## Order read model (phase 2, workstream D)

`order_events` mirrors `order-journal.jsonl` in place — the journal itself
is **never rotated, truncated, or rewritten**. Three read paths may serve
from the index: `orders.open`/`order.status` view folds, `orders.history`
range loads, preview-token redemption, plus the
`maxReservedBrokerOrderID` floor.

**The uniform safety rule: an indexed order read is served only when the
index is provably complete for the journal at that instant** — the on-disk
journal size equals the committed in-memory ingest watermark AND no
`parse_ok = 0` rows exist (checked again inside each serving transaction).
Anything else — staleness, parse markers, any query or decode error — falls
back automatically to the unchanged legacy journal scan with a rate-limited
(1/min/surface) log disclosure. There is deliberately **no settings escape
hatch**: the fallback is automatic, disclosed, exercised on every cold
start (the watermark is process-local and unseeded at open), and the
fallback path IS the unmodified legacy code. **The journal scan remains the
semantics-defining reference implementation**; a permanent dual-read parity
harness in `make test` deep-compares both paths' folds, and the SQL layer
only prunes (`ORDER BY id`, range widened 1 ms both ends) while the
existing Go predicates decide.

Token redemption keeps its exact locking and behavior: the check runs
inside the same `orderJournalStore.mu` critical section (appends serialize
on it, so the journal cannot grow mid-check), both paths share one
consumption predicate and one error format (byte-identical accept/reject
and messages), and the index query carries a 500 ms timeout so an
in-flight backfill can only cause a fallback, never a stall. Unparseable
journal lines reproduce the legacy hard-fail: the index refuses
(`parse_ok = 0` present) and the scan fails loudly on the same line.
Nothing in broker-write behavior, submit eligibility, freeze, journaling
semantics, or `trading.status`'s own journal summary changed.

## Purge invariant

Verified 2026-07-20: "purge" is position liquidation — it **appends**
order-journal evidence (with `purge_id`) and maintains `purge-ledger.json`;
no flow in the daemon or app deletes or redacts journal content, so
history.db needs no purge handling today. Binding invariant for the
future: **any flow that ever deletes or redacts journal content MUST, in
the same operation, delete `history.db` + `-wal` + `-shm` and every
`rotated/` archive of the affected journal; over-deletion is always safe.**
A regression test pins that a journaled purge flow only grows the order
journal and that its `purge_id` rows land in `order_events` untouched.

## Settings

Three runtime keys (registry-documented, `ibkr settings set`):
`history.rotation.enabled` (default true),
`history.rotation.keep_raw_months` (default 2, min 1),
`canary.journal.enabled` (default true). Remediation for a broken index
remains `rm history.db*` + restart, not a toggle.

## Offline analysis

Open read-only so the daemon's ingest is never blocked:

```sh
sqlite3 "file:$HOME/.local/state/ibkr/history.db?mode=ro"
```

Rotated archives are plain gzip — the offline path needs no tooling beyond
`zcat` / `gunzip -c`:

```sh
zcat ~/.local/state/ibkr/rotated/regime-decisions-2026-05.jsonl.gz | jq .stage | sort | uniq -c
# the complete original stream, byte-for-byte:
cat <(zcat ~/.local/state/ibkr/rotated/regime-decisions-*.jsonl.gz) \
    ~/.local/state/ibkr/regime-decisions.jsonl
```

Statement equity joined with declared capital flows (timeline):

```sql
SELECT d.day, d.equity_base,
       (SELECT COALESCE(SUM(CASE e.type WHEN 'withdrawal' THEN -e.amount_base ELSE e.amount_base END), 0)
        FROM capital_events e
        WHERE e.type IN ('deposit','withdrawal') AND substr(e.at, 1, 10) <= d.day) AS declared_flows_to_date
FROM statement_equity_days d
ORDER BY d.day DESC
LIMIT 30;
```

Canary time-in-action (sessions × actions, most recent two weeks):

```sql
SELECT session_key, action, COUNT(*) AS decisions
FROM canary_transitions
WHERE at_unix_ms >= (strftime('%s','now','-14 days') * 1000)
GROUP BY session_key, action
ORDER BY session_key DESC, decisions DESC;
```

Preview-token lifecycle lookup (audit; the daemon's own redemption check
uses the same rows only under its freshness proof):

```sql
SELECT at, type, order_ref, status, send_state
FROM order_events
WHERE preview_token_id = 'tok-…'
ORDER BY id;
```

Time in stage (regime, unchanged from phase 1):

```sql
SELECT session_key, stage, COUNT(*) AS decisions
FROM regime_decisions
WHERE at_unix_ms >= (strftime('%s','now','-14 days') * 1000)
GROUP BY session_key, stage
ORDER BY session_key DESC, decisions DESC;
```

Anything the schema does not project is still queryable from the verbatim
line via `json_extract` — future calibration questions are queries, not
schema migrations:

```sql
SELECT at,
       json_extract(raw_json, '$.market.eligible_red_clusters') AS eligible_reds,
       json_extract(raw_json, '$.policy.fingerprint.key') AS policy_key
FROM canary_transitions
ORDER BY at_unix_ms DESC
LIMIT 20;
```
