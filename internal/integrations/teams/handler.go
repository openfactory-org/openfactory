package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// Provider is the trigger provider name; IntegrationProvider is the core.Integration provider.
const (
	Provider            = eventattr.ProviderTeams
	IntegrationProvider = eventattr.IntegrationTeams
	// DefinitionType is the factory.yaml integrations[].type.
	DefinitionType = "microsoft-teams"
)

// MessagesPath is the Bot Framework messaging endpoint.
const MessagesPath = "/integrations/teams/messages"

// DefaultServiceURL is used when no serviceUrl was recorded for a conversation.
const DefaultServiceURL = "https://smba.trafficmanager.net/teams/"

// NotConnectedMessage is posted when a channel's team is not connected to a factory.
const NotConnectedMessage = "This Microsoft Teams channel isn’t connected to an Open Factory factory. Connect this team to a factory, then mention this bot again."

// Options configures the Teams handler and responder.
//
// Integration records (provider "microsoft_teams"), one per connected team and factory:
//   - config team_id: Microsoft Graph team UUID (channelData.team.aadGroupId), necessary.
//     One record serves one Microsoft team. The record of a factory (the
//     record's factory_id) wins over the team record.
//   - config tenant_id: Microsoft 365 tenant of the installation (only its users may start work).
//     It is also the token tenant of a single-tenant bot (see tokenTenants).
//   - config app_type: "SingleTenant" or "MultiTenant" (optional; see tokenTenants)
//   - config app_id: bot app ID (necessary)
//   - config channel_ids: optional comma-separated channel IDs selected for the factory
//   - SecretRef: bot app password / client secret (necessary)
type Options struct {
	Store             core.Store
	Secrets           core.SecretStore
	Bus               core.EventBus
	OpenIDMetadataURL string
	LoginURL          string
	HTTPClient        *http.Client
	// GraphURL is the Microsoft Graph endpoint (default
	// https://graph.microsoft.com/v1.0). The thread context and the name
	// resolver use it.
	GraphURL string
	Linker   *integrations.Linker
	Logger   *slog.Logger
	Now      func() time.Time
}

// Handler serves the Bot Framework activities endpoint and implements core.Responder.
type Handler struct {
	opts  Options
	log   *slog.Logger
	keys  *KeySet
	dedup *forgeutil.Dedupe
	wg    sync.WaitGroup

	mu     sync.Mutex
	tokens map[string]*TokenSource
}

var _ core.Responder = (*Handler)(nil)

// NewHandler creates the handler.
func NewHandler(opts Options) *Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{
		opts:   opts,
		log:    forgeutil.Logger(opts.Logger).With("component", "teams"),
		keys:   &KeySet{MetadataURL: opts.OpenIDMetadataURL, HTTPClient: opts.HTTPClient, Now: opts.Now},
		dedup:  forgeutil.NewDedupe(time.Hour),
		tokens: map[string]*TokenSource{},
	}
}

// Register mounts POST /integrations/teams/messages.
func (h *Handler) Register(mux *http.ServeMux) { mux.HandleFunc("POST "+MessagesPath, h.serveMessages) }

// Drain waits for in-flight background processing.
func (h *Handler) Drain() { h.wg.Wait() }

// Activity is the subset of a Bot Framework activity used here.
type Activity struct {
	Type         string   `json:"type"`
	ID           string   `json:"id"`
	Timestamp    string   `json:"timestamp,omitempty"`
	ServiceURL   string   `json:"serviceUrl,omitempty"`
	ChannelID    string   `json:"channelId,omitempty"`
	From         *Account `json:"from,omitempty"`
	Recipient    *Account `json:"recipient,omitempty"`
	Conversation *struct {
		ID               string `json:"id"`
		ConversationType string `json:"conversationType"`
		TenantID         string `json:"tenantId"`
	} `json:"conversation,omitempty"`
	Text       string `json:"text,omitempty"`
	TextFormat string `json:"textFormat,omitempty"`
	ReplyToID  string `json:"replyToId,omitempty"`
	Entities   []struct {
		Type      string   `json:"type"`
		Mentioned *Account `json:"mentioned"`
		Text      string   `json:"text"`
	} `json:"entities,omitempty"`
	ChannelData *struct {
		Team *struct {
			ID         string `json:"id"`
			AADGroupID string `json:"aadGroupId"`
			Name       string `json:"name"`
		} `json:"team"`
		Channel *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channel"`
		Tenant *struct {
			ID string `json:"id"`
		} `json:"tenant"`
	} `json:"channelData,omitempty"`
}

