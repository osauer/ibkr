# Alerts and Regime production commissioning

**Status:** Implementation and commissioning contract; Phase 6 is
shadow/advisory only

**Decision date:** 2026-07-20

**Owner:** osauer

**Shadow-producer policy approval:** 2026-07-21, desk-owner approval for
`market-stress-v1`, `orphan-reconcile-v1`, and `root-source-v1`

## Decision

The Alerts and Regime program has six implementation phases, preceded by one
operator-owned safety precondition. It establishes durable, source-neutral
decision evidence before any unified notification path is allowed to page the
operator.

`daemon.db` is the sole live authority for daemon-owned Regime state,
crash-recovery receipts, alert episode state, and decision evidence. The app
keeps only its separate app-local inbox and delivery ledger; it never opens the
daemon database and never becomes an alternate authority for whether a
candidate exists.

This program does not change a trading threshold, risk limit, freeze state,
broker-write gate, alert pageability rule, cooldown, or escalation cadence.
It also does not activate a unified sender. Those are separate human policy
and commissioning decisions after the shadow evidence is adequate.

## Non-negotiable boundaries

- The daemon owns source ingestion, Regime publication, candidate composition,
  episode lifecycle, source coverage, and evidence quality. CLI, MCP, app, and
  SPA surfaces may only adapt the typed result.
- Every alert episode identity includes the exact validated account and broker
  mode scope through an opaque one-way hash. Raw account or mode values are
  never persisted in alert keys, sent to the app, or exposed in status output.
  Identical facts in different account/mode scopes cannot alias.
- Missing, stale, partial, unavailable, invalid, or contradictory evidence is
  not a clear. It remains typed unknown or degraded. An active fact may be
  retained when its existing producer gate permits it, but degraded evidence
  cannot create a recovery, negative conclusion, or clean source-coverage
  claim.
- A successful call with zero candidates means clear only when the relevant
  expected sources are current and covered. Otherwise it means unknown.
- SQLite write failure, revision conflict, impossible lifecycle transition, or
  projection gap is surfaced and blocks publication of a falsely complete
  result. There is no file fallback, dual write, or legacy-history authority.
- Existing delivery owners remain in place during shadow commissioning. The
  Canary monitor, governance dispatcher, and order-mismatch watch continue to
  own their established delivery paths.
- Regime-authority and required MarketEvents health may enrich the current
  Canary advisory result, but they cannot silently change that established
  Canary path. The producer therefore carries one versioned compatibility
  projection of the exact pre-program Canary identity and mode eligibility;
  the legacy monitor consumes that projection atomically. A missing projection
  means an older daemon, while a malformed present projection fails closed.
- The source-neutral path is record-only: no unified transport, paging,
  cooldown, dwell, rearm, service-worker, foreground-badge, or unread-count
  cutover is authorized.
- Browser or push-service acceptance is not proof that a physical paired
  device received a notification. Physical-device receipt remains a distinct
  operator acceptance gate.

## Phase record

Phase 0 is an operator precondition, not an implementation phase. Phases 1-6
are the implementation and commissioning sequence.

