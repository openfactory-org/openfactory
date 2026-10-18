package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

type fakeSlack struct {
	mu    sync.Mutex
	calls map[string][]map[string]any
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer xoxb-test" && !strings.HasPrefix(r.URL.Path, "/respond") {
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/")
	body := map[string]any{}
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &body)
	}
	for k, v := range r.URL.Query() {
		body[k] = v[0]
	}
	f.mu.Lock()
	f.calls[method] = append(f.calls[method], body)
	f.mu.Unlock()
	switch method {
	case "users.info":
		switch body["user"] {
		case "U1":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U1","name":"alice","team_id":"T1","profile":{"email":"alice@example.com"}}}`)
		case "U3":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U3","name":"guest","team_id":"T1","is_restricted":true,"profile":{"email":"alice@example.com"}}}`)
		case "U4":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U4","name":"outsider","team_id":"T2","profile":{"email":"alice@example.com"}}}`)
		case "U5":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U5","name":"stranger","team_id":"T1","is_stranger":true,"profile":{"email":"alice@example.com"}}}`)
		case "U2":
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U2","name":"mallory","profile":{"email":"mallory@else.com"}}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U9","name":"bob","profile":{}}}`)
		}
	case "conversations.info":
		fmt.Fprint(w, `{"ok":true,"channel":{"name":"engineering"}}`)
	case "conversations.history":
		fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"111.222","text":"the build is broken","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"see logs"}}]}]}`)
	case "chat.postMessage":
		fmt.Fprint(w, `{"ok":true,"ts":"999.1"}`)
	default:
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func (f *fakeSlack) get(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

const signingSecret = "slack-signing"

type fixture struct {
	h       *Handler
	store   *forgetest.Store
	bus     *forgetest.Bus
	api     *fakeSlack
	srv     *httptest.Server
	actions []string
}

func newFixture(t *testing.T) *fixture {
	api := &fakeSlack{calls: map[string][]map[string]any{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_s", TeamID: "team_1", Provider: "slack", SecretRef: "SLACK_BOT",
		FactoryID: "fac_1",
		Config:    map[string]string{"team_id": "T1", "api_app_id": "A1", "bot_user_id": "UBOT", "signing_secret_ref": "SLACK_SIGNING"}}}
	store.Users = []core.User{{ID: "usr_1", TeamID: "team_1", Email: "alice@example.com"}}
	secrets := forgetest.NewSecrets(map[string]string{"SLACK_BOT": "xoxb-test", "SLACK_SIGNING": signingSecret})
	f := &fixture{store: store, bus: forgetest.NewBus(), api: api, srv: srv}
	var mu sync.Mutex
	f.h = NewHandler(Options{
		Store: store, Secrets: secrets, Bus: f.bus, APIBaseURL: srv.URL,
		Linker: &integrations.Linker{Key: []byte("k"), PublicURL: "https://of.example.com"},
		OnAction: func(_ context.Context, inboxID, option, actor string) error {
			mu.Lock()
			f.actions = append(f.actions, inboxID+"|"+option+"|"+actor)
			mu.Unlock()
			return nil
		},
	})
	return f
}

func (f *fixture) send(t *testing.T, path, contentType string, body []byte, secret string, ts time.Time) *httptest.ResponseRecorder {
	t.Helper()
	stamp := strconv.FormatInt(ts.Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", Sign([]byte(secret), stamp, body))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	f.h.Register(mux)
	mux.ServeHTTP(rec, req)
	f.h.Drain()
	return rec
}

func (f *fixture) event(t *testing.T, ev map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": "T1", "api_app_id": "A1", "event_id": core.NewID("Ev"), "event": ev})
	return f.send(t, EventsPath, "application/json", b, signingSecret, time.Now())
}

func TestURLVerificationAndSignature(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"type":"url_verification","challenge":"abc123","token":"x"}`)
	rec := f.send(t, EventsPath, "application/json", body, signingSecret, time.Now())
	if rec.Code != 200 || rec.Body.String() != "abc123" {
		t.Fatalf("challenge: %d %q", rec.Code, rec.Body)
	}
	if rec := f.send(t, EventsPath, "application/json", body, "wrong", time.Now()); rec.Code != 401 {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	if rec := f.send(t, EventsPath, "application/json", body, signingSecret, time.Now().Add(-10*time.Minute)); rec.Code != 401 {
		t.Fatalf("stale timestamp: %d", rec.Code)
	}
}

func TestAppMentionLinkedByEmail(t *testing.T) {
	f := newFixture(t)
	// The email link is off by default: the user gets a link prompt.
	f.event(t, map[string]any{"type": "app_mention", "user": "U1", "text": "<@UBOT> fix the login bug", "ts": "100.0", "channel": "C1", "channel_type": "channel"})
	if f.bus.Find("app_mention") != nil {
		t.Fatal("an unlinked user started work without the opt-in")
	}
	if _, err := f.store.FindExternalIdentity(context.Background(), "slack", "U1"); err == nil {
		t.Fatal("linked by email without the opt-in")
	}
	f.store.Integrations[0].Config[ConfigLinkByEmail] = "true"
	f.event(t, map[string]any{"type": "app_mention", "user": "U1", "text": "<@UBOT> fix the login bug", "ts": "100.1", "channel": "C1", "channel_type": "channel"})
	ev := f.bus.Find("app_mention")
	if ev == nil {
		t.Fatal("no app_mention event")
	}
	if ev.FactoryID != "fac_1" || ev.TeamID != "team_1" || ev.Actor.UserID != "usr_1" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Source.Key() != "slack:C1:100.1" || ev.Attributes[eventattr.ChannelName] != "engineering" || ev.Attributes[eventattr.UserEmail] != "alice@example.com" {
		t.Fatalf("source/attrs = %+v %v", ev.Source, ev.Attributes)
	}
	if _, err := f.store.FindExternalIdentity(context.Background(), "slack", "U1"); err != nil {
		t.Fatal("account not auto-linked by email")
	}
	if r := f.api.get("reactions.add"); len(r) != 1 || r[0]["name"] != "eyes" {
		t.Fatalf("acknowledgement = %v", r)
	}
}

func TestUnlinkedMentionPromptsLink(t *testing.T) {
	f := newFixture(t)
	f.event(t, map[string]any{"type": "app_mention", "user": "U2", "text": "<@UBOT> hi", "ts": "100.2", "channel": "C1"})
	if len(f.bus.Events()) != 0 {
		t.Fatal("unlinked user started work")
	}
	eph := f.api.get("chat.postEphemeral")
	if len(eph) != 1 || eph[0]["user"] != "U2" || !strings.Contains(eph[0]["text"].(string), "https://of.example.com/integrations/link/") {
		t.Fatalf("ephemeral = %v", eph)
	}
}

func TestMessagesAndReactions(t *testing.T) {
	f := newFixture(t)
	f.store.FactoryInts["fac_1"] = map[string]map[string]string{"slack": {"autoRespondToThreadReplies": "false"}}
	f.store.Identities["slack|U1"] = core.ExternalIdentity{UserID: "usr_1", Provider: "slack", ExternalID: "U1"}
	f.event(t, map[string]any{"type": "message", "user": "U1", "text": "dm", "ts": "1.1", "channel": "D1", "channel_type": "im"})
	f.event(t, map[string]any{"type": "message", "user": "U9", "text": "more info", "ts": "2.2", "thread_ts": "1.0", "channel": "C1", "channel_type": "channel"})
	f.event(t, map[string]any{"type": "message", "subtype": "message_changed", "channel": "C1"})
	f.event(t, map[string]any{"type": "message", "user": "UBOT", "text": "my own post", "ts": "3.3", "channel": "C1", "channel_type": "channel"})
	f.event(t, map[string]any{"type": "reaction_added", "user": "U9", "reaction": "ticket", "item_user": "U1", "item": map[string]any{"type": "message", "channel": "C1", "ts": "111.222"}})
	if got := strings.Join(f.bus.EventTypes(), ","); got != "message_im,message_posted,reaction_added" {
		t.Fatalf("types = %s", got)
	}
	reply := f.bus.Find("message_posted")
	if reply.Attributes[eventattr.ThreadReply] != true || reply.Attributes[eventattr.AutoFollowUp] != false || reply.Source.ThreadID != "1.0" {
		t.Fatalf("thread reply = %+v", reply)
	}
	re := f.bus.Find("reaction_added")
	if re.Attributes[eventattr.Emoji] != "ticket" || re.Attributes[eventattr.ItemUserID] != "U1" || re.Attributes[eventattr.ItemUserName] != "alice" ||
		!strings.Contains(re.Text, "the build is broken") || !strings.Contains(re.Text, "see logs") {
		t.Fatalf("reaction = %+v", re)
	}
}

func TestInteractionResolvesInbox(t *testing.T) {
	f := newFixture(t)
	f.store.Identities["slack|U1"] = core.ExternalIdentity{UserID: "usr_1", Provider: "slack", ExternalID: "U1"}
	f.store.WorkItems["wi_1"] = core.WorkItem{ID: "wi_1", FactoryID: "fac_1"}
	f.store.Inbox = []core.InboxItem{{ID: "inb_1", FactoryID: "fac_1", WorkItemID: "wi_1", Kind: core.InboxApproval}}
	msg := BuildMessage(core.OutboundMessage{WorkItemID: "wi_1", Kind: "approval_request", Text: "Approve **plan**?", Options: []string{"approve", "reject"},
		Source: core.SourceRef{Channel: "C1", ThreadID: "1.0"}})
	actions := msg.Blocks[len(msg.Blocks)-1]["elements"].([]map[string]any)
	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions", "api_app_id": "A1", "team": map[string]any{"id": "T1"}, "user": map[string]any{"id": "U1", "username": "alice"},
		"response_url": f.srv.URL + "/respond",
		"message":      map[string]any{"ts": "5.5", "text": "Approve plan?", "blocks": []any{map[string]any{"type": "section"}, map[string]any{"type": "actions"}}},
		"actions":      []any{map[string]any{"action_id": actions[0]["action_id"], "value": actions[0]["value"]}},
	})
	body := []byte(url.Values{"payload": {string(payload)}}.Encode())
	rec := f.send(t, InteractionsPath, "application/x-www-form-urlencoded", body, signingSecret, time.Now())
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.actions) != 1 || f.actions[0] != "inb_1|approve|usr_1" {
		t.Fatalf("actions = %v", f.actions)
	}
	resp := f.api.get("respond")
	if len(resp) != 1 || resp[0]["replace_original"] != true {
		t.Fatalf("response_url calls = %v", resp)
	}
	blocks := resp[0]["blocks"].([]any)
	if len(blocks) != 2 || blocks[1].(map[string]any)["type"] != "context" {
		t.Fatalf("buttons not replaced: %v", blocks)
	}
}

func TestResponder(t *testing.T) {
	f := newFixture(t)
	r := NewResponder(ResponderOptions{Store: f.store, Secrets: forgetest.NewSecrets(map[string]string{"SLACK_BOT": "xoxb-test"}), APIBaseURL: f.srv.URL})
	err := r.Reply(context.Background(), "team_1", core.OutboundMessage{WorkItemID: "wi_1", FactoryID: "fac_1", Kind: "question", Text: "See [PR](https://x/pr/1)", Options: []string{"yes", "no"},
		Source: core.SourceRef{Provider: "slack", Channel: "C1", ThreadID: "1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	posts := f.api.get("chat.postMessage")
	if len(posts) != 1 || posts[0]["channel"] != "C1" || posts[0]["thread_ts"] != "1.0" {
		t.Fatalf("posts = %v", posts)
	}
	blocks := posts[0]["blocks"].([]any)
	section := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	if !strings.Contains(section, "<https://x/pr/1|PR>") {
		t.Fatalf("mrkdwn = %q", section)
	}
	if blocks[1].(map[string]any)["type"] != "actions" {
		t.Fatal("no buttons")
	}
}

func TestMrkdwn(t *testing.T) {
	got := Mrkdwn("# Title\n**bold** & <tag> [l](https://e.com)")
	want := "*Title*\n*bold* &amp; &lt;tag&gt; <https://e.com|l>"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestOtherFactoryAppMarkedSelf(t *testing.T) {
	f := newFixture(t)
	f.store.Integrations = append(f.store.Integrations, core.Integration{ID: "int_s2", TeamID: "team_1", Provider: "slack",
		FactoryID: "fac_2",
		Config:    map[string]string{"team_id": "T1", "api_app_id": "A2", "bot_user_id": "UBOT2"}})
	f.event(t, map[string]any{"type": "message", "user": "UBOT2", "bot_id": "B2", "app_id": "A2", "text": "status update", "ts": "5.5", "channel": "C1", "channel_type": "channel"})
	ev := f.bus.Find("message_posted")
	if ev == nil {
		t.Fatal("no event")
	}
	if ev.Attributes[eventattr.ActorIsSelf] != true || !ev.Actor.IsBot || ev.Attributes[eventattr.ChannelName] != "engineering" {
		t.Fatalf("attrs = %v actor = %+v", ev.Attributes, ev.Actor)
	}
	f.event(t, map[string]any{"type": "message", "user": "U9", "text": "human", "ts": "6.6", "channel": "C1", "channel_type": "channel"})
	for _, e := range f.bus.Events() {
		if e.Text == "human" && e.Attributes[eventattr.ActorIsSelf] != false {
			t.Fatalf("human marked self: %v", e.Attributes)
		}
	}
}

func TestBuildMessageCarriesInboxID(t *testing.T) {
	msg := BuildMessage(core.OutboundMessage{WorkItemID: "wi_1", InboxID: "inb_9", Kind: "approval_request", Text: "ok?", Options: []string{"approve"}})
	actions := msg.Blocks[len(msg.Blocks)-1]["elements"].([]map[string]any)
	var v actionValue
	if err := json.Unmarshal([]byte(actions[0]["value"].(string)), &v); err != nil || v.InboxID != "inb_9" || v.WorkItemID != "wi_1" {
		t.Fatalf("value %v err %v", actions[0]["value"], err)
	}
}

// TestEmailLinkExcludesGuestsAndOutsiders checks the limits of the email
// link: a guest, a user of another workspace and a user of a shared channel
// are never linked by email, even with the same email address.
func TestEmailLinkExcludesGuestsAndOutsiders(t *testing.T) {
	f := newFixture(t)
	f.store.Integrations[0].Config[ConfigLinkByEmail] = "true"
	for i, u := range []string{"U3", "U4", "U5"} {
		f.event(t, map[string]any{"type": "app_mention", "user": u, "text": "<@UBOT> hi", "ts": fmt.Sprintf("200.%d", i), "channel": "C1"})
		if _, err := f.store.FindExternalIdentity(context.Background(), "slack", u); err == nil {
			t.Errorf("%s was linked by email", u)
		}
	}
	if len(f.bus.Events()) != 0 {
		t.Fatal("an excluded user started work")
	}
}

// TestResponderSelectsFactoryRecord checks the selection rule:
// the record of the factory wins over the team record for the
// workspace of the source, and no record for the workspace is an error that
// names it.
func TestResponderSelectsFactoryRecord(t *testing.T) {
	f := newFixture(t)
	f.store.Integrations = append(f.store.Integrations, core.Integration{ID: "int_team", TeamID: "team_1", Provider: "slack",
		SecretRef: "SLACK_TEAM", Config: map[string]string{"team_id": "T1"}})
	r := NewResponder(ResponderOptions{Store: f.store, Secrets: forgetest.NewSecrets(nil)})
	ctx := context.Background()
	src := core.SourceRef{Provider: "slack", Channel: "C1", ThreadID: "1.0", Workspace: "T1"}
	if in, err := r.integration(ctx, "team_1", "fac_1", "", src); err != nil || in.ID != "int_s" {
		t.Fatalf("factory record: %v %v", in, err)
	}
	if in, err := r.integration(ctx, "team_1", "fac_9", "", src); err != nil || in.ID != "int_team" {
		t.Fatalf("team record: %v %v", in, err)
	}
	src.Workspace = "T9"
	if _, err := r.integration(ctx, "team_1", "fac_1", "", src); err == nil || !strings.Contains(err.Error(), `"T9"`) {
		t.Fatalf("no match: %v", err)
	}
}
