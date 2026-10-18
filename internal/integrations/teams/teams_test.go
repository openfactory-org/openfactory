package teams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
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

const (
	appID     = "app-123"
	graphTeam = "11111111-1111-4111-8111-111111111111"
	channel   = "19:abc@thread.tacv2"
	tenant    = "tenant-1"
)

type env struct {
	h     *Handler
	store *forgetest.Store
	bus   *forgetest.Bus
	key   *rsa.PrivateKey
	srv   *httptest.Server

	mu     sync.Mutex
	posted []map[string]any
	paths  []string
	tokens int
}

func newEnv(t *testing.T) *env {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openid", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":"https://api.botframework.com","jwks_uri":"%s/keys"}`, e.srv.URL)
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		jwk := forgeutil.NewJWK("kid1", &key.PublicKey)
		jwk.Endorsements = []string{"msteams"}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwk}})
	})
	mux.HandleFunc("POST /botframework.com/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != appID || r.Form.Get("client_secret") != "pw" || r.Form.Get("scope") != BotConnectorScope {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		e.mu.Lock()
		e.tokens++
		e.mu.Unlock()
		fmt.Fprint(w, `{"access_token":"bot-token","expires_in":3600}`)
	})
	mux.HandleFunc("POST /svc/v3/conversations/{conv}/activities", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		e.mu.Lock()
		e.posted = append(e.posted, m)
		e.paths = append(e.paths, r.PathValue("conv"))
		e.mu.Unlock()
		fmt.Fprint(w, `{"id":"1"}`)
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.store = forgetest.NewStore()
	e.store.Integrations = []core.Integration{{ID: "int_t", TeamID: "team_1", Provider: IntegrationProvider, SecretRef: "TEAMS_PW",
		FactoryID: "fac_1", Config: map[string]string{"team_id": graphTeam, "tenant_id": tenant, "app_id": appID}}}
	e.store.Users = []core.User{{ID: "usr_1", TeamID: "team_1"}}
	e.bus = forgetest.NewBus()
	e.h = NewHandler(Options{
		Store: e.store, Secrets: forgetest.NewSecrets(map[string]string{"TEAMS_PW": "pw"}), Bus: e.bus,
		OpenIDMetadataURL: e.srv.URL + "/openid", LoginURL: e.srv.URL,
		Linker: &integrations.Linker{Key: []byte("k"), PublicURL: "https://of.example.com"},
	})
	return e
}

func (e *env) token(t *testing.T, aud string, exp time.Time) string {
	tok, err := forgeutil.SignRS256(e.key, map[string]any{"kid": "kid1"}, map[string]any{
		"iss": BotFrameworkIssuer, "aud": aud, "exp": exp.Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "serviceurl": e.srv.URL + "/svc",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *env) activity(text string, mention bool, conv, userAAD string) map[string]any {
	a := map[string]any{
		"type": "message", "id": core.NewID("m"), "serviceUrl": e.srv.URL + "/svc", "channelId": "msteams",
		"from":         map[string]any{"id": "29:user", "name": "Alice", "aadObjectId": userAAD},
		"recipient":    map[string]any{"id": "28:" + appID, "name": "Factory"},
		"conversation": map[string]any{"id": conv, "conversationType": "channel", "tenantId": tenant},
		"text":         text,
		"channelData": map[string]any{
			"team":    map[string]any{"id": "19:team@thread.tacv2", "aadGroupId": graphTeam},
			"channel": map[string]any{"id": channel, "name": "Incidents"},
			"tenant":  map[string]any{"id": tenant},
		},
	}
	if mention {
		a["entities"] = []any{map[string]any{"type": "mention", "mentioned": map[string]any{"id": "28:" + appID, "name": "Factory"}, "text": "<at>Factory</at>"}}
	}
	return a
}

func (e *env) post(t *testing.T, auth string, act map[string]any) int {
	b, _ := json.Marshal(act)
	req := httptest.NewRequest(http.MethodPost, MessagesPath, strings.NewReader(string(b)))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	e.h.Register(mux)
	mux.ServeHTTP(rec, req)
	e.h.Drain()
	return rec.Code
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	act := e.activity("<at>Factory</at> hi", true, channel, "aad-1")
	if code := e.post(t, "", act); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code := e.post(t, "Bearer "+e.token(t, "other-app", time.Now().Add(time.Hour)), act); code != 401 {
		t.Fatalf("wrong audience: %d", code)
	}
	if code := e.post(t, "Bearer "+e.token(t, appID, time.Now().Add(-time.Hour)), act); code != 401 {
		t.Fatalf("expired: %d", code)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged, _ := forgeutil.SignRS256(other, map[string]any{"kid": "kid1"}, map[string]any{"iss": BotFrameworkIssuer, "aud": appID, "exp": time.Now().Add(time.Hour).Unix()})
	if code := e.post(t, "Bearer "+forged, act); code != 401 {
		t.Fatalf("forged signature: %d", code)
	}
}

func TestMentionStartsWork(t *testing.T) {
	e := newEnv(t)
	e.store.Identities["teams|aad-1"] = core.ExternalIdentity{UserID: "usr_1", Provider: "teams", ExternalID: "aad-1"}
	auth := "Bearer " + e.token(t, appID, time.Now().Add(time.Hour))
	if code := e.post(t, auth, e.activity("<at>Factory</at> review the PR", true, channel, "aad-1")); code != 200 {
		t.Fatalf("status %d", code)
	}
	if got := strings.Join(e.bus.EventTypes(), ","); got != "app_mention,message_posted" {
		t.Fatalf("types = %s", got)
	}
	ev := e.bus.Find("app_mention")
	if ev.Text != "review the PR" || ev.FactoryID != "fac_1" || ev.Actor.UserID != "usr_1" ||
		ev.Attributes[eventattr.TeamID] != graphTeam || ev.Attributes[eventattr.ChannelID] != channel || ev.Attributes[eventattr.UserID] != "aad-1" {
		t.Fatalf("event = %+v", ev)
	}
	// The names are event attributes for the filters channels and users.
	if ev.Attributes[eventattr.ChannelName] != "Incidents" || ev.Attributes[eventattr.UserName] != "Alice" {
		t.Fatalf("names = %v", ev.Attributes)
	}
	if ev.Source.Channel != channel || ev.Source.ThreadID == "" {
		t.Fatalf("source = %+v", ev.Source)
	}
	// Thread reply without mention continues (subject to the factory setting).
	e.store.FactoryInts["fac_1"] = map[string]map[string]string{"microsoft-teams": {"autoRespondToThreadReplies": "false"}}
	e.post(t, auth, e.activity("more context", false, channel+";messageid=1700", "aad-1"))
	evs := e.bus.Events()
	last := evs[len(evs)-1]
	if last.Type != "message_posted" || last.Source.ThreadID != "1700" || last.Attributes[eventattr.AutoFollowUp] != false {
		t.Fatalf("reply = %+v", last)
	}
	if v, _ := e.store.KVGet(context.Background(), "teams:service_url:"+channel); v != e.srv.URL+"/svc" {
		t.Fatalf("service url not recorded: %q", v)
	}
}

func TestUnlinkedAndCommands(t *testing.T) {
	e := newEnv(t)
	auth := "Bearer " + e.token(t, appID, time.Now().Add(time.Hour))
	e.post(t, auth, e.activity("<at>Factory</at> do it", true, channel, "aad-2"))
	if len(e.bus.Events()) != 0 {
		t.Fatal("unlinked user started work")
	}
	e.post(t, auth, e.activity("<at>Factory</at> sign in", true, channel, "aad-2"))
	e.mu.Lock()
	defer e.mu.Unlock()
	// A thread reply is public: it names the dashboard and has no link URL.
	if len(e.posted) != 2 || strings.Contains(e.posted[1]["text"].(string), "/integrations/link/") || !strings.Contains(e.posted[1]["text"].(string), "https://of.example.com/integrations") {
		t.Fatalf("posted = %v", e.posted)
	}
	if e.tokens != 1 {
		t.Fatalf("bot token fetched %d times, want 1 (cached)", e.tokens)
	}
}

func TestNotConnectedTeam(t *testing.T) {
	e := newEnv(t)
	e.store.Integrations[0].Config["team_id"] = "another"
	e.store.Integrations[0].Config["app_id"] = appID
	e.post(t, "Bearer "+e.token(t, appID, time.Now().Add(time.Hour)), e.activity("<at>Factory</at> hi", true, channel, "aad-1"))
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.posted) != 1 || e.posted[0]["text"] != NotConnectedMessage {
		t.Fatalf("posted = %v", e.posted)
	}
}

func TestResponder(t *testing.T) {
	e := newEnv(t)
	_ = e.store.KVSet(context.Background(), "teams:service_url:"+channel, e.srv.URL+"/svc")
	err := e.h.Reply(context.Background(), "team_1", core.OutboundMessage{FactoryID: "fac_1", Kind: "update", Text: "Opened a PR",
		Source: core.SourceRef{Provider: "teams", Channel: channel, ThreadID: "1700"}})
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.paths) != 1 || e.paths[0] != channel+";messageid=1700" || e.posted[0]["textFormat"] != "markdown" {
		t.Fatalf("paths = %v posted = %v", e.paths, e.posted)
	}
}

func TestCleanText(t *testing.T) {
	if got := CleanText("<at>Factory</at>&nbsp;please <b>fix</b>"); got != "please fix" {
		t.Fatalf("got %q", got)
	}
}

// TestTokenTenant is a regression test: single-tenant bots must get Bot
// Connector tokens from their own Entra tenant (record tenant_id),
// otherwise they can receive messages but never reply.
func TestTokenTenant(t *testing.T) {
	const botTenant = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ten := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/oauth2/v2.0/token")
		mu.Lock()
		hits = append(hits, ten)
		mu.Unlock()
		// The app is single-tenant: botframework.com does not know it.
		if ten != botTenant {
			http.Error(w, `{"error":"unauthorized_client","error_description":"AADSTS700016: Application not found in the directory"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"tok-`+ten+`","expires_in":3600}`)
	}))
	defer srv.Close()
	rec := func(cfg map[string]string) *core.Integration {
		c := map[string]string{"app_id": "app-1"}
		for k, v := range cfg {
			c[k] = v
		}
		return &core.Integration{ID: "int", Provider: Provider, SecretRef: "TEAMS_PW", Config: c}
	}
	tests := []struct {
		name       string
		in         *core.Integration
		wantTok    string
		wantTenant string
		wantErr    bool
	}{
		{"explicit single tenant", rec(map[string]string{"tenant_id": botTenant, "app_type": "SingleTenant"}), "tok-" + botTenant, botTenant, false},
		{"single tenant without tenant_id", rec(map[string]string{"app_type": "SingleTenant"}), "", "", true},
		{"auto fallback to tenant_id", rec(map[string]string{"tenant_id": botTenant}), "tok-" + botTenant, botTenant, false},
		{"multi tenant stays on botframework", rec(map[string]string{"tenant_id": botTenant, "app_type": "MultiTenant"}), "", "", true},
		{"record without app_id", &core.Integration{ID: "int", Provider: Provider, SecretRef: "TEAMS_PW", Config: map[string]string{"team_id": "t"}}, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(Options{LoginURL: srv.URL, Secrets: forgetest.NewSecrets(map[string]string{"TEAMS_PW": "pw"})})
			ts, err := h.tokenSource(context.Background(), tc.in)
			if err == nil {
				var tok string
				tok, err = ts.Token(context.Background())
				if err == nil && (tok != tc.wantTok || ts.TokenTenant() != tc.wantTenant) {
					t.Fatalf("token = %q tenant = %q, want %q %q", tok, ts.TokenTenant(), tc.wantTok, tc.wantTenant)
				}
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
