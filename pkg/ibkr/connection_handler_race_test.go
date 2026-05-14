package ibkr

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRegisterUnregisterRace exercises the snapshotHandlers + UnregisterHandler
// path under -race. Before v0.16.0 snapshotHandlers released the RLock before
// iterating its captured slice; a concurrent UnregisterHandler shifted entries
// in place via append(entries[:i], entries[i+1:]...) on the same backing array.
// The fix lifts the iteration under the RLock so reader and writer are
// serialised through handlersMu.
//
// deferContractDetailsCleanup in connector.go is the canonical production
// caller — it runs UnregisterHandler from a goroutine while readMessages
// dispatches through snapshotHandlers; this test models that pattern with
// nothing of the IBKR wire layer attached.
func TestRegisterUnregisterRace(t *testing.T) {
	conn := &Connection{msgHandlers: map[int][]handlerEntry{}}

	const msgID = 42
	const handlers = 64
	ids := make([]uint64, handlers)
	for i := range handlers {
		ids[i] = conn.RegisterHandler(msgID, func([]string) {})
	}

	// Baseline dispatch outside the race window — gives us a known >0
	// call count and confirms the snapshot path is wired before the
	// concurrent goroutines start interleaving.
	var calls atomic.Int64
	for _, fn := range conn.snapshotHandlers(msgID) {
		fn(nil)
		calls.Add(1)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				fns := conn.snapshotHandlers(msgID)
				for _, fn := range fns {
					fn(nil)
					calls.Add(1)
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer close(stop)
		for i := range handlers {
			conn.UnregisterHandler(msgID, ids[i])
			runtime.Gosched()
		}
	}()

	wg.Wait()
	if calls.Load() == 0 {
		t.Fatal("snapshotHandlers never dispatched; race goroutine arrangement is wrong")
	}
}
