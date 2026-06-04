package appweb

import (
	"regexp"
	"strings"
	"testing"
)

func TestAppJSDoesNotUseBareNotificationGlobal(t *testing.T) {
	t.Parallel()
	data, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(data)
	bareNotification := regexp.MustCompile(`(^|[^.$A-Za-z0-9_])Notification([.()]|\b)`)
	for lineNo, line := range strings.Split(js, "\n") {
		if bareNotification.MatchString(line) && !strings.Contains(line, "globalThis.Notification") {
			t.Fatalf("app.js:%d uses unguarded Notification global: %s", lineNo+1, line)
		}
	}
}

func TestAppJSPushControlsUseCapabilityHelpers(t *testing.T) {
	t.Parallel()
	data, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(data)
	for _, want := range []string{
		"function notificationStateLabel()",
		"function hasNotifications()",
		"function canUseWebPush()",
		`$("pushState").textContent = notificationStateLabel();`,
		"if (!canUseWebPush())",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing push compatibility guard %q", want)
		}
	}
}

func TestAppJSConfirmInputsUsesTraderSafeCopy(t *testing.T) {
	t.Parallel()
	data, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(data)
	for _, want := range []string{
		`if (action === "confirm_inputs") return "Check data";`,
		"function canarySummaryText(canary, snap = {})",
		"before treating canary as a market signal",
		"no market-stress action",
		"function canaryNeedsInputCheck(canary)",
		"function canaryInputCheckBlocksAction(canary)",
		"function canaryInputIssueSummary(canary, snap = {})",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing confirm-inputs copy contract %q", want)
		}
	}
	if strings.Contains(js, `if (action === "confirm_inputs") return "Confirm";`) {
		t.Fatalf("app.js maps confirm_inputs to bare Confirm")
	}
}

func TestAppJSRegimeCardSeparatesDataGapsFromRegime(t *testing.T) {
	t.Parallel()
	data, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(data)
	for _, want := range []string{
		`marketRegimeLabel(market, indicators, canary)`,
		"function marketRegimeStatusLine(snap, canary, market, indicators)",
		`return "Normal + gaps";`,
		"Paper gateway live quotes OK",
		"HYG 50-DMA",
		"USD/JPY baseline",
		"gamma cache",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing regime data-gap contract %q", want)
		}
	}
}
