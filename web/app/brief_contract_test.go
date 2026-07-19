package appweb

import (
	"slices"
	"strings"
	"testing"
)

func TestBriefCardStaticContract(t *testing.T) {
	t.Parallel()

	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlData)
	briefAt := strings.Index(html, `id="briefPanel"`)
	signalAt := strings.Index(html, `id="signalPanel"`)
	if briefAt < 0 || signalAt < 0 || briefAt > signalAt {
		t.Fatalf("brief card must be the first monitor card: brief=%d signal=%d", briefAt, signalAt)
	}
	for _, id := range []string{"briefPanel", "briefAsOf", "briefSourceBanner", "briefSections", "briefAckStatus"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html missing brief id %q", id)
		}
	}

	briefData, err := Files.ReadFile("brief.js")
	if err != nil {
		t.Fatal("brief.js is not embedded:", err)
	}
	brief := string(briefData)
	for _, want := range []string{
		`renderMarketSection(brief.market || {})`,
		`renderCalendarSection(brief.calendar || {}, snap.sources || {})`,
		`renderPortfolioSection(brief.portfolio || {})`,
		`renderRiskSection(brief.risk_limits || {})`,
		`renderProcessSection(brief.process || {}, brief)`,
		`dateTimeValue(brief.as_of)`,
		`percentValue(section.gamma, "gap_pct", "Gap", true)`,
		`briefRow("Artefacts", section.artefacts, null)`,
		`return "not declared"`,
		`completed ${completedAt}`,
		`"not yet completed this week"`,
		`"not yet completed today"`,
		`heldNameEventsUnavailable(sources)`,
		`Held-name events require an available positions snapshot.`,
		`row.rulebook_fingerprint_changed === true`,
		`row.signable === true`,
		`Sign off this reconcile report — statement clean`,
		"button.title = `Report ${reportID}`",
		`rulesDeltaUnclean(rulesDelta)`,
		`review the Rules delta row before signing`,
		"`peak set ${dateTimeValue(capital.peak_as_of)}`",
		`percentValue(section.latch, "consumed_pct_at_latch", "Engaged at")`,
		`fetch("/api/recon/signoff"`,
		`["ok", "attention", "degraded", "unavailable"]`,
		`canaryHeadline(section.canary)`,
		`moversValue(section.movers, currency)`,
		`other_daily_pnl_base`,
		`fieldValue(capital, "tier", "Tier")`,
		`fieldValue(capital, "enforcement", "Enforcement")`,
		`body: JSON.stringify({ report_id: reportID })`,
		`fetch("/api/brief/seen"`,
		`body: JSON.stringify(briefAckBody(brief, fingerprint))`,
		`body.month = brief.process?.monthly_pulse?.month || ""`,
		`body.evidence = "render"`,
		`monthlyPulseStatus(monthly)`,
		`return "not due"`,
		`return "completed this month"`,
		`return "blocked by policy evidence"`,
		`foreground render recorded`,
		`state.authenticated === true`,
		`state.activeTab === "monitor"`,
		`document.visibilityState === "visible"`,
		`const attemptedStampFingerprints = new Set()`,
		`const pendingStampFingerprints = new Set()`,
		`let briefStampArmed = true`,
		`let briefStampScheduled = false`,
		`let briefStampInFlight = false`,
		`briefStampLook += 1`,
		`const look = briefStampLook`,
		`look !== briefStampLook`,
		`globalThis.requestAnimationFrame`,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief.js missing contract %q", want)
		}
	}
	for _, forbidden := range []string{
		"window.confirm",
		"confirm_account",
		"confirm_mode",
		"toLocaleString(",
		"shortTimeWithZone",
		`declared ${artefact.declared}`,
		`completed ${artefact.completed}`,
		`fingerprint changed ${row.rulebook_fingerprint_changed}`,
	} {
		if strings.Contains(brief, forbidden) {
			t.Errorf("brief.js contains forbidden contract %q", forbidden)
		}
	}
	percent := jsFunctionBlock(t, brief, "percentValue")
	for _, want := range []string{`.toFixed(1)`, `signed && object[key] > 0 ? "+" : ""`} {
		if !strings.Contains(percent, want) {
			t.Errorf("percentValue missing one-decimal/signed contract %q", want)
		}
	}
	date := jsFunctionBlock(t, brief, "dateValue")
	for _, want := range []string{"getFullYear()", "getMonth() + 1", "getDate()"} {
		if !strings.Contains(date, want) {
			t.Errorf("dateValue missing local ISO date component %q", want)
		}
	}
	dateTime := jsFunctionBlock(t, brief, "dateTimeValue")
	for _, want := range []string{"dateValue(at)", "getHours()", "getMinutes()"} {
		if !strings.Contains(dateTime, want) {
			t.Errorf("dateTimeValue missing local short-time component %q", want)
		}
	}
	row := jsFunctionBlock(t, brief, "briefRow")
	if !strings.Contains(row, "if (value !== null) el.append(provided);") {
		t.Fatalf("briefRow must omit the value element for a null group-header value:\n%s", row)
	}
	visibility := jsFunctionBlock(t, brief, "setupBriefVisibility")
	for _, want := range []string{
		`document.visibilityState !== "visible"`,
		`briefStampLook += 1`,
		`briefStampArmed = false`,
		`briefStampArmed = true`,
		`renderBriefCard(state.snapshot || {})`,
	} {
		if !strings.Contains(visibility, want) {
			t.Errorf("setupBriefVisibility missing foreground-look contract %q", want)
		}
	}
	schedule := jsFunctionBlock(t, brief, "scheduleBriefStamp")
	for _, want := range []string{
		`!briefStampArmed`,
		`briefStampScheduled`,
		`briefStampInFlight`,
		`attemptedStampFingerprints.has(fingerprint)`,
		`pendingStampFingerprints.has(fingerprint)`,
		`look !== briefStampLook`,
		`state.snapshot?.brief?.brief_fingerprint !== fingerprint`,
		`briefStampVisible()`,
		`globalThis.requestAnimationFrame`,
	} {
		if !strings.Contains(schedule, want) {
			t.Errorf("scheduleBriefStamp missing one-stamp-per-look gate %q", want)
		}
	}
	acknowledge := jsFunctionBlock(t, brief, "acknowledgeBrief")
	nonOK := strings.Index(acknowledge, `if (!res.ok) throw`)
	disarm := strings.Index(acknowledge, `if (look === briefStampLook) briefStampArmed = false;`)
	if nonOK < 0 || disarm < nonOK {
		t.Fatalf("acknowledgeBrief must disarm the current look only after a successful response:\n%s", acknowledge)
	}
	if strings.Index(acknowledge, `briefStampInFlight = false;`) < disarm {
		t.Fatalf("acknowledgeBrief must keep the global in-flight guard through successful disarm:\n%s", acknowledge)
	}

	appData, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(appData)
	if !strings.Contains(app, `from "./brief.js"`) || strings.Count(app, "renderBriefCard(snap);") < 2 || !strings.Contains(app, "setupBriefVisibility();") {
		t.Fatal("app.js does not wire the brief renderer into renderAll, the one-second loop, and visibility setup")
	}
	lifecycleData, err := Files.ReadFile("lifecycle.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lifecycleData), `"rules", "brief"`) {
		t.Fatal("lifecycle.js does not subscribe to incremental brief events")
	}
	if !slices.Contains(EmbeddedJavaScriptFileNames(), "brief.js") {
		t.Fatal("EmbeddedJavaScriptFileNames omits brief.js")
	}
}
