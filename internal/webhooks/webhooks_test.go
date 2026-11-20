package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/triggers"
)

type fakeStore struct {
	core.Store
	mu         sync.Mutex
	webhooks   map[string]*core.Webhook
	deliveries []*core.WebhookDelivery
}

func (f *fakeStore) GetWebhook(_ context.Context, id string) (*core.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.webhooks[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	c := *w
	return &c, nil
}

func (f *fakeStore) GetFactory(_ context.Context, id string) (*core.Factory, error) {
	return &core.Factory{ID: id, TeamID: "team_1"}, nil
}

func (f *fakeStore) CreateWebhookDelivery(_ context.Context, d *core.WebhookDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *d
	f.deliveries = append(f.deliveries, &c)
	return nil
}

func (f *fakeStore) UpdateWebhookDelivery(_ context.Context, d *core.WebhookDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.deliveries {
		if x.ID == d.ID {
			c := *d
			f.deliveries[i] = &c
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fakeStore) FindWebhookDelivery(_ context.Context, webhookID, deliveryID string) (*core.WebhookDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.deliveries) - 1; i >= 0; i-- {
		d := f.deliveries[i]
		if d.WebhookID == webhookID && d.DeliveryID == deliveryID {
			c := *d
			return &c, nil
		}
	}
	return nil, core.ErrNotFound
}

func (f *fakeStore) all() []core.WebhookDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.WebhookDelivery, len(f.deliveries))
	for i, d := range f.deliveries {
		out[i] = *d
	}
	return out
}

type fakeSecrets struct {
	core.SecretStore
	values map[string]string
	err    error
}

func (s *fakeSecrets) Get(_ context.Context, teamID, factoryID, name string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.values[name]
	if !ok {
		return nil, core.ErrNotFound
	}
	return []byte(v), nil
}

type fakeBus struct {
	mu     sync.Mutex
	events []*core.Event
}

func (b *fakeBus) Publish(_ context.Context, topic string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ev, ok := payload.(*core.Event); ok && topic == core.TopicInboundEvent {
		b.events = append(b.events, ev)
	}
}

func (b *fakeBus) Subscribe(string) (<-chan any, func()) { return make(chan any), func() {} }

func (b *fakeBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

type env struct {
	store   *fakeStore
	secrets *fakeSecrets
	bus     *fakeBus
	h       *Handler
	mux     *http.ServeMux
	now     time.Time
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	e := &env{
		store: &fakeStore{webhooks: map[string]*core.Webhook{
			"wh_tok":  {ID: "wh_tok", FactoryID: "fac_1", Name: "Internal CI", AuthMode: AuthToken, SecretName: "TOK", DeliveryIDHeader: "X-CI-Run-Id", Enabled: true},
			"wh_url":  {ID: "wh_url", FactoryID: "fac_1", Name: "Alerts", AuthMode: AuthURLToken, SecretName: "URLTOK", Enabled: true},
			"wh_gh":   {ID: "wh_gh", FactoryID: "fac_1", Name: "GitHub", AuthMode: AuthSignature, SignatureScheme: SchemeGitHub, SecretName: "GH", Enabled: true},
			"wh_sw":   {ID: "wh_sw", FactoryID: "fac_1", Name: "Svix", AuthMode: AuthSignature, SignatureScheme: SchemeStandardWebhooks, SecretName: "SW", Enabled: true},
			"wh_off":  {ID: "wh_off", FactoryID: "fac_1", Name: "Off", AuthMode: AuthToken, SecretName: "TOK", Enabled: false},
			"wh_nosc": {ID: "wh_nosc", FactoryID: "fac_1", Name: "No secret", AuthMode: AuthToken, SecretName: "MISSING", Enabled: true},
		}},
		// SW is the public Standard Webhooks test vector, not a real secret.
		secrets: &fakeSecrets{values: map[string]string{
			"TOK": "whsec_token-value", "URLTOK": "url-token_1.~", "GH": "gh-secret", "SW": "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
		}},
		bus: &fakeBus{},
		now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return e.now }
	}
	e.h = NewHandler(e.store, e.secrets, e.bus, opts)
	e.mux = http.NewServeMux()
	e.h.Register(e.mux)
	return e
}

