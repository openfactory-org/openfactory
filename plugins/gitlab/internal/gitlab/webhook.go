package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// Provider is the trigger provider name.
const Provider = eventattr.ProviderGitLab

// WebhookPath is where GitLab (group or project) webhooks deliver.
const WebhookPath = "/plugins/gitlab/webhook"

// WebhookOptions configures the receiver.
//
// Integration config keys (provider "gitlab"):
//   - group: top-level group path the integration covers (deliveries are matched by project path prefix)
//   - bot_username: the team-wide bot account (mentions of it emit bot_mentioned)
//   - factory_bots: per-factory bot accounts, "username=alias,username2=alias2"
//   - service_accounts: other service accounts whose activity is ignored, comma separated
//   - webhook_secret_ref: name of the secret holding the X-Gitlab-Token value
type WebhookOptions struct {
	Store   core.Store
	Secrets core.SecretStore
	Bus     core.EventBus
	Logger  *slog.Logger
	Now     func() time.Time
}

// WebhookHandler receives GitLab webhook deliveries.
type WebhookHandler struct {
	opts  WebhookOptions
	log   *slog.Logger
	dedup *forgeutil.Dedupe
}

// NewWebhookHandler creates the receiver.
func NewWebhookHandler(opts WebhookOptions) *WebhookHandler {
	return &WebhookHandler{opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "gitlab-webhook"), dedup: forgeutil.NewDedupe(time.Hour)}
}

// Register mounts POST /plugins/gitlab/webhook.
func (h *WebhookHandler) Register(mux *http.ServeMux) { mux.Handle("POST "+WebhookPath, h) }

type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

type glProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
}

type glMR struct {
	IID          int    `json:"iid"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	URL          string `json:"url"`
	Action       string `json:"action"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Draft        bool   `json:"draft"`
	LastCommit   struct {
		ID string `json:"id"`
	} `json:"last_commit"`
}

type glPayload struct {
	ObjectKind       string          `json:"object_kind"`
	User             glUser          `json:"user"`
	Project          glProject       `json:"project"`
	ObjectAttributes json.RawMessage `json:"object_attributes"`
	Labels           []struct {
		Title string `json:"title"`
	} `json:"labels"`
	MergeRequest *glMR `json:"merge_request"`
	Issue        *struct {
		IID         int    `json:"iid"`
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
	} `json:"issue"`
	// push
	Ref          string `json:"ref"`
	After        string `json:"after"`
	UserID       int64  `json:"user_id"`
	UserUsername string `json:"user_username"`
	Commits      []struct {
		Message  string   `json:"message"`
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
}

type botConfig struct {
	teamBot     string
	factoryBots map[string]string // username (lower) -> alias
	service     map[string]bool
}

func parseBots(in core.Integration) botConfig {
	bc := botConfig{teamBot: strings.ToLower(strings.TrimPrefix(in.Config["bot_username"], "@")), factoryBots: map[string]string{}, service: map[string]bool{}}
	for _, pair := range strings.Split(in.Config["factory_bots"], ",") {
		u, alias, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && u != "" {
			bc.factoryBots[strings.ToLower(strings.TrimPrefix(u, "@"))] = strings.TrimSpace(alias)
		}
	}
	for _, s := range strings.Split(in.Config["service_accounts"], ",") {
		if s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "@"))); s != "" {
			bc.service[s] = true
		}
	}
	return bc
}

func (b botConfig) isSelf(username string) bool {
	u := strings.ToLower(username)
	if u == "" {
		return false
	}
	_, fb := b.factoryBots[u]
	return fb || b.service[u] || (b.teamBot != "" && u == b.teamBot)
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	var p glPayload
	if err := json.Unmarshal(body, &p); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "payload is not valid JSON")
		return
	}
	ctx := r.Context()
	integ, err := h.findIntegration(ctx, p.Project.PathWithNamespace, r.Header.Get("X-Gitlab-Token"))
	if errors.Is(err, forgeutil.ErrDeliveryAmbiguous) {
		h.log.Warn("delivery rejected", "project", p.Project.PathWithNamespace, "err", err)
		forgeutil.WriteError(w, http.StatusConflict, "ambiguous_integration", err.Error())
		return
	}
	if err != nil {
		h.log.Info("delivery not verified", "project", p.Project.PathWithNamespace, "err", err)
		forgeutil.WriteError(w, http.StatusUnauthorized, "invalid_token", "X-Gitlab-Token does not match")
		return
	}
	delivery := r.Header.Get("X-Gitlab-Event-UUID")
	if delivery == "" {
		delivery = r.Header.Get("X-Gitlab-Webhook-UUID")
	}
	if h.dedup.Seen(delivery) {
		forgeutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "duplicate"})
		return
	}
	evs, err := h.Normalize(*integ, r.Header.Get("X-Gitlab-Event"), delivery, body)
	if err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	for _, ev := range evs {
		forgeutil.LinkActor(ctx, h.opts.Store, &ev.Actor)
		if alias, _ := ev.Attributes[eventattr.Alias].(string); alias != "" && h.opts.Store != nil {
			if f, err := h.opts.Store.GetFactoryByAlias(ctx, ev.TeamID, alias); err == nil {
				ev.FactoryID = f.ID
			}
		}
	}
	forgeutil.Publish(ctx, h.opts.Bus, evs)
	forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "events": len(evs)})
}

