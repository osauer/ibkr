// Package mcp serves the `ibkr` daemon's read-only RPC surface as an MCP
// (Model Context Protocol) server over stdio. Spoken by Claude Desktop and
// any other local MCP client; the tool inventory mirrors the CLI 1:1 and is
// gated by a parity test (see tools_test.go).
//
// Wire: newline-delimited JSON-RPC 2.0 over stdin/stdout, no framing headers.
// The MCP lifecycle is initialize → initialized (notification) → repeated
// tools/list + tools/call → EOF on stdin shuts the server down.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/osauer/ibkr/internal/dial"
)

// ProtocolVersion is the MCP spec revision we advertise. 2025-03-26 is the
// stable revision Claude Desktop and the official Go/TypeScript SDKs target.
const ProtocolVersion = "2025-03-26"

// Server hosts the MCP loop. One per process; safe to reuse Conn across
// requests because the daemon serializes each Call internally. Streaming
// resource subscriptions open additional daemon connections via dialer
// because dial.Conn.Stream holds the per-connection mutex for the stream's
// lifetime — multiplexing is left as a future optimization.
type Server struct {
	conn    *dial.Conn
	version string

	mu  sync.Mutex // serializes writes to out
	out *bufio.Writer

	// dialer is the function used to open additional daemon connections
	// for streaming resource subscriptions. Set via SetDialer; nil means
	// resources/subscribe is unsupported on this server (returns an
	// internal-error response).
	dialer func() (*dial.Conn, error)

	// subs tracks active resource subscriptions, keyed by URI string. The
	// CancelFunc tears down the per-subscription goroutine and the
	// underlying daemon conn. Guarded by subMu.
	subMu sync.Mutex
	subs  map[string]context.CancelFunc
}

// NewServer wires the MCP server to a live daemon connection and the version
// string the binary was built with (stamped via -ldflags).
func NewServer(conn *dial.Conn, version string) *Server {
	return &Server{
		conn:    conn,
		version: version,
		subs:    map[string]context.CancelFunc{},
	}
}

// SetDialer wires the function used to open additional daemon connections
// for streaming resource subscriptions. Required for resources/subscribe to
// work. Without it, the server still serves tools and resources/read but
// reports streaming as unsupported.
func (s *Server) SetDialer(d func() (*dial.Conn, error)) {
	s.dialer = d
}

// Serve runs the MCP loop until in returns io.EOF (client disconnect) or
// ctx is cancelled. Returns nil on clean shutdown. Active resource
// subscriptions are cancelled before return so per-sub goroutines unwind
// and the daemon-side refcount decrements promptly.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = bufio.NewWriter(out)
	defer s.out.Flush()
	defer s.shutdownSubscriptions()

	reader := bufio.NewReader(in)
	// Generous line buffer — MCP messages can include large tool results.
	// 4 MB matches the daemon's per-request payload ceiling.
	bufScan := bufio.NewScanner(reader)
	bufScan.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for bufScan.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := bufScan.Bytes()
		if len(line) == 0 {
			continue
		}
		// Each request is handled inline. Tools call the daemon, which may
		// take seconds; that's fine — MCP clients send one request at a
		// time over stdio and wait for the response.
		s.handle(ctx, line)
	}
	if err := bufScan.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// rpcRequest is the JSON-RPC 2.0 envelope MCP layers on top of.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // null/missing for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes used by the MCP server. The MCP spec inherits the
// JSON-RPC 2.0 error model verbatim.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

func (s *Server) handle(ctx context.Context, line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(nil, codeParseError, err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		s.writeError(req.ID, codeInvalidRequest, "jsonrpc must be \"2.0\"")
		return
	}

	// Notifications carry no id and expect no response.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		s.handleInitialize(req.ID, req.Params)
	case "initialized", "notifications/initialized":
		// Client confirms readiness; no response required.
		return
	case "tools/list":
		s.handleToolsList(req.ID)
	case "tools/call":
		s.handleToolsCall(ctx, req.ID, req.Params)
	case "resources/list":
		s.handleResourcesList(req.ID)
	case "resources/templates/list":
		s.handleResourcesTemplatesList(req.ID)
	case "resources/read":
		s.handleResourcesRead(ctx, req.ID, req.Params)
	case "resources/subscribe":
		s.handleResourcesSubscribe(req.ID, req.Params)
	case "resources/unsubscribe":
		s.handleResourcesUnsubscribe(req.ID, req.Params)
	case "ping":
		s.writeResult(req.ID, json.RawMessage(`{}`))
	case "shutdown":
		s.writeResult(req.ID, json.RawMessage(`{}`))
	default:
		if !isNotification {
			s.writeError(req.ID, codeMethodNotFound, "method not found: "+req.Method)
		}
	}
}

