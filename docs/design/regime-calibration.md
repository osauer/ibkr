# Regime calibration: confirmation eligibility, staleness honesty, and severity governance

**Status:** Reviewed — adversarial review + re-review passed 2026-06-12; gates implementation
**Created:** 2026-06-12 13:09 CEST
**Last update:** 2026-06-12 13:39 CEST
**Owner:** osauer
**Related:** `docs/specs/risk-regime-dashboard.md`, `docs/specs/regime-backtest-plan.md`,
`internal/rpc/lifecycle.go`, `internal/daemon/regime*.go`, `internal/cli/canary.go`,
`internal/cli/backtest.go`, `docs/design/platform-settings.md`,
`docs/templates/daemon-cli-trading-contract.md`, `docs/templates/spa-authority-matrix.md`

## Why this exists — the 2026-06-12 false positive

Pre-open on 2026-06-12 the engine headlined **"Broad Stress Regime /
confirmed_stress / severity act"** while every live tape input was green: SPY
+0.3% near its highs, VIX 18.84 vs VIX3M 21.42 in normal contango. The
maintainer judged the market calm; the engine's own tape evidence agreed.

The escalation was mechanically correct under current policy and wrong in
substance. The exact chain, verified in code:

1. **A 7 bps credit break counted as a full red.** HYG 79.95 sat 0.07% below
   its 50DMA (80.008) with SPY above the 97%-of-52-week-high line.
   `bandForHYGSPY` (`internal/daemon/regime_composite.go:509`) has no minimum
   depth: one tick below the DMA is the same red as a 3% break. The red streak
   was 1 session old, on a thin pre-open tick — and the indicator's own notes
   say "single-day moves are noise."
2. **A prior-evening gamma cache counted as a contemporaneous red.** The gamma
   compute ran 22:19 ET the prior evening; outside RTH the cache never
   refreshes (`softTTLClosed = math.MaxInt64`,
   `internal/daemon/gamma_zero_cache.go:259`). The regime row has no `stale`
   status path for gamma (only ok/computing/unavailable/error,
   `internal/daemon/regime.go:938-974`), and the lifecycle vote checks only
   rankability, never age (`rankableLifecycleGammaBand`,
   `internal/rpc/lifecycle.go:458`). Confirmed red evidence is stamped
   `timing: contemporaneous` unconditionally (`lifecycle.go:477-479`). Note
   the prior-evening profile still contained the day's *expired* 0DTE
   exposure — it was not merely old, it was partly about options that no
   longer existed.
3. **The two marginal reds rescued each other.** Credit's isolated-red
   downgrade is waived whenever any *raw* red exists elsewhere
   (`lifecycle.go:446`, `regime_composite.go:615`), and gamma has no
   isolated-red downgrade rule at all. So stale gamma red rescued the 7 bps
   credit red; `ClusterRedCount = 2` → `confirmed_stress` → `severity act`
   (`lifecycle.go:355-360`, `123-126`).
4. **Degraded readiness capped confidence, not severity.** The readiness check
   runs *after* stage/severity assignment and only calls
   `capLifecycleConfidence` (`lifecycle.go:152-155`). The stress tone
   short-circuits before the degraded-readiness check
   (`lifecycle.go:218-227`). So the snapshot simultaneously said
   `readiness: degraded`, `confidence: medium` — and `severity: act` with a
   red headline.
5. **Headline wording is computed in four places and two of them disagree at
   exactly this tally.** `verdictFor` (`regime_composite.go:478`) requires 3+
   reds for "Broad stress regime"; `regimePostureLabel` (`lifecycle.go:197`)
   says it at 2+. The other two copies live CLI-side:
   `(regimeComposite).verdict()` (`internal/cli/regime.go:824`, what the CLI
   actually renders) and `backtestRegimeVerdict`
   (`internal/cli/backtest.go:1131`). The SPA and MCP posture showed "Broad
   stress regime" while `composite.verdict` said "Stress signal present" for
   the same snapshot.
6. **The canary surface independently reproduces the same false positive.**
   `summarizeCanaryMarket` (`internal/cli/canary.go:539-611`) recomputes
   cluster bands through a *third copy* of the band/rescue logic
   (`rawRegimeClusterBands` / `confirmedRegimeClusterBands` /
   `hasIndependentRegimeRedCluster`, `internal/cli/backtest.go:1076-1116`)
   and `canaryConfirmedMarketStress` (`canary.go:1063`) confirms on raw
   `RedClusters >= 2`. Canary feeds the SPA regime panel
   (`renderRegimePanel` reads `snap.canary`, `web/app/app.js:3149-3164`) and
   the alert pipeline (`BuildCanaryFingerprint`,
   `internal/rpc/fingerprint.go:106`, deduped by
   `internal/app/alerts/alerts.go:32-44`) — the surfaces the user actually
   watches. Fixing the daemon lifecycle alone would not have fixed the
   incident as experienced.
7. **Every threshold involved is flagged `heuristic` + `pending_backtest`**
   (`heuristicThresholds`, `internal/daemon/regime_metadata.go:78-87`) — and
   no policy code reads either flag. The provenance disclosure is decorative.
8. **Streaks are persisted but purely presentational.** The streak store
   (`internal/daemon/regime_streaks.go`) tracks per-indicator band streaks
   across restarts, but lifecycle confirmation never consults them.