// Account is a Bot Framework channel account.
type Account struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AADObjectID string `json:"aadObjectId,omitempty"`
}

// isOwnBot reports whether a Bot Framework account ID ("28:<app id>") belongs
// to one of the Open Factory bot registrations known to this server.
func (h *Handler) isOwnBot(ctx context.Context, fromID string) bool {
	id, ok := strings.CutPrefix(fromID, "28:")
	if !ok || id == "" {
		return false
	}
	for _, a := range h.appIDs(ctx) {
		if a != "" && strings.EqualFold(a, id) {
			return true
		}
	}
	return false
}

func (h *Handler) appIDs(ctx context.Context) []string {
	var ids []string
	if h.opts.Store != nil {
		ints, _ := forgeutil.FindIntegrations(ctx, h.opts.Store, IntegrationProvider, nil)
		for _, in := range ints {
			ids = append(ids, in.Config["app_id"])
		}
	}
	return forgeutil.StringSet(ids...)
}

func (h *Handler) serveMessages(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	var act Activity
	if err := json.Unmarshal(body, &act); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "activity is not valid JSON")
		return
	}
	ctx := r.Context()
	if err := h.keys.Validate(ctx, r.Header.Get("Authorization"), h.appIDs(ctx), act.ChannelID, act.ServiceURL); err != nil {
		h.log.Info("rejected bot framework request", "err", err)
		forgeutil.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid Bot Connector token")
		return
	}
	if act.Type != "message" || h.dedup.Seen(act.ID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := h.handleMessage(bctx, act, body); err != nil {
			h.log.Warn("handle teams activity", "id", act.ID, "err", err)
		}
	}()
	w.WriteHeader(http.StatusOK)
}

var atTag = regexp.MustCompile(`(?is)<at[^>]*>.*?</at>`)
var htmlTag = regexp.MustCompile(`<[^>]+>`)

// CleanText removes <at> mention tags and HTML markup from a Teams message.
func CleanText(s string) string {
	s = atTag.ReplaceAllString(s, "")
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "</p>", "\n").Replace(s)
	s = htmlTag.ReplaceAllString(s, "")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	return strings.TrimSpace(s)
}

// ThreadRoot returns the channel ID and the thread's root message ID from a conversation ID
// ("19:abc@thread.tacv2;messageid=1700000000000").
func ThreadRoot(conversationID, activityID string) (channel, root string) {
	channel, rest, found := strings.Cut(conversationID, ";messageid=")
	if found && rest != "" {
		return channel, rest
	}
	return channel, activityID
}

// findIntegration selects the record of an activity with the selection rule:
// the records whose team_id is the Microsoft Graph
// team of the activity and whose app_id is the bot that received it
// (recipient "28:<app id>"). In the team, the team record wins over a
// factory record.
func (h *Handler) findIntegration(ctx context.Context, graphTeamID, recipientID string) (*core.Integration, error) {
	botID, _ := strings.CutPrefix(recipientID, "28:")
	return integrations.Intake(ctx, h.opts.Store, IntegrationProvider, graphTeamID, func(in core.Integration) bool {
		return botID == "" || strings.EqualFold(in.Config["app_id"], botID)
	})
}

