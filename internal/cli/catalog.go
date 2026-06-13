package cli

import "slices"

// GuardClass describes whether a command can run directly inside the TUI or
// needs a human confirmation first. It is metadata only; existing CLI gates
// still enforce the real safety policy.
type GuardClass string

const (
	GuardReadOnly GuardClass = "read-only"
	GuardLocal    GuardClass = "local"
	GuardConfirm  GuardClass = "confirm"
)

// TUISupport describes how the full-screen terminal app should handle a
// command. External commands are advertised for discovery but should be run in
// a regular terminal because they own a process, stdio stream, or installer
// lifecycle outside the TUI's prompt/output model.
type TUISupport string

const (
	TUISupported TUISupport = "supported"
	TUIExternal  TUISupport = "external"
)

// FlagSpec is the shared flag metadata used by command-line flag hoisting and
// TUI completion. Values is intentionally small and enum-like; dynamic
// completion (symbols, watchlist names) lives in the TUI layer.
type FlagSpec struct {
	Name       string
	TakesValue bool
	Values     []string
	Summary    string
}

// SubcommandSpec captures nested command words that are useful for completion
// and guard classification. The existing handlers remain authoritative for
// parsing and validation.
type SubcommandSpec struct {
	Name  string
	Guard GuardClass
}

// CommandSpec is the user-facing command catalog shared by the one-shot CLI
// and the TUI. Name/Summary/Usage are copied from Commands() at runtime so the
// help table and catalog cannot silently drift.
type CommandSpec struct {
	Name        string
	Summary     string
	Usage       string
	Flags       []FlagSpec
	Subcommands []SubcommandSpec
	Guard       GuardClass
	TUI         TUISupport
}

// Catalog returns the registered commands with shared metadata for completion,
// TUI guard decisions, and flag-value handling.
func Catalog() []CommandSpec {
	extras := catalogExtras()
	out := make([]CommandSpec, 0, len(commands))
	for _, cmd := range commands {
		spec := extras[cmd.Name]
		spec.Name = cmd.Name
		spec.Summary = cmd.Summary
		spec.Usage = cmd.Usage
		if spec.Guard == "" {
			spec.Guard = GuardReadOnly
		}
		if spec.TUI == "" {
			spec.TUI = TUISupported
		}
		out = append(out, spec)
	}
	return out
}

