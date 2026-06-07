package daemon

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/rpc"
)

const (
	marketEventsRegSHOFreshFor      = 12 * time.Hour
	marketEventsRegSHOMaxAge        = 96 * time.Hour
	marketEventsHaltsFreshFor       = time.Minute
	marketEventsBorrowPollBudget    = 2500 * time.Millisecond
	marketEventsBorrowFeeFreshFor   = 15 * time.Minute
	marketEventsBorrowFeeMaxAge     = 90 * time.Minute
	marketEventsBorrowFeeExtremePct = 50.0
	marketEventsRecentHaltWindow    = 24 * time.Hour
	marketEventsBorrowTightShares   = 10_000
	marketEventsBorrowExtremeShares = 1_000
)

var marketEventsHTTPClient = &http.Client{Timeout: 10 * time.Second}
var fetchIBKRBorrowFees = fetchIBKRBorrowFeesFTP

type marketEventCache struct {
	mu             sync.Mutex
	regSHO         marketEventRegSHOEntry
	halts          marketEventHaltsEntry
	borrowFees     marketEventBorrowFeeEntry
	regSHOFreshFor time.Duration
	haltsFreshFor  time.Duration
	now            func() time.Time
}

type marketEventRegSHOEntry struct {
	FetchedAt time.Time
	AsOf      time.Time
	SourceURL string
	Symbols   map[string]marketEventRegSHORecord
}

type marketEventRegSHORecord struct {
	Symbol         string
	SecurityName   string
	MarketCategory string
	Rule3210       string
}

type marketEventHaltsEntry struct {
	FetchedAt time.Time
	AsOf      time.Time
	SourceURL string
	Records   []marketEventHaltRecord
}

type marketEventHaltRecord struct {
	Symbol              string
	IssueName           string
	Market              string
	ReasonCode          string
	HaltedAt            time.Time
	ResumptionQuoteAt   time.Time
	ResumptionTradeAt   time.Time
	PauseThresholdPrice string
}

type marketEventBorrowFeeEntry struct {
	FetchedAt time.Time
	AsOf      time.Time
	SourceURL string
	Symbols   map[string]marketEventBorrowFeeRecord
}

type marketEventBorrowFeeRecord struct {
	Symbol     string
	Currency   string
	Name       string
	ConID      string
	ISIN       string
	RebateRate float64
	FeeRate    float64
	Available  int64
}

func newMarketEventCache(now func() time.Time) *marketEventCache {
	if now == nil {
		now = time.Now
	}
	return &marketEventCache{
		regSHOFreshFor: marketEventsRegSHOFreshFor,
		haltsFreshFor:  marketEventsHaltsFreshFor,
		now:            now,
	}
}

func (s *Server) installMarketEventCache() {
	s.marketEvents = newMarketEventCache(s.now)
}

func (s *Server) handleMarketEventsSnapshot(ctx context.Context, req *rpc.Request) (*rpc.MarketEventsResult, error) {
	var p rpc.MarketEventsParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	symbols := normalizeMarketEventSymbols(append(p.Symbols, p.Symbol))
	if len(symbols) == 0 {
		pos, err := s.handlePositionsList(ctx, &rpc.Request{})
		if err != nil {
			return nil, err
		}
		symbols = marketEventSymbolsFromPositions(pos)
	}
	res := s.marketEventsForSymbols(ctx, symbols)
	return &res, nil
}

func (s *Server) marketEventsForSymbols(ctx context.Context, symbols []string) rpc.MarketEventsResult {
	if s.marketEvents == nil {
		s.installMarketEventCache()
	}
	return s.marketEvents.snapshot(ctx, symbols, s.subs, s.gatewayConnector())
}

