package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
)

// Resource template URIs exposed by the streaming MCP surface. Two templates
// because stocks and options have different shape — see Q5 in the design
// session: `ibkr://quote/{symbol}` for stocks, an explicit option template
// for option contracts so URI templates stay shape-pure.
const (
	StockQuoteURITemplate  = "ibkr://quote/{symbol}"
	OptionQuoteURITemplate = "ibkr://option/{symbol}/{expiry}/{right}/{strike}"

	stockQuoteScheme  = "ibkr://quote/"
	optionQuoteScheme = "ibkr://option/"
)

// ResourceTemplate is the wire shape returned by resources/templates/list.
// Mirrors the MCP spec's resourceTemplate object: URI template (RFC 6570
// flavor), human name, mimeType for the read response.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType"`
}

// ResourceTemplates is the canonical streaming-resource inventory. The
// TestStreamingParity gate asserts this list is exactly what we expose,
// so a contributor adding a new template (or renaming one) updates the
// test in lockstep.
var ResourceTemplates = []ResourceTemplate{
	{
		URITemplate: StockQuoteURITemplate,
		Name:        "stock-quote",
		Description: "Live snapshot + streaming quote for an equity / ETF symbol. resources/read returns the latest tick; resources/subscribe streams coalesced ticks until unsubscribe.",
		MIMEType:    "application/json",
	},
	{
		URITemplate: OptionQuoteURITemplate,
		Name:        "option-quote",
		Description: "Live snapshot + streaming quote for an option contract. expiry is YYMMDD; right is C or P; strike is the numeric strike price.",
		MIMEType:    "application/json",
	},
}

// parsedURI is what URI parsing produces: the symbol the daemon
// subscribes against (always the synthetic SYMBOL or SYMBOL_YYMMDDC|PSTRIKE
// form the rest of the daemon already speaks).
type parsedURI struct {
	// Sym is the canonical symbol passed to the daemon's subscribe RPC.
	// For stocks, it's the bare ticker (uppercase). For options, the
	// synthetic AAPL_240119C00195000-style identifier the rest of the
	// daemon uses internally.
	Sym string

	// IsOption is true for OptionQuoteURITemplate URIs.
	IsOption bool

	// OriginalURI is preserved for use as the resource notification
	// target — MCP clients track subscriptions by URI string.
	OriginalURI string
}

