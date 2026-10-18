// Package forge resolves authenticated code-forge clients (core.Forge) for a
// team from its integration records, and provides core.Responder
// implementations that post factory replies as issue / pull request comments.
//
// GitHub and generic git are built in (subpackages github and gitgeneric).
// Other forges (GitLab, Azure DevOps, community forges) are served by gRPC
// plugins through Options.Plugins (see internal/plugin).
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/forge/gitgeneric"
	"github.com/openfactory-org/openfactory/internal/forge/github"
)

// Code-forge identifiers (definition `codeForge` values).
const (
	GitHub      = github.ForgeName // GITHUB
	GitLab      = "GITLAB"
	AzureDevOps = "AZURE_DEVOPS"
	Git         = gitgeneric.ForgeName // GIT
)

// ErrNoIntegration is returned when no integration record serves the owner
// of a repository.
var ErrNoIntegration = forgeutil.ErrNoIntegration

// ErrPluginNotInstalled is returned for a forge that only a plugin provides
// when no running plugin serves it.
var ErrPluginNotInstalled = errors.New("forge plugin not installed")

// PluginForges maps integration providers served by first-party plugins to the
// plugin name (binary openfactory-plugin-<name>).
var PluginForges = map[string]string{
	"gitlab":       "gitlab",
	"azure_devops": "azuredevops",
}

// ForgeTarget is what a plugin needs to act for one integration: its
// non-secret config and the resolved credential.
type ForgeTarget struct {
	Provider      string // integration provider, e.g. "gitlab"
	IntegrationID string
	TeamID        string
	Config        map[string]string
	Token         string
	// UserToken is true when Token is a team member's personal token.
	UserToken bool
}

// ForgeProvider builds plugin-backed forges (implemented by internal/plugin.Host).
type ForgeProvider interface {
	// HasForge reports whether a plugin serves forges for the integration provider.
	HasForge(provider string) bool
	ForgeFor(ctx context.Context, t ForgeTarget) (core.Forge, error)
}

// IntegrationProvider maps a code-forge identifier (GITHUB, gitlab, azure-devops...) to the
// core.Integration provider name ("github", "gitlab", "azure_devops", "git").
func IntegrationProvider(codeForge string) string {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(codeForge), "-", "_")) {
	case GitHub, "":
		return "github"
	case GitLab:
		return "gitlab"
	case AzureDevOps:
		return "azure_devops"
	case Git, "OTHER", "GENERIC":
		return "git"
	}
	return strings.ToLower(codeForge)
}

// ForgeFromProvider maps an integration/source provider ("github", "azure_devops") to its forge identifier.
func ForgeFromProvider(provider string) string {
	switch strings.ToLower(strings.ReplaceAll(provider, "-", "_")) {
	case "github":
		return GitHub
	case "gitlab":
		return GitLab
	case "azure_devops", "azuredevops":
		return AzureDevOps
	case "git":
		return Git
	}
	return strings.ToUpper(provider)
}

// Options configures a Resolver.
type Options struct {
	HTTPClient *http.Client
	// CacheTTL bounds how long resolved clients are reused (default 10 minutes).
	CacheTTL time.Duration
	Now      func() time.Time
	// UserTokens holds team members' personal forge tokens, used by
	// ForgeForUser (credentialStrategy CREATOR). Optional.
	UserTokens *UserTokens
	// Plugins serves forges for providers that are not built in (gitlab,
	// azure_devops, ...). Optional; without it those providers fail with
	// ErrPluginNotInstalled.
	Plugins ForgeProvider
}

// Resolver implements core.ForgeResolver. It selects one integration record
// for each forge action by the owner of the repository (see
// forgeutil.SelectIntegration), and caches the clients by the record ID.
//
// Integration records (core.Integration) consulted, by provider:
//
//	all forges:   config org (the owner that the record serves)
//	github:       config app_id, installation_id, base_url, web_url, private_key_ref;
//	              SecretRef = PAT, or the App private key when app_id is set and private_key_ref is empty
//	git:          config base_url, url_template, username; SecretRef = token (optional)
//	other:        served by a plugin (Options.Plugins); the plugin receives the
//	              record's config and the secret named by SecretRef. First-party
//	              plugins: gitlab (config base_url, web_url, token_type) and
//	              azure_devops (config org, base_url).
type Resolver struct {
	store   core.Store
	secrets core.SecretStore
	opts    Options

	mu    sync.Mutex
	cache map[string]cachedForge
}

