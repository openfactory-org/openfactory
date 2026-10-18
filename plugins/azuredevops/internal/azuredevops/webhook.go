package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// Provider is the trigger provider name.
const Provider = eventattr.ProviderAzureDevOps

// WebhookPath is where Azure DevOps service hooks (Web Hooks consumer) deliver.
const WebhookPath = "/plugins/azuredevops/webhook"

// WebhookOptions configures the service-hook receiver.
//
// Integration config keys (provider "azure_devops"):
//   - org: organization name (matched against resourceContainers.account.baseUrl)
//   - hook_username / hook_password_ref: basic-auth credentials configured on the service hook
//   - factory_identities: "identityGUID=alias,..." — factory identities; mentions of and
//     work item assignments to one route to its alias
//   - service_identities: other identity GUIDs whose activity is ignored, comma separated
type WebhookOptions struct {
	Store   core.Store
	Secrets core.SecretStore
	Bus     core.EventBus
	// FactoryRepos returns the repositories of a factory's active definition.
	// It is used to route work item events (which carry a project, not a
	// repository) to the factory covering that project. Optional.
	FactoryRepos func(ctx context.Context, factoryID string) ([]core.RepoRef, error)
	Logger       *slog.Logger
	Now          func() time.Time
}

// WebhookHandler receives Azure DevOps service-hook deliveries.
type WebhookHandler struct {
	opts  WebhookOptions
	log   *slog.Logger
	dedup *forgeutil.Dedupe
}

// NewWebhookHandler creates the receiver.
func NewWebhookHandler(opts WebhookOptions) *WebhookHandler {
	return &WebhookHandler{opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "azure-devops-webhook"), dedup: forgeutil.NewDedupe(time.Hour)}
}

// Register mounts POST /plugins/azuredevops/webhook.
func (h *WebhookHandler) Register(mux *http.ServeMux) { mux.Handle("POST "+WebhookPath, h) }

type identity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
}

type hookPR struct {
	PullRequestID int `json:"pullRequestId"`
	Repository    struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		WebURL  string `json:"webUrl"`
		Project struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	} `json:"repository"`
	Status        string   `json:"status"`
	CreatedBy     identity `json:"createdBy"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	SourceRefName string   `json:"sourceRefName"`
	TargetRefName string   `json:"targetRefName"`
	MergeStatus   string   `json:"mergeStatus"`
	IsDraft       bool     `json:"isDraft"`
	ForkSource    *struct {
		Repository struct {
			ID string `json:"id"`
		} `json:"repository"`
	} `json:"forkSource"`
	LastMergeSourceCommit *struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type hookPayload struct {
	ID                 string          `json:"id"`
	EventType          string          `json:"eventType"`
	Resource           json.RawMessage `json:"resource"`
	ResourceContainers struct {
		Account struct {
			ID      string `json:"id"`
			BaseURL string `json:"baseUrl"`
		} `json:"account"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	} `json:"resourceContainers"`
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "payload is not valid JSON")
		return
	}
	ctx := r.Context()
	org := OrgFromBaseURL(p.ResourceContainers.Account.BaseURL)
	user, pass, ok := r.BasicAuth()
	var integ *core.Integration
	if ok {
		integ, err = h.findIntegration(ctx, org, user, pass)
	} else {
		err = fmt.Errorf("no service hook credentials: %w", forgeutil.ErrDeliveryUnverified)
	}
	if errors.Is(err, forgeutil.ErrDeliveryAmbiguous) {
		h.log.Warn("delivery rejected", "org", org, "err", err)
		forgeutil.WriteError(w, http.StatusConflict, "ambiguous_integration", err.Error())
		return
	}
	if err != nil {
		h.log.Info("delivery not verified", "org", org, "err", err)
		w.Header().Set("WWW-Authenticate", `Basic realm="openfactory"`)
		forgeutil.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid service hook credentials")
		return
	}
	evs, err := h.Normalize(*integ, body)
	if err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	var kept []*core.Event
	for _, ev := range evs {
		if h.dedup.Seen(ev.DeliveryID) {
			continue
		}
		forgeutil.LinkActor(ctx, h.opts.Store, &ev.Actor)
		h.routeFactory(ctx, integ, ev)
		kept = append(kept, ev)
	}
	forgeutil.Publish(ctx, h.opts.Bus, kept)
	forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "events": len(kept)})
}

