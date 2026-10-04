// Package dashboard exposes the static PWA assets via embed.FS so the gateway
// binary can serve them directly with no separate build step.
package dashboard

import "embed"

//go:embed index.html app.js settings.js pricing.js style.css workspace.css sw.js manifest.webmanifest icon-192.svg icon-512.svg voice-worklet.js login.html login.js
var Assets embed.FS
