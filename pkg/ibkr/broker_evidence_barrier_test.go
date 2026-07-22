package ibkr

import (
	"errors"
	"testing"
	"time"
)

func readyBrokerEvidenceTestConnector(t *testing.T) *Connector {
	t.Helper()
	connector := NewConnector(&ConnectorConfig{})
	t.Cleanup(func() { connector.conn.rateLimiter.Stop() })
	connector.conn.setStatus(StatusConnected)
	connector.conn.resetOrderIDReadiness()
	connector.mu.Lock()
	connector.ready = true
	connector.mu.Unlock()
	return connector
}

func assertBrokerEvidenceMutationBlocked(t *testing.T, connector *Connector, mutate func(), assertAfter func()) {
	t.Helper()
	binding, ok := connector.CaptureBrokerEvidence()
	if !ok {
		t.Fatal("ready connector did not produce broker evidence binding")
	}
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	commitDone := make(chan bool, 1)
	go func() {
		commitDone <- connector.WithStableBrokerEvidence(binding, func() bool {
			close(commitEntered)
			<-releaseCommit
			return true
		})
	}()
	<-commitEntered
	mutationDone := make(chan struct{})
	go func() {
		mutate()
		close(mutationDone)
	}()
	select {
	case <-mutationDone:
		t.Fatal("broker evidence mutation crossed an in-progress stable commit")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseCommit)
	if committed := <-commitDone; !committed {
		t.Fatal("exact broker evidence binding did not commit")
	}
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("broker evidence mutation remained blocked after commit")
	}
	assertAfter()
}

func TestBrokerEvidenceBarrierBlocksLifecyclePortfolioAndSessionMutationAtCommit(t *testing.T) {
	connector := readyBrokerEvidenceTestConnector(t)

	orderGeneration := connector.OrderLifecycleGeneration()
	assertBrokerEvidenceMutationBlocked(t, connector, func() {
		connector.dispatchOrderLifecycle(OrderLifecycleEvent{Type: OrderLifecycleEventStatus, OrderID: 101, Status: "Submitted"})
	}, func() {
		if got := connector.OrderLifecycleGeneration(); got != orderGeneration+1 {
			t.Fatalf("order lifecycle generation=%d, want %d", got, orderGeneration+1)
		}
	})

	portfolioGeneration := connector.PortfolioProjectionGeneration()
	assertBrokerEvidenceMutationBlocked(t, connector, func() {
		connector.conn.handlePortfolioValue([]string{
			"7", "8", "265598", "AAA", "STK", "", "0", "", "1",
			"NASDAQ", "USD", "AAA", "AAA", "10", "24", "240", "25", "0", "0", "DU123",
		})
	}, func() {
		if got := connector.PortfolioProjectionGeneration(); got != portfolioGeneration+1 {
			t.Fatalf("portfolio projection generation=%d, want %d", got, portfolioGeneration+1)
		}
	})

	published := false
	assertBrokerEvidenceMutationBlocked(t, connector, func() {
		connector.WithBrokerEvidenceMutation(func() { published = true })
	}, func() {
		if !published {
			t.Fatal("external connector publication mutation did not run")
		}
	})

	session, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("ready connector did not produce session binding")
	}
	assertBrokerEvidenceMutationBlocked(t, connector, connector.conn.resetOrderIDReadiness, func() {
		if connector.SessionCurrent(session) {
			t.Fatal("socket epoch reset did not invalidate prior session")
		}
	})

	managedConnector := readyBrokerEvidenceTestConnector(t)
	assertBrokerEvidenceMutationBlocked(t, managedConnector, func() {
		managedConnector.conn.processMessage(managedConnector.conn.encodeMsg(msgManagedAccts, "1", "DU-MANAGED"))
	}, func() {
		if got := managedConnector.AccountID(); got != "DU-MANAGED" {
			t.Fatalf("managed-account mutation = %q, want DU-MANAGED", got)
		}
	})

	summaryConnector := readyBrokerEvidenceTestConnector(t)
	assertBrokerEvidenceMutationBlocked(t, summaryConnector, func() {
		summaryConnector.conn.handleAccountSummary([]string{"63", "2", "7", "DU-SUMMARY", "NetLiquidation", "100000", "USD"})
	}, func() {
		if got := summaryConnector.AccountID(); got != "DU-SUMMARY" {
			t.Fatalf("account-summary seed mutation = %q, want DU-SUMMARY", got)
		}
	})
}

func TestBrokerEvidenceBarrierRejectsDriftBeforeCommit(t *testing.T) {
	connector := readyBrokerEvidenceTestConnector(t)
	binding, ok := connector.CaptureBrokerEvidence()
	if !ok {
		t.Fatal("ready connector did not produce broker evidence binding")
	}
	connector.dispatchOrderLifecycle(OrderLifecycleEvent{Type: OrderLifecycleEventStatus, OrderID: 101, Status: "Submitted"})
	called := false
	if connector.WithStableBrokerEvidence(binding, func() bool { called = true; return true }) || called {
		t.Fatal("drifted order frontier reached stable commit")
	}
}

