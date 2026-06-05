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
		`marketRegimeLabel(posture)`,
		"function regimePosture(snap = {}, canary = {}, market = {})",
		"function regimeWeatherClass(tone)",
		"function normalizeRegimePosture(candidate)",
		`snap.regime?.posture`,
		`market.regime_posture`,
		"function marketRegimeStatusLine(snap, canary, market, indicators)",
		"Paper gateway live quotes OK",
		"HYG 50-DMA",
		"USD/JPY baseline",
		"gamma cache",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing regime data-gap contract %q", want)
		}
	}
	for _, forbidden := range []string{
		`if (redClusters > 0) return "red";`,
		`return "Risk-off";`,
	} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("app.js still has UI-owned regime policy %q", forbidden)
		}
	}
}

func TestAppMobileDashboardContracts(t *testing.T) {
	t.Parallel()
	appData, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	cssData, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatalf("read styles.css: %v", err)
	}
	js := string(appData)
	html := string(htmlData)
	css := string(cssData)

	for _, want := range []string{
		`const symbols = ["SPY", "VIX", "QQQ", "IWM", "HYG", "TLT"];`,
		"function handleExpandablePanelTap(event, which)",
		`$("regimePanel").addEventListener("click"`,
		`$("canaryHero").addEventListener("click"`,
		`"trading", "settings", "regime", "canary"`,
		"function setupLiveRefreshLoop()",
		"function setupBottomTabs()",
		"function renderTabs()",
		"function renderSettings()",
		"function setPurgeRestoreEnabled(enabled)",
		"function purgeRestoreSettingEnabled()",
		"function refreshBootstrapIfSSEUnavailable()",
		"function renderAccountDailyPnlPct(account = {})",
		"function accountDailyPnlPct(account = {})",
		"function setUnderlyingExpansion(open)",
		"function renderUnderlyingExpansion()",
		"function handleUnderlyingPanelTap(event)",
		"function underlyingHeldDailyPnlTotals(rows, baseCurrency)",
		"function compareUnderlyingRows(a, b)",
		"function heldUnderlyingChange(group, quote, price)",
		"function heldUnderlyingDailyPnl(group, baseCurrency, currency)",
		"function quoteChange(quote)",
		"function signedDisplayMoney(value, currency)",
		"const pnl = heldUnderlyingDailyPnl(group, baseCurrency, currency);",
		`source: "daily P/L"`,
		`group.group_daily_pnl_base`,
		"function marketQuoteChangeClass(symbol, change)",
		"function handlePortfolioPanelTap(event)",
		"function setPortfolioExpansion(open)",
		"function portfolioDeltaPosture(portfolio = {}, account = {})",
		"function regimePostureDetailTone(posture = {})",
		`$("underlyingDetailToggle").addEventListener("click"`,
		`$("underlyingPanel").addEventListener("click", handleUnderlyingPanelTap);`,
		`$("portfolioPanel").addEventListener("click", handlePortfolioPanelTap);`,
		"change: heldUnderlyingChange(group, quote, price.value)",
		"function gatewayIssueText(snap = {})",
		"snap.status?.last_error",
		"client id .*already in use",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing mobile dashboard contract %q", want)
		}
	}
	for _, want := range []string{
		`id="bannerStack"`,
		`id="bottomTabs"`,
		`data-tab="monitor"`,
		`data-tab="positions" aria-disabled="true" disabled`,
		`data-tab="alerts"`,
		`data-tab="settings"`,
		`id="accountPanel"`,
		`id="dailyPnlPct"`,
		`id="underlyingPanel" data-open="false"`,
		`id="underlyingDetailToggle"`,
		`Winner daily P/L`,
		`id="underlyingWinnerPnl"`,
		`Loser daily P/L`,
		`id="underlyingLoserPnl"`,
		`Purge all!`,
		`Restore all`,
		`Rebuild all`,
		`id="underlyingBookListPanel" hidden`,
		`id="portfolioPanel" data-open="false"`,
		`Delta posture`,
		`id="portfolioDeltaMeaning"`,
		`id="alertsTab" data-tab-panel="alerts"`,
		`id="settingsTab" data-tab-panel="settings"`,
		`id="purgeRestoreToggle"`,
		`id="settingsTradingLimits"`,
		`id="settingsMarketDataStatus"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing mobile dashboard contract %q", want)
		}
	}
	if strings.Index(html, `id="bannerStack"`) > strings.Index(html, `id="accountPanel"`) {
		t.Fatalf("snapshot banner should render above account panel")
	}
	if strings.Contains(html, `<details class="panel underlying-panel"`) {
		t.Fatalf("underlyings panel should not hide summary/actions inside native details")
	}
	for _, want := range []string{
		".source-banner",
		"background: var(--red);",
		"color: #fff;",
		`.underlying-panel[data-open="true"] .panel-chevron::after`,
		".underlying-pnl-summary",
		".underlying-pnl-card--winner",
		".underlying-pnl-card--loser",
		".underlying-row__metric--change",
		"touch-action: manipulation;",
		".underlying-book__list-panel",
		".account-pnl-pct",
		".portfolio-delta-posture",
		".portfolio-panel .panel-chevron",
		".portfolio-detail-panel",
		".bottom-tabs",
		".bottom-tab.active",
		".settings-panel",
		".toggle-switch input:checked + span",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("styles.css missing mobile dashboard contract %q", want)
		}
	}
}

func TestUnderlyingWinnerLoserTotalsUseDailyPnl(t *testing.T) {
	t.Parallel()
	data, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(data)
	for _, name := range []string{
		"underlyingHeldDailyPnlTotals",
		"heldUnderlyingDailyPnl",
	} {
		body := jsFunctionBlock(t, js, name)
		lower := strings.ToLower(body)
		if !strings.Contains(lower, "daily") {
			t.Fatalf("%s must aggregate daily P/L", name)
		}
		for _, forbidden := range []string{"group_unrealized_pnl", "unrealized_pnl_base"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s must aggregate daily P/L, not open/unrealized field %q", name, forbidden)
			}
		}
	}
	if strings.Contains(js, "heldUnderlyingQuoteMarkedPnl") || strings.Contains(js, "heldUnderlyingQuotePnlAdjustment") {
		t.Fatalf("underlying winner/loser totals must use broker daily P/L, not client quote-marked estimates")
	}
}

func jsFunctionBlock(t *testing.T, js, name string) string {
	t.Helper()
	start := strings.Index(js, "function "+name+"(")
	if start < 0 {
		t.Fatalf("app.js missing function %s", name)
	}
	next := strings.Index(js[start+1:], "\nfunction ")
	if next < 0 {
		return js[start:]
	}
	return js[start : start+1+next]
}