// parseQuoteURI accepts a fully-qualified MCP resource URI and returns the
// canonical symbol the daemon expects, or an error suitable for surfacing
// as an MCP invalid-params response.
//
// Accepted shapes:
//   - ibkr://quote/AAPL                              → stock, sym="AAPL"
//   - ibkr://option/AAPL/240119/C/195                → option, sym="AAPL_240119C195"
//   - ibkr://option/AAPL/240119/P/195.5              → option, sym="AAPL_240119P195.5" (decimals OK)
func parseQuoteURI(uri string) (parsedURI, error) {
	uri = strings.TrimSpace(uri)
	switch {
	case strings.HasPrefix(uri, stockQuoteScheme):
		rest := uri[len(stockQuoteScheme):]
		if rest == "" || strings.Contains(rest, "/") {
			return parsedURI{}, fmt.Errorf("invalid stock quote URI %q: expected %s", uri, StockQuoteURITemplate)
		}
		sym := strings.ToUpper(rest)
		return parsedURI{Sym: sym, OriginalURI: uri}, nil

	case strings.HasPrefix(uri, optionQuoteScheme):
		rest := uri[len(optionQuoteScheme):]
		parts := strings.Split(rest, "/")
		if len(parts) != 4 {
			return parsedURI{}, fmt.Errorf("invalid option quote URI %q: expected %s", uri, OptionQuoteURITemplate)
		}
		symbol, expiry, right, strikeStr := strings.ToUpper(parts[0]), parts[1], strings.ToUpper(parts[2]), parts[3]
		if symbol == "" {
			return parsedURI{}, fmt.Errorf("invalid option quote URI %q: missing symbol", uri)
		}
		if len(expiry) != 6 {
			return parsedURI{}, fmt.Errorf("invalid option quote URI %q: expiry must be YYMMDD", uri)
		}
		if right != "C" && right != "P" {
			return parsedURI{}, fmt.Errorf("invalid option quote URI %q: right must be C or P", uri)
		}
		// Strike validation: must parse as a positive number. We don't
		// reformat it (preserve user-supplied precision) — the daemon's
		// existing option-symbol path tolerates either integer or decimal
		// strike strings.
		strike, err := strconv.ParseFloat(strikeStr, 64)
		if err != nil || strike <= 0 {
			return parsedURI{}, fmt.Errorf("invalid option quote URI %q: strike must be a positive number", uri)
		}
		// Synthetic key matches the format runQuoteOption builds:
		//   fmt.Sprintf("%s_%s%s%.0f", symbol, expiry, right, strike)
		// Stripping the trailing .0 keeps integer-only strikes clean
		// (AAPL_240119C195) and uses a normal float string for fractional
		// strikes — daemon's downstream parser accepts both.
		var sym string
		if strike == float64(int64(strike)) {
			sym = fmt.Sprintf("%s_%s%s%d", symbol, expiry, right, int64(strike))
		} else {
			sym = fmt.Sprintf("%s_%s%s%s", symbol, expiry, right, strikeStr)
		}
		return parsedURI{Sym: sym, IsOption: true, OriginalURI: uri}, nil

	default:
		return parsedURI{}, fmt.Errorf("unrecognised resource URI %q (expected one of: %s, %s)", uri, StockQuoteURITemplate, OptionQuoteURITemplate)
	}
}

// uriContainsTradingVerb is the safety counterpart for resource URIs,
// parallel to TestNoTradingTools' check on tool names. Catches a
// contributor adding a `ibkr://order/...` template by mistake.
func uriContainsTradingVerb(uri string) (bool, string) {
	low := strings.ToLower(uri)
	for _, banned := range []string{"order", "trade", "cancel", "submit", "place"} {
		if strings.Contains(low, banned) {
			return true, banned
		}
	}
	return false, ""
}

// resourcesListResult is the wire shape for resources/list. We don't
// enumerate concrete resources (would be infinite — every symbol is a
// resource), so this is always empty; templates carry the actual surface.
type resourcesListResult struct {
	Resources []json.RawMessage `json:"resources"`
}

// resourcesTemplatesListResult is the wire shape for resources/templates/list.
type resourcesTemplatesListResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
}

// resourceReadResult is the wire shape for resources/read responses.
// Contents always carries one entry — the snapshot frame as a JSON string
// in a `text` block — matching the embedded-content notification format
// resources/subscribe later uses.
type resourceReadResult struct {
	Contents []resourceContent `json:"contents"`
}

// resourceSubscribeParams is the input to resources/subscribe and
// resources/unsubscribe.
type resourceSubscribeParams struct {
	URI string `json:"uri"`
}

// resourceContent is the per-content-block shape used in both reads and
// subscription notifications. text carries the JSON-stringified Frame.
type resourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text"`
}

// snapshotReadTimeout bounds how long resources/read waits for the daemon
// to deliver a snapshot quote. Generous because a cold subscribe against
// an unfamiliar symbol can take a few seconds for the gateway to deliver
// the first ticks.
const snapshotReadTimeout = 5 * time.Second

// handleResourcesList satisfies MCP's resources/list. We don't enumerate
// concrete resources because every symbol is implicitly a resource — the
// inventory would be unbounded. Clients discover the surface via
// resources/templates/list and parametrize URIs themselves.
func (s *Server) handleResourcesList(id json.RawMessage) {
	res := resourcesListResult{Resources: []json.RawMessage{}}
	b, _ := json.Marshal(res)
	s.writeResult(id, b)
}