type cachedForge struct {
	f       core.Forge
	teamID  string
	expires time.Time
}

var _ core.ForgeResolver = (*Resolver)(nil)

// NewResolver creates a resolver.
func NewResolver(store core.Store, secrets core.SecretStore, opts Options) *Resolver {
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 10 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Resolver{store: store, secrets: secrets, opts: opts, cache: map[string]cachedForge{}}
}

// Invalidate drops cached clients for a team (call after integrations change).
func (r *Resolver) Invalidate(teamID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, c := range r.cache {
		if c.teamID == teamID {
			delete(r.cache, k)
		}
	}
}

// Select returns the integration record for a forge action: the selection
// rule of forgeutil.SelectIntegration. It does not check the
// status of the record.
func (r *Resolver) Select(ctx context.Context, l core.ForgeLookup) (core.Integration, error) {
	provider := IntegrationProvider(l.CodeForge)
	in, err := forgeutil.SelectTeamIntegration(ctx, r.store, l.TeamID, provider, l.FactoryID, l.Repo)
	if err != nil {
		return core.Integration{}, fmt.Errorf("forge: team %s, repository %s: %w", l.TeamID, l.Repo, err)
	}
	return in, nil
}

// Forge implements core.ForgeResolver. It selects the record of the owner of
// l.Repo (Select). A record that is not active fails with
// core.ErrIntegrationDisabled. For a GitHub App the client requests each
// installation token with the repositories of l.Scope that have the same
// owner, so the token reaches only those repositories.
func (r *Resolver) Forge(ctx context.Context, l core.ForgeLookup) (core.Forge, error) {
	in, err := r.Select(ctx, l)
	if err != nil {
		return nil, err
	}
	if !in.Active() {
		return nil, disabledError(in)
	}
	f, err := r.client(ctx, in)
	if err != nil {
		return nil, err
	}
	if gc, ok := f.(*github.Client); ok && len(l.Scope) > 0 {
		return gc.Scoped(sameOwner(l.Scope, forgeutil.RepoOwner("github", l.Repo))), nil
	}
	return f, nil
}

// client returns the cached client of one record, by the record ID.
func (r *Resolver) client(ctx context.Context, in core.Integration) (core.Forge, error) {
	now := r.opts.Now()
	r.mu.Lock()
	c, hit := r.cache[in.ID]
	r.mu.Unlock()
	if hit && now.Before(c.expires) {
		return c.f, nil
	}
	f, err := r.build(ctx, in)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[in.ID] = cachedForge{f: f, teamID: in.TeamID, expires: now.Add(r.opts.CacheTTL)}
	r.mu.Unlock()
	return f, nil
}

// sameOwner returns the repositories of repos whose owner is owner.
func sameOwner(repos []string, owner string) []string {
	out := make([]string, 0, len(repos))
	for _, x := range repos {
		if o := forgeutil.RepoOwner("github", x); owner == "" || strings.EqualFold(o, owner) {
			out = append(out, x)
		}
	}
	return out
}

func disabledError(in core.Integration) error {
	msg := fmt.Sprintf("forge: integration %s (%s) is %s", in.ID, in.Provider, in.EffectiveStatus())
	if in.StatusReason != "" {
		msg += ": " + in.StatusReason
	}
	return fmt.Errorf("%s: %w", msg, core.ErrIntegrationDisabled)
}

// authFailed marks an integration record auth_failed after the provider
// refused its credential, and drops the cached clients of the team.
func (r *Resolver) authFailed(in core.Integration, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reason := "the provider refused the credential"
	if code := forgeutil.StatusCode(cause); code != 0 {
		reason = fmt.Sprintf("%s (HTTP %d)", reason, code)
	}
	_, _ = forgeutil.SetIntegrationStatus(ctx, r.store, in.ID, core.IntegrationAuthFailed, reason, r.opts.Now())
	r.Invalidate(in.TeamID)
}

func (r *Resolver) secret(ctx context.Context, in core.Integration, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if r.secrets == nil {
		return "", errors.New("forge: no secret store configured")
	}
	v, err := r.secrets.Get(ctx, in.TeamID, "", name)
	if err != nil {
		return "", fmt.Errorf("forge: integration %s secret %s: %w", in.ID, name, err)
	}
	return string(v), nil
}

