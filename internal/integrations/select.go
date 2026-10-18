package integrations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ErrNoIntegration is returned when no chat or tracker integration record
// serves an action.
var ErrNoIntegration = errors.New("no integration record")

// workspaceLabel is the name of the selection key in error messages.
func workspaceLabel(provider string) string {
	switch {
	case core.SameIntegrationProvider(provider, "slack"):
		return "workspace"
	case core.SameIntegrationProvider(provider, "microsoft_teams"):
		return "Microsoft team"
	case core.SameIntegrationProvider(provider, "linear"):
		return "organization"
	case core.SameIntegrationProvider(provider, "jira"):
		return "site"
	}
	return "workspace"
}

// Select is the integration selection rule for a chat or tracker
// provider. ints are the records of one team for the provider. It takes the
// records whose workspace (core.IntegrationWorkspaceKey) is workspace (the
// case is ignored), and returns the record of factoryID, else the team
// record (no factory). A record of another factory is never used. It never
// returns "the first record".
//
// An empty workspace (a source that does not name it) matches each record.
// Then the factory, or else the team, must have exactly one record.
//
// The error wraps ErrNoIntegration and names the workspace. The caller
// checks the status of the record.
func Select(ints []core.Integration, provider, factoryID, workspace string) (core.Integration, error) {
	workspace = core.NormalizeWorkspace(provider, workspace)
	label := workspaceLabel(provider)
	tier := func(fac string) []core.Integration {
		var out []core.Integration
		for _, in := range ints {
			if in.FactoryID != fac {
				continue
			}
			if workspace != "" && !strings.EqualFold(core.IntegrationWorkspace(in), workspace) {
				continue
			}
			out = append(out, in)
		}
		return out
	}
	tiers := [][]core.Integration{tier("")}
	if factoryID != "" {
		tiers = [][]core.Integration{tier(factoryID), tiers[0]}
	}
	for _, t := range tiers {
		switch len(t) {
		case 0:
			continue
		case 1:
			return t[0], nil
		}
		ids := make([]string, 0, len(t))
		for _, in := range t {
			ids = append(ids, in.ID)
		}
		if workspace == "" {
			return core.Integration{}, fmt.Errorf("%s: %d records (%s) can serve this source and it does not name its %s: keep one record: %w",
				provider, len(t), strings.Join(ids, ", "), label, ErrNoIntegration)
		}
		return core.Integration{}, fmt.Errorf("%s: %d records (%s) serve the %s %q: keep one: %w",
			provider, len(t), strings.Join(ids, ", "), label, workspace, ErrNoIntegration)
	}
	if workspace == "" {
		return core.Integration{}, fmt.Errorf("no %s integration record serves this source: %w", provider, ErrNoIntegration)
	}
	return core.Integration{}, fmt.Errorf("no %s integration record for the %s %q: add a record with config.%s %q: %w",
		provider, label, workspace, core.IntegrationWorkspaceKey(provider), workspace, ErrNoIntegration)
}

// active returns an error that wraps core.ErrIntegrationDisabled when the
// selected record is not active. The caller
// never falls back to another record.
func active(in core.Integration) error {
	if in.Active() {
		return nil
	}
	msg := fmt.Sprintf("%s: integration %s is %s", in.Provider, in.ID, in.EffectiveStatus())
	if in.StatusReason != "" {
		msg += ": " + in.StatusReason
	}
	return fmt.Errorf("%s: %w", msg, core.ErrIntegrationDisabled)
}

// SelectTeam loads the records of the team for provider and applies Select.
// A selected record that is not active gives an error that wraps
// core.ErrIntegrationDisabled.
func SelectTeam(ctx context.Context, store core.Store, teamID, provider, factoryID, workspace string) (core.Integration, error) {
	if store == nil {
		return core.Integration{}, errors.New("no store configured")
	}
	all, err := store.ListIntegrations(ctx, teamID)
	if err != nil {
		return core.Integration{}, fmt.Errorf("list integrations: %w", err)
	}
	var ints []core.Integration
	for _, in := range all {
		if core.SameIntegrationProvider(in.Provider, provider) {
			ints = append(ints, in)
		}
	}
	in, err := Select(ints, provider, factoryID, workspace)
	if err != nil {
		return core.Integration{}, err
	}
	if err := active(in); err != nil {
		return core.Integration{}, err
	}
	return in, nil
}

// Intake selects the record for an incoming event of provider about
// workspace, from the records of every team. match, if not nil, keeps only
// the records that can have received the event (for example the records of
// the app that the event names). The records of exactly one team must be
// left; records of two teams for one workspace give an error. In that team
// the team record wins. A team with no team record for the workspace uses
// its record of one factory, when it has exactly one. The factory of the
// event is the factory of the record.
//
// A selected record that is not active is returned with an error that wraps
// core.ErrIntegrationDisabled: the caller verifies the event with the secret
// of the record, then ignores the event (no publish).
func Intake(ctx context.Context, store core.Store, provider, workspace string, match func(core.Integration) bool) (*core.Integration, error) {
	if store == nil {
		return nil, errors.New("no store configured")
	}
	workspace = core.NormalizeWorkspace(provider, workspace)
	label := workspaceLabel(provider)
	if workspace == "" {
		return nil, fmt.Errorf("the %s event does not name its %s: %w", provider, label, ErrNoIntegration)
	}
	teamList, err := store.ListTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	var all []core.Integration
	for _, t := range teamList {
		ints, err := store.ListIntegrations(ctx, t.ID)
		if err != nil {
			return nil, fmt.Errorf("list integrations for team %s: %w", t.ID, err)
		}
		all = append(all, ints...)
	}
	var cands []core.Integration
	teams := map[string]bool{}
	for _, in := range all {
		if !core.SameIntegrationProvider(in.Provider, provider) || !strings.EqualFold(core.IntegrationWorkspace(in), workspace) || (match != nil && !match(in)) {
			continue
		}
		cands = append(cands, in)
		teams[in.TeamID] = true
	}
	switch {
	case len(cands) == 0:
		return nil, fmt.Errorf("no %s integration record for the %s %q: %w", provider, label, workspace, ErrNoIntegration)
	case len(teams) > 1:
		return nil, fmt.Errorf("records of %d teams serve the %s %s %q: keep the records of one team: %w", len(teams), provider, label, workspace, ErrNoIntegration)
	}
	in, err := Select(cands, provider, "", workspace)
	if err == nil {
		return &in, active(in)
	}
	var fac []core.Integration
	for _, x := range cands {
		if x.FactoryID != "" {
			fac = append(fac, x)
		}
	}
	if len(fac) == 1 {
		return &fac[0], active(fac[0])
	}
	if len(fac) > 1 {
		ids := make([]string, 0, len(fac))
		for _, x := range fac {
			ids = append(ids, x.ID)
		}
		return nil, fmt.Errorf("%s: %d factory records (%s) serve the %s %q and no team record does: add a team record, or keep one: %w",
			provider, len(fac), strings.Join(ids, ", "), label, workspace, ErrNoIntegration)
	}
	return nil, err
}