// routeFactory sets Event.FactoryID, like the other providers do:
//  1. a factory identity alias (mention, or work item assignment) routes to that factory;
//  2. otherwise a work item event goes to the factory of the integration
//     record (core.Integration.FactoryID) when the record has one. Intake
//     picks the record with the integration selection rule;
//  3. otherwise to the only team factory whose repositories are in the work item's
//     project. With several such factories FactoryID stays empty and the engine
//     offers the event to each of them (it matches work items by project).
//
// Pull request and push events carry a repository and are routed by the engine.
func (h *WebhookHandler) routeFactory(ctx context.Context, integ *core.Integration, ev *core.Event) {
	if h.opts.Store == nil {
		return
	}
	if alias, _ := ev.Attributes[eventattr.Alias].(string); alias != "" {
		if f, err := h.opts.Store.GetFactoryByAlias(ctx, ev.TeamID, alias); err == nil {
			ev.FactoryID = f.ID
			return
		}
	}
	if ev.Source.Kind != "work_item" {
		return
	}
	if integ.FactoryID != "" {
		if f, err := h.opts.Store.GetFactory(ctx, integ.FactoryID); err == nil && f.TeamID == ev.TeamID {
			ev.FactoryID = f.ID
			return
		}
	}
	project := ev.Source.Repo
	if project == "" || h.opts.FactoryRepos == nil {
		return
	}
	factories, err := h.opts.Store.ListFactories(ctx, ev.TeamID, "")
	if err != nil {
		h.log.Warn("list factories", "team", ev.TeamID, "err", err)
		return
	}
	var match []core.Factory
	for _, f := range factories {
		repos, err := h.opts.FactoryRepos(ctx, f.ID)
		if err != nil {
			continue
		}
		for _, r := range repos {
			if ProjectCovers(r, project) {
				match = append(match, f)
				break
			}
		}
	}
	if len(match) == 1 {
		ev.FactoryID = match[0].ID
	}
}

// ProjectCovers reports whether a factory repository lives in an Azure DevOps
// project. Azure DevOps repositories are declared as owner=<project> (or
// <org>/<project>) and name=<repository>.
func ProjectCovers(r core.RepoRef, project string) bool {
	if r.CodeForge != "" && !strings.EqualFold(r.CodeForge, "AZURE_DEVOPS") {
		return false
	}
	owner := r.Owner
	if i := strings.LastIndex(owner, "/"); i >= 0 {
		owner = owner[i+1:]
	}
	return project != "" && strings.EqualFold(owner, project)
}

// verify checks the service hook credentials with the password of one record.
func (h *WebhookHandler) verify(ctx context.Context, integ core.Integration, user, pass string) bool {
	if integ.Config["hook_password_ref"] == "" {
		return false
	}
	sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, integ, "hook_password_ref")
	if err != nil {
		h.log.Warn("load service hook password", "integration", integ.ID, "err", err)
		return false
	}
	wantUser := integ.Config["hook_username"]
	return forgeutil.ConstantTimeEqual(string(sec), pass) && (wantUser == "" || forgeutil.ConstantTimeEqual(wantUser, user))
}

// OrgFromBaseURL extracts the organization from https://dev.azure.com/<org>/ or https://<org>.visualstudio.com/.
func OrgFromBaseURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	if strings.HasSuffix(u.Host, ".visualstudio.com") {
		return strings.TrimSuffix(u.Host, ".visualstudio.com")
	}
	return strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)[0]
}

// findIntegration selects the record of a delivery with the selection
// rule (forgeutil.IntakeVerified): config.org of the record is the
// organization of the delivery, and its service hook credentials match.
func (h *WebhookHandler) findIntegration(ctx context.Context, org, user, pass string) (*core.Integration, error) {
	if h.opts.Store == nil {
		return nil, errors.New("no store configured")
	}
	if org == "" {
		return nil, fmt.Errorf("the delivery names no azure devops organization: %w", forgeutil.ErrDeliveryUnverified)
	}
	return forgeutil.IntakeVerified(ctx, h.opts.Store, Provider, []string{org}, func(in core.Integration) bool { return h.verify(ctx, in, user, pass) })
}

var (
	mdMention   = regexp.MustCompile(`@<([0-9a-fA-F-]{36})>`)
	htmlMention = regexp.MustCompile(`data-vss-mention="version:[0-9.]+,([0-9a-fA-F-]{36})"`)
)

