# Marketplace readiness

Last reviewed: 2026-05-26 20:12 CEST

This page is the maintainer checklist for presenting `ibkr` in AI tool marketplaces and app directories. It is intentionally about packaging, trust, and user expectations; feature details live in the README and reference docs.

## Positioning

`ibkr` is a local, read-only Interactive Brokers integration for users who already run IB Gateway or TWS. The clearest marketplace description is:

> Read-only Interactive Brokers access for agents: account, positions, quotes, official market calendars, option chains, history, technical/relative-strength screens, scans, fixed-fractional sizing, S&P 500 breadth, SPY+SPX dealer gamma, and an eight-row risk-regime dashboard through a local CLI/MCP server.

Always include these qualifiers:

- Requires IB Gateway or TWS running locally.
- Requires an IBKR Pro account; IBKR Lite does not include TWS API access.
- The plugin/skill does not ship the binary; users install `ibkr` separately.
- The tool is read-only and cannot place, cancel, or modify orders.
- Data returned by MCP tools can include account-sensitive balances, positions, and P&L.

## Anthropic / Claude Code

Current package:

- `.claude-plugin/plugin.json` describes the plugin.
- `.claude-plugin/marketplace.json` exposes the self-hosted marketplace.
- `skills/ibkr/SKILL.md` teaches Claude the read-only CLI workflow.
- `hooks/hooks.json` blocks trading-verb Bash calls and starts the install/version warning hook.
- `settings/ibkr.settings.json` is the optional global allow/deny template.

Pre-submit checks:

```sh
claude plugin validate .
make check
```

Anthropic's plugin submission guidance says plugins can bundle skills, MCP connectors, hooks, commands, and agents, and recommends clear setup guidance for MCP configuration. It also asks submitters to run plugin validation before submission. The public submission path is documented at <https://claude.com/docs/plugins/submit>.

## OpenAI / ChatGPT Apps

OpenAI's Apps SDK is MCP-based, but ChatGPT app distribution is not the same artifact as a Claude Code plugin. Treat this repo's current Claude plugin as reusable product content, not as an OpenAI-ready package.

Before submitting to OpenAI, add or decide:

- a ChatGPT app or connector packaging plan using OpenAI's current Apps SDK documentation
- how a local-only IB Gateway dependency will be reached from ChatGPT, for example a supported secure tunnel or a clear "local/developer mode only" installation story
- a privacy policy URL pointing at [PRIVACY.md](../../PRIVACY.md)
- a safety statement that no order placement, cancellation, or modification exists
- setup text covering IB Gateway/TWS prerequisites and the local binary install
- screenshots or a short demo focused on the risk-regime and portfolio workflows

OpenAI's Apps SDK help says app submissions are open, apps are built on MCP, and developers should prepare for safety, privacy, and functionality review, including a clear privacy policy. The current help article is at <https://help.openai.com/en/articles/12515353-build-with-the-apps-sdk>.

## Documentation That Must Stay In Sync

- README: first-run install, update path, core feature list, MCP setup, safety.
- [Agentic use](./agentic-use.md): natural-language workflows and limits.
- [MCP tools reference](../reference/mcp-tools.md): generated from `internal/mcp/tools.go`.
- [MCP resources reference](../reference/mcp-resources.md): quote resource read/subscribe behavior.
- [Configuration reference](../reference/config.md): generated from config structs and `docgen:env` comments.
- [Privacy](../../PRIVACY.md): data locality, local files, and third-party host caveat.
- [Security](../../SECURITY.md): read-only threat model, release integrity, diagnostic data sensitivity.

## Pre-Promotion Smoke List

Run these before marketplace announcements:

```sh
ibkr --help
ibkr mcp --help
ibkr setup --help
ibkr update --help
ibkr calendar --help
ibkr regime --help
make docs-regen
make check
```

With a gateway available, also run:

```sh
ibkr status
ibkr account --json
ibkr positions --json
ibkr quote SPY --json
ibkr regime --json
```

For a Claude Code release, reinstall from the marketplace path and confirm the SessionStart hook is quiet when binary and plugin major.minor match.
