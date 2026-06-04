package appweb

import "embed"

// Files contains the static PWA assets served by `ibkr app`.
//
//go:embed index.html app.js styles.css service-worker.js manifest.webmanifest icon-192.png icon-512.png favicon-16.png favicon-32.png favicon-64.png
var Files embed.FS
