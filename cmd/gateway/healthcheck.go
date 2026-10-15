package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// healthcheckTimeout bounds one `toolyard healthcheck` request.
const healthcheckTimeout = 3 * time.Second

// healthcheckEnvPrefix is the prefix of the variables that set the
// healthcheck flags (envconfig.go): TOOLYARD_HEALTHCHECK_URL,
// TOOLYARD_HEALTHCHECK_EXPECT, TOOLYARD_HEALTHCHECK_TOKEN_FILE. Its own
// prefix keeps them apart from serve's (TOOLYARD_URL would say nothing).
const healthcheckEnvPrefix = "TOOLYARD_HEALTHCHECK_"

// runHealthcheck is `toolyard healthcheck -url <u> -expect ready|live
// [-token-file <path>]`: it fetches a probe and succeeds only when the JSON
// body's "status" is the expected one ("ready" for /v1/readyz, "live" for
// /v1/livez). Any other answer, a timeout or a bad flag is an error, so the
// process exits 1. With -token-file it sends the file's content (surrounding
// whitespace trimmed) as "Authorization: Bearer <token>", for an ops
// listener started with -ops-token-file; the token is never printed. It
// needs nothing outside the binary (container images have no curl or
// wget).
func runHealthcheck(argv []string) error { return runHealthcheckEnv(argv, os.LookupEnv) }

func runHealthcheckEnv(argv []string, lookup func(string) (string, bool)) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	target := fs.String("url", "http://127.0.0.1:8787/v1/readyz", "probe URL to fetch")
	expect := fs.String("expect", "ready", "status the probe must report: ready | live")
	tokenFile := fs.String("token-file", "", "file holding a bearer token to send as Authorization: Bearer <token>, for an ops listener that requires one (-ops-token-file). Surrounding whitespace is trimmed; the token is never printed.")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("healthcheck: unexpected arguments %q", fs.Args())
	}
	if err := newEnvLayer(fs, healthcheckEnvPrefix).apply(lookup); err != nil {
		// %v, not %w: every healthcheck failure exits 1, which a container
		// runtime reads as unhealthy (a bad variable is no exception).
		return fmt.Errorf("healthcheck: %v", err)
	}
	token, err := readBearerToken(*tokenFile)
	if err != nil {
		return err
	}
	return checkHealthToken(&http.Client{Timeout: healthcheckTimeout}, *target, *expect, token)
}

// readBearerToken reads the -token-file of healthcheck: "" for no file. A
// file that cannot be read, is empty or holds more than one line is an
// error that names the path, never the content.
func readBearerToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// The *PathError names the path and the cause, not the content.
		return "", fmt.Errorf("healthcheck: -token-file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("healthcheck: -token-file %s is empty", path)
	}
	if strings.ContainsAny(tok, "\r\n") {
		return "", fmt.Errorf("healthcheck: -token-file %s holds more than one line; it must hold only the token", path)
	}
	return tok, nil
}

func checkHealth(client *http.Client, target, expect string) error {
	return checkHealthToken(client, target, expect, "")
}

// checkHealthToken is checkHealth that also sends token, when not empty, as
// a bearer token. No message quotes the token.
func checkHealthToken(client *http.Client, target, expect, token string) error {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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
