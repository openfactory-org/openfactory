package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

type gql struct {
	mu   sync.Mutex
	ops  []map[string]any
	auth string
}

func (g *gql) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(b, &req)
	g.mu.Lock()
	g.ops = append(g.ops, req)
	g.auth = r.Header.Get("Authorization")
	g.mu.Unlock()
	q := req["query"].(string)
	switch {
	case strings.Contains(q, "commentCreate"):
		fmt.Fprint(w, `{"data":{"commentCreate":{"success":true}}}`)
	case strings.Contains(q, "agentActivityCreate"):
		fmt.Fprint(w, `{"data":{"agentActivityCreate":{"success":true}}}`)
	case strings.Contains(q, "issue(id"):
		fmt.Fprint(w, `{"data":{"issue":{"id":"iss-1","identifier":"ENG-42","number":42,"title":"Bug","url":"https://linear.app/acme/issue/ENG-42",
			"team":{"id":"team-uuid","name":"Engineering"},"project":{"id":"proj-uuid","name":"Payments"},"state":{"id":"st-1","name":"Todo"},
			"labels":{"nodes":[{"id":"l1","name":"factory-ready"}]}}}}`)
	default:
		fmt.Fprint(w, `{"errors":[{"message":"unknown"}]}`)
	}
}

func (g *gql) queries() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, o := range g.ops {
		out = append(out, o["query"].(string))
	}
	return out
}

func setup(t *testing.T) (*Handler, *forgetest.Store, *forgetest.Bus, *gql) {
	api := &gql{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_l", TeamID: "team_1", Provider: "linear", SecretRef: "LINEAR_TOKEN",
		Config: map[string]string{"organization_id": "org-1", "app_user_id": "app-user", "token_type": "oauth", "webhook_secret_ref": "LINEAR_HOOK"}}}
	store.Users = []core.User{{ID: "usr_1", TeamID: "team_1"}}
	store.Identities["linear|user-1"] = core.ExternalIdentity{UserID: "usr_1", Provider: "linear", ExternalID: "user-1"}
	bus := forgetest.NewBus()
	h := NewHandler(Options{
		Store: store, Secrets: forgetest.NewSecrets(map[string]string{"LINEAR_TOKEN": "lin_oauth", "LINEAR_HOOK": "lsec"}), Bus: bus, GraphQLURL: srv.URL,
		Linker: &integrations.Linker{Key: []byte("k"), PublicURL: "https://of.example.com"},
	})
	return h, store, bus, api
}

func deliver(t *testing.T, h *Handler, secret string, p map[string]any) int {
	if _, ok := p["webhookTimestamp"]; !ok {
		p["webhookTimestamp"] = time.Now().UnixMilli()
	}
	p["organizationId"] = "org-1"
	b, _ := json.Marshal(p)
	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(b)))
	req.Header.Set("Linear-Signature", forgeutil.HMACSHA256Hex([]byte(secret), b))
	req.Header.Set("Linear-Delivery", core.NewID("d"))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	h.Drain()
	return rec.Code
}

func TestSignatureAndReplay(t *testing.T) {
	h, _, bus, _ := setup(t)
	if code := deliver(t, h, "bad", map[string]any{"type": "Issue", "action": "create", "data": map[string]any{}}); code != 401 {
		t.Fatalf("bad signature: %d", code)
	}
	if code := deliver(t, h, "lsec", map[string]any{"type": "Issue", "action": "create", "data": map[string]any{}, "webhookTimestamp": time.Now().Add(-5 * time.Minute).UnixMilli()}); code != 401 {
		t.Fatalf("stale: %d", code)
	}
	if len(bus.Events()) != 0 {
		t.Fatal("events published")
	}
}

func TestIssueEvents(t *testing.T) {
	h, _, bus, _ := setup(t)
	data := map[string]any{
		"id": "iss-1", "identifier": "ENG-42", "number": 42, "title": "Bug", "description": "see https://linear.app/acme/profiles/jdoe",
		"teamId": "team-uuid", "team": map[string]any{"id": "team-uuid", "name": "Engineering"}, "stateId": "st-2", "state": map[string]any{"id": "st-2", "name": "In Progress"},
		"assigneeId": "user-9", "creatorId": "user-1",
		"labelIds": []string{"l1", "l2"}, "labels": []any{map[string]any{"id": "l1", "name": "bug"}, map[string]any{"id": "l2", "name": "factory-ready"}},
	}
	deliver(t, h, "lsec", map[string]any{"type": "Issue", "action": "update", "data": data, "actor": map[string]any{"id": "user-1", "type": "user"},
		"updatedFrom": map[string]any{"labelIds": []string{"l1"}, "stateId": "st-1"}})
	if got := strings.Join(bus.EventTypes(), ","); got != "issue_labeled,issue_state_changed" {
		t.Fatalf("types = %s", got)
	}
	ev := bus.Find("issue_labeled")
	if added := ev.Attributes[eventattr.AddedLabels].([]string); len(added) != 1 || added[0] != "factory-ready" {
		t.Fatalf("added = %v", added)
	}
	if ev.Attributes[eventattr.TeamName] != "Engineering" || ev.Attributes[eventattr.IssueNumber] != 42 || ev.Source.Key() != "linear:ENG-42" || ev.Actor.UserID != "usr_1" {
		t.Fatalf("event = %+v", ev)
	}
	if m := ev.Attributes[eventattr.Mentioned].([]string); len(m) != 1 || m[0] != "jdoe" {
		t.Fatalf("mentioned = %v", m)
	}
	// own activity is ignored
	n := len(bus.Events())
	deliver(t, h, "lsec", map[string]any{"type": "Issue", "action": "create", "data": data, "actor": map[string]any{"id": "app-user", "type": "app"}})
	if len(bus.Events()) != n {
		t.Fatal("own activity produced events")
	}
}

