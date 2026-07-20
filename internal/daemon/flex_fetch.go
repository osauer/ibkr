package daemon

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/v2/internal/flexstmt"
)

// Daily IBKR Flex statement ingestion (docs/design/post-trade-truth.md).
// Read-only toward the broker. The Flex token is read from its own 0600
// file at fetch time and must never appear in any error, log line, journal,
// RPC result, or saved artifact — errors are built from Flex status codes,
// never from request URLs.

const (
	flexSendRequestURL  = "https://gdcdyn.interactivebrokers.com/Universal/servlet/FlexStatementService.SendRequest"
	flexGetStatementURL = "https://gdcdyn.interactivebrokers.com/Universal/servlet/FlexStatementService.GetStatement"
	flexStatementsDir   = "statements"
	// Engineering constants (operator decision 2026-07-13: code-owned).
	// The longer poll budget accommodates server-side generation of the
	// one-year backfill while the daily statement usually completes sooner.
	flexPollInterval   = 10 * time.Second
	flexPollAttempts   = 30
	flexHTTPTimeout    = 30 * time.Second
	flexFetchEvery     = 20 * time.Hour // daily cadence with slack
	flexRetryAfterFail = time.Hour
	flexCheckInterval  = time.Hour
)

type flexFetchState struct {
	mu          sync.Mutex
	lastAttempt time.Time
	lastSuccess time.Time
	lastError   string // sanitized: status/error codes only, never URLs
	busy        atomic.Bool
}

type flexServiceEnvelope struct {
	XMLName       xml.Name `xml:"FlexStatementResponse"`
	Status        string   `xml:"Status"`
	ReferenceCode string   `xml:"ReferenceCode"`
	URL           string   `xml:"Url"`
	ErrorCode     string   `xml:"ErrorCode"`
	ErrorMessage  string   `xml:"ErrorMessage"`
}

func flexStatementsDirPath() (string, error) {
	return defaultTradingStatePath(flexStatementsDir)
}

// newestFlexStatementTime scans the retained raw files; the newest mtime is
// the restart-safe "when did we last successfully ingest" signal.
func newestFlexStatementTime() time.Time {
	dir, err := flexStatementsDirPath()
	if err != nil {
		return time.Time{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// runFlexFetchLoop is the daily scheduler: hourly wake-ups, a fetch when
// the newest retained statement is older than the daily cadence, and an
// hour of quiet after a failure. Single-flight with the on-demand kick.
func (s *Server) runFlexFetchLoop(ctx context.Context) {
	if s == nil || s.cfg == nil || !s.cfg.Flex.Enabled {
		return
	}
	t := time.NewTicker(flexCheckInterval)
	defer t.Stop()
	for {
		s.maybeFetchFlex(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) maybeFetchFlex(ctx context.Context) {
	st := &s.flexFetch
	st.mu.Lock()
	due := time.Since(newestFlexStatementTime()) >= flexFetchEvery &&
		time.Since(st.lastAttempt) >= flexRetryAfterFail
	st.mu.Unlock()
	if due {
		s.kickFlexFetch(ctx)
	}
}

// kickFlexFetch runs one background fetch, single-flight. Called by the
// scheduler and by recon refresh requests.
func (s *Server) kickFlexFetch(ctx context.Context) {
	if s == nil || s.cfg == nil || !s.cfg.Flex.Enabled {
		return
	}
	st := &s.flexFetch
	if !st.busy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer st.busy.Store(false)
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flexPollAttempts*flexPollInterval+2*flexHTTPTimeout)
		defer cancel()
		st.mu.Lock()
		st.lastAttempt = time.Now()
		st.mu.Unlock()
		path, err := s.fetchFlexOnce(fctx)
		st.mu.Lock()
		if err != nil {
			st.lastError = err.Error()
		} else {
			st.lastError = ""
			st.lastSuccess = time.Now()
		}
		st.mu.Unlock()
		if err != nil {
			s.logger.Infof("flex fetch failed: %v", err)
		} else {
			s.logger.Infof("flex statement ingested: %s", filepath.Base(path))
			// The XML remains the broker evidence; refresh its typed SQLite
			// inventory/equity projection before downstream history reads.
			// A projection failure preserves the last complete generation.
			projectionCtx, projectionCancel := context.WithTimeout(context.WithoutCancel(ctx), flexHTTPTimeout)
			if err := s.refreshStatementProjection(projectionCtx); err != nil {
				s.logger.Warnf("statement projection refresh failed: %v", err)
			}
			projectionCancel()
			go s.evaluateRiskPolicyV3Reconciliation()
		}
	}()
}

func (s *Server) flexFetchStatus() (configured bool, lastSuccess, lastAttempt time.Time, lastErr string) {
	configured = s.cfg != nil && s.cfg.Flex.Enabled && strings.TrimSpace(s.cfg.Flex.QueryID) != ""
	st := &s.flexFetch
	st.mu.Lock()
	defer st.mu.Unlock()
	return configured, st.lastSuccess, st.lastAttempt, st.lastError
}

// fetchFlexOnce runs the two-step Flex protocol: SendRequest returns a
// reference code, GetStatement is polled until the report is generated.
// The saved raw file is validated through the parser before retention so a
// service envelope can never sit in the statements dir pretending to be a
// week with no activity.
func (s *Server) fetchFlexOnce(ctx context.Context) (string, error) {
	cfg := s.cfg.Flex
	queryID := strings.TrimSpace(cfg.QueryID)
	if queryID == "" {
		return "", fmt.Errorf("flex.query_id is not configured")
	}
	tokenBytes, err := os.ReadFile(expandUserPath(cfg.TokenPath))
	if err != nil {
		return "", fmt.Errorf("flex token unavailable at %s (create the Flex Web Service token in IBKR Account Management and store it there, mode 0600)", cfg.TokenPath)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return "", fmt.Errorf("flex token file %s is empty", cfg.TokenPath)
	}
	client := &http.Client{Timeout: flexHTTPTimeout}

	env, err := flexServiceCall(ctx, client, flexSendRequestURL, url.Values{"t": {token}, "q": {queryID}, "v": {"3"}})
	if err != nil {
		return "", fmt.Errorf("flex send-request: %w", err)
	}
	if !strings.EqualFold(env.Status, "Success") || strings.TrimSpace(env.ReferenceCode) == "" {
		return "", fmt.Errorf("flex send-request refused: code %s: %s", nonEmptyString(env.ErrorCode, "unknown"), nonEmptyString(env.ErrorMessage, env.Status))
	}
	getURL := strings.TrimSpace(env.URL)
	if getURL == "" || !strings.HasPrefix(getURL, "https://") {
		getURL = flexGetStatementURL
	}

	var raw []byte
	for attempt := range flexPollAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(flexPollInterval):
			}
		}
		body, err := flexRawCall(ctx, client, getURL, url.Values{"t": {token}, "q": {env.ReferenceCode}, "v": {"3"}})
		if err != nil {
			return "", fmt.Errorf("flex get-statement: %w", err)
		}
		if strings.Contains(string(body), "<FlexStatementResponse") {
			var progress flexServiceEnvelope
			if xml.Unmarshal(body, &progress) == nil && progress.ErrorCode == "1019" {
				continue // statement generation in progress
			}
			var code, msg string
			if xml.Unmarshal(body, &progress) == nil {
				code, msg = progress.ErrorCode, progress.ErrorMessage
			}
			return "", fmt.Errorf("flex get-statement refused: code %s: %s", nonEmptyString(code, "unknown"), nonEmptyString(msg, "unrecognized envelope"))
		}
		raw = body
		break
	}
	if raw == nil {
		return "", fmt.Errorf("flex statement not generated after %d polls; will retry next cycle", flexPollAttempts)
	}
	if _, err := flexstmt.Parse(raw); err != nil {
		return "", fmt.Errorf("flex statement rejected by parser: %w", err)
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("flex-%s.xml", time.Now().UTC().Format("20060102-150405")))
	if err := writePrivateStateAtomic(path, raw); err != nil {
		return "", fmt.Errorf("retain flex statement: %w", err)
	}
	return path, nil
}

