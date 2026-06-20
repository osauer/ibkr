package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/osauer/ibkr/internal/rpc"
)

func runSettings(ctx context.Context, env *Env, args []string) int {
	if len(args) == 1 && helpArg(args[0]) {
		printSettingsUsage(env)
		return 0
	}
	sub := "show"
	if idx := settingsSubcommandIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	switch sub {
	case "show":
		return runSettingsShow(ctx, env, args)
	case "set":
		return runSettingsSet(ctx, env, args)
	default:
		return fail(env, "settings: unknown subcommand %q (try `ibkr settings show` or `ibkr settings set key=value`)", sub)
	}
}

func settingsSubcommandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			name := strings.TrimLeft(arg, "-")
			if before, _, ok := strings.Cut(name, "="); ok {
				name = before
			}
			if isValueFlag(name) && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		switch arg {
		case "show", "set":
			return i
		default:
			return -1
		}
	}
	return -1
}

func runSettingsShow(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "settings show")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "settings show: takes no positional args")
	}
	var res rpc.PlatformSettings
	if err := env.Conn.Call(ctx, rpc.MethodSettingsGet, nil, &res); err != nil {
		return fail(env, "settings show: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderSettingsText(env, &res)
	return 0
}

func runSettingsSet(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "settings set")
	fs.Usage = func() { printSettingsSetUsage(env) }
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "settings set: usage is `ibkr settings set key=value`; run `ibkr settings set --help` for supported keys")
	}
	patch, err := settingsPatchFromAssignment(fs.Arg(0))
	if err != nil {
		return fail(env, "settings set: %v", err)
	}
	patch, err = settingsPatchWithOrigin(patch, env.Origin)
	if err != nil {
		return fail(env, "settings set: %v", err)
	}
	var res rpc.PlatformSettings
	if err := env.Conn.Call(ctx, rpc.MethodSettingsUpdate, patch, &res); err != nil {
		return fail(env, "settings set: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderSettingsText(env, &res)
	return 0
}

// settingsPatchWithOrigin stamps the request origin into the settings patch;
// the daemon pops the reserved "origin" key before validating settings keys
// and uses it to gate trading-limit writes on live routes.
func settingsPatchWithOrigin(patch json.RawMessage, origin string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(patch, &obj); err != nil {
		return nil, err
	}
	rawOrigin, err := json.Marshal(origin)
	if err != nil {
		return nil, err
	}
	obj["origin"] = rawOrigin
	return json.Marshal(obj)
}

func settingsPatchFromAssignment(raw string) (json.RawMessage, error) {
	key, valueRaw, ok := strings.Cut(raw, "=")
	if !ok {
		return nil, fmt.Errorf("expected key=value")
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.TrimSpace(valueRaw) == "" {
		return nil, fmt.Errorf("expected non-empty key and value")
	}
	value, err := parseSettingsValue(strings.TrimSpace(valueRaw))
	if err != nil {
		return nil, err
	}
	marshalPatch := func(path []string, value any) (json.RawMessage, error) {
		root := map[string]any{}
		cur := root
		for _, part := range path[:len(path)-1] {
			next := map[string]any{}
			cur[part] = next
			cur = next
		}
		cur[path[len(path)-1]] = value
		raw, err := json.Marshal(root)
		return json.RawMessage(raw), err
	}
	switch key {
	case "features.purge_restore.enabled":
		if value != nil {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be true, false, or null", key)
			}
		}
		return marshalPatch([]string{"features", "purge_restore", "enabled"}, value)
	case "features.stock_protection.enabled":
		if value != nil {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be true, false, or null", key)
			}
		}
		return marshalPatch([]string{"features", "stock_protection", "enabled"}, value)
	case "trading.freeze":
		if value != nil {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be true, false, or null", key)
			}
		}
		return marshalPatch([]string{"trading", "freeze"}, value)
	case "trading.limits.max_notional":
		return marshalPatch([]string{"trading", "limits", "max_notional"}, value)
	case "trading.limits.max_option_contracts":
		return marshalPatch([]string{"trading", "limits", "max_option_contracts"}, value)
	case "trading.limits.allow_stock_short":
		return marshalPatch([]string{"trading", "limits", "allow_stock_short"}, value)
	case "trading.limits.allow_option_sell_to_open":
		return marshalPatch([]string{"trading", "limits", "allow_option_sell_to_open"}, value)
	case "regime.journal.enabled":
		if value != nil {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be true, false, or null", key)
			}
		}
		return marshalPatch([]string{"regime", "journal", "enabled"}, value)
	default:
		return nil, fmt.Errorf("unsupported setting key %q (supported: %s)", key, strings.Join(supportedSettingsKeys(), ", "))
	}
}

