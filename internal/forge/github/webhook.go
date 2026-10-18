package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// WebhookPath is where GitHub deliveries are received.
const WebhookPath = "/integrations/github/webhook"

// DefinitionCheckName is the base name of the factory-definition check.
const DefinitionCheckName = "openfactory/definition"

// CheckName returns the definition check name for a definition stored at gitPath
// (the directory is appended for definitions kept in a subdirectory).
func CheckName(gitPath string) string {
	p := strings.Trim(gitPath, "/")
	if p == "" || p == "." {
		return DefinitionCheckName
	}
	return DefinitionCheckName + " (" + p + ")"
}

// DefinitionPR describes a pull request that touches a git-backed factory definition.
type DefinitionPR struct {
	TeamID       string
	FactoryID    string
	Repo         string // owner/name
	Number       int
	HeadSHA      string
	HeadRef      string
	BaseRef      string
	CheckName    string
	ChangedPaths []string // changed definition paths (repository-relative)
}

// WebhookOptions configures the webhook receiver. The webhook secret
// (config.webhook_secret_ref) and the app slug (config.app_slug) come from
// the integration record of each delivery.
type WebhookOptions struct {
	Store   core.Store
	Secrets core.SecretStore
	Bus     core.EventBus
	// Resolver provides authenticated clients for enrichment (PR paths, linked PR
	// labels), alias labels, notices and definition checks. Optional.
	Resolver core.ForgeResolver
	// OnDefinitionPush is invoked for a push to a git-backed factory's production branch.
	OnDefinitionPush func(ctx context.Context, repo, branch, sha string)
	// OnDefinitionPR is invoked for pull requests that change definition files.
	OnDefinitionPR func(ctx context.Context, pr DefinitionPR)
	Logger         *slog.Logger
	Now            func() time.Time
}

// WebhookHandler receives GitHub webhook deliveries.
type WebhookHandler struct {
	opts  WebhookOptions
	log   *slog.Logger
	dedup *forgeutil.Dedupe
	wg    sync.WaitGroup
}

// NewWebhookHandler creates the receiver.
func NewWebhookHandler(opts WebhookOptions) *WebhookHandler {
	return &WebhookHandler{
		opts:  opts,
		log:   forgeutil.Logger(opts.Logger).With("component", "github-webhook"),
		dedup: forgeutil.NewDedupe(time.Hour),
	}
}

// Register mounts POST /integrations/github/webhook.
func (h *WebhookHandler) Register(mux *http.ServeMux) {
	mux.Handle("POST "+WebhookPath, h)
}

// Drain waits for in-flight background processing (used on shutdown and in tests).
func (h *WebhookHandler) Drain() { h.wg.Wait() }

type envelope struct {
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	// Hook is set on the ping of a GitHub App, which has no installation.
	Hook *struct {
		AppID int64 `json:"app_id"`
	} `json:"hook"`
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	eventName := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	sig := r.Header.Get("X-Hub-Signature-256")
	if eventName == "" {
		forgeutil.WriteError(w, http.StatusBadRequest, "missing_event", "X-GitHub-Event header is required")
		return
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "payload is not valid JSON")
		return
	}
	ctx := r.Context()
	integ, err := h.intake(ctx, env, body, sig)
	if errors.Is(err, forgeutil.ErrDeliveryAmbiguous) {
		h.log.Warn("delivery rejected", "event", eventName, "repo", env.Repository.FullName, "err", err)
		forgeutil.WriteError(w, http.StatusConflict, "ambiguous_integration", err.Error())
		return
	}
	if err != nil {
		h.log.Info("delivery not verified", "event", eventName, "repo", env.Repository.FullName, "err", err)
		forgeutil.WriteError(w, http.StatusUnauthorized, "invalid_signature", "X-Hub-Signature-256 does not match")
		return
	}
	if eventName == "ping" {
		forgeutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	if eventName == "installation" {
		n := h.installationLifecycle(ctx, integ, env, body, sig)
		forgeutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "accepted", "integrations": n})
		return
	}
	if integ == nil {
		h.log.Info("delivery for unknown installation ignored", "event", eventName, "repo", env.Repository.FullName)
		forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "ignored", "reason": "no matching integration"})
		return
	}
	if h.dedup.Seen(delivery) {
		forgeutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "duplicate"})
		return
	}
	norm := Normalizer{AppSlug: integ.Config["app_slug"], Now: h.opts.Now}
	evs, err := norm.Normalize(eventName, delivery, body)
	if err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	// Process asynchronously: enrichment calls must not delay the delivery response.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		h.process(bctx, *integ, env.Repository.FullName, eventName, body, evs)
	}()
	forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "events": len(evs)})
}

