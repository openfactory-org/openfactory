// Package jira connects factories to Jira Cloud: the webhook receiver
// (shared-secret HMAC signature or Connect-style JWT with query-string hash),
// issue created / updated / commented events, agent sessions (the factory's
// Jira account being assigned or mentioned), account linking, and a
// core.Responder that posts comments through REST v3 with ADF bodies.
package jira

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// Provider is the trigger and integration provider name.
const Provider = eventattr.ProviderJira

// WebhookPath is where Jira delivers webhooks.
const WebhookPath = "/integrations/jira/webhook"

// Options configures the webhook receiver and responder.
//
// Integration records (provider "jira"), one per Jira Cloud site and factory:
//   - config site: the host name of the site, e.g. acme.atlassian.net (or the
//     cloud ID of the site), necessary. One record serves one site. The
//     record of a factory (the record's factory_id) wins over the team
//     record.
//   - config base_url: site URL, e.g. https://acme.atlassian.net (or https://api.atlassian.com/ex/jira/<cloudId> for OAuth)
//   - config auth: "basic" (default; email + API token) or "oauth" (Bearer access token)
//   - config email: account e-mail for basic auth
//   - config bot_account_id: the factory's Jira account; assigning or mentioning it starts an agent session
//   - config client_key: Connect client key (JWT iss) when deliveries are signed with a Connect JWT
//   - config webhook_secret_ref: secret name for the webhook HMAC secret (X-Hub-Signature)
//   - config shared_secret_ref: secret name for the Connect shared secret (JWT HS256)
//   - SecretRef: API token or OAuth access token
type Options struct {
	Store      core.Store
	Secrets    core.SecretStore
	Bus        core.EventBus
	HTTPClient *http.Client
	Linker     *integrations.Linker
	Logger     *slog.Logger
	Now        func() time.Time
}

// Handler receives Jira webhooks and implements core.Responder.
type Handler struct {
	opts  Options
	log   *slog.Logger
	dedup *forgeutil.Dedupe
	wg    sync.WaitGroup
}

var _ core.Responder = (*Handler)(nil)

// NewHandler creates the handler.
func NewHandler(opts Options) *Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "jira"), dedup: forgeutil.NewDedupe(time.Hour)}
}

// Register mounts POST /integrations/jira/webhook.
func (h *Handler) Register(mux *http.ServeMux) { mux.HandleFunc("POST "+WebhookPath, h.serveWebhook) }

// Drain waits for in-flight background processing.
func (h *Handler) Drain() { h.wg.Wait() }

type user struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	AccountType string `json:"accountType"`
}

type issue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Labels      []string        `json:"labels"`
		Status      *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"status"`
		Project *struct {
			ID   string `json:"id"`
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"project"`
		Assignee *user `json:"assignee"`
		Reporter *user `json:"reporter"`
		Creator  *user `json:"creator"`
	} `json:"fields"`
}

type webhookPayload struct {
	Timestamp          int64  `json:"timestamp"`
	WebhookEvent       string `json:"webhookEvent"`
	IssueEventTypeName string `json:"issue_event_type_name"`
	User               *user  `json:"user"`
	Issue              *issue `json:"issue"`
	Changelog          *struct {
		ID    string `json:"id"`
		Items []struct {
			Field      string `json:"field"`
			FieldID    string `json:"fieldId"`
			From       string `json:"from"`
			FromString string `json:"fromString"`
			To         string `json:"to"`
			ToString   string `json:"toString"`
		} `json:"items"`
	} `json:"changelog"`
	Comment *struct {
		ID     string          `json:"id"`
		Body   json.RawMessage `json:"body"`
		Author *user           `json:"author"`
	} `json:"comment"`
}