func (c *marketEventCache) snapshot(ctx context.Context, symbols []string, subs *subManager, connector *ibkrlib.Connector) rpc.MarketEventsResult {
	now := c.now().UTC()
	symbols = normalizeMarketEventSymbols(symbols)
	res := rpc.MarketEventsResult{
		Kind:          rpc.MarketEventsKind,
		SchemaVersion: rpc.MarketEventsSchemaVersion,
		AsOf:          now,
		Symbols:       symbols,
		BySymbol:      map[string][]rpc.MarketEventFlag{},
		NotExecution:  "Market-event flags are observed context and daemon safety gates; no orders are placed by ibkr.",
	}
	if len(symbols) == 0 {
		res.WarningDetails = append(res.WarningDetails, rpc.DataWarning{
			Code:     "market_events_no_symbols",
			Severity: "data_quality",
			Message:  "No symbols were provided and no held underlyings were available.",
			Impact:   "No market-event flags can be evaluated.",
			Action:   "Pass --symbol or hold a stock/ETF position before relying on held-name tags.",
		})
		res.Fingerprint = rpc.BuildMarketEventsFingerprint(&res)
		return res
	}

	regSHO, regSHOHealth, err := c.loadRegSHO(ctx, now)
	res.SourceHealth = append(res.SourceHealth, regSHOHealth)
	if err != nil {
		res.WarningDetails = append(res.WarningDetails, marketEventSourceWarning("reg_sho_threshold", err))
	} else {
		for _, sym := range symbols {
			if rec, ok := regSHO.Symbols[sym]; ok {
				res.Flags = append(res.Flags, marketEventRegSHOFlag(sym, rec, regSHO, now))
			}
		}
	}

	halts, haltsHealth, err := c.loadHalts(ctx, now)
	res.SourceHealth = append(res.SourceHealth, haltsHealth)
	if err != nil {
		res.WarningDetails = append(res.WarningDetails, marketEventSourceWarning("halts", err))
	} else {
		for _, sym := range symbols {
			for _, rec := range halts.Records {
				if rec.Symbol == sym {
					if flag, ok := marketEventHaltFlag(sym, rec, halts, now); ok {
						res.Flags = append(res.Flags, flag)
					}
				}
			}
		}
	}

	borrowHealth := marketEventBorrowInventory(ctx, symbols, subs, connector, now, &res)
	res.SourceHealth = append(res.SourceHealth, borrowHealth)
	borrowFees, borrowFeeHealth, err := c.loadBorrowFees(ctx, now)
	res.SourceHealth = append(res.SourceHealth, borrowFeeHealth)
	if err != nil {
		res.WarningDetails = append(res.WarningDetails, marketEventSourceWarning("borrow_fee", err))
	} else {
		for _, sym := range symbols {
			if rec, ok := borrowFees.Symbols[sym]; ok {
				if flag, ok := marketEventBorrowFeeFlag(sym, rec, borrowFees, now); ok {
					res.Flags = append(res.Flags, flag)
				}
			}
		}
	}

	slices.SortFunc(res.Flags, func(a, b rpc.MarketEventFlag) int {
		if c := cmpMarketEventSeverity(a.Severity, b.Severity); c != 0 {
			return c
		}
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, flag := range res.Flags {
		res.BySymbol[flag.Symbol] = append(res.BySymbol[flag.Symbol], flag)
	}
	if len(res.BySymbol) == 0 {
		res.BySymbol = nil
	}
	res.Fingerprint = rpc.BuildMarketEventsFingerprint(&res)
	return res
}

func (c *marketEventCache) loadRegSHO(ctx context.Context, now time.Time) (marketEventRegSHOEntry, rpc.SourceHealth, error) {
	c.mu.Lock()
	if !c.regSHO.FetchedAt.IsZero() && now.Sub(c.regSHO.FetchedAt) <= c.regSHOFreshFor {
		entry := cloneRegSHOEntry(c.regSHO)
		c.mu.Unlock()
		return entry, marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusOK, entry.AsOf, now, marketEventsRegSHOMaxAge, "high", regSHOSourceNotes()), nil
	}
	c.mu.Unlock()

	entry, err := fetchLatestNasdaqRegSHO(ctx, now)
	if err != nil {
		c.mu.Lock()
		cached := cloneRegSHOEntry(c.regSHO)
		c.mu.Unlock()
		if len(cached.Symbols) > 0 {
			age := now.Sub(cached.FetchedAt)
			health := marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusStale, cached.AsOf, now, marketEventsRegSHOMaxAge, "medium-low", []string{"using stale cached Nasdaq Reg SHO threshold list: " + err.Error()})
			health.AgeSeconds = int64(age.Seconds())
			return cached, health, nil
		}
		return marketEventRegSHOEntry{}, marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusUnknown, now, now, marketEventsRegSHOMaxAge, "low", []string{err.Error()}), err
	}
	c.mu.Lock()
	c.regSHO = cloneRegSHOEntry(entry)
	c.mu.Unlock()
	return entry, marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusOK, entry.AsOf, now, marketEventsRegSHOMaxAge, "high", regSHOSourceNotes()), nil
}

