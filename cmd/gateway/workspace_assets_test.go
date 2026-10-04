package main

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The new UI modules must ship in the binary and resolve to assets rather
// than the SPA's HTML fallback. Their URLs must change with their content.
func TestWorkspaceAssets(t *testing.T) {
	handler := staticHandler()
	for _, page := range []string{"/", "/login"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", page, nil))
		pattern := regexp.MustCompile(`(?:src|href)="(/(?:settings\.js|pricing\.js|workspace\.css)\?v=[^"]+)"`)
		links := pattern.FindAllStringSubmatch(rec.Body.String(), -1)
		want := 3
		if page == "/login" {
			want = 1
		}
		if len(links) != want {
			t.Fatalf("%s: found %d fingerprinted modules, want %d", page, len(links), want)
		}
		for _, link := range links {
			asset := httptest.NewRecorder()
			handler.ServeHTTP(asset, httptest.NewRequest("GET", link[1], nil))
			if asset.Code != 200 || strings.Contains(asset.Header().Get("Content-Type"), "text/html") || asset.Body.Len() == 0 {
				t.Fatalf("%s: module was not served as an asset", link[1])
			}
		}
	}
}