// Build creates a forge client for one integration record (no caching).
func (r *Resolver) Build(ctx context.Context, in core.Integration) (core.Forge, error) {
	return r.build(ctx, in)
}

func (r *Resolver) build(ctx context.Context, in core.Integration) (core.Forge, error) {
	cfg := in.Config
	switch strings.ToLower(in.Provider) {
	case "github":
		gc := github.Config{BaseURL: cfg["base_url"], WebURL: cfg["web_url"], HTTPClient: r.opts.HTTPClient,
			Now: r.opts.Now, OnAuthFailure: func(err error) { r.authFailed(in, err) }}
		inst, err := github.ParseInstallationID(cfg["installation_id"])
		if err != nil {
			return nil, fmt.Errorf("forge: github integration %s: invalid installation_id: %w", in.ID, err)
		}
		gc.InstallationID = inst
		switch {
		case cfg["app_id"] != "":
			gc.AppID = cfg["app_id"]
			ref := cfg["private_key_ref"]
			if ref == "" {
				ref = in.SecretRef
			}
			key, err := r.secret(ctx, in, ref)
			if err != nil {
				return nil, err
			}
			gc.PrivateKey = []byte(key)
		case in.SecretRef != "":
			tok, err := r.secret(ctx, in, in.SecretRef)
			if err != nil {
				return nil, err
			}
			gc.Token = tok
		default:
			return nil, fmt.Errorf("forge: github integration %s has no credentials", in.ID)
		}
		return github.New(gc)
	case "git":
		tok, err := r.secret(ctx, in, in.SecretRef)
		if err != nil {
			return nil, err
		}
		return gitgeneric.New(gitgeneric.Config{BaseURL: cfg["base_url"], URLTemplate: cfg["url_template"], Username: cfg["username"], Token: tok})
	}
	t := ForgeTarget{Provider: strings.ToLower(in.Provider), IntegrationID: in.ID, TeamID: in.TeamID, Config: cfg}
	if r.opts.Plugins == nil || !r.opts.Plugins.HasForge(t.Provider) {
		return r.pluginForge(ctx, t) // explains which plugin is missing
	}
	tok, err := r.secret(ctx, in, in.SecretRef)
	if err != nil {
		return nil, err
	}
	t.Token = tok
	return r.pluginForge(ctx, t)
}

// pluginForge builds a forge served by a plugin.
func (r *Resolver) pluginForge(ctx context.Context, t ForgeTarget) (core.Forge, error) {
	if r.opts.Plugins != nil && r.opts.Plugins.HasForge(t.Provider) {
		if t.Config == nil {
			t.Config = map[string]string{}
		}
		return r.opts.Plugins.ForgeFor(ctx, t)
	}
	if name, ok := PluginForges[t.Provider]; ok {
		return nil, fmt.Errorf("forge: %s is provided by the %q plugin; install openfactory-plugin-%s in the plugin directory (OF_PLUGIN_DIR): %w",
			ForgeFromProvider(t.Provider), name, name, ErrPluginNotInstalled)
	}
	return nil, fmt.Errorf("forge: unsupported provider %q (no plugin serves it): %w", t.Provider, ErrPluginNotInstalled)
}

// IssueCommenter is implemented by forges whose issue comments differ from pull request comments.
type IssueCommenter interface {
	CommentIssue(ctx context.Context, owner, repo string, number int, body string) error
}

// WorkItemCommenter is implemented by forges with tracker work items (Azure DevOps).
type WorkItemCommenter interface {
	CommentWorkItem(ctx context.Context, project string, id int, body string) error
}

// WorkItemTracker is implemented by forges with tracker work items (Azure
// DevOps). It is the write-back of the tracker. state
// is core.IssueStateInProgress, core.IssueStateInReview or core.IssueStateDone.
type WorkItemTracker interface {
	SetWorkItemState(ctx context.Context, project string, id int, state string) error
	AttachPullRequestToWorkItem(ctx context.Context, project string, id int, pr core.PullRequestRef) error
}

// Responder posts outbound messages as forge comments on the originating issue,
// pull request or work item.
type Responder struct {
	provider string
	resolver core.ForgeResolver
}

var (
	_ core.Responder    = (*Responder)(nil)
	_ core.IssueTracker = (*Responder)(nil)
)

