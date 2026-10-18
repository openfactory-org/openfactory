package integrations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// selectCase is one provider of the selection rule.
type selectCase struct {
	provider, key, ws, other string
	// source is how a source names the workspace (for Jira, a URL of the site).
	source string
}

var selectCases = []selectCase{
	{"slack", "team_id", "T1", "T2", "t1"},
	{"microsoft_teams", "team_id", "aaaa-1111", "bbbb-2222", "AAAA-1111"},
	{"linear", "organization_id", "org-1", "org-2", "org-1"},
	{"jira", "site", "acme.atlassian.net", "beta.atlassian.net", "https://acme.atlassian.net/browse/ENG-1"},
}

func rec(c selectCase, id, team, factory, ws string) core.Integration {
	return core.Integration{ID: id, TeamID: team, Provider: c.provider, FactoryID: factory, Config: map[string]string{c.key: ws}}
}

func TestSelectFactoryRecordWins(t *testing.T) {
	for _, c := range selectCases {
		t.Run(c.provider, func(t *testing.T) {
			ints := []core.Integration{
				rec(c, "int_team", "team_1", "", c.ws),
				rec(c, "int_fac", "team_1", "fac_1", c.ws),
				rec(c, "int_fac2", "team_1", "fac_2", c.ws),
				rec(c, "int_other", "team_1", "fac_1", c.other),
			}
			if in, err := Select(ints, c.provider, "fac_1", c.source); err != nil || in.ID != "int_fac" {
				t.Fatalf("factory: %v %v", in.ID, err)
			}
			// A factory with no record for the workspace uses the team record,
			// never the record of another factory.
			if in, err := Select(ints, c.provider, "fac_3", c.source); err != nil || in.ID != "int_team" {
				t.Fatalf("other factory: %v %v", in.ID, err)
			}
			if in, err := Select(ints, c.provider, "", c.source); err != nil || in.ID != "int_team" {
				t.Fatalf("no factory: %v %v", in.ID, err)
			}
			// A source that does not name its workspace: the factory has two
			// records, so the rule does not guess.
			if _, err := Select(ints, c.provider, "fac_1", ""); !errors.Is(err, ErrNoIntegration) {
				t.Fatalf("no workspace: %v", err)
			}
			if in, err := Select(ints, c.provider, "fac_2", ""); err != nil || in.ID != "int_fac2" {
				t.Fatalf("no workspace, one record: %v %v", in.ID, err)
			}
		})
	}
}

func TestSelectNoMatchNamesWorkspace(t *testing.T) {
	for _, c := range selectCases {
		t.Run(c.provider, func(t *testing.T) {
			store := forgetest.NewStore()
			store.Integrations = []core.Integration{
				rec(c, "int_team", "team_1", "", c.other),
				rec(c, "int_fac", "team_1", "fac_1", c.ws),
			}
			_, err := SelectTeam(context.Background(), store, "team_1", c.provider, "fac_2", c.source)
			want := core.NormalizeWorkspace(c.provider, c.source)
			if !errors.Is(err, ErrNoIntegration) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "config."+c.key) {
				t.Fatalf("error = %v, want one that names %q", err, want)
			}
		})
	}
}

func TestIntake(t *testing.T) {
	for _, c := range selectCases {
		t.Run(c.provider, func(t *testing.T) {
			ctx := context.Background()
			store := forgetest.NewStore()
			store.Teams = []core.Team{{ID: "team_1"}, {ID: "team_2"}}
			store.Integrations = []core.Integration{
				rec(c, "int_fac", "team_1", "fac_1", c.ws),
				rec(c, "int_team", "team_1", "", c.ws),
				rec(c, "int_t2", "team_2", "", c.other),
			}
			if in, err := Intake(ctx, store, c.provider, c.source, nil); err != nil || in.ID != "int_team" {
				t.Fatalf("team record: %v %v", in, err)
			}
			// match keeps the records that can have received the event.
			only := func(id string) func(core.Integration) bool {
				return func(in core.Integration) bool { return in.ID == id }
			}
			if in, err := Intake(ctx, store, c.provider, c.source, only("int_fac")); err != nil || in.FactoryID != "fac_1" {
				t.Fatalf("factory record: %v %v", in, err)
			}
			if _, err := Intake(ctx, store, c.provider, "unknown", nil); !errors.Is(err, ErrNoIntegration) || !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("no match: %v", err)
			}
			// Records of two teams for one workspace: no guess.
			store.Integrations = append(store.Integrations, rec(c, "int_t2b", "team_2", "", c.ws))
			if _, err := Intake(ctx, store, c.provider, c.source, nil); !errors.Is(err, ErrNoIntegration) {
				t.Fatalf("two teams: %v", err)
			}
		})
	}
}

// A selected record that is not active fails with ErrIntegrationDisabled.
// No other record is used in its place.
func TestSelectedRecordMustBeActive(t *testing.T) {
	for _, c := range selectCases {
		t.Run(c.provider, func(t *testing.T) {
			ctx := context.Background()
			store := forgetest.NewStore()
			store.Teams = []core.Team{{ID: "team_1"}}
			fac := rec(c, "int_fac", "team_1", "fac_1", c.ws)
			fac.Status = core.IntegrationDisabled
			store.Integrations = []core.Integration{fac, rec(c, "int_team", "team_1", "", c.ws)}
			if in, err := SelectTeam(ctx, store, "team_1", c.provider, "fac_1", c.source); !errors.Is(err, core.ErrIntegrationDisabled) || in.ID != "" {
				t.Fatalf("select: %v %v", in.ID, err)
			}
			if in, err := SelectTeam(ctx, store, "team_1", c.provider, "fac_2", c.source); err != nil || in.ID != "int_team" {
				t.Fatalf("other factory: %v %v", in.ID, err)
			}
			store.Integrations[1].Status = core.IntegrationAuthFailed
			// Intake returns the record, so the caller can verify the event
			// with its secret before it ignores the event.
			if in, err := Intake(ctx, store, c.provider, c.source, nil); !errors.Is(err, core.ErrIntegrationDisabled) || in == nil || in.ID != "int_team" {
				t.Fatalf("intake: %v %v", in, err)
			}
		})
	}
}
