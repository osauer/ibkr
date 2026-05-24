# Regime v2 — Design Document

**Drafted:** 2026-05-20 09:45 CEST · **Status:** locked plan, no code yet

Source companion: [docs/research/regime-review-2026-05-20.md](../research/regime-review-2026-05-20.md).
Builds on contract-store persistence pattern from commit `2fbd614`. Plan reviewed by a senior
algo-trading / market-data architect; decisions below reflect the locked outcome.

---

## Locked plan (the only thing you need to read)

Three small sequential releases. Everything outside this table is either dropped or deferred.

**R1 — Methodology lift.** Skew-aware sweep (pure cutover, no legacy field) + near/term split
(hardcoded 7-DTE boundary) + per-indicator streak counter. Quadratic skew curve in
log-moneyness. Method token bumps to `perfiliev-bs-sweep-v2-stickymoneyness`. Decision criterion
for revert ("if sign-agreement vs SpotGamma's Friday post drops over 4 weeks, revert") goes in
CHANGELOG. See §1, §3, and the new §A below.

**R2 — Breadth expansion.** 200-day breadth + net new 52-week highs/lows in one shared
`WindowSet` v2 bump. 60/40 bands for the 200-day reading (calibrated to post-Mag-7 era). New
highs/lows surface as a sub-signal under the breadth row, not a 7th indicator. The breadth
`value` field migrates to `pct_above_50dma` cleanly (no one-release alias). See §7, §8.

**R3 — Validation infrastructure.** `ibkr regime --log <path>` appends JSONL. Path is required
(no default). No per-line `schema_version` — version lives in the filename
(`regime-v1.jsonl`). No `--replay` subcommand until we have JSONL data and a real opinion on
what shape to extract. See §4.

### Dropped entirely

- **DEX (delta exposure)** — minimal incremental signal net of GEX/VIX per FlashAlpha; you'd be
  adding caveats, not signal.
- **`[regime].indicators` config knob** — the existing `unavailable` status pattern already
  handles missing entitlements gracefully.
- **`--replay` subcommand** — premature; build it after JSONL accumulates and the right
  extraction shape is clear.
- **Three-row decorated sparkline** — single-row bar in default render, multi-line plot only in
  `--explain`. No grid markers or ★ in default mode.
- **Per-line `schema_version` in JSONL** — version goes in the filename.
- **Dual-emit during methodology cutover** — pure cutover; SpotGamma is the honest comparator,
  not our own old recipe.

### Deferred (not dropped)

- **MOVE index** — comes back as a release-4 candidate after R1+R2+R3 give a quarter of dashboard
  use. The argument: knowing what blind spot MOVE fills requires having stared at the existing
  five for a meaningful stretch.

### New (added during the senior review)

- **§A — Per-indicator streak counter.** Cheap, high information density, closes the wire-shape
  gap with the spec's repeated "sustained 2-3 days, not single spikes" language.

---

Below is the original design narrative, kept for the rationale on each kept feature. Sections
covering dropped or deferred features are annotated inline.

This document originally planned eight changes — five to lift dealer-gamma signal quality from
~6.5/10 to a defensible 8/10 without paying for data, plus three new indicators on the regime
envelope. The senior architect review cut DEX and deferred MOVE; the locked plan above is the
result.

---

## Scope and sequencing

Eight changes, ordered by recommended implementation sequence. Reasons explained in §10.

| # | Change | Methodology / UX | Effort | Risk |
|---|---|---|---|---|
| 1 | Skew-aware sweep (sticky-moneyness IV) | methodology | M | M |
| 2 | DEX (delta exposure) companion | methodology | S | L |
| 3 | Multi-horizon split (near vs term) | methodology | S | L |
| 4 | Snapshot logging (`--log` flag + replay) | UX + validation | S | L |
| 5 | Profile rendering (sparkline) | UX | S | L |
| 6 | MOVE index as 6th regime indicator | new indicator | S | L |
| 7 | 200-day breadth alongside 50-day | new indicator | M | L |
| 8 | Net new 52-week highs/lows | new indicator | M | L |

Effort: S=~1 PR, M=~2-3 PRs, L=major. Risk: L=easy to roll back, M=changes wire-shape, H=changes
behaviour widely.

---

## Caching context (2fbd614)

The contract store gives us three things we should consciously exploit:

