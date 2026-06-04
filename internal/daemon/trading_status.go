package daemon

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/discover"
	"github.com/osauer/ibkr/internal/rpc"
)

func (s *Server) handleTradingStatus() *rpc.TradingStatus {
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	return &status
}

func (s *Server) tradingStatus(ep discover.Endpoint) rpc.TradingStatus {
	var cfg config.Resolved
	if s != nil && s.cfg != nil {
		cfg = *s.cfg
	}
	tr := cfg.Trading.WithDefaults()

	host := ep.Host
	if host == "" {
		host = cfg.Gateway.HostOrDefault()
	}
	port := ep.Port
	if port == 0 && cfg.Gateway.Port != nil {
		port = *cfg.Gateway.Port
	}
	account := ep.Account
	if account == "" {
		account = cfg.Gateway.Account
	}
	clientID := ep.ClientID
	if clientID == 0 {
		clientID = cfg.Gateway.ClientIDOrDefault()
	}

	status := rpc.TradingStatus{
		Enabled:         tr.Enabled,
		Mode:            tr.Mode,
		LocalGate:       rpc.TradingLocalGateDisabled,
		BrokerGate:      rpc.BrokerTradingGateUnknown,
		GatewayHost:     host,
		GatewayPort:     port,
		Endpoint:        endpointString(host, port),
		PortOrigin:      string(ep.PortOrigin),
		Account:         account,
		AccountOrigin:   originPinnedOrAuto(cfg.Gateway.Account != ""),
		ClientID:        clientID,
		ClientIDOrigin:  originPinnedOrDefault(cfg.Gateway.ClientID != nil),
		MCPTrading:      tradingMCPStatus(tr),
		PreviewRequired: tr.PreviewRequired(),
		LiveOverride:    rpc.TradingLiveOverrideBlocked,
	}
	if status.PortOrigin == "" {
		if cfg.Gateway.Port != nil {
			status.PortOrigin = string(discover.OriginPinned)
		} else {
			status.PortOrigin = string(discover.OriginDiscovered)
		}
	}
	if !tr.Enabled {
		return status
	}

	status.LocalGate = tr.Mode
	add := func(code, message, action string) {
		status.Blockers = append(status.Blockers, rpc.TradingBlocker{
			Code:    code,
			Message: message,
			Action:  action,
		})
	}
	summary, journalErr := s.orderJournalSummary()
	if journalErr != nil {
		add("order_journal_unavailable", "order writes require a writable local order journal", "Fix the XDG state directory permissions before enabling trading.")
	} else {
		status.OpenOrders = summary.OpenOrders
		status.LastOrderEvent = summary.LastEvent
	}

	switch tr.Mode {
	case config.TradingModePaper, config.TradingModeLive:
	default:
		add("invalid_mode", fmt.Sprintf("trading mode %q is invalid", tr.Mode), "Set [trading].mode to paper or live.")
	}
	if !tr.PreviewRequired() {
		add("preview_not_required", "trading requires submit-eligible preview tokens", "Set [trading].require_preview = true.")
	}
	if cfg.Gateway.Port == nil {
		add("gateway_port_unpinned", "order submission requires a pinned gateway port", "Set [gateway].port.")
	}
	if cfg.Gateway.Account == "" {
		add("gateway_account_unpinned", "order submission requires a pinned account", "Set [gateway].account.")
	} else if strings.EqualFold(strings.TrimSpace(cfg.Gateway.Account), "All") {
		add("gateway_account_not_concrete", "order preview requires a concrete IBKR account, not the aggregate account \"All\"", "Pin the paper/live account code shown by TWS, such as a DU paper account.")
	} else if connectedAccount := s.connectedGatewayAccount(); connectedAccount != "" && !strings.EqualFold(cfg.Gateway.Account, connectedAccount) {
		add("gateway_account_mismatch", fmt.Sprintf("configured account %q does not match connected account %q", cfg.Gateway.Account, connectedAccount), "Pin [gateway].account to the account advertised by the connected TWS/Gateway session.")
	}
	if cfg.Gateway.ClientID == nil {
		add("gateway_client_id_unpinned", "order submission requires a pinned client ID", "Set [gateway].client_id.")
	} else if ep.ClientID != 0 && ep.ClientID != *cfg.Gateway.ClientID {
		add("gateway_client_id_autowalked", "connected client ID differs from the pinned client ID", "Stop the conflicting API client or choose a free [gateway].client_id.")
	}

	switch tr.Mode {
	case config.TradingModePaper:
		if cfg.Gateway.Port != nil && cfg.Gateway.Account != "" && !looksPaper(port, account) {
			add("paper_endpoint_unconfirmed", "paper trading requires a paper-looking endpoint or account", "Use port 4002/7497 or a DU paper account.")
		}
	case config.TradingModeLive:
		if cfg.Gateway.Port != nil && cfg.Gateway.Account != "" && looksPaper(port, account) {
			add("live_endpoint_unconfirmed", "live trading requires a live-looking endpoint and account", "Use port 4001/7496 and a live account, then update [trading].live_ack_endpoint and live_ack_account.")
		}
		if !tr.AllowLive {
			add("live_not_allowed", "live trading requires an explicit local override", "Set [trading].allow_live = true.")
		}
		if tr.LiveAckAccount == "" || tr.LiveAckAccount != account {
			add("live_account_ack_mismatch", "live trading requires a matching account acknowledgement", "Set [trading].live_ack_account to the pinned live account.")
		}
		if tr.LiveAckEndpoint == "" || tr.LiveAckEndpoint != status.Endpoint {
			add("live_endpoint_ack_mismatch", "live trading requires a matching endpoint acknowledgement", "Set [trading].live_ack_endpoint to the pinned live endpoint.")
		}
		check := s.checkPaperSmoke(account, status.Endpoint, clientID, tr.PaperSmokeMaxAgeDuration())
		status.PaperSmoke = check.Status
		status.PaperSmokeMaxAge = tr.PaperSmokeMaxAgeDuration().String()
		if check.Evidence != nil {
			at := check.Evidence.At
			status.PaperSmokeAt = &at
			status.PaperSmokeAccount = check.Evidence.Account
			status.PaperSmokeEndpoint = check.Evidence.Endpoint
			status.PaperSmokeClientID = check.Evidence.ClientID
			status.PaperSmokeVersion = check.Evidence.Version
		}
		if check.Status == tradingPaperSmokeStatusValid {
			status.BrokerGate = rpc.BrokerTradingGatePaperSmokePassed
		} else {
			add(paperSmokeBlockerCode(check.Status), check.Message, check.Action)
		}
	}

	status.Blocked = len(status.Blockers) > 0
	status.CanPreview = tr.Enabled && !status.Blocked && tr.PreviewRequired()
	status.CanTransmit = false
	status.CanModify = false
	status.CanCancel = false
	if tr.Mode == config.TradingModeLive && !status.Blocked {
		status.LiveOverride = rpc.TradingLiveOverrideReady
	}
	return status
}

