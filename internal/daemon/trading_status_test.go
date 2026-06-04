package daemon

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/discover"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestTradingStatusDefaultDisabled(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: &config.Resolved{}}
	st := srv.tradingStatus(discover.Endpoint{})

	if st.Enabled {
		t.Fatal("default trading status should be disabled")
	}
	if st.LocalGate != rpc.TradingLocalGateDisabled {
		t.Fatalf("LocalGate = %q, want disabled", st.LocalGate)
	}
	if st.Blocked {
		t.Fatalf("disabled trading should not render as blocked: %+v", st.Blockers)
	}
	if !st.PreviewRequired {
		t.Fatal("preview should default required")
	}
	if st.CanPreview || st.CanTransmit || st.CanModify || st.CanCancel {
		t.Fatalf("disabled capabilities should all be false: %+v", st)
	}
}

func TestTradingStatusBlocksEnabledWithoutPinnedGateway(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: &config.Resolved{
		Trading: config.Trading{Enabled: true, Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 15})

	for _, code := range []string{"gateway_port_unpinned", "gateway_account_unpinned", "gateway_client_id_unpinned"} {
		if !hasTradingBlocker(st, code) {
			t.Fatalf("missing blocker %q in %+v", code, st.Blockers)
		}
	}
}

func TestTradingStatusBlocksPaperModeOnLiveLookingEndpoint(t *testing.T) {
	t.Parallel()
	port := 4001
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "U1234567"},
		Trading: config.Trading{Enabled: true, Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 31, Account: "U1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "paper_endpoint_unconfirmed") {
		t.Fatalf("missing paper endpoint blocker in %+v", st.Blockers)
	}
}

func TestTradingStatusBlocksAggregateAccount(t *testing.T) {
	t.Parallel()
	port := 7497
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "All"},
		Trading: config.Trading{Enabled: true, Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 7497, ClientID: 31, Account: "All", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "gateway_account_not_concrete") {
		t.Fatalf("missing aggregate account blocker in %+v", st.Blockers)
	}
	if st.CanPreview || st.CanTransmit || st.CanModify || st.CanCancel {
		t.Fatalf("blocked capabilities should all be false: %+v", st)
	}
}

func TestTradingStatusBlocksClientIDAutoWalk(t *testing.T) {
	t.Parallel()
	port := 4002
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
		Trading: config.Trading{Enabled: true, Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 32, Account: "DU1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "gateway_client_id_autowalked") {
		t.Fatalf("missing client-id auto-walk blocker in %+v", st.Blockers)
	}
	if st.CanPreview || st.CanTransmit || st.CanModify || st.CanCancel {
		t.Fatalf("blocked capabilities should all be false: %+v", st)
	}
}

func TestTradingStatusReadyPaperPreviewCapability(t *testing.T) {
	t.Parallel()
	port := 4002
	clientID := 31
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
			Trading: config.Trading{Enabled: true, Mode: config.TradingModePaper}.WithDefaults(),
		},
		orderJournal: newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl")),
	}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU1234567", PortOrigin: discover.OriginPinned})

	if st.Blocked {
		t.Fatalf("paper status should be ready, got blockers %+v", st.Blockers)
	}
	if !st.CanPreview {
		t.Fatalf("ready paper gate should allow preview: %+v", st)
	}
	if st.CanTransmit || st.CanModify || st.CanCancel {
		t.Fatalf("write capabilities must remain disabled in preview-only build: %+v", st)
	}
}

func TestTradingStatusLiveModeRequiresOverrideAndReadiness(t *testing.T) {
	t.Parallel()
	port := 4001
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "U1234567"},
		Trading: config.Trading{Enabled: true, Mode: config.TradingModeLive}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 31, Account: "U1234567", PortOrigin: discover.OriginPinned})

	for _, code := range []string{"live_not_allowed", "live_account_ack_mismatch", "live_endpoint_ack_mismatch", "paper_smoke_missing"} {
		if !hasTradingBlocker(st, code) {
			t.Fatalf("missing blocker %q in %+v", code, st.Blockers)
		}
	}
}

func TestTradingStatusBlocksLiveModeOnPaperLookingEndpoint(t *testing.T) {
	t.Parallel()
	port := 4002
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
		Trading: config.Trading{
			Enabled:         true,
			Mode:            config.TradingModeLive,
			AllowLive:       true,
			LiveAckAccount:  "DU1234567",
			LiveAckEndpoint: "127.0.0.1:4002",
		}.WithDefaults(),
	}, orderJournal: newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl"))}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "live_endpoint_unconfirmed") {
		t.Fatalf("missing live endpoint blocker in %+v", st.Blockers)
	}
}

func TestTradingStatusLiveReadyWithMatchingPaperSmoke(t *testing.T) {
	t.Parallel()
	port := 4001
	clientID := 31
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	store := newTradingReadinessStore(filepath.Join(t.TempDir(), "trading-readiness.json"))
	if err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "U1234567"},
			Trading: config.Trading{
				Enabled:         true,
				Mode:            config.TradingModeLive,
				AllowLive:       true,
				LiveAckAccount:  "U1234567",
				LiveAckEndpoint: "127.0.0.1:4001",
			}.WithDefaults(),
		},
		version:          "test-version",
		now:              func() time.Time { return now },
		tradingReadiness: store,
		orderJournal:     newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl")),
	}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 31, Account: "U1234567", PortOrigin: discover.OriginPinned})

	if st.Blocked {
		t.Fatalf("live status should be ready, got blockers %+v", st.Blockers)
	}
	if st.PaperSmoke != tradingPaperSmokeStatusValid {
		t.Fatalf("PaperSmoke = %q, want valid", st.PaperSmoke)
	}
	if st.PaperSmokeAt == nil || !st.PaperSmokeAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("PaperSmokeAt = %s", st.PaperSmokeAt)
	}
	if st.BrokerGate != rpc.BrokerTradingGatePaperSmokePassed {
		t.Fatalf("BrokerGate = %q, want paper-smoke-passed", st.BrokerGate)
	}
	if st.LiveOverride != rpc.TradingLiveOverrideReady {
		t.Fatalf("LiveOverride = %q, want ready", st.LiveOverride)
	}
	if !st.CanPreview || st.CanTransmit || st.CanModify || st.CanCancel {
		t.Fatalf("live-ready capabilities should expose preview only in default build: %+v", st)
	}
}

func hasTradingBlocker(st rpc.TradingStatus, code string) bool {
	return slices.ContainsFunc(st.Blockers, func(b rpc.TradingBlocker) bool {
		return b.Code == code
	})
}