func (c *marketEventCache) loadHalts(ctx context.Context, now time.Time) (marketEventHaltsEntry, rpc.SourceHealth, error) {
	c.mu.Lock()
	if !c.halts.FetchedAt.IsZero() && now.Sub(c.halts.FetchedAt) <= c.haltsFreshFor {
		entry := cloneHaltsEntry(c.halts)
		c.mu.Unlock()
		return entry, marketEventSourceHealth("trading_halts", rpc.SourceStatusOK, entry.AsOf, now, c.haltsFreshFor, "high", nil), nil
	}
	c.mu.Unlock()

	entry, err := fetchNasdaqTradeHalts(ctx)
	if err != nil {
		c.mu.Lock()
		cached := cloneHaltsEntry(c.halts)
		c.mu.Unlock()
		if len(cached.Records) > 0 {
			health := marketEventSourceHealth("trading_halts", rpc.SourceStatusStale, cached.AsOf, now, c.haltsFreshFor, "medium-low", []string{"using stale cached Nasdaq trade-halt RSS feed: " + err.Error()})
			health.AgeSeconds = int64(now.Sub(cached.FetchedAt).Seconds())
			return cached, health, nil
		}
		return marketEventHaltsEntry{}, marketEventSourceHealth("trading_halts", rpc.SourceStatusUnknown, now, now, c.haltsFreshFor, "low", []string{err.Error()}), err
	}
	entry.FetchedAt = now
	c.mu.Lock()
	c.halts = cloneHaltsEntry(entry)
	c.mu.Unlock()
	return entry, marketEventSourceHealth("trading_halts", rpc.SourceStatusOK, entry.AsOf, now, c.haltsFreshFor, "high", nil), nil
}

func (c *marketEventCache) loadBorrowFees(ctx context.Context, now time.Time) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	c.mu.Lock()
	if !c.borrowFees.FetchedAt.IsZero() && now.Sub(c.borrowFees.FetchedAt) <= marketEventsBorrowFeeFreshFor {
		entry := cloneBorrowFeeEntry(c.borrowFees)
		c.mu.Unlock()
		return entry, marketEventSourceHealth("borrow_fee", rpc.SourceStatusOK, entry.AsOf, now, marketEventsBorrowFeeMaxAge, "medium", []string{"IBKR short-stock availability fee rate"}), nil
	}
	c.mu.Unlock()

	entry, err := fetchIBKRBorrowFees(ctx)
	if err != nil {
		c.mu.Lock()
		cached := cloneBorrowFeeEntry(c.borrowFees)
		c.mu.Unlock()
		if len(cached.Symbols) > 0 {
			health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusStale, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "medium-low", []string{"using stale cached IBKR short-stock availability: " + err.Error()})
			health.AgeSeconds = int64(now.Sub(cached.FetchedAt).Seconds())
			return cached, health, nil
		}
		return marketEventBorrowFeeEntry{}, marketEventSourceHealth("borrow_fee", rpc.SourceStatusUnknown, now, now, marketEventsBorrowFeeMaxAge, "low", []string{err.Error()}), err
	}
	entry.FetchedAt = now
	c.mu.Lock()
	c.borrowFees = cloneBorrowFeeEntry(entry)
	c.mu.Unlock()
	return entry, marketEventSourceHealth("borrow_fee", rpc.SourceStatusOK, entry.AsOf, now, marketEventsBorrowFeeMaxAge, "medium", []string{"IBKR short-stock availability fee rate"}), nil
}

func fetchLatestNasdaqRegSHO(ctx context.Context, now time.Time) (marketEventRegSHOEntry, error) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	base := now.In(ny)
	var lastErr error
	for daysBack := range 8 {
		date := base.AddDate(0, 0, -daysBack)
		endpoint := "https://www.nasdaqtrader.com/dynamic/symdir/regsho/nasdaqth" + date.Format("20060102") + ".txt"
		entry, err := fetchNasdaqRegSHO(ctx, endpoint)
		if err == nil {
			if entry.AsOf.IsZero() {
				entry.AsOf = time.Date(date.Year(), date.Month(), date.Day(), 23, 0, 0, 0, ny).UTC()
			}
			return entry, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return marketEventRegSHOEntry{}, lastErr
	}
	return marketEventRegSHOEntry{}, fmt.Errorf("no Nasdaq Reg SHO threshold file found")
}