func catalogExtras() map[string]CommandSpec {
	return map[string]CommandSpec{
		"status":    {Flags: flags(boolFlag("json"))},
		"account":   {Flags: flags(boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"))},
		"positions": {Flags: flags(valueFlag("symbol", nil), valueFlag("type", []string{"stk", "opt"}), valueFlag("sort", []string{"alpha", "pnl", "value"}), boolFlag("quotes"), valueFlag("by", []string{"underlying"}), valueFlag("view", []string{"full", "risk"}), boolFlag("watch"), valueFlag("rate", nil), boolFlag("json"))},
		"quote":     {Flags: flags(valueFlag("market", []string{"us", "de"}), boolFlag("watch"), valueFlag("rate", nil), valueFlag("timeout", nil), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), boolFlag("json"))},
		"watch": {
			Flags:       flags(boolFlag("quotes"), valueFlag("timeout", nil), boolFlag("json"), boolFlag("list"), boolFlag("watch"), valueFlag("rate", nil), boolFlag("add"), boolFlag("remove"), boolFlag("clear")),
			Subcommands: subcommands("add", "remove", "list", "clear"),
			Guard:       GuardLocal,
		},
		"calendar":      {Flags: flags(valueFlag("market", []string{"us", "us-options", "de"}), valueFlag("date", nil), valueFlag("next", nil), boolFlag("json"))},
		"chain":         {Flags: flags(valueFlag("expiry", nil), valueFlag("width", nil), valueFlag("side", []string{"calls", "puts", "both"}), valueFlag("class", []string{"SPX", "SPXW"}), boolFlag("no-iv"), boolFlag("all-expiries"), boolFlag("require-live-iv"), valueFlag("min-dte", nil), valueFlag("max-dte", nil), valueFlag("target-dte", nil), boolFlag("json"))},
		"history":       {Flags: flags(valueFlag("days", nil), boolFlag("json"))},
		"technical":     {Flags: flags(valueFlag("benchmark", nil), valueFlag("market", []string{"us", "de"}), valueFlag("lookback-days", nil), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), boolFlag("json"))},
		"market-events": {Flags: flags(valueFlag("symbol", nil), boolFlag("json"))},
		"breadth":       {Flags: flags(valueFlag("days", nil), boolFlag("json"))},
		"gamma":         {Flags: flags(boolFlag("no-wait"), boolFlag("force"), valueFlag("only", []string{"spy", "spx"}), boolFlag("explain"), boolFlag("diagnostics"), boolFlag("profiles"), boolFlag("json"))},
		"regime":        {Flags: flags(boolFlag("explain"), boolFlag("diagnostics"), boolFlag("watch"), valueFlag("rate", nil), valueFlag("log", nil), valueFlag("view", []string{"detail", "monitor"}), boolFlag("json"))},
		"canary":        {Flags: flags(boolFlag("details"), valueFlag("view", []string{"full", "alert"}), boolFlag("json"))},
		"proposals":     {Flags: flags(valueFlag("quantity", nil), valueFlag("timeout", nil), boolFlag("fast-path"), valueFlag("reason", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "submit", Guard: GuardConfirm}, {Name: "ignore", Guard: GuardLocal}}, Guard: GuardConfirm},
		"opportunities": {Flags: flags(valueFlag("quantity", nil), valueFlag("timeout", nil), valueFlag("reason", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "refresh", Guard: GuardReadOnly}, {Name: "list", Guard: GuardReadOnly}, {Name: "preview", Guard: GuardReadOnly}, {Name: "exercise", Guard: GuardConfirm}, {Name: "ignore", Guard: GuardLocal}}, Guard: GuardConfirm},
		"purge":         {Flags: flags(boolFlag("all"), boolFlag("json"), valueFlag("timeout", nil), boolFlag("watch"), valueFlag("rate", nil), valueFlag("scale", nil), boolFlag("record"), boolFlag("save"), boolFlag("execute"), valueFlag("wait", nil), valueFlag("account", nil)), Subcommands: []SubcommandSpec{{Name: "dry-run", Guard: GuardReadOnly}, {Name: "status", Guard: GuardReadOnly}, {Name: "monitor", Guard: GuardReadOnly}, {Name: "restore", Guard: GuardReadOnly}, {Name: "execute", Guard: GuardConfirm}}, Guard: GuardConfirm},
		"backtest":      {Flags: flags(valueFlag("input", nil), boolFlag("json")), Subcommands: subcommands("canary", "regime", "opportunity", "build-regime"), Guard: GuardLocal},
		"scan":          {Flags: flags(valueFlag("type", nil), valueFlag("exchange", nil), valueFlag("instrument", nil), valueFlag("limit", nil), valueFlag("min-price", nil), valueFlag("min-volume", nil), valueFlag("min-dollar-volume", nil), boolFlag("require-live"), boolFlag("exclude-penny"), boolFlag("raw"), boolFlag("json")), Subcommands: subcommands("list", "params")},
		"size":          {Flags: flags(valueFlag("symbol", nil), valueFlag("entry", nil), valueFlag("stop", nil), valueFlag("target", nil), valueFlag("risk-pct", nil), valueFlag("side", []string{"long", "short"}), valueFlag("lot", nil), valueFlag("fx", nil), boolFlag("json"))},
		"trading":       {Flags: flags(valueFlag("timeout", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "status", Guard: GuardReadOnly}, {Name: "paper-smoke", Guard: GuardConfirm}}},
		"orders":        {Flags: flags(valueFlag("account", nil), boolFlag("json")), Subcommands: subcommands("open")},
		"order":         {Flags: flags(valueFlag("limit", nil), valueFlag("strategy", []string{"patient-limit", "explicit-limit", "broker-trail"}), valueFlag("order-type", []string{"LMT", "TRAIL", "TRAIL-LIMIT"}), valueFlag("trail-percent", nil), valueFlag("trail-amount", nil), valueFlag("initial-stop", nil), valueFlag("limit-offset", nil), valueFlag("tif", []string{"DAY", "GTC"}), boolFlag("outside-rth"), valueFlag("replace-order", nil), valueFlag("timeout", nil), valueFlag("market", []string{"us", "de"}), valueFlag("exchange", nil), valueFlag("primary", nil), valueFlag("currency", nil), valueFlag("preview-token", nil), boolFlag("json")), Subcommands: []SubcommandSpec{{Name: "preview", Guard: GuardReadOnly}, {Name: "status", Guard: GuardReadOnly}, {Name: "place", Guard: GuardConfirm}, {Name: "modify", Guard: GuardConfirm}, {Name: "cancel", Guard: GuardConfirm}}},
		"app":           {Flags: flags(valueFlag("addr", nil), valueFlag("public-url", nil), valueFlag("state-dir", nil), boolFlag("json")), Subcommands: subcommands("pair", "serve"), Guard: GuardLocal, TUI: TUIExternal},
		"mcp":           {Flags: flags(valueFlag("profile", []string{"full", "monitor"})), Guard: GuardLocal, TUI: TUIExternal},
		"daemon":        {Flags: flags(boolFlag("foreground"), boolFlag("version"), valueFlag("config", nil), valueFlag("socket", nil), valueFlag("log", nil)), Guard: GuardLocal, TUI: TUIExternal},
		"setup":         {Guard: GuardLocal, TUI: TUIExternal},
		"update":        {Flags: flags(boolFlag("check"), boolFlag("force"), boolFlag("restart"), boolFlag("no-restart")), Guard: GuardConfirm},
		"restart":       {Flags: flags(boolFlag("app"), boolFlag("force"), valueFlag("timeout", nil), valueFlag("addr", nil), valueFlag("public-url", nil), valueFlag("state-dir", nil), boolFlag("json")), Guard: GuardConfirm},
		"version":       {Guard: GuardLocal},
	}
}

func boolFlag(name string) FlagSpec {
	return FlagSpec{Name: name}
}

func valueFlag(name string, values []string) FlagSpec {
	return FlagSpec{Name: name, TakesValue: true, Values: slices.Clone(values)}
}

func flags(in ...FlagSpec) []FlagSpec {
	return append([]FlagSpec(nil), in...)
}

func subcommands(names ...string) []SubcommandSpec {
	out := make([]SubcommandSpec, 0, len(names))
	for _, name := range names {
		out = append(out, SubcommandSpec{Name: name, Guard: GuardReadOnly})
	}
	return out
}
