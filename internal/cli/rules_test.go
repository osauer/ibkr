package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

type rulesFakeConn struct {
	result rpc.RulesResult
}

func (c *rulesFakeConn) Call(_ context.Context, method string, _ any, out any) error {
	if method != rpc.MethodRulesSnapshot {
		return nil
	}
	raw, _ := json.Marshal(c.result)
	return json.Unmarshal(raw, out)
}

func (*rulesFakeConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestRunRulesRendersTerminalContractAsExemptNotPass(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &rulesFakeConn{result: rpc.RulesResult{
		AsOf: now, Enabled: true, Status: "ok", PolicyID: "rulebook-v2", PolicyVersion: 2,
		Rules: []risk.RuleRow{{
			ID: risk.RuleCatalystCoverage, Number: 6, Title: "Option outlives its catalyst",
			Status: risk.RuleStatusNotEvaluated, Reason: risk.EarningsReasonTerminalNonReporting,
			Evidence: "1 exact terminal/non-reporting contract has no future issuer earnings catalyst.",
			Exempt:   []risk.RuleOffender{{Symbol: "EXAMPLEQ", Note: "exact contract is verified terminal/non-reporting"}},
		}},
		Ranked:       []int{0},
		BreachCounts: map[string]int{risk.RuleStatusNotEvaluated: 1},
		Earnings: []rpc.EarningsInfo{{
			Symbol: "EXAMPLEQ", Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting,
			Terminal: &rpc.EarningsTerminalInfo{RevalidateAfter: now.AddDate(1, 0, 0)},
		}},
	}}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(context.Background(), env, "rules", nil); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"--     6 catalyst_coverage",
		"exempt: EXAMPLEQ — exact contract is verified terminal/non-reporting",
		"1 not evaluated",
		"Earnings not applicable: EXAMPLEQ (terminal/non-reporting; review by 2027-07-21)",
		"exact-contract evidence and provenance are available in --json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"All 1 rules pass", "Earnings unresolved:"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("output unexpectedly contains %q:\n%s", forbidden, out)
		}
	}
}
