# Portfolio review with Claude and IBKR

Updated: 2026-05-29 12:28 CEST

`ibkr mcp` lets Claude review an Interactive Brokers portfolio from the same local IB Gateway or TWS session a trader already uses. The point is not to turn a broker account into a chatbot. The point is to give an assistant enough structured account, position, option, quote, calendar, scanner, sizing, and regime context to do the kind of review a disciplined trader would otherwise assemble by hand.

This page is for searches such as "portfolio review with Claude and IBKR", "Claude IBKR portfolio review", "Interactive Brokers portfolio review AI", and "IBKR portfolio analysis prompt".

## Who this is for

Use this workflow when you already trade through IBKR and want a second screen that can read the portfolio, organize the evidence, and make the next questions explicit.

It fits traders who care about:

- exposure by underlying, not just a flat position list
- options delta, theta, gamma, vega, and expiry concentration
- stale or delayed market data before acting on a quote
- watchlist names that suddenly matter because they are now near levels or have unusual volume
- portfolio risk in the context of volatility, breadth, credit, dealer gamma, and funding conditions
- sizing a trade plan before any separate execution step

Today, the public bundled MCP tools are analysis and sizing tools. They do not place, modify, or cancel orders. That boundary keeps the review workflow clean: first collect the evidence, then decide what deserves attention, then handle execution through whatever approved trading path you trust.

## What Claude can inspect

A good portfolio review needs more than balances and positions. Through `ibkr mcp`, Claude can call:

- `ibkr_account` for net liquidation, buying power, cash, margin, and daily P&L
- `ibkr_positions` for holdings, option legs, Greeks, and per-underlying grouping
- `ibkr_quote` and `ibkr://quote/{symbol}` for snapshot and streaming stock or ETF quotes
- `ibkr_calendar` for official session, holiday, and early-close context
- `ibkr_chain` for option expiries, IV, expected move, strikes, deltas, and open interest
- `ibkr_watch` and `ibkr_scan` for watchlist and market-discovery context
- `ibkr_breadth`, `ibkr_gamma`, and `ibkr_regime` for market background
- `ibkr_canary` for scheduled `Go` / `Watch` / `De-lever` / `Liquidate` stress checks
- `ibkr_size` for fixed-fractional position sizing math

The assistant should not treat any single tool result as a trade signal. The value is in combining the evidence, naming uncertainty, and showing which follow-up checks would change the decision.

## Use this prompt

Start with the maintained prompt in [examples/ibkr_portfolio_analysis_prompt.md](https://github.com/osauer/ibkr/blob/main/examples/ibkr_portfolio_analysis_prompt.md). It is written for semi-professional retail traders who use IBKR as capital-markets infrastructure, not for a generic personal-finance summary.

Short version to paste into Claude:

```text
Review my IBKR portfolio using the ibkr MCP tools. Start with account state,
positions grouped by underlying, options Greeks, quote freshness, and the current
risk-regime dashboard. Rank the risks I should inspect today, explain the evidence
behind each one, and separate observations from possible next actions. Do not
place or imply orders.
```

For a deeper review, use the full example prompt and let Claude decide which MCP calls are needed before it writes the answer.

For a scheduled canary check that returns only `Go`, `Watch`, `De-lever`, or `Liquidate` action rows, use [examples/ibkr_portfolio_canary_prompt.md](https://github.com/osauer/ibkr/blob/main/examples/ibkr_portfolio_canary_prompt.md).

## What a useful answer looks like

A strong review is compact, evidence-led, and explicit about missing data. It should read like a desk note, not a stream of tool output.

Sanitized sample:

```text
Executive snapshot
- Account state is usable for review. Buying power and margin are available; no order action was taken.
- The portfolio is concentrated in three underlyings. One options position drives most of the effective delta.
- Two quotes are delayed or outside the regular session, so marks should be treated as review inputs, not live execution prices.

Risk dashboard
- Market regime: risk-on but narrowing breadth. Treat long exposure as acceptable only if single-name concentration is intentional.
- Dealer gamma: near neutral. Do not assume intraday mean reversion from this row alone.
- Options: one expiry dominates near-term theta. Check whether that is intended before adding exposure.

Top findings
1. Underlying A has the largest effective delta after options. The stock line understates the true exposure.
2. Underlying B has stale quote context. Recheck during the next open session before sizing anything.
3. Underlying C has high implied move versus recent realized range. Review whether the option premium is still paying for the risk.

Next review steps
- Pull fresh quotes for the three largest underlyings during the regular session.
- Inspect the option chain around the dominant expiry before changing the position.
- Run sizing math only after the stop and invalidation level are stated in price terms.
```

This is intentionally not a prediction engine. It is a structured review loop: current account state, current market context, current uncertainties, and the next checks that would make a decision better.

## How to install

Claude Desktop users can install the MCP Bundle:

```text
https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

Shell, Cursor, Continue, Zed, and generic MCP users can install the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr status
```

Generic MCP configuration:

```json
{
  "mcpServers": {
    "ibkr": {
      "command": "/ABSOLUTE/PATH/TO/ibkr",
      "args": ["mcp"]
    }
  }
}
```

Requirements: IB Gateway 10.37+ or TWS running locally, plus an IBKR Pro account with TWS API access.

## Related pages

- [Analyze an Interactive Brokers portfolio with AI](../analyze-interactive-brokers-portfolio-with-ai/) for the broader AI portfolio-analysis search intent.
- [IBKR MCP server](../ibkr-mcp/) for the short setup path.
- [Connect Claude to IBKR](../connect-claude-to-ibkr/) for Claude Desktop and Claude Code setup.
- [Agentic use guide](../guides/agentic-use.md) for natural-language workflows.
- [MCP tools reference](../reference/mcp-tools.md) for exact tool schemas.
