package spx

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFetcher returns a canned response. Counter pins the
// singleflight invariant: concurrent triggers MUST result in exactly
// one underlying fetch.
type fakeFetcher struct {
	members []string
	asOf    time.Time
	err     error
	calls   atomic.Int64
	delay   time.Duration
}

func (f *fakeFetcher) fn(_ context.Context) ([]string, time.Time, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, time.Time{}, f.err
	}
	return slices.Clone(f.members), f.asOf, nil
}

// freshEngine returns a usable Engine seeded with the embedded
// members list. The fetcher is a stub — Run() / fetchAndSwap don't
// hit it in any refresher test.
func freshEngine(t *testing.T) *Engine {
	t.Helper()
	bars := stubBarFetcher{}
	return New(NewStore(t.TempDir()), bars, Options{})
}

type stubBarFetcher struct{}

func (stubBarFetcher) FetchDaily(_ context.Context, _ string, _ int) ([]Bar, error) {
	return nil, nil
}

// makeMembers builds a sanity-bound-passing list with N unique
// symbols. Real S&P members aren't needed — only the count and
// distinctness matter for refresher tests.
func makeMembers(n int) []string {
	out := make([]string, n)
	for i := range n {
		// Names like AAA, AAB, AAC ... ensure uniqueness up to ~17k.
		out[i] = string(rune('A'+i/676)) + string(rune('A'+(i/26)%26)) + string(rune('A'+i%26))
	}
	return out
}

// TestRefresherHappyPath: fetch succeeds, file written, engine updated.
func TestRefresherHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	eng := freshEngine(t)
	asOf := time.Date(2026, time.May, 23, 7, 30, 0, 0, time.UTC)
	newMembers := makeMembers(503)
	ff := &fakeFetcher{members: newMembers, asOf: asOf}

	r := NewRefresher(RefresherOptions{
		Engine:    eng,
		CachePath: path,
		Fetch:     ff.fn,
	})

	r.TriggerNow(context.Background())
	waitForFlight(t, r, time.Second)

	if !r.State().IsHealthy() {
		t.Errorf("state: want healthy, got %s", r.State())
	}
	if ff.calls.Load() != 1 {
		t.Errorf("fetch calls: want 1, got %d", ff.calls.Load())
	}
	got := eng.Members()
	if !slices.Equal(got, newMembers) {
		t.Errorf("engine members not updated: want %d got %d", len(newMembers), len(got))
	}
	// File written.
	loaded, _, ok := LoadExternal(path)
	if !ok {
		t.Fatal("expected file to be written")
	}
	if !slices.Equal(loaded, newMembers) {
		t.Error("file contents don't match fetched members")
	}
}

// TestRefresherSingleflight: 4 concurrent triggers → exactly one fetch.
func TestRefresherSingleflight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	eng := freshEngine(t)
	ff := &fakeFetcher{
		members: makeMembers(503),
		asOf:    time.Now().UTC(),
		delay:   50 * time.Millisecond,
	}
	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})

	var wg sync.WaitGroup
	ctx := context.Background()
	for range 4 {
		wg.Go(func() {
			r.TriggerNow(ctx)
		})
	}
	wg.Wait()
	waitForFlight(t, r, time.Second)

	if got := ff.calls.Load(); got != 1 {
		t.Errorf("singleflight broken: want 1 fetch, got %d", got)
	}
}

// TestRefresherFallbackOnNetworkError: fetch fails → engine list
// unchanged, state=network_failed.
func TestRefresherFallbackOnNetworkError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	eng := freshEngine(t)
	prior := eng.Members()
	ff := &fakeFetcher{err: errors.New("dial tcp: connection refused")}

	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})
	r.TriggerNow(context.Background())
	waitForFlight(t, r, time.Second)

	if r.State() != RefreshNetworkFailed {
		t.Errorf("state: want network_failed, got %s", r.State())
	}
	if !slices.Equal(eng.Members(), prior) {
		t.Error("engine members changed on network failure — should be untouched")
	}
}

