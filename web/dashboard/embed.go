// Package dashboard exposes the static PWA assets via embed.FS so the gateway
// binary can serve them directly with no separate build step.
package dashboard

import "embed"

//go:embed index.html app.js sw.js manifest.webmanifest icon-192.svg icon-512.svg
var Assets embed.FS
