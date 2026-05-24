# Agentic use

Updated: 2026-05-24 14:08 CEST

ibkr ships as an MCP server (`ibkr mcp`), making every read-only operation in the CLI available to any MCP client — Claude Code, claude-desktop, or any other host that speaks the protocol. The same daemon serves both surfaces; the MCP layer is a thin adapter over the existing RPCs. Stock and ETF quotes are also exposed as an MCP resource: `resources/read` returns one snapshot, while `resources/subscribe` streams updates until unsubscribe.

This page is for the human installing the plugin and wondering *"what can I actually ask Claude with this?"* For the per-tool surface — exact parameter shapes, JSON envelopes — see the auto-generated [MCP tools reference](../reference/mcp-tools.md). For protocol mechanics, see the upstream [Model Context Protocol spec](https://modelcontextprotocol.io/).

## Setup

The plugin manifest (`.claude-plugin/plugin.json`) is registered when you install via the Claude Code marketplace. Confirm it's wired:

```sh
ibkr status                   # daemon health, gateway connection, data freshness
```

The 12 MCP tools are listed in [reference/mcp-tools.md](../reference/mcp-tools.md). They mirror the agent-appropriate CLI commands — `ibkr_status` ↔ `ibkr status`, `ibkr_gamma` ↔ `ibkr gamma`, etc. — while local lifecycle verbs such as `setup`, `update`, `mcp`, and `daemon` stay outside the tool surface. Claude calls the tools as MCP operations rather than CLI subcommands.

## Example conversations

These are the kinds of questions the tool is built for. Each shows the user's message, the tool(s) Claude is likely to invoke (the description-driven match), and what the human can expect back.

### "Is the market regime favorable right now?"

→ Claude invokes `ibkr_regime`.

Returns the eight-row dashboard: VIX term structure, VVIX, HYG/SPY divergence, HY/IG OAS, funding spread, USD/JPY weekly move, dealer zero-gamma, and S&P breadth. Each row carries raw measurements, compact band/as-of metadata, scoped warnings when data is stale or unavailable, and a `streak` field when the row is rankable.

Claude composes an answer that names which indicators are in which band, calls out any in red, and flags streaks (a Day-5 stress event reads differently from a Day-1 spike). The dashboard is *information*, not a verdict — the user's risk tolerance determines what to do with it. See [Concepts → Regime](../concepts.md#regime).

### "Show me my SPY positions and any options on them."

→ Claude invokes `ibkr_positions` with `{"symbol": "SPY"}`.

Returns rows for SPY stock holdings and any SPY options, with per-leg Greeks (delta/gamma/theta/vega) for the options, plus a `portfolio` block aggregating effective_delta in share-equivalents. Claude typically renders the stock holding alongside an aggregate Greek line ("you're net long ~1,500 SPY-deltas after the options"). Daily P&L is included from IBKR's reqPnLSingle stream — `null` when the daemon hasn't pre-warmed that contract, never zero-substituted.

If you also want context, follow-up questions naturally chain: *"and what's SPY's dealer gamma profile?"* invokes `ibkr_gamma`; *"how does that compare to where SPY closed yesterday?"* invokes `ibkr_history` + `ibkr_quote`.

### "Are SPY dealers supporting or amplifying today's moves?"

→ Claude invokes `ibkr_gamma` (default scope = combined SPY+SPX).

Returns the signed zero-gamma price level, the dealer book's current sign (`positive` = long-gamma = stabilising; `negative` = short-gamma = amplifying), the regime-agreement classifier between SPY and SPX (`agree:long-gamma` / `agree:short-gamma` / `agree:transition-gamma` / `disagree`), and the magnitude view via `gamma_total_abs` and `top_strikes`.

The actionable signal is **`disagree`** — one book stabilising while the other amplifies, indicating institutional/retail positioning divergence. Claude usually flags this prominently. The first call of an NY trading day kicks a multi-minute background compute; you'll see `status: "computing"` with an ETA — re-ask in a few minutes for the result. See [Concepts → Gamma](../concepts.md#gamma).

### "Find me top S&P 500 names trading above their 50-day moving average."

→ Claude invokes `ibkr_breadth` to get the index-wide reading, then `ibkr_scan` with the `top-movers` preset (or an ad-hoc scan) to surface specific names.

Returns the % of S&P names above their 50-DMA (the tactical signal) and per-row scanner output enriched with last / prev_close / change_pct / volume / IV. Claude typically pairs the breadth context ("market-wide reading: 54% above 50-DMA, healthy") with the specific names that match the scan. For follow-up questions like *"show me daily bars for AAPL"*, Claude chains to `ibkr_history`. See [Concepts → Breadth](../concepts.md#breadth).

## What Claude can't do here

The MCP surface is intentionally **read-only**. There is no trade-execution tool. Claude can:

- ✅ tell you what you own
- ✅ tell you the market state
- ✅ size a trade (`ibkr_size` — pure math against your NLV, never proposes an order)
- ❌ place an order
- ❌ cancel an order
- ❌ modify a position

If you ask Claude to "buy 100 shares of AAPL," it will tell you it can't — and won't try to. This is a hard architectural boundary: the daemon never exposes write paths to IBKR, regardless of what Claude asks.

Streaming quote resources are separate from tools. MCP clients discover the `ibkr://quote/{symbol}` template via `resources/templates/list`; `resources/read` gives one quote snapshot, and `resources/subscribe` emits coalesced tick frames through `notifications/resources/updated` until the client unsubscribes or closes the MCP session. This streaming surface is stock/ETF only; option streaming is not exposed.

Other things outside the scope today:

- **Option streaming** (continuous option contract ticks). Option snapshots are available through chains and option quotes, but the MCP streaming resource is stock/ETF only.
- **Non-equity asset classes** (futures, FX spot, crypto). Equity, ETF, and equity-options surfaces are covered; everything else is out of scope today.
- **Other indices' breadth or constituents** (NDX, RUT, sector-specific). S&P 500 only.

## Tips for getting good answers

A few prompt patterns that work well, learned from observing real conversations:

- **Ask the question, don't name the tool.** "How does my portfolio look?" works better than "Run ibkr_positions." Claude picks the right tool based on the question; naming the tool just adds friction.
- **Chain follow-ups freely.** Each tool call is cheap (cached when possible). "And what about gamma for those?" or "How did that look yesterday?" generate natural follow-up tool calls.
- **For the dashboard, ask "how does the market regime look?"** — it triggers `ibkr_regime`, which returns the eight-row snapshot in one call. Faster than asking about each indicator separately.
- **For sizing, give Claude the full plan.** "I want to enter AAPL at 180 with a stop at 175 and a target at 195, risking 1% of NLV" lets `ibkr_size` return the R-multiple, breakeven win rate, and share count in one round-trip.

## Reference

- [MCP tools reference](../reference/mcp-tools.md) — auto-generated table of every tool, parameters, descriptions.
- [MCP resources reference](../reference/mcp-resources.md) — streaming stock/ETF quote resource semantics.
- [Concepts](../concepts.md) — the mental model for regime / gamma / breadth.
- [Updating](./updating.md) — keeping the binary + constituent list current.
- [Model Context Protocol spec](https://modelcontextprotocol.io/) — the upstream protocol.
