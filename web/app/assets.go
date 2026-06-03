package appweb

import "embed"

// Files contains the static PWA assets served by `ibkr app`.
//
//go:embed index.html app.js styles.css service-worker.js manifest.webmanifest icon.svg
var Files embed.FS