func (s *Server) connectedGatewayAccount() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	if c == nil || !c.IsReady() {
		return ""
	}
	return strings.TrimSpace(c.AccountID())
}

func endpointString(host string, port int) string {
	if host == "" {
		return ""
	}
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func originPinnedOrAuto(pinned bool) string {
	if pinned {
		return string(discover.OriginPinned)
	}
	return "auto"
}

func originPinnedOrDefault(pinned bool) string {
	if pinned {
		return string(discover.OriginPinned)
	}
	return string(discover.OriginDefault)
}

func looksPaper(port int, account string) bool {
	if port == 4002 || port == 7497 {
		return true
	}
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(account)), "DU")
}

func accountModeForStatus(port int, account string) string {
	if looksPaper(port, account) {
		return rpc.AccountModePaper
	}
	account = strings.TrimSpace(account)
	if account != "" && !strings.EqualFold(account, "All") {
		return rpc.AccountModeLive
	}
	switch port {
	case 4001, 7496:
		return rpc.AccountModeLive
	default:
		return rpc.AccountModeUnknown
	}
}

func tradingMCPStatus(tr config.Trading) string {
	if !tr.MCPEnabled {
		return rpc.TradingMCPDisabled
	}
	switch tr.MCPMode {
	case config.MCPModePaperWrite:
		return rpc.TradingMCPPaper
	case config.MCPModeLiveWrite:
		return rpc.TradingMCPLive
	default:
		return rpc.TradingMCPPreview
	}
}

func (s *Server) checkPaperSmoke(account, endpoint string, clientID int, maxAge time.Duration) tradingPaperSmokeCheck {
	if s == nil {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusMissing,
			Message: "live trading requires recent paper-smoke evidence in daemon-owned state",
			Action:  "Run `ibkr trading paper-smoke` against the pinned paper account first.",
		}
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	return s.tradingReadiness.CheckPaperSmoke(account, endpoint, clientID, s.version, maxAge, now)
}

func (s *Server) orderJournalSummary() (orderJournalSummary, error) {
	if s == nil || s.orderJournal == nil {
		return orderJournalSummary{}, fmt.Errorf("order journal is not configured")
	}
	return s.orderJournal.Summary()
}

func paperSmokeBlockerCode(status string) string {
	switch status {
	case tradingPaperSmokeStatusStale:
		return "paper_smoke_stale"
	case tradingPaperSmokeStatusMismatch:
		return "paper_smoke_mismatch"
	case tradingPaperSmokeStatusFailed:
		return "paper_smoke_failed"
	case tradingPaperSmokeStatusUnreadable:
		return "paper_smoke_unreadable"
	default:
		return "paper_smoke_missing"
	}
}