func fetchNasdaqRegSHO(ctx context.Context, endpoint string) (marketEventRegSHOEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return marketEventRegSHOEntry{}, err
	}
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	resp, err := marketEventsHTTPClient.Do(req)
	if err != nil {
		return marketEventRegSHOEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return marketEventRegSHOEntry{}, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}
	entry, err := parseNasdaqRegSHO(resp.Body)
	if err != nil {
		return marketEventRegSHOEntry{}, err
	}
	entry.SourceURL = endpoint
	return entry, nil
}

func parseNasdaqRegSHO(r io.Reader) (marketEventRegSHOEntry, error) {
	reader := csv.NewReader(r)
	reader.Comma = '|'
	reader.FieldsPerRecord = -1
	entry := marketEventRegSHOEntry{Symbols: map[string]marketEventRegSHORecord{}}
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return marketEventRegSHOEntry{}, fmt.Errorf("read Nasdaq Reg SHO row: %w", err)
		}
		if len(rec) == 1 {
			raw := strings.TrimSpace(rec[0])
			if len(raw) >= 14 {
				if ts, err := time.Parse("20060102150405", raw[:14]); err == nil {
					entry.AsOf = ts.UTC()
				}
			}
			continue
		}
		if len(rec) < 5 || strings.EqualFold(strings.TrimSpace(rec[0]), "Symbol") {
			continue
		}
		flag := strings.ToUpper(strings.TrimSpace(rec[3]))
		if flag != "Y" {
			continue
		}
		sym := normSym(rec[0])
		if sym == "" {
			continue
		}
		entry.Symbols[sym] = marketEventRegSHORecord{
			Symbol:         sym,
			SecurityName:   strings.TrimSpace(rec[1]),
			MarketCategory: strings.TrimSpace(rec[2]),
			Rule3210:       strings.TrimSpace(rec[4]),
		}
	}
	return entry, nil
}

func fetchIBKRBorrowFeesFTP(ctx context.Context) (marketEventBorrowFeeEntry, error) {
	const endpoint = "ftp://ftp3.interactivebrokers.com/usa.txt"
	body, err := fetchFTPFile(ctx, "ftp3.interactivebrokers.com:21", "shortstock", "", "usa.txt")
	if err != nil {
		return marketEventBorrowFeeEntry{}, err
	}
	entry, err := parseIBKRBorrowFees(strings.NewReader(body))
	if err != nil {
		return marketEventBorrowFeeEntry{}, err
	}
	entry.SourceURL = endpoint
	return entry, nil
}

func parseIBKRBorrowFees(r io.Reader) (marketEventBorrowFeeEntry, error) {
	reader := csv.NewReader(r)
	reader.Comma = '|'
	reader.FieldsPerRecord = -1
	entry := marketEventBorrowFeeEntry{Symbols: map[string]marketEventBorrowFeeRecord{}}
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return marketEventBorrowFeeEntry{}, fmt.Errorf("read IBKR borrow-fee row: %w", err)
		}
		if len(rec) == 0 {
			continue
		}
		tag := strings.TrimSpace(rec[0])
		switch {
		case tag == "#BOF":
			if len(rec) >= 3 {
				entry.AsOf = parseIBKRBorrowFeeAsOf(rec[1], rec[2])
			}
			continue
		case strings.HasPrefix(tag, "#"):
			continue
		}
		if len(rec) < 8 {
			continue
		}
		sym := normSym(rec[0])
		if sym == "" {
			continue
		}
		feeRate, feeOK := parseFloatField(rec[6])
		if !feeOK {
			continue
		}
		rebateRate, _ := parseFloatField(rec[5])
		available, _ := parseIntField(rec[7])
		entry.Symbols[sym] = marketEventBorrowFeeRecord{
			Symbol:     sym,
			Currency:   strings.TrimSpace(rec[1]),
			Name:       strings.TrimSpace(rec[2]),
			ConID:      strings.TrimSpace(rec[3]),
			ISIN:       strings.TrimSpace(rec[4]),
			RebateRate: rebateRate,
			FeeRate:    feeRate,
			Available:  available,
		}
	}
	return entry, nil
}

