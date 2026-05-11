package discover

import (
	"context"
	"testing"
)

// stubProcessLister returns a fixed list of process-list lines. Lets tests
// drive DetectIBKRApp without depending on the host's actual process table
// (CI may not have any of the IBKR apps installed).
func stubProcessLister(lines ...string) func(context.Context) []string {
	return func(_ context.Context) []string { return lines }
}

func withProcessLister(t *testing.T, fn func(context.Context) []string, body func()) {
	t.Helper()
	saved := ProcessLister
	ProcessLister = fn
	defer func() { ProcessLister = saved }()
	body()
}

func TestDetectIBKRApp_TWS(t *testing.T) {
	line := "48176 /Users/local/Applications/Trader Workstation/Trader Workstation.app/Contents/MacOS/JavaApplicationStub"
	withProcessLister(t, stubProcessLister(line), func() {
		app := DetectIBKRApp(context.Background())
		if app.Name != "TWS" {
			t.Errorf("Name = %q, want TWS", app.Name)
		}
		if app.PID != 48176 {
			t.Errorf("PID = %d, want 48176", app.PID)
		}
	})
}

func TestDetectIBKRApp_IBGateway(t *testing.T) {
	withProcessLister(t, stubProcessLister("  9012 /opt/IBJts/ibgateway/1024/ibgateway"), func() {
		app := DetectIBKRApp(context.Background())
		if app.Name != "IB Gateway" {
			t.Errorf("Name = %q, want IB Gateway", app.Name)
		}
		if app.PID != 9012 {
			t.Errorf("PID = %d, want 9012", app.PID)
		}
	})
}

func TestDetectIBKRApp_IBKRDesktop(t *testing.T) {
	withProcessLister(t, stubProcessLister("3456 /Applications/IBKR Desktop.app/Contents/MacOS/IBKR Desktop"), func() {
		app := DetectIBKRApp(context.Background())
		if app.Name != "IBKR Desktop" {
			t.Errorf("Name = %q, want IBKR Desktop", app.Name)
		}
		if app.PID != 3456 {
			t.Errorf("PID = %d, want 3456", app.PID)
		}
	})
}

func TestDetectIBKRApp_NoMatch(t *testing.T) {
	// A few unrelated processes. Zero IBKRApp expected; no panics on
	// lines without leading integers (Windows CSV shape).
	withProcessLister(t, stubProcessLister(
		`"chrome.exe","1234","Console","1","456,789 K"`,
		"  789 /usr/bin/zsh",
	), func() {
		app := DetectIBKRApp(context.Background())
		if app.Name != "" {
			t.Errorf("Name = %q, want empty (no match)", app.Name)
		}
		if app.PID != 0 {
			t.Errorf("PID = %d, want 0", app.PID)
		}
	})
}

func TestDetectIBKRApp_LookupUnavailable(t *testing.T) {
	withProcessLister(t, stubProcessLister( /* nil */ ), func() {
		app := DetectIBKRApp(context.Background())
		if app.Name != "" || app.PID != 0 {
			t.Errorf("zero IBKRApp expected on empty process list, got %+v", app)
		}
	})
}
