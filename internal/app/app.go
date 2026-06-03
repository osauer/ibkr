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

	"github.com/osauer/ibkr/internal/app/alerts"
	"github.com/osauer/ibkr/internal/app/auth"
	"github.com/osauer/ibkr/internal/app/daemonclient"
	apphttp "github.com/osauer/ibkr/internal/app/http"
	"github.com/osauer/ibkr/internal/app/live"
	"github.com/osauer/ibkr/internal/app/push"
	"github.com/osauer/ibkr/internal/app/relay"
	"github.com/osauer/ibkr/internal/app/state"
	"github.com/osauer/ibkr/internal/rpc"
	"github.com/osauer/ibkr/internal/xdgcache"
)

type App struct {
	Options Options
	Store   *state.Store
	Auth    *auth.Manager
	Live    *live.Service
	Relay   relay.Client
	Server  *hyperserve.Server
	lock    *xdgcache.Lock
}

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
	liveSvc := live.New(
		daemonclient.Real{SocketPath: opts.SocketPath, AutoSpawn: true},
		opts.PollEvery,
		opts.CanaryEvery,
	)
	relayClient := relay.Noop{PublicURL: opts.PublicURL}
	monitor := alerts.Monitor{
		Store:  store,
		Sender: push.WebPushSender{Subscriber: "mailto:ibkr-app@localhost"},
		URL:    opts.PublicURL,
	}
	liveSvc.OnCanary = func(ctx context.Context, canary rpc.CanaryResult) {
		monitor.Observe(ctx, canary)
	}
	app, err := newWithParts(opts, store, authMgr, liveSvc, relayClient)
	if err != nil {
		return nil, err
	}
	app.lock = lock
	lock = nil
	return app, nil
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

func newWithParts(opts Options, store *state.Store, authMgr *auth.Manager, liveSvc *live.Service, relayClient relay.Client) (*App, error) {
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
		Server:    srv,
		Store:     store,
		Auth:      authMgr,
		Live:      liveSvc,
		Relay:     relayClient,
		PublicURL: opts.PublicURL,
		Version:   opts.Version,
	})
	return a, nil
}

func (a *App) Run(ctx context.Context) error {
	defer func() { _ = a.Close() }()
	liveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Live.Start(liveCtx)
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

func (a *App) Close() error {
	if a == nil || a.lock == nil {
		return nil
	}
	err := a.lock.Release()
	a.lock = nil
	return err
}
