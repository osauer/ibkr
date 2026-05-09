//go:build ibkrprotocol

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/pkg/ibkr/protocoltest"
)

func main() {
	host := flag.String("host", "127.0.0.1", "IBKR Gateway host")
	port := flag.Int("port", 4001, "IBKR Gateway port")
	clients := flag.String("clients", "100,101,102,103", "Comma-separated client IDs to cycle")
	timeout := flag.Duration("timeout", 30*time.Second, "Overall run timeout")
	readTimeout := flag.Duration("read-timeout", 2*time.Second, "Per-message read timeout")
	useTLS := flag.Bool("tls", false, "Use TLS when dialing the IBKR gateway")
	useTLSFallback := flag.Bool("tls-fallback", true, "Attempt the opposite TLS mode if the first attempt fails")
	tlsInsecure := flag.Bool("tls-insecure", true, "Skip TLS certificate verification when tls is enabled")
	tlsServerName := flag.String("tls-server-name", "", "Override TLS server name (defaults to host when verification enabled)")
	flag.Parse()

	clientIDs, err := parseClientIDs(*clients)
	if err != nil {
		log.Fatalf("invalid clients list: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	runner := &protocoltest.LiveRunner{
		Host:                  *host,
		Port:                  *port,
		ClientIDs:             clientIDs,
		Cases:                 protocoltest.SampleCases,
		ReadTimeout:           *readTimeout,
		UseTLS:                *useTLS,
		EnableTLSFallback:     *useTLSFallback,
		TLSInsecureSkipVerify: *tlsInsecure,
		TLSServerName:         *tlsServerName,
	}

	results, err := runner.Run(ctx)
	if err != nil {
		log.Fatalf("runner error: %v", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		log.Fatalf("encode results: %v", err)
	}
}

func parseClientIDs(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		id, err := strconv.Atoi(trimmed)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no client IDs parsed")
	}
	return out, nil
}
