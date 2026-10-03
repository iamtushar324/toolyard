package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/clerk"
)

// connectAZP parses -connect-azp: the comma-separated browser origins of
// bkt3 (our T3 Code fork) whose Clerk session tokens POST /v1/connect/t3
// swaps for the person's agent token. Each must be an https origin with
// no path, query or credentials (plain http only on loopback, for local
// testing); they come back normalised the way the
// clerk package compares azp (lowercase, no trailing slash). An entry
// "tailnet:<name>.ts.net" admits a whole Tailscale tailnet instead: any
// http or https origin, any port, on a host under that name or a
// Tailscale address (see clerk.TailnetPartyPrefix); "https://*.<domain>"
// admits any https origin under that domain. The Clerk checks
// (signature, freshness, org membership) still apply, and the
// dashboard's own origin never passes. Empty means
// the endpoint is off. Set without Clerk sign-in, or naming the
// dashboard's own origin (-public-url), is a startup error: dashboard
// tokens must never connect.
func connectAZP(raw, publicURL string, clerkOn bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if name, ok := strings.CutPrefix(strings.ToLower(part), clerk.TailnetPartyPrefix); ok {
			name = strings.Trim(name, ".")
			labels := strings.Split(name, ".")
			// Only a tailnet name (<tailnet>.ts.net): a wider suffix such as
			// a company domain would admit every host under it.
			if !strings.HasSuffix(name, ".ts.net") || len(labels) != 3 || labels[0] == "" {
				return nil, fmt.Errorf("-connect-azp: %q must name one tailnet, like tailnet:example-tailnet.ts.net", part)
			}
			o := clerk.TailnetPartyPrefix + name
			if !seen[o] {
				seen[o] = true
				out = append(out, o)
			}
			continue
		}
		if strings.Contains(part, "*") {
			// A domain wildcard: https://*.<domain> only, and the domain must
			// have at least two labels (never *.com or *.co).
			scheme, rest, _ := strings.Cut(strings.ToLower(part), "://")
			domain, ok := strings.CutPrefix(rest, "*.")
			domain = strings.TrimRight(domain, "/")
			if scheme != "https" || !ok || strings.ContainsAny(domain, "/:@?#*") ||
				len(strings.Split(domain, ".")) < 2 || strings.HasPrefix(domain, ".") {
				return nil, fmt.Errorf("-connect-azp: %q must be a wildcard like https://*.example.com (https only)", part)
			}
			o := "https://*." + domain
			if !seen[o] {
				seen[o] = true
				out = append(out, o)
			}
			continue
		}
		u, err := url.Parse(part)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			strings.TrimRight(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("-connect-azp: %q is not an origin like https://bkt3.example.com", part)
		}
		if strings.EqualFold(u.Scheme, "http") && !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("-connect-azp: %q must be https (plain http is allowed only for localhost, 127.0.0.1 or ::1)", part)
		}
		o := clerk.NormalizeOrigin(u.Scheme + "://" + u.Host)
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	if !clerkOn {
		return nil, errors.New("-connect-azp needs Clerk sign-in (the TOOLYARD_CLERK_* env vars): the endpoint verifies Clerk session tokens")
	}
	if pu, err := url.Parse(publicURL); err == nil && pu.Host != "" {
		dash := clerk.NormalizeOrigin(pu.Scheme + "://" + pu.Host)
		if seen[dash] {
			return nil, fmt.Errorf("-connect-azp must not include the dashboard's own origin %s", dash)
		}
	}
	return out, nil
}

// isLoopbackHost: localhost, or a loopback IP (127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
