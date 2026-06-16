package memwebhook

import (
	"context"
	"time"
)

// LedgerMetrics is the ledger-derived half of the Memory panel payload. The
// HTTP route composes it with the MemPalace status snapshot + best-effort wing
// stats (owned by mempalace.Service).
type LedgerMetrics struct {
	Totals       IngestTotals  `json:"totals"`
	PerWing      []WingVolume  `json:"per_wing"`
	PerWebhook   []WebhookStat `json:"per_webhook"`
	Recent       []Ingestion   `json:"recent"`
	WebhookCount int           `json:"webhook_count"`
	EnabledCount int           `json:"enabled_count"`
}

// IngestTotals aggregates ingestion volume + outcomes across all webhooks.
type IngestTotals struct {
	Total    int64 `json:"total"`
	OK       int64 `json:"ok"`
	Failed   int64 `json:"failed"`
	Rejected int64 `json:"rejected"`
	TooLarge int64 `json:"too_large"`
	Last24h  int64 `json:"last_24h"`
	Last7d   int64 `json:"last_7d"`
}

// WingVolume is per-wing ingestion volume + health.
type WingVolume struct {
	Wing   string `json:"wing"`
	Count  int64  `json:"count"`
	OK     int64  `json:"ok"`
	Failed int64  `json:"failed"`
	LastAt int64  `json:"last_at"`
}

// WebhookStat is per-webhook ingestion stats for the panel table.
type WebhookStat struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Wing        string `json:"wing"`
	Enabled     bool   `json:"enabled"`
	IngestCount int64  `json:"ingest_count"`
	FailCount   int64  `json:"fail_count"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
}

// Ingestion is one ledger row surfaced as recent activity.
type Ingestion struct {
	ID          string `json:"id"`
	WebhookID   string `json:"webhook_id"`
	WebhookName string `json:"webhook_name"`
	Wing        string `json:"wing"`
	Source      string `json:"source,omitempty"`
	ReceivedAt  int64  `json:"received_at"`
	PayloadSize int64  `json:"payload_size"`
	Status      string `json:"status"`
	Detail      string `json:"detail,omitempty"`
}

// KnownWings returns the distinct set of wings already bound to webhooks or
// seen in the ledger, for the create-form suggestion list. Free text is still
// accepted by Create.
func (s *Service) KnownWings(ctx context.Context) ([]string, error) {
	if s.db == nil {
		return []string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT wing FROM memory_webhooks
		UNION
		SELECT wing FROM memory_webhook_ingestions
		ORDER BY wing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var wing string
		if err := rows.Scan(&wing); err != nil {
			return nil, err
		}
		if wing != "" {
			out = append(out, wing)
		}
	}
	return out, rows.Err()
}

// Metrics returns the ledger-derived panel metrics.
func (s *Service) Metrics(ctx context.Context, recentLimit int) (*LedgerMetrics, error) {
	if recentLimit <= 0 || recentLimit > 200 {
		recentLimit = 50
	}
	m := &LedgerMetrics{
		PerWing:    []WingVolume{},
		PerWebhook: []WebhookStat{},
		Recent:     []Ingestion{},
	}
	if s.db == nil {
		return m, nil
	}
	now := time.Now().UnixMilli()
	since24h := now - 24*time.Hour.Milliseconds()
	since7d := now - 7*24*time.Hour.Milliseconds()

	// Totals + outcome breakdown.
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status='ok'        THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status='failed'    THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status='rejected'  THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status='too_large' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN received_at>=? THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN received_at>=? THEN 1 ELSE 0 END),0)
		FROM memory_webhook_ingestions`, since24h, since7d).
		Scan(&m.Totals.Total, &m.Totals.OK, &m.Totals.Failed, &m.Totals.Rejected,
			&m.Totals.TooLarge, &m.Totals.Last24h, &m.Totals.Last7d); err != nil {
		return nil, err
	}

	// Per-wing volume.
	wingRows, err := s.db.QueryContext(ctx, `
		SELECT wing, COUNT(*),
			COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status<>'ok' THEN 1 ELSE 0 END),0),
			COALESCE(MAX(received_at),0)
		FROM memory_webhook_ingestions
		GROUP BY wing
		ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer wingRows.Close()
	for wingRows.Next() {
		var wv WingVolume
		if err := wingRows.Scan(&wv.Wing, &wv.Count, &wv.OK, &wv.Failed, &wv.LastAt); err != nil {
			return nil, err
		}
		m.PerWing = append(m.PerWing, wv)
	}
	if err := wingRows.Err(); err != nil {
		return nil, err
	}

	// Per-webhook stats (from the webhook table itself).
	whRows, err := s.db.QueryContext(ctx, `
		SELECT id, name, wing, enabled, ingest_count, fail_count, COALESCE(last_used_at,0)
		FROM memory_webhooks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer whRows.Close()
	for whRows.Next() {
		var ws WebhookStat
		var enabled int
		if err := whRows.Scan(&ws.ID, &ws.Name, &ws.Wing, &enabled, &ws.IngestCount, &ws.FailCount, &ws.LastUsedAt); err != nil {
			return nil, err
		}
		ws.Enabled = enabled != 0
		m.PerWebhook = append(m.PerWebhook, ws)
		m.WebhookCount++
		if ws.Enabled {
			m.EnabledCount++
		}
	}
	if err := whRows.Err(); err != nil {
		return nil, err
	}

	// Recent activity.
	recRows, err := s.db.QueryContext(ctx, `
		SELECT id, webhook_id, webhook_name, wing, COALESCE(source,''), received_at, payload_size, status, COALESCE(detail,'')
		FROM memory_webhook_ingestions
		ORDER BY received_at DESC
		LIMIT ?`, recentLimit)
	if err != nil {
		return nil, err
	}
	defer recRows.Close()
	for recRows.Next() {
		var ig Ingestion
		if err := recRows.Scan(&ig.ID, &ig.WebhookID, &ig.WebhookName, &ig.Wing, &ig.Source,
			&ig.ReceivedAt, &ig.PayloadSize, &ig.Status, &ig.Detail); err != nil {
			return nil, err
		}
		m.Recent = append(m.Recent, ig)
	}
	return m, recRows.Err()
}
