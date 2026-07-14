# Post-Trade Truth (Phase 3a): Flex Ingestion and Reconciliation

Updated: 2026-07-13 21:30 CEST
Status: implemented (phase 3a, 2026-07-13) — live with risk-policy v2; the
remaining human step is creating the Flex query/token and enabling [flex]
in config. This document is also the post-trade authority contract the
harness guide names as the second First Harness Milestone artifact
(docs/guides/trading-harness-development.md), alongside the risk
constitution (docs/design/risk-policy.md).

Phase 3a turns weekly capital reconciliation from a human attestation into a
machine diff against broker records. Broker truth comes from IBKR Flex
statements; the declared capital-event ledger and the local order journal
are claims and intent, never truth. Everything here is read-only toward the
broker: no order path, no submit eligibility, no freeze, no pins.

## Decisions (operator, 2026-07-13)

1. **Ingestion: Flex Web Service pull.** The daemon fetches statements
   itself using a Flex token + query id. Chosen over the recommended file
   drop; the accepted risks are recorded under Residual risk and the
   credential rules below are the mitigation.
2. **Cadence: daily.** One scheduled fetch per day. Daily
   `EquitySummaryInBase` rows arrive either way; what daily buys is
   next-morning detection of undeclared flows.
3. **Reconcile gate: required from day one.** Once 3a ships,
   `ibkr policy capital-event reconcile` refuses without referencing a recon
   report whose exceptions are all resolved. No shadow period. The escape
   valve for tooling outages is the existing one-shot override mechanism
   (below), not a soft mode.
4. **Deferred, still unapproved:** strategy-attribution tagging (needs
   Phase 2 experience); the 3b measurement reports; any alerting or
   auto-generated report artefacts (Phase 4).

Numbers approved by the operator (2026-07-13, second interview) and
recorded in risk-policy.toml v2 under `[recon]`: amount tolerance 0.5% with
a 5 EUR floor; date window 3 business days; maximum statement age for a
reconcile 4 calendar days (2 was rejected as failing every Monday); fetch
retry/backoff stays code-owned engineering constants.

## Four things, kept separate

- **Policy:** `max_unreconciled_days` and the reconcile-gate semantics live
  in the risk constitution. Requiring a report-backed reconcile changes what
  that key *means*, so shipping 3a includes a `policy_version` bump and a
  schema note — never a silent code-side reinterpretation.
- **Measurement:** statement records, the daily equity series, and the
  recon report. All typed, all carrying `as_of`, source, and finality.
- **Enforcement:** unchanged in 3a. The capital tier already degrades to
  `unknown` when the reconcile clock expires; 3a changes how the clock is
  fed, not what expiry does.
- **Reporting:** `ibkr recon` renders the latest report; the reconcile
  journal entry records which report was reviewed and how each exception
  was resolved.

## Authority

| Concept | Authoritative source | Typed contract | Finality | Fallback / unavailable |
|---|---|---|---|---|
| External cash flows, dividends, interest, fees, transfers, corporate actions | Flex statement line items | `internal/flexstmt` typed records | Final per report date; later restatements supersede by (account-day, line id) | statement source `unavailable`; recon report not producible |
| Daily equity curve (EUR base) | Flex `EquitySummaryInBase` | equity-series store | Same restatement rule | runtime observations remain, divergence metric marked unknown |
| Declared flows | `capital-events.jsonl` | existing v1 events | Provisional until matched | n/a (local) |
| Order intent and lifecycle | `order-journal.jsonl` | existing v1 events | Never broker truth | n/a (local) |
| Recon verdict | daemon recon engine | `rpc` recon report (id, fingerprint, coverage window, exceptions) | Regenerated per ingest; report id pins content | absent → reconcile refuses, clock runs out, tier degrades as today |
| Reconcile sign-off | human, via gated verb | journal entry referencing report id + resolutions | Final once journaled | one-shot override on `capital.max_unreconciled_days` extends the clock during outages (journaled, expiring, human-only) |

## Ingestion

**Credential rules (the price of the pull decision).** The Flex token
lives in its own file, `~/.config/ibkr/flex-token`, mode 0600, path
configurable under a new strict-loader `[flex]` config section (query id,
token path, enable flag). The token never appears in config.toml itself,
settings surfaces, RPC results, logs, journals, or errors; the settings
surface may show only `flex.configured: true/false` with source `config`.
Agent sessions must not read the token file — same standing as the
order-preview key. Token expiry (IBKR tokens expire on the order of a
year) surfaces as statement-source health `unavailable` with a renewal
action message; renewal is a human act at IBKR.

**Fetch mechanics.** Flex is a two-step API (SendRequest returns a
reference code; GetStatement polls until the report is generated) with
aggressive server-side throttling. One scheduled fetch per day
(single-flight, failure memory with retry-after — borrow-fee/earnings
refresher precedent), fetching a rolling window (last N days, N covering
the restatement horizon) so late broker corrections are picked up by
re-ingest. Raw XML is retained immutably under
`~/.local/state/ibkr/statements/` (0700/0600), one file per fetch, so every
recon report is reproducible from kept evidence.