func TestBoundBrokerSessionDoesNotBlockPublicationWhileOperationWaits(t *testing.T) {
	connector := readyBrokerEvidenceTestConnector(t)
	session, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("ready connector did not produce session binding")
	}

	operationEntered := make(chan struct{})
	releaseOperation := make(chan struct{})
	operationDone := make(chan error, 1)
	go func() {
		ran, err := connector.WithBoundBrokerSession(session, func() error {
			close(operationEntered)
			<-releaseOperation
			if connector.SessionCurrent(session) {
				return errors.New("session invalidation did not reach bound operation")
			}
			return nil
		})
		if !ran && err == nil {
			err = ErrIBKRUnavailable
		}
		operationDone <- err
	}()
	<-operationEntered

	publicationDone := make(chan struct{})
	go func() {
		connector.WithBrokerEvidenceMutation(func() {})
		close(publicationDone)
	}()
	select {
	case <-publicationDone:
	case <-time.After(time.Second):
		t.Fatal("publication deadlocked behind non-wire bound operation")
	}

	resetDone := make(chan struct{})
	go func() {
		connector.conn.resetOrderIDReadiness()
		close(resetDone)
	}()
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("read-side session invalidation deadlocked behind bound operation")
	}
	close(releaseOperation)
	if err := <-operationDone; err != nil {
		t.Fatalf("bound operation completion: %v", err)
	}
}

func TestBrokerEvidenceMutationDrainsInFlightLifecycleCallback(t *testing.T) {
	connector := readyBrokerEvidenceTestConnector(t)
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	connector.RegisterOrderLifecycleHandler(func(OrderLifecycleEvent) {
		close(handlerEntered)
		<-releaseHandler
	})
	dispatchDone := make(chan struct{})
	go func() {
		connector.dispatchOrderLifecycle(OrderLifecycleEvent{Type: OrderLifecycleEventStatus, OrderID: 77})
		close(dispatchDone)
	}()
	<-handlerEntered

	publicationDone := make(chan struct{})
	go func() {
		connector.WithBrokerEvidenceMutation(func() {})
		close(publicationDone)
	}()
	select {
	case <-publicationDone:
		t.Fatal("publication did not drain the in-flight lifecycle callback")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseHandler)
	select {
	case <-dispatchDone:
	case <-time.After(time.Second):
		t.Fatal("lifecycle callback did not finish")
	}
	select {
	case <-publicationDone:
	case <-time.After(time.Second):
		t.Fatal("publication remained blocked after lifecycle callback finished")
	}
}

func TestBrokerScopeFramesDoNotHoldInboundLeaseWhileWaitingForPublication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message []byte
		assert  func(*testing.T, *Connection)
	}{
		{
			name:    "managed_accounts",
			message: readyBrokerEvidenceTestConnector(t).conn.encodeMsg(msgManagedAccts, "1", "DU-RETIRED"),
			assert: func(t *testing.T, conn *Connection) {
				if got := conn.GetAccountCode(); got != "" {
					t.Fatalf("retired managed account mutated successor state: %q", got)
				}
			},
		},
		{
			name:    "account_summary",
			message: readyBrokerEvidenceTestConnector(t).conn.encodeMsg(msgAccountSummary, "2", 7, "DU-RETIRED", "NetLiquidation", "1", "USD"),
			assert: func(t *testing.T, conn *Connection) {
				if got := conn.GetAccountSummary(); len(got) != 0 {
					t.Fatalf("retired account summary mutated successor state: %+v", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connector := readyBrokerEvidenceTestConnector(t)
			conn := connector.conn
			epoch := conn.BrokerSessionEpoch()
			connector.publicationBarrier.RLock()
			frameDone := make(chan struct{})
			go func() {
				conn.processMessageAtEpoch(tc.message, epoch)
				close(frameDone)
			}()
			// Give the scope frame a chance to queue for publication W. It must
			// do so before taking inboundEpochMu.R, or rollover cannot advance.
			time.Sleep(25 * time.Millisecond)
			resetDone := make(chan struct{})
			go func() {
				conn.resetOrderIDReadiness()
				close(resetDone)
			}()
			select {
			case <-resetDone:
			case <-time.After(time.Second):
				connector.publicationBarrier.RUnlock()
				t.Fatal("socket rollover deadlocked behind publication-blocked scope frame")
			}
			connector.publicationBarrier.RUnlock()
			select {
			case <-frameDone:
			case <-time.After(time.Second):
				t.Fatal("retired scope frame did not finish after publication released")
			}
			tc.assert(t, conn)
		})
	}
}
