package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

// handleOrderPlace and handleOrderCancel must refuse every call with
// ErrTradingDisabled until the gated write path exists. Preview can mint local
// tokens, but no broker place/cancel path is opened by this slice.
func TestOrderHandlersAlwaysRefuse(t *testing.T) {
	t.Parallel()

	req := &rpc.Request{ID: "td-1", Method: rpc.MethodOrderPlace}

	if _, err := handleOrderPlace(context.Background(), req); !errors.Is(err, ErrTradingDisabled) {
		t.Errorf("handleOrderPlace returned %v, want ErrTradingDisabled", err)
	}
	if _, err := handleOrderCancel(context.Background(), req); !errors.Is(err, ErrTradingDisabled) {
		t.Errorf("handleOrderCancel returned %v, want ErrTradingDisabled", err)
	}
}

// The dispatcher must classify order-verb refusals as CodeTradingDisabled
// on the wire — README's safety contract claims this code reaches the
// client, and the CLI/MCP renderers branch on it.
func TestDispatchOrderVerbsClassifyAsTradingDisabled(t *testing.T) {
	t.Parallel()

	for _, method := range []string{rpc.MethodOrderPlace, rpc.MethodOrderCancel} {
		t.Run(method, func(t *testing.T) {
			srv := newTestServer(t)

			req := &rpc.Request{ID: "td-" + method, Method: method}

			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			r := bufio.NewReader(strings.NewReader(""))
			srv.dispatch(context.Background(), req, enc, r)

			var resp struct {
				ID    string     `json:"id"`
				Ok    bool       `json:"ok"`
				Error *rpc.Error `json:"error"`
			}
			if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v (body=%q)", err, buf.String())
			}
			if resp.Ok {
				t.Fatalf("%s succeeded; want refusal", method)
			}
			if resp.Error == nil {
				t.Fatalf("%s returned ok=false with no error payload (body=%q)", method, buf.String())
			}
			if resp.Error.Code != rpc.CodeTradingDisabled {
				t.Errorf("%s error code = %q, want %q", method, resp.Error.Code, rpc.CodeTradingDisabled)
			}
		})
	}
}