| Phase | Outcome | Completion evidence |
|---|---|---|
| 0 — Operator safety | The operator chooses the observation/cutover window and retains sole authority over freeze, limits, trading mode, and any broker action. | Explicit operator go/no-go; no automated change to a trading guardrail. |
| 1 — Regime authority | One daemon-owned, durable last-good Regime snapshot is served with explicit authority health. Its streak, rule-stage, and decision-event projections are tied to the exact SQLite revision, publication time, and fingerprint. | Cold start is typed unavailable; stale last-good stays visibly stale; a future commit time is `clock_invalid` and cannot refresh or relax the rulebook; concurrent callers share one refresh; restart hydration is strict; a crash between snapshot commit and projections replays exactly one revision before a newer publication is allowed. |
| 2 — Candidate contract | Producers emit one validated, redacted, source-neutral candidate contract. The contract describes fact identity, lifecycle state, severity, evidence health/as-of, policy identity, and an allowlisted destination without granting delivery authority. | Contract validation covers every enum and rejects malformed identity, time, transition, health, destination, and private-display data. Equal input replays are stable; same-time divergent input is an equivocation, not a silent overwrite. |
| 3 — Durable lifecycle and inbox | The daemon persists episode and occurrence lifecycle in `daemon.db`; the app observes complete snapshots into its separate durable inbox ledger. App corruption is quarantined rather than normalized into plausible state. | Open, qualifying escalation, recovery, reopen, omission, restart, old-snapshot rejection, storage bounds, and quarantine are deterministic. App ingestion cannot alter legacy attention state or send a notification. |
| 4 — Data-health truthfulness | Snapshot state and source coverage distinguish current, partial, stale, unavailable, error, and not-yet-covered inputs. Outage or age can move a former clear to unknown, never preserve reassurance. | Fresh clear, degraded positive, degraded negative, poll outage, restart outage, cold outage, ageing, future time, and recovery scenarios retain the correct typed health and do not invent coverage. |
| 5 — Composer, replay, fault, and measurement | One daemon composer maps the fixed producer universe into durable episode observations and exposes redacted shadow measurements. Policy identity, source-store health, candidate snapshot, and account/mode scope are captured from one evaluation boundary. | Replay and retry are idempotent; failed registry application can retry the same input; current coverage does not survive a restart by assumption; source and scope collisions are rejected; fault injection proves no false clear; measurement distinguishes repeats, evidence revisions, duplicates, equivocations, lifecycle changes, coverage failures, and time-to-observe. |
| 6 — Shadow/advisory commissioning | The typed status remains `authority = "shadow"` and `delivery_active = false`. Candidate and inbox evidence may be reviewed, replayed, and labelled, but it cannot page or enforce. | Full verification and live redacted status prove shadow authority, inactive delivery, durable restart behavior, and explicit source-coverage gaps. Human precision and recall remain `unlabelled` until the operator supplies outcome labels. |

Installed cutovers are mechanically consumer-first. The normal combined
restart and `ibkr update --restart` paths restart and verify the app from the
installed executable before they mutate the daemon. If the app step fails,
the daemon remains untouched; if the later daemon step fails, the compatible
new app remains in place and the command reports the partial result without
rolling it back. The new app deliberately accepts a nil projection from an old
daemon, while a new daemon's present projection is strict. This removes the
otherwise brief old-app/new-daemon window in which an old monitor could
interpret richer advisory fields as established delivery input. A restart
that explicitly selects another daemon socket leaves the app untouched and is
reported as non-atomic; it is outside this production cutover guarantee.

## Regime publication and crash recovery

The durable Regime snapshot is a versioned SQLite state document. Its canonical
payload is immutable after publication, and callers receive deep copies. One
daemon-owned cache serves a fresh last-good immediately, may serve a stale
last-good with explicit stale health while a refresh runs, and returns typed
unavailability when no last-good exists.

The authoritative publication identity is the tuple of SQLite document
revision, commit timestamp, and semantic fingerprint. Streak state, the
rulebook's Regime-stage latch, and the Regime decision event are derived
projections. A separate receipt records that all three accepted that exact
publication. Startup reconciles a single interrupted publication before the
RPC socket is exposed; a missing, ahead, mismatched, or multi-revision receipt
gap fails closed. A live projection failure blocks publication revision N+1
until revision N has been repaired. The served-revision gate is atomic with
the cache read, so a callback failure racing a request cannot expose a newly
committed but unreceipted revision. A daemon clock behind the retained commit
serves that intact result only as stale `clock_invalid` context and suppresses
new refresh publication until wall time catches up. The rulebook treats a
future stage as carried fail-closed evidence; it cannot silently fall back to
fresh calm thresholds.

No CLI, MCP, app, brief, or status path recomputes a current Regime verdict or
reads a retired cache/history file. They consume the durable last-good and its
typed authority health.

This authority change intentionally alters one old timing edge: an incomplete
fan-out is no longer published as a new partial/error-row Regime snapshot.
Cold state remains unavailable; warm state retains the preceding last-good and
marks its authority degraded. The established-alert projection preserves the
old Canary interpretation of every snapshot that is actually served, but it
does not recreate an alert from an unpublishable partial snapshot. Restoring
that behavior would require a second Regime authority and is therefore outside
the compatibility promise.

## Alert authority and failure semantics

