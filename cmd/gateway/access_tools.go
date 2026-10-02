package main

import (
	"context"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// serversAdapter answers the gateway's servers.list and servers.reconnect
// tools from the upstreams service and the OAuth store. It copies only
// what an agent may see: never a URL, header, env value or token. The
// gateway declares the interface (gateway.ServersProvider) because it
// cannot import internal/upstreams, which imports it.
type serversAdapter struct {
	upstreams *upstreams.Service
	oauth     *oauth.Service
}

func (a serversAdapter) ListServers(ctx context.Context, ownerUserID string) ([]gateway.ServerInfo, error) {
	servers, err := a.upstreams.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ServerInfo, 0, len(servers))
	for _, sv := range servers {
		out = append(out, a.info(ctx, sv, ownerUserID))
	}
	return out, nil
}

func (a serversAdapter) ReconnectServer(ctx context.Context, name string) (*gateway.ServerInfo, error) {
	srv, err := a.upstreams.Reconnect(ctx, name)
	if srv == nil {
		return nil, err
	}
	info := a.info(ctx, *srv, "")
	return &info, err
}

// info is the agent-safe view of one persisted server row. Status comes
// from the row the upstreams service keeps (ok, waiting_signin, or the
// last error); sign-in state from the OAuth store: a shared server's
// stored client, or the owner's own connection on a per_user server.
func (a serversAdapter) info(ctx context.Context, sv upstreams.Server, owner string) gateway.ServerInfo {
	info := gateway.ServerInfo{
		Name: sv.Name, Transport: sv.Transport, Enabled: sv.Enabled,
		ToolCount: sv.ToolCount, AuthMode: upstreams.AuthShared,
	}
	if sv.AuthMode == upstreams.AuthPerUser {
		info.AuthMode = upstreams.AuthPerUser
	}
	switch {
	case !sv.Enabled:
		info.Status = "disabled"
	case sv.LastStatus == "ok":
		info.Status = "ok"
	case sv.LastStatus == gateway.StatusWaitingSignIn:
		info.Status = gateway.StatusWaitingSignIn
	case sv.LastError != "":
		info.Status, info.Error = "error", sv.LastError
	default:
		info.Status = "unknown"
	}
	if a.oauth == nil || sv.Transport == "stdio" {
		return info
	}
	if info.AuthMode == upstreams.AuthPerUser {
		info.OAuth = true
		if owner != "" {
			if ok, err := a.oauth.UserConnected(ctx, sv.Name, owner); err == nil {
				info.SignedIn = &ok
			}
		}
		return info
	}
	if has, err := a.oauth.HasClient(ctx, sv.Name); err == nil && has {
		info.OAuth, info.SignedIn = true, &has
	}
	return info
}