// flexServiceCall performs one envelope-returning call. The token travels
// only in the POST form body, and errors never include the request.
func flexServiceCall(ctx context.Context, client *http.Client, endpoint string, params url.Values) (*flexServiceEnvelope, error) {
	body, err := flexRawCall(ctx, client, endpoint, params)
	if err != nil {
		return nil, err
	}
	var env flexServiceEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("unrecognized flex service response")
	}
	return &env, nil
}

func flexRawCall(ctx context.Context, client *http.Client, endpoint string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build flex request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		// Transport errors can embed the full URL (with query) — never
		// propagate them verbatim.
		return nil, fmt.Errorf("flex service unreachable: %s", sanitizeFlexTransportError(err, endpoint))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("flex service HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// sanitizeFlexTransportError keeps the failure class while guaranteeing no
// credential can ride an error string.
func sanitizeFlexTransportError(err error, endpoint string) string {
	msg := err.Error()
	if u, perr := url.Parse(endpoint); perr == nil {
		msg = strings.ReplaceAll(msg, endpoint, u.Host)
	}
	if i := strings.Index(msg, "?"); i >= 0 {
		msg = msg[:i] + "?…"
	}
	return msg
}

// loadRetainedFlexStatements parses every retained raw statement, newest
// file first. A file that no longer parses is reported, never skipped
// silently.
func loadRetainedFlexStatements() ([]flexstmt.Statement, []string, error) {
	return loadRetainedFlexStatementsContext(context.Background(), nil)
}

func loadRetainedFlexStatementsContext(ctx context.Context, checkpoint func(string) error) ([]flexstmt.Statement, []string, error) {
	check := func(stage string) error {
		if checkpoint != nil {
			return checkpoint(stage)
		}
		return ctx.Err()
	}
	if err := check("retained_statements_start"); err != nil {
		return nil, nil, err
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if err := check("retained_statements_entries"); err != nil {
			return nil, nil, err
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".xml") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // timestamped names: newest first
	var out []flexstmt.Statement
	var problems []string
	for _, name := range names {
		if err := check("retained_statement_file"); err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		sts, err := flexstmt.Parse(data)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		out = append(out, sts...)
	}
	if err := check("retained_statements_complete"); err != nil {
		return nil, nil, err
	}
	return out, problems, nil
}
