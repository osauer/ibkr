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