// ExtractMentions returns identity GUIDs mentioned in markdown ("@<GUID>") or work item HTML.
func ExtractMentions(text string) []string {
	var out []string
	for _, m := range mdMention.FindAllStringSubmatch(forgeutil.StripCode(text), -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	for _, m := range htmlMention.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	return forgeutil.StringSet(out...)
}

func parsePairs(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(pair), "=")
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// identityField decodes System.AssignedTo / System.CreatedBy (object or "Name <email>" string).
func identityField(raw json.RawMessage) identity {
	var id identity
	if len(raw) == 0 || string(raw) == "null" {
		return id
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		if i := strings.LastIndex(s, "<"); i >= 0 && strings.HasSuffix(s, ">") {
			return identity{DisplayName: strings.TrimSpace(s[:i]), UniqueName: s[i+1 : len(s)-1]}
		}
		return identity{UniqueName: s, DisplayName: s}
	}
	_ = json.Unmarshal(raw, &id)
	return id
}

func stringField(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func splitTags(s string) []string {
	out := []string{}
	for _, t := range strings.Split(s, ";") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

var htmlTag = regexp.MustCompile(`<[^>]+>`)

func stripHTML(s string) string {
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "</p>", "\n", "</div>", "\n").Replace(s)
	s = htmlTag.ReplaceAllString(s, "")
	return strings.TrimSpace(strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s))
}

// Normalize maps a service-hook payload to events.
func (h *WebhookHandler) Normalize(in core.Integration, body []byte) ([]*core.Event, error) {
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("azuredevops: decode payload: %w", err)
	}
	now := time.Now
	if h.opts.Now != nil {
		now = h.opts.Now
	}
	factories := parsePairs(in.Config["factory_identities"])
	service := map[string]bool{}
	for _, s := range strings.Split(in.Config["service_identities"], ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			service[s] = true
		}
	}
	isSelf := func(id string) bool {
		id = strings.ToLower(id)
		_, f := factories[id]
		return id != "" && (f || service[id])
	}
	var out []*core.Event
	emit := func(typ string, actor identity, src core.SourceRef, text string, attrs map[string]any, dedupeKey string) {
		self := isSelf(actor.ID)
		a := map[string]any{eventattr.ActorIsBot: self, eventattr.ActorIsSelf: self}
		for k, v := range attrs {
			a[k] = v
		}
		if text != "" {
			a[eventattr.Text] = text
		}
		did, akey := p.ID+":"+typ, ""
		if p.ID != "" {
			// The events of one delivery share the action key.
			akey = Provider + ":" + p.ID
		}
		if dedupeKey != "" {
			// The key names the fact (a merge), which several deliveries can
			// report, so it is also the action key.
			did, akey = dedupeKey, Provider+":"+dedupeKey
		}
		out = append(out, &core.Event{
			ID: core.NewID("evt"), TeamID: in.TeamID, Provider: Provider, Type: typ, DeliveryID: did, ActionKey: akey,
			Actor:      core.Actor{Provider: Provider, ExternalID: strings.ToLower(actor.ID), Handle: actor.UniqueName, IsBot: self},
			Source:     src,
			Attributes: a,
			Payload:    json.RawMessage(body),
			Text:       text,
			ReceivedAt: now().UTC(),
		})
	}
	mentionAttrs := func(base map[string]any, mentioned []string) map[string]any {
		a := map[string]any{}
		for k, v := range base {
			a[k] = v
		}
		a[eventattr.Mentioned] = mentioned
		for _, m := range mentioned {
			if alias := factories[m]; alias != "" {
				a[eventattr.Alias] = alias
				break
			}
		}
		return a
	}

	prSource := func(pr hookPR) (core.SourceRef, map[string]any) {
		u := ""
		if pr.Repository.WebURL != "" {
			u = fmt.Sprintf("%s/pullrequest/%d", pr.Repository.WebURL, pr.PullRequestID)
		}
		src := core.SourceRef{
			Provider: Provider, Kind: "pull_request", URL: u,
			Repo: pr.Repository.Project.Name + "/" + pr.Repository.Name, Number: pr.PullRequestID, Title: pr.Title,
		}
		sourceRepo := pr.Repository.ID
		if pr.ForkSource != nil && pr.ForkSource.Repository.ID != "" {
			sourceRepo = pr.ForkSource.Repository.ID
		}
		labels := []string{}
		for _, l := range pr.Labels {
			labels = append(labels, l.Name)
		}
		a := map[string]any{
			eventattr.Repo:       strings.ToLower(pr.Repository.ID),
			eventattr.SourceRepo: strings.ToLower(sourceRepo),
			eventattr.BaseBranch: strings.TrimPrefix(pr.TargetRefName, "refs/heads/"),
			eventattr.HeadBranch: strings.TrimPrefix(pr.SourceRefName, "refs/heads/"),
			eventattr.PRNumber:   pr.PullRequestID,
			eventattr.Author:     pr.CreatedBy.UniqueName,
			eventattr.Draft:      pr.IsDraft,
			eventattr.Labels:     labels,
		}
		if pr.LastMergeSourceCommit != nil {
			a[eventattr.HeadSHA] = pr.LastMergeSourceCommit.CommitID
		}
		return src, a
	}

	switch p.EventType {
	case "git.pullrequest.created", "git.pullrequest.updated", "git.pullrequest.merged":
		var pr hookPR
		if err := json.Unmarshal(p.Resource, &pr); err != nil {
			return nil, fmt.Errorf("azuredevops: decode pull request: %w", err)
		}
		src, a := prSource(pr)
		text := pr.Title
		if pr.Description != "" {
			text += "\n\n" + pr.Description
		}
		mergedKey := fmt.Sprintf("%s/%d:merged", strings.ToLower(pr.Repository.ID), pr.PullRequestID)
		switch {
		case p.EventType == "git.pullrequest.created":
			emit("pull_request_created", pr.CreatedBy, src, text, a, "")
			if m := ExtractMentions(pr.Description); len(m) > 0 {
				emit("pull_request_mentioned", pr.CreatedBy, src, text, mentionAttrs(a, m), "")
			}
		case pr.Status == "completed":
			emit("pull_request_merged", pr.CreatedBy, src, text, a, mergedKey)
		case p.EventType == "git.pullrequest.merged":
			// merge attempted but the pull request is not completed yet
		case pr.Status == "abandoned":
			emit("pull_request_closed", pr.CreatedBy, src, text, a, "")
		default:
			emit("pull_request_updated", pr.CreatedBy, src, text, a, "")
		}
	case "ms.vss-code.git-pullrequest-comment-event":
		var res struct {
			Comment struct {
				ID      int      `json:"id"`
				Content string   `json:"content"`
				Author  identity `json:"author"`
				Links   struct {
					Threads struct {
						Href string `json:"href"`
					} `json:"threads"`
				} `json:"_links"`
			} `json:"comment"`
			PullRequest hookPR `json:"pullRequest"`
		}
		if err := json.Unmarshal(p.Resource, &res); err != nil {
			return nil, fmt.Errorf("azuredevops: decode comment: %w", err)
		}
		src, a := prSource(res.PullRequest)
		// A reply goes into the comment thread.
		if href := strings.TrimRight(res.Comment.Links.Threads.Href, "/"); href != "" {
			if i := strings.LastIndex(href, "/"); i >= 0 {
				if _, err := strconv.Atoi(href[i+1:]); err == nil {
					src.ThreadRef = href[i+1:]
					a[eventattr.ThreadRef] = src.ThreadRef
				}
			}
		}
		emit("pull_request_commented", res.Comment.Author, src, res.Comment.Content, a, "")
		if m := ExtractMentions(res.Comment.Content); len(m) > 0 && !isSelf(res.Comment.Author.ID) {
			emit("pull_request_mentioned", res.Comment.Author, src, res.Comment.Content, mentionAttrs(a, m), "")
		}
	case "workitem.created", "workitem.updated", "workitem.commented":
		var res struct {
			ID         int                        `json:"id"`
			WorkItemID int                        `json:"workItemId"`
			Fields     map[string]json.RawMessage `json:"fields"`
			RevisedBy  identity                   `json:"revisedBy"`
			Revision   *struct {
				ID     int                        `json:"id"`
				Fields map[string]json.RawMessage `json:"fields"`
			} `json:"revision"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"_links"`
		}
		if err := json.Unmarshal(p.Resource, &res); err != nil {
			return nil, fmt.Errorf("azuredevops: decode work item: %w", err)
		}
		id := res.ID
		fields := res.Fields
		var changes map[string]json.RawMessage
		if p.EventType == "workitem.updated" {
			id = res.WorkItemID
			changes = res.Fields
			fields = map[string]json.RawMessage{}
			if res.Revision != nil {
				fields = res.Revision.Fields
			}
		}
		project := stringField(fields["System.TeamProject"])
		title := stringField(fields["System.Title"])
		src := core.SourceRef{Provider: Provider, Kind: "work_item", URL: res.Links.HTML.Href, Repo: project, Number: id, ExternalID: strconv.Itoa(id), Title: title}
		assignee := identityField(fields["System.AssignedTo"])
		creator := identityField(fields["System.CreatedBy"])
		tags := splitTags(stringField(fields["System.Tags"]))
		a := map[string]any{
			eventattr.WorkItemType: stringField(fields["System.WorkItemType"]),
			eventattr.Labels:       tags,
			eventattr.Assignees:    forgeutil.StringSet(assignee.UniqueName),
			eventattr.Author:       creator.UniqueName,
		}
		desc := stringField(fields["System.Description"])
		text := strings.TrimSpace(title + "\n\n" + stripHTML(desc))
		switch p.EventType {
		case "workitem.created":
			ca := copyMap(a)
			ca[eventattr.AddedLabels] = tags
			emit("work_item_created", creator, src, text, ca, "")
			if m := ExtractMentions(desc); len(m) > 0 {
				emit("work_item_mentioned", creator, src, text, mentionAttrs(map[string]any{}, m), "")
			}
		case "workitem.updated":
			actor := res.RevisedBy
			if ch, ok := changes["System.AssignedTo"]; ok {
				var v struct {
					NewValue json.RawMessage `json:"newValue"`
				}
				_ = json.Unmarshal(ch, &v)
				if nv := identityField(v.NewValue); nv.UniqueName != "" {
					aa := copyMap(a)
					aa[eventattr.Assignees] = []string{nv.UniqueName}
					// Assigning a factory identity routes the work item to its factory.
					if alias := factories[strings.ToLower(nv.ID)]; alias != "" {
						aa[eventattr.Alias] = alias
					}
					emit("work_item_assigned", actor, src, text, aa, "")
				}
			}
			if ch, ok := changes["System.Tags"]; ok {
				var v struct {
					OldValue string `json:"oldValue"`
					NewValue string `json:"newValue"`
				}
				_ = json.Unmarshal(ch, &v)
				if added := forgeutil.Diff(splitTags(v.NewValue), splitTags(v.OldValue)); len(added) > 0 {
					la := copyMap(a)
					la[eventattr.AddedLabels] = added
					la[eventattr.Labels] = splitTags(v.NewValue)
					emit("work_item_labeled", actor, src, text, la, "")
				}
			}
			if ch, ok := changes["System.History"]; ok {
				var v struct {
					NewValue string `json:"newValue"`
				}
				_ = json.Unmarshal(ch, &v)
				h.workItemComment(emit, mentionAttrs, isSelf, actor, src, a, v.NewValue)
			}
		case "workitem.commented":
			h.workItemComment(emit, mentionAttrs, isSelf, res.RevisedBy, src, a, stringField(fields["System.History"]))
		}
	case "git.push":
		var res struct {
			PushedBy   identity `json:"pushedBy"`
			Repository struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Project struct {
					Name string `json:"name"`
				} `json:"project"`
			} `json:"repository"`
			RefUpdates []struct {
				Name        string `json:"name"`
				NewObjectID string `json:"newObjectId"`
			} `json:"refUpdates"`
		}
		if err := json.Unmarshal(p.Resource, &res); err != nil {
			return nil, fmt.Errorf("azuredevops: decode push: %w", err)
		}
		for _, ru := range res.RefUpdates {
			if !strings.HasPrefix(ru.Name, "refs/heads/") || strings.Trim(ru.NewObjectID, "0") == "" {
				continue
			}
			branch := strings.TrimPrefix(ru.Name, "refs/heads/")
			src := core.SourceRef{Provider: Provider, Kind: "push", Repo: res.Repository.Project.Name + "/" + res.Repository.Name, ExternalID: res.Repository.ID + "@" + ru.NewObjectID, Title: branch}
			emit("push", res.PushedBy, src, "", map[string]any{
				eventattr.Repo: strings.ToLower(res.Repository.ID), eventattr.Branch: branch, eventattr.HeadSHA: ru.NewObjectID,
			}, p.ID+":push:"+branch)
		}
	}
	return out, nil
}

func (h *WebhookHandler) workItemComment(
	emit func(string, identity, core.SourceRef, string, map[string]any, string),
	mentionAttrs func(map[string]any, []string) map[string]any,
	isSelf func(string) bool,
	actor identity, src core.SourceRef, base map[string]any, html string,
) {
	if html == "" {
		return
	}
	text := stripHTML(html)
	emit("work_item_commented", actor, src, text, base, "")
	if m := ExtractMentions(html); len(m) > 0 && !isSelf(actor.ID) {
		emit("work_item_mentioned", actor, src, text, mentionAttrs(map[string]any{}, m), "")
	}
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