**Parsing.** `internal/flexstmt` is a pure, fixture-tested parser: XML in,
typed records out. Statement text is untrusted broker data — typed
extraction only, unknown line types land in an `uncategorized` bucket that
always surfaces as an exception, never a silent drop, and nothing in a
statement can carry an instruction anywhere. Restatements supersede by
(account-day, line id); superseded lines are kept with a superseded mark
for audit.

## Reconciliation

Every ingest regenerates the recon report over the coverage window:

- **Flow matching:** statement deposits/withdrawals/transfers vs. declared
  ledger events, matched on type + amount (within the approved tolerance)
  + value date (within the approved window). Categories: `matched`,
  `missing_from_ledger` (statement flow with no declaration — the
  undeclared-withdrawal case), `ledger_only` (declaration with no statement
  line — fat-finger or timing), `amount_mismatch`, `date_mismatch`,
  `uncategorized` (unknown statement line type).
- **Non-flow lines** (dividends, withholding, interest, fees, corporate
  actions) are classified and *excluded* from flow matching by type — the
  machine, not the weekly eyeball, now guards the only channel that could
  understate drawdown (declaring a loss as a withdrawal would produce a
  `ledger_only` exception).
- **Ambiguity never auto-resolves.** Two candidate matches for one line is
  an exception, not a best-effort pick (never-false-match, the recon
  analogue of never-false-pass).
- **Resolutions**, all journaled: declare the missing event
  (`missing_from_ledger`), counter-declare (`ledger_only`), or dismiss with
  reason — dismiss is human-only and carries the report id.
- **Equity series:** daily `EquitySummaryInBase` is stored and compared
  with the runtime-observed peak/drawdown; divergence beyond a disclosed
  bound is a report warning (a data-quality fact about the runtime
  sampler, not an exception to resolve).
- Once flows are statement-confirmed, statement values become the
  authoritative `cumFlows` input and matched declarations are demoted to
  provisional bridge entries for the fetch lag (Rückbau R3/R4).

**The reconcile verb after 3a:** `ibkr policy capital-event reconcile
--report <id>` requires a report that exists, covers through the approved
max age, and has zero unresolved exceptions. Human-only, as today. During
a statement-source outage the sanctioned path is a one-shot override on
`capital.max_unreconciled_days` — journaled, reasoned, expiring — which
keeps outages visible instead of adding a quiet soft mode.

## Surfaces

`internal/flexstmt` (pure parser) → daemon statement store + recon engine +
scheduler → `internal/rpc` recon types → CLI `ibkr recon [--json]` and the
extended reconcile verb. MCP/SPA: none in 3a. New `[flex]` config section
regenerates the config reference (`make docs-regen`).

## Rückbau

| # | Retired | Replaced by | Trigger |
|---|---|---|---|
| R1 | Bare reconcile attestation | Report-backed reconcile | At 3a cutover (operator chose no shadow period); `policy_version` bump carries the semantic change |
| R2 | "Not an Activity Statement — ask for an export" disclaimers (MCP orders-history description, skills) | Pointer to the statement/recon surface | 3a shipped |
| R3 | Declared flows as the only `cumFlows` source | Statement-confirmed flows authoritative; declarations provisional | Two fetch cycles with all declarations auto-matched |
| R4 | Late-deposit peak-correction heuristic (`effective_at` vs. peak time) | Statement value-date correction | With R3 |
| R5 | Attestation-era reconcile prose in risk-policy.md and explain strings | This contract | With R1 |

## Safety invariants

- Read-only toward the broker; nothing here touches submit eligibility,
  blockers, freeze, pins, tokens, or any order path.
- Reconcile and every resolution verb stay human-origin-only.
- Statement content is untrusted data; typed extraction only; unknown
  content becomes exceptions, never actions.
- The Flex token is never readable through any RPC, log, journal, or
  settings surface, and never by agent sessions.
- Data absence never improves the picture: missing statements, parser
  gaps, and ambiguous matches all degrade loudly.

## Residual risk (accepted by the operator, 2026-07-13)

- A standing broker-history credential exists on disk (mitigated to a
  0600 single-purpose file outside config, but it exists).
- Require-from-day-one means a parser gap on a real statement quirk can
  stall reconciliation until fixed; the override valve keeps the stall
  visible rather than painless.
- Daily scheduled fetch adds a network dependency to a truth-critical
  path; failure memory and LKG raw files bound the damage to staleness.

## Verification

Recorded, anonymized Flex XML fixtures (normal, restatement, unknown line
type, FX-converted flow, in-kind transfer, corporate action, malformed);
recon engine table tests per category incl. ambiguity-never-auto-resolves
and dismiss-requires-human; equity-series divergence cases; reconcile-verb
refusal cases (no report, stale report, unresolved exceptions); token
never in any marshaled output (grep-style test over RPC results and logs);
`make check` + `make test` binding; live artifact: redacted `ibkr recon
--json` (report id, category counts, coverage window — no amounts) plus a
journaled report-backed reconcile performed by the operator.

## Rollback

Revert the 3a files; retained raw statements and the recon store are inert
orphans. The reconcile verb's report requirement retires by policy
revision, returning to bare attestation. No trading-path change either way.

## Out of scope

3b measurement reports (drawdown attribution, execution quality, adherence
scoring — gated on Phase 2 data), alerting, auto-generated artefacts,
strategy tagging, capital-allocation responses.