// intake selects the record of a delivery and verifies its signature with
// the selection rule (forgeutil.IntakeVerified): the owner of the
// repository must be the config.org of the record, and the webhook secret of
// the record (config.webhook_secret_ref) must verify the delivery. A delivery
// with no repository uses the orgs of the records of its installation. The
// ping of a GitHub App has no installation and no repository: it is checked
// with the secrets of the records of that app (config.app_id) and gives no
// record.
func (h *WebhookHandler) intake(ctx context.Context, env envelope, body []byte, sig string) (*core.Integration, error) {
	if sig == "" {
		return nil, fmt.Errorf("no signature: %w", forgeutil.ErrDeliveryUnverified)
	}
	if h.opts.Store == nil {
		return nil, errors.New("no store configured")
	}
	verify := func(in core.Integration) bool { return h.verifyRecord(ctx, in, body, sig) }
	var owners []string
	if owner := forgeutil.RepoOwner(Provider, env.Repository.FullName); owner != "" {
		owners = []string{owner}
	} else if env.Installation != nil && env.Installation.ID != 0 {
		want := strconv.FormatInt(env.Installation.ID, 10)
		recs, err := forgeutil.FindIntegrations(ctx, h.opts.Store, Provider, func(in core.Integration) bool { return in.Config["installation_id"] == want })
		if err != nil {
			return nil, err
		}
		for _, in := range recs {
			if org := strings.TrimSpace(in.Config["org"]); org != "" && !slices.ContainsFunc(owners, func(o string) bool { return strings.EqualFold(o, org) }) {
				owners = append(owners, org)
			}
		}
	}
	if len(owners) > 0 {
		return forgeutil.IntakeVerified(ctx, h.opts.Store, Provider, owners, verify)
	}
	if env.Hook != nil && env.Hook.AppID != 0 {
		app := strconv.FormatInt(env.Hook.AppID, 10)
		recs, _ := forgeutil.FindIntegrations(ctx, h.opts.Store, Provider, func(in core.Integration) bool { return in.Config["app_id"] == app })
		if slices.ContainsFunc(recs, verify) {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("no github integration for installation/repository %q: %w", env.Repository.FullName, forgeutil.ErrDeliveryUnverified)
}

// verifyRecord checks the signature with the webhook secret of one record.
func (h *WebhookHandler) verifyRecord(ctx context.Context, in core.Integration, body []byte, sig string) bool {
	if in.Config["webhook_secret_ref"] == "" {
		return false
	}
	sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, in, "webhook_secret_ref")
	if err != nil {
		h.log.Warn("load webhook secret", "integration", in.ID, "err", err)
		return false
	}
	return forgeutil.VerifyHMACSHA256Hex(sec, body, sig)
}

// forge returns the client of the record that intake selected, for one
// repository of the delivery. The lookup uses the factory of the record, so
// the selection rule finds the same record.
func (h *WebhookHandler) forge(ctx context.Context, integ core.Integration, repo string) core.Forge {
	if h.opts.Resolver == nil || repo == "" {
		return nil
	}
	f, err := h.opts.Resolver.Forge(ctx, core.ForgeLookup{TeamID: integ.TeamID, FactoryID: integ.FactoryID, CodeForge: ForgeName, Repo: repo})
	if err != nil {
		h.log.Warn("resolve github forge", "team", integ.TeamID, "integration", integ.ID, "repo", repo, "err", err)
		return nil
	}
	return f
}

type prFilesLister interface {
	ListPullRequestFiles(ctx context.Context, owner, repo string, number int) ([]string, error)
}

type prDetailer interface {
	GetPullRequestDetails(ctx context.Context, owner, repo string, number int) (*PullRequest, error)
}

type labelAdder interface {
	AddLabels(ctx context.Context, owner, repo string, number int, labels ...string) error
}

// events whose filters accept `paths`.
var pathEvents = map[string]bool{
	"pull_request_opened": true, "pull_request_ready": true, "pull_request_reopened": true,
	"pull_request_closed": true, "pull_request_dequeued": true, "pull_request_enqueued": true,
	"pull_request_labeled": true, "pull_request_merged": true, "pull_request_synchronized": true,
	"pull_request_assigned": true,
}

func (h *WebhookHandler) process(ctx context.Context, integ core.Integration, repo, eventName string, body []byte, evs []*core.Event) {
	teamID := integ.TeamID
	var gf core.Forge
	forgeOnce := func() core.Forge {
		if gf == nil {
			gf = h.forge(ctx, integ, repo)
		}
		return gf
	}
	var prPaths []string
	prPathsLoaded := false
	for _, ev := range evs {
		ev.TeamID = teamID
		forgeutil.LinkActor(ctx, h.opts.Store, &ev.Actor)
		owner, name := forgeutil.SplitRepo(ev.Source.Repo)

		// Enrichment: changed paths for pull request events.
		if pathEvents[ev.Type] && ev.Source.Number != 0 {
			if !prPathsLoaded {
				prPathsLoaded = true
				if l, ok := forgeOnce().(prFilesLister); ok {
					p, err := l.ListPullRequestFiles(ctx, owner, name, ev.Source.Number)
					if err != nil {
						h.log.Warn("list pull request files", "repo", ev.Source.Repo, "pr", ev.Source.Number, "err", err)
					} else {
						prPaths = p
					}
				}
			}
			if prPaths != nil {
				ev.Attributes[eventattr.Paths] = prPaths
			}
		}
		// Enrichment: CI events with exactly one linked PR carry its labels and author.
		if (ev.Type == "check_suite_completed" || ev.Type == "workflow_run_completed") && ev.Attributes[eventattr.LinkedPRCount] == 1 {
			if num, ok := ev.Attributes[eventattr.PRNumber].(int); ok {
				if d, ok := forgeOnce().(prDetailer); ok {
					pr, err := d.GetPullRequestDetails(ctx, owner, name, num)
					if err != nil {
						h.log.Warn("load linked pull request", "repo", ev.Source.Repo, "pr", num, "err", err)
					} else {
						ev.Attributes[eventattr.Labels] = pr.LabelNames()
						ev.Attributes[eventattr.Author] = pr.User.Login
					}
				}
			}
		}
		h.routeFactory(ctx, ev, integ.Config["app_slug"], forgeOnce)
	}

	switch eventName {
	case "push":
		h.definitionPush(ctx, teamID, body)
	case "pull_request":
		h.definitionPR(ctx, teamID, body, forgeOnce)
	}
	forgeutil.Publish(ctx, h.opts.Bus, evs)
}

// routeFactory sets Event.FactoryID from a factory:<alias> label or an @alias
// mention. slug is config.app_slug of the record of the delivery: an
// "@<slug>" mention routes by the factories of the team.
func (h *WebhookHandler) routeFactory(ctx context.Context, ev *core.Event, slug string, forgeOnce func() core.Forge) {
	if h.opts.Store == nil {
		return
	}
	if alias, _ := ev.Attributes[eventattr.Alias].(string); alias != "" {
		if f, err := h.opts.Store.GetFactoryByAlias(ctx, ev.TeamID, alias); err == nil {
			ev.FactoryID = f.ID
		}
		return
	}
	mentioned, _ := ev.Attributes[eventattr.Mentioned].([]string)
	if !strings.HasSuffix(ev.Type, "_mentioned") || len(mentioned) == 0 {
		return
	}
	// Direct @alias mention.
	for _, m := range mentioned {
		if f, err := h.opts.Store.GetFactoryByAlias(ctx, ev.TeamID, m); err == nil {
			ev.FactoryID = f.ID
			ev.Attributes[eventattr.Alias] = f.Alias
			return
		}
	}
	// "@<app>" mention without a routing label: a linked account can still route it.
	if slug == "" || !forgeutil.HasMention(ev.Text, slug) || ev.Actor.IsBot {
		return
	}
	owner, name := forgeutil.SplitRepo(ev.Source.Repo)
	gf := forgeOnce()
	notice := func(msg string) {
		if gf == nil || ev.Source.Number == 0 {
			return
		}
		if err := gf.Comment(ctx, owner, name, ev.Source.Number, msg); err != nil {
			h.log.Warn("post routing notice", "repo", ev.Source.Repo, "err", err)
		}
	}
	if ev.Actor.UserID == "" {
		// Unlinked accounts cannot start work; the engine posts the one-time
		// onboarding notice (automations.githubOnboardingNotices).
		return
	}
	factories, err := h.opts.Store.ListFactories(ctx, ev.TeamID, "")
	if err != nil {
		h.log.Warn("list factories", "team", ev.TeamID, "err", err)
		return
	}
	switch len(factories) {
	case 0:
		notice("No factory is connected to this repository. Connect the repository to a factory or request access, then mention @" + slug + " again.")
	case 1:
		f := factories[0]
		ev.FactoryID = f.ID
		ev.Attributes[eventattr.Alias] = f.Alias
		if la, ok := gf.(labelAdder); ok && f.Alias != "" {
			if err := la.AddLabels(ctx, owner, name, ev.Source.Number, AliasLabelPrefix+f.Alias); err != nil {
				h.log.Warn("add factory label", "repo", ev.Source.Repo, "err", err)
			}
		}
	default:
		names := make([]string, 0, len(factories))
		for _, f := range factories {
			names = append(names, "`"+AliasLabelPrefix+f.Alias+"`")
		}
		notice("Several factories can handle this repository. Add one of these labels and mention @" + slug + " again: " + strings.Join(names, ", "))
	}
}

// NormalizeRepo reduces "https://github.com/acme/x.git", "github.com/acme/x" or "acme/x" to "acme/x".
func NormalizeRepo(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".git")
	for _, p := range []string{"https://", "http://", "ssh://", "git@"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.Replace(s, ":", "/", 1)
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) >= 2 {
		return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1])
	}
	return strings.ToLower(s)
}