func parseIBKRBorrowFeeAsOf(rawDate, rawTime string) time.Time {
	raw := strings.TrimSpace(rawDate) + " " + strings.TrimSpace(rawTime)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	if t, err := time.ParseInLocation("2006.01.02 15:04:05", raw, ny); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func fetchFTPFile(ctx context.Context, addr, user, pass, path string) (string, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	control, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer control.Close()
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = control.SetDeadline(deadline)
	reader := bufio.NewReader(control)
	if code, line, err := readFTPResponse(reader); err != nil || code != 220 {
		return "", fmt.Errorf("FTP greeting: %s: %w", line, err)
	}
	if err := writeFTPCommand(control, "USER "+user); err != nil {
		return "", err
	}
	code, line, err := readFTPResponse(reader)
	if err != nil {
		return "", fmt.Errorf("FTP USER: %w", err)
	}
	if code == 331 {
		if err := writeFTPCommand(control, "PASS "+pass); err != nil {
			return "", err
		}
		code, line, err = readFTPResponse(reader)
		if err != nil {
			return "", fmt.Errorf("FTP PASS: %w", err)
		}
	}
	if code != 230 {
		return "", fmt.Errorf("FTP login failed: %d %s", code, line)
	}
	if err := writeFTPCommand(control, "TYPE I"); err != nil {
		return "", err
	}
	if code, line, err := readFTPResponse(reader); err != nil || code != 200 {
		return "", fmt.Errorf("FTP TYPE I: %d %s: %w", code, line, err)
	}
	if err := writeFTPCommand(control, "PASV"); err != nil {
		return "", err
	}
	code, line, err = readFTPResponse(reader)
	if err != nil || code != 227 {
		return "", fmt.Errorf("FTP PASV: %d %s: %w", code, line, err)
	}
	dataAddr, err := ftpPassiveAddr(line)
	if err != nil {
		return "", err
	}
	data, err := dialer.DialContext(ctx, "tcp", dataAddr)
	if err != nil {
		return "", err
	}
	_ = data.SetDeadline(deadline)
	if err := writeFTPCommand(control, "RETR "+path); err != nil {
		data.Close()
		return "", err
	}
	code, line, err = readFTPResponse(reader)
	if err != nil {
		data.Close()
		return "", fmt.Errorf("FTP RETR: %w", err)
	}
	if code != 125 && code != 150 {
		data.Close()
		return "", fmt.Errorf("FTP RETR failed: %d %s", code, line)
	}
	body, readErr := io.ReadAll(data)
	closeErr := data.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	code, line, err = readFTPResponse(reader)
	if err != nil || code != 226 {
		return "", fmt.Errorf("FTP transfer complete: %d %s: %w", code, line, err)
	}
	_ = writeFTPCommand(control, "QUIT")
	return string(body), nil
}

func readFTPResponse(reader *bufio.Reader) (int, string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 3 {
		return 0, line, fmt.Errorf("short FTP response")
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, line, err
	}
	if len(line) > 3 && line[3] == '-' {
		prefix := line[:3] + " "
		for {
			next, err := reader.ReadString('\n')
			if err != nil {
				return code, line, err
			}
			next = strings.TrimRight(next, "\r\n")
			line += "\n" + next
			if strings.HasPrefix(next, prefix) {
				break
			}
		}
	}
	return code, line, nil
}

func writeFTPCommand(conn net.Conn, cmd string) error {
	_, err := fmt.Fprintf(conn, "%s\r\n", cmd)
	return err
}

func ftpPassiveAddr(line string) (string, error) {
	match := regexp.MustCompile(`\((\d+),(\d+),(\d+),(\d+),(\d+),(\d+)\)`).FindStringSubmatch(line)
	if len(match) != 7 {
		return "", fmt.Errorf("parse PASV address from %q", line)
	}
	parts := make([]int, 6)
	for i := 1; i < len(match); i++ {
		v, err := strconv.Atoi(match[i])
		if err != nil {
			return "", err
		}
		parts[i-1] = v
	}
	host := net.IPv4(byte(parts[0]), byte(parts[1]), byte(parts[2]), byte(parts[3])).String()
	port := parts[4]*256 + parts[5]
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func parseFloatField(raw string) (float64, bool) {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil
}

func parseIntField(raw string) (int64, bool) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, ",", ""))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	return v, err == nil
}

func regSHOSourceNotes() []string {
	return []string{"Nasdaq-listed threshold securities source; non-Nasdaq listing-exchange threshold feeds remain outside V1."}
}

func fetchNasdaqTradeHalts(ctx context.Context) (marketEventHaltsEntry, error) {
	const endpoint = "https://www.nasdaqtrader.com/rss.aspx?feed=tradehalts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return marketEventHaltsEntry{}, err
	}
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	resp, err := marketEventsHTTPClient.Do(req)
	if err != nil {
		return marketEventHaltsEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return marketEventHaltsEntry{}, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}
	entry, err := parseNasdaqTradeHalts(resp.Body)
	if err != nil {
		return marketEventHaltsEntry{}, err
	}
	entry.SourceURL = endpoint
	return entry, nil
}

