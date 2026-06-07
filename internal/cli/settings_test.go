package cli

import (
	"encoding/json"
	"testing"
)

func TestSettingsPatchFromAssignmentBuildsNestedPatch(t *testing.T) {
	t.Parallel()
	raw, err := settingsPatchFromAssignment("features.purge_restore.enabled=false")
	if err != nil {
		t.Fatalf("settingsPatchFromAssignment: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	features := got["features"].(map[string]any)
	purge := features["purge_restore"].(map[string]any)
	if purge["enabled"] != false {
		t.Fatalf("enabled = %#v, want false", purge["enabled"])
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
