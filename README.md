# ibkr — a standalone IBKR CLI for Claude Code

A read-only command-line tool that connects to a running IB Gateway, surfaces
portfolio, quote, and option-chain data, and is wired up to Claude Code via
a Skill plus permission rules.

```
$ ibkr account
Account  U1234567 · profile=live · base=EUR

  Net liquidation         $ 248,310.42
  Buying power            $ 992,841.68
  Available funds         $ 124,055.21
  ...
```

## What you get

- **`ibkr account`** — NLV, buying power, cash, margin in account base currency
- **`ibkr positions`** — open positions (stocks + options) with live marks and P&L
- **`ibkr quote SYM[,SYM…]`** — snapshot quotes; option contracts via `SYM YYMMDD C|P STRIKE`
- **`ibkr quote SYM --watch --rate 250ms`** — coalesced streaming ticks
- **`ibkr chain SYM --expiry YYYY-MM-DD --width 5`** — option-chain table
- **`ibkr scan <preset>`** — `top-movers`, `high-iv`, `unusual-vol`, `most-active` (configurable)
- **`ibkr status`** — daemon + gateway health
- **`--json`** on any of the above for parseable output

Trading verbs (`order`, `trade`, `cancel`) **do not exist** in this build. v2 will
add them behind a `-tags trading` build flag, with confirmations, dry-run, and
audit logs. Three independent layers refuse trading commands today: the binary
doesn't implement them; settings.json denies them; a `PreToolUse` hook blocks them.

## Architecture

Two binaries, one shared library. The CLI never speaks the IBKR protocol
directly; it talks JSON-RPC over a Unix socket to a daemon that holds the
long-lived gateway connection, contract cache, and subscription state.

```
   ┌──────────────┐    Unix socket       ┌──────────────┐    TCP    ┌────────────┐
   │   ibkr (CLI) │◄────────────────────►│   ibkrd      │◄─────────►│ IB Gateway │
   │   stateless  │ JSON-RPC newline-    │   daemon     │  port     │ /TWS       │
   │              │ delimited            │   stateful   │  4001     │            │
   └──────────────┘                      └──────┬───────┘           └────────────┘
                                                │
                                       ┌────────▼────────┐
                                       │ pkg/ibkr        │  ← shared lib
                                       │ (TWS protocol)  │
                                       └─────────────────┘
```

The daemon auto-spawns on first CLI invocation and auto-shuts-down after
30 minutes of inactivity (configurable). One IBKR client ID is held for the
daemon's lifetime; one TWS handshake amortises across many CLI calls.

Layout:

```
ibkr/
├── pkg/ibkr/         ← TWS protocol implementation (importable as a library)
├── internal/
│   ├── cli/          subcommand handlers + formatters
│   ├── daemon/       server, handlers, lifecycle, trading guard
│   ├── dial/         Unix-socket client used by the CLI
│   ├── config/       TOML config loader
│   ├── cache/        contract / inactive JSON caches
│   └── rpc/          shared JSON-RPC wire types
├── cmd/
│   ├── ibkr/         CLI entrypoint with daemon autospawn
│   └── ibkrd/        Daemon entrypoint
├── skill/            Claude Code skill bundle (SKILL.md + schemas.md)
├── settings/         settings.json snippet (permissions + PreToolUse hook)
└── test/integration/ live-gateway end-to-end tests
```

## Install

Prerequisites: Go 1.24+, a running IB Gateway (paper or live), `jq` on PATH for
the Claude Code hook.

```sh
make build                 # produces bin/ibkr and bin/ibkrd
make install               # installs both into $GOBIN
./install.sh --merge-settings   # copy skill bundle + merge ~/.claude/settings.json
```

`./install.sh --help` documents the install flags.

## Configure

Default config path: `~/.config/ibkr/config.toml`. A missing file is fine — the
daemon falls back to a `live` profile pointed at `127.0.0.1:4001` with TLS off
and client ID `15`. To override:

```toml
default_profile = "live"

[profiles.live]
host       = "127.0.0.1"
port       = 4001
client_id  = 15
account    = ""        # auto-detect
tls        = false

[profiles.paper]
host       = "127.0.0.1"
port       = 4002
client_id  = 16
tls        = false

[daemon]
idle_timeout = "30m"
log_level    = "info"

[scans.top-movers]
type     = "TOP_PERC_GAIN"
exchange = "STK.US.MAJOR"
limit    = 20
```

Switch profile per invocation with `ibkrd --profile paper` (the daemon's profile
is fixed for its lifetime; restart the daemon to switch).

**TLS:** defaults to `tls = false`, matching IB Gateway's out-of-box plain-socket
setting. The connector auto-detects mismatches and falls back to TLS when the
plain handshake returns no data. Setting `tls = true` is treated as a contract:
fallback is disabled and the daemon refuses to downgrade silently. `ibkr status`
shows both the configured and negotiated mode; a `⚠ fallback` marker appears
when they differ.

## Use with Claude Code

End-to-end install in three steps. Step 1 installs the binary that talks to
IB Gateway; steps 2–3 hook the binary into Claude Code via a plugin.