func (h *Handler) handleMessage(ctx context.Context, act Activity, raw []byte) error {
	if act.From == nil || act.Recipient == nil || act.Conversation == nil {
		return nil
	}
	if act.From.ID == act.Recipient.ID || h.isOwnBot(ctx, act.From.ID) {
		return nil // posts by Open Factory's own bots never start or continue work
	}
	if act.Conversation.ConversationType != "channel" {
		return nil // private chats and group chats are not supported
	}
	mentioned := false
	for _, e := range act.Entities {
		if e.Type == "mention" && e.Mentioned != nil && e.Mentioned.ID == act.Recipient.ID {
			mentioned = true
		}
	}
	graphTeam, tenant, channelID, channelName := "", act.Conversation.TenantID, "", ""
	if cd := act.ChannelData; cd != nil {
		if cd.Team != nil {
			graphTeam = cd.Team.AADGroupID
		}
		if cd.Tenant != nil && cd.Tenant.ID != "" {
			tenant = cd.Tenant.ID
		}
		if cd.Channel != nil {
			channelID, channelName = cd.Channel.ID, cd.Channel.Name
		}
	}
	convChannel, root := ThreadRoot(act.Conversation.ID, act.ID)
	if channelID == "" {
		channelID = convChannel
	}
	if h.opts.Store != nil && act.ServiceURL != "" {
		if err := h.opts.Store.KVSet(ctx, serviceURLKey(channelID), act.ServiceURL); err != nil {
			h.log.Debug("record service url", "err", err)
		}
	}
	text := CleanText(act.Text)
	in, err := h.findIntegration(ctx, graphTeam, act.Recipient.ID)
	if errors.Is(err, core.ErrIntegrationDisabled) {
		h.log.Info("activity for a disabled integration ignored", "team", graphTeam, "err", err)
		return nil
	}
	if err != nil {
		if mentioned {
			h.reply(ctx, h.botRecord(ctx, act.Recipient.ID), act, NotConnectedMessage)
		}
		return nil
	}
	actorID := act.From.AADObjectID
	if actorID == "" {
		actorID = act.From.ID
	}
	isBot := strings.HasPrefix(act.From.ID, "28:")
	actor := core.Actor{Provider: Provider, ExternalID: actorID, Handle: act.From.Name, IsBot: isBot}

	if mentioned {
		switch strings.ToLower(strings.Trim(text, " .!")) {
		case "sign in", "signin", "login", "link":
			h.reply(ctx, in, act, h.signInText(in, actor))
			return nil
		case "sign out", "signout", "logout", "unlink":
			if err := h.opts.Linker.Unlink(ctx, h.opts.Store, Provider, actorID, ""); err != nil {
				return err
			}
			h.reply(ctx, in, act, "Your Microsoft account is no longer linked to Open Factory in this organization.")
			return nil
		case "help", "":
			h.reply(ctx, in, act, "Mention me with a request to start factory work in this thread. Reply in the thread to continue it. Use `sign in` to link your Microsoft account, or `sign out` to unlink it.")
			return nil
		}
	}

	// Eligible users: the installation's organization and a linked active team member.
	member, err := integrations.Member(ctx, h.opts.Store, in.TeamID, Provider, actorID)
	if err != nil {
		h.log.Warn("membership lookup", "err", err)
	}
	if want := in.Config["tenant_id"]; want != "" && !strings.EqualFold(want, tenant) {
		member = nil
		if mentioned {
			h.reply(ctx, in, act, "Only people in this team's Microsoft 365 organization with a linked Open Factory account can start or continue factory work.")
		}
		return nil
	}
	isReply := root != act.ID
	if member == nil {
		if mentioned {
			h.reply(ctx, in, act, h.signInText(in, actor))
		}
		return nil
	}
	actor.UserID = member.ID
	factoryID := in.FactoryID
	attrs := map[string]any{
		eventattr.TeamID:      graphTeam,
		eventattr.ChannelID:   channelID,
		eventattr.UserID:      actorID,
		eventattr.TenantID:    tenant,
		eventattr.ActorIsBot:  isBot,
		eventattr.ActorIsSelf: false, // own bots are dropped above
		eventattr.Linked:      true,
	}
	if text != "" {
		attrs[eventattr.Text] = text
	}
	// Names for the filters that name channels and users. Teams gives the
	// name of a channel only when it is not the General channel, and no
	// e-mail address of the sender.
	if channelName != "" {
		attrs[eventattr.ChannelName] = channelName
	}
	if act.From.Name != "" {
		attrs[eventattr.UserName] = act.From.Name
	}
	if isReply {
		attrs[eventattr.ThreadReply] = true
		attrs[eventattr.AutoFollowUp] = integrations.AutoRespondToThreadReplies(ctx, h.opts.Store, factoryID, DefinitionType)
	}
	src := core.SourceRef{Provider: Provider, Kind: "thread", Channel: channelID, ThreadID: root, Workspace: core.IntegrationWorkspace(*in)}
	emit := func(typ string) {
		a := make(map[string]any, len(attrs))
		for k, v := range attrs {
			a[k] = v
		}
		ev := &core.Event{
			ID: core.NewID("evt"), TeamID: in.TeamID, FactoryID: factoryID, Provider: Provider, Type: typ,
			DeliveryID: act.ID + ":" + typ, Actor: actor, Source: src, Attributes: a,
			Payload: json.RawMessage(raw), Text: text, ReceivedAt: h.opts.Now().UTC(),
		}
		if h.opts.Bus != nil {
			h.opts.Bus.Publish(ctx, core.TopicInboundEvent, ev)
		}
	}
	if mentioned {
		emit("app_mention")
	}
	emit("message_posted")
	return nil
}

