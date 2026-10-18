package teams

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// DefaultGraphURL is the Microsoft Graph endpoint.
const DefaultGraphURL = "https://graph.microsoft.com/v1.0"

// maxGraphPages is the most pages of one Graph list that the server reads.
const maxGraphPages = 50

func (h *Handler) graphURL() string {
	if h.opts.GraphURL != "" {
		return strings.TrimRight(h.opts.GraphURL, "/")
	}
	return DefaultGraphURL
}

// graphToken returns a token source for Microsoft Graph. It uses the bot of
// the record with the client-credentials grant in the tenant of the
// installation: the tenant_id of the record, or tenant when the record has
// none. The permissions of the bot decide what Graph returns (the resource
// permissions of the app manifest, such as ChannelMessage.Read.Group).
func (h *Handler) graphToken(ctx context.Context, in *core.Integration, tenant string) (*TokenSource, error) {
	appID, password, err := h.botCredentials(ctx, in)
	if err != nil {
		return nil, err
	}
	if in != nil && strings.TrimSpace(in.Config["tenant_id"]) != "" {
		tenant = strings.TrimSpace(in.Config["tenant_id"])
	}
	if tenant == "" {
		return nil, errors.New("teams: Microsoft Graph needs the Microsoft 365 tenant: set config tenant_id on the integration record")
	}
	key := appID + "|" + tenant + "|graph"
	h.mu.Lock()
	defer h.mu.Unlock()
	ts, ok := h.tokens[key]
	if !ok || ts.AppPassword != password {
		ts = &TokenSource{AppID: appID, AppPassword: password, Tenant: tenant, Scope: GraphScope, LoginURL: h.opts.LoginURL, HTTPClient: h.opts.HTTPClient, Now: h.opts.Now}
		h.tokens[key] = ts
	}
	return ts, nil
}

// graphClient returns a Microsoft Graph client for a record. See graphToken.
func (h *Handler) graphClient(ctx context.Context, in *core.Integration, tenant string) (*forgeutil.Client, error) {
	ts, err := h.graphToken(ctx, in, tenant)
	if err != nil {
		return nil, err
	}
	tok, err := ts.Token(ctx)
	if err != nil {
		return nil, err
	}
	return &forgeutil.Client{
		BaseURL: h.graphURL(), HTTP: h.opts.HTTPClient, UserAgent: "openfactory-teams",
		Authorize: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+tok)
			return nil
		},
	}, nil
}

// graphPages reads a Graph list and calls each with the "value" items of each
// page. It follows @odata.nextLink only on the Graph host, so the token never
// goes to another host. each returns false to stop. It stops after
// maxGraphPages pages.
func (h *Handler) graphPages(ctx context.Context, c *forgeutil.Client, path string, each func(items []map[string]any) bool) error {
	base, err := url.Parse(h.graphURL())
	if err != nil {
		return err
	}
	next := path
	for page := 0; next != "" && page < maxGraphPages; page++ {
		var resp struct {
			Value []map[string]any `json:"value"`
			Next  string           `json:"@odata.nextLink"`
		}
		if err := c.Do(ctx, http.MethodGet, next, nil, &resp); err != nil {
			return err
		}
		if !each(resp.Value) {
			return nil
		}
		next = ""
		if resp.Next != "" {
			if u, err := url.Parse(resp.Next); err == nil && u.Scheme == base.Scheme && u.Host == base.Host {
				next = resp.Next
			}
		}
	}
	return nil
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

// describeGraphError adds a hint to the errors that a missing permission or a
// wrong tenant gives.
func describeGraphError(what, permission string, err error) error {
	var ae *forgeutil.APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("Microsoft Graph refused the request to %s: the bot needs the permission %s in the team: %w", what, permission, err) //nolint:staticcheck // ST1005: the error string starts with a proper noun
		}
	}
	return err
}
