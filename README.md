# ibkr — IBKR CLI for Claude Code

A read-only CLI that talks to a running IB Gateway and exposes account,
quote, and option-chain data, both as a regular shell tool and as a Claude
Code plugin.

```
$ ibkr account
Account  U1234567 · profile=live · base=EUR

  Net liquidation         € 248,310.42
  Buying power            € 992,841.68
  Available funds         € 124,055.21
  ...
```

Status: v1.0, read-only. Trading verbs (`order`, `trade`, `cancel`) are
not implemented. Three independent layers refuse them today: the binary
doesn't compile them, the bundled settings deny them, and a `PreToolUse`
hook blocks them. A future v2 will add them behind a build tag.

## What it does

- Account snapshot: NLV, buying power, cash, margin in base currency.
- Positions with live marks, unrealized P&L, and `--by underlying` grouping.
- Quotes: snapshot or coalesced streaming (`--watch`); options addressed
  as `SYM YYMMDD C|P STRIKE`.
- Option chains: expiry list (with optional ATM IV) or strike grid for
  a single expiry.
- Daily OHLCV history.
- Configurable scanner presets (`top-movers`, `high-iv`, `unusual-vol`,
  `most-active` out of the box).
- `--json` on every command for parseable output.

`ibkr --help` lists every subcommand and `ibkr <cmd> --help` shows its
flags. See the [CHANGELOG](CHANGELOG.md) for what shipped in each release.

## Architecture

Two binaries, one shared library. The CLI is stateless and never speaks
the IBKR protocol directly; it talks JSON-RPC over a Unix socket to a
daemon that holds the long-lived gateway connection, contract cache, and
subscription state.

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

The daemon auto-spawns on first CLI invocation and idle-shuts-down after
30 minutes (configurable). One IBKR client ID is held for the daemon's
lifetime, so one TWS handshake amortises across every CLI call.

```
ibkr/
├── pkg/ibkr/         TWS protocol implementation (importable as a library)
├── internal/
│   ├── cli/          subcommand handlers and formatters
│   ├── daemon/       server, handlers, lifecycle, trading guard
│   ├── dial/         Unix-socket client used by the CLI
│   ├── config/       TOML config loader
│   ├── cache/        contract / inactive JSON caches
│   └── rpc/          shared JSON-RPC wire types
├── cmd/{ibkr,ibkrd}/ CLI and daemon entrypoints
├── skills/ibkr/      Claude Code skill bundle (SKILL.md, schemas.md)
├── hooks/            plugin hooks (PreToolUse trading guard, SessionStart hint)
├── settings/         settings.json snippet for the read-only allowlist
├── .claude-plugin/   plugin and marketplace manifests
└── test/integration/ live-gateway end-to-end tests
```

## Install

Prerequisites: Go 1.24+, a running IB Gateway 10.37+ (paper or live), and
`jq` on PATH if you want the bundled settings merged.

```sh
go install github.com/osauer/ibkr/cmd/ibkr@latest
go install github.com/osauer/ibkr/cmd/ibkrd@latest
```

Or build from a checkout:

```sh
make install               # builds and installs both binaries into $GOBIN
```

For the Claude Code plugin (skill + hooks), from inside a Claude Code
session:

```sh
/plugin marketplace add osauer/ibkr
/plugin install ibkr
```

Plugins cannot ship permission rules, so the read-only allowlist is a
separate step that keeps Claude from prompting on every `ibkr` call:

```sh
./install.sh --merge-settings
```

Hacking on the plugin itself? Replace the marketplace step with
`/plugin marketplace add /absolute/path/to/ibkr` so Claude pulls the
skill and hooks from your working tree.

## Configure

Default config path: `$XDG_CONFIG_HOME/ibkr/config.toml`, falling back to
`~/.config/ibkr/config.toml`. A missing file is fine — the daemon assumes
a `live` profile pointed at `127.0.0.1:4001`, plain socket, client ID `15`.

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

Pick a profile per daemon with `ibkrd --profile paper`. The profile is
fixed for the daemon's lifetime; restart the daemon to switch.