func (e *env) do(method, path, body string, h http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

func TestHandlerResponses(t *testing.T) {
	ghBody := `{"action":"opened"}`
	ghHeaders, _ := Sign(SchemeGitHub, []byte("gh-secret"), []byte(ghBody), time.Now(), "gh-delivery-1")
	swNow := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	swHeaders, _ := Sign(SchemeStandardWebhooks, []byte("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"), []byte(`{"a":1}`), swNow.Add(-time.Minute), "msg_1")
	staleSW, _ := Sign(SchemeStandardWebhooks, []byte("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"), []byte(`{"a":1}`), swNow.Add(-6*time.Minute), "msg_2")

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		headers    http.Header
		wantStatus int
		wantReason string // recorded rejection reason ("" = none recorded or accepted)
	}{
		{"token ok", "POST", "/webhooks/w/wh_tok", `{"event":"deploy.finished"}`, bearer("whsec_token-value"), 202, ""},
		{"token lowercase bearer", "POST", "/webhooks/w/wh_tok", `{"x":1}`, http.Header{"Authorization": {"bearer whsec_token-value"}}, 202, ""},
		{"token wrong", "POST", "/webhooks/w/wh_tok", `{}`, bearer("nope"), 401, ReasonAuth},
		{"token missing", "POST", "/webhooks/w/wh_tok", `{}`, nil, 401, ReasonAuth},
		{"token mode on url path", "POST", "/webhooks/w/wh_tok/whsec_token-value", `{}`, nil, 401, ReasonAuth},
		{"url token ok", "POST", "/webhooks/w/wh_url/url-token_1.~", `[1,2]`, nil, 202, ""},
		{"url token wrong", "POST", "/webhooks/w/wh_url/wrong", `{}`, nil, 401, ReasonAuth},
		{"url token without segment", "POST", "/webhooks/w/wh_url", `{}`, bearer("url-token_1.~"), 401, ReasonAuth},
		{"github signature ok", "POST", "/webhooks/w/wh_gh", ghBody, ghHeaders, 202, ""},
		{"github unsigned", "POST", "/webhooks/w/wh_gh", ghBody, nil, 401, ReasonAuth},
		{"standard webhooks ok", "POST", "/webhooks/w/wh_sw", `{"a":1}`, swHeaders, 202, ""},
		{"standard webhooks stale", "POST", "/webhooks/w/wh_sw", `{"a":1}`, staleSW, 401, ReasonAuth},
		{"unknown webhook", "POST", "/webhooks/w/wh_none", `{}`, nil, 404, ""},
		{"disabled webhook", "POST", "/webhooks/w/wh_off", `{}`, bearer("whsec_token-value"), 404, ""},
		{"GET not allowed", "GET", "/webhooks/w/wh_tok", ``, bearer("whsec_token-value"), 405, ""},
		{"invalid json", "POST", "/webhooks/w/wh_tok", `{nope`, bearer("whsec_token-value"), 400, ReasonInvalidJSON},
		{"empty body invalid", "POST", "/webhooks/w/wh_tok", ``, bearer("whsec_token-value"), 400, ReasonInvalidJSON},
		{"scalar json ok", "POST", "/webhooks/w/wh_tok", `"hello"`, bearer("whsec_token-value"), 202, ""},
		{"too large", "POST", "/webhooks/w/wh_tok", `{"a":"` + strings.Repeat("x", 256<<10) + `"}`, bearer("whsec_token-value"), 413, ReasonTooLarge},
		{"missing secret", "POST", "/webhooks/w/wh_nosc", `{}`, bearer("anything"), 401, ReasonAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, Options{})
			rr := e.do(tc.method, tc.path, tc.body, tc.headers)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			ds := e.store.all()
			switch {
			case tc.wantStatus == 202:
				var resp map[string]string
				if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp["delivery_id"] == "" || resp["status"] != "accepted" {
					t.Fatalf("bad 202 body %s", rr.Body.String())
				}
				if len(ds) != 1 || ds[0].Status != StatusAccepted || string(ds[0].Body) != tc.body {
					t.Fatalf("deliveries = %+v", ds)
				}
				if e.bus.count() != 1 {
					t.Fatalf("published %d events", e.bus.count())
				}
			case tc.wantReason != "":
				if len(ds) != 1 || ds[0].Status != StatusRejected || ds[0].Reason != tc.wantReason {
					t.Fatalf("deliveries = %+v", ds)
				}
				if ds[0].Body != nil || ds[0].DeliveryID != "" {
					t.Fatal("rejected delivery must record metadata only")
				}
				if e.bus.count() != 0 {
					t.Fatal("rejected delivery published an event")
				}
			default:
				if len(ds) != 0 || e.bus.count() != 0 {
					t.Fatalf("unexpected deliveries %+v", ds)
				}
			}
			if rr.Code >= 400 && rr.Code != 405 {
				var errBody struct {
					Error struct{ Code, Message string } `json:"error"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &errBody); err != nil || errBody.Error.Code == "" {
					t.Fatalf("bad error body %q", rr.Body.String())
				}
			}
		})
	}
}

func TestHandlerAcceptedEvent(t *testing.T) {
	e := newEnv(t, Options{})
	h := bearer("whsec_token-value")
	h.Set("X-CI-Run-Id", "run-77")
	h.Set("Cookie", "session=abc")
	h.Set("X-Custom-Token", "secret")
	h.Set("User-Agent", "ci/1.0")
	body := `{"event":"deploy.finished","env":"production"}`
	rr := e.do("POST", "/webhooks/w/wh_tok", body, h)
	if rr.Code != 202 {
		t.Fatalf("status %d", rr.Code)
	}
	ev := e.bus.events[0]
	if ev.Provider != "webhook" || ev.Type != "received" || ev.FactoryID != "fac_1" || ev.TeamID != "team_1" || ev.DeliveryID != "run-77" {
		t.Fatalf("bad event %+v", ev)
	}
	if ev.Attributes[triggers.AttrWebhookID] != "wh_tok" || string(ev.Payload) != body {
		t.Fatalf("bad event attrs/payload %+v", ev)
	}
	if ev.Source.Provider != "webhook" || ev.Source.Kind != "delivery" || ev.Source.ExternalID != "run-77" {
		t.Fatalf("bad source %+v", ev.Source)
	}
	d := e.store.all()[0]
	if ev.Attributes[triggers.AttrDeliveryRecordID] != d.ID {
		t.Fatal("delivery record id not linked")
	}
	for _, banned := range []string{"Authorization", "Cookie", "X-Custom-Token"} {
		if _, ok := d.Headers[banned]; ok {
			t.Errorf("credential header %s was recorded", banned)
		}
	}
	if d.Headers["User-Agent"] != "ci/1.0" || d.Headers["X-Ci-Run-Id"] != "run-77" {
		t.Errorf("headers = %v", d.Headers)
	}

	// The event matches a webhook trigger with a payload filter.
	a := core.Automation{ID: "a", FactoryID: "fac_1", Enabled: true, Triggers: []core.Trigger{{
		Provider: "webhook", Event: "received",
		Filter: json.RawMessage(`{"webhook_ids":["wh_tok"],"payload":{"env":{"not_in":["dev"]},"event":["deploy.finished"]}}`),
	}}}
	if !triggers.Match(ev, a) {
		t.Fatal("accepted event should match the automation")
	}
}

func TestPayloadTitle(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{name: "no title field", body: `{"event":"deploy.finished"}`, want: "hook"},
		{name: "title", body: `{"title":"Deploy failed","subject":"x"}`, want: "Deploy failed"},
		{name: "subject", body: `{"subject":" Disk full \nmore"}`, want: "Disk full"},
		{name: "summary", body: `{"summary":"Nightly report"}`, want: "Nightly report"},
		{name: "empty title falls through", body: `{"title":"  ","summary":"S"}`, want: "S"},
		{name: "non-string title", body: `{"title":{"text":"x"}}`, want: "hook"},
		{name: "array body", body: `[1,2]`, want: "hook"},
		{name: "long title is cut", body: `{"title":"` + strings.Repeat("é", 150) + `"}`, want: strings.Repeat("é", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := payloadTitle([]byte(tt.body), "hook"); got != tt.want {
				t.Fatalf("payloadTitle = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandlerDedupe(t *testing.T) {
	e := newEnv(t, Options{})
	post := func(body string, extra http.Header) map[string]string {
		h := bearer("whsec_token-value")
		for k, v := range extra {
			h[k] = v
		}
		rr := e.do("POST", "/webhooks/w/wh_tok", body, h)
		if rr.Code != 202 {
			t.Fatalf("status %d", rr.Code)
		}
		var m map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &m)
		return m
	}
	// Identical bodies without a delivery ID are duplicates (body hash).
	r1 := post(`{"a":1}`, nil)
	r2 := post(`{"a":1}`, nil)
	if r1["status"] != "accepted" || r2["status"] != "duplicate" || r1["delivery_id"] != r2["delivery_id"] || !strings.HasPrefix(r1["delivery_id"], "sha256:") {
		t.Fatalf("hash dedupe: %v %v", r1, r2)
	}
	// Configured header wins over the generic header.
	r3 := post(`{"a":2}`, http.Header{"X-Ci-Run-Id": {"r-1"}, "X-Of-Delivery-Id": {"g-1"}})
	if r3["delivery_id"] != "r-1" {
		t.Fatalf("identity = %v", r3)
	}
	r4 := post(`{"a":3}`, http.Header{"X-Ci-Run-Id": {"r-1"}})
	if r4["status"] != "duplicate" {
		t.Fatalf("same header id must dedupe: %v", r4)
	}
	r5 := post(`{"a":4}`, http.Header{"X-Of-Delivery-Id": {"g-1"}})
	if r5["status"] != "accepted" || r5["delivery_id"] != "g-1" {
		t.Fatalf("generic header: %v", r5)
	}
	if e.bus.count() != 3 {
		t.Fatalf("published %d events, want 3", e.bus.count())
	}
	var dups int
	for _, d := range e.store.all() {
		if d.Status == StatusDeduplicated {
			dups++
			if d.Body != nil || d.Reason != ReasonDuplicate {
				t.Fatalf("duplicate entry must record metadata only: %+v", d)
			}
		}
	}
	if dups != 2 {
		t.Fatalf("dups = %d", dups)
	}
}

func TestHandlerConcurrentDedupe(t *testing.T) {
	e := newEnv(t, Options{RateLimit: -1})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.do("POST", "/webhooks/w/wh_tok", `{"same":true}`, bearer("whsec_token-value"))
		}()
	}
	wg.Wait()
	if e.bus.count() != 1 {
		t.Fatalf("published %d events, want exactly 1", e.bus.count())
	}
}

func TestDeliveryIdentityProviderHeader(t *testing.T) {
	gh := &core.Webhook{AuthMode: AuthSignature, SignatureScheme: SchemeGitHub}
	h := http.Header{}
	h.Set("X-GitHub-Delivery", "gh-1")
	h.Set(SenderDeliveryIDHeader, "generic")
	if got := DeliveryIdentity(gh, h, nil); got != "gh-1" {
		t.Fatalf("got %q", got)
	}
	tok := &core.Webhook{AuthMode: AuthToken}
	if got := DeliveryIdentity(tok, h, nil); got != "generic" {
		t.Fatalf("token webhook must ignore provider header: %q", got)
	}
	gh.DeliveryIDHeader = "X-Mine"
	h.Set("X-Mine", "mine")
	if got := DeliveryIdentity(gh, h, nil); got != "mine" {
		t.Fatalf("configured header must win: %q", got)
	}
	sw := &core.Webhook{AuthMode: AuthSignature, SignatureScheme: SchemeStandardWebhooks}
	if got := DeliveryIdentity(sw, http.Header{"Svix-Id": {"svix-1"}}, nil); got != "svix-1" {
		t.Fatalf("svix id: %q", got)
	}
}

func TestHandlerRateLimits(t *testing.T) {
	e := newEnv(t, Options{RateLimit: 3, AuthFailureLimit: 2})
	for i := 0; i < 3; i++ {
		if rr := e.do("POST", "/webhooks/w/wh_tok", `{"i":`+string(rune('0'+i))+`}`, bearer("whsec_token-value")); rr.Code != 202 {
			t.Fatalf("delivery %d: %d", i, rr.Code)
		}
	}
	e.now = e.now.Add(20 * time.Second)
	rr := e.do("POST", "/webhooks/w/wh_tok", `{"i":9}`, bearer("whsec_token-value"))
	if rr.Code != 429 || rr.Header().Get("Retry-After") != "40" {
		t.Fatalf("status %d retry-after %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	// Another webhook has its own budget.
	if rr := e.do("POST", "/webhooks/w/wh_url/url-token_1.~", `{}`, nil); rr.Code != 202 {
		t.Fatalf("other webhook limited: %d", rr.Code)
	}
	// Window resets.
	e.now = e.now.Add(41 * time.Second)
	if rr := e.do("POST", "/webhooks/w/wh_tok", `{"i":10}`, bearer("whsec_token-value")); rr.Code != 202 {
		t.Fatalf("after reset: %d", rr.Code)
	}

	// Failed authentication has a separate budget.
	for i := 0; i < 2; i++ {
		if rr := e.do("POST", "/webhooks/w/wh_gh", `{}`, nil); rr.Code != 401 {
			t.Fatalf("auth failure %d: %d", i, rr.Code)
		}
	}
	rr = e.do("POST", "/webhooks/w/wh_gh", `{}`, nil)
	if rr.Code != 429 || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("auth budget: %d", rr.Code)
	}
}

func TestHandlerSecretStoreError(t *testing.T) {
	e := newEnv(t, Options{})
	e.secrets.err = errors.New("kms down")
	rr := e.do("POST", "/webhooks/w/wh_tok", `{}`, bearer("whsec_token-value"))
	if rr.Code != 500 {
		t.Fatalf("status %d", rr.Code)
	}
	if ds := e.store.all(); len(ds) != 1 || ds[0].Reason != ReasonSecret {
		t.Fatalf("deliveries %+v", ds)
	}
}

func TestHandlerDispatchRecordsRuns(t *testing.T) {
	var got *core.Event
	e := newEnv(t, Options{Dispatch: func(_ context.Context, ev *core.Event) ([]*core.Run, error) {
		got = ev
		return []*core.Run{{ID: "run_1"}, {ID: "run_2"}}, nil
	}})
	rr := e.do("POST", "/webhooks/w/wh_tok", `{"x":1}`, bearer("whsec_token-value"))
	if rr.Code != 202 {
		t.Fatalf("status %d", rr.Code)
	}
	e.h.Wait()
	if got == nil || e.bus.count() != 0 {
		t.Fatal("dispatch not used instead of bus")
	}
	d := e.store.all()[0]
	if len(d.RunIDs) != 2 || d.RunIDs[0] != "run_1" {
		t.Fatalf("run ids = %v", d.RunIDs)
	}
}

func TestIngressURL(t *testing.T) {
	cases := []struct {
		w     core.Webhook
		token string
		want  string
	}{
		{core.Webhook{ID: "wh_1", AuthMode: AuthToken}, "", "https://of.example.test/webhooks/w/wh_1"},
		{core.Webhook{ID: "wh_1", AuthMode: AuthSignature}, "ignored", "https://of.example.test/webhooks/w/wh_1"},
		{core.Webhook{ID: "wh_1", AuthMode: AuthURLToken}, "tok-1", "https://of.example.test/webhooks/w/wh_1/tok-1"},
		{core.Webhook{ID: "wh_1", AuthMode: AuthURLToken}, "", "https://of.example.test/webhooks/w/wh_1/{token}"},
	}
	for _, c := range cases {
		if got := IngressURL("https://of.example.test/", c.w, c.token); got != c.want {
			t.Errorf("IngressURL = %q, want %q", got, c.want)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name   string
		w      core.Webhook
		errSub string
	}{
		{"token default", core.Webhook{SecretName: "S"}, ""},
		{"signature ok", core.Webhook{AuthMode: AuthSignature, SignatureScheme: SchemeVercel, SecretName: "S"}, ""},
		{"signature without scheme", core.Webhook{AuthMode: AuthSignature, SecretName: "S"}, "signatureScheme is required"},
		{"unknown scheme", core.Webhook{AuthMode: AuthSignature, SignatureScheme: "bitbucket", SecretName: "S"}, "unknown signatureScheme"},
		{"scheme on token", core.Webhook{AuthMode: AuthToken, SignatureScheme: SchemeGitHub, SecretName: "S"}, "only valid with authMode signature"},
		{"unknown mode", core.Webhook{AuthMode: "basic", SecretName: "S"}, "unknown authMode"},
		{"secret required", core.Webhook{AuthMode: AuthToken}, "secretName is required"},
		{"delivery header ok", core.Webhook{SecretName: "S", DeliveryIDHeader: "X-CI-Run-Id"}, ""},
		{"delivery header too long", core.Webhook{SecretName: "S", DeliveryIDHeader: "X-" + strings.Repeat("a", 63)}, "at most 64"},
		{"delivery header invalid", core.Webhook{SecretName: "S", DeliveryIDHeader: "X Run"}, "not a valid HTTP header"},
		{"delivery header authorization", core.Webhook{SecretName: "S", DeliveryIDHeader: "authorization"}, "carries credentials"},
		{"delivery header cookie", core.Webhook{SecretName: "S", DeliveryIDHeader: "Cookie"}, "carries credentials"},
		{"delivery header signature", core.Webhook{SecretName: "S", DeliveryIDHeader: "Stripe-Signature"}, "carries credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(tc.w)
			if tc.errSub == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSub) || !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("err = %v, want %q", err, tc.errSub)
			}
		})
	}
}

func TestGenerateSecretAndURLToken(t *testing.T) {
	tok, err := GenerateSecret(AuthURLToken, "")
	if err != nil || !strings.HasPrefix(tok, "whsec_") || !ValidURLToken(tok) {
		t.Fatalf("url token %q %v", tok, err)
	}
	sw, err := GenerateSecret(AuthSignature, SchemeStandardWebhooks)
	if err != nil || !strings.HasPrefix(sw, "whsec_") || len(standardWebhooksKey([]byte(sw))) != 24 {
		t.Fatalf("standard webhooks secret %q %v", sw, err)
	}
	if _, err := GenerateSecret(AuthSignature, SchemeStripe); err == nil {
		t.Fatal("stripe secrets are provider-issued")
	}
	for s, want := range map[string]bool{"abc-._~XYZ09": true, "": false, "a/b": false, "a b": false, "a+b": false} {
		if ValidURLToken(s) != want {
			t.Errorf("ValidURLToken(%q) != %v", s, want)
		}
	}
}

func TestTestFilter(t *testing.T) {
	accepted := core.WebhookDelivery{WebhookID: "wh_1", Status: StatusAccepted, Body: json.RawMessage(`{"status":"failed","env":"prod"}`)}
	cases := []struct {
		name     string
		filter   string
		d        core.WebhookDelivery
		want     FilterTestResult
		wantErr  bool
		reasonIn string
	}{
		{"would trigger", `{"webhook_ids":["wh_1"],"payload":{"status":["failed"]}}`, accepted, FilterTestResult{WouldTrigger: true, WebhookMatched: true, PayloadMatched: true}, false, ""},
		{"no payload filter", `{"webhook_ids":["wh_1"]}`, accepted, FilterTestResult{WouldTrigger: true, WebhookMatched: true, PayloadMatched: true}, false, ""},
		{"payload mismatch", `{"webhook_ids":["wh_1"],"payload":{"env":{"not_in":["prod"]}}}`, accepted, FilterTestResult{WebhookMatched: true}, false, "does not match"},
		{"other webhook", `{"webhook_ids":["wh_2"],"payload":{"status":["failed"]}}`, accepted, FilterTestResult{PayloadMatched: true}, false, "different webhook"},
		{"rejected delivery", `{"webhook_ids":["wh_1"]}`, core.WebhookDelivery{WebhookID: "wh_1", Status: StatusRejected}, FilterTestResult{WebhookMatched: true}, false, "not accepted"},
		{"invalid filter", `{"webhook_ids":["wh_1"],"payload":{"a":"x"}}`, accepted, FilterTestResult{}, true, ""},
		{"missing webhook_ids", `{"payload":{"a":["x"]}}`, accepted, FilterTestResult{}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TestFilter(json.RawMessage(tc.filter), tc.d)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr {
				return
			}
			if got.WouldTrigger != tc.want.WouldTrigger || got.WebhookMatched != tc.want.WebhookMatched || got.PayloadMatched != tc.want.PayloadMatched {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if !strings.Contains(got.Reason, tc.reasonIn) {
				t.Fatalf("reason %q, want containing %q", got.Reason, tc.reasonIn)
			}
		})
	}
}

func TestWebhookActionKey(t *testing.T) {
	if got := ActionKey("wh_1", "sha256:ab"); got != "webhook:wh_1:sha256:ab" {
		t.Fatalf("ActionKey = %q", got)
	}
}