// NewResponder returns a responder for provider ("github", "gitlab" or "azure_devops").
func NewResponder(resolver core.ForgeResolver, provider string) *Responder {
	return &Responder{provider: strings.ToLower(provider), resolver: resolver}
}

// Responders returns responders for every first-class forge.
func Responders(resolver core.ForgeResolver) []core.Responder {
	return []core.Responder{
		NewResponder(resolver, "github"),
		NewResponder(resolver, "gitlab"),
		NewResponder(resolver, "azure_devops"),
	}
}

// workItemTracker returns the work item tracker of the forge of a tracker
// issue. A source that is not a work item, or a forge without work items,
// gives core.ErrOperationNotSupported.
func (r *Responder) workItemTracker(ctx context.Context, teamID string, issue core.SourceRef) (WorkItemTracker, error) {
	if issue.Kind != "work_item" || issue.Number == 0 || issue.Repo == "" {
		return nil, fmt.Errorf("%s: %s %s is not a tracker issue: %w", r.provider, issue.Kind, issue.Key(), core.ErrOperationNotSupported)
	}
	f, err := r.resolver.Forge(ctx, core.ForgeLookup{TeamID: teamID, CodeForge: ForgeFromProvider(r.provider), Repo: issue.Repo})
	if err != nil {
		return nil, err
	}
	wt, ok := f.(WorkItemTracker)
	if !ok {
		return nil, fmt.Errorf("forge %s has no work items: %w", f.Name(), core.ErrOperationNotSupported)
	}
	return wt, nil
}

// SetIssueState implements core.IssueTracker for the work items of a forge.
// For the other sources it gives core.ErrOperationNotSupported: the forge
// closes a GitHub or GitLab issue from the pull request.
func (r *Responder) SetIssueState(ctx context.Context, teamID string, issue core.SourceRef, state string) error {
	wt, err := r.workItemTracker(ctx, teamID, issue)
	if err != nil {
		return err
	}
	return wt.SetWorkItemState(ctx, issue.Repo, issue.Number, state)
}

// AttachPullRequest implements core.IssueTracker for the work items of a forge.
func (r *Responder) AttachPullRequest(ctx context.Context, teamID string, issue core.SourceRef, pr core.PullRequestRef) error {
	wt, err := r.workItemTracker(ctx, teamID, issue)
	if err != nil {
		return err
	}
	return wt.AttachPullRequestToWorkItem(ctx, issue.Repo, issue.Number, pr)
}

// Provider implements core.Responder.
func (r *Responder) Provider() string { return r.provider }

// Reply implements core.Responder. Sources without a comment thread (pushes, CI runs)
// are reported on the work item instead, so Reply is a no-op for them.
func (r *Responder) Reply(ctx context.Context, teamID string, msg core.OutboundMessage) error {
	src := msg.Source
	if src.Number == 0 || src.Repo == "" {
		return nil
	}
	switch src.Kind {
	case "issue", "pull_request", "work_item":
	default:
		return nil
	}
	f, err := r.resolver.Forge(ctx, core.ForgeLookup{TeamID: teamID, FactoryID: msg.FactoryID, CodeForge: ForgeFromProvider(r.provider), Repo: src.Repo})
	if err != nil {
		return err
	}
	body := forgeutil.FormatOutbound(msg)
	switch src.Kind {
	case "work_item":
		wc, ok := f.(WorkItemCommenter)
		if !ok {
			return fmt.Errorf("forge %s cannot comment on work items", f.Name())
		}
		return wc.CommentWorkItem(ctx, src.Repo, src.Number, body)
	case "issue":
		owner, name := forgeutil.SplitRepo(src.Repo)
		if ic, ok := f.(IssueCommenter); ok {
			return ic.CommentIssue(ctx, owner, name, src.Number, body)
		}
		return f.Comment(ctx, owner, name, src.Number, body)
	default:
		owner, name := forgeutil.SplitRepo(src.Repo)
		// A reply to an action in a review thread goes into that thread.
		// A forge without threads gets a top-level
		// comment.
		thread := msg.ThreadRef
		if thread == "" {
			thread = src.ThreadRef
		}
		if thread != "" {
			err := f.ReplyInThread(ctx, owner, name, src.Number, thread, body)
			if !errors.Is(err, core.ErrOperationNotSupported) {
				return err
			}
		}
		return f.Comment(ctx, owner, name, src.Number, body)
	}
}