type nasdaqTradeHaltsRSS struct {
	Channel nasdaqTradeHaltsChannel `xml:"channel"`
}

type nasdaqTradeHaltsChannel struct {
	PubDate string                 `xml:"pubDate"`
	Items   []nasdaqTradeHaltsItem `xml:"item"`
}

type nasdaqTradeHaltsItem struct {
	HaltDate            string `xml:"HaltDate"`
	HaltTime            string `xml:"HaltTime"`
	IssueSymbol         string `xml:"IssueSymbol"`
	IssueName           string `xml:"IssueName"`
	Market              string `xml:"Market"`
	ReasonCode          string `xml:"ReasonCode"`
	PauseThresholdPrice string `xml:"PauseThresholdPrice"`
	ResumptionDate      string `xml:"ResumptionDate"`
	ResumptionQuoteTime string `xml:"ResumptionQuoteTime"`
	ResumptionTradeTime string `xml:"ResumptionTradeTime"`
}

func parseNasdaqTradeHalts(r io.Reader) (marketEventHaltsEntry, error) {
	var feed nasdaqTradeHaltsRSS
	decoder := xml.NewDecoder(r)
	if err := decoder.Decode(&feed); err != nil {
		return marketEventHaltsEntry{}, fmt.Errorf("decode Nasdaq trade halt RSS: %w", err)
	}
	entry := marketEventHaltsEntry{}
	if pubDate := strings.TrimSpace(feed.Channel.PubDate); pubDate != "" {
		if t, err := time.Parse(time.RFC1123, pubDate); err == nil {
			entry.AsOf = t.UTC()
		} else if t, err := time.Parse(time.RFC1123Z, pubDate); err == nil {
			entry.AsOf = t.UTC()
		}
	}
	for _, item := range feed.Channel.Items {
		sym := normSym(item.IssueSymbol)
		if sym == "" {
			continue
		}
		rec := marketEventHaltRecord{
			Symbol:              sym,
			IssueName:           strings.TrimSpace(item.IssueName),
			Market:              strings.TrimSpace(item.Market),
			ReasonCode:          strings.ToUpper(strings.TrimSpace(item.ReasonCode)),
			PauseThresholdPrice: strings.TrimSpace(item.PauseThresholdPrice),
		}
		rec.HaltedAt = parseNasdaqHaltTime(item.HaltDate, item.HaltTime)
		rec.ResumptionQuoteAt = parseNasdaqHaltTime(item.ResumptionDate, item.ResumptionQuoteTime)
		rec.ResumptionTradeAt = parseNasdaqHaltTime(item.ResumptionDate, item.ResumptionTradeTime)
		entry.Records = append(entry.Records, rec)
	}
	return entry, nil
}

