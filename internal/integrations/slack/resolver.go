package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// Timing of the name lists. A list is read again when it is older than
// listTTL. A name that is not in the list gives one more read, but not more
// often than minRefresh (a channel that was just created is found, and a
// typing error does not call the API on each check). After an error the
// resolver does not call the API again for errorBackoff, and it uses the
// old list for up to staleMax.
const (
	listTTL      = 5 * time.Minute
	minRefresh   = 30 * time.Second
	errorBackoff = 30 * time.Second
	staleMax     = time.Hour
	maxPages     = 50
	pageSize     = 200
)

// NameResolverOptions configures a NameResolver.
type NameResolverOptions struct {
	Store      core.Store
	Secrets    core.SecretStore
	APIBaseURL string
	HTTPClient *http.Client
	// Now overrides the clock (tests).
	Now    func() time.Time
	Logger *slog.Logger
}

// NameResolver implements core.NameResolver for Slack: it maps channel names
// and user names or e-mail addresses to Slack IDs, for the name filters of
// automations (channels, users, itemUsers). It lists the conversations and
// users of each Slack integration of the team and keeps the lists in memory.
type NameResolver struct {
	opts NameResolverOptions
	log  *slog.Logger
	now  func() time.Time

	mu    sync.Mutex
	lists map[string]*nameList
}

var _ core.NameResolver = (*NameResolver)(nil)

// NewNameResolver creates the resolver.
func NewNameResolver(opts NameResolverOptions) *NameResolver {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &NameResolver{opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "slack-names"), now: now, lists: map[string]*nameList{}}
}

// nameList is the cached list of one kind (channels or users) of one
// integration. mu is held while the list is read from the API.
type nameList struct {
	mu      sync.Mutex
	byName  map[string][]string
	fetched time.Time
	err     error
	errAt   time.Time
}

// ResolveNames implements core.NameResolver. kind is channels, users or
// itemUsers. A channel name ignores a leading "#". A user name is a username
// or an e-mail address, and the match ignores case. When the team has more
// than one Slack integration, the result holds the IDs of all of them. An
// error means that the names cannot be resolved now.
func (r *NameResolver) ResolveNames(ctx context.Context, teamID, provider, kind string, names []string, _ map[string][]string) (map[string][]string, error) {
	if !strings.EqualFold(provider, Provider) {
		return nil, fmt.Errorf("the Slack resolver cannot list %s objects", provider)
	}
	var list string
	switch kind {
	case "channels":
		list = "channels"
	case "users", "itemUsers":
		list = "users"
	default:
		return nil, fmt.Errorf("the Slack resolver cannot resolve %q", kind)
	}
	all, err := forgeutil.TeamIntegrations(ctx, r.opts.Store, teamID, Provider)
	if err != nil {
		return nil, fmt.Errorf("could not read the Slack integrations: %w", err)
	}
	var ints []core.Integration
	for _, in := range all {
		if in.Active() {
			ints = append(ints, in)
		}
	}
	if len(ints) == 0 {
		return nil, errors.New("the team has no active Slack integration")
	}
	keys := make([]string, len(names))
	for i, n := range names {
		keys[i] = normalizeName(list, n)
	}
	out := make(map[string][]string, len(names))
	var firstErr error
	ok := 0
	for _, in := range ints {
		byName, err := r.lookup(ctx, in, list, keys)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
		for i, n := range names {
			for _, id := range byName[keys[i]] {
				if !containsID(out[n], id) {
					out[n] = append(out[n], id)
				}
			}
		}
	}
	if ok == 0 {
		return nil, firstErr
	}
	for n := range out {
		sort.Strings(out[n])
	}
	return out, nil
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func normalizeName(list, name string) string {
	name = strings.TrimSpace(name)
	if list == "channels" {
		name = strings.TrimSpace(strings.TrimPrefix(name, "#"))
	}
	return strings.ToLower(name)
}

// lookup returns the list of an integration. It reads the list again once
// when a wanted name is missing and the list is older than minRefresh.
func (r *NameResolver) lookup(ctx context.Context, in core.Integration, list string, keys []string) (map[string][]string, error) {
	byName, err := r.list(ctx, in, list, listTTL)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if _, found := byName[k]; !found {
			if again, err := r.list(ctx, in, list, minRefresh); err == nil {
				return again, nil
			}
			break
		}
	}
	return byName, nil
}