TLS defaults to `tls = false` to match IB Gateway's out-of-box plain-socket
setting. The connector auto-detects mismatches and falls back to TLS when
the plain handshake returns no data. Setting `tls = true` is treated as a
contract: the fallback is disabled, and `ibkr status` shows both the
configured and negotiated mode with a `⚠ fallback` marker if they differ.

## Use with Claude Code

The plugin installs a Skill at `skills/ibkr/SKILL.md`. Claude reads its
frontmatter description on session start, decides when to reach for
`ibkr`, and consults the body for per-command flag docs. The
`--merge-settings` step pre-allows read-only invocations so Claude
doesn't ask before running `ibkr account` or `ibkr positions`.

Two hooks travel with the plugin:

- `PreToolUse` blocks any `ibkr order|trade|cancel` invocation, defence
  in depth on top of the binary's own trading guard.
- `SessionStart` prints an install hint when the `ibkr` binary is missing
  from PATH; silent otherwise.

The Skill tells Claude to pass `--json` whenever it's parsing output, and
to leave Greeks or implied volatility blank rather than fabricate them
when the gateway didn't return them.

## Testing

```sh
make check      # gofmt + go vet + staticcheck + govulncheck
make test       # check + unit tests + integration tests against live gateway
```

`make check` is the gate: any committed code keeps it green. It fails on
stdlib vulnerabilities too, so an outdated Go toolchain is a build
failure — `brew upgrade go` (or equivalent) until clean.

One-time tool installs (each target reminds you with the exact command on
first run):

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

Integration tests under `test/integration/` connect to the live IB
Gateway on `127.0.0.1:4001` and skip cleanly when the gateway is
unreachable, so a stand-alone `go test ./...` doesn't hang. To force a
different gateway port:

```sh
IBKR_TEST_PORT=4002 make test
```

There are no mock daemons. The `pkg/ibkr/protocoltest/` server is a
wire-level encoder/decoder spec used by inherited unit tests;
behavioural verification of the daemon, CLI, and client integration
runs against a real IB Gateway.

## Troubleshooting

**`daemon socket did not appear`** — `ibkrd` crashed during startup.
Check `~/.local/state/ibkr/ibkrd.log` for the underlying error. Common
causes: IB Gateway not running, the configured `client_id` is in use by
another session, or the `port` is wrong (paper gateway is `4002` by
default in this project).

**Single-instance lock.** Only one `ibkrd` per socket path may run at a time.
On startup the daemon takes an exclusive flock on `<rundir>/ibkrd.lock`
(e.g. `~/.cache/ibkr/ibkrd.lock`) and writes its PID. Concurrent CLI
invocations that race to autospawn produce one winner; losers exit cleanly
with `Another ibkrd is already running for socket …; exiting cleanly`. If a
daemon dies hard (e.g. `kill -9`), the kernel releases the flock, and the
next launch acquires it without manual cleanup.

**Quotes time out** — Gateway is on strict live entitlements and the
market is closed. The daemon defaults to `SetMarketDataType(2)` (frozen)
which returns the last-known price; if the gateway is configured for
`live` only, snapshots may stay empty out of hours. Adjust the gateway's
market-data permissions to allow delayed/frozen data.

**`use of closed network connection` during handshake** — IB Gateway has
been rate-limiting fast handshake retries lately. Wait ~30 seconds before
restarting. The integration tests share a single daemon for this reason.

**Gateway accepts TCP but never replies to the v100 handshake** — IB
Gateway 10.37+ ships with the API socket disabled by default and, unlike
TWS, exposes no UI toggle to turn it on. The persistence file
(`~/Jts/<userdir>/ibg.xml`) is shared between TWS and Gateway: launch
TWS once, accept its "Enable ActiveX and Socket Clients" prompt, quit
TWS, restart Gateway. The flag carries over and Gateway then completes
the handshake. Symptoms: spec-perfect `API\0`+`v100..MAX` frames get
silence for the full 10s read deadline; `~/Jts/<userdir>/api.<clientID>.*.ibgzenc`
log files are not created on connection attempts.

## License

UNLICENSED — source-available, not yet open-source. Contact the author
before redistributing.
