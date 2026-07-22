package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestRegimeSnapshotCacheRefreshesAheadWithoutChangingHardCeiling(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "first"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	cache.mu.Lock()
	firstCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	refresh := func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return regimeSnapshotCacheFixture(clock.Now(), "second"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}

	clock.Set(firstCommit.Add(39 * time.Second))
	if cache.startRefreshAhead(refresh, 20*time.Second) {
		t.Fatal("refresh started before the ahead window")
	}
	clock.Set(firstCommit.Add(40 * time.Second))
	if !cache.startRefreshAhead(refresh, 20*time.Second) {
		t.Fatal("refresh did not start at the ahead boundary")
	}
	<-started
	beforeDeadline, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if beforeDeadline.Health.Status != rpc.RegimeAuthorityFresh || !beforeDeadline.Health.Refreshing {
		t.Fatalf("early refresh health=%+v, want fresh and refreshing", beforeDeadline.Health)
	}

	clock.Set(firstCommit.Add(time.Minute))
	atDeadline, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if atDeadline.Health.Status != rpc.RegimeAuthorityStale || !atDeadline.Health.Refreshing {
		t.Fatalf("hard ceiling health=%+v, want stale and refreshing", atDeadline.Health)
	}

	close(release)
	cache.wait()
	cache.mu.Lock()
	secondCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	clock.Set(secondCommit)
	recovered, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || recovered.Revision != 2 || recovered.Health.Status != rpc.RegimeAuthorityFresh || recovered.Snapshot.Summary.Label != "second" {
		t.Fatalf("recovered view=%+v calls=%d", recovered, calls.Load())
	}
}

func TestRegimeSnapshotCacheEarlyFailureRetriesWithinFreshnessWindow(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
		FreshFor:          regimeSnapshotFreshFor,
		RefreshTimeout:    regimeSnapshotRefreshTimeout,
		FailureRetryAfter: regimeSnapshotFailureRetry,
		Now:               clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "first"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	firstCommit := cache.lastSuccessAt
	cache.mu.Unlock()

	firstAttempt := firstCommit.Add(regimeSnapshotFreshFor - regimeSnapshotRefreshAhead)
	clock.Set(firstAttempt)
	if !cache.startRefreshAhead(func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return nil, false, nil, errors.New("temporary source failure")
	}, regimeSnapshotRefreshAhead) {
		t.Fatal("early failure attempt did not start")
	}
	warmFailed := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return !view.Health.Refreshing && view.Health.FailureCode == rpc.RegimeAuthorityFailureRefreshFailed
	})
	if warmFailed.Health.Status != rpc.RegimeAuthorityFresh {
		t.Fatalf("early failure prematurely made authority stale: %+v", warmFailed.Health)
	}

	clock.Set(firstAttempt.Add(regimeSnapshotFailureRetry - time.Nanosecond))
	if cache.startRefreshAhead(func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "too early"), true, nil, nil
	}, regimeSnapshotRefreshAhead) {
		t.Fatal("failed refresh retried before the bounded quiet period")
	}

	clock.Set(firstAttempt.Add(regimeSnapshotFailureRetry))
	if !cache.startRefreshAhead(func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "recovered"), true, nil, nil
	}, regimeSnapshotRefreshAhead) {
		t.Fatal("failed refresh did not retry while last-good was still fresh")
	}
	cache.wait()
	cache.mu.Lock()
	secondCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	clock.Set(secondCommit)
	recovered, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != 2 || recovered.Health.Status != rpc.RegimeAuthorityFresh || recovered.Snapshot.Summary.Label != "recovered" {
		t.Fatalf("early retry did not recover authority: %+v", recovered)
	}
}

func TestRegimeRefreshLoopRunsWithoutAlertOrApp(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "first"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	firstCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	clock.Set(firstCommit.Add(40 * time.Second))

	loopContext, cancelLoop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer close(loopDone)
		runRegimeRefreshLoop(
			loopContext,
			cache,
			time.Hour,
			20*time.Second,
			func() bool { return true },
			func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
				close(started)
				select {
				case <-release:
					return regimeSnapshotCacheFixture(clock.Now(), "scheduled"), true, nil, nil
				case <-ctx.Done():
					return nil, false, nil, ctx.Err()
				}
			},
		)
	}()

	<-started
	close(release)
	cache.wait()
	cancelLoop()
	<-loopDone
	view, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != 2 || view.Snapshot == nil || view.Snapshot.Summary.Label != "scheduled" {
		t.Fatalf("scheduler did not publish without alert/app owners: %+v", view)
	}
}
