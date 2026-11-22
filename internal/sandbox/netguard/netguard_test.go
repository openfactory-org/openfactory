package netguard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

func TestAllowed(t *testing.T) {
	restricted := rtcore.EffectiveNetworkPolicy{
		Restricted:   true,
		AllowedHosts: []string{"API.example.com", "registry.npmjs.org", "blocked.example.com", "10.0.0.5"},
		DeniedHosts:  []string{"blocked.example.com", "github.com"},
	}
	always := []string{"github.com", ".openfactory.internal", "*.forge.example"}
	tests := []struct {
		policy rtcore.EffectiveNetworkPolicy
		host   string
		want   bool
	}{
		{rtcore.EffectiveNetworkPolicy{}, "anything.example", true},
		{rtcore.EffectiveNetworkPolicy{DeniedHosts: []string{"bad.example"}}, "bad.example", false},
		{restricted, "api.example.com", true},
		{restricted, "api.example.com:443", true},
		{restricted, "API.EXAMPLE.COM.", true},
		{restricted, "sub.api.example.com", false}, // no implicit subdomains
		{restricted, "blocked.example.com", false}, // deny beats allow
		{restricted, "github.com", true},           // platform host cannot be denied
		{restricted, "control.openfactory.internal", true},
		{restricted, "openfactory.internal", true},
		{restricted, "git.forge.example", true},
		{restricted, "forge.example", false},
		{restricted, "10.0.0.5", true},
		{restricted, "10.0.0.6", false},
		{restricted, "[::1]:80", false},
		{restricted, "", false},
		{rtcore.EffectiveNetworkPolicy{Restricted: true}, "example.com", false}, // empty policy denies all
	}
	for _, tt := range tests {
		p := New(tt.policy, always)
		if got := p.Allowed(tt.host); got != tt.want {
			t.Errorf("Allowed(%q) with %+v = %v, want %v", tt.host, tt.policy, got, tt.want)
		}
	}
}

func TestHTTPForwardAndBlocked(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("hop-by-hop header forwarded")
		}
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, "hello "+r.URL.Path)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	p := New(rtcore.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: []string{"127.0.0.1"}}, nil)
	p.AllowPrivateHosts([]string{"127.0.0.1"})
	var mu sync.Mutex
	var seen []string
	p.OnBlocked = func(h string) { mu.Lock(); seen = append(seen, h); mu.Unlock() }
	addr, err := p.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse("http://" + addr.String())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	req, _ := http.NewRequest("GET", upstream.URL+"/path", nil)
	req.Header.Set("Proxy-Authorization", "secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello /path" || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("forward: %d %q", resp.StatusCode, body)
	}

	resp, err = client.Get("http://localhost:" + u.Port() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "localhost") {
		t.Fatalf("blocked: %d %q", resp.StatusCode, body)
	}
	_, _ = client.Get("http://localhost:" + u.Port() + "/again")
	blocked := p.Blocked()
	if len(blocked) != 1 || blocked[0].Host != "localhost" || blocked[0].Count != 2 {
		t.Fatalf("blocked = %+v", blocked)
	}
	d := p.Diagnostics()
	if len(d) != 1 || d[0].Code != DiagnosticCode || d[0].Data["blocked_host"] != "localhost" || d[0].Message != "Network access to localhost blocked" {
		t.Fatalf("diagnostics = %+v", d)
	}
	mu.Lock()
	if len(seen) != 2 {
		t.Fatalf("OnBlocked calls = %v", seen)
	}
	mu.Unlock()

	// Non-proxy requests are rejected.
	resp, err = http.Get("http://" + addr.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("direct request status %d", resp.StatusCode)
	}
}

func TestConnectTunnel(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secure")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	p := New(rtcore.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: []string{"127.0.0.1"}}, nil)
	p.AllowPrivateHosts([]string{"127.0.0.1"})
	addr, err := p.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse("http://" + addr.String())
	tr := upstream.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(proxyURL)
	client := &http.Client{Transport: tr}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secure" {
		t.Fatalf("tunnel body %q", body)
	}
	// Policy updates apply to new connections.
	p.SetPolicy(rtcore.EffectiveNetworkPolicy{Restricted: true}, nil)
	tr.CloseIdleConnections()
	if _, err := client.Get("https://localhost:" + u.Port()); err == nil {
		t.Fatal("CONNECT to blocked host succeeded")
	}
	if b := p.Blocked(); len(b) != 1 || b[0].Host != "localhost" {
		t.Fatalf("blocked = %+v", b)
	}
}

func TestReportHandlerServesBlockedHosts(t *testing.T) {
	p := New(rtcore.EffectiveNetworkPolicy{Restricted: true}, nil)
	p.recordBlocked("Evil.Example:443")
	p.recordBlocked("evil.example")
	rec := httptest.NewRecorder()
	p.ReportHandler().ServeHTTP(rec, httptest.NewRequest("GET", ReportPath, nil))
	var rep Report
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rep) != nil || len(rep.Blocked) != 1 || rep.Blocked[0].Host != "evil.example" || rep.Blocked[0].Count != 2 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	p.ReportHandler().ServeHTTP(rec, httptest.NewRequest("POST", ReportPath, nil))
	if rec.Code == 200 {
		t.Error("a POST must not work")
	}
}
