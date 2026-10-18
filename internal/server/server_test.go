package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c := config.Defaults()
	c.Dev = true
	c.DataDir = t.TempDir()
	c.DatabaseURL = "sqlite://:memory:"
	c.SandboxRoot = t.TempDir()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return &c
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestServerStartsAndServesProbes(t *testing.T) {
	ctx := t.Context()
	cfg := testConfig(t)
	var out bytes.Buffer
	srv, err := New(ctx, cfg, WithLogger(quietLogger()), WithStdout(&out), WithVersion("test"), WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Not ready before Start.
	if code := get(t, ts.URL+"/readyz", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before start = %d", code)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if !strings.Contains(out.String(), "API key (shown once): of_") {
		t.Fatalf("dev bootstrap key not printed: %q", out.String())
	}
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/": 200, "/api/v1/factories": 401} {
		if code := get(t, ts.URL+path, nil); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}
	var body bytes.Buffer
	if code := get(t, ts.URL+"/metrics", &body); code != 200 {
		t.Fatalf("metrics = %d", code)
	}
	for _, m := range []string{`of_runs{state="queued"} 0`, "of_run_queue_depth 0", "of_workers_connected 0", `of_build_info{version="test"} 1`} {
		if !strings.Contains(body.String(), m) {
			t.Errorf("metrics missing %q:\n%s", m, body.String())
		}
	}
	if strings.Contains(body.String(), "of_executor") {
		t.Errorf("the executor metrics were removed with the executor:\n%s", body.String())
	}
	// A second start on a non-empty store does not bootstrap again.
	out.Reset()
	if err := srv.devBootstrap(context.Background()); err != nil || out.Len() != 0 {
		t.Fatalf("second bootstrap: %v %q", err, out.String())
	}
}

func TestPanicRecovery(t *testing.T) {
	s := &Server{log: quietLogger()}
	h := s.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestParseGitRepo(t *testing.T) {
	cases := map[string]GitLocation{
		"github.com/acme/defs": {"GITHUB", "acme", "defs"},
		"acme/defs":            {"GITHUB", "acme", "defs"},
		"https://gitlab.example.com/grp/sub/defs.git": {"GITLAB", "grp/sub", "defs"},
		"https://dev.azure.com/org/proj/_git/defs":    {"AZURE_DEVOPS", "org/proj", "defs"},
		"https://git.example.com/team/defs":           {"GIT", "team", "defs"},
	}
	for in, want := range cases {
		got, err := ParseGitRepo(in)
		if err != nil || got != want {
			t.Errorf("ParseGitRepo(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := ParseGitRepo("nope"); err == nil {
		t.Error("expected error")
	}
}

// After the stop signal the server reports not ready but keeps accepting new
// connections for OF_SHUTDOWN_DRAIN, and then closes the listener.
func TestServeDrainsBeforeTheListenerCloses(t *testing.T) {
	cfg := testConfig(t)
	cfg.ShutdownDrain = "600ms"
	srv, err := New(t.Context(), cfg, WithLogger(quietLogger()), WithStdout(io.Discard), WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + ln.Addr().String() + "/readyz"
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	// A new connection for each request, as a load balancer opens them.
	hc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 2 * time.Second}
	status := func() (int, error) {
		resp, err := hc.Get(u)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, err := status(); err == nil && code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not get ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := time.Now()
	stop()
	time.Sleep(100 * time.Millisecond)
	if code, err := status(); err != nil || code != http.StatusServiceUnavailable {
		t.Fatalf("during the drain: status %d, err %v, want 503 on a new connection", code, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if d := time.Since(stopped); d < 600*time.Millisecond {
		t.Errorf("Serve returned %v after the stop, before the drain ended", d)
	}
	if _, err := status(); err == nil {
		t.Error("the listener still accepts connections after Serve returned")
	}
}

func get(t *testing.T, u string, body *bytes.Buffer) int {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body != nil {
		_, _ = io.Copy(body, resp.Body)
	}
	return resp.StatusCode
}

// TestRunDataRetention checks that the leader prunes run data older than
// OF_OBJECT_STORE_RETENTION.
func TestRunDataRetention(t *testing.T) {
	ctx := t.Context()
	cfg := testConfig(t)
	cfg.ObjectStoreRetention = "50ms"
	srv, err := New(ctx, cfg, WithLogger(quietLogger()), WithStdout(io.Discard), WithDefinitionSyncInterval(-1), withRetentionInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RunData.PutArtifact(ctx, "run_old", "report.txt", strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		arts, err := srv.RunData.ListArtifacts(ctx, "run_old")
		if err == nil && len(arts) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("artifacts not pruned: %v %v", arts, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWebhookDeliveryRetention checks that the leader removes the body of
// webhook deliveries after the newest 50 and keeps their identity.
func TestWebhookDeliveryRetention(t *testing.T) {
	ctx := t.Context()
	cfg := testConfig(t)
	srv, err := New(ctx, cfg, WithLogger(quietLogger()), WithStdout(io.Discard), WithDefinitionSyncInterval(-1), withRetentionInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 60; i++ {
		d := &core.WebhookDelivery{WebhookID: "wh_ret", DeliveryID: "d" + strconv.Itoa(i), Status: "accepted",
			Body: json.RawMessage(`{"a":1}`), ReceivedAt: now.Add(-time.Duration(i) * time.Second)}
		if err := srv.Store.CreateWebhookDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := srv.Store.ListWebhookDeliveries(ctx, "wh_ret", 100)
		withBody := 0
		for _, d := range list {
			if len(d.Body) > 0 {
				withBody++
			}
		}
		if err == nil && len(list) == 60 && withBody == core.WebhookKeepNewest {
			if _, err := srv.Store.FindWebhookDelivery(ctx, "wh_ret", "d59"); err != nil {
				t.Fatalf("identity of a stripped delivery lost: %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliveries not pruned: %d rows, %d with body, %v", len(list), withBody, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The two /.well-known/ documents for cloud providers need no authentication.
// The issuer is OF_OIDC_ISSUER_URL, and OF_PUBLIC_URL when that is not set.
func TestWellKnownNeedsNoAuthAndUsesIssuerURL(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct{ name, issuerURL, want string }{
		{"default", "", "https://pub.example.test"},
		{"setting", "https://issuer.example.test/", "https://issuer.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.PublicURL = "https://pub.example.test"
			cfg.OIDCIssuerURL = tc.issuerURL
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			srv, err := New(ctx, cfg, WithLogger(quietLogger()), WithStdout(io.Discard), WithDefinitionSyncInterval(-1))
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			var disc bytes.Buffer
			if code := get(t, ts.URL+"/.well-known/openid-configuration", &disc); code != 200 {
				t.Fatalf("discovery = %d", code)
			}
			var doc map[string]any
			if err := json.Unmarshal(disc.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc["issuer"] != tc.want || doc["jwks_uri"] != tc.want+"/.well-known/jwks.json" {
				t.Errorf("discovery = %v, want issuer %s", doc, tc.want)
			}
			var jwks bytes.Buffer
			if code := get(t, ts.URL+"/.well-known/jwks.json", &jwks); code != 200 || !strings.Contains(jwks.String(), `"keys"`) {
				t.Errorf("jwks = %d %s", code, jwks.String())
			}
		})
	}
}