func (h *WebhookHandler) gitFactories(ctx context.Context, teamID, repo string) []core.Factory {
	if h.opts.Store == nil {
		return nil
	}
	fs, err := h.opts.Store.ListFactories(ctx, teamID, "")
	if err != nil {
		h.log.Warn("list factories", "team", teamID, "err", err)
		return nil
	}
	var out []core.Factory
	want := NormalizeRepo(repo)
	for _, f := range fs {
		if f.Source == core.DefinitionGit && f.GitRepo != "" && NormalizeRepo(f.GitRepo) == want {
			out = append(out, f)
		}
	}
	return out
}

func productionBranch(f core.Factory) string {
	if f.GitBranch == "" {
		return "main"
	}
	return f.GitBranch
}

func (h *WebhookHandler) definitionPush(ctx context.Context, teamID string, body []byte) {
	if h.opts.OnDefinitionPush == nil {
		return
	}
	var p ghPayload
	if json.Unmarshal(body, &p) != nil || p.Deleted || !strings.HasPrefix(p.Ref, "refs/heads/") {
		return
	}
	branch := strings.TrimPrefix(p.Ref, "refs/heads/")
	for _, f := range h.gitFactories(ctx, teamID, p.Repository.FullName) {
		if productionBranch(f) == branch {
			h.opts.OnDefinitionPush(ctx, p.Repository.FullName, branch, p.After)
			return
		}
	}
}

