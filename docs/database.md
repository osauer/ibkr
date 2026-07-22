# Storage: State, Evidence, and Recovery

`ibkr` needs to remember more than preferences. After a restart, the daemon
must still know which state was current, which evidence supported a decision,
which preview tokens have already been consumed, how far broker order IDs have
advanced, and which broker statements were incorporated.

The daemon solves that problem with one local storage layer, implemented as a
SQLite file named `daemon.db`. SQLite is the tool. The design is the ownership
rule around it:

> One daemon writes durable runtime state. Other product surfaces ask the
> daemon through typed RPC. Human-authored policy and original broker evidence
> remain outside the database because they are different kinds of truth.

This page explains why that design fits the current single-trader desk, how the
data model follows from the questions the product must answer, how it fails and
recovers, and where it must improve before it can support a family-office data
platform.

## What the Storage Layer Protects

The storage layer gives the running system continuity across process exits,
machine restarts, and software upgrades. Its durable responsibilities are:

- **Current daemon state:** settings, risk-capital state, readiness, purge
  state, current proposals and opportunities, alert episodes, and last-good
  market or model publications.
- **Local decision history:** events that explain what the software observed,
  evaluated, or attempted. These records explain local intent and lifecycle;
  they do not prove what executed at the broker.
- **Measured evidence:** retained market, gamma, membership, contract, and
  other source observations with timestamps, provenance, and an explicit flag
  saying whether they were allowed to support a live decision.
- **Order safety continuity:** exact broker-route bindings, consumed preview
  tokens, conservative order-ID floors, and pre-transmit order events.
- **Statement-derived views:** an inventory and daily equity view rebuilt from
  the complete retained set of original Flex XML statements.

The database is deliberately not a container for every file. Configuration,
policy, original broker statements, secrets, app credentials, and recovery
artifacts have different owners and remain separate.

## Why SQLite

The decisive fact is not that SQLite is convenient. It is that this deployment
has one application writer on one machine.

| Requirement | Why SQLite fits | Accepted trade-off |
|---|---|---|
| One daemon owns writes | A local SQLite file needs no database service, credentials, or administrator. The project uses a pure-Go SQLite driver, so the storage engine ships inside the `ibkr` binary. | It is not a shared network database and must not be opened by several writers. |
| Related facts must change together | Transactions can commit current state, evidence, token tombstones, order floors, and lifecycle events as one unit. | Writers must use the daemon's typed transaction APIs rather than editing files or rows directly. |
| Reads need more structure than JSON files | Tables and indexes can support bounded current-state, event, order, observation, and statement queries. | The current implementation has not yet moved every history path onto those indexes. |
| Installation should stay local and quiet | There is no server process, port, backup agent, or database account to operate. | High availability, multi-host writes, and centralized concurrent analytics are outside the present topology. |
| Schema changes must be recoverable | The daemon can inspect a database, upgrade a copy, verify it, and publish the copy only after it passes checks. | The upgrade machinery is substantial, while routine backup and restore operations are not yet complete. |

SQLite is therefore a good fit for the current desk, not a universal database
choice. If the product needs several writer hosts, automatic failover, or many
concurrent analytical users, the topology must change. Pointing those users at
the live file would not turn SQLite into that architecture.

## Where Each Kind of Truth Lives

“Source of truth” means the place the software is allowed to treat as current
for one particular kind of fact. There is intentionally more than one because
human intent, broker evidence, and daemon state are not interchangeable.

| Kind of fact | Owning location | Why it stays there |
|---|---|---|
| Human intent and approved limits | `config.toml` and `policies/*.toml` | People author these declarations. SQLite may retain applied identities and resulting events, but it is not the editing surface. |
| Original broker statement evidence | `$XDG_STATE_HOME/ibkr/statements/flex-*.xml` | The XML is what the broker supplied. SQLite stores inventory, versions, and derived daily views; those projections do not replace the original evidence claim. |
| Daemon working state and local evidence | `$XDG_STATE_HOME/ibkr/daemon.db` | One daemon can update related facts transactionally and serve one typed interpretation to the rest of the product. |
| Preview signer and Flex credentials | Private config/state files | Secrets and signing material have a different lifecycle from ordinary database rows. |
| Device grants, push subscriptions, and relay credentials | App-owned state directory | The app is a separate process with separate authentication responsibilities and never opens `daemon.db`. |
| Recovery state | `daemon.db.head`, verified backups, and sealed legacy artifacts | These help detect rollback or support offline recovery. They are not live read replicas or fallback business-state stores. |

