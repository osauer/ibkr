package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/config"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestAlertShadowHandlersAreScopedColdRedactedAndDeliveryInactive(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	port := 4002
	base := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	server := &Server{
		coreStore: store,
		cfg: &config.Resolved{Gateway: config.Gateway{
			Host: "127.0.0.1", Port: &port, Account: "DU-HANDLER-A",
		}},
		now: func() time.Time { return base },
	}
	if err := server.attachAlertShadowAuthority(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.alertShadow.now = server.now

	cold, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	wantA, err := rpc.BuildAlertAuthorityScope("DU-HANDLER-A", rpc.AccountModePaper)
	if err != nil {
		t.Fatal(err)
	}
	if cold.AuthorityScope != wantA || cold.CurrentState != rpc.AlertSnapshotUnknown ||
		cold.Coverage.State != rpc.AlertCoverageUnavailable || len(cold.Candidates) != 0 {
		t.Fatalf("cold scoped snapshot=%+v", cold)
	}

	scopeA, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}
	relevant := true
	result := alertShadowTestCanary(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "handler-a")
	if _, err := server.alertShadow.ObserveCanary(t.Context(), scopeA, result); err != nil {
		t.Fatal(err)
	}
	active, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if active.AuthorityScope != wantA || len(active.Candidates) != 1 || active.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("active scoped snapshot=%+v", active)
	}
	status, err := server.handleAlertShadowStatus(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Authority != alertShadowAuthority || status.DeliveryActive || len(status.ExpectedSources) != 9 {
		t.Fatalf("shadow status=%+v", status)
	}

	rawSnapshot, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	rawStatus, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{rawSnapshot, rawStatus} {
		text := string(raw)
		if strings.Contains(text, "DU-HANDLER-A") || strings.Contains(text, `"account"`) || strings.Contains(text, `"account_mode"`) {
			t.Fatalf("handler exposed raw broker scope: %s", text)
		}
	}
	if strings.Contains(string(rawStatus), "alert-authority-scope-v1:") {
		t.Fatalf("status exposed private opaque scope: %s", rawStatus)
	}

	server.cfg.Gateway.Account = "DU-HANDLER-B"
	coldB, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	wantB, err := rpc.BuildAlertAuthorityScope("DU-HANDLER-B", rpc.AccountModePaper)
	if err != nil {
		t.Fatal(err)
	}
	if coldB.AuthorityScope != wantB || coldB.AuthorityScope == wantA || len(coldB.Candidates) != 0 || coldB.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("foreign scope leaked prior candidates: %+v", coldB)
	}

	server.cfg.Gateway.Account = "DU-HANDLER-A"
	server.now = func() time.Time { return base.Add(alertShadowCanarySilenceHorizon + time.Nanosecond) }
	server.alertShadow.now = server.now
	stale, err := server.handleAlertCandidates(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if stale.AuthorityScope != wantA || stale.Coverage.Freshness != rpc.AlertCoverageStale ||
		len(stale.Candidates) != 1 || stale.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale || stale.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("silent producer did not project stale: %+v", stale)
	}
}