// verify checks the token of a delivery with the webhook secret of one record.
func (h *WebhookHandler) verify(ctx context.Context, integ core.Integration, token string) bool {
	if token == "" || integ.Config["webhook_secret_ref"] == "" {
		return false
	}
	sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, integ, "webhook_secret_ref")
	if err != nil {
		h.log.Warn("load webhook secret", "integration", integ.ID, "err", err)
		return false
	}
	return forgeutil.ConstantTimeEqual(string(sec), token)
}

// findIntegration selects the record of a delivery with the selection
// rule (forgeutil.IntakeVerified): config.org of the record is the
// top-level group of the project, and its webhook secret matches the token.
// A project in a subgroup ("acme/platform/api") uses the record of "acme".
func (h *WebhookHandler) findIntegration(ctx context.Context, projectPath, token string) (*core.Integration, error) {
	if h.opts.Store == nil {
		return nil, errors.New("no store configured")
	}
	owner := forgeutil.RepoOwner(Provider, projectPath)
	if owner == "" {
		return nil, fmt.Errorf("no gitlab integration for project %q: %w", projectPath, forgeutil.ErrDeliveryUnverified)
	}
	return forgeutil.IntakeVerified(ctx, h.opts.Store, Provider, []string{owner}, func(in core.Integration) bool { return h.verify(ctx, in, token) })
}