// TestRefresherFallbackOnParseFailure: count outside sanity bounds →
// state=parse_failed, engine unchanged.
func TestRefresherFallbackOnParseFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	eng := freshEngine(t)
	prior := eng.Members()
	// 100 names — well below MinMembers. Simulates a regex that
	// matched only a fraction of the table.
	ff := &fakeFetcher{members: makeMembers(100), asOf: time.Now()}

	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})
	r.TriggerNow(context.Background())
	waitForFlight(t, r, time.Second)

	if r.State() != RefreshParseFailed {
		t.Errorf("state: want parse_failed, got %s", r.State())
	}
	if !slices.Equal(eng.Members(), prior) {
		t.Error("engine members changed on parse failure — should be untouched")
	}
}

// TestRefresherPinnedByConfig: Run() returns immediately, state
// surfaces disabled (config).
func TestRefresherPinnedByConfig(t *testing.T) {
	eng := freshEngine(t)
	ff := &fakeFetcher{members: makeMembers(503), asOf: time.Now()}
	r := NewRefresher(RefresherOptions{
		Engine: eng, CachePath: "", Fetch: ff.fn,
		PinnedByConfig: true,
	})
	if r.State() != RefreshDisabledConfig {
		t.Errorf("state: want disabled (config), got %s", r.State())
	}

	// Run should exit promptly without fetching.
	done := make(chan struct{})
	go func() {
		r.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit when disabled by config")
	}
	if ff.calls.Load() != 0 {
		t.Errorf("fetch ran under PinnedByConfig: %d calls", ff.calls.Load())
	}

	// TriggerIfRolledOver is a no-op under disabled.
	r.TriggerIfRolledOver(context.Background())
	time.Sleep(20 * time.Millisecond)
	if ff.calls.Load() != 0 {
		t.Errorf("TriggerIfRolledOver fetched under PinnedByConfig: %d", ff.calls.Load())
	}
}

// TestRefresherPinnedByEnv: same as config but env state token.
func TestRefresherPinnedByEnv(t *testing.T) {
	eng := freshEngine(t)
	ff := &fakeFetcher{members: makeMembers(503), asOf: time.Now()}
	r := NewRefresher(RefresherOptions{
		Engine: eng, Fetch: ff.fn,
		PinnedByEnv: true,
	})
	if r.State() != RefreshDisabledEnv {
		t.Errorf("state: want disabled (env), got %s", r.State())
	}
}

// TestRefresherEnvOverridesConfig: both pins set → env wins on the
// status token (matches user expectation: env is the override).
func TestRefresherEnvOverridesConfig(t *testing.T) {
	eng := freshEngine(t)
	ff := &fakeFetcher{members: makeMembers(503), asOf: time.Now()}
	r := NewRefresher(RefresherOptions{
		Engine: eng, Fetch: ff.fn,
		PinnedByConfig: true,
		PinnedByEnv:    true,
	})
	if r.State() != RefreshDisabledEnv {
		t.Errorf("env should win over config: got %s", r.State())
	}
}

// TestRefresherNoSwapOnUnchanged: same list → no engine SetMembers,
// no cache rewrite, state=healthy.
func TestRefresherNoSwapOnUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	eng := freshEngine(t)
	current := eng.Members()
	ff := &fakeFetcher{members: current, asOf: time.Now().UTC()}

	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})
	r.TriggerNow(context.Background())
	waitForFlight(t, r, time.Second)

	if r.State() != RefreshHealthy {
		t.Errorf("state: want healthy, got %s", r.State())
	}
	// No file written for an unchanged list (cache rewrite would be
	// noise on disk if the list hasn't moved).
	if MembersFileExists(path) {
		t.Error("file should NOT be written when members are unchanged")
	}
}

// TestRefresherUnchangedBumpsStaleDiskDate: unchanged list but the
// on-disk as_of lags today → fetchAndSwap bumps the file's date so
// needsCatchup()/TriggerIfRolledOver stop re-fetching Wikipedia on
// every breadth-touching request (observed loop: six unchanged days,
// 450+ fetches).
func TestRefresherUnchangedBumpsStaleDiskDate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	eng := freshEngine(t)
	current := eng.Members()
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).UTC()
	if err := SaveExternal(path, current, weekAgo); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ff := &fakeFetcher{members: current, asOf: time.Now().UTC()}

	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})
	if !r.needsCatchup() {
		t.Fatal("precondition: stale on-disk file should report needsCatchup")
	}
	r.TriggerNow(context.Background())
	waitForFlight(t, r, time.Second)

	if r.State() != RefreshHealthy {
		t.Errorf("state: want healthy, got %s", r.State())
	}
	if r.needsCatchup() {
		t.Error("unchanged fetch should bump the on-disk as_of so catch-up stops re-firing")
	}
}