// definition files and directories at the root of a definition tree.
var definitionRoots = []string{"factory.yaml", "factory.yml", "agents", "automations", "runners", "routers", "benchmarks", "scorers", "webhooks", "skills"}

// IsDefinitionPath reports whether a repository path belongs to a definition stored at gitPath.
func IsDefinitionPath(gitPath, p string) bool {
	dir := strings.Trim(gitPath, "/")
	if dir == "." {
		dir = ""
	}
	if dir != "" {
		if !strings.HasPrefix(p, dir+"/") {
			return false
		}
		p = strings.TrimPrefix(p, dir+"/")
	}
	first := strings.SplitN(p, "/", 2)[0]
	for _, r := range definitionRoots {
		if first == r {
			return true
		}
	}
	return false
}

// factoryConfigChecks reads github.factoryConfigChecks from the active definition ("main" by default).
func (h *WebhookHandler) factoryConfigChecks(ctx context.Context, f core.Factory) string {
	if f.ActiveVersionID == "" {
		return "main"
	}
	v, err := h.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
	if err != nil {
		return "main"
	}
	var doc struct {
		GitHub struct {
			FactoryConfigChecks string `yaml:"factoryConfigChecks"`
		} `yaml:"github"`
	}
	src := v.Files["factory.yaml"]
	if src == "" {
		src = v.Files["factory.yml"]
	}
	if yaml.Unmarshal([]byte(src), &doc) != nil || doc.GitHub.FactoryConfigChecks == "" {
		return "main"
	}
	return doc.GitHub.FactoryConfigChecks
}