func supportedSettingsKeys() []string {
	return []string{
		"features.purge_restore.enabled",
		"features.stock_protection.enabled",
		"trading.freeze",
		"trading.limits.max_notional",
		"trading.limits.max_option_contracts",
		"trading.limits.allow_stock_short",
		"trading.limits.allow_option_sell_to_open",
		"regime.journal.enabled",
	}
}

func printSettingsUsage(env *Env) {
	fmt.Fprintln(env.Stdout, "ibkr settings — Runtime platform preferences and observed read-only state")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Usage: ibkr settings show [--json]")
	fmt.Fprintln(env.Stdout, "       ibkr settings set <supported-key>=true|false|null|number [--json]")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Run `ibkr settings set --help` for supported keys.")
}

func printSettingsSetUsage(env *Env) {
	fmt.Fprintln(env.Stdout, "ibkr settings set — update a daemon-owned runtime setting")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Usage: ibkr settings set <supported-key>=true|false|null|number [--json]")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Supported keys:")
	for _, key := range supportedSettingsKeys() {
		fmt.Fprintf(env.Stdout, "  - %s\n", key)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "The daemon still decides writability from each field's access/source metadata.")
}

func parseSettingsValue(raw string) (any, error) {
	switch strings.ToLower(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if i, err := strconv.Atoi(raw); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("value must be true, false, null, or a number")
}

func renderSettingsText(env *Env, st *rpc.PlatformSettings) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Settings  %s\n", env.statusBadge(settingsVerdict(*st)))
	fmt.Fprintln(out)
	statusRow(env, out, "Purge/restore", formatSettingsBool(env, st.Features.PurgeRestore.Enabled))
	statusRow(env, out, "Stock protection", formatSettingsBool(env, st.Features.StockProtection.Enabled))
	statusRow(env, out, "Trading freeze", formatSettingsBool(env, st.Trading.Freeze))
	statusRow(env, out, "Trading", nonEmpty(st.Trading.Mode.Value, "disabled"))
	statusRow(env, out, "Endpoint", nonEmpty(st.Trading.Endpoint.Value, "unknown"))
	statusRow(env, out, "Account", nonEmpty(st.Trading.Account.Value, "unknown"))
	statusRow(env, out, "MCP trading", nonEmpty(st.Trading.MCPTrading.Value, "disabled"))
	statusRow(env, out, "Build", st.Build.Channel.Value)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Trading limits:")
	statusRow(env, out, "Max notional (opening)", fmt.Sprintf("%.2f (%s)", st.Trading.Limits.MaxNotional.Value, accessSummary(st.Trading.Limits.MaxNotional.Access, st.Trading.Limits.MaxNotional.Source)))
	statusRow(env, out, "Max option qty (opening)", fmt.Sprintf("%d (%s)", st.Trading.Limits.MaxOptionContracts.Value, accessSummary(st.Trading.Limits.MaxOptionContracts.Access, st.Trading.Limits.MaxOptionContracts.Source)))
	statusRow(env, out, "Reduce-only", "exempt (bounded by position size)")
	statusRow(env, out, "Stock short", formatSettingsBool(env, st.Trading.Limits.AllowStockShort))
	statusRow(env, out, "Option STO", formatSettingsBool(env, st.Trading.Limits.AllowOptionSellToOpen))
	fmt.Fprintln(out)
	statusRow(env, out, "Market data", nonEmpty(st.MarketData.Quality.Status, "unknown")+" - "+nonEmpty(st.MarketData.Quality.Summary, "no observation"))
	if st.Build.ExperimentalTradingNote != "" {
		statusRow(env, out, "Build note", st.Build.ExperimentalTradingNote)
	}
	fmt.Fprintln(out)
}

func settingsVerdict(st rpc.PlatformSettings) statusConcern {
	if st.Trading.Freeze.Value {
		return statusConcern{Text: "FROZEN", Level: statusConcernWarn}
	}
	if !st.Features.PurgeRestore.Enabled.Value {
		return statusConcern{Text: "LIMITED", Level: statusConcernNotice}
	}
	if !st.Features.StockProtection.Enabled.Value {
		return statusConcern{Text: "LIMITED", Level: statusConcernNotice}
	}
	if st.MarketData.Quality.Status == "degraded" || st.MarketData.Quality.Status == "delayed" {
		return statusConcern{Text: "DEGRADED", Level: statusConcernWarn}
	}
	return statusConcern{Text: "READY", Level: statusConcernNone}
}

func formatSettingsBool(env *Env, v rpc.SettingsBool) string {
	value := fmt.Sprint(v.Value)
	if v.Access == rpc.SettingsAccessWrite {
		return env.green(value) + " (" + accessSummary(v.Access, v.Source) + ")"
	}
	return value + " (" + accessSummary(v.Access, v.Source) + ")"
}

func accessSummary(access, source string) string {
	if source == "" {
		return access
	}
	return access + "/" + source
}