// TestRefresherCatchupOnStartup: on-disk file's as_of older than today
// → Run() fires a fetch on entry.
func TestRefresherCatchupOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	// Pre-populate with a stale file.
	stale := makeMembers(503)
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).UTC()
	if err := SaveExternal(path, stale, weekAgo); err != nil {
		t.Fatalf("seed: %v", err)
	}

	eng := freshEngine(t)
	updated := makeMembers(503)
	// flip last name so SetMembers reports changed.
	updated[len(updated)-1] = "ZZZ"
	ff := &fakeFetcher{members: updated, asOf: time.Now().UTC()}

	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)

	// Wait for the catchup fetch to fire.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ff.calls.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	if ff.calls.Load() == 0 {
		t.Error("startup catchup did not fire — Run should fetch when on-disk file is stale")
	}
}

// TestRefresherNoCatchupWhenFresh: file from today → Run() doesn't
// fire an immediate fetch, just waits for the daily tick.
func TestRefresherNoCatchupWhenFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	fresh := makeMembers(503)
	if err := SaveExternal(path, fresh, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	eng := freshEngine(t)
	ff := &fakeFetcher{members: fresh, asOf: time.Now().UTC()}
	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	if ff.calls.Load() != 0 {
		t.Errorf("Run fetched immediately despite fresh file: %d calls", ff.calls.Load())
	}
}

// TestRefresherTriggerIfRolledOver: stale file + opportunistic
// trigger → fetch fires.
func TestRefresherTriggerIfRolledOver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	stale := makeMembers(503)
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).UTC()
	if err := SaveExternal(path, stale, weekAgo); err != nil {
		t.Fatalf("seed: %v", err)
	}

	eng := freshEngine(t)
	ff := &fakeFetcher{members: stale, asOf: time.Now().UTC()}
	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})

	r.TriggerIfRolledOver(context.Background())
	waitForFlight(t, r, time.Second)
	if ff.calls.Load() != 1 {
		t.Errorf("TriggerIfRolledOver should fetch when on-disk is stale: %d calls", ff.calls.Load())
	}
}

// TestRefresherTriggerIfRolledOverFreshNoOp: fresh file → opportunistic
// trigger is a no-op.
func TestRefresherTriggerIfRolledOverFreshNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	fresh := makeMembers(503)
	if err := SaveExternal(path, fresh, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	eng := freshEngine(t)
	ff := &fakeFetcher{members: fresh, asOf: time.Now().UTC()}
	r := NewRefresher(RefresherOptions{Engine: eng, CachePath: path, Fetch: ff.fn})

	r.TriggerIfRolledOver(context.Background())
	time.Sleep(50 * time.Millisecond)
	if ff.calls.Load() != 0 {
		t.Errorf("TriggerIfRolledOver should no-op on fresh file: %d calls", ff.calls.Load())
	}
}

// TestRefreshStateClassification pins the helpers — IsHealthy /
// IsDisabled. Status renderer relies on them; bare string comparison
// would break under a future state-name refactor.
func TestRefreshStateClassification(t *testing.T) {
	cases := []struct {
		state      RefreshState
		healthy    bool
		isDisabled bool
	}{
		{RefreshHealthy, true, false},
		{RefreshNetworkFailed, false, false},
		{RefreshParseFailed, false, false},
		{RefreshDisabledConfig, false, true},
		{RefreshDisabledEnv, false, true},
	}
	for _, c := range cases {
		if c.state.IsHealthy() != c.healthy {
			t.Errorf("%s: IsHealthy got %v want %v", c.state, c.state.IsHealthy(), c.healthy)
		}
		if c.state.IsDisabled() != c.isDisabled {
			t.Errorf("%s: IsDisabled got %v want %v", c.state, c.state.IsDisabled(), c.isDisabled)
		}
	}
}

// waitForFlight blocks until the singleflight gate clears or the
// budget elapses. Refresher launches a goroutine per trigger; tests
// need to wait for it to finish before asserting state.
func waitForFlight(t *testing.T, r *Refresher, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		r.flightMu.Lock()
		active := r.flightActive
		r.flightMu.Unlock()
		if !active {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("flight did not complete within %s", budget)
}