1. **Free symbol resolution across restarts** for new IND tickers (MOVE). One resolution → entry
   in `contracts.json` → seeded on every subsequent boot. Add `MOVE` to the prune-survival list
   alongside `VIX`, `VIX3M`, `HYG`, `SPY`, `USD.JPY`, `DXY`, `NDX`.

2. **Per-domain persistence pattern.** The commit deliberately rejects merging the gamma /
   Greeks / PnL caches into the contract store ("different cadence and invalidation rules;
   cataloguing them under one persistence layer would over-engineer the shared piece"). New
   persistence (gamma snapshot log; breadth 200-day window state) follows the same pattern —
   own store, own version field, own load/save lifecycle.

3. **Atomic-write convention.** Temp+rename for any file the daemon writes. Append-only logs
   skip this (a partial trailing line is salvageable); state files must use it.

What 2fbd614 does **not** solve and what this design does **not** change:

- Gamma compute cache stays in-memory (`gammaZeroCache`). Per-day persistence would add
  complexity for negligible gain — the compute is cheap enough at 2-4 min that re-running
  on cold-start is fine.
- Skew curves (added in §1) live inside the gamma compute result envelope. No separate persistence.

---

## 1. Skew-aware sweep (sticky-moneyness IV)

### Why

The current sweep holds each leg's IV constant across all 60 scenario spots. That's the
canonical Perfiliev recipe and what every open-source clone does — but it's also documented
to bias the zero-gamma estimate. SPX is empirically closer to **sticky-strike** in calm
regimes (each strike keeps its own IV regardless of spot) and **sticky-delta** in stress (ATM
IV stays at ATM as spot moves). Sticky-IV is closer to sticky-strike but doesn't admit the
right skew shape at off-snapshot scenario spots.

### Design

Fit a per-expiry **moneyness skew curve** at snapshot time, then look up IV from the curve at
each (scenario spot, strike) pair during the sweep.

Moneyness convention: `m = ln(K / S)`. Negative m = OTM puts, positive m = OTM calls. The SPX
skew is reasonably described as a quadratic in m for a ±10 % strike window:

```
σ(m) = a + b·m + c·m²
```

where `a` is ATM IV, `b` is skew slope (typically negative — puts trade richer), `c` is curve
(usually small but positive). Fit via least squares over the legs that landed an IV in each
expiry's pool — minimum 3 points to fit, otherwise fall back to sticky-IV for that expiry.

During sweep at `scenario_spot`:
- For each leg's strike K, recompute moneyness: `m_scenario = ln(K / scenario_spot)`
- IV for this leg in this scenario: `σ_scenario = σ(m_scenario)` from the fitted curve
- Pass `σ_scenario` to `bsGamma(scenario_spot, K, dte, σ_scenario, 0, 0)`

Implementation touchpoints (no code, just where):

- New file: `internal/daemon/gamma_skew.go`
  - Type: `SkewCurve { A, B, C float64; nPoints int; ok bool }`
  - Method: `(*SkewCurve) IVAtMoneyness(m float64) float64`
  - Constructor: `fitSkewCurve(legs []legData) SkewCurve` — per-expiry, called once per expiry
    after fan-out, before sweep
- Modify: `sweepProfile` in [gamma_zero_compute.go:915](../../internal/daemon/gamma_zero_compute.go)
  - Take additional `skewByExpiry map[string]SkewCurve` parameter
  - For each leg in inner loop: `σ := skewByExpiry[l.expiryYMD].IVAtMoneyness(ln(l.strike / scenarioSpot))`
  - Fallback to `l.iv` if the curve's `ok=false`
- Modify: `computeGammaZeroSPX` — build `skewByExpiry` after legs collected, before sweep
- Wire: add `skew_model` string field to `rpc.GammaZeroComputed` (`"sticky-moneyness-v1"` or
  `"sticky-iv-fallback"` per-expiry). Add `skew_fit_quality` per expiry on the envelope (`R²`
  of the fit, count of fit points).
- Method token bumps: `perfiliev-bs-sweep-v2-stickymoneyness`.

### Edge cases

- **Too few IV legs per expiry.** Need ≥ 3 to fit a quadratic. Below 3 → fall back to sticky-IV
  for that expiry; surface in envelope.
- **All legs at single moneyness.** Degenerate fit. Same fallback.
- **Extrapolation beyond fit range.** A scenario spot in the sweep can push a leg's moneyness
  outside the range we fitted over. Clamp the curve evaluation to the fitted range to avoid
  wild extrapolation; surface a warning on the result envelope.
- **Sign-flip behaviour.** Sticky-moneyness should shift the zero-gamma estimate by 30-80 SPX
  points consistent with the spec's claim (which is exactly what this fix is for). For the
  calibration window, see decision below.

### Decision recommended

**Run both methodologies side-by-side for one release cycle.** The skew-aware result becomes
the headline; the sticky-IV result is emitted as `zero_gamma_legacy` for comparison. After
N weeks (the snapshot log accumulates the deltas), promote the new methodology as default and
remove the legacy field. This converts a methodology change from "trust me" to "show me" —
and gives us the H5 empirical-validation finding from the review for free.

---

## 2. DEX (Delta Exposure) companion — **DROPPED**

**Status:** dropped per senior review (2026-05-20). FlashAlpha's 8-year backtest finds modest
incremental signal net of GEX/VIX, and the dealer sign convention applies to delta the same as
gamma — adding DEX brings another caveat without adding signal. Reconsider only if we move to a
DDOI sign-refinement methodology, which requires paid data we don't have.

Original rationale retained below for reference.

### Why

The dealer's gamma flip tells you when hedging stops mean-reverting and starts amplifying.
Dealer delta exposure tells you **which direction** they're leaning. They answer different
questions. Currently we have GEX (signed and magnitude); we don't have DEX at all. The
research found DEX adds modest predictive power net of GEX/VIX but is invaluable as a
diagnostic ("dealers are net short delta = they need to buy dips" vs "net long = they sell
rips").

### Design

Same chain pull, same fan-out. We already capture per-leg `Greeks.Delta` from the gateway
(see `productionLegFetcher`); we just don't aggregate it.

Formula: `DEX = Σ sign(right) × Δ × OI × multiplier × spot × 0.01`

Sign convention matches GEX (calls-long dealers, puts-short dealers — same caveats apply).
Compute at snapshot spot only — sweeping DEX has different semantics (delta-of-spot changes
fast and the curve is dominated by short-dated 0DTE; less interpretable as a "regime profile").

Implementation touchpoints:

- Extend `legData` struct ([gamma_zero_compute.go:135](../../internal/daemon/gamma_zero_compute.go)):
  add `deltaAtSnapshot float64`
- Modify `productionLegFetcher` to capture `g.Delta` alongside `g.Gamma` from the Greeks tick
- Extend `legResult` similarly; populate via existing `GetOptionGreeks` lookup
- New function: `aggregateDEX(legs, spot) (totalSigned, totalAbs float64)` next to existing
  `absGEX` in [blackscholes.go](../../internal/daemon/blackscholes.go)
- Modify `computeGammaZeroSPX` to call `aggregateDEX` after legs collected
- Wire: add `dex_total`, `dex_total_abs` to `rpc.GammaZeroComputed`; add `top_strikes_dex`
  (analogous to `top_strikes` but ranked by `|Δ|·OI`)
- CLI: render alongside gamma in `--explain` mode

### Edge cases

- **BS-IV fallback legs.** If a leg's IV came from Newton-Raphson against an option
  quote mid or prior-session close, its delta is similarly derived (not gateway-pushed).
  Compute `bsDelta` from the back-solved IV — straightforward addition to `bsIVFallback`.
- **Sign convention inversion.** Same as GEX. The `top_strikes_dex` magnitude view is robust;
  the signed `dex_total` carries the same dealer-prior caveats.

### Decision recommended

**Default-on, no flag.** DEX is cheap incremental work on data we already collect. No reason
to gate it.

---

## 3. Multi-horizon split (near vs term)

### Why

0DTE was ~59 % of SPX option volume in 2025 (Cboe, Aug 2025). Near-dated and longer-dated
gamma behave very differently — near-dated is larger in magnitude, decays faster, and reacts
more violently to flow. Aggregating into one number hides this. When near and term disagree,
the disagreement is the signal.

### Design

Current implementation note (2026-05-24 09:02 CEST): this design began
as a two-bucket near/term sketch. The shipped wire contract uses three
explicit buckets: 0DTE, 1-7 DTE, and term.

After the existing leg fan-out collects all 6 expirations' worth of legs:

- Partition by DTE:
  - `zeroDTELegs` = legs with `DTE == 0`
  - `oneToSevenLegs` = legs with `0 < DTE ≤ 7`
  - `termLegs` = legs with `dte > 7/365`
- Run `sweepProfile` four times:
  - Combined (existing): all legs
  - 0DTE: zeroDTELegs only
  - 1-7: oneToSevenLegs only
  - Term: termLegs only
- Run `findZeroCrossing` on each profile

Boundary at 7 days picked deliberately — captures 0DTE through end-of-week, separates from
monthly OPEX dynamics. Could be parameterised via `params.NearDTEDays` (default 7) but I'd
keep it hardcoded for v1.

Implementation touchpoints:

- New function: `partitionLegs(legs []legData, nearCutoffYears float64) (near, term []legData)`
- Modify `computeGammaZeroSPX`: after legs collected, partition and run sweep×3 + zero-crossing×3
- Wire on `rpc.GammaZeroComputed`:
  - Existing: `zero_gamma`, `profile`, `gamma_sign`
  - New: `zero_gamma_0dte`, `profile_0dte`, `gamma_sign_0dte`, `leg_count_0dte`
  - New: `zero_gamma_1to7`, `profile_1to7`, `gamma_sign_1to7`, `leg_count_1to7`
  - New: `zero_gamma_term`, `profile_term`, `gamma_sign_term`, `leg_count_term`
- Regime row: surface horizon agreement for single-underlying gamma envelopes; combined
  SPY+SPX leaves horizon buckets under each `per_index` result.

### Edge cases

- **All expirations are 0-7 DTE.** Mid-week morning before any longer-dated expiration in the
  6 nearest. `leg_count_term == 0`. Surface `zero_gamma_term: null` plus scoped
  `warning_details`.
- **All expirations are > 7 DTE.** Rare but possible Friday afternoon after the weekly expires.
  `leg_count_0dte == 0` and `leg_count_1to7 == 0`. Symmetric handling.
- **Threshold edge.** A 7-day-and-1-hour expiration falls in term; a 6-day-and-23-hour falls in
  near. The two estimates change very little from each other at the boundary so the
  classification jitter doesn't matter.

### Decision recommended

**Default-on. Surface in CLI default render** alongside the headline number when near vs term
disagree (small "(near 583, term 587)" annotation). This is the highest-information delta a
user reads day-to-day.

---

## 4. Snapshot logging (`--log` flag + replay)

### Why

The spec mandates a 4-week SpotGamma cross-check ritual to calibrate the dealer-sign and
sticky-IV assumptions. There's no CLI surface for it. Adding one closes review-finding H5
("bias claims uncited") for ~50 lines of code and gives us a real dataset to validate the §1
change against.

### Design

Pure CLI feature — daemon doesn't need to know.

```
ibkr regime --log /path/to/regime.jsonl
```

Appends today's full snapshot (the same JSON envelope `--json` produces) as one line to the
file. Each line:

```jsonl
{"timestamp":"2026-05-20T16:35:00Z","regime":{...},"external_refs":null}
```

`external_refs` is a nullable object the trader fills in manually later (post-Friday-SpotGamma
post) with `{"spotgamma_zero_gamma": 5891, "spotgamma_vol_trigger": 5910, "note": "..."}`.
No automated scraping; manual annotation by the user.

Companion verb: `ibkr regime --replay /path/to/regime.jsonl --since 2026-04-01` reads the
file and prints a CSV with `ts, zero_gamma, spy_spot, spotgamma_zero_gamma, delta, sign_agreement`
columns — the calibration report.

Storage location: trader-supplied path; default suggestion `$XDG_DATA_HOME/ibkr/regime.jsonl`
or `~/.local/share/ibkr/regime.jsonl` if no path. Note: `$XDG_DATA_HOME` not `$XDG_CACHE_HOME`
(2fbd614 uses `$XDG_CACHE_HOME` for caches; logs are data, not caches — they shouldn't be
wiped on a `~/.cache` clear).

Implementation touchpoints:

- New CLI flag handler in [internal/cli/regime.go](../../internal/cli/regime.go): after the
  fetch, if `--log` is set, open file with `O_APPEND|O_CREATE`, write one JSON line + newline
- New CLI verb: `ibkr regime --replay` — reads JSONL, walks lines, emits CSV. Could be a
  subcommand `ibkr regime replay ...` if `--replay` doesn't fit the flag taxonomy
- No daemon changes; the CLI already has the envelope from the standard fetch

### Edge cases

- **Concurrent writers.** Two `ibkr regime --log` invocations could interleave lines (POSIX
  guarantees atomic writes only ≤ PIPE_BUF, typically 4 KB; a regime envelope is small enough
  in practice but not guaranteed). Acceptable for a daily-cadence log; document in the help
  text.
- **Schema evolution.** Add a `schema_version` field to each line for future-proofing. Replay
  filters or migrates by version.
- **File doesn't exist.** Create on first write.

### Decision recommended

**Ship the `--log` flag in v0.x.y. Defer daemon-scheduled logging.** If the manual ritual proves
valuable, adding a daemon scheduler (cron-like) is a follow-up. Most users won't use either; a
small number will use the flag from their own cron.

---

## 5. Profile rendering (CLI sparkline)

### Why

We compute the full 60-point sweep profile but only surface a single scalar (`zero_gamma`).
The full curve is dramatically more interpretable — you see *where* the flip is steep vs flat,
where the magnitude concentrates, and whether the sweep brackets the spot cleanly. Every paid
vendor's dashboard renders the profile. Cost: rendering only, data already exists.

### Design

CLI output in default mode, compact:

```
SPY γ-zero level:  583.20    (SPY 591.42, +1.40% above)

  ▁▂▃▅▆▇▇▆▅▃▁ · ▁▁▁▁▁ · ╷╷╷╷╷ · ▔▔▔▔▔ · ▇▆▅▃▁
  574        ┊   583┊        ┊ 591 ★ ┊        609
              ↑ γ-zero      ↑ spot
```

Three-row sparkline using Unicode block characters scaled to the profile's `|GEX|` peak. Crossing
marked between the two grid lines. Spot marked with ★.

For `--explain` mode: render an ASCII line plot with `+`/`-` markers for sign, e.g.

```
  Sweep profile (signed dealer GEX vs spot, $bn per 1%):

    574  +++++++++++  6.2
    580   +++++++++   4.8
    583  --━━zero━━--  0.0  (interpolated, gap +1.4% from spot)
    591   --------    -3.1  ← SPY here
    600    -------    -4.2
    609     ------    -3.8
```

For `--json --profiles`: emit the `profile` array plus `profile_0dte`, `profile_1to7`,
and `profile_term` from §3. Default JSON strips these arrays so agent/tooling output stays compact.

Implementation touchpoints:

- New file: `internal/cli/gamma_sparkline.go` with `renderSparkline(profile, spot, zg) string`
- Modify [internal/cli/gamma.go](../../internal/cli/gamma.go) and
  [internal/cli/regime.go](../../internal/cli/regime.go) to render the sparkline in default
  and --explain modes (default: one-line bar; explain: multi-line plot)

### Edge cases

- **Non-TTY output / `--json`.** Skip sparkline rendering, just emit numbers + JSON profile
  array.
- **All-positive or all-negative profile.** No zero crossing. Render the curve, annotate with
  "no crossing in sweep window — dealer book is long-gamma across full ±15% range" (the existing
  `gamma_sign: "positive"` case).
- **Terminal width.** Render at fixed 30-char width; degrade gracefully on narrow terminals.

### Decision recommended

**Default-on for `ibkr gamma`; in `--explain` only for `ibkr regime`** (regime is dense
already; gamma is the place to show the curve).

---

## 6. MOVE Index as 6th regime indicator — **DEFERRED**

**Status:** deferred per senior review (2026-05-20) until R1+R2+R3 have shipped and a quarter
of dashboard use has surfaced a specific blind spot MOVE would fill. The April-2025 basis-trade
anecdote and the SOA Sep-2025 paper are interesting but don't constitute "I needed this on day
X." Original rationale retained below.

### Why

MOVE (ICE BofA Merrill Lynch Option Volatility Estimate — Treasury implied volatility) led
VIX in the April-2025 hedge-fund basis-trade stress (Schwab, 2025). The bond market has
historical precedent of pricing macro/systemic risk earlier than equity. SOA's Sep-2025
asset-allocation paper formalises MOVE-alongside-VIX as a regime input. We already do
VIX/VIX3M; adding MOVE makes our cross-asset coverage materially better.

### Design

Same shape as the VIX/VIX3M row but single-leg.

- Symbol: `MOVE` (IND, CBOE). Verify via `reqContractDetails` at first use.
- Thresholds (initial; calibration over time):
  - Green: MOVE < 100 (calm rate-vol)
  - Yellow: 100-130 (elevated)
  - Red: > 130 (stress)
- Day-over-day change anchor on the row, same as VIX

Wire:

- New type: `rpc.RegimeMOVE` mirroring `RegimeVIXTerm`'s shape (Move pointer, MovePrevClose,
  MoveChangePct, MoveQuality, DataType, Status, Notes)
- Notes const: `moveNotes` with thresholds verbatim
- New fetcher: `fetchRegimeMOVE` next to `fetchRegimeVIXTerm`
- Fan-out: extend `runRegimeFanout` from 5-way to 6-way (channel cap to 6, 6 received slots,
  add to the deadline-fired fill section)
- MCP tool description (mcp/tools.go): mention MOVE alongside VIX/VIX3M
- CLI row in default render
- Symbol resolution: piggybacks on contract-store from `2fbd614` — add `MOVE` to the
  always-survive-prune list in `internal/daemon/server.go` (wherever the SPX-non-member
  preserve list lives)

Implementation touchpoints:

- New rpc type in [internal/rpc/rpc.go](../../internal/rpc/rpc.go)
- New fetcher in [internal/daemon/regime.go](../../internal/daemon/regime.go)
- Extend `runRegimeFanout` to 6-way (rename the channel cap from 5 to 6; the fan-out is
  structurally extensible already)
- New CLI row + notes
- Bump `MethodRegime` snapshot wire shape (`RegimeSnapshotResult.MOVEIndex` field added)
- Add `MOVE` to contract-store prune-survival list

### Edge cases

- **MOVE not entitled.** Some accounts lack the bond-vol index entitlement. Row surfaces
  `status: "unavailable"` with a notes-pointer explaining the gateway entitlement.
- **MOVE delayed.** Bond vol updates more slowly than equity vol off-hours. Use the same
  frozen/live treatment as VIX3M.

### Decision recommended

**Add behind a `regime.indicators` config knob.** Default: enabled. Lets users with no bond-vol
entitlement disable it without seeing perpetual error rows. The `[regime]` config section is a
new addition — small, follows the existing `[scans.*]` pattern.

---

## 7. 200-day breadth alongside 50-day

### Why

The 50-day reading is good for tactical (weeks-to-month) signals. The 200-day catches
cyclical tops cleanly (1999, 2021). Same data path. Adding it is mostly accounting — we
already pull the closes; the SMA window is just a `len(bars)-200` slice instead of `-50`.

### Design

Extend the breadth engine to compute both windows in one pass.

- The breadth engine in [internal/breadth/](../../internal/breadth/) currently maintains a
  rolling window per constituent for 50-day SMA. The persisted state file
  (`internal/breadth/spx/types.go` `WindowSet`) is at version 1.
- Extend `WindowSet` to track both 50-day and 200-day windows. Bump file format version (v2).
  Old v1 files cold-restart (per the contract-store pattern — unknown version triggers fresh
  rebuild).
- Cold-start cost: unchanged. We pull more bars per request (200 days vs 50) but IBKR's pacing
  is per-request, not per-bar. Same ~60 minutes of cold start, same 60-req/10-min cap.
- Steady-state: incremental (one bar/day per constituent), unchanged cost.

Wire:

- Current `BreadthSPXResult.Value` field → keep as alias for `PctAbove50DMA` for one release
  cycle for backward compat
- New: `pct_above_50dma`, `pct_above_200dma` on the envelope
- Bands: keep the existing 50-day bands (revised per review H3); add 200-day bands of 70/30
  (StockCharts default) or — better — 60/40 calibrated to the post-Mag-7 baseline
- Regime row: `RegimeBreadth.PctAbove200DMA` field added; notes string explains the
  fast/slow pair

Implementation touchpoints:

- Modify `internal/breadth/spx/types.go`: bump `WindowSet` version, add 200-day window
- Modify the engine refresh logic: compute both SMAs in one constituent pass
- Modify `RegimeBreadth` rpc type + `fetchRegimeBreadth`
- CLI row in regime + standalone breadth
- MCP tool description for `ibkr_breadth`

### Edge cases

- **Cold-start cache file at v1.** On load, version mismatch → rebuild from scratch. The
  rebuild needs 200 days of history instead of 50; this is the first time the engine pays the
  longer-window cost. Add a one-time log line on cold rebuild explaining.
- **A constituent without 200 days of history.** Recently-IPO'd or recently-added names won't
  have 200 closes. Skip from the 200-day numerator until they do (consistent with the 50-day
  treatment for short-history constituents).
- **Backward compat.** `value` field stays for one release for old consumers. Deprecation
  comment on the rpc field. Remove in the next minor.

### Decision recommended

**Use 60/40 bands for 200-day** (calibrated to the post-Mag-7 era — see review §H3) rather
than the StockCharts 70/30 default. The lower thresholds are honest about the concentration
regime. Surface the bands in `notes` and the spec doc.

---

## 8. Net new 52-week highs/lows

### Why

Caught the September-2025 narrow-rally divergence cleanly (only 4.6 % of names at 52w highs
with SPX at all-time highs — textbook narrow breadth). Same data path as breadth — we already
have daily closes for all 500 constituents. The metric is free.

### Design

Per constituent, track a 252-bar rolling max(close) and min(close) (252 trading days ≈ 1 year).

Computation per refresh:

- For each constituent: `made_new_high_today = today_close > max(close for previous 251 sessions)`
- Same for `made_new_low_today`
- Aggregate counts across all 500 constituents
- Output: `new_highs_today`, `new_lows_today`, `net_new_highs_pct = (new_highs - new_lows) / total × 100`

Storage: extend `WindowSet` again to carry the 252-bar max/min per constituent. This is the
same v2 bump as §7 — both should land together.

Wire:

- Extend `BreadthSPXResult`:
  - `new_highs_today: int`
  - `new_lows_today: int`
  - `net_new_highs_pct: float`
- Extend `RegimeBreadth` similarly
- Thresholds for the regime row (a separate sub-signal under the breadth indicator):
  - **Narrow rally warning (yellow):** SPX within 3% of 52w high AND new_highs_pct < 5%
  - **Confirmed divergence (red):** Above + (new_lows > new_highs for 5 consecutive sessions)
- Notes string explains the canonical "few names at highs with index at highs" pattern

### Edge cases

- **Constituents with < 252 days of history.** Skip from numerator and denominator (apply the
  same "needs enough history" treatment as the SMAs). Surface the effective coverage in the
  envelope so a renderer can flag low-coverage days.
- **The percentage is small.** A typical day might have 5-15 new highs and 2-5 new lows
  across 500 names. Render the raw counts alongside the percent.

### Decision recommended

**Compute the metric in the breadth engine; surface as sub-fields under the breadth row in
regime**, not as a 7th indicator. Keeps the 6-indicator total (after MOVE) clean. The
sub-fields document the "narrow rally" signal in the breadth notes.

---

## A. Per-indicator streak counter (new in R1)

### Why

The spec repeats "sustained 2-3 days/weeks, not single spikes" across VIX, HYG, and breadth —
but every indicator on the wire publishes today's snapshot only. An LLM consumer has no way to
tell whether today's flip is day 1 of a stress event or day 5. The streak counter closes that
gap with minimal cost.

### Design

Per indicator, persist a small "consecutive sessions in band" tally. On each refresh:

- Compute today's band from the indicator's current value (using each indicator's own threshold
  bands — VIX/VIX3M green/yellow/red, HYG/SPY green/yellow/red, etc.)
- If today's band matches yesterday's: increment the counter
- Otherwise: reset to 1 (today is the first session in the new band)

Wire shape: add `streak: {band: "yellow", sessions: 3}` (or similar) to each indicator row on
the regime envelope. The renderer can show "VIX/VIX3M 0.95 (yellow · day 3)" inline.

Storage: extend whatever per-indicator state already exists. The breadth engine persists its
own state; VIX/HYG/USD-JPY currently don't have any. Adding minimal per-indicator state to the
daemon — keyed on indicator name, value: `{band, since_date, sessions}` — is the lightest path.
Persist via the same JSON-on-disk pattern as `2fbd614`, in
`$XDG_CACHE_HOME/ibkr/regime-streaks.json`.

### Edge cases

- **Bootstrap.** No prior state on first call → start every counter at 1 with today's band.
- **Indicator unavailable / computing.** No band → don't increment, don't reset; counter freezes
  until the indicator returns to a known band.
- **Threshold edge wobble.** A value that oscillates around the threshold (e.g. VIX/VIX3M
  bouncing between 0.91 and 0.92) would reset the counter every day. Acceptable for v1; if it
  proves noisy in practice, add hysteresis later.

### Decision

Locked. Include in R1.

---

## 9. Wire-format and version-token plan

This pass touches the wire in several places. A single coordinated bump is cleaner than
piecemeal additions:

| Surface | Current | After this pass |
|---|---|---|
| `MethodRegime` envelope | 5 sub-indicators | 6 (adds MOVE), nested fields per §7-§8 |
| `MethodGammaZeroSPX` envelope | single zero_gamma | + near/term + DEX + skew_model + legacy field |
| `MethodBreadthSPX` envelope | value (50d only) | + 200d + new-highs/lows |
| Breadth on-disk state | `WindowSet` v1 | v2 (200d + 252-bar max/min) |
| Gamma method token | `perfiliev-bs-sweep-v1` | `perfiliev-bs-sweep-v2-stickymoneyness` |

All field additions are backward-additive (new optional fields, never removing or renaming).
The `value` → `pct_above_50dma` migration on breadth is the one case where an old field gets
aliased for one release cycle then removed. Renderers reading both `value` and
`pct_above_50dma` during the overlap window need to prefer the new name.

The method-token bump on gamma signals to consumers that the dealer-gamma number is now
sticky-moneyness-derived. The previous-recipe value remains available as `zero_gamma_legacy`
during the calibration window (§1).

---

## 10. Recommended sequencing

The eight changes split into three phases:

**Phase 1 — Foundations and quick wins (1-2 weeks):**
- §4 Snapshot logging — gives us empirical data starting day 1
- §5 Profile rendering — pure UX, no methodology risk, immediate user value
- §2 DEX companion — small, safe, completes the dealer-positioning view
- §3 Multi-horizon split — small, safe, adds the 0DTE-aware view

**Phase 2 — Methodology lift (2-3 weeks):**
- §1 Skew-aware sweep — biggest methodology change; needs the snapshot log from §4 to
  validate
- Run §1 dual-emit (legacy + new) for 4 weeks; gather calibration data via §4

**Phase 3 — Coverage expansion (2-3 weeks, can run in parallel with Phase 2):**
- §6 MOVE indicator
- §7 200-day breadth + §8 new highs/lows (one PR — shared `WindowSet` v2 bump)
- Update [docs/specs/risk-regime-dashboard.md](risk-regime-dashboard.md) to describe the
  expanded 6-indicator panel + new breadth sub-fields

Phase 1 alone takes dealer gamma from ~6.5/10 to ~7.5/10. Phase 1 + Phase 2 takes it to a
solid 8/10. Phase 3 expands coverage without touching gamma further.

---

## 11. Open decisions for review

These are the choices I made in the design above that I'd flag for explicit user sign-off
before coding:

| § | Decision | My choice | Alternative |
|---|---|---|---|
| 1 | Dual-emit during calibration vs straight cutover | Dual-emit for one release | Straight cutover with `--methodology=legacy` flag |
| 1 | Skew curve shape | Quadratic in log-moneyness | Piecewise-linear interp; spline |
| 3 | Near/term boundary | 7 DTE | Parameterise via `params.NearDTEDays` |
| 4 | Default log path | `$XDG_DATA_HOME/ibkr/regime.jsonl` | `--log` mandatory, no default |
| 6 | Disable knob for MOVE | `[regime].indicators` config section | Hardcoded always-on |
| 7 | 200-day bands | 60/40 (post-Mag-7 calibrated) | 70/30 (StockCharts default) |
| 8 | New-highs as sub-signal under breadth row vs 7th indicator | Sub-signal | 7th indicator |
| Wire | Breadth `value` → `pct_above_50dma` migration cycle | One release alias, then remove | Drop immediately; major bump |
| Process | Whether to run Phase 1+2+3 sequentially or 2+3 in parallel | 2+3 parallel (different files) | Strict sequential |

None of these is structurally hard to change after the fact — they're calibration choices, not
architectural ones.
