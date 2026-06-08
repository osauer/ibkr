package daemon

import (
	"strings"

	"github.com/osauer/ibkr/internal/rpc"
)

type brokerStateScope struct {
	Account string
	Mode    string
}

func (s *Server) currentBrokerStateScope() brokerStateScope {
	if s == nil {
		return brokerStateScope{}
	}
	s.mu.Lock()
	ep := s.endpoint
	c := s.connector
	s.mu.Unlock()

	account := strings.TrimSpace(ep.Account)
	if c != nil {
		if connected := strings.TrimSpace(c.AccountID()); connected != "" {
			account = connected
		}
	}
	return brokerStateScope{
		Account: account,
		Mode:    accountModeForStatus(ep.Port, account),
	}
}

func brokerScopeAccountConcrete(account string) bool {
	account = strings.TrimSpace(account)
	return account != "" && !strings.EqualFold(account, "All")
}

func brokerScopedModeMatches(rowMode, scopeMode string) bool {
	scopeMode = strings.TrimSpace(scopeMode)
	if scopeMode == "" || scopeMode == rpc.AccountModeUnknown {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(rowMode), scopeMode)
}

func brokerScopeIsUnfiltered(scope brokerStateScope) bool {
	return strings.TrimSpace(scope.Account) == "" && strings.TrimSpace(scope.Mode) == ""
}

func brokerScopedAccountMatches(rowAccount string, scope brokerStateScope) bool {
	if brokerScopeIsUnfiltered(scope) {
		return true
	}
	if !brokerScopeAccountConcrete(scope.Account) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(rowAccount), scope.Account)
}

func orderViewMatchesBrokerScope(view rpc.OrderView, scope brokerStateScope) bool {
	return brokerScopedAccountMatches(view.Account, scope) &&
		brokerScopedModeMatches(view.Mode, scope.Mode)
}

func purgeLedgerRowMatchesBrokerScope(row purgeLedgerRow, scope brokerStateScope) bool {
	return brokerScopedAccountMatches(row.Account, scope) &&
		brokerScopedModeMatches(row.Mode, scope.Mode)
}
