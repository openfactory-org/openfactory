package forgeutil

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ErrNoIntegration is returned when no integration record serves the owner
// of a repository. It matches core.ErrInvalid with errors.Is: the cause is in
// the configuration of the team, and a retry gives the same result.
var ErrNoIntegration error = &configError{msg: "no code-forge integration configured"}

// configError is a sentinel error that matches core.ErrInvalid.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// Is reports whether target is core.ErrInvalid.
func (e *configError) Is(target error) bool { return target == core.ErrInvalid }

// RepoOwner returns the owner of a repository ("owner/name") for the
// selection rule of SelectIntegration. The owner is the value that
// config.org of a record must match:
//
//   - github: the organization or the user (the first path segment).
//   - gitlab: the top-level group (the first path segment). A project in
//     the subgroup "acme/platform" has the owner "acme".
//   - azure_devops: the organization. A repository is "project/name" and
//     does not hold it, so the owner is "" unless the repository is written
//     "org/project/name".
//   - git: "" (a generic git record has no owner).
//
// The case of the letters is kept: SelectIntegration ignores it.
func RepoOwner(provider, repo string) string {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	parts := strings.Split(repo, "/")
	switch strings.ToLower(provider) {
	case "git":
		return ""
	case "azure_devops":
		if len(parts) >= 3 {
			return parts[0]
		}
		return ""
	}
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

// SelectIntegration is the integration selection rule. ints are
// the records of the team for one provider. It takes the records whose
// config.org is owner (the case is ignored), and returns the record of
// factoryID, else the team record (no factory). A record of another factory
// is never used. It returns one record or an error. It never returns "the
// first record".
//
// An empty owner (Azure DevOps "project/name", generic git) matches each
// record. Then the factory, or else the team, must have exactly one record.
//
// The error wraps ErrNoIntegration and names the owner. The caller checks
// the status of the record (core.Integration.Active).
func SelectIntegration(ints []core.Integration, provider, factoryID, owner string) (core.Integration, error) {
	tier := func(fac string) []core.Integration {
		var out []core.Integration
		for _, in := range ints {
			if in.FactoryID != fac {
				continue
			}
			if owner != "" && !strings.EqualFold(strings.TrimSpace(in.Config["org"]), owner) {
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
		if owner == "" {
			return core.Integration{}, fmt.Errorf("%s: %d records (%s) can serve this repository: write the repository with its organization, or keep one record: %w",
				provider, len(t), strings.Join(ids, ", "), ErrNoIntegration)
		}
		return core.Integration{}, fmt.Errorf("%s: %d records (%s) serve the owner %q: keep one: %w",
			provider, len(t), strings.Join(ids, ", "), owner, ErrNoIntegration)
	}
	if owner == "" {
		return core.Integration{}, fmt.Errorf("no %s integration record serves this repository: %w", provider, ErrNoIntegration)
	}
	return core.Integration{}, fmt.Errorf("no %s integration record for the owner %q: add a record with config.org %q: %w", provider, owner, owner, ErrNoIntegration)
}

// SelectTeamIntegration loads the records of the team for provider and
// applies SelectIntegration for one repository.
func SelectTeamIntegration(ctx context.Context, store core.Store, teamID, provider, factoryID, repo string) (core.Integration, error) {
	ints, err := TeamIntegrations(ctx, store, teamID, provider)
	if err != nil {
		return core.Integration{}, fmt.Errorf("list integrations: %w", err)
	}
	return SelectIntegration(ints, provider, factoryID, RepoOwner(provider, repo))
}

// ErrDeliveryUnverified is returned when no integration record verifies the
// signature (or token) of a webhook delivery. The caller answers 401.
var ErrDeliveryUnverified = errors.New("no integration record verifies the delivery")

// ErrDeliveryAmbiguous is returned when the records of two or more teams
// verify one webhook delivery (they share a webhook secret). The delivery is
// rejected: the server does not pick one of the teams.
var ErrDeliveryAmbiguous = errors.New("integration records of several teams verify the delivery")

// IntakeVerified selects the record for a webhook delivery of provider about
// one of owners, from the records of every team. Each
// team gives at most one candidate: SelectIntegration picks its team record,
// else the team uses its record of one factory, when it has exactly one. The
// delivery goes to the candidate whose secret verifies it (verify). Two teams
// may each have a record for one owner: the store order never decides.
//
// No candidate verifies: the error wraps ErrDeliveryUnverified (and
// ErrNoIntegration when no team has a record for the owner). Candidates of
// two or more teams verify: the error wraps ErrDeliveryAmbiguous.
func IntakeVerified(ctx context.Context, store core.Store, provider string, owners []string, verify func(core.Integration) bool) (*core.Integration, error) {
	all, err := FindIntegrations(ctx, store, provider, nil)
	if err != nil {
		return nil, err
	}
	var teams []string
	byTeam := map[string][]core.Integration{}
	for _, in := range all {
		if _, ok := byTeam[in.TeamID]; !ok {
			teams = append(teams, in.TeamID)
		}
		byTeam[in.TeamID] = append(byTeam[in.TeamID], in)
	}
	var cands []core.Integration
	var skipped []error
	seen := map[string]bool{}
	for _, owner := range owners {
		for _, team := range teams {
			in, err := teamIntakeRecord(byTeam[team], provider, owner)
			if err != nil {
				if !errors.Is(err, errNoOwnerRecord) {
					skipped = append(skipped, fmt.Errorf("team %s: %w", team, err))
				}
				continue
			}
			if !seen[in.ID] {
				seen[in.ID] = true
				cands = append(cands, in)
			}
		}
	}
	var ok []core.Integration
	for _, in := range cands {
		if verify(in) {
			ok = append(ok, in)
		}
	}
	switch {
	case len(ok) == 1:
		return &ok[0], nil
	case len(ok) > 1:
		ids := make([]string, 0, len(ok))
		for _, in := range ok {
			ids = append(ids, in.ID+" (team "+in.TeamID+")")
		}
		return nil, fmt.Errorf("%s: the records %s verify the delivery: give each team its own webhook secret: %w",
			provider, strings.Join(ids, ", "), ErrDeliveryAmbiguous)
	case len(cands) == 0 && len(skipped) == 0:
		return nil, fmt.Errorf("no %s integration record for the owner %q: %w: %w", provider, strings.Join(owners, ", "), ErrNoIntegration, ErrDeliveryUnverified)
	}
	err = fmt.Errorf("%s: %d records can serve the owner %q and none verifies the delivery: %w", provider, len(cands), strings.Join(owners, ", "), ErrDeliveryUnverified)
	if len(skipped) > 0 {
		err = errors.Join(append([]error{err}, skipped...)...)
	}
	return nil, err
}

// errNoOwnerRecord marks a team with no record for the owner.
var errNoOwnerRecord = errors.New("no record for the owner")

// teamIntakeRecord is the candidate of one team for a delivery about owner:
// the team record, else the one factory record.
func teamIntakeRecord(ints []core.Integration, provider, owner string) (core.Integration, error) {
	var mine []core.Integration
	for _, x := range ints {
		if owner == "" || strings.EqualFold(strings.TrimSpace(x.Config["org"]), owner) {
			mine = append(mine, x)
		}
	}
	if len(mine) == 0 {
		return core.Integration{}, errNoOwnerRecord
	}
	in, err := SelectIntegration(mine, provider, "", owner)
	if err == nil {
		return in, nil
	}
	var fac []core.Integration
	for _, x := range mine {
		if x.FactoryID != "" {
			fac = append(fac, x)
		}
	}
	if len(fac) == 1 {
		return fac[0], nil
	}
	return core.Integration{}, err
}