```sh
# 1. Install the binaries (skip if already on PATH)
go install github.com/osauer/ibkr/cmd/ibkr@latest
go install github.com/osauer/ibkr/cmd/ibkrd@latest

# 2. From inside a Claude Code session, register and install the plugin
/plugin marketplace add osauer/ibkr
/plugin install ibkr

# 3. (Optional but recommended) merge the read-only permission allowlist
#    so Claude doesn't prompt for every ibkr command. Plugins cannot ship
#    permissions, so this stays a separate step.
./install.sh --merge-settings
```

What the plugin gives you:

1. **Skill** at `skills/ibkr/SKILL.md` — Claude reads the frontmatter
   description on session start and decides when to reach for `ibkr`,
   then reads the body for per-command flag docs.
2. **Hooks**:
   - `PreToolUse` blocks any `ibkr order|trade|cancel` invocation
     independently of the binary's own trading guard.
   - `SessionStart` prints a clear install hint when the `ibkr` binary
     is missing from PATH; silent on the happy path.
3. **Permissions** (via the optional `--merge-settings` step) pre-allow
   read-only invocations so Claude doesn't ask before running
   `ibkr account` or `ibkr positions`.

The skill instructs Claude to always pass `--json` when parsing, surface
non-`live` data types prominently, and never fabricate Greeks or IV.

### Developer / dogfood path

If you're hacking on the plugin itself, replace step 2 with
`/plugin marketplace add /Users/osauer/dev/ibkr` (an absolute local path)
so Claude pulls from your working tree instead of GitHub.

## Testing

```sh
make check      # binding gate: gofmt + go vet + staticcheck + govulncheck
make test       # check + pkg/ibkr unit tests + internal/* (-race) + integration tests (-race)
```

`make check` is the gate: any committed code must keep it green. It
fails on stdlib vulnerabilities too, so an outdated Go toolchain is a
build failure — `brew upgrade go` (or equivalent) until clean. `make
test` invokes `check` first, so vet / staticcheck / vuln findings
short-circuit the suite.

One-time tool installs (the target reminds you with the exact command
on first run):

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

The integration tests under `test/integration/` connect to the live IB Gateway
on `127.0.0.1:4001`. They skip cleanly when the gateway is unreachable so a
stand-alone `go test ./...` doesn't hang waiting for an absent gateway.

To force the gateway port for tests:

```sh
IBKR_TEST_PORT=4002 make test
```

There are no mock daemons. The `protocoltest/` server inside `pkg/ibkr/` is
strictly a wire-level encoder/decoder spec used by the inherited unit tests;
all behavioural verification of the daemon, CLI, and client integration runs
against the real IB Gateway.

## Smoke-test checklist (pre-release)

Run by hand against a paper account with IB Gateway running:

- [ ] `bin/ibkrd --foreground --log stderr` connects without error
- [ ] `bin/ibkr status` reports `connected: true` and a non-zero `server_version`
- [ ] `bin/ibkr account` shows non-zero NLV
- [ ] `bin/ibkr positions` lists open positions with non-zero marks
- [ ] `bin/ibkr quote AAPL,MSFT,SPY` returns snapshot prices
- [ ] `bin/ibkr quote AAPL --watch --rate 250ms` streams; Ctrl-C exits cleanly
- [ ] `bin/ibkr chain AAPL --expiry <next-monthly>` shows a strike grid
- [ ] `bin/ibkr scan list` enumerates the four configured presets
- [ ] `make install-skill` then a fresh Claude Code session can run
      `ibkr account` without manual approval

## Troubleshooting

**`daemon socket did not appear`** — `ibkrd` crashed during startup. Check
`~/.local/state/ibkr/ibkrd.log` for the underlying error. The most common
causes are: IB Gateway not running, the configured `client_id` is in use by
another session, or the `port` is wrong (paper gateway is `4002` by default
in this project).

**Quotes time out** — Gateway is configured with strict live entitlements and
the market is closed. The daemon defaults to `SetMarketDataType(2)` (frozen)
which returns the last-known price; if your gateway is configured for `live`
only and the market is closed, snapshots may stay empty. Adjust the gateway's
market-data permissions to allow delayed/frozen data.

**`use of closed network connection` during handshake** — IB Gateway has
recently rate-limited fast handshake retries. Wait ~30 seconds before
restarting. The integration tests share a single daemon for this reason.

**Gateway accepts TCP but never replies to the v100 handshake** — IB Gateway
10.37+ ships with the API socket disabled by default and, unlike TWS, exposes
no UI toggle to turn it on. The persistence file (`~/Jts/<userdir>/ibg.xml`)
is shared between TWS and Gateway. To unblock: launch TWS once, accept its
"Enable ActiveX and Socket Clients" prompt, quit TWS, restart Gateway. The
flag carries over and Gateway will then complete the handshake. Symptoms:
spec-perfect `API\0`+`v100..MAX` frames get silence for the full 10s read
deadline; `~/Jts/<userdir>/api.<clientID>.*.ibgzenc` log files are not
created on connection attempts.

## Roadmap

| Version | Theme |
|---|---|
| v1.0 | This release: read-only foundation |
| v1.1 | History bars, watchlists, closed-today view, full chain pricing, JSON schema versioning |
| v1.2 | Full IBKR scanner DSL |
| v2.0 | Trading opt-in (`-tags trading`): dry-run, paper-only default, mandatory confirmation, audit log, kill switch |

## License

Personal use; not yet open-sourced. Contact the author before redistributing.
