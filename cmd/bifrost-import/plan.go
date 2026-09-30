package main

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// secretItem is one toolyard secret the import creates. Value is a live
// credential: never print it.
type secretItem struct {
	Name   string
	Header string
	Value  string
}

// toolyardServer mirrors the POST /v1/servers body (upstreams.Server).
type toolyardServer struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	Identity  *identity         `json:"identity,omitempty"`
	Enabled   bool              `json:"enabled"`
}

type identity struct {
	Header string `json:"header"`
}

type planItem struct {
	Name    string
	Skip    string // non-empty: not imported, and why
	Notes   []string
	Where   string // scheme://host/path with long path segments hidden
	Server  toolyardServer
	Secrets []secretItem
}

type planOptions struct {
	Existing            map[string]bool // server names already in toolyard; nil when not checked
	URLMap              map[string]string
	SecretPrefix        string
	CreateDisabled      bool
	AllowURLCredentials bool
}

var nonAlnum = regexp.MustCompile(`[^A-Z0-9]+`)

// secretName builds a toolyard secret name (^[A-Z][A-Z0-9_]{0,63}$) for one
// header of one server.
func secretName(prefix, server, header string) string {
	part := func(s string) string {
		return strings.Trim(nonAlnum.ReplaceAllString(strings.ToUpper(s), "_"), "_")
	}
	n := part(prefix) + "_" + part(server) + "_" + part(header)
	n = strings.Trim(n, "_")
	if n == "" || n[0] < 'A' || n[0] > 'Z' {
		n = "S_" + n
	}
	if len(n) > 64 {
		n = strings.TrimRight(n[:64], "_")
	}
	return n
}

// privateHost reports whether toolyard, outside Bifrost's network, cannot
// reach host: Swarm service names, localhost and private address ranges.
func privateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	h := strings.ToLower(host)
	return !strings.Contains(h, ".") || strings.HasSuffix(h, ".internal") ||
		strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".localhost")
}

// displayURL is safe to print: no userinfo, no query, and any path segment
// long enough to be a token replaced by "…".
func displayURL(u *url.URL) string {
	segs := strings.Split(u.EscapedPath(), "/")
	for i, s := range segs {
		if len(s) >= 20 {
			segs[i] = "…"
		}
	}
	return u.Scheme + "://" + u.Host + strings.Join(segs, "/")
}

func urlCarriesCredentials(u *url.URL) bool {
	if u.User != nil || u.RawQuery != "" {
		return true
	}
	for _, s := range strings.Split(u.EscapedPath(), "/") {
		if len(s) >= 20 {
			return true
		}
	}
	return false
}

func buildPlan(clients []bifrostClient, opt planOptions) []planItem {
	out := make([]planItem, 0, len(clients))
	for _, c := range clients {
		out = append(out, planOne(c, opt))
	}
	return out
}

func planOne(c bifrostClient, opt planOptions) planItem {
	p := planItem{Name: c.Name}
	skip := func(format string, a ...any) planItem {
		p.Skip = fmt.Sprintf(format, a...)
		return p
	}

	switch c.ConnType {
	case "http":
	case "stdio":
		return skip("runs as a local program (%s) inside the Bifrost container; toolyard's public instance refuses local programs", c.StdioCommand)
	case "sse":
		return skip("SSE transport; toolyard speaks streamable HTTP only")
	case "inprocess":
		return skip("built into Bifrost itself")
	default:
		return skip("unknown connection type %q", c.ConnType)
	}
	switch c.AuthType {
	case "per_user_oauth":
		return skip("each person signs in with their own account; toolyard keeps one sign-in per server")
	case "per_user_headers":
		return skip("each person supplies their own %s; toolyard can't hold per-person headers", strings.Join(c.PerUserHeaderKeys, ", "))
	case "oauth":
		p.Notes = append(p.Notes, "shared OAuth: sign in once on toolyard's Servers page after the import")
	case "", "none", "headers":
	default:
		return skip("unknown auth type %q", c.AuthType)
	}
	if c.ReadErr != "" {
		return skip("could not read it: %s", c.ReadErr)
	}
	if opt.Existing != nil && opt.Existing[c.Name] {
		return skip("a server with this name is already in toolyard")
	}

	raw := c.URL
	if mapped, ok := opt.URLMap[c.Name]; ok {
		raw = mapped
		p.Notes = append(p.Notes, "URL taken from -url-map")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return skip("URL is not an http(s) address")
	}
	p.Where = displayURL(u)
	if privateHost(u.Hostname()) {
		return skip("private address %s, reachable only inside Bifrost's network; give it a public URL in -url-map to import it", u.Host)
	}
	if urlCarriesCredentials(u) && !opt.AllowURLCredentials {
		return skip("URL seems to carry a credential (query, user info or a token-like path); toolyard stores URLs in plain text. Rerun with -allow-url-credentials to import it anyway")
	}
	if u.Scheme == "http" {
		p.Notes = append(p.Notes, "plain http")
	}
	if c.HasTLSConfig {
		p.Notes = append(p.Notes, "custom TLS settings in Bifrost are not copied")
	}
	if c.ToolsListed {
		all := len(c.ToolsToExecute) == 1 && c.ToolsToExecute[0] == "*"
		switch {
		case len(c.ToolsToExecute) == 0:
			p.Notes = append(p.Notes, "Bifrost exposed none of its tools; toolyard will list all of them")
		case !all:
			p.Notes = append(p.Notes, fmt.Sprintf("Bifrost exposed only %d chosen tools; toolyard will list all of them", len(c.ToolsToExecute)))
		}
	}
	if extra := without(c.AllowedExtraHeaders, virtualKeyHeader); len(extra) > 0 {
		p.Notes = append(p.Notes, "Bifrost passed caller headers through ("+strings.Join(extra, ", ")+"); toolyard does not")
	}

	srv := toolyardServer{Name: c.Name, Transport: "http", URL: raw, Enabled: !c.Disabled && !opt.CreateDisabled}
	if c.Disabled {
		p.Notes = append(p.Notes, "disabled in Bifrost, so created disabled")
	}
	names := make([]string, 0, len(c.Headers))
	for k := range c.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	used := map[string]string{}
	for _, h := range names {
		v := c.Headers[h]
		if strings.EqualFold(h, virtualKeyHeader) && v == virtualKeyMarker {
			srv.Identity = &identity{Header: virtualKeyHeader}
			p.Notes = append(p.Notes, "forwards each caller's own key ("+virtualKeyHeader+")")
			continue
		}
		if strings.Contains(v, "{{") {
			return skip("header %s uses a Bifrost template toolyard can't fill", h)
		}
		name := secretName(opt.SecretPrefix, c.Name, h)
		if other, clash := used[name]; clash {
			return skip("headers %s and %s map to the same secret name %s", other, h, name)
		}
		used[name] = h
		if srv.Headers == nil {
			srv.Headers = map[string]string{}
		}
		srv.Headers[h] = "secret://" + name
		p.Secrets = append(p.Secrets, secretItem{Name: name, Header: h, Value: v})
	}
	p.Server = srv
	return p
}

func without(list []string, drop string) []string {
	var out []string
	for _, s := range list {
		if !strings.EqualFold(s, drop) {
			out = append(out, s)
		}
	}
	return out
}