// handleResourcesTemplatesList returns the URI templates exposed by this
// server. Stable across daemon restarts; gated by TestStreamingParity so
// drift is caught at build time.
func (s *Server) handleResourcesTemplatesList(id json.RawMessage) {
	res := resourcesTemplatesListResult{ResourceTemplates: ResourceTemplates}
	b, _ := json.Marshal(res)
	s.writeResult(id, b)
}

// handleResourcesRead returns a current snapshot quote for the given URI.
// Contents always carries one entry — the Quote JSON in a text block —
// matching the embedded-content notification shape resources/subscribe uses.
func (s *Server) handleResourcesRead(ctx context.Context, id, params json.RawMessage) {
	var p resourceSubscribeParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	pu, err := parseQuoteURI(p.URI)
	if err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}

	readCtx, cancel := context.WithTimeout(ctx, snapshotReadTimeout)
	defer cancel()

	snapParams := rpc.QuoteSnapshotParams{
		Contract:  rpc.ContractParams{Symbol: pu.Sym, SecType: "STK", Currency: "USD"},
		TimeoutMs: int(snapshotReadTimeout.Milliseconds()),
	}
	var quote rpc.Quote
	if err := s.conn.Call(readCtx, rpc.MethodQuoteSnapshot, snapParams, &quote); err != nil {
		s.writeError(id, codeInternalError, err.Error())
		return
	}
	body, err := json.Marshal(quote)
	if err != nil {
		s.writeError(id, codeInternalError, err.Error())
		return
	}
	res := resourceReadResult{
		Contents: []resourceContent{{
			URI:      pu.OriginalURI,
			MIMEType: "application/json",
			Text:     string(body),
		}},
	}
	out, _ := json.Marshal(res)
	s.writeResult(id, out)
}

// handleResourcesSubscribe acks the subscription synchronously, opens a
// dedicated daemon connection, and spawns a goroutine that translates each
// streaming Frame into a notifications/resources/updated message with the
// frame JSON embedded in params.contents. Idempotent on the URI: a duplicate
// subscribe to a URI we already serve is a no-op success.
//
// Per the H semantics from the design session, no transparent reconnect:
// when the daemon closes the stream (gateway lost, daemon shutdown, …)
// the goroutine emits a final terminal-error notification and exits. The
// MCP client decides whether to retry.
func (s *Server) handleResourcesSubscribe(id, params json.RawMessage) {
	var p resourceSubscribeParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	pu, err := parseQuoteURI(p.URI)
	if err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	if s.dialer == nil {
		s.writeError(id, codeInternalError, "streaming subscriptions not configured on this server")
		return
	}

	s.subMu.Lock()
	if _, exists := s.subs[pu.OriginalURI]; exists {
		// Idempotent: already subscribed. Ack and move on.
		s.subMu.Unlock()
		s.writeResult(id, json.RawMessage(`{}`))
		return
	}
	s.subMu.Unlock()

	// Open a dedicated daemon conn for this subscription so it doesn't
	// contend with unary tools/call traffic on the shared conn. dial.Conn
	// holds the per-conn mutex for the stream's whole lifetime.
	daemonConn, err := s.dialer()
	if err != nil {
		s.writeError(id, codeInternalError, fmt.Sprintf("dial daemon: %v", err))
		return
	}

	// Parent the streaming context on the Serve()-scoped context so a
	// client EOF (or ctx cancel from the outer process) propagates to
	// every active subscription without relying solely on
	// shutdownSubscriptions() draining the map. Falls back to Background
	// only if Serve hasn't initialized yet — defensive, not a path that
	// fires in practice.
	parent := s.serveCtx
	if parent == nil {
		parent = context.Background()
	}
	streamCtx, cancel := context.WithCancel(parent)
	s.subMu.Lock()
	s.subs[pu.OriginalURI] = cancel
	s.subMu.Unlock()

	go s.runResourceSubscription(streamCtx, pu, daemonConn)

	s.writeResult(id, json.RawMessage(`{}`))
}