func TestCommentEnriched(t *testing.T) {
	h, _, bus, _ := setup(t)
	deliver(t, h, "lsec", map[string]any{"type": "Comment", "action": "create",
		"data": map[string]any{"id": "c1", "body": "please look", "issueId": "iss-1", "userId": "user-1"}})
	ev := bus.Find("comment_created")
	if ev == nil || ev.Attributes[eventattr.ProjectName] != "Payments" || ev.Attributes[eventattr.Author] != "user-1" || ev.Text != "please look" {
		t.Fatalf("comment = %+v", ev)
	}
	if labels := ev.Attributes[eventattr.Labels].([]string); len(labels) != 1 || labels[0] != "factory-ready" {
		t.Fatalf("labels = %v", labels)
	}
}

func TestAgentSessions(t *testing.T) {
	h, _, bus, api := setup(t)
	session := func(creator string) map[string]any {
		return map[string]any{"id": "sess-1", "issueId": "iss-1", "creatorId": creator, "comment": map[string]any{"body": "@factory write a spec"}}
	}
	deliver(t, h, "lsec", map[string]any{"type": "AgentSessionEvent", "action": "created", "agentSession": session("user-1")})
	ev := bus.Find("agent_session_created")
	if ev == nil || ev.Source.ThreadID != "sess-1" || ev.Source.Kind != "agent_session" || ev.Text != "@factory write a spec" || ev.Actor.UserID != "usr_1" {
		t.Fatalf("session = %+v", ev)
	}
	n := len(bus.Events())
	deliver(t, h, "lsec", map[string]any{"type": "AgentSessionEvent", "action": "created", "agentSession": session("stranger")})
	if len(bus.Events()) != n {
		t.Fatal("unlinked session creator started work")
	}
	qs := api.queries()
	if !strings.Contains(qs[len(qs)-1], "agentActivityCreate") {
		t.Fatalf("no authentication prompt: %v", qs)
	}
}

func TestResponder(t *testing.T) {
	h, _, _, api := setup(t)
	ctx := context.Background()
	if err := h.Reply(ctx, "team_1", core.OutboundMessage{Kind: "update", Text: "done", Source: core.SourceRef{Provider: "linear", Kind: "issue", Channel: "iss-1", ExternalID: "ENG-42"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Reply(ctx, "team_1", core.OutboundMessage{Kind: "question", Text: "which?", Source: core.SourceRef{Provider: "linear", Kind: "agent_session", ThreadID: "sess-1"}}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.auth != "Bearer lin_oauth" {
		t.Fatalf("auth = %q", api.auth)
	}
	in0 := api.ops[0]["variables"].(map[string]any)["input"].(map[string]any)
	in1 := api.ops[1]["variables"].(map[string]any)["input"].(map[string]any)
	if in0["issueId"] != "iss-1" || in1["agentSessionId"] != "sess-1" || in1["content"].(map[string]any)["type"] != "elicitation" {
		t.Fatalf("ops = %v", api.ops)
	}
}

func TestOtherFactoryAppMarkedSelf(t *testing.T) {
	h, store, bus, _ := setup(t)
	store.Integrations = append(store.Integrations, core.Integration{ID: "int_l2", TeamID: "team_1", Provider: "linear", FactoryID: "fac_2",
		Config: map[string]string{"organization_id": "org-1", "app_user_id": "other-app"}})
	data := map[string]any{"id": "iss-2", "identifier": "ENG-43", "number": 43, "title": "Bug", "teamId": "team-uuid",
		"team": map[string]any{"id": "team-uuid", "name": "Engineering"}, "creatorId": "other-app"}
	deliver(t, h, "lsec", map[string]any{"type": "Issue", "action": "create", "data": data, "actor": map[string]any{"id": "other-app", "type": "app"}})
	ev := bus.Find("issue_created")
	if ev == nil || ev.Attributes[eventattr.ActorIsSelf] != true || ev.Attributes[eventattr.TeamName] != "Engineering" {
		t.Fatalf("event = %+v", ev)
	}
}
