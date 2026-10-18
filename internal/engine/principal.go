package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Principals of runs. Each run records its
// creator (who caused it, can be nil) and its run_as principal (who it acts
// as, never nil):
//
//   - A person or a service account (API, dashboard, Factory MCP): that
//     principal for both.
//   - A mention or a follow-up from a linked account: that user for both.
//   - An automation: the linked user of the actor (or nil) as creator, and
//     the runAs of the automation (default: the factory) as run_as.
//   - A child run: the principals of its parent.
//   - A scorer, a benchmark or self-improvement: no creator, the factory.

// platformTrigger reports whether a run that triggeredBy names is started
// by the platform itself (a scorer, a benchmark, self-improvement). Such a
// run has no creator and acts as the factory.
func platformTrigger(triggeredBy string) bool {
	switch src, _ := core.RunSourceOf(triggeredBy); src {
	case core.SourceScorer, core.SourceBenchmark, core.SourceSelfImprovement:
		return true
	}
	return false
}

// userPrincipal returns the principal of a user or a service account of the
// team, or nil when the record does not exist or belongs to another team.
func (e *Engine) userPrincipal(ctx context.Context, teamID, id string) *core.Principal {
	if id == "" {
		return nil
	}
	u, err := e.store.GetUser(ctx, id)
	if err != nil || u.TeamID != teamID {
		return nil
	}
	return u.AsPrincipal()
}

// resolveRunAs returns the principal that a runAs value of an automation
// names (core.ParseRunAs). A principal that does not exist is an error: the
// engine never falls back to another principal.
func resolveRunAs(ctx context.Context, st core.Store, f *core.Factory, runAs string) (*core.Principal, error) {
	typ, ref, err := core.ParseRunAs(runAs)
	if err != nil {
		return nil, err
	}
	switch typ {
	case core.PrincipalUser:
		u, err := st.GetUserByEmail(ctx, ref)
		if err != nil || u.TeamID != f.TeamID || u.IsServiceAccount() {
			if err != nil && !errors.Is(err, core.ErrNotFound) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: runAs %q: no user with this email in the team", core.ErrInvalid, runAs)
		}
		return u.AsPrincipal(), nil
	case core.PrincipalServiceAccount:
		users, err := st.ListUsers(ctx, f.TeamID)
		if err != nil {
			return nil, err
		}
		for i := range users {
			if users[i].IsServiceAccount() && users[i].Name == ref {
				return users[i].AsPrincipal(), nil
			}
		}
		return nil, fmt.Errorf("%w: runAs %q: no service account with this name in the team", core.ErrInvalid, runAs)
	}
	return core.FactoryPrincipal(f), nil
}

// dispatchPrincipals returns the creator and the run_as principal of a run
// that req starts in factory f.
func (e *Engine) dispatchPrincipals(ctx context.Context, f *core.Factory, req core.DispatchRequest) (creator, runAs *core.Principal, err error) {
	factory := core.FactoryPrincipal(f)
	if platformTrigger(req.TriggeredBy) {
		return nil, factory, nil
	}
	creatorID := req.CreatorID
	if creatorID == "" && req.Event != nil {
		creatorID = req.Event.Actor.UserID
	}
	creator = e.userPrincipal(ctx, f.TeamID, creatorID)
	if req.AutomationID != "" {
		autos, err := e.store.ListAutomations(ctx, f.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, a := range autos {
			if a.ID == req.AutomationID {
				runAs, err := resolveRunAs(ctx, e.store, f, a.RunAs)
				if err != nil {
					return nil, nil, fmt.Errorf("automation %s: %w", a.Name, err)
				}
				return creator, runAs, nil
			}
		}
		return creator, factory, nil
	}
	if creator != nil {
		return creator, creator, nil
	}
	return nil, factory, nil
}

// syncTrigger reports whether the caller of a dispatch waits for the answer
// (API, dashboard, CLI, Factory MCP). A CREATOR run of such a request is not
// created when the person has no linked account: the caller gets the error.
func syncTrigger(req core.DispatchRequest) bool {
	if req.AutomationID != "" {
		return false
	}
	switch src, _ := core.RunSourceOf(req.TriggeredBy); src {
	case core.SourceAPI, core.SourceMCP, core.SourceDashboard, core.SourceCLI:
		return true
	}
	return false
}

// checkCreatorLink returns an error that wraps core.ErrExternalAuthRequired
// when a CREATOR run acts as a person who has no linked account at a forge
// of the factory. Other errors are left to the session start.
func checkCreatorLink(ctx context.Context, forges core.ForgeResolver, teamID, factoryID string, cfg core.AgentConfig, rt *FactoryRuntime, runAs *core.Principal) error {
	if EffectiveCredentialStrategy(cfg, rt) != CredentialCreator || !runAs.IsPerson() || rt == nil {
		return nil
	}
	ur, ok := forges.(UserForgeResolver)
	if !ok {
		return nil
	}
	for _, ref := range rt.Repositories {
		l := core.ForgeLookup{TeamID: teamID, FactoryID: factoryID, CodeForge: strings.ToUpper(ref.CodeForge), Repo: ref.FullName()}
		if _, _, err := ur.ForgeForUser(ctx, l, runAs.ID); core.IsExternalAuthRequired(err) {
			return err
		}
	}
	return nil
}

// preflightRunAs checks the runAs of each automation:
// it names an existing principal (an error), and a person of an automation
// with the strategy CREATOR has a linked forge account (a warning).
func (e *Engine) preflightRunAs(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	rt := &FactoryRuntime{Repositories: def.Repositories, CredentialStrategy: def.CredentialStrategy}
	for _, a := range def.Automations {
		if a.RunAs == "" || a.RunAs == core.RunAsFactory {
			continue
		}
		p, err := resolveRunAs(ctx, e.store, f, a.RunAs)
		if err != nil {
			if !errors.Is(err, core.ErrInvalid) {
				chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "runAs", Reason: "could not read the principals: " + err.Error()})
				continue
			}
			chk.Errors = append(chk.Errors, core.ValidationError{File: automationFile(a.Name), Field: "runAs",
				Message: fmt.Sprintf("runAs %q names no principal of the team", a.RunAs)})
			continue
		}
		var cfg core.AgentConfig
		for _, ag := range def.Agents {
			if ag.Name == a.Agent {
				cfg = ag
			}
		}
		if err := checkCreatorLink(ctx, e.forges, f.TeamID, f.ID, cfg, rt, p); err != nil {
			chk.Warnings = append(chk.Warnings, core.ValidationError{File: automationFile(a.Name), Field: "runAs",
				Message: fmt.Sprintf("runAs %q has no linked forge account, and the agent uses credentialStrategy CREATOR: its runs fail with external_authentication_required", a.RunAs)})
		}
	}
}