Each of these is an independent defect; together they let two pieces of weak
evidence escalate to the strongest non-panic posture against a green tape.

## Design principles

- **Confirmation must be deep, persistent, fresh, and independent.** Marginal,
  stale, or mutually-dependent evidence may warn; it may not confirm.
- **Display and policy separate — with one precise caveat.** Row bands keep
  their current semantics (a red row stays visible the moment a threshold
  crosses). What changes is which reds may *escalate*: the cluster-level
  rollup and lifecycle. Note this means a cluster can render yellow while its
  row renders red (the incident's credit cluster, post-fix) — that is
  intended and must be explained in row `band_reason` / cluster notes, not
  smoothed over.
- **Data quality caps escalation, not just confidence.** A snapshot that
  self-reports degraded inputs on its confirming evidence cannot demand "act".
- **One copy of policy.** Band classification, rescue/confirmation, and
  headline wording each exist in one shared place; daemon, CLI, canary, and
  backtest consume served values or the shared functions. (The third-reader
  threshold promised in `regime_composite.go:27-30` has been crossed.)
- **Raw measurements stay on the wire; banding stays disclosed.** New policy
  outputs are additive fields.
- **Every governor action is observable.** When policy downgrades or caps
  something, the payload says what was capped, from what, and why.
- **Calibration is forward-data-driven.** Persist decision history now
  (skew-R² JSONL precedent), backtest against it later, and only then promote
  thresholds out of `pending_backtest`. No threshold value in this document is
  claimed to be calibrated; they are defensible noise floors. The fast-path
  depths are the most invented numbers here and are flagged as such.

## Part 1 — Confirmation eligibility (the core mechanism)

Introduce one concept that all four task questions hang off: a red cluster
band is either **eligible** (may confirm stress) or **provisional** (may only
warn).

A red is *eligible* when all three hold:

1. **Depth** — the measurement clears the threshold by at least the
   indicator's minimum depth (noise floor), OR clears a deeper "fast-path"
   level that overrides persistence (so genuine day-one crashes still
   escalate same-day).
2. **Persistence** — the indicator's red streak (already persisted in
   `regime-streaks.json`) has reached the indicator's minimum sessions, OR
   the fast-path depth is met. A missing streak entry (fresh install, deleted
   store) counts as sessions = 1.
3. **Freshness** — the data is *cadence-fresh*: no newer observation should
   exist under the indicator's native cadence (Part 3). Overdue data can
   never confirm.

**Eligibility latches for the life of the red streak.** Once a red becomes
eligible, it stays eligible until the band exits red (per exit hysteresis),
even if the measurement wobbles back inside the minimum depth. This prevents
eligible/provisional churn at the depth boundary without a second hysteresis
band. (Freshness is not latched — if the feed goes stale mid-streak,
eligibility drops with it; see the severity governor.)

Provisional reds remain fully visible: the row renders red, the evidence list
carries them with `confirmed: false`, they appear in `unconfirmed`, and they
still trigger `early_warning`. They no longer:

- count toward the red tally used by `confirmed_stress` / `panic`,
- rescue another cluster from its isolated-red downgrade
  (`hasIndependentLifecycleRed` / `hasIndependentRedCluster` count *eligible*
  reds only — this kills marginal mutual confirmation),
- appear in `confirmed_by`,
- carry `timing: contemporaneous` or `severity: act` in evidence rows.

### Per-indicator calibration table

Band boundaries (green/yellow/red) are unchanged in this design — they are
the `pending_backtest` quantities the forward data will calibrate. This table
adds the eligibility gates and band-exit hysteresis. "Streak" means
consecutive NY *trading* sessions in red (see the weekend fix below); a
session held red only by exit hysteresis still counts toward the streak —
persistence measures how long the market has been in the state, and the state
includes hysteresis.

| Indicator | Red band (unchanged) | Min depth for eligible red | Fast path (eligible day 1) | Min streak | Cadence-freshness for eligibility | Exit hysteresis (band re-arm) |
|---|---|---|---|---|---|---|
| VIX term (VIX/VIX3M) | ratio ≥ 1.00 | ratio ≥ 1.00 (inversion is already discrete) | ratio ≥ 1.05 | 2 | same-session tick (live/frozen-today; VIX publishes from ~03:00 ET, so pre-open live ticks qualify) | leave red only when ratio < 0.98; re-enter at ≥ 1.00 |
| VVIX | ≥ 110 | ≥ 110 | ≥ 120 (existing isolated rule, kept) | 2 | latest official daily close (newest possible observation) | leave red < 105 |
| HYG/SPY credit proxy | HYG < 50DMA and SPY ≥ 97% of 52w high | HYG ≥ 0.25% below 50DMA | HYG ≥ 1.0% below 50DMA | 2 | RTH tick, or latest official close outside RTH (never a thin pre/post-market tick) | leave red only after HYG closes back above 50DMA |
| Credit spreads (HY OAS) | ≥ 5.5 or 20-obs widening ≥ 1.00 pp | levels are already deep — no extra depth | n/a (official daily series) | 1 | series ≤ 7d old (as today) | leave red < 5.25 and widening < 0.85 pp |
| Funding (CP−T-bill) | ≥ 75 bp | ≥ 75 bp | n/a | 1 | series ≤ 7d old (as today) | leave red < 65 bp |
| USD/JPY | yen +≥2%/week | ≥ 2% (speed *is* the depth; Aug-2024-calibrated) | ≥ 2% in 3 days (existing spec prose, now enforced) | 1 | same-day tick or same-day close | leave red < 1.5% |
| Dealer gamma | spot below γ-zero / wholly short profile | see gamma paths below | see gamma paths below | 1 | compute `AsOf` within the current NY trading date | leave red when gap > +0.5% |
| SPX breadth | < 40% above 50DMA with SPX near highs | ≤ 38% | ≤ 30% | 2 | last completed session's compute (the newest possible observation) | leave red > 45% |