// signInText is the link prompt. A thread reply is public, so it has no
// link URL.
func (h *Handler) signInText(_ *core.Integration, _ core.Actor) string {
	return "To use this factory, link your Microsoft account to an active member of the factory's Open Factory workspace, then send your request again. " + h.opts.Linker.DashboardHint(Provider)
}

func serviceURLKey(channelID string) string { return "teams:service_url:" + channelID }

// botRecord returns a record of the bot that received an activity
// ("28:<app id>"), for its credentials only. It returns nil when no record
// has that app_id.
func (h *Handler) botRecord(ctx context.Context, recipientID string) *core.Integration {
	id, ok := strings.CutPrefix(recipientID, "28:")
	if !ok || id == "" || h.opts.Store == nil {
		return nil
	}
	ints, _ := forgeutil.FindIntegrations(ctx, h.opts.Store, IntegrationProvider, func(in core.Integration) bool {
		return strings.EqualFold(in.Config["app_id"], id) && in.SecretRef != "" && in.Active()
	})
	if len(ints) == 0 {
		return nil
	}
	return &ints[0]
}

// botCredentials returns the app ID and the password of the bot of a record
// (config app_id and SecretRef).
func (h *Handler) botCredentials(ctx context.Context, in *core.Integration) (appID, password string, err error) {
	if in == nil || in.Config["app_id"] == "" || in.SecretRef == "" {
		return "", "", errors.New("teams: bot credentials are not configured: set config app_id and secret_ref on the record")
	}
	sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, *in, "")
	if err != nil {
		return "", "", err
	}
	return in.Config["app_id"], string(sec), nil
}

func (h *Handler) tokenSource(ctx context.Context, in *core.Integration) (*TokenSource, error) {
	appID, password, err := h.botCredentials(ctx, in)
	if err != nil {
		return nil, err
	}
	tenant, fallback, err := h.tokenTenants(in)
	if err != nil {
		return nil, err
	}
	key := appID + "|" + tenant + "|" + fallback
	h.mu.Lock()
	defer h.mu.Unlock()
	ts, ok := h.tokens[key]
	if !ok || ts.AppPassword != password {
		ts = &TokenSource{AppID: appID, AppPassword: password, Tenant: tenant, FallbackTenant: fallback, LoginURL: h.opts.LoginURL, HTTPClient: h.opts.HTTPClient, Now: h.opts.Now}
		h.tokens[key] = ts
	}
	return ts, nil
}

// tokenTenants picks the tenant Bot Connector tokens are requested from for a
// record (and an optional fallback):
//
//   - config app_type "SingleTenant": the record's tenant_id (required);
//   - config app_type "MultiTenant": botframework.com;
//   - otherwise botframework.com, falling back to the record's tenant_id when
//     the botframework.com request fails (a single-tenant bot).
func (h *Handler) tokenTenants(in *core.Integration) (string, string, error) {
	recTenant, appType := "", ""
	if in != nil {
		recTenant = strings.TrimSpace(in.Config["tenant_id"])
		appType = strings.ToLower(strings.TrimSpace(in.Config["app_type"]))
	}
	switch appType {
	case "singletenant", "single_tenant", "single-tenant":
		if recTenant == "" {
			return "", "", errors.New("teams: app_type SingleTenant requires config tenant_id")
		}
		return recTenant, "", nil
	case "multitenant", "multi_tenant", "multi-tenant":
		return DefaultTokenTenant, "", nil
	case "":
	default:
		return "", "", fmt.Errorf("teams: unknown app_type %q (want SingleTenant or MultiTenant)", in.Config["app_type"])
	}
	return DefaultTokenTenant, recTenant, nil
}