func (r *NameResolver) list(ctx context.Context, in core.Integration, kind string, maxAge time.Duration) (map[string][]string, error) {
	key := in.ID + "/" + kind
	r.mu.Lock()
	l := r.lists[key]
	if l == nil {
		l = &nameList{}
		r.lists[key] = l
	}
	r.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	now := r.now()
	if l.byName != nil && now.Sub(l.fetched) < maxAge {
		return l.byName, nil
	}
	if l.err != nil && now.Sub(l.errAt) < errorBackoff {
		return l.stale(now, l.err)
	}
	tok, err := forgeutil.IntegrationSecret(ctx, r.opts.Secrets, in, "")
	var byName map[string][]string
	if err == nil {
		c := NewClient(r.opts.APIBaseURL, string(tok), r.opts.HTTPClient)
		if kind == "channels" {
			byName, err = listChannels(ctx, c)
		} else {
			byName, err = listUsers(ctx, c)
		}
	}
	if err != nil {
		err = describeListError(kind, err)
		l.err, l.errAt = err, now
		r.log.Warn("list slack names", "integration_id", in.ID, "list", kind, "err", err)
		return l.stale(now, err)
	}
	l.byName, l.fetched, l.err = byName, now, nil
	return byName, nil
}

// stale returns the old list when it is not too old, and err otherwise.
func (l *nameList) stale(now time.Time, err error) (map[string][]string, error) {
	if l.byName != nil && now.Sub(l.fetched) < staleMax {
		return l.byName, nil
	}
	return nil, err
}

func describeListError(kind string, err error) error {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "missing_scope":
			scopes := "channels:read and groups:read"
			if kind == "users" {
				scopes = "users:read and users:read.email"
			}
			return fmt.Errorf("the Slack app needs the scopes %s to list %s: %w", scopes, kind, err)
		case "invalid_auth", "token_revoked", "account_inactive", "not_authed":
			return fmt.Errorf("Slack refused the bot token: %w", err) //nolint:staticcheck // ST1005: the error string starts with a proper noun
		}
	}
	return err
}

type listResponse struct {
	Channels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channels"`
	Members []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Deleted bool   `json:"deleted"`
		Profile struct {
			Email string `json:"email"`
		} `json:"profile"`
	} `json:"members"`
	Meta struct {
		Next string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// listChannels lists the public and private channels that the bot can see.
// Without the scope for private channels it lists the public ones.
func listChannels(ctx context.Context, c *Client) (map[string][]string, error) {
	types := "public_channel,private_channel"
	byName, err := pageChannels(ctx, c, types)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == "missing_scope" {
		byName, err = pageChannels(ctx, c, "public_channel")
	}
	return byName, err
}

func pageChannels(ctx context.Context, c *Client, types string) (map[string][]string, error) {
	out := map[string][]string{}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"types": {types}, "exclude_archived": {"true"}, "limit": {fmt.Sprint(pageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp listResponse
		if err := c.Call(ctx, "conversations.list", q, &resp); err != nil {
			return nil, err
		}
		for _, ch := range resp.Channels {
			k := strings.ToLower(ch.Name)
			out[k] = append(out[k], ch.ID)
		}
		if cursor = resp.Meta.Next; cursor == "" {
			break
		}
	}
	return out, nil
}

// listUsers lists the users of the workspace by username and e-mail address.
// Users that were deleted are left out.
func listUsers(ctx context.Context, c *Client) (map[string][]string, error) {
	out := map[string][]string{}
	add := func(k, id string) {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && !containsID(out[k], id) {
			out[k] = append(out[k], id)
		}
	}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"limit": {fmt.Sprint(pageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp listResponse
		if err := c.Call(ctx, "users.list", q, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Members {
			if m.Deleted {
				continue
			}
			add(m.Name, m.ID)
			add(m.Profile.Email, m.ID)
		}
		if cursor = resp.Meta.Next; cursor == "" {
			break
		}
	}
	return out, nil
}
