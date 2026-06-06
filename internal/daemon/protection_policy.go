package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/ibkr/internal/rpc"
)

const protectionPolicyKind = "ibkr.protection_policy"

type protectionPolicy struct {
	Kind          string `toml:"kind" json:"kind"`
	SchemaVersion int    `toml:"schema_version" json:"schema_version"`
	PolicyID      string `toml:"policy_id" json:"policy_id"`
	PolicyVersion int    `toml:"policy_version" json:"policy_version"`
	Profile       string `toml:"profile" json:"profile"`

	Authority protectionPolicyAuthority `toml:"authority" json:"authority"`
	Buckets   protectionPolicyBuckets   `toml:"buckets" json:"buckets"`
}

type protectionPolicyAuthority struct {
	PaperOnly       bool `toml:"paper_only" json:"paper_only"`
	CloseReduceOnly bool `toml:"close_reduce_only" json:"close_reduce_only"`
	AutoSubmit      bool `toml:"auto_submit" json:"auto_submit"`
}

type protectionPolicyBuckets struct {
	ThetaHygiene  protectionThetaPolicy `toml:"theta_hygiene" json:"theta_hygiene"`
	RiskReduction protectionRiskPolicy  `toml:"risk_reduction" json:"risk_reduction"`
}

type protectionThetaPolicy struct {
	Enabled           bool    `toml:"enabled" json:"enabled"`
	MaxDTE            int     `toml:"max_dte" json:"max_dte"`
	MinAbsThetaPerDay float64 `toml:"min_abs_theta_per_day" json:"min_abs_theta_per_day"`
	MaxSpreadPctOfMid float64 `toml:"max_spread_pct_of_mid" json:"max_spread_pct_of_mid"`
}

type protectionRiskPolicy struct {
	Enabled                bool    `toml:"enabled" json:"enabled"`
	SingleNameTargetPctNLV float64 `toml:"single_name_target_pct_nlv" json:"single_name_target_pct_nlv"`
	MaxOrderNotional       float64 `toml:"max_order_notional" json:"max_order_notional"`
}

type protectionPolicyManager struct {
	mu              sync.Mutex
	path            string
	hotReload       bool
	reloadInterval  time.Duration
	now             func() time.Time
	active          protectionPolicy
	status          rpc.ProtectionPolicyStatus
	lastFingerprint rpc.Fingerprint
}

func (s *Server) installProtectionPolicyManager() {
	if s == nil || s.cfg == nil {
		return
	}
	cfg := s.cfg.AutoTrade.WithDefaults()
	pm := newProtectionPolicyManager(cfg.PolicyFile, cfg.HotReloadEnabled(), cfg.ReloadIntervalDuration(), s.now)
	pm.reload()
	s.protectionPolicies = pm
}

func newProtectionPolicyManager(path string, hotReload bool, interval time.Duration, now func() time.Time) *protectionPolicyManager {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &protectionPolicyManager{
		path:           expandUserPath(strings.TrimSpace(path)),
		hotReload:      hotReload,
		reloadInterval: interval,
		now:            now,
	}
}

func (m *protectionPolicyManager) Run(ctx context.Context, logf func(string, ...any)) {
	if m == nil || !m.hotReload {
		return
	}
	t := time.NewTicker(m.reloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			before := m.Status()
			m.reload()
			after := m.Status()
			if logf != nil && before.Status != after.Status {
				logf("protection policy status changed: %s -> %s", before.Status, after.Status)
			}
		}
	}
}

