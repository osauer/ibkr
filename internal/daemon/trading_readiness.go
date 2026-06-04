package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	tradingReadinessFileVersion = 1

	tradingPaperSmokeStatusValid        = "valid"
	tradingPaperSmokeStatusMissing      = "missing"
	tradingPaperSmokeStatusStale        = "stale"
	tradingPaperSmokeStatusMismatch     = "mismatch"
	tradingPaperSmokeStatusFailed       = "failed"
	tradingPaperSmokeStatusUnreadable   = "unreadable"
	tradingPaperSmokeEndpointClassPaper = "paper"
	tradingPaperSmokeResultPassed       = "passed"
)

type tradingReadinessStore struct {
	Path string
}

type tradingReadinessFile struct {
	Version    int                        `json:"version"`
	PaperSmoke *tradingPaperSmokeEvidence `json:"paper_smoke,omitempty"`
}

type tradingPaperSmokeEvidence struct {
	Account       string    `json:"account"`
	Endpoint      string    `json:"endpoint"`
	EndpointClass string    `json:"endpoint_class"`
	ClientID      int       `json:"client_id"`
	Version       string    `json:"version"`
	Result        string    `json:"result"`
	At            time.Time `json:"at"`
}

type tradingPaperSmokeCheck struct {
	Status   string
	Message  string
	Action   string
	Evidence *tradingPaperSmokeEvidence
}

func defaultTradingReadinessPath() (string, error) {
	return defaultTradingStatePath("trading-readiness.json")
}

func newTradingReadinessStore(path string) *tradingReadinessStore {
	return &tradingReadinessStore{Path: path}
}

func (s *tradingReadinessStore) Load() (*tradingReadinessFile, error) {
	if s == nil || s.Path == "" {
		return &tradingReadinessFile{Version: tradingReadinessFileVersion}, nil
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return &tradingReadinessFile{Version: tradingReadinessFileVersion}, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.Path, err)
	}
	var f tradingReadinessFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	if f.Version == 0 {
		f.Version = tradingReadinessFileVersion
	}
	if f.Version != tradingReadinessFileVersion {
		return nil, fmt.Errorf("unsupported trading-readiness version %d", f.Version)
	}
	return &f, nil
}

func (s *tradingReadinessStore) SavePaperSmoke(ev tradingPaperSmokeEvidence) error {
	if s == nil || s.Path == "" {
		return fmt.Errorf("trading readiness path is empty")
	}
	f := tradingReadinessFile{
		Version:    tradingReadinessFileVersion,
		PaperSmoke: &ev,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trading readiness: %w", err)
	}
	data = append(data, '\n')
	return writePrivateStateAtomic(s.Path, data)
}

func (s *tradingReadinessStore) CheckPaperSmoke(liveAccount, liveEndpoint string, clientID int, version string, maxAge time.Duration, now time.Time) tradingPaperSmokeCheck {
	if s == nil || s.Path == "" {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusMissing,
			Message: "live trading requires recent paper-smoke evidence in daemon-owned state",
			Action:  "Run `ibkr trading paper-smoke` against the pinned paper account first.",
		}
	}
	f, err := s.Load()
	if err != nil {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusUnreadable,
			Message: "paper-smoke evidence is unreadable",
			Action:  "Inspect or remove the trading-readiness state file, then rerun `ibkr trading paper-smoke`.",
		}
	}
	if f.PaperSmoke == nil {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusMissing,
			Message: "live trading requires recent paper-smoke evidence in daemon-owned state",
			Action:  "Run `ibkr trading paper-smoke` against the pinned paper account first.",
		}
	}
	ev := f.PaperSmoke
	if ev.Result != tradingPaperSmokeResultPassed {
		return tradingPaperSmokeCheck{
			Status:   tradingPaperSmokeStatusFailed,
			Message:  "last paper-smoke evidence did not pass",
			Action:   "Fix the paper order lifecycle and rerun `ibkr trading paper-smoke`.",
			Evidence: ev,
		}
	}
	if ev.EndpointClass != tradingPaperSmokeEndpointClassPaper ||
		!paperSmokeEvidenceMatchesLiveGate(*ev, liveAccount, liveEndpoint, clientID, version) {
		return tradingPaperSmokeCheck{
			Status:   tradingPaperSmokeStatusMismatch,
			Message:  "paper-smoke evidence does not match the live account family, pinned host, client ID, binary version, or paper endpoint requirement",
			Action:   "Run `ibkr trading paper-smoke` on the paper gateway that pairs with this live setup before enabling live.",
			Evidence: ev,
		}
	}
	if ev.At.IsZero() || now.Sub(ev.At) > maxAge || ev.At.After(now.Add(5*time.Minute)) {
		return tradingPaperSmokeCheck{
			Status:   tradingPaperSmokeStatusStale,
			Message:  "paper-smoke evidence is stale",
			Action:   "Rerun `ibkr trading paper-smoke` before enabling live.",
			Evidence: ev,
		}
	}
	return tradingPaperSmokeCheck{
		Status:   tradingPaperSmokeStatusValid,
		Message:  "paper-smoke evidence is fresh and matches the live gate",
		Evidence: ev,
	}
}

func paperSmokeEvidenceMatchesLiveGate(ev tradingPaperSmokeEvidence, liveAccount, liveEndpoint string, clientID int, version string) bool {
	if ev.ClientID != clientID {
		return false
	}
	if version != "" && ev.Version != version {
		return false
	}
	if !looksPaperEndpoint(ev.Endpoint, ev.Account) {
		return false
	}
	if accountFamilyKey(ev.Account) != accountFamilyKey(liveAccount) {
		return false
	}
	return endpointHost(ev.Endpoint) == endpointHost(liveEndpoint)
}

func looksPaperEndpoint(endpoint, account string) bool {
	host, port, err := net.SplitHostPort(endpoint)
	if err == nil && host != "" {
		switch port {
		case "4002", "7497":
			return true
		}
	}
	return looksPaper(0, account)
}

func endpointHost(endpoint string) string {
	host, _, err := net.SplitHostPort(endpoint)
	if err == nil {
		return host
	}
	return endpoint
}

func accountFamilyKey(account string) string {
	account = strings.ToUpper(strings.TrimSpace(account))
	if after, ok := strings.CutPrefix(account, "DU"); ok {
		return "U" + after
	}
	return account
}
