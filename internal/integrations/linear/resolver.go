package linear

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// Limits of the lists that the resolver reads.
const (
	listPageSize = 100
	maxListPages = 50
	// issueMatches is the most issues that one issue number can give (the
	// same number exists in each team).
	issueMatches = 50
)

// NameResolverOptions configures a NameResolver.
type NameResolverOptions struct {
	Store      core.Store
	Secrets    core.SecretStore
	GraphQLURL string
	HTTPClient *http.Client
	// Now overrides the clock (tests).
	Now    func() time.Time
	Logger *slog.Logger
}

// NameResolver implements core.NameResolver for Linear: it maps the names in
// the filters of automations to Linear IDs: teams (team_ids), projects
// (project_ids), states (state_ids) and issue numbers (issue_ids). It lists
// the objects of each Linear integration of the team, keeps the lists in
// memory, and uses the rules of integrations.NameLists (cache time, one
// extra read for a missing name, back-off after an error, stale lists).
type NameResolver struct {
	opts  NameResolverOptions
	log   *slog.Logger
	lists integrations.NameLists
}

var _ core.NameResolver = (*NameResolver)(nil)

// NewNameResolver creates the resolver.
func NewNameResolver(opts NameResolverOptions) *NameResolver {
	r := &NameResolver{opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "linear-names")}
	r.lists.Now = opts.Now
	return r
}

// ResolveNames implements core.NameResolver. kind is teams, projects, states
// or issues. A name ignores case. An issue is a number, such as "42". The
// scope key team_ids limits projects, states and issues to those teams. When
// the team has more than one Linear integration, the result holds the IDs of
// all of them. An error means that the names cannot be resolved now.
func (r *NameResolver) ResolveNames(ctx context.Context, teamID, provider, kind string, names []string, scope map[string][]string) (map[string][]string, error) {
	if !strings.EqualFold(provider, Provider) {
		return nil, fmt.Errorf("the Linear resolver cannot list %s objects", provider)
	}
	switch kind {
	case "teams", "projects", "states", "issues":
	default:
		return nil, fmt.Errorf("the Linear resolver cannot resolve %q", kind)
	}
	all, err := forgeutil.TeamIntegrations(ctx, r.opts.Store, teamID, Provider)
	if err != nil {
		return nil, fmt.Errorf("could not read the Linear integrations: %w", err)
	}
	var ints []core.Integration
	for _, in := range all {
		if in.Active() {
			ints = append(ints, in)
		}
	}
	if len(ints) == 0 {
		return nil, errors.New("the team has no active Linear integration")
	}
	keys := make([]string, len(names))
	for i, n := range names {
		keys[i] = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n), "#")))
	}
	teams := scopeTeams(scope)
	out := make(map[string][]string, len(names))
	var firstErr error
	ok := 0
	for _, in := range ints {
		byName, err := r.lookup(ctx, in, kind, keys, teams)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
		for i, n := range names {
			for _, id := range byName[keys[i]] {
				integrations.AddID(out, n, id)
			}
		}
	}
	if ok == 0 {
		return nil, firstErr
	}
	integrations.SortIDs(out)
	return out, nil
}

// scopeTeams returns the resolved team IDs of the scope, sorted.
func scopeTeams(scope map[string][]string) []string {
	ids := append([]string(nil), scope["team_ids"]...)
	sort.Strings(ids)
	return ids
}

func inTeams(teams []string, id string) bool {
	if len(teams) == 0 {
		return true
	}
	for _, t := range teams {
		if t == id {
			return true
		}
	}
	return false
}

// lookup returns the names of one kind of one integration, with the rules of
// integrations.NameLists. Issue numbers have no list. Each number is read
// and cached by itself.
func (r *NameResolver) lookup(ctx context.Context, in core.Integration, kind string, keys, teams []string) (map[string][]string, error) {
	base := in.ID + "/" + kind + "/" + strings.Join(teams, ",")
	c, err := r.client(ctx, in)
	if err != nil {
		return nil, err
	}
	if kind != "issues" {
		byName, err := r.lists.Lookup(ctx, base, keys, func(ctx context.Context) (map[string][]string, error) {
			return listKind(ctx, c, kind, teams)
		})
		return describeListError(kind, err, byName)
	}
	out := map[string][]string{}
	for _, k := range keys {
		n, err := strconv.Atoi(k)
		if err != nil || n <= 0 {
			continue // not an issue number: no match
		}
		byName, err := r.lists.Lookup(ctx, base+"/"+k, []string{k}, func(ctx context.Context) (map[string][]string, error) {
			return findIssues(ctx, c, n, teams)
		})
		if err != nil {
			return describeListError(kind, err, nil)
		}
		for key, ids := range byName {
			for _, id := range ids {
				integrations.AddID(out, key, id)
			}
		}
	}
	return out, nil
}