func (m *protectionPolicyManager) Active() (protectionPolicy, rpc.ProtectionPolicyStatus) {
	if m == nil {
		p := defaultProtectionPolicy()
		return p, protectionPolicyStatus(p, rpc.ProtectionPolicyStatusDefault, "", "", time.Now().UTC())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active, m.status
}

func (m *protectionPolicyManager) Status() rpc.ProtectionPolicyStatus {
	_, st := m.Active()
	return st
}

func (m *protectionPolicyManager) reload() {
	if m == nil {
		return
	}
	now := time.Now().UTC()
	if m.now != nil {
		now = m.now().UTC()
	}
	policy, source, err := m.loadPolicy()
	if err != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active.PolicyID == "" {
			m.active = defaultProtectionPolicy()
		}
		st := protectionPolicyStatus(m.active, rpc.ProtectionPolicyStatusError, source, err.Error(), now)
		st.Path = m.path
		m.status = st
		return
	}
	fp := fingerprintProtectionPolicy(policy)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active.PolicyID == "" {
		m.active = policy
		statusKind := rpc.ProtectionPolicyStatusActive
		if source == "embedded-default" {
			statusKind = rpc.ProtectionPolicyStatusDefault
		}
		st := protectionPolicyStatus(policy, statusKind, source, "", now)
		st.Path = m.path
		m.status = st
		m.lastFingerprint = fp
		return
	}

	switch {
	case policy.PolicyVersion > m.active.PolicyVersion:
		m.active = policy
		st := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusActive, source, "", now)
		st.Path = m.path
		m.status = st
		m.lastFingerprint = fp
	case policy.PolicyVersion == m.active.PolicyVersion && fp.Key == m.lastFingerprint.Key:
		st := protectionPolicyStatus(m.active, m.status.Status, source, "", now)
		if st.Status == "" || st.Status == rpc.ProtectionPolicyStatusDrift || st.Status == rpc.ProtectionPolicyStatusError {
			st.Status = rpc.ProtectionPolicyStatusActive
		}
		st.Path = m.path
		m.status = st
	case policy.PolicyVersion <= m.active.PolicyVersion && fp.Key != m.lastFingerprint.Key:
		st := protectionPolicyStatus(m.active, rpc.ProtectionPolicyStatusDrift, source, "policy file changed without a higher policy_version", now)
		st.Path = m.path
		m.status = st
	}
}

func (m *protectionPolicyManager) loadPolicy() (protectionPolicy, string, error) {
	if m == nil || strings.TrimSpace(m.path) == "" {
		p := defaultProtectionPolicy()
		return p, "embedded-default", validateProtectionPolicy(p)
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p := defaultProtectionPolicy()
			return p, "embedded-default", validateProtectionPolicy(p)
		}
		return protectionPolicy{}, "file", fmt.Errorf("read protection policy %s: %w", m.path, err)
	}
	var p protectionPolicy
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return protectionPolicy{}, "file", fmt.Errorf("parse protection policy %s: %w", m.path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return protectionPolicy{}, "file", fmt.Errorf("unknown protection policy key(s): %s", strings.Join(keys, ", "))
	}
	applyProtectionPolicyDefaults(&p)
	if err := validateProtectionPolicy(p); err != nil {
		return protectionPolicy{}, "file", err
	}
	return p, "file", nil
}

func defaultProtectionPolicy() protectionPolicy {
	return protectionPolicy{
		Kind:          protectionPolicyKind,
		SchemaVersion: 1,
		PolicyID:      "protection-mvp",
		PolicyVersion: 1,
		Profile:       "theta-priority-mvp",
		Authority: protectionPolicyAuthority{
			PaperOnly:       true,
			CloseReduceOnly: true,
			AutoSubmit:      false,
		},
		Buckets: protectionPolicyBuckets{
			ThetaHygiene: protectionThetaPolicy{
				Enabled:           true,
				MaxDTE:            21,
				MinAbsThetaPerDay: 5.0,
				MaxSpreadPctOfMid: 25.0,
			},
			RiskReduction: protectionRiskPolicy{
				Enabled:                true,
				SingleNameTargetPctNLV: 25.0,
				MaxOrderNotional:       10000.0,
			},
		},
	}
}

func applyProtectionPolicyDefaults(p *protectionPolicy) {
	if p == nil {
		return
	}
	if p.Kind == "" {
		p.Kind = protectionPolicyKind
	}
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	if p.Profile == "" {
		p.Profile = p.PolicyID
	}
}

