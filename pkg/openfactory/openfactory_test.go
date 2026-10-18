package openfactory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/pkg/openfactory"
)

// denyWrites allows reads but refuses every write and admin action, and
// records each decision.
type denyWrites struct {
	mu      sync.Mutex
	actions []openfactory.Action
}

func (d *denyWrites) Allow(_ context.Context, p *openfactory.Principal, a openfactory.Action, _ openfactory.Resource) error {
	d.mu.Lock()
	d.actions = append(d.actions, a)
	d.mu.Unlock()
	if p == nil || p.User == nil {
		return openfactory.ErrUnauthorized
	}
	switch a.Category() {
	case openfactory.ActionRead, openfactory.ActionOperate:
		return nil
	}
	return fmt.Errorf("%w: custom policy forbids %s", openfactory.ErrForbidden, a)
}

func (d *denyWrites) seen(a openfactory.Action) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, x := range d.actions {
		if x.Category() == a {
			return true
		}
	}
	return false
}

type memorySink struct {
	mu     sync.Mutex
	events []openfactory.AuditEvent
}

func (m *memorySink) Record(_ context.Context, e openfactory.AuditEvent) {
	m.mu.Lock()
	m.events = append(m.events, e)
	m.mu.Unlock()
}

func (m *memorySink) has(action, outcome string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.Action == action && e.Outcome == outcome {
			return true
		}
	}
	return false
}

func TestComposeServerWithCustomAuthorizerAndAuditSink(t *testing.T) {
	ctx := t.Context()
	cfg := openfactory.DefaultConfig()
	cfg.Dev = true
	cfg.DatabaseURL = "sqlite://:memory:"
	cfg.DataDir = t.TempDir()
	cfg.SandboxRoot = t.TempDir()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	authz := &denyWrites{}
	sink := &memorySink{}
	var out bytes.Buffer
	srv, err := openfactory.New(ctx, cfg,
		openfactory.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		openfactory.WithStdout(&out),
		openfactory.WithVersion("custom-1.0"),
		openfactory.WithAuthorizer(authz),
		openfactory.WithAuditSink(sink),
		openfactory.WithRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /custom/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	key := regexp.MustCompile(`of_[A-Za-z0-9_]+`).FindString(out.String())
	if key == "" {
		t.Fatalf("no dev bootstrap key in %q", out.String())
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	do := func(method, path, body string, bearer bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", ts.URL)
		if bearer {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	// Extra routes are mounted before the dashboard catch-all.
	if code, body := do("GET", "/custom/ping", "", false); code != 200 || body != "pong" {
		t.Fatalf("custom route = %d %q", code, body)
	}

	// Reads pass the custom authorizer; writes are refused even for the admin.
	if code, body := do("GET", "/api/v1/factories", "", true); code != 200 {
		t.Fatalf("list factories = %d %s", code, body)
	}
	code, body := do("POST", "/api/v1/factories", `{"name":"x"}`, true)
	if code != http.StatusForbidden || !strings.Contains(body, "custom policy") {
		t.Fatalf("create factory = %d %s, want 403 from the custom authorizer", code, body)
	}
	if !authz.seen(openfactory.ActionRead) || !authz.seen(openfactory.ActionWrite) {
		t.Fatalf("authorizer not consulted: %v", authz.actions)
	}

	// API-key login is audited through the custom sink.
	payload, _ := json.Marshal(map[string]string{"api_key": key})
	if code, body := do("POST", "/auth/session", string(payload), false); code/100 != 2 {
		t.Fatalf("auth/session = %d %s", code, body)
	}
	if code, body := do("GET", "/api/v1/me", "", false); code != 200 {
		t.Fatalf("me with session cookie = %d %s", code, body)
	}
	if code, _ := do("POST", "/auth/session", `{"api_key":"of_bad_key"}`, false); code != http.StatusUnauthorized {
		t.Fatalf("bad key login = %d", code)
	}
	if !sink.has("auth.session", openfactory.OutcomeSuccess) || !sink.has("auth.session", openfactory.OutcomeFailure) {
		t.Fatalf("audit events = %+v", sink.events)
	}
	if srv.Store() == nil {
		t.Fatal("Store() is nil")
	}

	// An extension gives a team a credential: a team secret and a record
	// that names it. The server has no settings for credentials.
	if _, err := srv.Secrets().Put(ctx, "team_ext", "", "GITHUB_APP_PRIVATE_KEY", openfactory.SecretGeneric, []byte("pem")); err != nil {
		t.Fatal(err)
	}
	rec := &openfactory.Integration{ID: "int_ext", TeamID: "team_ext", Provider: "github",
		Config: map[string]string{"org": "acme", "app_id": "1", "private_key_ref": "GITHUB_APP_PRIVATE_KEY", "app_slug": "acme-factory"}}
	if err := srv.Store().UpsertIntegration(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if v, err := srv.Secrets().Get(ctx, "team_ext", "", "GITHUB_APP_PRIVATE_KEY"); err != nil || string(v) != "pem" {
		t.Fatalf("secret = %q, %v", v, err)
	}
}
