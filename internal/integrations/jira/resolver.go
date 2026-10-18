package jira

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// Limits of the lists that the resolver reads.
const (
	listPageSize = 50
	maxListPages = 100
)

// NameResolverOptions configures a NameResolver.
type NameResolverOptions struct {
	Store      core.Store
	Secrets    core.SecretStore
	HTTPClient *http.Client
	// Now overrides the clock (tests).
	Now    func() time.Time
	Logger *slog.Logger
}

// NameResolver implements core.NameResolver for Jira: it maps project names
// to project keys (the project_keys filter) and status names to status IDs
// (the status_ids filter). It lists the projects and statuses of each Jira
// integration of the team, keeps the lists in memory, and uses the rules of
// integrations.NameLists (cache time, one extra read for a missing name,
// back-off after an error, stale lists).
type NameResolver struct {
	opts  NameResolverOptions
	log   *slog.Logger
	lists integrations.NameLists
}

var _ core.NameResolver = (*NameResolver)(nil)

// NewNameResolver creates the resolver.
func NewNameResolver(opts NameResolverOptions) *NameResolver {
	r := &NameResolver{opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "jira-names")}
	r.lists.Now = opts.Now
	return r
}

// ResolveNames implements core.NameResolver. kind is projects (the result
// holds project keys) or statuses (the result holds status IDs). A name
// ignores case. When the team has more than one Jira integration, the result
// holds the values of all of them. An error means that the names cannot be
// resolved now.
func (r *NameResolver) ResolveNames(ctx context.Context, teamID, provider, kind string, names []string, _ map[string][]string) (map[string][]string, error) {
	if !strings.EqualFold(provider, Provider) {
		return nil, fmt.Errorf("the Jira resolver cannot list %s objects", provider)
	}
	switch kind {
	case "projects", "statuses":
	default:
		return nil, fmt.Errorf("the Jira resolver cannot resolve %q", kind)
	}
	all, err := forgeutil.TeamIntegrations(ctx, r.opts.Store, teamID, Provider)
	if err != nil {
		return nil, fmt.Errorf("could not read the Jira integrations: %w", err)
	}
	var ints []core.Integration
	for _, in := range all {
		if in.Active() {
			ints = append(ints, in)
		}
	}
	if len(ints) == 0 {
		return nil, errors.New("the team has no active Jira integration")
	}
	keys := make([]string, len(names))
	for i, n := range names {
		keys[i] = strings.ToLower(strings.TrimSpace(n))
	}
	out := make(map[string][]string, len(names))
	var firstErr error
	ok := 0
	for _, in := range ints {
		byName, err := r.lookup(ctx, in, kind, keys)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
		for i, n := range names {
			for _, v := range byName[keys[i]] {
				integrations.AddID(out, n, v)
			}
		}
	}
	if ok == 0 {
		return nil, firstErr
	}
	integrations.SortIDs(out)
	return out, nil
}

func (r *NameResolver) lookup(ctx context.Context, in core.Integration, kind string, keys []string) (map[string][]string, error) {
	tok, err := forgeutil.IntegrationSecret(ctx, r.opts.Secrets, in, "")
	if err != nil {
		return nil, fmt.Errorf("Jira integration credential: %w", err) //nolint:staticcheck // ST1005: the error string starts with a proper noun
	}
	api, err := NewAPIClient(in, tok, r.opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	byName, err := r.lists.Lookup(ctx, in.ID+"/"+kind, keys, func(ctx context.Context) (map[string][]string, error) {
		if kind == "projects" {
			return listProjects(ctx, api)
		}
		return listStatuses(ctx, api)
	})
	if err != nil {
		var ae *forgeutil.APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
			return nil, fmt.Errorf("Jira refused the credential of the integration, so the %s cannot be listed: %w", kind, err) //nolint:staticcheck // ST1005: the error string starts with a proper noun
		}
		return nil, err
	}
	return byName, nil
}

// listProjects lists the projects that the account can see, by name. The
// value is the project key.
func listProjects(ctx context.Context, api *forgeutil.Client) (map[string][]string, error) {
	out := map[string][]string{}
	for page, start := 0, 0; page < maxListPages; page++ {
		var resp struct {
			Values []struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"values"`
			IsLast bool `json:"isLast"`
		}
		q := url.Values{"startAt": {fmt.Sprint(start)}, "maxResults": {fmt.Sprint(listPageSize)}}
		if err := api.Do(ctx, http.MethodGet, "/rest/api/3/project/search?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, p := range resp.Values {
			integrations.AddName(out, p.Name, p.Key)
		}
		if resp.IsLast || len(resp.Values) == 0 {
			break
		}
		start += len(resp.Values)
	}
	return out, nil
}

// listStatuses lists the workflow statuses, by name. The value is the status
// ID.
func listStatuses(ctx context.Context, api *forgeutil.Client) (map[string][]string, error) {
	var resp []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := api.Do(ctx, http.MethodGet, "/rest/api/3/status", nil, &resp); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, s := range resp {
		integrations.AddName(out, s.Name, s.ID)
	}
	return out, nil
}