func validateProtectionPolicy(p protectionPolicy) error {
	if p.Kind != protectionPolicyKind {
		return fmt.Errorf("protection policy kind %q is invalid", p.Kind)
	}
	if p.SchemaVersion != 1 {
		return fmt.Errorf("protection policy schema_version %d is unsupported", p.SchemaVersion)
	}
	if strings.TrimSpace(p.PolicyID) == "" {
		return fmt.Errorf("protection policy policy_id is required")
	}
	if p.PolicyVersion <= 0 {
		return fmt.Errorf("protection policy policy_version must be positive")
	}
	if !p.Authority.PaperOnly {
		return fmt.Errorf("protection policy authority.paper_only must be true in MVP")
	}
	if !p.Authority.CloseReduceOnly {
		return fmt.Errorf("protection policy authority.close_reduce_only must be true in MVP")
	}
	if p.Authority.AutoSubmit {
		return fmt.Errorf("protection policy authority.auto_submit must be false in MVP")
	}
	if p.Buckets.ThetaHygiene.Enabled {
		if p.Buckets.ThetaHygiene.MaxDTE <= 0 {
			return fmt.Errorf("theta_hygiene.max_dte must be positive")
		}
		if p.Buckets.ThetaHygiene.MinAbsThetaPerDay <= 0 {
			return fmt.Errorf("theta_hygiene.min_abs_theta_per_day must be positive")
		}
		if p.Buckets.ThetaHygiene.MaxSpreadPctOfMid <= 0 {
			return fmt.Errorf("theta_hygiene.max_spread_pct_of_mid must be positive")
		}
	}
	if p.Buckets.RiskReduction.Enabled {
		if p.Buckets.RiskReduction.SingleNameTargetPctNLV <= 0 {
			return fmt.Errorf("risk_reduction.single_name_target_pct_nlv must be positive")
		}
		if p.Buckets.RiskReduction.MaxOrderNotional <= 0 {
			return fmt.Errorf("risk_reduction.max_order_notional must be positive")
		}
	}
	return nil
}

func protectionPolicyStatus(p protectionPolicy, status, source, message string, at time.Time) rpc.ProtectionPolicyStatus {
	fp := fingerprintProtectionPolicy(p)
	st := rpc.ProtectionPolicyStatus{
		Kind:          protectionPolicyKind,
		Status:        status,
		PolicyID:      p.PolicyID,
		PolicyVersion: p.PolicyVersion,
		Profile:       p.Profile,
		Fingerprint:   fp,
		Source:        source,
		LoadedAt:      at,
		LastCheckedAt: at,
		Message:       message,
	}
	if status == rpc.ProtectionPolicyStatusDrift || status == rpc.ProtectionPolicyStatusError {
		st.Blockers = []rpc.TradingBlocker{{
			Code:    "policy_" + status,
			Message: nonEmptyString(message, "protection policy is not safe for writes"),
			Action:  "Fix the protection policy file and bump policy_version before preview or submit.",
		}}
	}
	return st
}

func fingerprintProtectionPolicy(p protectionPolicy) rpc.Fingerprint {
	normalized := struct {
		Kind          string                    `json:"kind"`
		SchemaVersion int                       `json:"schema_version"`
		PolicyID      string                    `json:"policy_id"`
		PolicyVersion int                       `json:"policy_version"`
		Profile       string                    `json:"profile"`
		Authority     protectionPolicyAuthority `json:"authority"`
		Buckets       protectionPolicyBuckets   `json:"buckets"`
	}{
		Kind:          strings.TrimSpace(p.Kind),
		SchemaVersion: p.SchemaVersion,
		PolicyID:      strings.TrimSpace(p.PolicyID),
		PolicyVersion: p.PolicyVersion,
		Profile:       strings.TrimSpace(p.Profile),
		Authority:     p.Authority,
		Buckets:       p.Buckets,
	}
	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:" + hex.EncodeToString(sum[:])}
}

func expandUserPath(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func nonEmptyString(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