**Gamma eligibility, all three red paths** (red has three producers —
`classifyGammaBand` gap path, no-crossing wholly-short profile
(`regime_streaks.go:390-398`), and the combined-scope weighted vote
(`combineGammaComputedBands`, `regime_streaks.go:415-458`)):

- (a) gap path: min depth = gap ≤ −0.5% below γ-zero (within ±0.5% is
  transition noise); fast path gap ≤ −2.0%.
- (b) wholly-short profile with no crossing: an extreme state — treated as
  fast-path (eligible day 1, depth gate vacuous).
- (c) combined-scope vote: eligibility evaluated on the per-index weighted
  gap that produced the vote, same −0.5%/−2.0% levels.

Rationale highlights:

- **HYG depth 0.25%** ≈ the noise floor of HYG's typical daily range; the
  incident's 0.07% fails it. The 1.0% fast path keeps a genuine credit gap
  eligible on day one. Depth is measured against the DMA:
  `(dma − price) / dma`.
- **Streak = 2 for vol/credit-proxy/breadth** enforces what the indicator
  notes already claim in prose ("sustained inversion over 2-3 sessions",
  "single-day moves are noise") but never enforced. Fast-moving indicators
  (USD/JPY, gamma, funding, cash OAS) keep streak 1 because their red bands
  are either speed-defined or already deep.
- **Exit hysteresis** prevents band flapping at the boundary (red → green →
  red consuming the streak reset each time). It is applied **once, in the
  daemon fetch/annotate path**, so the served `Band` is post-hysteresis;
  every downstream reader (CLI, canary, SPA, MCP) consumes served bands and
  needs no store access. The previous band comes from the streak store
  (`StreakStore.Get`), which lives daemon-side where banding happens.
- **Panic is untouched.** SPY −4%/−7% tape triggers stay as they are; tape is
  never depth/streak-gated. Real crashes escalate immediately through the
  panic path and through fast-path depths regardless of streaks. (The
  2026-07-19 closed-date pass adds a session gate, not a depth/streak gate:
  frozen closed-date prints cannot fire the tape branches, live trading-date
  prints keep immediate escalation — see Part 2.)
- Deleting `regime-streaks.json` mid-stress demotes an ongoing confirmation
  to provisional until fast-path depth or two fresh sessions re-establish it.
  Accepted: the store is daemon-owned runtime state, not user-facing.

### Trading-day streak fix

`StreakStore.Tick` increments on any new NY *calendar* date
(`regime_streaks.go:181`), so a Saturday poll inflates streaks. Sessions must
be derived from NY trading days — `spx.CompletedSessionKey`
(`internal/breadth/spx/scheduler.go:118`) is the holiday-aware precedent.
Small correctness fix, bundled because eligibility makes streaks load-bearing.

## Part 2 — Lifecycle and severity policy

### Stage triggers (revised)

`BuildRegimeLifecycle` (`internal/rpc/lifecycle.go:89`) keeps its shape; the
red tallies change meaning. All existing branches are kept — including the
`SPY ≤ −4% && yellow ≥ 2` branch (`lifecycle.go:358`) the tape can satisfy
without any red:

- `panic` — `eligibleRed ≥ 3 || (SPY ≤ −4% && eligibleRed ≥ 1) || SPY ≤ −7%`.
- `confirmed_stress` — `eligibleRed ≥ 2 || (eligibleRed ≥ 1 && SPY ≤ −2.5%)
  || (SPY ≤ −4% && yellow ≥ 2) || (eligibleRed ≥ 1 && VIX +20%)`.
- `early_warning` — unchanged, and explicitly the home of provisional reds:
  `rawRed ≥ 1 || yellow ≥ 3 || provisional present || SPY ≤ −1.5% || VIX +10%`.
- `opportunity` / `stabilization` / `quiet` — unchanged.

