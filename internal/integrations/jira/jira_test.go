package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

type site struct {
	mu       sync.Mutex
	comments []map[string]any
	paths    []string
	auth     string
}

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s.mu.Lock()
	s.comments = append(s.comments, m)
	s.paths = append(s.paths, r.URL.Path)
	s.auth = r.Header.Get("Authorization")
	s.mu.Unlock()
	w.WriteHeader(201)
	_, _ = w.Write([]byte(`{"id":"1"}`))
}

func setup(t *testing.T) (*Handler, *forgetest.Store, *forgetest.Bus, *site) {
	s := &site{}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_j", TeamID: "team_1", Provider: "jira", SecretRef: "JIRA_TOKEN",
		Config: map[string]string{"base_url": srv.URL, "site": "acme.atlassian.net", "email": "bot@acme.com", "bot_account_id": "bot-acc", "webhook_secret_ref": "JIRA_HOOK",
			"client_key": "ck-1", "shared_secret_ref": "JIRA_SHARED"}}}
	store.Users = []core.User{{ID: "usr_1", TeamID: "team_1"}}
	store.Identities["jira|acc-1"] = core.ExternalIdentity{UserID: "usr_1", Provider: "jira", ExternalID: "acc-1"}
	bus := forgetest.NewBus()
	h := NewHandler(Options{Store: store, Bus: bus,
		Secrets: forgetest.NewSecrets(map[string]string{"JIRA_TOKEN": "api-token", "JIRA_HOOK": "jsec", "JIRA_SHARED": "shared"}),
		Linker:  &integrations.Linker{Key: []byte("k"), PublicURL: "https://of.example.com"},
	})
	return h, store, bus, s
}

func issuePayload(event string) map[string]any {
	return map[string]any{
		"webhookEvent": event,
		"user":         map[string]any{"accountId": "acc-1", "displayName": "Alice"},
		"issue": map[string]any{"id": "100", "key": "ENG-7", "self": "https://acme.atlassian.net/rest/api/3/issue/100",
			"fields": map[string]any{"summary": "Broken login", "labels": []string{"bug"},
				"project": map[string]any{"key": "ENG", "name": "Engineering"}, "status": map[string]any{"id": "10000", "name": "To Do"},
				"description": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "Steps"}}}}}}},
	}
}

func send(t *testing.T, h *Handler, p map[string]any, sign func(*http.Request, []byte)) int {
	b, _ := json.Marshal(p)
	req := httptest.NewRequest(http.MethodPost, WebhookPath+"?user_id=x", strings.NewReader(string(b)))
	sign(req, b)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	h.Drain()
	return rec.Code
}

func hmacSign(secret string) func(*http.Request, []byte) {
	return func(r *http.Request, b []byte) {
		r.Header.Set("X-Hub-Signature", "sha256="+forgeutil.HMACSHA256Hex([]byte(secret), b))
	}
}

func TestSignature(t *testing.T) {
	h, _, bus, _ := setup(t)
	if code := send(t, h, issuePayload("jira:issue_created"), hmacSign("bad")); code != 401 {
		t.Fatalf("code = %d", code)
	}
	if code := send(t, h, issuePayload("jira:issue_created"), func(*http.Request, []byte) {}); code != 401 {
		t.Fatalf("unsigned code = %d", code)
	}
	if len(bus.Events()) != 0 {
		t.Fatal("events published")
	}
}