// handleResourcesUnsubscribe cancels the named subscription. Idempotent
// against an unknown URI — silently succeeds, matching the spec's "client
// should not have to know subscription state precisely" posture.
func (s *Server) handleResourcesUnsubscribe(id, params json.RawMessage) {
	var p resourceSubscribeParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	s.subMu.Lock()
	cancel, exists := s.subs[p.URI]
	if exists {
		delete(s.subs, p.URI)
	}
	s.subMu.Unlock()
	if exists {
		cancel()
	}
	s.writeResult(id, json.RawMessage(`{}`))
}

// runResourceSubscription owns one streaming subscription's lifecycle.
// Opens a Stream against the daemon's MethodQuoteSubscribe, translates each
// Frame into an MCP resources/updated notification, and on stream end (for
// any reason) cleans up its subs entry and closes the daemon conn.
//
// On transport-level errors (daemon socket dropped, daemon shutdown) the
// daemon may not have delivered an in-band Frame.Error — emit a synthetic
// terminal frame so the MCP client always gets a structured signal.
func (s *Server) runResourceSubscription(ctx context.Context, pu parsedURI, conn *dial.Conn) {
	defer conn.Close()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, pu.OriginalURI)
		s.subMu.Unlock()
	}()

	subParams := rpc.QuoteSubscribeParams{
		Contract: rpc.ContractParams{Symbol: pu.Sym, SecType: "STK", Currency: "USD"},
	}

	streamErr := conn.Stream(ctx, rpc.MethodQuoteSubscribe, subParams, func(raw json.RawMessage) error {
		s.emitResourceUpdate(pu.OriginalURI, raw)
		return nil
	})

	// The daemon emits a terminal Frame.Error before closing the stream
	// for known scenarios (gateway_lost, daemon_shutdown). For transport-
	// level failures (socket dropped without {end:true}) we synthesize an
	// error frame so the consumer sees a structured terminator.
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		code := rpc.FrameErrSubscriptionRejected
		if rpcErr, ok := streamErr.(*rpc.Error); ok && rpcErr.Code == rpc.CodeGatewayUnavailable {
			code = rpc.FrameErrGatewayLost
		}
		frame := rpc.Frame{
			T:     time.Now(),
			Error: &rpc.FrameError{Code: code, Message: streamErr.Error()},
		}
		raw, _ := json.Marshal(frame)
		s.emitResourceUpdate(pu.OriginalURI, raw)
	}
}

// emitResourceUpdate writes a notifications/resources/updated message with
// the frame JSON embedded in params.contents. The empty extension key
// (`contents`) is the de-facto standard for shipping live content with the
// notification — the MCP spec is permissive.
func (s *Server) emitResourceUpdate(uri string, frameJSON json.RawMessage) {
	payload := map[string]any{
		"uri": uri,
		"contents": []resourceContent{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(frameJSON),
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.writeNotification("notifications/resources/updated", body)
}

// writeNotification is the JSON-RPC notification counterpart to writeResult
// (no id). Serializes alongside other writes via s.mu so notifications
// don't interleave with response bytes.
func (s *Server) writeNotification(method string, params json.RawMessage) {
	notif := struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	b, err := json.Marshal(notif)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		return
	}
	_, _ = s.out.Write(b)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

// shutdownSubscriptions cancels every active subscription. Called by the
// MCP server's main Serve loop on EOF / context cancel so the per-sub
// goroutines unwind and the daemon-side socket connections close, which
// in turn triggers the daemon's subManager refcount decrement.
func (s *Server) shutdownSubscriptions() {
	s.subMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.subs))
	for _, c := range s.subs {
		cancels = append(cancels, c)
	}
	s.subs = map[string]context.CancelFunc{}
	s.subMu.Unlock()
	for _, c := range cancels {
		c()
	}
}