Changing only `IBKR_SOCKET` does not isolate storage. A separately isolated
daemon needs its own config, socket, broker/account/client pins, and XDG state
roots. A lock beside `daemon.db` prevents alternate socket paths from becoming
concurrent writers of the same file.

## Storage Layer in One View

[![What the daemon storage layer owns and what remains outside it](diagrams/storage-overview.svg)](diagrams/storage-overview.svg)

[PNG fallback](diagrams/storage-overview.png) ·
[SVG source generator](diagrams/render-architecture.mjs) ·
[Tabler Icons license](diagrams/ICON-LICENSE.txt)

The left side of the diagram contains truths owned outside SQLite: human
declarations, original broker evidence, and secrets. The daemon is the only
component that turns those inputs and live observations into durable SQLite
state. Product surfaces on the right receive typed results; they do not become
database readers or second writers.

## How the Data Model Follows from the Questions

The model is easier to understand as five questions than as a list of tables.

| Product question | Storage structure | What one row means |
|---|---|---|
| What is true now? | `state_documents`, `statement_files`, `statement_equity_days` | The current revision or selected current statement-derived record for a defined scope and kind. |
| What did the software observe or decide? | `event_log` | One immutable local lifecycle event with its original JSON payload and payload digest. |
| What was measured? | `observations` | One retained source measurement, including when it was observed and whether it could support a live decision. |
| What must never be reused or move backwards? | `broker_scopes`, `consumed_preview_tokens`, `order_id_floors`, `order_events` | Route identity, a spent capability, a conservative ID lower bound, or an order-lifecycle fact. |
| What did the broker originally report? | Retained Flex XML plus the four statement tables | XML remains original evidence; SQLite records exact file versions and current or historical derived equity rows. |

This is selective normalization. Safety-critical identities and irreversible
facts get dedicated columns, keys, foreign keys, and triggers. Heterogeneous
current state stays in versioned JSON documents so every cache or snapshot does
not require a new table. Immutable events retain their original payload while
some frequently queried fields are copied into relational projection tables.

That last part is incomplete today: projection rows are written, but the main
Regime, rules, Canary, and capital history readers still scan matching
`event_log` JSON and filter it in Go. The extra projection tables therefore do
not yet earn all of their complexity. They should either become the bounded,
indexed read path or be removed when compatibility permits.

## Physical Data Model

[![Physical entity relationships in daemon.db schema version 1](diagrams/sqlite-data-model.svg)](diagrams/sqlite-data-model.svg)

