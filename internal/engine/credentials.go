package engine

import (
	"context"
	"errors"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Credential strategies (credentialStrategy).
const (
	CredentialExecutor = "EXECUTOR"
	CredentialCreator  = "CREATOR"
)

// UserForgeResolver is implemented by forge resolvers that can act with a
// team member's personal token (forge.Resolver). The lookup is the one of
// core.ForgeResolver: the record of the owner of the repository gives the
// API address and the user_tokens setting.
// ForgeForUser uses the record's own credential when the forge or the record
// does not support per-user tokens; the boolean reports which was used. When
// per-user tokens are supported and the user has none, it returns an error
// that wraps core.ErrExternalAuthRequired: the run never falls back.
type UserForgeResolver interface {
	ForgeForUser(ctx context.Context, l core.ForgeLookup, userID string) (core.Forge, bool, error)
}

// EffectiveCredentialStrategy is the run's credential strategy: the agent's
// credentialStrategy, else the factory's, else EXECUTOR.
func EffectiveCredentialStrategy(cfg core.AgentConfig, rt *FactoryRuntime) string {
	s := strings.ToUpper(strings.TrimSpace(cfg.CredentialStrategy))
	if s == "" && rt != nil {
		s = strings.ToUpper(strings.TrimSpace(rt.CredentialStrategy))
	}
	if s != CredentialCreator {
		return CredentialExecutor
	}
	return s
}

// credentialUser returns the Open Factory user whose personal forge
// credentials the run uses: with CREATOR, the run_as
// principal of the run when it is a person. With EXECUTOR, or a run_as
// principal that is a service account or the factory, it returns "" and the
// credential of the factory acts. A run from before Run.RunAs uses the
// creator of its work item.
func (x *Planner) credentialUser(ctx context.Context, r *core.Run, rt *FactoryRuntime) string {
	if EffectiveCredentialStrategy(r.Config, rt) != CredentialCreator {
		return ""
	}
	if r.RunAs != nil {
		if r.RunAs.IsPerson() {
			return r.RunAs.ID
		}
		return ""
	}
	if r.WorkItemID == "" {
		return ""
	}
	wi, err := x.store.GetWorkItem(ctx, r.WorkItemID)
	if err != nil {
		return ""
	}
	return wi.CreatorID
}

// forgeFor resolves the forge client of one repository of a run ("owner/name"):
// the record of the owner of the repository, of the factory or else of the
// team. repos is the scope of the credential: the
// repositories of the run on that forge.
func (x *Planner) forgeFor(ctx context.Context, r *core.Run, codeForge, repo, userID string, repos []string) (core.Forge, error) {
	l := core.ForgeLookup{TeamID: r.TeamID, FactoryID: r.FactoryID, CodeForge: codeForge, Repo: repo, Scope: repos}
	if userID != "" {
		if ur, ok := x.opts.Forges.(UserForgeResolver); ok {
			f, usedUser, err := ur.ForgeForUser(ctx, l, userID)
			if err == nil {
				x.log.Debug("forge credentials", "forge", codeForge, "repo", repo, "user_token", usedUser, "user_id", userID)
			}
			if err != nil || usedUser {
				return f, err
			}
		}
	}
	return x.opts.Forges.Forge(ctx, l)
}

// credentialScope returns the repositories of a run by forge, as
// "owner/name".
func credentialScope(refs []core.RepoRef, start []core.RepoAtCommit) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	add := func(forge, owner, name string) {
		forge = strings.ToUpper(forge)
		full := owner + "/" + name
		if k := forge + "|" + strings.ToLower(full); !seen[k] {
			seen[k] = true
			out[forge] = append(out[forge], full)
		}
	}
	for _, ref := range refs {
		add(ref.CodeForge, ref.Owner, ref.Name)
	}
	for _, ref := range start {
		add(ref.CodeForge, ref.Owner, ref.Repo)
	}
	return out
}

// credentialFailureCode returns the failure code of a credential error, or "".
func credentialFailureCode(err error) core.FailureCode {
	if errors.Is(err, core.ErrIntegrationDisabled) {
		return core.FailureIntegrationDisabled
	}
	if core.IsExternalAuthRequired(err) {
		return core.FailureExternalAuthenticationRequired
	}
	return ""
}

// CredentialPreflight checks the integration records that a run needs
// before the run gets a task. A record that is not active returns a
// *core.FailureCodeError with FailureIntegrationDisabled. A CREATOR run whose run_as person has no
// linked forge account returns
// FailureExternalAuthenticationRequired. Other errors leave the
// decision to the session start.
func (x *Planner) CredentialPreflight(ctx context.Context, r *core.Run) error {
	if x.opts.Forges == nil {
		return nil
	}
	rt, err := LoadFactoryRuntime(ctx, x.store, r.FactoryID)
	if err != nil {
		return nil //nolint:nilerr // other errors leave the decision to the session start (see the doc comment)
	}
	// Each repository can have its own record (the record of its owner).
	for _, ref := range rt.Repositories {
		if _, err := x.forgeFor(ctx, r, strings.ToUpper(ref.CodeForge), ref.FullName(), "", nil); errors.Is(err, core.ErrIntegrationDisabled) {
			return &core.FailureCodeError{Code: core.FailureIntegrationDisabled, Err: err}
		}
	}
	if user := x.credentialUser(ctx, r, rt); user != "" {
		if err := checkCreatorLink(ctx, x.opts.Forges, r.TeamID, r.FactoryID, r.Config, rt, &core.Principal{Type: core.PrincipalUser, ID: user}); err != nil {
			return &core.FailureCodeError{Code: core.FailureExternalAuthenticationRequired, Err: err}
		}
	}
	return nil
}