The fixed measurement universe is:

- Canary;
- Regime;
- rulebook;
- risk policy;
- protection;
- order integrity;
- reconciliation;
- governance; and
- data health.

A source is covered only after the composer has a current, typed evaluation
that is capable of expressing both an active fact and a trustworthy negative.
An unwired producer reports `producer_not_implemented`; a positive-only seam
reports `positive_only_not_wired`. Neither is counted as clear. Coverage is
current-process knowledge and is not reconstructed from a durable active
episode after restart.

The commissioned producer boundary is explicit:

- Canary consumes the producer-authored current result and its established-path
  compatibility projection.
- Risk policy, reconciliation, and governance consume one normalized Nudge
  evaluation. Valid v3 policy keeps v4 cadence and confirmed-flow reminders
  explicitly inactive without suppressing independent policy-drift, drawdown,
  or reconciliation-exception facts. V4-only reminder coverage remains gated
  by its own approved cadence and cutover evidence.
- Rulebook consumes the complete unfiltered `rules.snapshot`; watch/act rows
  open episodes, while only a current account-and-positions-backed snapshot may
  clear them.
- Order integrity consumes the journal-scoped open-order read model. A critical
  mismatch must repeat on two consecutive current portfolio-stream reads;
  stale, unprimed, future, or wrong-account stream evidence may retain an open
  episode but cannot clear it.
- Regime consumes the served, fingerprint-validated last-good snapshot under
  `market-stress-v1`. A current `early_warning` opens one watch episode;
  `confirmed_stress` and `panic` continue that episode at the lifecycle's
  governed severity (their uncapped levels are act and urgent), with only an
  upward severity change qualifying as an escalation. The producer replays the
  shared lifecycle classifier and source-age policy before trusting the served
  state; it does not override provenance or evidence-quality governors.
  Current `quiet`, `stabilization`, or `opportunity` is negative evidence.
  `data_quality`, a stale/unavailable authority, overdue required evidence, or
  an invalid `not_due` claim never opens an early warning and cannot recover an
  existing episode.
- Protection consumes only the complete, unfiltered positions coverage ledger
  backed by a current, account-matched portfolio-stream receipt. Under
  `orphan-reconcile-v1`, orphaned protective orders and
  reconciliation-required rows open watch episodes in Alerts. Partial or
  unprotected holdings remain context and are negative for this deliberately
  narrow producer; the policy does not assert that every holding must have a
  stop. Unknown or stale positions/order evidence retains an episode and
  cannot clear it.
- Data Health consumes the complete typed `status.health` projection under
  `root-source-v1`. It opens one watch episode per failing root in the v1
  allowlist: Gateway connectivity, SQLite storage, enabled proposal/opportunity
  engines, typed decision-surface data quality, and one aggregate IBKR
  data-farm root. Raw broker farm names never enter episode identity.
  `not_due`, normal computing, and intentionally disabled states are negative
  evidence. Startup remains uncovered until the first post-connect setup has
  completed, so a normal restart cannot manufacture a Gateway outage.
  Recovery requires a later complete status read in which that same root is
  current. Gateway-dependent capability rows are not fanned out into duplicate
  incidents. Earnings and borrow-fee availability remain source-owned
  Rulebook/Canary coverage dependencies rather than additional standalone Data
  Health episodes in v1.
- Data Health observation is detached and single-flight with an in-memory,
  bounded semantic-transition queue and failure backoff, so `status.health`
  never waits on the shadow write it triggers and an outage followed by a
  recovery remains ordered across an in-flight or failed write. Adjacent
  identical polls coalesce. The worker is drained before SQLite closes. A
  SQLite failure remains directly visible on the typed status surface; its
  shadow episode is best-effort because the failed SQLite authority cannot
  durably record its own outage. There is deliberately no second hidden
  persistence authority.
- Regime, Protection, and Data Health are not dependent on an open app or CLI.
  Daemon-owned 30-second engineering heartbeats re-observe Regime authority,
  rebuild Protection from the portfolio cache plus local order journal, and
  refresh typed health. The canonical status read retains its normal throttled
  reconnect behavior, and an empty held-position cache may renew the existing
  account-updates subscription; these are read-side stream repairs, never
  order or account mutations. Contract identity is retained, and ambiguous
  same-symbol fallback evidence cannot clear Protection. This interval is
  sized inside the shortest one-minute source silence horizon; it is not
  pageability, market threshold, or broker-write policy. Client reads may
  still provide earlier observations, and the same semantic throttle prevents
  duplicate lifecycle churn.
