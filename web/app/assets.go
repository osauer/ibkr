package appweb

import (
	"embed"
	"path/filepath"
)

// Files contains the static PWA assets served by `ibkr app`.
//
//go:embed index.html app.js alert-inbox-v2.js alerts.js auth.js brief.js canary.js chrome.js lifecycle.js market-events.js opportunities.js orders.js portfolio.js protection-coverage.js protection.js settings.js shared.js shell.js state.js underlyings.js styles.css service-worker.js manifest.webmanifest icon-192.png icon-512.png favicon-16.png favicon-32.png favicon-64.png
var Files embed.FS

// EmbeddedJavaScriptFileNames returns the root-level JavaScript assets in the
// embedded app. The embed list remains the source of truth for both the local
// app routes and the remote relay allowlist.
func EmbeddedJavaScriptFileNames() []string {
	entries, err := Files.ReadDir(".")
	if err != nil {
		panic(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".js" {
			names = append(names, entry.Name())
		}
	}
	return names
}