func (h *Handler) serveWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "payload is not valid JSON")
		return
	}
	ctx := r.Context()
	jwtIss := ""
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "JWT "); ok {
		if j, err := forgeutil.ParseJWT(tok); err == nil {
			jwtIss = j.ClaimString("iss")
		}
	}
	site := ""
	if p.Issue != nil {
		site = p.Issue.Self
	}
	integ, ierr := h.findIntegration(ctx, site, jwtIss)
	if !h.verify(ctx, integ, r, body) {
		forgeutil.WriteError(w, http.StatusUnauthorized, "invalid_signature", "webhook authentication failed")
		return
	}
	if ierr != nil {
		h.log.Info("delivery ignored", "site", site, "err", ierr)
		forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "ignored", "reason": "no matching integration"})
		return
	}
	delivery := r.Header.Get("X-Atlassian-Webhook-Identifier")
	if h.dedup.Seen(delivery) {
		forgeutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "duplicate"})
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		evs := h.Normalize(bctx, *integ, delivery, body)
		forgeutil.Publish(bctx, h.opts.Bus, evs)
	}()
	forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
}

// QSH computes the Connect query-string hash of a request.
func QSH(method, path string, query url.Values) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		if k == "jwt" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	enc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	for _, k := range keys {
		vals := append([]string(nil), query[k]...)
		sort.Strings(vals)
		for i := range vals {
			vals[i] = enc(vals[i])
		}
		parts = append(parts, enc(k)+"="+strings.Join(vals, ","))
	}
	if path == "" {
		path = "/"
	}
	canonical := strings.ToUpper(method) + "&" + path + "&" + strings.Join(parts, "&")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) verify(ctx context.Context, integ *core.Integration, r *http.Request, body []byte) bool {
	if sig := r.Header.Get("X-Hub-Signature"); sig != "" {
		if integ != nil && integ.Config["webhook_secret_ref"] != "" {
			sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, *integ, "webhook_secret_ref")
			if err == nil {
				return forgeutil.VerifyHMACSHA256Hex(sec, body, sig)
			}
			h.log.Warn("load webhook secret", "integration", integ.ID, "err", err)
		}
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "JWT ")
	if !ok || integ == nil || integ.Config["shared_secret_ref"] == "" {
		return false
	}
	sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, *integ, "shared_secret_ref")
	if err != nil {
		h.log.Warn("load connect shared secret", "integration", integ.ID, "err", err)
		return false
	}
	j, err := forgeutil.ParseJWT(tok)
	if err != nil || j.VerifyHS256(sec) != nil || j.VerifyTime(h.opts.Now(), time.Minute) != nil {
		return false
	}
	if ck := integ.Config["client_key"]; ck != "" && j.ClaimString("iss") != ck {
		return false
	}
	return j.ClaimString("qsh") == QSH(r.Method, r.URL.Path, r.URL.Query())
}

// findIntegration selects the record of a webhook delivery with the
// selection rule: the records whose site is the site
// of the issue. A delivery signed by a Connect app (clientKey) can only come
// to the records of that app. In the team, the team record wins over a
// factory record.
func (h *Handler) findIntegration(ctx context.Context, issueSelf, clientKey string) (*core.Integration, error) {
	return integrations.Intake(ctx, h.opts.Store, Provider, issueSelf, func(in core.Integration) bool {
		return clientKey == "" || in.Config["client_key"] == "" || in.Config["client_key"] == clientKey
	})
}

// BrowseURL returns the web URL of an issue.
func BrowseURL(self, key string) string {
	p, err := url.Parse(self)
	if err != nil || p.Host == "" || key == "" {
		return ""
	}
	return p.Scheme + "://" + p.Host + "/browse/" + key
}

func splitLabels(s string) []string {
	return strings.Fields(s)
}