- All three policies use the Alerts destination only as a redacted shadow
  classification. Their delivery preference remains `unapproved`; none of
  these approvals authorizes transport, pageability, or legacy-owner cutover.

Source schedules do not masquerade as outages, and outages do not masquerade
as warnings. VIX and VIX3M keep separate schedules on the official options
calendar: VIX is due approximately 03:15–09:25 and 09:31–16:15 ET, while
VIX3M is due approximately 09:31–16:15 ET. A present frozen VIX3M is
`not_due` before its start, after the session's close-plus-15-minute
dissemination window, and on closed dates; a frozen VIX is also expected in
the 09:25–09:31 pause. A missing leg, frozen VIX during its due window, or
frozen VIX3M during its due window is `overdue`.
Gamma process health distinguishes `not_due`, `missed_session`, and
`no_last_good`: the latest completed-session result is context before the next
options open; absence, an older result, or failure to replace it after open is
a defect. Required-source defects produce an explicit undefined/data-quality
state, never `early_warning`. The external IBKR borrow-fee file is attempted during the official
U.S. equity regular session; outside that window it is `not_due`, while an actual
failed attempt and its bounded retry backoff are exposed separately. A last-good
file from the latest completed equity session is expected overnight; an older
file remains stale rather than being quieted as merely not due.

Candidate lifecycle has three externally meaningful states: open, qualifying
escalation, and recovered. The producer decides whether a fact is active and
which escalation qualifies; the registry owns durable, monotonic transition
identity. Repeated active observations, evidence revisions, and qualifying
escalations are measured separately so a noisy input does not masquerade as
many independent episodes.

The app polls the typed snapshot on its normal live cadence and records it in
the source-neutral inbox ledger. It projects an unavailable or stale snapshot
to unknown and persists that loss of confidence. Candidate episode and
occurrence keys remain private; public inbox and status DTOs expose allowlisted
presentation and aggregate health only. When the broker authority scope
changes, the app retains a bounded, generic previous-context projection with
`authority_scope_changed`; it does not recover or clear the old occurrence,
increment attention, or make it delivery-eligible. Returning to a prior scope
resumes that scope's private lifecycle instead of aliasing it with the current
one.

Pre-scope v1 daemon and app documents are never assigned a fabricated current
scope. Strictly valid legacy bytes are retained as typed unscoped migration
evidence and concrete v2 scopes start cold; malformed legacy state fails
closed or enters the app's quarantine path.

## Shadow topology

During Phase 6, the new path and the established delivery paths deliberately
coexist without sharing send authority:

| Layer | Shadow responsibility | Explicitly not authorized |
|---|---|---|
| Producer and daemon composer | Classify facts, capture source health and policy identity, measure coverage, persist lifecycle. | Transport selection, pageability, cooldown, or broker action. |
| Typed daemon RPC | Expose the complete candidate snapshot and redacted calibration status. | Acknowledge, suppress, mutate policy, or activate delivery. |
| App live service and inbox | Poll, validate, persist, quarantine, and render evidence for operator review. | Unified Web Push, service-worker routing changes, foreground notification, or badge/unread cutover. |
| Legacy delivery owners | Continue the pre-program Canary, governance, and order-integrity notification behavior. Canary consumes its producer-authored established-alert projection, including the prior canonical fingerprint and mode eligibility. | Treat the richer advisory result or shadow registry as permission to change cadence, dedupe identity, or duplicate a page. |
| Operator | Review evidence, label outcomes, approve policy, prove physical-device receipt, and authorize a later cutover. | Delegate broker-write or guardrail authority through an alert. |

## Measurements and promotion criteria

Shadow status records counts and timestamps; it does not choose its own
acceptance bands. Before any promotion, the operator must approve the band and
observation window for each measure below in a separate policy/cutover record.
No numerical threshold is approved by this document.