func parseNasdaqHaltTime(rawDate, rawTime string) time.Time {
	rawDate = strings.TrimSpace(rawDate)
	rawTime = strings.TrimSpace(rawTime)
	if rawDate == "" || rawTime == "" {
		return time.Time{}
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	for _, layout := range []string{"01/02/2006 15:04:05.000", "01/02/2006 15:04:05"} {
		if t, err := time.ParseInLocation(layout, rawDate+" "+rawTime, ny); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func marketEventRegSHOFlag(sym string, rec marketEventRegSHORecord, source marketEventRegSHOEntry, now time.Time) rpc.MarketEventFlag {
	return rpc.MarketEventFlag{
		ID:         rpc.MarketEventRegSHOThreshold,
		Symbol:     sym,
		Label:      "Reg SHO",
		Status:     rpc.MarketEventStatusActive,
		Severity:   rpc.MarketEventSeverityWatch,
		Role:       rpc.MarketEventRoleContext,
		Source:     "Nasdaq Reg SHO threshold list",
		SourceURL:  source.SourceURL,
		AsOf:       source.AsOf,
		ObservedAt: now,
		Details: compactNonEmptyStrings(
			"threshold security",
			"market_category="+rec.MarketCategory,
			"rule_3210="+rec.Rule3210,
			rec.SecurityName,
		),
	}
}

func marketEventHaltFlag(sym string, rec marketEventHaltRecord, source marketEventHaltsEntry, now time.Time) (rpc.MarketEventFlag, bool) {
	status := rpc.MarketEventStatusActive
	if !rec.ResumptionTradeAt.IsZero() {
		if now.Sub(rec.ResumptionTradeAt) > marketEventsRecentHaltWindow {
			return rpc.MarketEventFlag{}, false
		}
		status = rpc.MarketEventStatusRecent
	}
	id := rpc.MarketEventHaltRegulatoryOrNews
	label := "Halt"
	severity := rpc.MarketEventSeverityBlock
	role := rpc.MarketEventRoleHardBlocker
	if status == rpc.MarketEventStatusRecent {
		severity = rpc.MarketEventSeverityWatch
		role = rpc.MarketEventRoleProposalModifier
	}
	if marketEventLULDReason(rec.ReasonCode) {
		id = rpc.MarketEventLULDRecent
		label = "LULD"
		if status == rpc.MarketEventStatusActive {
			label = "LULD active"
		} else {
			label = "LULD recent"
		}
	}
	flag := rpc.MarketEventFlag{
		ID:         id,
		Symbol:     sym,
		Label:      label,
		Status:     status,
		Severity:   severity,
		Role:       role,
		Source:     "Nasdaq trade halt RSS",
		SourceURL:  source.SourceURL,
		AsOf:       source.AsOf,
		ObservedAt: now,
		Details: compactNonEmptyStrings(
			"reason_code="+rec.ReasonCode,
			rec.IssueName,
			rec.Market,
			"pause_threshold="+rec.PauseThresholdPrice,
		),
	}
	if status == rpc.MarketEventStatusActive {
		flag.ExpiresAt = rec.ResumptionTradeAt
	}
	return flag, true
}

func marketEventLULDReason(reason string) bool {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "M", "T7":
		return true
	default:
		return false
	}
}

func marketEventBorrowInventory(ctx context.Context, symbols []string, subs *subManager, connector *ibkrlib.Connector, now time.Time, res *rpc.MarketEventsResult) rpc.SourceHealth {
	if connector == nil || subs == nil {
		return marketEventSourceHealth("borrow_inventory", rpc.SourceStatusUnknown, now, now, 2*time.Minute, "low", []string{"IBKR gateway is unavailable; shortable-share inventory is unknown"})
	}
	var observed, tight int
	for _, sym := range symbols {
		holdCtx, cancel := context.WithTimeout(ctx, marketEventsBorrowPollBudget)
		release, err := subs.Hold(holdCtx, sym)
		if err == nil {
			_ = pollMarketData(holdCtx, connector, sym, time.Now().Add(marketEventsBorrowPollBudget), func(md *ibkrlib.MarketData) bool {
				return md.ShortableObserved
			})
			if md := connector.GetMarketData()[sym]; md != nil && md.ShortableObserved {
				observed++
				if flag, ok := marketEventBorrowInventoryFlag(sym, *md, now); ok {
					tight++
					res.Flags = append(res.Flags, flag)
				}
			}
			release()
		}
		cancel()
	}
	status := rpc.SourceStatusUnknown
	confidence := "low"
	notes := []string{"shortable-share tick did not arrive for requested symbols"}
	if observed > 0 {
		status = rpc.SourceStatusOK
		confidence = "medium"
		notes = []string{fmt.Sprintf("observed shortable-share inventory for %d/%d symbols", observed, len(symbols))}
		if tight == 0 {
			notes = append(notes, "no tight borrow-inventory flags crossed V1 thresholds")
		}
	}
	return marketEventSourceHealth("borrow_inventory", status, now, now, 2*time.Minute, confidence, notes)
}

func marketEventBorrowInventoryFlag(sym string, md ibkrlib.MarketData, now time.Time) (rpc.MarketEventFlag, bool) {
	if !md.ShortableObserved || md.ShortableShares > marketEventsBorrowTightShares {
		return rpc.MarketEventFlag{}, false
	}
	severity := rpc.MarketEventSeverityWatch
	label := "Borrow tight"
	if md.ShortableShares <= marketEventsBorrowExtremeShares {
		severity = rpc.MarketEventSeverityAct
		label = "Borrow scarce"
	}
	value := float64(md.ShortableShares)
	return rpc.MarketEventFlag{
		ID:         rpc.MarketEventBorrowInventoryTight,
		Symbol:     sym,
		Label:      label,
		Status:     rpc.MarketEventStatusActive,
		Severity:   severity,
		Role:       rpc.MarketEventRoleProposalModifier,
		Source:     "IBKR generic tick 236",
		AsOf:       md.Timestamp,
		ObservedAt: now,
		Value:      &value,
		Unit:       "shares",
		Details:    []string{"shortable_shares=" + strconv.FormatInt(md.ShortableShares, 10)},
	}, true
}

func marketEventBorrowFeeFlag(sym string, rec marketEventBorrowFeeRecord, source marketEventBorrowFeeEntry, now time.Time) (rpc.MarketEventFlag, bool) {
	if rec.FeeRate < marketEventsBorrowFeeExtremePct {
		return rpc.MarketEventFlag{}, false
	}
	value := rec.FeeRate
	return rpc.MarketEventFlag{
		ID:         rpc.MarketEventBorrowFeeExtreme,
		Symbol:     sym,
		Label:      "Fee extreme",
		Status:     rpc.MarketEventStatusActive,
		Severity:   rpc.MarketEventSeverityAct,
		Role:       rpc.MarketEventRoleProposalModifier,
		Source:     "IBKR short stock availability",
		SourceURL:  source.SourceURL,
		AsOf:       source.AsOf,
		ObservedAt: now,
		Value:      &value,
		Unit:       "pct_annualized",
		Details: compactNonEmptyStrings(
			fmt.Sprintf("fee_rate=%.4f%%", rec.FeeRate),
			fmt.Sprintf("rebate_rate=%.4f%%", rec.RebateRate),
			"available="+strconv.FormatInt(rec.Available, 10),
			rec.Currency,
			rec.Name,
		),
	}, true
}

func marketEventSourceHealth(source, status string, asOf, now time.Time, maxAge time.Duration, confidence string, notes []string) rpc.SourceHealth {
	health := rpc.SourceHealth{
		Source:               source,
		Status:               status,
		AsOf:                 asOf,
		MaxAgeSeconds:        int64(maxAge.Seconds()),
		Confidence:           confidence,
		FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
		Notes:                notes,
	}
	if !asOf.IsZero() && !now.IsZero() {
		health.AgeSeconds = int64(now.Sub(asOf).Seconds())
	}
	return health
}

func marketEventSourceWarning(scope string, err error) rpc.DataWarning {
	return rpc.DataWarning{
		Code:     scope + "_unavailable",
		Scope:    scope,
		Severity: "data_quality",
		Message:  "Market-event source is unavailable: " + err.Error(),
		Impact:   "The corresponding flag remains unknown, not inactive.",
		Action:   "Retry later or inspect source health before relying on absence of this flag.",
	}
}

func normalizeMarketEventSymbols(raw []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, token := range raw {
		for part := range strings.SplitSeq(token, ",") {
			sym := normSym(part)
			if sym == "" || seen[sym] {
				continue
			}
			seen[sym] = true
			out = append(out, sym)
		}
	}
	slices.Sort(out)
	return out
}

func marketEventSymbolsFromPositions(pos *rpc.PositionsResult) []string {
	if pos == nil {
		return nil
	}
	var raw []string
	for _, stock := range pos.Stocks {
		raw = append(raw, stock.Symbol)
	}
	for _, group := range pos.ByUnderlying {
		raw = append(raw, group.Underlying)
	}
	return normalizeMarketEventSymbols(raw)
}

func cloneRegSHOEntry(in marketEventRegSHOEntry) marketEventRegSHOEntry {
	out := in
	if in.Symbols != nil {
		out.Symbols = make(map[string]marketEventRegSHORecord, len(in.Symbols))
		maps.Copy(out.Symbols, in.Symbols)
	}
	return out
}

func cloneHaltsEntry(in marketEventHaltsEntry) marketEventHaltsEntry {
	out := in
	out.Records = slices.Clone(in.Records)
	return out
}

func cloneBorrowFeeEntry(in marketEventBorrowFeeEntry) marketEventBorrowFeeEntry {
	out := in
	if in.Symbols != nil {
		out.Symbols = make(map[string]marketEventBorrowFeeRecord, len(in.Symbols))
		maps.Copy(out.Symbols, in.Symbols)
	}
	return out
}

func compactNonEmptyStrings(values ...string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.HasSuffix(value, "=") {
			continue
		}
		out = append(out, value)
	}
	return out
}

func cmpMarketEventSeverity(a, b string) int {
	rank := func(v string) int {
		switch v {
		case rpc.MarketEventSeverityBlock:
			return 0
		case rpc.MarketEventSeverityAct:
			return 1
		case rpc.MarketEventSeverityWatch:
			return 2
		default:
			return 3
		}
	}
	return rank(a) - rank(b)
}