// initializeResult is the MCP server-info payload. Capabilities advertise the
// tools surface; we don't currently expose resources or prompts.
type initializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    map[string]any    `json:"capabilities"`
	ServerInfo      initializeSrvInfo `json:"serverInfo"`
	Instructions    string            `json:"instructions,omitempty"`
}

type initializeSrvInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleInitialize(id, _ json.RawMessage) {
	caps := map[string]any{
		"tools":     map[string]any{"listChanged": false},
		"resources": map[string]any{"subscribe": s.dialer != nil, "listChanged": false},
	}
	res := initializeResult{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    caps,
		ServerInfo: initializeSrvInfo{
			Name:    "ibkr",
			Version: s.version,
		},
		Instructions: "Read-only Interactive Brokers tools and resources. Tools cover account, positions, snapshot quotes, option chains, history, scans, and position sizing. Resources expose live streaming quotes via subscribe (URI templates: ibkr://quote/{symbol} for stocks, ibkr://option/{symbol}/{expiry}/{right}/{strike} for options).",
	}
	b, _ := json.Marshal(res)
	s.writeResult(id, b)
}

// toolDescriptor is the wire shape MCP expects in tools/list.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (s *Server) handleToolsList(id json.RawMessage) {
	descs := make([]toolDescriptor, 0, len(Tools))
	for _, t := range Tools {
		descs = append(descs, toolDescriptor{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.JSONSchema,
		})
	}
	b, _ := json.Marshal(map[string]any{"tools": descs})
	s.writeResult(id, b)
}

// callParams is the input to tools/call. We accept missing arguments as an
// empty object; some clients omit the field for zero-arg tools.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolResultPayload mirrors the MCP tools/call response. Content is always a
// single text block carrying the daemon's JSON, stringified. IsError flags
// daemon/RPC errors so the LLM can distinguish them from on-the-wire success
// with empty payloads.
type toolResultPayload struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) handleToolsCall(ctx context.Context, id, params json.RawMessage) {
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	tool, ok := lookupTool(p.Name)
	if !ok {
		s.writeError(id, codeMethodNotFound, "unknown tool: "+p.Name)
		return
	}
	out, err := tool.Handler(ctx, s.conn, p.Arguments)
	if err != nil {
		// Tool-level errors land inside a non-error JSON-RPC response with
		// isError=true, per the MCP spec — clients distinguish protocol
		// errors (codeMethodNotFound, codeInvalidParams) from tool
		// failures (gateway down, symbol inactive). Surfacing inactive-
		// symbol or gateway-unavailable as JSON-RPC errors would mislead
		// the LLM into thinking the protocol broke.
		payload := toolResultPayload{
			IsError: true,
			Content: []contentBlock{{Type: "text", Text: err.Error()}},
		}
		b, _ := json.Marshal(payload)
		s.writeResult(id, b)
		return
	}
	payload := toolResultPayload{
		Content: []contentBlock{{Type: "text", Text: string(out)}},
	}
	b, _ := json.Marshal(payload)
	s.writeResult(id, b)
}

func lookupTool(name string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func (s *Server) writeResult(id, result json.RawMessage) {
	if len(id) == 0 {
		id = json.RawMessage(`null`)
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	s.write(resp)
}

func (s *Server) writeError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage(`null`)
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
	s.write(resp)
}

func (s *Server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		// json.Marshal of a fixed struct only fails on cycles — none here.
		// Fall back to a minimal in-band error so the client doesn't hang.
		b = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":%q}}`, codeInternalError, err.Error()))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(b)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}