func TestConnectJWT(t *testing.T) {
	h, _, bus, _ := setup(t)
	jwtSign := func(qsh string) func(*http.Request, []byte) {
		return func(r *http.Request, _ []byte) {
			tok, _ := forgeutil.SignHS256([]byte("shared"), map[string]any{"iss": "ck-1", "qsh": qsh, "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()})
			r.Header.Set("Authorization", "JWT "+tok)
		}
	}
	good := QSH("POST", WebhookPath, url.Values{"user_id": {"x"}})
	if code := send(t, h, issuePayload("jira:issue_created"), jwtSign("wrong")); code != 401 {
		t.Fatalf("bad qsh accepted: %d", code)
	}
	if code := send(t, h, issuePayload("jira:issue_created"), jwtSign(good)); code != 202 {
		t.Fatalf("good jwt rejected: %d", code)
	}
	if bus.Find("issue_created") == nil {
		t.Fatal("no event")
	}
}

func TestIssueEvents(t *testing.T) {
	h, _, bus, _ := setup(t)
	send(t, h, issuePayload("jira:issue_created"), hmacSign("jsec"))
	c := bus.Find("issue_created")
	if c == nil || c.Attributes[eventattr.ProjectKey] != "ENG" || c.Source.Key() != "jira:ENG-7" || c.Text != "Broken login\n\nSteps" || c.Actor.UserID != "usr_1" {
		t.Fatalf("created = %+v", c)
	}
	// The names are event attributes for the filters projects and statuses.
	if c.Attributes[eventattr.ProjectName] != "Engineering" || c.Attributes[eventattr.StatusName] != "To Do" {
		t.Fatalf("names = %v", c.Attributes)
	}
	if bus.Find("issue_labeled") == nil {
		t.Fatal("labels present at creation should emit issue_labeled")
	}
	p := issuePayload("jira:issue_updated")
	p["changelog"] = map[string]any{"items": []any{
		map[string]any{"field": "labels", "fromString": "bug", "toString": "bug factory-ready"},
		map[string]any{"field": "status", "from": "10000", "to": "10001", "toString": "In Progress"},
		map[string]any{"field": "assignee", "to": "bot-acc"},
	}}
	send(t, h, p, hmacSign("jsec"))
	var labeled, status, session *core.Event
	for _, ev := range bus.Events() {
		switch {
		case ev.Type == "issue_labeled" && ev.Attributes[eventattr.AddedLabels].([]string)[0] == "factory-ready":
			labeled = ev
		case ev.Type == "status_changed":
			status = ev
		case ev.Type == "agent_session_created":
			session = ev
		}
	}
	if status != nil && (status.Attributes[eventattr.StatusName] != "In Progress" || status.Attributes[eventattr.ProjectName] != "Engineering") {
		t.Fatalf("status names = %v", status.Attributes)
	}
	if labeled == nil || status == nil || status.Attributes[eventattr.StatusID] != "10001" || session == nil || session.Attributes[eventattr.ProjectKey] != "ENG" {
		t.Fatalf("labeled=%v status=%v session=%v", labeled, status, session)
	}
}

func TestMentionUnlinkedPromptsConnect(t *testing.T) {
	h, _, bus, s := setup(t)
	p := issuePayload("comment_created")
	p["comment"] = map[string]any{"id": "c1", "author": map[string]any{"accountId": "acc-2", "displayName": "Bob"},
		"body": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{
			map[string]any{"type": "mention", "attrs": map[string]any{"id": "bot-acc", "text": "@Factory"}}, map[string]any{"type": "text", "text": " investigate"}}}}}}
	send(t, h, p, hmacSign("jsec"))
	if bus.Find("agent_session_created") != nil {
		t.Fatal("unlinked user started a session")
	}
	cm := bus.Find("comment_created")
	if cm == nil || cm.Text != "@Factory investigate" {
		t.Fatalf("comment = %+v", cm)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.paths) != 1 || s.paths[0] != "/rest/api/3/issue/ENG-7/comment" {
		t.Fatalf("paths = %v", s.paths)
	}
}

func TestResponderADF(t *testing.T) {
	h, _, _, s := setup(t)
	err := h.Reply(context.Background(), "team_1", core.OutboundMessage{Kind: "update", Text: "# Done\nOpened **PR** [#1](https://x/1)\n\n- a\n- b\n\n```go\nx := 1\n```",
		Source: core.SourceRef{Provider: "jira", ExternalID: "ENG-7"}})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@acme.com:api-token"))
	if s.auth != want {
		t.Fatalf("auth = %q", s.auth)
	}
	body := s.comments[0]["body"].(map[string]any)
	content := body["content"].([]any)
	types := []string{}
	for _, c := range content {
		types = append(types, c.(map[string]any)["type"].(string))
	}
	if body["type"] != "doc" || strings.Join(types, ",") != "heading,paragraph,bulletList,codeBlock" {
		t.Fatalf("adf = %v", types)
	}
}

func TestADFText(t *testing.T) {
	raw, _ := json.Marshal(MarkdownToADF("hello **world**\nline2"))
	if got := Text(raw); got != "hello world\nline2" {
		t.Fatalf("round trip = %q", got)
	}
	if m := Mentions(json.RawMessage(`"[~accountid:abc-1] please"`)); len(m) != 1 || m[0] != "abc-1" {
		t.Fatalf("wiki mentions = %v", m)
	}
}

func TestOtherFactoryAccountMarkedSelf(t *testing.T) {
	h, store, bus, _ := setup(t)
	store.Integrations = append(store.Integrations, core.Integration{ID: "int_j2", TeamID: "team_1", Provider: "jira", FactoryID: "fac_2",
		Config: map[string]string{"base_url": "https://other.atlassian.net", "site": "other.atlassian.net", "bot_account_id": "other-bot"}})
	p := issuePayload("jira:issue_created")
	p["user"] = map[string]any{"accountId": "other-bot", "displayName": "Factory B", "accountType": "app"}
	send(t, h, p, hmacSign("jsec"))
	ev := bus.Find("issue_created")
	if ev == nil || ev.Attributes[eventattr.ActorIsSelf] != true || !ev.Actor.IsBot {
		t.Fatalf("event = %+v", ev)
	}
}
