package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	pollTick      = 30 * time.Second
	pollHTTPTimeo = 20 * time.Second
	pollMaxBody   = 2 << 20 // 2 MiB
	maxBackoffPow = 5
)

// RunPollers drives all enabled poller sources. Shaped as a
// goroutines.Supervise fn (blocking; returns on ctx cancel). Every 30s it
// re-reads the source list (so config edits apply live) and polls those that
// are due.
func (s *Service) RunPollers(ctx context.Context) error {
	client := &http.Client{Timeout: pollHTTPTimeo}
	t := time.NewTicker(pollTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.pollDue(ctx, client)
		}
	}
}

func (s *Service) pollDue(ctx context.Context, client *http.Client) {
	sources, err := s.ListSources(ctx)
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	for i := range sources {
		src := sources[i]
		if src.Kind != KindPoller || !src.Enabled || src.PollerCfg == nil {
			continue
		}
		if !pollerDue(&src, now) {
			continue
		}
		s.pollOne(ctx, client, &src)
	}
}

// pollerDue computes whether a poller is due: last_polled + interval *
// 2^min(failures,5), with ±10% jitter folded into the comparison.
func pollerDue(src *Source, nowMS int64) bool {
	state := src.PollerState
	if state == nil || state.LastPolledAt == 0 {
		return true // never polled
	}
	interval := src.PollerCfg.IntervalSec
	if interval < MinPollInterval {
		interval = MinPollInterval
	}
	pow := state.ConsecutiveFailures
	if pow > maxBackoffPow {
		pow = maxBackoffPow
	}
	wait := time.Duration(interval) * time.Second * time.Duration(int64(1)<<uint(pow))
	// ±10% jitter.
	jitter := time.Duration(rand.Int63n(int64(wait/5)+1)) - wait/10
	next := time.UnixMilli(state.LastPolledAt).Add(wait + jitter)
	return time.UnixMilli(nowMS).After(next)
}

func (s *Service) pollOne(ctx context.Context, client *http.Client, src *Source) {
	cfg := src.PollerCfg
	state := src.PollerState
	if state == nil {
		state = &PollerState{}
	}
	now := time.Now().UnixMilli()
	state.LastPolledAt = now

	body, err := fetch(ctx, client, cfg)
	if err != nil {
		state.ConsecutiveFailures++
		s.savePollerState(ctx, src.ID, state, err.Error())
		return
	}

	switch cfg.Mode {
	case ModeHash:
		sum := sha256.Sum256(body)
		h := hex.EncodeToString(sum[:])
		if state.LastHash == "" {
			// First poll seeds silently.
			state.LastHash = h
			state.ConsecutiveFailures = 0
			s.savePollerState(ctx, src.ID, state, "")
			return
		}
		if h != state.LastHash {
			state.LastHash = h
			s.emitPollerEvent(ctx, src, "content_changed",
				fmt.Sprintf("The page at %s changed", cfg.URL), map[string]any{"url": cfg.URL})
		}
	case ModeJSONField:
		val, perr := extractJSONPath(body, cfg.JSONPath)
		if perr != nil {
			state.ConsecutiveFailures++
			s.savePollerState(ctx, src.ID, state, perr.Error())
			return
		}
		if !state.seeded() {
			// First observation seeds silently.
			state.LastValue = val
			state.markSeeded()
			state.ConsecutiveFailures = 0
			s.savePollerState(ctx, src.ID, state, "")
			return
		}
		if val != state.LastValue {
			old := state.LastValue
			state.LastValue = val
			s.emitPollerEvent(ctx, src, "value_changed",
				fmt.Sprintf("%s at %s changed from %q to %q", cfg.JSONPath, cfg.URL, old, val),
				map[string]any{"url": cfg.URL, "json_path": cfg.JSONPath, "old": old, "new": val})
		}
	}
	state.ConsecutiveFailures = 0
	s.savePollerState(ctx, src.ID, state, "")
}

func fetch(ctx context.Context, client *http.Client, cfg *PollerConfig) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("poll %s: HTTP %d", cfg.URL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, pollMaxBody))
}

// emitPollerEvent ingests a poller-originated event. Poller events carry NO
// dedup_key — an A→B→A flap must emit twice.
func (s *Service) emitPollerEvent(ctx context.Context, src *Source, typ, summary string, payload map[string]any) {
	pb, _ := json.Marshal(payload)
	_, _ = s.ingestInternal(ctx, src, IngestInput{
		Type:    typ,
		Summary: summary,
		Payload: pb,
	})
}

func (s *Service) savePollerState(ctx context.Context, id string, state *PollerState, lastErr string) {
	b, _ := json.Marshal(state)
	_, _ = s.db.ExecContext(ctx,
		`UPDATE event_sources SET poller_state=?, last_error=?, updated_at=? WHERE id=?`,
		string(b), nullStr(lastErr), time.Now().UnixMilli(), id)
}

// extractJSONPath walks a minimal dot/bracket path: object fields by name and
// arrays by integer index. Examples: "a.b", "a.0.c", "a[0].c". The matched
// leaf is returned as a compact string (numbers/bools/strings render plainly;
// objects/arrays as their JSON).
func extractJSONPath(body []byte, path string) (string, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", fmt.Errorf("json_field: body is not JSON: %w", err)
	}
	cur := root
	for _, seg := range splitPath(path) {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return "", fmt.Errorf("json_field: key %q not found", seg)
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return "", fmt.Errorf("json_field: bad index %q", seg)
			}
			cur = node[idx]
		default:
			return "", fmt.Errorf("json_field: cannot descend into %q", seg)
		}
	}
	return stringifyLeaf(cur), nil
}

func splitPath(path string) []string {
	// Normalize "a[0].b" -> "a.0.b".
	path = strings.ReplaceAll(path, "[", ".")
	path = strings.ReplaceAll(path, "]", "")
	parts := strings.Split(path, ".")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func stringifyLeaf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// seeded/markSeeded track whether a json_field poller has captured its
// baseline. We piggyback on LastHash as a "seeded" flag for json_field
// pollers (it is otherwise unused in that mode) so an empty real value
// (legitimately "") doesn't look unseeded forever.
func (st *PollerState) markSeeded()  { st.LastHash = "seeded" }
func (st *PollerState) seeded() bool { return st.LastHash == "seeded" }
