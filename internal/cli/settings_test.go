package cli

import (
	"encoding/json"
	"testing"
)

func TestSettingsPatchFromAssignmentBuildsNestedPatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		assignment string
		feature    string
		want       any
	}{
		{"features.purge_restore.enabled=false", "purge_restore", false},
		{"features.stock_protection.enabled=null", "stock_protection", nil},
	} {
		raw, err := settingsPatchFromAssignment(tc.assignment)
		if err != nil {
			t.Fatalf("settingsPatchFromAssignment(%q): %v", tc.assignment, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal patch: %v", err)
		}
		features := got["features"].(map[string]any)
		feature := features[tc.feature].(map[string]any)
		if feature["enabled"] != tc.want {
			t.Fatalf("%s enabled = %#v, want %#v", tc.feature, feature["enabled"], tc.want)
		}
	}
}

func TestSettingsPatchFromAssignmentRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	if _, err := settingsPatchFromAssignment("trading.mode=paper"); err == nil {
		t.Fatal("unsupported read-only key succeeded")
	}
}

func TestSettingsSubcommandIndexHandlesHoistedFlags(t *testing.T) {
	t.Parallel()
	if got := settingsSubcommandIndex([]string{"--json", "show"}); got != 1 {
		t.Fatalf("settingsSubcommandIndex(--json show) = %d, want 1", got)
	}
	if got := settingsSubcommandIndex([]string{"--json", "set", "features.purge_restore.enabled=true"}); got != 1 {
		t.Fatalf("settingsSubcommandIndex(--json set ...) = %d, want 1", got)
	}
}