[PNG fallback](diagrams/sqlite-data-model.png) ·
[SVG source generator](diagrams/render-architecture.mjs) ·
[Canonical DDL](https://github.com/osauer/ibkr/blob/main/internal/daemon/corestore/schema.go)

This is a physical ER view of schema version 1. A solid relationship in the
diagram corresponds to a declared SQLite foreign key. A dashed relationship is
explicitly labelled as a convention enforced by Go code rather than by SQL.
Shared names such as `scope_key` are namespaces, not automatic foreign keys.

The declared foreign-key relationships are:

| Parent | Child | Cardinality |
|---|---|---|
| `event_log` | Each individual event projection: `regime_decisions`, `rule_transitions`, `canary_transitions`, `capital_events`, `risk_policy_events`, `proposal_outcomes`, and `order_events` | One event to zero or one row in each individual child table. |
| `regime_decisions` | `regime_indicators` | One decision to zero or more indicator rows. |
| `broker_scopes` | `consumed_preview_tokens` | One exact broker route to zero or more consumed tokens. |
| `broker_scopes` | `order_events` | One exact broker route to zero or more order events. |
| `statement_files` | `statement_equity_days` | One current statement file identity to zero or more current daily equity rows. |
| `statement_file_versions` | `statement_equity_day_versions` | One immutable statement-file version to zero or more immutable daily equity versions. |

`store_meta`, `schema_migrations`, `legacy_imports`, `state_documents`,
`observations`, and `order_id_floors` have no declared foreign key to another
table. Several useful relationships are application conventions instead:

- a state revision may commit in the same transaction as an event or
  observation, but it stores no lineage key to that row;
- the writer intends at most one event-projection family per event, but SQL
  permits the same `event_seq` to appear in several projection tables;
- current statement rows and immutable statement versions are written together,
  but the current file table does not reference the version table;
- broker-scoped order-ID floors are associated with a broker scope by validated
  values, not by a foreign key.

The exact columns, constraints, indexes, and triggers live in
`internal/daemon/corestore/schema.go`. The diagram renderer checks its table and
foreign-key inventory against that source so schema drift fails the diagram
gate instead of silently changing the picture's meaning.

JSON inside `state_documents`, events, and observations is not one universal
schema. Every reader must select the expected kind, validate its payload
version, and preserve scope, time basis, provenance, eligibility, and data
quality.

## What a Write Looks Like

### Publishing a market observation

1. The daemon validates the scope, payload, source metadata, and expected
   current revision.
2. One SQLite transaction updates the current `state_documents` row and appends
   the immutable `observations` row.
3. The transaction advances an ever-increasing committed-write counter and
   commits under SQLite WAL with foreign keys and full synchronization enabled.
4. The daemon updates and fsyncs `daemon.db.head`, an external record of the
   newest accepted counter, before publishing the new in-memory/RPC view.

If step 4 fails, the SQLite transaction is already committed. The store returns
an error, marks itself unhealthy, and does not claim success through the
in-memory or RPC surface. The state and observation were committed together,
but the schema does not retain a direct observation ID on the state row.

### Staging a broker transmission

Before the daemon may send an order, one transaction binds the exact broker
route, records the consumed preview token, advances the conservative order-ID
floor, and appends the durable pre-transmit event. If that critical transaction
fails, the broker send does not happen. No policy, dashboard, or alternate file
path can substitute for that evidence.

## How Data Is Read Today

The supported product boundary is the daemon, not the SQLite file.

| Question | Current path | Current limitation |
|---|---|---|
| Current operator or dashboard state | A typed daemon RPC and daemon-owned reader. | New dashboard needs a defined typed contract; direct SQL is not a product API. |
| Regime, rules, Canary, or capital history | Existing CLI commands call typed daemon RPC. | The daemon currently loads matching canonical event JSON and filters in Go instead of using most projection tables. This is not yet a scalable analytical read model. |
| Orders | `ibkr orders open`, `ibkr orders history`, and `ibkr order status`. | The local order lifecycle is intent/evidence, not a broker Activity Statement. Some reads still fold substantial event history. |
| Statement-derived equity | Typed reconciliation/equity paths over the current statement projection. | The current reader has a fixed result ceiling rather than a general paginated analytical API. |
| Retained observations | Narrow daemon-owned readers for their product purpose. | There is no supported general observation-research API. The existing pagination cursor also needs correction before it is advertised as an analytics contract. |
| Offline forensic SQL | Stop the daemon and open `daemon.db` read-only, or use a separately verified consistent backup. | SQL shapes are implementation details, not stable public contracts. |

Opening the file through SQLite while stopped is different from copying only
the main file while WAL changes may exist. Do not copy `daemon.db` alone from a
running daemon, mutate it with `sqlite3`, or point Grafana, notebooks, an ORM,
or another service at the live file.

A new dashboard or analytical feature should first define the question's
scope, row meaning, time basis, freshness, eligibility, ordering, pagination,
and redaction. Then add a bounded daemon query or typed export backed by the
index or projection that makes that contract efficient.

## Durability, Startup, and Recovery

### Normal durability

The store uses SQLite WAL, foreign keys, `synchronous=FULL`, supported
full-fsync controls, a bounded busy timeout, and one serialized connection.
One connection makes writer ordering simple, but a long unbounded read can also
delay a broker-critical write. Bounded and indexed reads are therefore a safety
property, not merely a performance improvement.

### Startup checks

Storage readiness completes before the daemon serves RPC, runs schedulers, or
connects to the broker. Startup checks private file types and modes, the SQLite
application ID and schema version, the checksummed migration ledger, the exact
table/index/trigger inventory, SQLite `quick_check`, foreign keys, the external
minimum write counter, and stored digests for selected payload columns.

Those checks detect schema drift, structural damage, rollback, and selected
payload-byte changes. They are not a claim of comprehensive tamper detection:
event headers, observation metadata and eligibility, typed projections, broker
bindings, and current statement winners are not all covered by a canonical
whole-record digest.

### Schema upgrades

An older valid schema is upgraded as a copy, not in place. Under the persistence
lock, the daemon creates a verified backup, migrates an unpublished
same-directory candidate, validates it, records coarse recovery state, and then
atomically swaps the candidate into place. A newer schema refuses downgrade;
an ambiguous recovery state fails closed rather than guessing.

The detailed crash-boundary protocol belongs in the
[SQLite implementation contract](design/daemon-sqlite-authority.md). Schema
version 1 is still the only production migration, so the general upgrade path
has not yet carried a released schema transition.

### Backup and restore today

The code can create and verify a standalone database backup, and upgrades or
cutovers use that primitive. There is not yet a supported scheduled-current
backup, backup-status, restore command, retention policy, off-host copy policy,
RPO/RTO, or rehearsed restore runbook. Recovery remains a deliberate offline
procedure because the database head, preview signer generation, broker-open
orders, and conservative order-ID floors must agree before writes resume.

That is an operational gap, not a feature hidden behind documentation. Until it
is closed, this page must not present backups as a complete operator recovery
system.

## Known Limits and Design Debt

These are current implementation boundaries, not speculative concerns:

| Area | Current state | Consequence or required decision |
|---|---|---|
| Event projections | Projection rows are written, while most non-order histories still read canonical event JSON. | Route bounded reads through indexed projections or remove redundant projections when compatibility allows. |
| Read concurrency | One SQLite connection serves reads and critical writes; several histories scan and filter broadly. | Bound and index reads before adding a separate read-only handle or pool. |
| Observation pagination | Results order by observation time plus ID, while the cursor carries only ID. | Backfilled or out-of-order rows can be skipped; fix the cursor before exposing general research queries. |
| Original Flex durability | Retained XML publication closes and renames but does not currently fsync the file and statements directory before the SQLite projection may commit. | A power-loss window can leave the projection without the claimed original file; repair the publication boundary. |
| Integrity coverage | Structural checks and selected payload hashes do not cover every semantic field or current statement winner. | State exactly what is protected; extend canonical validation where audit requirements demand it. |
| Backup and lifecycle | No scheduled backup, restore, retention, archive, size budget, or restore drill exists. | Define and operate the lifecycle before retained evidence grows without bound. |
| Desk identity | Most business state assumes one isolated daemon/account context; entity, portfolio, book, currency, and actor identity are not a general relational model. | Add explicit opaque identities before claiming consolidated family-office support. |
| Migration layout | Append-only triggers are appended dynamically to migration 1. | Future table triggers must live in the migration that creates them so migration-1 checksums remain immutable. |

The database contains 21 tables, but table count is not the main complexity
measure. Dedicated order-safety and statement-version tables earn their cost.
Write-only projections, unbounded duplicate snapshots, and application-only
relationships do not automatically earn theirs.

## When the Architecture Must Evolve

Keep one authoritative writer per daemon/account stack while that remains the
operational shape. Reconsider the topology when any of these becomes real:

- writers must run on several hosts or support automatic failover;
- many analytical readers need concurrent access;
- several entities, accounts, portfolios, books, or currencies must be
  consolidated with durable actor and command identity;
- retained data makes startup validation or current full-history scans
  operationally unsafe;
- the desk needs centralized retention, legal hold, audit anchoring, or
  recovery objectives that a device-local store cannot meet.

The likely next step is not to let dashboards read each live database. Keep the
per-daemon transactional store for local safety, then publish typed, redacted,
versioned exports or a change feed into a central read-only analytical and
control plane.

## Glossary

| Term | Plain-language meaning |
|---|---|
| Storage layer | The code and operating rules that preserve state, evidence, and recovery continuity. SQLite is the engine used by this layer. |
| Source of truth | The location the software is allowed to treat as current for one particular kind of fact. |
| Current state | The latest accepted version used by the running product. |
| Event | An immutable record of something the software observed, decided, or attempted. |
| Observation | A retained measurement from a named source, separate from the current conclusion derived from it. |
| Projection | Selected fields copied from a richer payload into searchable columns. |
| Revision check | Update only when the caller's expected revision is still current; otherwise reject the stale write. |
| Transaction | A group of database changes that all commit or all fail together. |
| WAL | SQLite's write-ahead log, which can hold committed changes not yet folded into the main database file. |
| Write counter | An ever-increasing number identifying the newest committed storage change. The external `daemon.db.head` file records the minimum accepted value. |
| Verified backup | A standalone copy tied to one known committed write counter and reopened for validation. |
| Upgrade a copy | Apply schema changes to an unpublished candidate, verify it, then atomically replace the old file. |
| Decision eligible | Allowed to support a live decision. Imported historical observations can be retained for research while remaining ineligible. |
| Broker route | The exact endpoint, client ID, account, and mode combination to which an order lifecycle belongs. |

## Reference Map

- [Architecture](architecture.md): process, broker, RPC, and state ownership.
- [SQLite Implementation Contract](design/daemon-sqlite-authority.md): cutover,
  durability, upgrade, and fail-closed recovery mechanics.
- [Platform Settings](design/platform-settings.md): why live preferences are a
  typed daemon document rather than a second configuration system.
- `internal/daemon/corestore/schema.go`: canonical tables, indexes, constraints,
  triggers, and migration ledger.
- `internal/daemon/corestore`: typed transactions, events, observations,
  statements, backup, validation, and upgrade code.
- [Trading Policy](policies.md): why human-authored limits remain outside SQLite even
  when applied state and resulting evidence are retained inside it.