func (r *NameResolver) client(ctx context.Context, in core.Integration) (*Client, error) {
	tok, err := forgeutil.IntegrationSecret(ctx, r.opts.Secrets, in, "")
	if err != nil {
		return nil, fmt.Errorf("Linear integration credential: %w", err) //nolint:staticcheck // ST1005: the error string starts with a proper noun
	}
	return NewClient(r.opts.GraphQLURL, string(tok), in.Config["token_type"], r.opts.HTTPClient), nil
}

func describeListError(kind string, err error, byName map[string][]string) (map[string][]string, error) {
	if err == nil {
		return byName, nil
	}
	var ae *forgeutil.APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
		return nil, fmt.Errorf("Linear refused the credential of the integration, so the %s cannot be listed: %w", kind, err) //nolint:staticcheck // ST1005: the error string starts with a proper noun
	}
	var ge *GraphQLError
	if errors.As(err, &ge) && strings.Contains(strings.ToLower(ge.Error()), "authenticat") {
		return nil, fmt.Errorf("Linear refused the credential of the integration, so the %s cannot be listed: %w", kind, err) //nolint:staticcheck // ST1005: the error string starts with a proper noun
	}
	return nil, err
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type idNode struct {
	ID string `json:"id"`
}

// listKind reads all objects of a kind and returns them by lower-case name.
// Projects and states are limited to the teams, when there are any.
func listKind(ctx context.Context, c *Client, kind string, teams []string) (map[string][]string, error) {
	out := map[string][]string{}
	var after any
	for page := 0; page < maxListPages; page++ {
		vars := map[string]any{"first": listPageSize, "after": after}
		var more pageInfo
		switch kind {
		case "teams":
			var d struct {
				Teams struct {
					Nodes    []struct{ ID, Name string } `json:"nodes"`
					PageInfo pageInfo                    `json:"pageInfo"`
				} `json:"teams"`
			}
			if err := c.Query(ctx, `query($first: Int, $after: String) { teams(first: $first, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } }`, vars, &d); err != nil {
				return nil, err
			}
			for _, n := range d.Teams.Nodes {
				if inTeams(teams, n.ID) {
					integrations.AddName(out, n.Name, n.ID)
				}
			}
			more = d.Teams.PageInfo
		case "projects":
			var d struct {
				Projects struct {
					Nodes []struct {
						ID, Name string
						Teams    struct {
							Nodes []idNode `json:"nodes"`
						} `json:"teams"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"projects"`
			}
			if err := c.Query(ctx, `query($first: Int, $after: String) { projects(first: $first, after: $after) { nodes { id name teams(first: 50) { nodes { id } } } pageInfo { hasNextPage endCursor } } }`, vars, &d); err != nil {
				return nil, err
			}
			for _, n := range d.Projects.Nodes {
				for _, t := range n.Teams.Nodes {
					if inTeams(teams, t.ID) {
						integrations.AddName(out, n.Name, n.ID)
						break
					}
				}
			}
			more = d.Projects.PageInfo
		case "states":
			var d struct {
				States struct {
					Nodes []struct {
						ID, Name string
						Team     idNode `json:"team"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"workflowStates"`
			}
			if err := c.Query(ctx, `query($first: Int, $after: String) { workflowStates(first: $first, after: $after) { nodes { id name team { id } } pageInfo { hasNextPage endCursor } } }`, vars, &d); err != nil {
				return nil, err
			}
			for _, n := range d.States.Nodes {
				if inTeams(teams, n.Team.ID) {
					integrations.AddName(out, n.Name, n.ID)
				}
			}
			more = d.States.PageInfo
		}
		if !more.HasNextPage || more.EndCursor == "" {
			return out, nil
		}
		after = more.EndCursor
	}
	return out, nil
}

// findIssues returns the issues with a number, in the teams (all teams when
// there are none). The result maps the number to the issue IDs.
func findIssues(ctx context.Context, c *Client, number int, teams []string) (map[string][]string, error) {
	var d struct {
		Issues struct {
			Nodes []struct {
				ID   string `json:"id"`
				Team idNode `json:"team"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	q := `query($n: Float!, $first: Int) { issues(first: $first, filter: { number: { eq: $n } }) { nodes { id team { id } } } }`
	if err := c.Query(ctx, q, map[string]any{"n": number, "first": issueMatches}, &d); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	key := strconv.Itoa(number)
	for _, n := range d.Issues.Nodes {
		if inTeams(teams, n.Team.ID) {
			integrations.AddID(out, key, n.ID)
		}
	}
	return out, nil
}