func (h *WebhookHandler) definitionPR(ctx context.Context, teamID string, body []byte, forgeOnce func() core.Forge) {
	var p ghPayload
	if json.Unmarshal(body, &p) != nil || p.PR == nil {
		return
	}
	switch p.Action {
	case "opened", "reopened", "synchronize", "ready_for_review":
	case "edited":
		if _, ok := p.Changes["base"]; !ok {
			return
		}
	default:
		return
	}
	repo := p.Repository.FullName
	factories := h.gitFactories(ctx, teamID, repo)
	if len(factories) == 0 {
		return
	}
	owner, name := forgeutil.SplitRepo(repo)
	var files []string
	filesKnown := false
	if l, ok := forgeOnce().(prFilesLister); ok {
		fs, err := l.ListPullRequestFiles(ctx, owner, name, p.PR.Number)
		if err != nil {
			h.log.Warn("list pull request files for definition check", "repo", repo, "err", err)
		} else {
			files, filesKnown = fs, true
		}
	}
	for _, f := range factories {
		if p.PR.Base.Ref != productionBranch(f) && h.factoryConfigChecks(ctx, f) != "all" {
			continue
		}
		check := CheckName(f.GitPath)
		var changed []string
		for _, fp := range files {
			if IsDefinitionPath(f.GitPath, fp) {
				changed = append(changed, fp)
			}
		}
		if filesKnown && len(changed) == 0 {
			// Unchanged definitions pass immediately so a required check never blocks unrelated work.
			if gf := forgeOnce(); gf != nil {
				err := gf.SetCommitStatus(ctx, owner, name, p.PR.Head.SHA, core.CommitStatus{
					Context: check, State: "success", Description: "No factory definition changes",
				})
				if err != nil {
					h.log.Warn("publish definition check", "repo", repo, "err", err)
				}
			}
			continue
		}
		if h.opts.OnDefinitionPR != nil {
			h.opts.OnDefinitionPR(ctx, DefinitionPR{
				TeamID: teamID, FactoryID: f.ID, Repo: repo, Number: p.PR.Number,
				HeadSHA: p.PR.Head.SHA, HeadRef: p.PR.Head.Ref, BaseRef: p.PR.Base.Ref,
				CheckName: check, ChangedPaths: changed,
			})
		}
	}
}

// installationLifecycle sets the status of the integration record of an
// app installation from an installation event: deleted
// and suspend disable the records, unsuspend and created make them active
// again. in is the record whose webhook secret verified the event. Nothing
// changes unless its own installation is the installation of the event. Then
// the records of its team for that installation change, when they have no
// webhook secret of their own or their secret verifies the event too. A team
// never changes the records of another team. It returns the number of records
// that changed.
func (h *WebhookHandler) installationLifecycle(ctx context.Context, in *core.Integration, env envelope, body []byte, sig string) int {
	if h.opts.Store == nil || in == nil || env.Installation == nil || env.Installation.ID == 0 {
		return 0
	}
	if in.Config["installation_id"] != strconv.FormatInt(env.Installation.ID, 10) {
		h.log.Info("installation event for another installation ignored", "integration", in.ID, "installation", env.Installation.ID)
		return 0
	}
	var p struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(body, &p)
	status, reason := "", ""
	switch p.Action {
	case "deleted":
		status, reason = core.IntegrationDisabled, "the app was removed"
	case "suspend":
		status, reason = core.IntegrationDisabled, "the app was suspended"
	case "unsuspend", "created", "new_permissions_accepted":
		status = core.IntegrationActive
	default:
		return 0
	}
	now := time.Now
	if h.opts.Now != nil {
		now = h.opts.Now
	}
	recs, err := forgeutil.TeamIntegrations(ctx, h.opts.Store, in.TeamID, Provider)
	if err != nil {
		h.log.Warn("installation event: list integrations", "team", in.TeamID, "err", err)
		return 0
	}
	n := 0
	for _, r := range recs {
		if r.Config["installation_id"] != in.Config["installation_id"] {
			continue
		}
		if r.ID != in.ID && r.Config["webhook_secret_ref"] != "" && !h.verifyRecord(ctx, r, body, sig) {
			continue
		}
		changed, err := forgeutil.SetIntegrationStatus(ctx, h.opts.Store, r.ID, status, reason, now())
		if err != nil {
			h.log.Warn("installation event: set status", "integration", r.ID, "err", err)
			continue
		}
		if !changed {
			continue
		}
		n++
		h.log.Info("integration status changed", "integration", r.ID, "status", status, "action", p.Action)
	}
	if n > 0 {
		if inv, ok := h.opts.Resolver.(interface{ Invalidate(teamID string) }); ok {
			inv.Invalidate(in.TeamID)
		}
	}
	return n
}
