package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Onboarding notice texts (automations.githubOnboardingNotices).
const (
	onboardingUnlinked  = "Hi @%s, this repository is connected to an Open Factory factory, but your GitHub account is not linked to an Open Factory account, so this request did not start any work.\n\n%s\n\nAfter linking, mention the factory or apply the label again."
	onboardingNonMember = "Hi @%s, this repository is connected to an Open Factory factory, but your linked Open Factory account is not an active member of the factory's team, so this request did not start any work.\n\nAsk a team admin to add you to the team, then mention the factory or apply the label again."
)

// githubActorEligible gates new GitHub work: an event from a human GitHub
// account starts work only when that account is linked to an Open Factory
// user who is a member of the factory's team. Bot and system events (no
// external actor, e.g. CI) are not gated. When the actor is not eligible and
// any candidate factory has automations.githubOnboardingNotices enabled (the
// default), a one-time onboarding comment explains how to link the account.
func (e *Engine) githubActorEligible(ctx context.Context, ev *core.Event, factories []core.Factory) bool {
	if normalizeProvider(ev.Provider) != "github" || ev.Actor.IsBot || ev.Actor.ExternalID == "" || len(factories) == 0 {
		return true
	}
	reason := ""
	if ev.Actor.UserID == "" {
		reason = "unlinked"
	} else {
		u, err := e.store.GetUser(ctx, ev.Actor.UserID)
		switch {
		case errors.Is(err, core.ErrNotFound):
			reason = "unlinked"
		case err != nil:
			e.log.Warn("look up linked user", "user_id", ev.Actor.UserID, "err", err)
			return true // fail open on store errors rather than drop work
		case u.TeamID != ev.TeamID && !factoryInTeam(factories, u.TeamID):
			reason = "non_member"
		}
	}
	if reason == "" {
		return true
	}
	e.log.Info("github event from an ineligible account ignored", "reason", reason, "actor", ev.Actor.Handle, "type", ev.Type)
	notify := false
	for i := range factories {
		rt, err := LoadFactoryRuntime(ctx, e.store, factories[i].ID)
		if err == nil && (rt.GithubOnboardingNotices == nil || *rt.GithubOnboardingNotices) {
			notify = true
			break
		}
	}
	if notify {
		e.postOnboardingNotice(ctx, ev, reason)
	}
	return false
}

func factoryInTeam(fs []core.Factory, teamID string) bool {
	for _, f := range fs {
		if f.TeamID == teamID {
			return true
		}
	}
	return false
}

// postOnboardingNotice comments once per (team, GitHub account, reason).
func (e *Engine) postOnboardingNotice(ctx context.Context, ev *core.Event, reason string) {
	if e.forges == nil || ev.Source.Number == 0 || ev.Source.Repo == "" {
		return
	}
	key := "engine:github-onboarding:" + ev.TeamID + ":" + strings.ToLower(ev.Actor.ExternalID) + ":" + reason
	defer e.lock(key)()
	if v, err := e.store.KVGet(ctx, key); err == nil && v != "" {
		return
	}
	handle := ev.Actor.Handle
	if handle == "" {
		handle = ev.Actor.ExternalID
	}
	var body string
	if reason == "unlinked" {
		// The comment is public, so it has no link URL: anyone who can read
		// it could open the URL. The person starts the
		// link from the dashboard with the GitHub sign-in.
		body = fmt.Sprintf(onboardingUnlinked, handle, onboardingHow(e.publicURL))
	} else {
		body = fmt.Sprintf(onboardingNonMember, handle)
	}
	f, err := e.forges.Forge(ctx, core.ForgeLookup{TeamID: ev.TeamID, FactoryID: ev.FactoryID, CodeForge: "GITHUB", Repo: ev.Source.Repo})
	if err != nil {
		e.log.Warn("onboarding notice: resolve forge", "err", err)
		return
	}
	owner, name, _ := strings.Cut(ev.Source.Repo, "/")
	if err := f.Comment(ctx, owner, name, ev.Source.Number, body); err != nil {
		e.log.Warn("onboarding notice: comment", "repo", ev.Source.Repo, "err", err)
		return
	}
	_ = e.store.KVSet(ctx, key, e.now().UTC().Format("2006-01-02T15:04:05Z07:00"))
}

// onboardingHow tells an unlinked person how to link the GitHub account.
func onboardingHow(publicURL string) string {
	where := "the Open Factory dashboard"
	if publicURL != "" {
		where = strings.TrimRight(publicURL, "/") + "/integrations"
	}
	return "Sign in to " + where + " and link your GitHub account under \"Link your account\", then try again."
}
