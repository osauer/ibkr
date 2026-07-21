package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	hyperserve "github.com/osauer/hyperserve/pkg/server"

	"github.com/osauer/ibkr/v2/internal/app/alerts"
	"github.com/osauer/ibkr/v2/internal/app/auth"
	"github.com/osauer/ibkr/v2/internal/app/daemonclient"
	apphttp "github.com/osauer/ibkr/v2/internal/app/http"
	"github.com/osauer/ibkr/v2/internal/app/live"
	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/app/relay"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
	"github.com/osauer/ibkr/v2/internal/xdgcache"
)

// App is one configured Canary app-host process. New acquires exclusive
// ownership of Options.StateDir and populates the host components; Run starts
// their background loops and HTTP server. Close releases the process lock.
// Exported component fields expose app-local adapters, not broker authority.
type App struct {
	Options          Options
	Store            *state.Store
	Auth             *auth.Manager
	Live             *live.Service
	Relay            relay.Client
	Server           *hyperserve.Server
	governanceWorker *alerts.GovernanceWorker
	lock             *xdgcache.Lock
}

// New constructs an App and acquires the exclusive lock for opts.StateDir. If
// opts.Addr is empty, opts is replaced with [DefaultOptions] for opts.Version.
// The lock remains held until [App.Close] or the end of [App.Run], and is
// released if construction fails. New opens app-local state and prepares the
// relay, but it does not start the HTTP server or background loops.
func New(opts Options) (*App, error) {
	if opts.Addr == "" {
		opts = DefaultOptions(opts.Version)
	}
	lock, err := acquireAppLock(opts.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if lock != nil {
			_ = lock.Release()
		}
	}()
	store, err := state.Open(opts.StateDir)
	if err != nil {
		return nil, err
	}
	if _, err := store.EnsureVAPID(time.Now().UTC(), push.GenerateVAPIDKeys); err != nil {
		return nil, fmt.Errorf("vapid keys: %w", err)
	}
	authMgr := auth.NewManager(store, opts.PairingTTL)
	daemonClient := daemonclient.Real{SocketPath: opts.SocketPath, AutoSpawn: true}
	liveSvc := live.New(
		daemonClient,
		opts.PollEvery,
		opts.CanaryEvery,
	)
	if err := liveSvc.SetAlertSnapshotStore(store); err != nil {
		return nil, fmt.Errorf("prime alert shadow state: %w", err)
	}
	relayClient, err := newRelayClient(opts, store)
	if err != nil {
		return nil, err
	}
	if worker, ok := relayClient.(interface{ PublicURL() string }); ok {
		if publicURL := strings.TrimSpace(worker.PublicURL()); publicURL != "" {
			opts.PublicURL = publicURL
		}
	}
	pushSender := push.WebPushSender{Subscriber: push.Subscriber}
	monitor := alerts.Monitor{
		Store:  store,
		Sender: pushSender,
		URL:    opts.PublicURL,
	}
	monitor.TradingStatus = func() *rpc.TradingStatus {
		return liveSvc.Snapshot().Trading
	}
	liveSvc.OnCanary = func(ctx context.Context, canary rpc.CanaryResult) {
		monitor.Observe(ctx, canary)
	}
	mismatchWatch := &alerts.OrderMismatchWatch{
		Store:         store,
		Sender:        pushSender,
		URL:           opts.PublicURL,
		TradingStatus: monitor.TradingStatus,
	}
	liveSvc.OnOrders = func(ctx context.Context, orders rpc.OrdersOpenResult) {
		mismatchWatch.Observe(ctx, orders)
	}
	dispatcher := alerts.GovernanceDispatcher{Store: store, Sender: pushSender}
	governanceWorker := alerts.NewGovernanceWorker(&dispatcher)
	liveSvc.OnNudges = func(ctx context.Context, snapshot rpc.NudgesSnapshotResult) {
		governanceWorker.Submit(snapshot)
	}
	app, err := newWithParts(opts, store, authMgr, daemonClient, liveSvc, relayClient, pushSender)
	if err != nil {
		return nil, err
	}
	app.lock = lock
	app.governanceWorker = governanceWorker
	lock = nil
	return app, nil
}

