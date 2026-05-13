// Package lake holds the embedded assets for the personal-data-lake
// dashboard at /lake/. The bundle is a sibling of web/dashboard/ and is
// served by cmd/gateway via a sub-mux at /lake/.
package lake

import "embed"

// Assets embeds every static file under web/lake/ — the SPA shell, its
// style + script, the manifest that drives tab/panel layout, and the
// SQL files for named queries. The api package reads `manifest.json` and
// `queries/<tab>/<id>.sql` from this FS at request time, so adding a new
// chart means dropping a SQL file + editing manifest.json — no code change.
//
//go:embed index.html app.js style.css manifest.json queries vendor
var Assets embed.FS