| Measure | Required evidence | Promotion question |
|---|---|---|
| Source coverage | Evaluations, covered evaluations, coverage failures, and explicit reason per expected source. | Are all sources intended for the cutover implemented and current often enough for their operating cadence, with every exclusion understood? |
| Decision quality | Operator-labelled alert/no-alert outcomes, including quiet periods, stress episodes, and documented near misses; precision and recall derived from those labels. | Is the observed false-alert and missed-alert trade-off inside the human-approved band for each source and severity? |
| Noise and lifecycle | Repeated active observations, evidence revisions, duplicate inputs/candidates, opened/escalated/recovered/reopened episodes, and equivocations. | Does one real condition produce one understandable episode, with only intended escalations and rearming? |
| Timeliness | Time-to-observe sample count, distribution, maximum, source as-of, and observation time. | Is end-to-end observation latency inside the approved band for that source's cadence and severity? |
| Durability | Registry-apply failures, retry results, restart replay, projection receipts, quarantine state, and fault-injection results. | Can every accepted fact survive restart, and can every interrupted write recover without a false clear, duplicate occurrence, or lost escalation? |
| Scope and privacy | Cross-account/mode collision tests, opaque identity checks, and redacted wire/app artifacts. | Is scope isolation exact, with no raw private identity or broker data on public surfaces? |
| Legacy comparison | Side-by-side candidate and established-delivery timelines with explained differences. | Are excess, missing, earlier, and later alerts understood before one legacy owner is retired? |
| Physical delivery | One explicitly commissioned end-to-end receipt on the actual paired device for each intended transport class. | Did the physical device receive and open the exact safe payload, rather than merely obtaining a push-service success? |

Promotion is source-by-source, not an all-or-nothing declaration. A source may
leave shadow only when its producer is fully covered, replay/fault evidence is
clean, decision-quality and timeliness measures meet approved bands, legacy
differences are accepted, safe copy and destination are approved, and physical
receipt is proven. The operator must then approve the exact pageability,
severity, cooldown, dwell, rearm, recovery-notice, and transport ownership
policy. Activation requires a separate reviewed change; changing the status
field or attaching a sender is not an administrative toggle.

## Rollback criteria and action

Before promotion, any unified-path defect leaves the path in shadow while the
legacy delivery owner continues. After a future promotion, rollback is
immediate for an authority, safety, or privacy invariant breach, including:

- a false clear from stale, unavailable, partial, invalid, or uncovered input;
- account/mode aliasing or private identity leakage;
- a non-idempotent replay, lost lifecycle transition, impossible projection
  gap, or unrepairable SQLite write failure;
- delivery while the typed authority says shadow or inactive;
- an unapproved cadence, pageability, cooldown, severity, badge, or
  service-worker behavior; or
- loss of physical-device behavior required by the approved transport class.

Metric degradation outside a later approved acceptance band also triggers the
rollback procedure defined by that promotion record. The action is to disable
the promoted unified delivery owner, restore the preceding delivery owner, and
return the source to shadow. Rollback does not delete `daemon.db`, rewrite
episode evidence, reopen legacy file/history authority, change a broker order,
or weaken a trading guardrail. Retained observations remain available for
incident analysis and a corrected replay.

## Remaining human gates

Phase 6 does not close these decisions:

1. Approve the measurement bands and observation windows used to judge source
   coverage, alert quality, noise, latency, and legacy parity.
2. Supply outcome labels so precision and recall stop being `unlabelled`.
3. For any source leaving shadow, approve pageability, delivery cadence,
   cooldown, dwell, rearm, recovery notice, transport destination, and
   legacy-owner retirement. The approved shadow severity and product-surface
   classification above do not authorize delivery.
4. Prove receipt and tap-through on the actual paired physical device.
5. Authorize a separate unified-transport, service-worker, badge, or foreground
   cutover, if desired.

Until all applicable gates are recorded, the production-safe result of this
program is better evidence and a durable shadow inbox, not more pages.

## Related authority

- [Architecture](../architecture.md)
- [Daemon SQLite authority](daemon-sqlite-authority.md)
- [Regime calibration](regime-calibration.md)
- [Risk governance nudges](risk-governance-nudges.md)
- [Trading harness development](../guides/trading-harness-development.md)