// Normalize converts a verified webhook payload into events.
func (h *Handler) Normalize(ctx context.Context, in core.Integration, deliveryID string, body []byte) []*core.Event {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Issue == nil {
		return nil
	}
	is := p.Issue
	bot := in.Config["bot_account_id"]
	factoryID := in.FactoryID
	actorUser := p.User
	if p.Comment != nil && p.Comment.Author != nil {
		actorUser = p.Comment.Author
	}
	actor := core.Actor{Provider: Provider}
	if actorUser != nil {
		actor.ExternalID, actor.Handle = actorUser.AccountID, actorUser.DisplayName
		actor.IsBot = actorUser.AccountType == "app"
	}
	self := bot != "" && actor.ExternalID == bot
	if self {
		return nil // the factory's own Jira activity never starts work
	}
	// Activity by another factory's Jira account on this server is marked
	// actor_is_self so it never matches triggers.
	ownAccount := false
	if actor.ExternalID != "" && h.opts.Store != nil {
		if all, err := forgeutil.FindIntegrations(ctx, h.opts.Store, Provider, nil); err == nil {
			for _, o := range all {
				if o.Config["bot_account_id"] == actor.ExternalID {
					ownAccount = true
					actor.IsBot = true
				}
			}
		}
	}
	forgeutil.LinkActor(ctx, h.opts.Store, &actor)
	projectKey, projectName := "", ""
	if is.Fields.Project != nil {
		projectKey, projectName = is.Fields.Project.Key, is.Fields.Project.Name
	}
	src := core.SourceRef{Provider: Provider, Kind: "issue", URL: BrowseURL(is.Self, is.Key), ExternalID: is.Key, Title: is.Fields.Summary, Workspace: core.IntegrationWorkspace(in)}
	desc := Text(is.Fields.Description)
	issueText := strings.TrimSpace(is.Fields.Summary + "\n\n" + desc)
	base := func() map[string]any {
		a := map[string]any{
			eventattr.ProjectKey:  projectKey,
			eventattr.ProjectName: projectName,
			eventattr.Labels:      append([]string{}, is.Fields.Labels...),
			eventattr.ActorIsBot:  actor.IsBot,
			eventattr.ActorIsSelf: ownAccount,
			"issue_key":           is.Key,
		}
		if is.Fields.Status != nil {
			a[eventattr.StatusID] = is.Fields.Status.ID
			a[eventattr.StatusName] = is.Fields.Status.Name
		}
		return a
	}
	var out []*core.Event
	emit := func(typ, text string, a map[string]any, s core.SourceRef) {
		if text != "" {
			a[eventattr.Text] = text
		}
		did := deliveryID
		if did != "" {
			did += ":" + typ
		}
		out = append(out, &core.Event{
			ID: core.NewID("evt"), TeamID: in.TeamID, FactoryID: factoryID, Provider: Provider, Type: typ,
			DeliveryID: did, Actor: actor, Source: s, Attributes: a, Payload: json.RawMessage(body),
			Text: text, ReceivedAt: h.opts.Now().UTC(),
		})
	}
	sessionText := ""
	startSession := false
	switch p.WebhookEvent {
	case "jira:issue_created":
		a := base()
		a[eventattr.AddedLabels] = append([]string{}, is.Fields.Labels...)
		emit("issue_created", issueText, a, src)
		if len(is.Fields.Labels) > 0 {
			la := base()
			la[eventattr.AddedLabels] = append([]string{}, is.Fields.Labels...)
			emit("issue_labeled", issueText, la, src)
		}
		if bot != "" && is.Fields.Assignee != nil && is.Fields.Assignee.AccountID == bot {
			startSession, sessionText = true, issueText
		}
		for _, m := range Mentions(is.Fields.Description) {
			if bot != "" && m == bot {
				startSession, sessionText = true, issueText
			}
		}
	case "jira:issue_updated":
		if p.Changelog == nil {
			break
		}
		for _, it := range p.Changelog.Items {
			switch strings.ToLower(it.Field) {
			case "labels":
				added := forgeutil.Diff(splitLabels(it.ToString), splitLabels(it.FromString))
				if len(added) > 0 {
					a := base()
					a[eventattr.Labels] = splitLabels(it.ToString)
					a[eventattr.AddedLabels] = added
					emit("issue_labeled", issueText, a, src)
				}
			case "status":
				a := base()
				a[eventattr.StatusID] = it.To
				a[eventattr.StatusName] = it.ToString
				emit("status_changed", issueText, a, src)
			case "assignee":
				if bot != "" && it.To == bot {
					startSession, sessionText = true, issueText
				}
			}
		}
	case "comment_created":
		if p.Comment == nil {
			break
		}
		text := Text(p.Comment.Body)
		a := base()
		a["comment_id"] = p.Comment.ID
		// Comments continue the issue's existing work item (same source key).
		emit("comment_created", text, a, src)
		for _, m := range Mentions(p.Comment.Body) {
			if bot != "" && m == bot {
				startSession, sessionText = true, text
			}
		}
	}
	if startSession {
		member, err := integrations.Member(ctx, h.opts.Store, in.TeamID, Provider, actor.ExternalID)
		if err != nil {
			h.log.Warn("membership lookup", "err", err)
		}
		if member == nil {
			// A comment is public, so the prompt has no link URL.
			msg := "To start factory work from Jira, connect your Jira account to an active member of this factory's Open Factory team, then assign or mention the factory again. " + h.opts.Linker.DashboardHint(Provider)
			if err := h.comment(ctx, in, is.Key, msg); err != nil {
				h.log.Warn("post connect prompt", "err", err)
			}
		} else {
			actor.UserID = member.ID
			a := base()
			a[eventattr.Linked] = true
			emit("agent_session_created", sessionText, a, src)
		}
	}
	return out
}

