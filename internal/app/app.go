package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
)

type App struct {
	Options Options
	Store   *state.Store
	Auth    *auth.Manager
	Live    *live.Service
	Relay   relay.Client
	Server  *hyperserve.Server
}

func New(opts Options) (*App, error) {
	if opts.Addr == "" {
		opts = DefaultOptions(opts.Version)
	}
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
	return newWithParts(opts, store, authMgr, liveSvc, relayClient)
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