// Normalize maps a delivery to events for the integration's team.
func (h *WebhookHandler) Normalize(in core.Integration, eventHeader, deliveryID string, body []byte) ([]*core.Event, error) {
	var p glPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("gitlab: decode payload: %w", err)
	}
	now := time.Now
	if h.opts.Now != nil {
		now = h.opts.Now
	}
	bots := parseBots(in)
	repo := p.Project.PathWithNamespace
	user := p.User
	if user.Username == "" && p.UserUsername != "" {
		user = glUser{ID: p.UserID, Username: p.UserUsername}
	}
	self := bots.isSelf(user.Username)
	var out []*core.Event
	emit := func(typ string, src core.SourceRef, text string, attrs map[string]any) {
		a := map[string]any{
			eventattr.Repo:        repo,
			eventattr.ActorIsBot:  self,
			eventattr.ActorIsSelf: self,
		}
		for k, v := range attrs {
			a[k] = v
		}
		if text != "" {
			a[eventattr.Text] = text
		}
		did, akey := deliveryID, ""
		if did != "" {
			// One delivery can give several events: they share the action
			// key, and the engine routes them one time.
			did += ":" + typ
			akey = Provider + ":" + deliveryID
		}
		out = append(out, &core.Event{
			ID: core.NewID("evt"), TeamID: in.TeamID, Provider: Provider, Type: typ, DeliveryID: did, ActionKey: akey,
			Actor:      core.Actor{Provider: Provider, ExternalID: fmt.Sprint(user.ID), Handle: user.Username, IsBot: self},
			Source:     src,
			Attributes: a,
			Payload:    json.RawMessage(body),
			Text:       text,
			ReceivedAt: now().UTC(),
		})
	}
	labels := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		labels = append(labels, l.Title)
	}

	kind := p.ObjectKind
	if kind == "" {
		kind = strings.ToLower(strings.TrimSuffix(strings.ReplaceAll(eventHeader, " ", "_"), "_Hook"))
	}
	switch kind {
	case "merge_request":
		var mr glMR
		if err := json.Unmarshal(p.ObjectAttributes, &mr); err != nil {
			return nil, fmt.Errorf("gitlab: decode merge request: %w", err)
		}
		src := core.SourceRef{Provider: Provider, Kind: "pull_request", URL: mr.URL, Repo: repo, Number: mr.IID, Title: mr.Title}
		emit("merge_request", src, joinText(mr.Title, mr.Description), map[string]any{
			eventattr.Action:     mr.Action,
			eventattr.BaseBranch: mr.TargetBranch,
			eventattr.HeadBranch: mr.SourceBranch,
			eventattr.HeadSHA:    mr.LastCommit.ID,
			eventattr.PRNumber:   mr.IID,
			eventattr.Labels:     labels,
			eventattr.Draft:      mr.Draft,
		})
	case "note":
		var note struct {
			ID           int64  `json:"id"`
			Note         string `json:"note"`
			NoteableType string `json:"noteable_type"`
			URL          string `json:"url"`
			DiscussionID string `json:"discussion_id"`
			Action       string `json:"action"`
			Type         string `json:"type"` // DiffNote, DiscussionNote or empty
		}
		if err := json.Unmarshal(p.ObjectAttributes, &note); err != nil {
			return nil, fmt.Errorf("gitlab: decode note: %w", err)
		}
		if note.Action == "update" { // only new comments count
			return nil, nil
		}
		var src core.SourceRef
		switch {
		case note.NoteableType == "MergeRequest" && p.MergeRequest != nil:
			src = core.SourceRef{Provider: Provider, Kind: "pull_request", URL: p.MergeRequest.URL, Repo: repo, Number: p.MergeRequest.IID, Title: p.MergeRequest.Title}
		case note.NoteableType == "Issue" && p.Issue != nil:
			src = core.SourceRef{Provider: Provider, Kind: "issue", URL: p.Issue.URL, Repo: repo, Number: p.Issue.IID, Title: p.Issue.Title}
		default:
			return nil, nil
		}
		attrs := map[string]any{"discussion_id": note.DiscussionID, "comment_url": note.URL}
		if src.Kind == "pull_request" {
			attrs[eventattr.PRNumber] = src.Number
			// A note in a discussion (a review thread) gets its reply in
			// that discussion.
			if note.DiscussionID != "" && (note.Type == "DiffNote" || note.Type == "DiscussionNote") {
				src.ThreadRef = note.DiscussionID
				attrs[eventattr.ThreadRef] = note.DiscussionID
			}
		}
		// Comments continue an existing work item on the same merge request / issue.
		emit(src.Kind+"_commented", src, note.Note, attrs)
		mentioned := forgeutil.ExtractMentions(note.Note)
		var botHits []string
		alias := ""
		for _, m := range mentioned {
			lm := strings.ToLower(m)
			if a, ok := bots.factoryBots[lm]; ok {
				botHits = append(botHits, m)
				if alias == "" {
					alias = a
				}
			} else if bots.teamBot != "" && lm == bots.teamBot {
				botHits = append(botHits, m)
			}
		}
		if len(botHits) > 0 && !self {
			ma := map[string]any{eventattr.Mentioned: botHits, "discussion_id": note.DiscussionID}
			if alias != "" {
				ma[eventattr.Alias] = alias
			}
			emit("bot_mentioned", src, note.Note, ma)
		}
	case "push":
		if !strings.HasPrefix(p.Ref, "refs/heads/") || strings.Trim(p.After, "0") == "" {
			return nil, nil
		}
		branch := strings.TrimPrefix(p.Ref, "refs/heads/")
		var paths []string
		text := ""
		for _, c := range p.Commits {
			paths = append(paths, c.Added...)
			paths = append(paths, c.Modified...)
			paths = append(paths, c.Removed...)
			text = c.Message
		}
		emit("push", core.SourceRef{Provider: Provider, Kind: "push", Repo: repo, ExternalID: repo + "@" + p.After, Title: branch}, text,
			map[string]any{eventattr.Branch: branch, eventattr.Paths: forgeutil.StringSet(paths...), eventattr.HeadSHA: p.After})
	case "issue":
		var is struct {
			IID         int    `json:"iid"`
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
			Action      string `json:"action"`
		}
		if err := json.Unmarshal(p.ObjectAttributes, &is); err != nil {
			return nil, fmt.Errorf("gitlab: decode issue: %w", err)
		}
		emit("issue", core.SourceRef{Provider: Provider, Kind: "issue", URL: is.URL, Repo: repo, Number: is.IID, Title: is.Title},
			joinText(is.Title, is.Description), map[string]any{eventattr.Action: is.Action, eventattr.Labels: labels})
	case "pipeline":
		var pl struct {
			ID     int64  `json:"id"`
			Ref    string `json:"ref"`
			SHA    string `json:"sha"`
			Status string `json:"status"`
			URL    string `json:"url"`
		}
		if err := json.Unmarshal(p.ObjectAttributes, &pl); err != nil {
			return nil, fmt.Errorf("gitlab: decode pipeline: %w", err)
		}
		src := core.SourceRef{Provider: Provider, Kind: "pipeline", URL: pl.URL, Repo: repo, ExternalID: fmt.Sprintf("%s/pipeline/%d", repo, pl.ID), Title: pl.Ref}
		attrs := map[string]any{eventattr.Branch: pl.Ref, eventattr.HeadSHA: pl.SHA, eventattr.Conclusion: pl.Status}
		if p.MergeRequest != nil {
			attrs[eventattr.PRNumber] = p.MergeRequest.IID
		}
		emit("pipeline", src, "", attrs)
	}
	return out, nil
}

func joinText(title, body string) string {
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}
