package main

import (
	"encoding/json"
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
	AllowURLCredentials bool
	AllowToolSubset     bool
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
// of 8 or more characters, which could be a token, replaced by "…".
func displayURL(u *url.URL) string {
	segs := strings.Split(u.EscapedPath(), "/")
	for i, s := range segs {
		if len(s) >= 8 {
			segs[i] = "…"
		}
	}
	return u.Scheme + "://" + u.Host + strings.Join(segs, "/")
}

// tokenLike is a path segment that reads like a key rather than a word:
// long, or mixing letters and digits.
func tokenLike(seg string) bool {
	if len(seg) >= 20 {
		return true
	}
	if len(seg) < 12 {
		return false
	}
	letters := strings.IndexFunc(seg, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }) >= 0
	digits := strings.IndexFunc(seg, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0
	return letters && digits
}

func urlCarriesCredentials(u *url.URL) bool {
	if u.User != nil || u.RawQuery != "" {
		return true
	}
	for _, s := range strings.Split(u.EscapedPath(), "/") {
		if tokenLike(s) {
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
	// toolyard switches every new server on, so one that is off in Bifrost
	// stays behind rather than going live.
	if c.Disabled {
		return skip("disabled in Bifrost")
	}
	// Bifrost's tools_to_execute: ["*"] is every tool, a list is only those,
	// and empty or unset is none. toolyard lists every tool a server has.
	switch {
	case !c.ToolsListed || len(c.ToolsToExecute) == 0:
		return skip("Bifrost exposes none of its tools")
	case !(len(c.ToolsToExecute) == 1 && c.ToolsToExecute[0] == "*"):
		if !opt.AllowToolSubset {
			return skip("Bifrost exposes only %d chosen tools and toolyard would list all of them. Rerun with -allow-tool-subset to import it anyway", len(c.ToolsToExecute))
		}
		p.Notes = append(p.Notes, fmt.Sprintf("Bifrost exposed only %d chosen tools; toolyard lists all of them", len(c.ToolsToExecute)))
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
	if extra := without(c.AllowedExtraHeaders, virtualKeyHeader); len(extra) > 0 {
		p.Notes = append(p.Notes, "Bifrost passed caller headers through ("+strings.Join(extra, ", ")+"); toolyard does not")
	}

	srv := toolyardServer{Name: c.Name, Transport: "http", URL: raw, Enabled: true}
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
		// Bifrost sends a stored Authorization only for header auth; for
		// OAuth and "none" it never reaches the wire (StaticConfigHeaders).
		if strings.EqualFold(h, "Authorization") && c.AuthType != "" && c.AuthType != "headers" {
			p.Notes = append(p.Notes, "stored Authorization header not copied: Bifrost doesn't send it for auth type "+c.AuthType)
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

// scrub hides anything in text that could be one of p's credentials: its
// header values and its URL, whole or in parts. toolyard's connect warnings
// quote the upstream URL, and sometimes the upstream's own reply.
//
// Each value is hidden whole, word by word (an upstream may echo the token
// without its "Bearer "), and in its JSON-escaped form.
func (p planItem) scrub(text string) string {
	var hide []string
	words := func(v string) {
		hide = append(hide, v)
		for _, w := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ' ' || r == ',' || r == ';' || r == '"' || r == '\''
		}) {
			if len(w) >= 8 {
				hide = append(hide, w)
			}
		}
	}
	for _, s := range p.Secrets {
		words(s.Value)
	}
	if u, err := url.Parse(p.Server.URL); err == nil && p.Server.URL != "" {
		hide = append(hide, p.Server.URL, u.String(), u.RawQuery)
		if u.User != nil {
			hide = append(hide, u.User.String(), u.User.Username())
			if pw, ok := u.User.Password(); ok {
				hide = append(hide, pw)
			}
		}
		for _, vals := range u.Query() {
			for _, v := range vals {
				words(v)
				hide = append(hide, url.QueryEscape(v))
			}
		}
		for _, path := range []string{u.Path, u.EscapedPath()} {
			for _, seg := range strings.Split(path, "/") {
				if len(seg) >= 8 {
					hide = append(hide, seg)
				}
			}
		}
	}
	plain := append([]string(nil), hide...)
	for _, h := range plain {
		if b, err := json.Marshal(h); err == nil {
			hide = append(hide, strings.Trim(string(b), `"`))
		}
	}
	sort.Slice(hide, func(i, j int) bool { return len(hide[i]) > len(hide[j]) })
	for _, h := range hide {
		if len(h) >= 4 {
			text = strings.ReplaceAll(text, h, "[redacted]")
		}
	}
	return text
}
