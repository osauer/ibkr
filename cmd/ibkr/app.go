package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	mobileapp "github.com/osauer/ibkr/internal/app"
	"github.com/osauer/ibkr/internal/app/auth"
	"github.com/osauer/ibkr/internal/cli"
)

func runApp(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h", "help":
			printAppUsage(os.Stdout)
			return 0
		case "pair":
			return runAppPair(args[1:])
		case "restart":
			return runAppRestart(args[1:])
		case "serve":
			return runAppServe(args[1:])
		}
	}
	return runAppServe(args)
}

func runAppRestart(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return cli.RunRestart(ctx, append([]string{"--app"}, args...), os.Stdout, os.Stderr)
}

func runAppServe(args []string) int {
	opts := mobileapp.DefaultOptions(effectiveVersion())
	fs := flag.NewFlagSet("ibkr app", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		printAppUsage(os.Stdout)
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Serve flags:")
		fs.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(os.Stdout, "  --%-12s  %s (default %q)\n", f.Name, f.Usage, f.DefValue)
		})
	}
	addr := fs.String("addr", opts.Addr, "HTTP listen address")
	publicURL := fs.String("public-url", opts.PublicURL, "trusted browser-visible base URL")
	remote := fs.Bool("remote", opts.Remote, "enable the outbound Cloudflare Worker relay")
	remoteURL := fs.String("remote-url", opts.RemoteURL, "Cloudflare Worker relay base URL")
	stateDir := fs.String("state-dir", opts.StateDir, "local app state directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "ibkr app: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	opts.Addr = strings.TrimSpace(*addr)
	opts.Remote = *remote
	opts.RemoteURL = strings.TrimRight(strings.TrimSpace(*remoteURL), "/")
	if flagWasSet(fs, "public-url") {
		opts.PublicURL = strings.TrimRight(strings.TrimSpace(*publicURL), "/")
	} else if !opts.PublicURLFromEnv {
		opts.PublicURL = mobileapp.PublicURLForAddr(opts.Addr)
	}
	opts.StateDir = strings.TrimSpace(*stateDir)

	app, err := mobileapp.New(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr app: %v\n", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stdout, "ibkr app serving %s (listen %s)\n", app.Options.PublicURL, app.Options.Addr)
	fmt.Fprintln(os.Stdout, "Pair a phone with: ibkr app pair")
	if err := app.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ibkr app: %v\n", err)
		return 1
	}
	return 0
}

func runAppPair(args []string) int {
	opts := mobileapp.DefaultOptions(effectiveVersion())
	fs := flag.NewFlagSet("ibkr app pair", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, "ibkr app pair - print a short-lived QR pairing URL from the local app host.")
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Usage: ibkr app pair [--addr HOST:PORT] [--public-url URL] [--json]")
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Flags:")
		fs.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(os.Stdout, "  --%-12s  %s (default %q)\n", f.Name, f.Usage, f.DefValue)
		})
	}
	addr := fs.String("addr", opts.Addr, "local app host listen address")
	publicURLDefault := ""
	if opts.PublicURLFromEnv {
		publicURLDefault = opts.PublicURL
	}
	publicURL := fs.String("public-url", publicURLDefault, "override browser-visible base URL to embed in the pairing QR")
	asJSON := fs.Bool("json", false, "print the pairing session as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "ibkr app pair: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	pairAddr := strings.TrimSpace(*addr)
	pairPublicURL := appPairPublicURLOverride(fs, *publicURL, opts.PublicURLFromEnv)
	session, err := createPairingSession(pairAddr, pairPublicURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr app pair: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(session)
		return 0
	}
	qr, err := qrcode.New(session.URL, qrcode.Medium)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr app pair: QR: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "Scan this QR code with the iPhone:")
	fmt.Fprintln(os.Stdout)
	fmt.Fprint(os.Stdout, qr.ToSmallString(false))
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "Pairing URL: %s\n", session.URL)
	fmt.Fprintf(os.Stdout, "Expires: %s\n", session.ExpiresAt.Local().Format(time.RFC1123))
	return 0
}

func createPairingSession(addr, publicURL string) (auth.PairingSession, error) {
	baseURL := "http://" + mobileapp.LoopbackAddrForLocalConnect(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body := []byte("{}")
	if strings.TrimSpace(publicURL) != "" {
		var err error
		body, err = json.Marshal(struct {
			PublicURL string `json:"public_url,omitempty"`
		}{PublicURL: strings.TrimRight(strings.TrimSpace(publicURL), "/")})
		if err != nil {
			return auth.PairingSession{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/pairing/sessions", bytes.NewReader(body))
	if err != nil {
		return auth.PairingSession{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return auth.PairingSession{}, fmt.Errorf("connect to local app host at %s: %w (start it with `ibkr app`)", baseURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		if body.Error == "" {
			body.Error = res.Status
		}
		return auth.PairingSession{}, errors.New(body.Error)
	}
	var session auth.PairingSession
	if err := json.NewDecoder(res.Body).Decode(&session); err != nil {
		return auth.PairingSession{}, err
	}
	return session, nil
}

func appPairPublicURLOverride(fs *flag.FlagSet, publicURL string, defaultIsExplicit bool) string {
	if !flagWasSet(fs, "public-url") && !defaultIsExplicit {
		return ""
	}
	return strings.TrimSpace(publicURL)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

func printAppUsage(w *os.File) {
	fmt.Fprintln(w, "ibkr app - run the paired mobile PWA application layer.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ibkr app [--addr HOST:PORT] [--public-url URL] [--remote] [--remote-url URL] [--state-dir PATH]")
	fmt.Fprintln(w, "  ibkr app restart [--addr HOST:PORT] [--public-url URL] [--remote] [--remote-url URL] [--state-dir PATH]")
	fmt.Fprintln(w, "  ibkr app pair [--addr HOST:PORT] [--public-url URL] [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The app serves a mobile-first PWA, live SSE snapshots,")
	fmt.Fprintln(w, "and opt-in canary Web Push subscriptions. Pairing URLs are short-lived.")
}
