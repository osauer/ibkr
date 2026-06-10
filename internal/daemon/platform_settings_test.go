package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !got.Features.StockProtection.Enabled.Value {
		t.Fatal("stock_protection.enabled default = false, want true")
	}
	if got.Features.PurgeRestore.Enabled.Access != rpc.SettingsAccessWrite {
		t.Fatalf("purge_restore.enabled access = %q, want write", got.Features.PurgeRestore.Enabled.Access)
	}
	if got.Features.StockProtection.Enabled.Access != rpc.SettingsAccessWrite {
		t.Fatalf("stock_protection.enabled access = %q, want write", got.Features.StockProtection.Enabled.Access)
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
		"features": map[string]any{
			"purge_restore":    map[string]any{"enabled": false},
			"stock_protection": map[string]any{"enabled": false},
		},
	})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: patch}); err != nil {
		t.Fatalf("disable runtime settings: %v", err)
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
	if got.Features.StockProtection.Enabled.Value {
		t.Fatal("stock_protection.enabled persisted true, want false")
	}

	reset := []byte(`{"features":{"purge_restore":{"enabled":null},"stock_protection":{"enabled":null}}}`)
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: reset}); err != nil {
		t.Fatalf("reset runtime settings: %v", err)
	}
	got, _ = srv.handleSettingsGet()
	if !got.Features.PurgeRestore.Enabled.Value {
		t.Fatal("purge_restore.enabled reset = false, want default true")
	}
	if !got.Features.StockProtection.Enabled.Value {
		t.Fatal("stock_protection.enabled reset = false, want default true")
	}
}

func TestPlatformSettingsRejectsUnknownAndReadOnlyWrites(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeDisabled})

	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"bogus":true}`)}); err == nil {
		t.Fatal("unknown top-level field succeeded")
	}
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"mode":"paper"}}`)}); err == nil {
		t.Fatal("read-only trading.mode write succeeded")
	}
	_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"limits":{"max_notional":5000}}}`)})
	if err == nil {
		t.Fatal("trading limit write succeeded while trading disabled")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("limit write error = %v, want read-only", err)
	}
}

func TestPlatformSettingsLiveTradingPatchRefusesAgentOrigin(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeLive})

	// Agent origin (explicit or missing) may not touch trading settings on a
	// live route, regardless of which trading key is inside the patch.
	for _, params := range []string{
		`{"trading":{"limits":{"max_notional":50000}},"origin":"agent"}`,
		`{"trading":{"limits":{"max_notional":50000}}}`,
	} {
		_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(params)})
		if err == nil || !strings.Contains(err.Error(), "agent-origin") {
			t.Fatalf("live agent trading patch (%s) err = %v, want agent-origin refusal", params, err)
		}
	}

	// A human origin passes the origin gate; whatever error remains must be
	// about limit writability, not origin.
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"limits":{"max_notional":50000}},"origin":"human-tty"}`)}); err != nil && strings.Contains(err.Error(), "agent-origin") {
		t.Fatalf("live human trading patch err = %v, want no origin refusal", err)
	}

	// Feature toggles stay origin-free even on live: they cannot loosen
	// broker-write limits.
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":true}},"origin":"agent"}`)}); err != nil {
		t.Fatalf("live agent feature patch err = %v, want success", err)
	}
}

func TestPlatformSettingsPurgeDisabledBlocksPurgeWrites(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModePaper})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"purge_restore":{"enabled":false}}}`)}); err != nil {
		t.Fatalf("disable purge_restore: %v", err)
	}
	blockers := srv.purgeExecuteBlockers(rpc.TradingStatus{Mode: config.TradingModePaper})
	if !hasBlocker(blockers, "purge_restore_disabled") {
		t.Fatalf("purge blockers missing purge_restore_disabled: %#v", blockers)
	}
	preview := srv.purgeRestorePreviewBlockers(rpc.TradingStatus{Mode: config.TradingModePaper})
	if !hasBlocker(preview, "purge_restore_disabled") {
		t.Fatalf("restore preview blockers missing purge_restore_disabled: %#v", preview)
	}
}

func TestPlatformSettingsStockProtectionDisabledBlocksStockTrailProposal(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":false}}}`)}); err != nil {
		t.Fatalf("disable stock_protection: %v", err)
	}
	bid, ask := 100.0, 100.2
	status := protectionPolicyStatus(defaultProtectionPolicy(), rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	prop, ok := trailingStopStockProposal(defaultProtectionPolicy(), status, rpc.PositionView{Symbol: "MSFT", SecType: "STK", Quantity: 10, Bid: &bid, Ask: &ask, Mark: 100.1, Multiplier: 1, Currency: "USD"}, rpc.TradeProposalSourceFingerprints{}, time.Now(), srv.stockProtectionEnabled(), 0)
	if !ok {
		t.Fatal("stock trail proposal missing")
	}
	if !hasBlocker(prop.Blockers, "stock_protection_disabled") {
		t.Fatalf("proposal blockers = %+v, want stock_protection_disabled", prop.Blockers)
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

func TestPlatformSettingsTradingFreezeBlocksWritesAllowsCancels(t *testing.T) {
	t.Parallel()
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderWritesEnabled = func() bool { return true }
	srv.orderJournal = newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl"))

	if srv.tradingFrozen() {
		t.Fatal("tradingFrozen default = true, want false")
	}
	got, err := srv.handleSettingsGet()
	if err != nil {
		t.Fatalf("handleSettingsGet: %v", err)
	}
	if got.Trading.Freeze.Value || got.Trading.Freeze.Access != rpc.SettingsAccessWrite || got.Trading.Freeze.Source != rpc.SettingsSourceRuntime {
		t.Fatalf("freeze setting = %+v, want writable runtime false", got.Trading.Freeze)
	}

	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"freeze":true}}`)}); err != nil {
		t.Fatalf("engage freeze: %v", err)
	}
	if !srv.tradingFrozen() {
		t.Fatal("tradingFrozen after freeze=true patch = false, want true")
	}

	status := rpc.TradingStatus{Mode: config.TradingModePaper}
	auth := srv.brokerWriteAuthorization(status)
	if auth.Allowed || !hasBlocker(auth.Blockers, tradingFrozenBlockerCode) {
		t.Fatalf("frozen write authorization = %+v, want trading_frozen blocker", auth)
	}
	cancelAuth := auth.forCancel()
	if !cancelAuth.Allowed || hasBlocker(cancelAuth.Blockers, tradingFrozenBlockerCode) {
		t.Fatalf("frozen cancel authorization = %+v, want allowed", cancelAuth)
	}
	if !hasBlocker(srv.purgeExecuteBlockers(status), tradingFrozenBlockerCode) {
		t.Fatal("purge execute blockers missing trading_frozen while frozen")
	}

	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"freeze":null}}`)}); err != nil {
		t.Fatalf("reset freeze: %v", err)
	}
	if srv.tradingFrozen() {
		t.Fatal("tradingFrozen after freeze=null reset = true, want false")
	}
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"freeze":"yes"}}`)}); err == nil {
		t.Fatal("non-boolean trading.freeze accepted")
	}

	// The brake is deliberately not gated on tradingLimitWritability: it
	// must engage even while order entry is disabled or misconfigured.
	disabled := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeDisabled})
	if _, err := disabled.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"freeze":true}}`)}); err != nil {
		t.Fatalf("freeze while trading disabled: %v", err)
	}
	if !disabled.tradingFrozen() {
		t.Fatal("freeze did not engage while trading disabled")
	}
}