**Closed-date tape gating (added 2026-07-19).** Every tape term above reads
the direct SPY/VIX day-change prints, which freeze at last-session values on
official non-trading dates and can even reset independently while closed (the
2026-07-18/19 weekend journal held `early_warning`/watch on a half-reset
Saturday print: SPY change collapsed to 0.00 while VIX kept Friday's +12%).
The stage arms, the governor's SPY/VIX co-sign arms, and the pure-tape panic
exemption therefore require a confirmable session — `tape_session_state !=
closed_date`, classified by `rpc.TapeSessionFor` (marketcal US-equity date
state; the shared copy the canary tape row also keys on), stamped by the
daemon at snapshot time and by the backtest replay from the observation
clock, empty-and-fail-open outside embedded calendar coverage. Cluster-driven
terms and the status-gated term-inversion co-sign are untouched. The isolated
equity-vol corroboration inside cluster combination deliberately stays
session-blind (decision 2026-07-19: bounded residual — it can only preserve a
red that live-session banding produced, and its worst weekend effect is
cluster-grade early warning; revisit with journal evidence). The rulebook's
regime-stage latch skips closed-date snapshots so the last trading-date stage
ages into the carried worse-of(carried, calm) path instead of a weekend stage
re-latching fresh.

On 2026-06-12 this yields: HYG red provisional (depth 0.07% < 0.25%, streak
1 < 2), gamma red provisional (prior-trading-date compute) → `eligibleRed =
0` → `early_warning / watch`, both clusters in `unconfirmed`. Correct
posture: "something worth watching, not confirmed, data partly stale."

### Severity governor (ordered, applied after stage selection)

1. **Provenance gate** (task Q4 — the explicit policy):

   > While a confirming cluster's threshold set carries
   > `pending_backtest: true`, its evidence is *heuristic evidence*. The
   > gate lifts when either a fresh tape co-signature is present in the same
   > snapshot (SPY day change ≤ −1.5%, or VIX day change ≥ +10%, or a
   > same-session VIX-term inversion at ratio ≥ 1.00 on a fresh tick) or
   > every confirming set has been promoted. Without a lift, heuristic
   > evidence is capped one severity rung down: stage `confirmed_stress`
   > reads **watch** instead of act, and the `eligibleRed ≥ 3` panic branch
   > reads **act** instead of urgent (three deep, fresh, persistent,
   > independent reds have earned a strong response — but never the maximum
   > on unvalidated thresholds alone). The pure-tape panic branches
   > (SPY ≤ −7%; SPY ≤ −4%, which is itself act-grade tape) carry their
   > co-signature by construction and always reach **urgent**.
   > A threshold set is promoted out of `pending_backtest` per versioned
   > label (e.g. `hyg_spy_credit_proxy_v1` → `_v2`) only through the backtest
   > plan, with documented precision/recall on the forward-collected
   > decisions journal (Part 4).

   The `PendingBacktest` flag on `RegimeThresholds` finally becomes
   load-bearing: the governor reads it from the snapshot's own metadata, so
   per-set promotion automatically relaxes the gate without policy edits.
   Note the tape-corroborated `confirmed_stress` branches (SPY ≤ −2.5%, VIX
   +20%, SPY ≤ −4%) satisfy the co-sign by construction — the gate bites only
   on the pure `eligibleRed ≥ 2` path, which is exactly the incident shape.

2. **Evidence-keyed readiness cap.** If any cluster in `confirmed_by` has
   stale / partial / degraded data quality, severity is capped at **watch**
   (urgent exempt only via the pure-tape SPY ≤ −4%/−7% paths, which are
   self-evidencing). Keyed to the *confirming* clusters, not global
   readiness: one unavailable funding feed must not mute a fresh, deep,
   multi-cluster confirmation elsewhere. (Same evidence-keyed-guard pattern
   as the protection panel's `positions_pending`.) Global
   `readiness: degraded` keeps its current meaning and keeps capping
   confidence only.
3. **Disclosure.** Any cap or downgrade emits a governor record (new
   `lifecycle.governors[]`, Part 5): `{action: "severity_capped", from:
   "act", to: "watch", reason: "pending_backtest_no_tape_cosign", clusters:
   ["credit","gamma"]}`. Nothing is silently weakened.

Severity reachability after this design:

| Severity | Reachable when |
|---|---|
| observe | default / stabilization |
| watch | any warning state; the ceiling for heuristic-only `confirmed_stress` without co-sign, and for quality-impaired confirmation |
| act | stage `confirmed_stress` (any branch) with promotion or tape co-sign and quality-clean confirmers; OR heuristic-only `eligibleRed ≥ 3` panic without co-sign (governor record: "urgent withheld") |
| urgent | pure-tape panic (SPY ≤ −7%, or ≤ −4% with an eligible red — the tape is act-grade by construction); or `eligibleRed ≥ 3` panic with co-sign or full promotion |

The ladder is monotone in evidence: 2 heuristic eligible reds → watch, 3 →
act, tape co-sign or promotion lifts each one rung. "Act" is reachable with a
single eligible red **when the tape co-signs** (SPY ≤ −2.5% / VIX +20%
branches) — intended; the tape is the second witness. Stage and severity stay
separable throughout: a governed panic reads stage `panic`, severity `act`,
with the governor record explaining the withheld rung.

### Tone follows governed severity — stated explicitly

When the governor caps severity, the *stage* is not rewritten: two deep, fresh,
persistent eligible reds with no tape co-sign still produce stage
`confirmed_stress` and label "Confirmed stress regime". The display tone does
follow the governed severity: `severity: watch` renders tone `watch` (amber),
with a governor record explaining why act/red was withheld. This preserves
headroom for act-grade stress and full risk-off conditions while keeping the
evidence label honest. The incident case remains weaker still (provisional reds
→ `early_warning`, tone `watch`).

### Timing honesty

Evidence rows derive `timing` from cadence-freshness: only cadence-fresh data
may be `contemporaneous`; overdue evidence is `forward_warning` with
`confirmed: false`. A 22:19-yesterday gamma read can never again be presented
as contemporaneous confirmation.

### Headline unification

One function, one wording table, in `internal/rpc`; all four current copies
(`regimePostureLabel`, daemon `verdictFor`, CLI `(regimeComposite).verdict`,
CLI `backtestRegimeVerdict`) collapse onto it. The CLI renders the *served*
`composite.verdict`; the backtest builder calls the shared function. First
match wins:

| Condition | Label | Tone |
|---|---|---|
| ranked == 0 | No usable signal yet | data_quality |
| ranked < 3 | Insufficient signal — too few inputs ready | data_quality |
| all ranked clusters eligible-red, none unranked | Full risk-off conditions | risk_off |
| eligible red ≥ 3 | Broad stress regime | stress |
| stage == confirmed_stress and severity == watch | Confirmed stress regime | watch |
| stage == confirmed_stress and severity >= act | Confirmed stress regime | stress |
| raw red ≥ 1 (provisional or single eligible without tape) | Stress signal present | watch |
| yellow ≥ 3 | Elevated stress watch | watch |
| otherwise | Normal regime | normal |

This fixes the red==2 drift (incident headline), makes "Broad" mean broad
again, and removes the label/stage mismatch when a tape-corroborated single
red confirms. The Tone column is the display contract after severity governors:
the label can confirm stress while the tone remains watch if policy withheld the
act rung. A pure-tape panic with few reds still renders red because the panic
stage reaches act/urgent severity.

### Deferred: stage dwell

The first draft proposed a persisted stage-dwell (hold `confirmed_stress` one
session before de-escalating). Review killed it for v1: no flap incident has
been observed, eligibility latching + exit hysteresis already dampen the
plausible flap sources, dwell-on-stale-evidence contradicts staleness honesty
(a confirming feed going stale would *hold* the stress headline on evidence
the snapshot itself reports stale), and the decisions journal is precisely
the instrument that will measure whether stage flap exists. Revisit with
journal data; if added later it must re-verify the held stage's confirming
evidence is still eligible.

## Part 3 — Staleness model: cadence-relative freshness, served on the wire

### The rule

An observation is **cadence-fresh** when no newer observation *should* exist
under the indicator's native cadence; it is **overdue** otherwise. Overdue
data cannot make a red eligible. This replaces the first draft's
"prior-session data can never confirm" absolute, which was both too strong
(breadth's post-close compute and HYG's settled close are legitimately the
newest possible observations for the next session) and too weak (a
"post-close window" clause would have re-admitted the incident's
prior-evening gamma).

Per-indicator cadence policy:

| Indicator | Native cadence | Fresh means | Overdue example |
|---|---|---|---|
| VIX term | intraday ticks (from ~03:00 ET) | same-session tick, incl. pre-open live ticks | frozen prior-day close after ticks should flow |
| VVIX | one official close per day | latest published daily close | close > 1 publication day behind |
| HYG/SPY | intraday RTH ticks; settled closes outside RTH | RTH tick during RTH; latest official close otherwise (thin pre/post-market ticks are *never* the banding input — this also removes the incident's thin-tick wobble) | missing yesterday's close |
| Credit OAS / funding | official daily series with publication lag | ≤ 7 days (unchanged) | > 7 days |
| USD/JPY | 24/5 FX ticks | same-day tick or same-day close | only a prior-day close |
| Dealer gamma | intraday-capable compute during option RTH | compute `AsOf` within the current NY trading date | any prior-date compute — pre-open, gamma simply cannot confirm yet (yesterday evening's profile includes 0DTE exposure that has since expired) |
| Breadth | once per session, post-close | last completed session's compute — inherently the newest possible | compute older than the last completed session |

The gamma/breadth asymmetry is principled, not ad-hoc: breadth's inputs
(daily closes) cannot exist intraday, so its post-close compute *is* current;
gamma's inputs (live option chains) refresh intraday and roll at the open, so
a prior-date compute is overdue by definition.

Gamma additionally gets the missing `stale` status path: envelope ready but
`AsOf` from a prior trading date → row `status: stale`, band visible, never
eligible (`bandForGamma` keeps requiring `ok`; cadence-freshness fails too —
two independent guards).

### Served policy, no hardcoded twins, no churn

Following the protection-panel pattern (`renderProtectionTimestamp` /
`goDurationMinutes` deriving from served `proposal_cadence`):

- `SourceHealth.MaxAgeSeconds` — exists in the contract, never populated for
  regime (`BuildRegimeSourceHealth`, `lifecycle.go:300-312`) — gets filled
  from the cadence policy, per cluster. The SPA's hardcoded
  `staleMinutes: 60` in `renderRegimePanel` (`web/app/app.js:3159-3161`) is
  replaced by a value derived from served max ages.
- Per-row, `RegimeIndicatorMeta` gains `freshness {class, max_age_seconds}`
  where class ∈ {fresh, overdue}. **No wall-clock `age_seconds` on served
  rows**: the app's change detection JSON-hashes the monitor result
  (`internal/app/live/service.go:863-876`), and a ticking age field would
  emit an SSE "regime" event every poll. Clients derive age from the existing
  `as_of` — the same reason `compactSourceHealth` already strips
  `AgeSeconds` (`internal/rpc/compact.go`).
- No separate top-level `policy` block (first draft had one; review cut it):
  per-row gates land on `eligibility`/`freshness` meta, max ages on
  `SourceHealth`, and the provenance gate discloses itself through
  `governors[]` when it bites.

### Fingerprint policy

Fingerprints feed alert dedupe, so new fields need an explicit stance:

- `Evidence.Confirmed` is already projected into `lifecycleFingerprint`
  (`lifecycle.go:320-347`), so eligibility flips re-key the fingerprint —
  intended: eligible→provisional is a semantically different state and
  *should* re-alert.
- `governors[]` enter the projection as `{action, reason}` only — never ages,
  depths, or other continuous values.
- `freshness.class` (binary) may be projected; `max_age_seconds` and all raw
  measurements stay out.
- The projection version bumps `lifecycle-fp-v1` → `lifecycle-fp-v2`. One-time
  alert re-fire on upgrade: accepted and called out in the changelog.

## Part 4 — Persistence and the calibration data gap (Q2)

### What exists today

- `regime-streaks.json` (XDG cache): current band + sessions per indicator —
  current state only, no history.
- `regime-history/` (XDG cache): HMDS daily-bar cache — *inputs*, not
  decisions.
- `gamma-skew-diagnostics.jsonl` (XDG state): the skew-R² precedent —
  append-only single file, never read at runtime, safe to delete.
- `ibkr regime --log <path>`: manual, opt-in JSONL of full snapshots.
- `docs/specs/regime-backtest-plan.md`: PIT-panel methodology, with gamma and
  breadth explicitly *unavailable* in its historical tiers.

Conclusion: **no automatic persisted history of indicator values, bands, or
lifecycle decisions exists.** The 2026-06-12 incident cannot be fully
reconstructed from disk today, and the `pending_backtest` thresholds for
gamma/breadth can never be calibrated from historical data — forward
collection is the only path (same finding as the skew-R² analysis).

**Update 2026-07-20:** the decisions journal below is now also queryable. The
daemon maintains a derived SQLite index (`$XDG_STATE_HOME/ibkr/history.db`,
see `docs/design/history-index.md`) with automatic backfill and tail ingest.
`ibkr regime history` serves timelines over typed RPC, and offline
calibration reads use read-only `sqlite3` (including `json_extract` over the
verbatim journal lines) instead of `jq`. The JSONL journal stays the evidence
of record. Since the same day's phase-2 build, monthly rotation bounds the
raw journal (`history.rotation.keep_raw_months`, default 2); older months
live as immutable gzip archives that, together with the index, preserve the
full corpus. A sibling canary-decisions journal now collects the portfolio
side under the same mechanics.

### Decisions journal (new)

`$XDG_STATE_HOME/ibkr/regime-decisions.jsonl` — one line per
*decision-relevant* snapshot:

- **When:** every assembled snapshot whose semantic fingerprint differs from
  the last persisted line, plus one heartbeat line per hour while snapshots
  are being served (the app polls regime at 1-minute cadence
  (`internal/app/live/service.go:86`); fingerprint dedupe keeps the journal
  small and the heartbeat keeps a baseline for time-in-state statistics).
- **Schema (versioned `v: 1`):** `ts`, `session_key`; per indicator: raw
  value(s), band, status, freshness class, age at evaluation, streak
  sessions, depth metric (e.g. `hyg_dma_gap_pct`, `vix_ratio`,
  `gamma_gap_pct`), thresholds label; cluster bands raw/confirmed/eligible;
  composite tallies; lifecycle `{stage, severity, readiness, confidence}`;
  posture label; governor records; data-quality statuses.
- **File contract:** single append-only file, exactly the skew-diagnostics
  contract — never read at runtime, safe to delete. No rotation in v1
  (~hundreds of bytes per line, tens of lines per active day; revisit if size
  is ever a demonstrated problem).
- **Consumers:** the backtest plan's forward passes (false-alarm and recall
  measurement against labeled episodes), threshold promotion evidence, and
  incident forensics.

### Promotion criteria (binding for leaving `pending_backtest`)

A threshold set may drop `pending_backtest` only with: ≥ 6 months of
decisions-journal coverage including at least one labeled stress episode or
documented near-miss set; measured false-alarm rate and recall against the
backtest plan's labels; and a written delta in the spec doc bumping the set's
version label. The journal exists precisely so this stops being aspirational.

## Part 5 — Front-to-back change list (Q3)

Implementation goes through `docs/templates/daemon-cli-trading-contract.md`
first (daemon/CLI semantic change), and the SPA rendering changes go through
`docs/templates/spa-authority-matrix.md`. The authority table below seeds the
contract template.

| Concept | Authoritative source | Typed contract | Renderers |
|---|---|---|---|
| Band thresholds + provenance | daemon code + spec doc (versioned labels) | `RegimeThresholds` (exists) | CLI `--explain`, MCP, SPA tooltip |
| Eligibility gates (depth/streak/freshness) | daemon policy, computed once daemon-side | new `RegimeIndicatorMeta.eligibility` | CLI row suffix, SPA detail line |
| Cadence/staleness policy (max ages) | daemon policy, served | `SourceHealth.MaxAgeSeconds` + row `freshness` | SPA badges, CLI as-of column |
| Lifecycle stage/severity + governors | shared rpc lifecycle builder | `LifecycleState` + new `governors[]` | CLI summary, MCP, SPA headline |
| Headline wording + tone | single shared function in `internal/rpc` | `composite.verdict` == `posture.label` | all (CLI renders served verdict) |
| Canary market-stress confirmation | served eligible tallies / lifecycle | existing canary contract fields | canary CLI, SPA, alerts |
| Decision history | daemon journal (XDG state) | n/a (offline file) | backtest tooling |

### Daemon (`internal/daemon`)

- `regime_streaks.go` — trading-day session keys; exit-hysteresis-aware
  classification (consult previous band via the store, daemon-side only);
  expose depth metrics.
- `regime_composite.go` — eligibility computation per cluster (with latch);
  independence rescue counts eligible reds only; `verdictFor` delegates to
  the shared rpc wording function; new tallies (`ClusterEligibleRedCount`).
- `regime.go` — HYG/SPY banding input switches to latest official close
  outside RTH; gamma row gains `stale` status for prior-trading-date `AsOf`;
  cadence-freshness computation per row.
- `regime_metadata.go` — populate eligibility + freshness in row meta; serve
  `MaxAgeSeconds`.
- New: `regime_decisions.go` (journal writer + fingerprint dedupe + hourly
  heartbeat).

### Contract (`internal/rpc`)

Additive only; raw measurements untouched:

- `RegimeIndicatorMeta` += `freshness {class, max_age_seconds}`,
  `eligibility {eligible, latched, reasons[]}` (reasons name the failed gate:
  `depth_below_min`, `streak_1_of_2`, `data_overdue`).
- `LifecycleState` += `governors []GovernorAction`.
- `RegimeComposite` += eligible-red tallies.
- Lifecycle builder: revised triggers (all branches kept), governor ordering,
  timing honesty; shared headline/tone function; fingerprint projection v2.
- `CompactRegimeSnapshot` keeps the new fields; `CompactRegimeMonitor`'s
  flattened indicator rows (`compact.go:171-205`) gain explicit
  `eligible`/`freshness_class` fields — they are agent-relevant. No ticking
  values in either view (SSE-hash stability).

### CLI (`internal/cli`)

- `regime.go` — row rendering: provisional marker on non-eligible reds, e.g.
  `stress (provisional — day 1 of 2, depth 0.07% < 0.25%)`; summary governor
  line ("act withheld: thresholds pending backtest, no tape co-sign");
  `--explain` gains eligibility gates and cadence policy; the local
  `(regimeComposite).verdict()` copy is deleted in favor of the served
  verdict.
- `backtest.go` — `rawRegimeClusterBands` / `confirmedRegimeClusterBands` /
  `hasIndependentRegimeRedCluster` / `backtestRegimeVerdict` collapse onto
  the shared rpc functions (the promised lift-on-third-reader).
- `canary.go` — **blocker fix:** `summarizeCanaryMarket` and
  `canaryConfirmedMarketStress` consume served eligible tallies / the shared
  confirmation function instead of recomputing from raw bands; canary's
  market severity inherits the governor. Canary row wording gains the same
  provisional language.

### MCP (`internal/mcp/tools.go`)

`ibkr_regime` (and the regime-relevant text in `ibkr_canary`) updated to
documentation grade: explain eligible-vs-provisional reds, the governor (why
severity may read watch while two rows show red), cadence freshness, and that
`governors[]` is the place to look before concluding the engine is "ignoring"
red rows. Then `make docs-regen`; `make check` enforces no drift.

### SPA (`web/app/app.js`)

- Headline tone keeps reading `posture.tone` — it inherits the unified
  wording and tone table automatically.
- `renderRegimePanel` stale badge derives from served max ages (drop
  hardcoded `staleMinutes: 60`), reusing the `goDurationMinutes` pattern.
- Status line gains the provisional/governor detail ("2 stress signals
  pending confirmation; data partly stale") instead of unqualified red.
- Goes through `docs/templates/spa-authority-matrix.md`.

### Spec prose, notes, and doc gates

- `docs/specs/risk-regime-dashboard.md`: new "Confirmation eligibility and
  severity governance" section; per-indicator tables gain
  depth/streak/freshness columns; promotion criteria section.
- `docs/specs/regime-backtest-plan.md`: decisions journal becomes the
  forward-collection corpus; promotion criteria cross-referenced.
- Both specs have `.html` twins under the docs-html sync gate → hand-convert
  and `make docs-html-stamp`.
- Indicator notes constants in `regime.go`: each gains one sentence naming
  its eligibility gates (the notes are consumer-visible documentation).

### Tests

- **Incident regression fixture (regime):** synthetic snapshot reproducing
  2026-06-12 inputs (HYG 79.95/50DMA 80.008, SPY +0.3% near highs,
  prior-evening gamma red, VIX 18.84/VIX3M 21.42, vol cluster stale)
  asserting `early_warning / watch`, headline "Stress signal present", both
  clusters in `unconfirmed`, governor records present.
- **Incident regression fixture (canary):** same inputs through
  `summarizeCanaryMarket` asserting no "Confirmed market stress" row and no
  act-severity canary alert.
- Eligibility unit tables per indicator (depth/streak/fast-path/freshness/
  latch), incl. gamma's three red paths and nil-streak fresh-install.
- Hysteresis transition tables (enter/exit, no flap; hysteresis-held sessions
  count toward streak).
- Crash-sensitivity fixtures: Aug-2024-style carry unwind (fast paths + tape
  co-sign ⇒ confirmed_stress day 1), gap-crash (panic unaffected), slow-bleed
  2007-style (OAS+funding eligible, act gated on co-sign or promotion —
  asserting the documented trade-off, not hiding it).
- Journal: fingerprint dedupe, heartbeat, valid JSONL.
- Contract: compact + monitor views keep new fields; **monitor-hash
  stability** (two consecutive snapshots with identical semantics hash
  identically — no ticking fields); verdict == posture.label property test
  across all four former call sites; fingerprint v2 projection test (age-only
  change does not re-key).
- Verification per repo rules: `make check`; **full `make smoke`** (daemon +
  wire-path change), `make restart-daemon`, then `ibkr regime` /
  `ibkr regime --json --explain` and an `ibkr canary` output pasted in the
  completion message.

## Part 6 — Settings knobs (deliberately almost none)

Review position, adopted: the eligibility gates (min depth, streaks, co-sign)
are the *same class* of pending-backtest confirmation policy as the band
values themselves — code/spec-owned until promotion. User-tunable gates would
also fork the decisions journal's comparability, undermining Part 4. So:

- **Becomes a setting now:** `regime.journal.enabled` (default true) — runtime
  preference, daemon XDG state per `docs/design/platform-settings.md`.
- **Stays a code constant until promotion:** every gate value in Part 1's
  table, the co-sign thresholds, the cadence max ages. They are named
  constants in one place with the threshold-set version labels, so a future
  settings task can expose any of them *after* the journal gives promotion
  evidence — but the recommendation to that task is: don't, until then.

## Non-goals

- No backtest execution or threshold re-derivation here — there is no data
  yet; that is what the journal enables.
- No settings mechanism build (Part 6 names the one knob).
- No stage dwell / lifecycle persistence (deferred; journal will measure).
- No changes to gamma compute methodology, breadth fan-out, data sources, or
  MOVE/rates-vol addition.
- No change to row-band boundaries or to the panic tape triggers.
- No SPA layout redesign — wording/badge/detail changes only.

## Risks and trade-offs

- **Missed-detection risk** is the cost of every gate. Mitigations are
  structural: tape triggers (panic, co-sign) are never gated; fast-path
  depths make day-one eligibility possible for every gated indicator; and
  provisional reds still escalate to `early_warning` immediately. **Stated
  loudly:** a slow, deep, multi-cluster credit bleed (2007-style) that moves
  no tape will sit at stage `confirmed_stress` / severity `watch` until a
  tape co-sign day or threshold promotion — "act" arrives later than today.
  That is the deliberate price of the provenance gate, and the journal will
  measure whether it was ever paid in practice.
- **Tone follows governed severity by design:** deep fresh confirmed evidence
  keeps the "Confirmed stress regime" label, but renders amber/watch when the
  governor holds severity at watch. Red is reserved for act-grade stress, and
  the incident case cannot reach the confirmed label at all (its reds were
  provisional).
- **Degraded-cap scope:** capping on global readiness was rejected (one dead
  feed would mute everything); evidence-keyed capping can in principle miss a
  quality problem outside the confirming set — accepted, since non-confirming
  quality issues only inflate *toward* caution.
- **Fingerprint v2** re-fires active alerts once on upgrade — accepted,
  changelog-noted.
- **Rollback:** policy sits behind typed additive fields and a handful of
  shared functions; reverting the lifecycle builder and composite to raw-red
  tallies restores today's behavior without contract breakage. Journal and
  streak-store changes are delete-safe.

## Review log

- **2026-06-12 — adversarial review (subagent, full code verification):
  needs-rework.** Blockers: (1) canary surface recomputes confirmation from
  raw bands and would have reproduced the incident on the SPA/alert path —
  added as finding #6 and Part 5 canary/backtest consolidation; (2)
  freshness-class definitions readmitted the prior-evening gamma via the
  post-close-window clause — replaced with cadence-relative freshness
  (Part 3). Should-fixes adopted: kept the `SPY ≤ −4% && yellow ≥ 2` branch;
  reconciled the severity table with single-red+tape branches; stated
  tone-vs-severity explicitly; moved hysteresis application daemon-side and
  defined streak-under-hysteresis; added eligibility latch for depth-boundary
  churn; specified fingerprint policy + v2 bump; de-ticked monitor fields for
  SSE-hash stability; enumerated all four headline copies; defined gamma's
  three red paths. Cuts adopted: stage dwell, per-gate settings knobs (one
  journal knob remains), top-level policy block, journal rotation. Factual
  fixes: SPA regime stale badge is 60m (not 15) at `app.js:3159`; app polls
  regime at 1-minute cadence.
- **2026-06-12 — re-review (fresh subagent, citation spot-checks):
  ready-to-gate, one condition.** Both blockers verified closed; all
  should-fixes/cuts/nits landed; three-table cross-check passed on five
  scenarios. Condition applied: reconciled the urgent row with the panic
  triggers — heuristic-only `eligibleRed ≥ 3` panic now reads severity act
  (not urgent) without co-sign/promotion, making the severity ladder monotone
  and the provenance rationale truthful (pure-tape branches carry their
  co-sign by construction). Cosmetic: `BuildCanaryFingerprint` cite corrected
  to `internal/rpc/fingerprint.go:106`; `CompletedSessionKey` path corrected
  to `internal/breadth/spx/scheduler.go:118`; tone-precedence sentence added
  under the headline table.

## Open questions for the maintainer

1. HYG min depth 0.25% / fast path 1.0% — acceptable starting floors, or
   prefer a vol-scaled depth (e.g. 0.5 × 20d realized vol) at the cost of
   explainability?
2. Label wording at 2 eligible reds: "Confirmed stress regime" (proposed) vs
   keeping "Broad stress regime" and accepting 2-cluster breadth.
3. Should the decisions journal also capture `ibkr canary` lifecycle
   decisions in the same file or a sibling (`canary-decisions.jsonl`)?