func (h *Handler) apiClient(ctx context.Context, in core.Integration) (*forgeutil.Client, error) {
	tok, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, in, "")
	if err != nil {
		return nil, err
	}
	return NewAPIClient(in, tok, h.opts.HTTPClient)
}

// NewAPIClient returns a Jira REST client for an integration record and its
// token (an API token with config email, or an OAuth token with config auth=oauth).
func NewAPIClient(in core.Integration, tok []byte, hc *http.Client) (*forgeutil.Client, error) {
	base := strings.TrimRight(in.Config["base_url"], "/")
	if base == "" {
		return nil, fmt.Errorf("jira: integration %s has no base_url", in.ID)
	}
	var auth string
	if strings.EqualFold(in.Config["auth"], "oauth") {
		auth = "Bearer " + string(tok)
	} else {
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(in.Config["email"]+":"+string(tok)))
	}
	return &forgeutil.Client{
		BaseURL: base, HTTP: hc, UserAgent: "openfactory-jira",
		Authorize: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", auth)
			return nil
		},
	}, nil
}

func (h *Handler) comment(ctx context.Context, in core.Integration, key, markdown string) error {
	c, err := h.apiClient(ctx, in)
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment",
		map[string]any{"body": MarkdownToADF(markdown)}, nil); err != nil {
		return fmt.Errorf("jira: comment on %s: %w", key, err)
	}
	return nil
}

// Provider implements core.Responder.
func (h *Handler) Provider() string { return Provider }

// Reply implements core.Responder by commenting on the originating issue.
func (h *Handler) Reply(ctx context.Context, teamID string, msg core.OutboundMessage) error {
	if msg.Source.ExternalID == "" {
		return errors.New("jira: outbound message has no issue key")
	}
	in, err := h.integrationFor(core.WithFactory(ctx, msg.FactoryID), teamID, msg.Source)
	if err != nil {
		return err
	}
	return h.comment(ctx, in, msg.Source.ExternalID, forgeutil.FormatOutbound(msg))
}

// integrationFor selects the Jira record of a team for a source with the
// selection rule: the records whose site is the site
// of the source (Workspace, else the host of its URL), the record of the
// factory in ctx (core.WithFactory) first, then the team record.
func (h *Handler) integrationFor(ctx context.Context, teamID string, src core.SourceRef) (core.Integration, error) {
	site := src.Workspace
	if site == "" {
		site = src.URL
	}
	return integrations.SelectTeam(ctx, h.opts.Store, teamID, Provider, core.FactoryFrom(ctx), site)
}