// send posts a message activity into a channel thread.
func (h *Handler) send(ctx context.Context, in *core.Integration, serviceURL, channelID, rootID, text string) error {
	ts, err := h.tokenSource(ctx, in)
	if err != nil {
		return err
	}
	tok, err := ts.Token(ctx)
	if err != nil {
		return err
	}
	conv := channelID
	if rootID != "" {
		conv += ";messageid=" + rootID
	}
	c := &forgeutil.Client{
		BaseURL: strings.TrimRight(serviceURL, "/"), HTTP: h.opts.HTTPClient, UserAgent: "openfactory-teams",
		Authorize: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+tok)
			return nil
		},
	}
	activity := map[string]any{"type": "message", "text": text, "textFormat": "markdown"}
	if err := c.Do(ctx, http.MethodPost, "/v3/conversations/"+url.PathEscape(conv)+"/activities", activity, nil); err != nil {
		return fmt.Errorf("teams: send activity: %w", err)
	}
	return nil
}

// PostMessage posts a message into a channel, or into a thread of it when
// rootID is set, as the bot of the integration record in. The built-in tool
// server uses it. If the record selects channels (config channel_ids), only
// those channels are allowed. Otherwise the bot may post only in a channel
// where it already got an activity, because only then the service URL of the
// channel is known.
func (h *Handler) PostMessage(ctx context.Context, in core.Integration, channelID, rootID, text string) error {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return fmt.Errorf("%w: teams: a channel is required", core.ErrInvalid)
	}
	serviceURL := ""
	if h.opts.Store != nil {
		serviceURL, _ = h.opts.Store.KVGet(ctx, serviceURLKey(channelID))
	}
	selected := false
	var allowed []string
	for _, id := range strings.Split(in.Config["channel_ids"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			allowed = append(allowed, id)
			selected = selected || id == channelID
		}
	}
	switch {
	case len(allowed) > 0 && !selected:
		return fmt.Errorf("%w: teams: channel %s is not one of the channels of this integration", core.ErrForbidden, channelID)
	case serviceURL == "" && !selected:
		return fmt.Errorf("%w: teams: the bot has not been used in channel %s", core.ErrForbidden, channelID)
	}
	if serviceURL == "" {
		serviceURL = DefaultServiceURL
	}
	return h.send(ctx, &in, serviceURL, channelID, rootID, text)
}

func (h *Handler) reply(ctx context.Context, in *core.Integration, act Activity, text string) {
	channel, root := ThreadRoot(act.Conversation.ID, act.ID)
	if err := h.send(ctx, in, act.ServiceURL, channel, root, text); err != nil {
		h.log.Warn("teams reply", "err", err)
	}
}

// Provider implements core.Responder.
func (h *Handler) Provider() string { return Provider }

// Reply implements core.Responder: it replies in the originating channel thread.
func (h *Handler) Reply(ctx context.Context, teamID string, msg core.OutboundMessage) error {
	if msg.Source.Channel == "" {
		return errors.New("teams: outbound message has no channel")
	}
	sel, err := integrations.SelectTeam(ctx, h.opts.Store, teamID, IntegrationProvider, msg.FactoryID, msg.Source.Workspace)
	if err != nil {
		return err
	}
	in := &sel
	serviceURL := DefaultServiceURL
	if h.opts.Store != nil {
		if v, err := h.opts.Store.KVGet(ctx, serviceURLKey(msg.Source.Channel)); err == nil && v != "" {
			serviceURL = v
		}
	}
	return h.send(ctx, in, serviceURL, msg.Source.Channel, msg.Source.ThreadID, forgeutil.FormatOutbound(msg))
}
