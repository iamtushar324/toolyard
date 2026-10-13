// inbox-migration-check applies additive migrations and verifies legacy Inbox
// preservation against an offline SQLite backup. It never opens a live path.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	var path string
	flag.StringVar(&path, "database", "", "offline backup database")
	flag.Parse()
	abs, err := filepath.Abs(path)
	if err != nil || path == "" || strings.HasPrefix(abs, "/var/lib/") {
		fmt.Fprintln(os.Stderr, "An offline copy outside /var/lib is required")
		os.Exit(2)
	}
	if _, err = os.Stat(abs); err != nil {
		fmt.Fprintln(os.Stderr, "Backup does not exist")
		os.Exit(2)
	}
	db, err := store.Open(abs)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	svc, err := inbox.New(context.Background(), inbox.Options{DB: db})
	if err != nil {
		panic(err)
	}
	if err = svc.SyncLegacy(context.Background()); err != nil {
		panic(err)
	}
	counts := map[string]int{}
	queries := map[string]string{
		"legacy_total":                  `SELECT count(*) FROM approval_requests`,
		"legacy_pending":                `SELECT count(*) FROM approval_requests WHERE status='pending'`,
		"inbox_legacy_total":            `SELECT count(*) FROM inbox_requests WHERE json_extract(doc,'$.execution_mode')='legacy'`,
		"inbox_legacy_pending":          `SELECT count(*) FROM inbox_requests WHERE json_extract(doc,'$.execution_mode')='legacy' AND status='pending'`,
		"changed_ownership_or_deadline": `SELECT count(*) FROM approval_requests a LEFT JOIN inbox_requests i ON i.id=a.id WHERE i.id IS NULL OR i.agent_id!=COALESCE(a.agent_id,'') OR i.expires_at!=a.expires_at OR i.created_at!=a.created_at`,
		"historical_executable_grants":  `SELECT count(*) FROM inbox_grants g JOIN approval_requests a ON a.id=g.request_id`,
		"preserved_history_mismatch":    `SELECT count(*) FROM approval_requests a JOIN inbox_requests i ON i.id=a.id WHERE a.status!='pending' AND i.status!=CASE WHEN a.status='allowed' THEN 'approved' ELSE a.status END`,
	}
	for name, q := range queries {
		var n int
		if err = db.QueryRow(q).Scan(&n); err != nil {
			panic(err)
		}
		counts[name] = n
	}
	json.NewEncoder(os.Stdout).Encode(counts)
	if counts["legacy_total"] != counts["inbox_legacy_total"] || counts["legacy_pending"] != counts["inbox_legacy_pending"] || counts["changed_ownership_or_deadline"] != 0 || counts["historical_executable_grants"] != 0 || counts["preserved_history_mismatch"] != 0 {
		os.Exit(1)
	}
}
