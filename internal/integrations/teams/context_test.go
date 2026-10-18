package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// graphAPI is a Microsoft Graph fake: the token endpoint, a channel message
// and its replies in pages.
type graphAPI struct {
	mu       sync.Mutex
	srv      *httptest.Server
	tokens   []string // scope and tenant of each token request
	requests []string // path of each Graph request
	auth     []string
	status   int // status of the message request, when not zero
	root     string
	pages    [][]string        // the reply pages, each a list of message JSON
	nextHost string            // host of the nextLink of the first page (default: this server)
	lists    map[string]string // the JSON body of /channels and /members, for the name resolver
	listCall map[string]int
}

func newGraphAPI(t *testing.T) *graphAPI {
	t.Helper()
	g := &graphAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		g.mu.Lock()
		g.tokens = append(g.tokens, r.PathValue("tenant")+" "+r.Form.Get("scope"))
		g.mu.Unlock()
		if r.Form.Get("client_secret") != "pw" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"access_token":"graph-token","expires_in":3600}`)
	})
	mux.HandleFunc("GET /v1.0/teams/{team}/", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.requests = append(g.requests, r.URL.RequestURI())
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		if g.status != 0 {
			http.Error(w, `{"error":{"code":"Forbidden"}}`, g.status)
			return
		}
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/channels") || strings.HasSuffix(p, "/members"):
			if g.listCall == nil {
				g.listCall = map[string]int{}
			}
			kind := p[strings.LastIndex(p, "/")+1:]
			g.listCall[kind]++
			body, ok := g.lists[r.PathValue("team")+"/"+kind]
			if !ok {
				body = g.lists[kind]
			}
			fmt.Fprint(w, body)
		case strings.HasSuffix(p, "/replies"):
			page := 0
			fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
			next := ""
			if page+1 < len(g.pages) {
				host := g.srv.URL
				if g.nextHost != "" && page == 0 {
					host = g.nextHost
				}
				next = fmt.Sprintf(`,"@odata.nextLink":"%s%s?page=%d"`, host, p, page+1)
			}
			var items []string
			if page < len(g.pages) {
				items = g.pages[page]
			}
			fmt.Fprintf(w, `{"value":[%s]%s}`, strings.Join(items, ","), next)
		default:
			fmt.Fprint(w, g.root)
		}
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *graphAPI) handler(t *testing.T, cfg map[string]string) (*Handler, *forgetest.Store) {
	t.Helper()
	rec := map[string]string{"team_id": graphTeam, "app_id": appID}
	for k, v := range cfg {
		rec[k] = v
	}
	st := forgetest.NewStore()
	st.Integrations = []core.Integration{{ID: "int_t", TeamID: "team_1", Provider: IntegrationProvider, SecretRef: "TEAMS_PW", Config: rec}}
	return NewHandler(Options{Store: st, Secrets: forgetest.NewSecrets(map[string]string{"TEAMS_PW": "pw"}),
		LoginURL: g.srv.URL, GraphURL: g.srv.URL + "/v1.0", HTTPClient: g.srv.Client()}), st
}

func threadEvent() *core.Event {
	return &core.Event{TeamID: "team_1", Provider: "teams",
		Source:     core.SourceRef{Provider: "teams", Kind: "thread", Channel: channel, ThreadID: "1700000000000"},
		Attributes: map[string]any{eventattr.TeamID: graphTeam, eventattr.TenantID: tenant}}
}

const rootMessage = `{"id":"1700000000000","messageType":"message","createdDateTime":"2026-11-01T10:00:00Z",
	"from":{"user":{"id":"aad-alice","displayName":"Alice"}},
	"body":{"contentType":"html","content":"<p><at id=\"0\">Factory</at> the deploy broke &amp; it&#39;s red</p>"},
	"attachments":[{"name":"log.txt","contentType":"reference","contentUrl":"https://sharepoint.test/log.txt"}]}`

func TestEventContextThread(t *testing.T) {
	g := newGraphAPI(t)
	g.root = rootMessage
	g.pages = [][]string{
		// Graph lists the newest reply first, and pages hold older ones later.
		{`{"id":"1700000000002","messageType":"message","createdDateTime":"2026-11-01T10:02:00Z","from":{"application":{"displayName":"Factory"}},"body":{"contentType":"text","content":"On it."}}`,
			`{"id":"1700000000003","messageType":"systemEventMessage","createdDateTime":"2026-11-01T10:03:00Z","body":{"content":"member added"}}`},
		{`{"id":"1700000000001","messageType":"message","createdDateTime":"2026-11-01T10:01:00Z","from":{"user":{"id":"aad-bob","displayName":"Bob"}},"body":{"contentType":"html","content":"<p>Same here</p>"}}`,
			`{"id":"1700000000004","messageType":"message","createdDateTime":"2026-11-01T10:04:00Z","deletedDateTime":"2026-11-01T10:05:00Z","body":{"content":"oops"}}`},
	}
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	files, omitted, err := h.EventContext(context.Background(), threadEvent())
	if err != nil || len(omitted) != 0 || len(files) != 1 || files[0].Path != "thread.json" {
		t.Fatalf("files=%+v omitted=%+v err=%v", files, omitted, err)
	}
	var got []ThreadEntry
	if err := json.Unmarshal(files[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	// The system message and the deleted message are left out.
	if len(got) != 3 {
		t.Fatalf("entries = %+v", got)
	}
	// Oldest first, root first.
	if got[0].ID != "1700000000000" || got[1].User != "Bob" || got[2].ID != "1700000000002" || got[1].ID != "1700000000001" {
		t.Fatalf("order = %+v", got)
	}
	if got[0].Text != "the deploy broke & it's red" || got[0].User != "Alice" || got[0].UserID != "aad-alice" || got[0].Bot {
		t.Fatalf("root = %+v", got[0])
	}
	if len(got[0].Attachments) != 1 || got[0].Attachments[0].Name != "log.txt" {
		t.Fatalf("attachments = %+v", got[0].Attachments)
	}
	if !got[2].Bot || got[2].User != "Factory" || got[2].Text != "On it." {
		t.Fatalf("bot reply = %+v", got[2])
	}
	// The token comes from the tenant of the record, with the Graph scope.
	if len(g.tokens) != 1 || g.tokens[0] != tenant+" "+GraphScope {
		t.Fatalf("token requests = %v", g.tokens)
	}
	for _, a := range g.auth {
		if a != "Bearer graph-token" {
			t.Fatalf("authorization = %q", a)
		}
	}
	wantBase := "/v1.0/teams/" + graphTeam + "/channels/" + channel + "/messages/1700000000000"
	if len(g.requests) != 3 || g.requests[0] != wantBase || !strings.HasPrefix(g.requests[1], wantBase+"/replies?") || !strings.Contains(g.requests[2], "page=1") {
		t.Fatalf("requests = %v", g.requests)
	}
}

func TestEventContextDoesNotFollowALinkToAnotherHost(t *testing.T) {
	g := newGraphAPI(t)
	g.root = rootMessage
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request to another host: %s with %q", r.URL, r.Header.Get("Authorization"))
	}))
	defer other.Close()
	g.nextHost = other.URL
	g.pages = [][]string{{}, {`{"id":"9","messageType":"message","createdDateTime":"2026-11-01T11:00:00Z","body":{"content":"x"}}`}}
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	files, _, err := h.EventContext(context.Background(), threadEvent())
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	var got []ThreadEntry
	_ = json.Unmarshal(files[0].Data, &got)
	if len(got) != 1 {
		t.Fatalf("entries = %+v, want only the first message", got)
	}
}

func TestEventContextNeedsAThread(t *testing.T) {
	g := newGraphAPI(t)
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	ev := threadEvent()
	ev.Source.ThreadID = ""
	if files, omitted, err := h.EventContext(context.Background(), ev); files != nil || omitted != nil || err != nil {
		t.Fatalf("files=%v omitted=%v err=%v", files, omitted, err)
	}
	if len(g.requests) != 0 || len(g.tokens) != 0 {
		t.Fatalf("calls: %v %v", g.requests, g.tokens)
	}
}

func TestEventContextGraphErrorsLeaveTheContextOut(t *testing.T) {
	g := newGraphAPI(t)
	g.root = rootMessage
	g.status = http.StatusForbidden
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	files, omitted, err := h.EventContext(context.Background(), threadEvent())
	if err != nil || files != nil || len(omitted) != 1 || omitted[0].Name != "context/thread.json" || !strings.Contains(omitted[0].Reason, "ChannelMessage.Read.Group") {
		t.Fatalf("files=%v omitted=%+v err=%v", files, omitted, err)
	}
}

func TestEventContextNeedsATenant(t *testing.T) {
	g := newGraphAPI(t)
	g.root = rootMessage
	h, _ := g.handler(t, nil) // no tenant_id on the record
	ev := threadEvent()
	delete(ev.Attributes, eventattr.TenantID)
	files, omitted, err := h.EventContext(context.Background(), ev)
	if err != nil || files != nil || len(omitted) != 1 || !strings.Contains(omitted[0].Reason, "tenant_id") {
		t.Fatalf("files=%v omitted=%+v err=%v", files, omitted, err)
	}
	// The tenant of the event is enough.
	g.tokens = nil
	ev.Attributes[eventattr.TenantID] = "tenant-from-event"
	if files, _, err := h.EventContext(context.Background(), ev); err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if len(g.tokens) != 1 || !strings.HasPrefix(g.tokens[0], "tenant-from-event ") {
		t.Fatalf("tokens = %v", g.tokens)
	}
}

func TestEventContextOtherWorkspace(t *testing.T) {
	g := newGraphAPI(t)
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	ev := threadEvent()
	ev.TeamID = "team_other"
	if _, _, err := h.EventContext(context.Background(), ev); err == nil {
		t.Fatal("want an error: the Teams team belongs to another workspace")
	}
	ev = threadEvent()
	ev.Attributes = map[string]any{}
	if _, _, err := h.EventContext(context.Background(), ev); err == nil {
		t.Fatal("want an error without a team ID")
	}
}

func TestEventContextRespectsTheSizeLimit(t *testing.T) {
	g := newGraphAPI(t)
	g.root = rootMessage
	big := strings.Repeat("x", 4000)
	var page []string
	for i := 1; i <= 150; i++ {
		page = append(page, fmt.Sprintf(`{"id":"%d","messageType":"message","createdDateTime":"2026-11-01T10:%02d:%02dZ","body":{"contentType":"text","content":"%d %s"}}`, 1700000000000+i, i/60, i%60, i, big))
	}
	g.pages = [][]string{page}
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	files, omitted, err := h.EventContext(context.Background(), threadEvent())
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if len(files[0].Data) > eventctx.MaxPayloadBytes {
		t.Fatalf("thread.json is %d bytes, over the limit of %d", len(files[0].Data), eventctx.MaxPayloadBytes)
	}
	var got []ThreadEntry
	if err := json.Unmarshal(files[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "1700000000000" || got[len(got)-1].ID != "1700000000150" {
		t.Fatalf("the first message and the newest reply must stay: %s ... %s", got[0].ID, got[len(got)-1].ID)
	}
	if len(got) >= 151 || len(omitted) != 1 || !strings.Contains(omitted[0].Reason, "left out") {
		t.Fatalf("kept %d, omitted %+v", len(got), omitted)
	}
}

func TestEventContextMessageCountLimit(t *testing.T) {
	var entries []ThreadEntry
	for i := 0; i < 300; i++ {
		entries = append(entries, ThreadEntry{ID: fmt.Sprint(i), Text: "m"})
	}
	data, left := fitThread(entries)
	var got []ThreadEntry
	_ = json.Unmarshal(data, &got)
	if len(got) != maxThreadMessages || left != 100 || got[0].ID != "0" || got[len(got)-1].ID != "299" {
		t.Fatalf("kept %d, left %d, first %s, last %s", len(got), left, got[0].ID, got[len(got)-1].ID)
	}
}

// The files of the builder go through eventctx: they are listed in the
// envelope, and the redactor of the run removes secret values.
func TestEventContextThroughBuildAndRedact(t *testing.T) {
	g := newGraphAPI(t)
	g.root = `{"id":"1700000000000","messageType":"message","createdDateTime":"2026-11-01T10:00:00Z","body":{"contentType":"text","content":"the key is sk-secret-123"}}`
	g.pages = [][]string{{}}
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	b := eventctx.Build(context.Background(), "msg_1", threadEvent(), true, []eventctx.Builder{h})
	if b == nil || len(b.Envelope.ContextFiles) != 1 || b.Envelope.ContextFiles[0] != "context/thread.json" {
		t.Fatalf("bundle = %+v", b)
	}
	files := eventctx.Redact(b.Files, func(s string) string { return strings.ReplaceAll(s, "sk-secret-123", "[redacted]") })
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.Path, "context/thread.json") {
			found = true
			if strings.Contains(string(f.Data), "sk-secret-123") || !strings.Contains(string(f.Data), "[redacted]") {
				t.Fatalf("thread.json = %s", f.Data)
			}
		}
	}
	if !found {
		t.Fatal("no thread.json in the bundle")
	}
}
