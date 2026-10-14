package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// healthcheckTimeout bounds one `toolyard healthcheck` request.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck is `toolyard healthcheck -url <u> -expect ready|live`: it
// fetches a probe and succeeds only when the JSON body's "status" is the
// expected one ("ready" for /v1/readyz, "live" for /v1/livez). Any other
// answer, a timeout or a bad flag is an error, so the process exits 1.
// It needs nothing outside the binary (container images have no curl or
// wget).
func runHealthcheck(argv []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	target := fs.String("url", "http://127.0.0.1:8787/v1/readyz", "probe URL to fetch")
	expect := fs.String("expect", "ready", "status the probe must report: ready | live")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("healthcheck: unexpected arguments %q", fs.Args())
	}
	return checkHealth(&http.Client{Timeout: healthcheckTimeout}, *target, *expect)
}

func checkHealth(client *http.Client, target, expect string) error {
	if expect != "ready" && expect != "live" {
		return fmt.Errorf("healthcheck: -expect must be ready or live, not %q", expect)
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("healthcheck: -url %q is not an absolute http(s) URL", target)
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return fmt.Errorf("healthcheck: %s answered HTTP %d without a JSON status", target, resp.StatusCode)
	}
	if body.Status != expect {
		msg := fmt.Sprintf("healthcheck: %s answered HTTP %d status %q, want %q", target, resp.StatusCode, body.Status, expect)
		if body.Reason != "" {
			msg += " (reason " + body.Reason + ")"
		}
		return errors.New(msg)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s answered HTTP %d", target, resp.StatusCode)
	}
	fmt.Println("healthcheck:", strings.TrimSpace(body.Status))
	return nil
}
