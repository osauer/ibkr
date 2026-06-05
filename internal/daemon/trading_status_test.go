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

func TestAccountModeForStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		port    int
		account string
		want    string
	}{
		{name: "gateway paper port", port: 4002, want: rpc.AccountModePaper},
		{name: "tws paper port", port: 7497, want: rpc.AccountModePaper},
		{name: "paper account", port: 4001, account: "DU1234567", want: rpc.AccountModePaper},
		{name: "gateway live port", port: 4001, want: rpc.AccountModeLive},
		{name: "tws live port", port: 7496, want: rpc.AccountModeLive},
		{name: "live account on custom port", port: 5000, account: "U1234567", want: rpc.AccountModeLive},
		{name: "aggregate account on custom port", port: 5000, account: "All", want: rpc.AccountModeUnknown},
		{name: "unknown", port: 5000, want: rpc.AccountModeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountModeForStatus(tc.port, tc.account); got != tc.want {
				t.Fatalf("accountModeForStatus(%d, %q) = %q, want %q", tc.port, tc.account, got, tc.want)
			}
		})
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

func TestAccountMismatchesConnectedAllowsAggregateManagedAccount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		configured string
		connected  string
		want       bool
	}{
		{name: "same concrete account", configured: "DU1234567", connected: "DU1234567", want: false},
		{name: "case-insensitive concrete account", configured: "du1234567", connected: "DU1234567", want: false},
		{name: "connected aggregate all", configured: "DU1234567", connected: "All", want: false},
		{name: "blank connected account unknown", configured: "DU1234567", connected: "", want: false},
		{name: "different concrete account", configured: "DU1234567", connected: "DU7654321", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := accountMismatchesConnected(tc.configured, tc.connected); got != tc.want {
				t.Fatalf("accountMismatchesConnected(%q, %q) = %v, want %v", tc.configured, tc.connected, got, tc.want)
			}
		})
	}
}

func TestTradingStatusBlocksClientIDMismatch(t *testing.T) {
	t.Parallel()
	port := 4002
	clientID := 31
	srv := &Server{cfg: &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
		Trading: config.Trading{Enabled: true, Mode: config.TradingModePaper}.WithDefaults(),
	}}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 32, Account: "DU1234567", PortOrigin: discover.OriginPinned})

	if !hasTradingBlocker(st, "gateway_client_id_mismatch") {
		t.Fatalf("missing client-id mismatch blocker in %+v", st.Blockers)
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
	if st.CanTransmit != orderWritesAvailable || st.CanModify != orderWritesAvailable || st.CanCancel != orderWritesAvailable {
		t.Fatalf("write capabilities mismatch build mode: %+v", st)
	}
}

func TestTradingStatusPrefersPinnedAccountOverEndpointAggregate(t *testing.T) {
	t.Parallel()
	port := 7497
	clientID := 31
	srv := &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: &clientID, Account: "DU1234567"},
			Trading: config.Trading{Enabled: true, Mode: config.TradingModePaper}.WithDefaults(),
		},
		orderJournal: newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl")),
	}
	st := srv.tradingStatus(discover.Endpoint{Host: "127.0.0.1", Port: 7497, ClientID: 31, Account: "All", PortOrigin: discover.OriginPinned})

	if st.Blocked {
		t.Fatalf("paper status should be ready, got blockers %+v", st.Blockers)
	}
	if st.Account != "DU1234567" {
		t.Fatalf("Account = %q, want pinned concrete account", st.Account)
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
		t.Fatalf("live-ready capabilities should remain preview-only in this release: %+v", st)
	}
}

func hasTradingBlocker(st rpc.TradingStatus, code string) bool {
	return slices.ContainsFunc(st.Blockers, func(b rpc.TradingBlocker) bool {
		return b.Code == code
	})
}
