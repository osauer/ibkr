package ibkr

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPrewarmContractsSkipsRetryAfterLateDetail(t *testing.T) {
	conn := NewConnector(nil)
	conn.conn = NewConnection(nil)
	conn.conn.status = StatusConnected
	conn.mu.Lock()
	conn.lease = &ConnectionLease{LeaseID: "test", ClientID: 1, Active: true}
	conn.mu.Unlock()

	var (
		mu    sync.Mutex
		calls int
	)

	conn.fetchContractDetails = func(symbol string, timeout time.Duration) ([]ContractDetailsLite, error) {
		mu.Lock()
		calls++
		mu.Unlock()

		if calls == 1 {
			// Simulate late arrival: cache detail shortly after timeout trigger.
			go func() {
				time.Sleep(150 * time.Millisecond)
				conn.contractMu.Lock()
				conn.contractCache[symbol] = ContractDetailsLite{
					Symbol:      symbol,
					PrimaryExch: "ARCA",
					ConID:       756733,
					LocalSymbol: symbol,
				}
				conn.contractMu.Unlock()
			}()
			return nil, context.DeadlineExceeded
		}
		return []ContractDetailsLite{{
			Symbol:      symbol,
			PrimaryExch: "ARCA",
			ConID:       756733,
			LocalSymbol: symbol,
		}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cfg := PrewarmConfig{
		Enabled:          true,
		Timeout:          100 * time.Millisecond,
		MaxRetries:       2,
		RetryDelay:       50 * time.Millisecond,
		MaxConcurrency:   1,
		LateArrivalGrace: 500 * time.Millisecond,
	}

	if err := conn.PrewarmContracts(ctx, []string{"SPY"}, cfg); err != nil {
		t.Fatalf("PrewarmContracts returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected single contract request, got %d", calls)
	}
}