func newRelayClient(opts Options, store *state.Store) (relay.Client, error) {
	if !opts.Remote {
		return relay.Noop{PublicURL: opts.PublicURL}, nil
	}
	originURL := "http://" + LoopbackAddrForLocalConnect(opts.Addr)
	var routeID, connectorToken string
	if store != nil {
		if route, ok := store.RelayRoute(opts.RemoteURL); ok {
			routeID = route.RouteID
			connectorToken = route.ConnectorToken
		}
	}
	client, err := relay.NewWorker(relay.WorkerOptions{
		BaseURL:              opts.RemoteURL,
		OriginURL:            originURL,
		Version:              opts.Version,
		ResumeRouteID:        routeID,
		ResumeConnectorToken: connectorToken,
		OnRoute: func(reg relay.RouteRegistration) error {
			if store == nil {
				return nil
			}
			return store.SetRelayRoute(state.RelayRoute{
				RemoteURL:      strings.TrimRight(strings.TrimSpace(opts.RemoteURL), "/"),
				RouteID:        reg.RouteID,
				ConnectorToken: reg.ConnectorToken,
				PublicURL:      reg.PublicURL,
				ConnectorURL:   reg.ConnectorURL,
				ExpiresAt:      reg.ExpiresAt,
			})
		},
	})
	if err != nil {
		return nil, fmt.Errorf("remote relay: %w", err)
	}
	return client, nil
}

func acquireAppLock(stateDir string) (*xdgcache.Lock, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("state dir required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create app state dir: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure app state dir: %w", err)
	}
	lock, err := xdgcache.OpenLock(filepath.Join(stateDir, "app.lock"))
	if err != nil {
		if errors.Is(err, xdgcache.ErrLocked) {
			return nil, errors.New("another ibkr app process is already running for this state directory")
		}
		return nil, err
	}
	return lock, nil
}

func newWithParts(opts Options, store *state.Store, authMgr *auth.Manager, daemonClient daemonclient.Client, liveSvc *live.Service, relayClient relay.Client, pushSender push.Sender) (*App, error) {
	srv, err := hyperserve.NewServer(
		hyperserve.WithAddr(opts.Addr),
		hyperserve.WithTimeouts(30*time.Second, 0, 0),
		hyperserve.WithSuppressBanner(true),
		hyperserve.WithHardenedMode(),
	)
	if err != nil {
		return nil, err
	}
	a := &App{
		Options: opts,
		Store:   store,
		Auth:    authMgr,
		Live:    liveSvc,
		Relay:   relayClient,
		Server:  srv,
	}
	apphttp.Register(apphttp.Dependencies{
		Server:     srv,
		Store:      store,
		Auth:       authMgr,
		Daemon:     daemonClient,
		Live:       liveSvc,
		Relay:      relayClient,
		PublicURL:  opts.PublicURL,
		Version:    opts.Version,
		PushSender: pushSender,
	})
	return a, nil
}

// Run starts live-cache polling, alert workers, relay transport, credential
// reaping, and the HTTP server, then blocks until the server exits. Cancelling
// ctx stops the server and background contexts; normal cancellation and
// [http.ErrServerClosed] return nil. Run always calls [App.Close] before
// returning and must not be invoked concurrently on the same App.
func (a *App) Run(ctx context.Context) error {
	defer func() { _ = a.Close() }()
	liveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Live.Start(liveCtx)
	if a.governanceWorker != nil {
		go a.governanceWorker.Run(liveCtx)
	}
	go a.Relay.Run(liveCtx)
	go a.Auth.StartReaper(liveCtx, time.Minute)
	go func() {
		<-ctx.Done()
		_ = a.Server.Stop()
	}()
	err := a.Server.Run()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Close releases the app state-directory lock. It is a no-op for a nil App or
// after the first call. Close does not itself stop the HTTP server; cancel the
// Run context to shut down a running App.
func (a *App) Close() error {
	if a == nil || a.lock == nil {
		return nil
	}
	err := a.lock.Release()
	a.lock = nil
	return err
}
