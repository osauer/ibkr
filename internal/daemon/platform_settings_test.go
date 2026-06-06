package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestPlatformSettingsDefaultsAndPersistence(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{})

	got, err := srv.handleSettingsGet()
	if err != nil {
		t.Fatalf("handleSettingsGet: %v", err)
	}
	if !got.Features.PurgeRestore.Enabled.Value {
		t.Fatal("purge_restore.enabled default = false, want true")
	}
	if got.Features.PurgeRestore.Enabled.Access != rpc.SettingsAccessWrite {
		t.Fatalf("purge_restore.enabled access = %q, want write", got.Features.PurgeRestore.Enabled.Access)
	}
	if !got.AutoTrade.ProposalsEnabled.Value {
		t.Fatal("auto_trade.proposals_enabled default = false, want true")
	}
	if got.AutoTrade.ProposalsEnabled.Access != rpc.SettingsAccessRead || got.AutoTrade.ProposalsEnabled.Source != rpc.SettingsSourceConfig {
		t.Fatalf("auto_trade.proposals_enabled meta = %s/%s, want read/config", got.AutoTrade.ProposalsEnabled.Access, got.AutoTrade.ProposalsEnabled.Source)
	}
	if !got.AutoTrade.FastPathEnabled.Value {
		t.Fatal("auto_trade.fast_path_enabled default = false, want true")
	}

	patch := mustRaw(t, map[string]any{
		"features": map[string]any{"purge_restore": map[string]any{"enabled": false}},
	})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: patch}); err != nil {
		t.Fatalf("disable purge_restore: %v", err)
	}
	reopened, err := newPlatformSettingsStore(srv.platformSettings.path)
	if err != nil {
		t.Fatalf("reopen settings store: %v", err)
	}
	srv.platformSettings = reopened
	got, err = srv.handleSettingsGet()
	if err != nil {
		t.Fatalf("handleSettingsGet after reopen: %v", err)
	}
	if got.Features.PurgeRestore.Enabled.Value {
		t.Fatal("purge_restore.enabled persisted true, want false")
	}

	reset := []byte(`{"features":{"purge_restore":{"enabled":null}}}`)
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: reset}); err != nil {
		t.Fatalf("reset purge_restore: %v", err)
	}
	got, _ = srv.handleSettingsGet()
	if !got.Features.PurgeRestore.Enabled.Value {
		t.Fatal("purge_restore.enabled reset = false, want default true")
	}
}

func TestPlatformSettingsRejectsUnknownAndReadOnlyWrites(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Enabled: false, Mode: config.TradingModePaper})

	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"bogus":true}`)}); err == nil {
		t.Fatal("unknown top-level field succeeded")
	}
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"enabled":true}}`)}); err == nil {
		t.Fatal("read-only trading.enabled write succeeded")
	}
	_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"limits":{"max_notional":5000}}}`)})
	if err == nil {
		t.Fatal("trading limit write succeeded while trading disabled")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("limit write error = %v, want read-only", err)
	}
}

func TestPlatformSettingsPurgeDisabledBlocksPurgeWrites(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"purge_restore":{"enabled":false}}}`)}); err != nil {
		t.Fatalf("disable purge_restore: %v", err)
	}
	blockers := srv.purgeExecuteBlockers(rpc.TradingStatus{Enabled: true, Mode: config.TradingModePaper})
	if !hasBlocker(blockers, "purge_restore_disabled") {
		t.Fatalf("purge blockers missing purge_restore_disabled: %#v", blockers)
	}
	preview := srv.purgeRestorePreviewBlockers(rpc.TradingStatus{Enabled: true, Mode: config.TradingModePaper})
	if !hasBlocker(preview, "purge_restore_disabled") {
		t.Fatalf("restore preview blockers missing purge_restore_disabled: %#v", preview)
	}
}

func newPlatformSettingsTestServer(t *testing.T, tr config.Trading) *Server {
	t.Helper()
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatalf("newPlatformSettingsStore: %v", err)
	}
	return &Server{
		cfg:              &config.Resolved{Trading: tr},
		platformSettings: store,
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func hasBlocker(blockers []rpc.TradingBlocker, code string) bool {
	for _, blocker := range blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}
