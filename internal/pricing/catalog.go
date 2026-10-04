// Package pricing maintains a public, provider-specific model price catalog.
// Prices are reference data, not proof of billable usage or an invoice.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SourceURL       = "https://models.dev/api.json?type=all"
	refreshInterval = 6 * time.Hour
	staleAfter      = 24 * time.Hour
	maxBytes        = 12 << 20
)

type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Pointers distinguish an explicitly published zero from a missing price.
// All rates retain Models.dev's USD per million tokens units.
type Rates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type Model struct {
	ProviderID    string `json:"provider_id"`
	ProviderName  string `json:"provider_name"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	Rates         Rates  `json:"rates"`
	VariableRates bool   `json:"variable_rates"`
}

type snapshot struct {
	CheckedAt time.Time `json:"checked_at"`
	ETag      string    `json:"etag"`
	Models    []Model   `json:"models"`
}

type View struct {
	Source       string     `json:"source"`
	SourceURL    string     `json:"source_url"`
	Currency     string     `json:"currency"`
	Unit         string     `json:"unit"`
	Status       string     `json:"status"` // ready | stale | unavailable
	CheckedAt    *time.Time `json:"checked_at"`
	RefreshHours int        `json:"refresh_hours"`
	ModelCount   int        `json:"model_count"`
	Total        int        `json:"total"`
	Providers    []Provider `json:"providers"`
	Models       []Model    `json:"models"`
	LastError    string     `json:"last_error,omitempty"`
}

type Catalog struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	data      snapshot
	lastError string
	cachePath string
	client    *http.Client
	url       string
	now       func() time.Time
}

func New(cachePath string) *Catalog {
	c := &Catalog{cachePath: cachePath, url: SourceURL, now: time.Now,
		client: &http.Client{Timeout: 20 * time.Second}}
	if cachePath != "" {
		if f, err := os.Open(cachePath); err == nil {
			defer f.Close()
			var saved snapshot
			if json.NewDecoder(io.LimitReader(f, maxBytes)).Decode(&saved) == nil && validSnapshot(saved, c.now()) {
				c.data = saved
			}
		}
	}
	return c
}

// Run fetches off the request path. Outages retain the last valid snapshot,
// retry after 15 minutes, and never delay startup or a tool call.
func (c *Catalog) Run(ctx context.Context) {
	for {
		delay := refreshInterval
		if err := c.Refresh(ctx); err != nil {
			delay = 15 * time.Minute
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *Catalog) Refresh(ctx context.Context) (err error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.lastError = "The price source could not refresh. The last valid catalog is retained."
		} else {
			c.lastError = ""
		}
	}()
	c.mu.RLock()
	previous := c.data
	c.mu.RUnlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Toolyard/1.0 model-price-catalog")
	if previous.ETag != "" {
		req.Header.Set("If-None-Match", previous.ETag)
	}
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	next := previous
	switch res.StatusCode {
	case http.StatusNotModified:
		if len(previous.Models) == 0 {
			return errors.New("empty catalog revalidation")
		}
	case http.StatusOK:
		body, readErr := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
		if readErr != nil {
			return readErr
		}
		if len(body) > maxBytes {
			return errors.New("price catalog exceeds size limit")
		}
		models, parseErr := parse(body)
		if parseErr != nil {
			return parseErr
		}
		next.Models = models
		next.ETag = res.Header.Get("ETag")
	default:
		return fmt.Errorf("price source returned HTTP %d", res.StatusCode)
	}
	next.CheckedAt = c.now().UTC()
	// Persist before publishing so restart never revives a partial snapshot.
	if c.cachePath != "" {
		body, marshalErr := json.Marshal(next)
		if marshalErr != nil {
			return marshalErr
		}
		tmp := c.cachePath + ".tmp"
		defer os.Remove(tmp)
		if err := os.WriteFile(tmp, body, 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, c.cachePath); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.data = next
	c.mu.Unlock()
	return nil
}

func (c *Catalog) View(provider, query string, offset, limit int) View {
	v := View{Source: "Models.dev", SourceURL: SourceURL, Currency: "USD", Unit: "per_million_tokens",
		Status: "unavailable", RefreshHours: 6, Providers: []Provider{}, Models: []Model{}}
	if c == nil {
		return v
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v.LastError = c.lastError
	v.ModelCount = len(c.data.Models)
	if v.ModelCount == 0 {
		return v
	}
	checked := c.data.CheckedAt
	v.CheckedAt = &checked
	v.Status = "ready"
	if c.now().Sub(checked) > staleAfter || c.lastError != "" {
		v.Status = "stale"
	}
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	if limit > 100 {
		limit = 100
	}
	query = strings.ToLower(strings.TrimSpace(query))
	providers := map[string]string{}
	for _, m := range c.data.Models {
		providers[m.ProviderID] = m.ProviderName
		if provider != "" && m.ProviderID != provider {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(m.ID+" "+m.Name+" "+m.ProviderName), query) {
			continue
		}
		if v.Total >= offset && len(v.Models) < limit {
			v.Models = append(v.Models, m)
		}
		v.Total++
	}
	for id, name := range providers {
		v.Providers = append(v.Providers, Provider{ID: id, Name: name})
	}
	sort.Slice(v.Providers, func(i, j int) bool { return v.Providers[i].Name < v.Providers[j].Name })
	return v
}

func validRates(r Rates) bool {
	for _, n := range []*float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if n != nil && (*n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0)) {
			return false
		}
	}
	return true
}

func validSnapshot(s snapshot, now time.Time) bool {
	if len(s.Models) == 0 || s.CheckedAt.IsZero() || s.CheckedAt.After(now.Add(time.Minute)) {
		return false
	}
	for _, m := range s.Models {
		if m.ID == "" || m.ProviderID == "" || !validRates(m.Rates) {
			return false
		}
	}
	return true
}

func parse(body []byte) ([]Model, error) {
	var providers map[string]struct {
		Name   string `json:"name"`
		Models map[string]struct {
			ID         string          `json:"id"`
			Name       string          `json:"name"`
			Cost       json.RawMessage `json:"cost"`
			Modalities struct {
				Output []string `json:"output"`
			} `json:"modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, err
	}
	var models []Model
	for providerID, p := range providers {
		if providerID == "" || p.Name == "" {
			continue
		}
		for id, raw := range p.Models {
			text := false
			for _, modality := range raw.Modalities.Output {
				if modality == "text" {
					text = true
				}
			}
			if id == "" || !text {
				continue
			}
			m := Model{ProviderID: providerID, ProviderName: p.Name, ID: id, Name: raw.Name}
			if m.Name == "" {
				m.Name = id
			}
			if len(raw.Cost) > 0 && string(raw.Cost) != "null" {
				if err := json.Unmarshal(raw.Cost, &m.Rates); err != nil {
					return nil, err
				}
				if !validRates(m.Rates) {
					return nil, errors.New("invalid model price")
				}
				var extra map[string]json.RawMessage
				if err := json.Unmarshal(raw.Cost, &extra); err != nil {
					return nil, err
				}
				for key := range extra {
					if key != "input" && key != "output" && key != "cache_read" && key != "cache_write" {
						m.VariableRates = true
					}
				}
			}
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("price source has no text models")
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].ProviderName != models[j].ProviderName {
			return models[i].ProviderName < models[j].ProviderName
		}
		return models[i].ID < models[j].ID
	})
	return models, nil
}
