package cli

import "testing"

func TestCatalogCoversCommands(t *testing.T) {
	t.Parallel()
	cmds := Commands()
	catalog := Catalog()
	if len(catalog) != len(cmds) {
		t.Fatalf("Catalog len=%d, Commands len=%d", len(catalog), len(cmds))
	}
	seen := map[string]bool{}
	for i, cmd := range cmds {
		spec := catalog[i]
		if seen[spec.Name] {
			t.Fatalf("duplicate catalog entry %q", spec.Name)
		}
		seen[spec.Name] = true
		if spec.Name != cmd.Name {
			t.Fatalf("catalog[%d].Name=%q, Commands[%d].Name=%q", i, spec.Name, i, cmd.Name)
		}
		if spec.Summary != cmd.Summary {
			t.Fatalf("%s summary drift", cmd.Name)
		}
		if spec.Usage != cmd.Usage {
			t.Fatalf("%s usage drift", cmd.Name)
		}
		if spec.Guard == "" {
			t.Fatalf("%s missing guard class", cmd.Name)
		}
		if spec.TUI == "" {
			t.Fatalf("%s missing TUI support", cmd.Name)
		}
	}
}

func TestCatalogValueFlagsDriveHoisting(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"expiry", "width", "side", "rate", "timeout", "limit", "symbol",
		"type", "sort", "days", "by", "lookback-days", "benchmark",
		"entry", "stop", "target", "risk-pct", "lot", "fx",
		"only", "scale", "market", "exchange", "primary", "currency", "instrument", "log",
		"date", "next", "input", "min-price", "min-volume", "min-dollar-volume",
		"min-dte", "max-dte", "target-dte", "class",
		"mode", "from-canary", "from-plan", "candidate",
		"account", "preview-token", "strategy", "tif", "replace-order",
		"addr", "public-url", "state-dir", "config", "socket",
		"profile", "view", "wait",
	} {
		if !isValueFlag(name) {
			t.Fatalf("isValueFlag(%q)=false, want true", name)
		}
	}
	for _, name := range []string{"json", "watch", "force", "details", "all", "save", "record", "execute", "require-live", "exclude-penny", "profiles", "bypass-preview"} {
		if isValueFlag(name) {
			t.Fatalf("isValueFlag(%q)=true, want false", name)
		}
	}
}
