package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// toolyard guest: manage guest invites (people outside the organisation
// who may sign in, TEC-2058) through /v1/guests with an operator token. A
// change needs a token with the owner scope.

const guestUsage = `toolyard guest — guest invites (people outside the organisation)

  add EMAIL --servers a,b [--tools x,y] [--expires 2026-12-31] [--note TEXT]
                    create or replace the invite for EMAIL
  list              list invites, with each guest's account state
  remove EMAIL      remove the invite; the guest is blocked at once

--servers  servers the guest may use (never memory, events or notes)
--tools    only these tools of those servers (full names, as tools/list
           shows them); empty: every tool of the servers
--expires  last day the invite admits the guest (YYYY-MM-DD, through the
           end of that day UTC), or an RFC 3339 time; empty: never`

func runGuest(argv []string) error {
	fs := flag.NewFlagSet("guest", flag.ExitOnError)
	servers := fs.String("servers", "", "comma-separated server names")
	tools := fs.String("tools", "", "comma-separated full tool names (empty: every tool of the servers)")
	expires := fs.String("expires", "", "YYYY-MM-DD (last day, UTC) or RFC 3339; empty: never")
	note := fs.String("note", "", "a note for other admins")
	fs.Usage = func() { fmt.Println(guestUsage) }
	pos, err := splitFlags(fs, argv)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		fmt.Println(guestUsage)
		return nil
	}
	switch pos[0] {
	case "add":
		if len(pos) != 2 || strings.TrimSpace(*servers) == "" {
			return errors.New("usage: toolyard guest add EMAIL --servers a,b [--tools x,y] [--expires 2026-12-31] [--note TEXT]")
		}
		body := map[string]any{
			"email":   pos[1],
			"servers": splitList(*servers),
			"tools":   splitList(*tools),
			"note":    *note,
		}
		if strings.TrimSpace(*expires) != "" {
			ms, err := parseGuestExpiry(*expires)
			if err != nil {
				return err
			}
			body["expires_at"] = ms
		}
		c, err := newOpClient()
		if err != nil {
			return err
		}
		return c.show("POST", "/v1/guests", body)
	case "list":
		c, err := newOpClient()
		if err != nil {
			return err
		}
		return c.show("GET", "/v1/guests", nil)
	case "remove":
		if len(pos) != 2 {
			return errors.New("usage: toolyard guest remove EMAIL")
		}
		c, err := newOpClient()
		if err != nil {
			return err
		}
		return c.show("DELETE", "/v1/guests/"+url.PathEscape(strings.TrimSpace(pos[1])), nil)
	case "help", "-h", "--help":
		fmt.Println(guestUsage)
		return nil
	}
	return fmt.Errorf("unknown guest command %q (try `toolyard guest help`)", pos[0])
}

// splitList splits a comma-separated flag into trimmed, non-empty names;
// an empty flag is an empty list (never null in the JSON body).
func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseGuestExpiry turns --expires into Unix milliseconds. A date is the
// last day the invite admits the guest, so it ends at the next midnight
// UTC; an RFC 3339 time is taken as it is.
func parseGuestExpiry(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.AddDate(0, 0, 1).UnixMilli(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	return 0, fmt.Errorf("--expires %q: want YYYY-MM-DD or an RFC 3339 time", s)
}
