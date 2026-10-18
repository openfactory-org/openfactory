package main

import (
	"bytes"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "token-value-that-must-not-be-logged"

func writeCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgentForwardsCredentials(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"AccessKeyId":"AKIAFAKE","SecretAccessKey":"s3cret-body","Token":"t","Expiration":"2030-01-01T00:00:00Z"}`)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	h, err := newAgentHandler(upstream.URL+"/__pod-identity/", writeCA(t, upstream), log.New(&logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(h)
	defer agent.Close()

	req, _ := http.NewRequest(http.MethodGet, agent.URL+"/v1/credentials", nil)
	req.Header.Set("Authorization", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || !strings.Contains(string(body), "AKIAFAKE") {
		t.Fatalf("%d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if gotAuth != testToken || gotPath != "/__pod-identity/v1/credentials" || gotMethod != http.MethodGet {
		t.Errorf("upstream got auth=%q path=%q method=%q", gotAuth, gotPath, gotMethod)
	}
	if !strings.Contains(logs.String(), "status=200") {
		t.Errorf("log: %q", logs.String())
	}
	if strings.Contains(logs.String(), testToken) || strings.Contains(logs.String(), "s3cret-body") || strings.Contains(logs.String(), "AKIAFAKE") {
		t.Errorf("the log has a secret: %q", logs.String())
	}
}

func TestAgentPassesErrorStatus(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"Forbidden","message":"no association"}`)
	}))
	defer upstream.Close()
	h, err := newAgentHandler(upstream.URL, writeCA(t, upstream), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(h)
	defer agent.Close()
	resp, err := http.Get(agent.URL + "/v1/credentials")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "no association") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestAgentDoesNotTrustUnknownCA(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the request reached an upstream with an untrusted certificate")
	}))
	defer upstream.Close()
	h, err := newAgentHandler(upstream.URL, "", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(h)
	defer agent.Close()
	resp, err := http.Get(agent.URL + "/v1/credentials")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAgentHealthAndMethods(t *testing.T) {
	h, err := newAgentHandler("https://127.0.0.1:1", "", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(h)
	defer agent.Close()
	resp, err := http.Get(agent.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, err = http.Post(agent.URL+"/v1/credentials", "text/plain", nil)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %v %v", resp, err)
	}
	resp.Body.Close()
}

func TestAgentFlagErrors(t *testing.T) {
	if _, err := newAgentHandler("", "", nil); err == nil {
		t.Error("an empty upstream must fail")
	}
	if _, err := newAgentHandler("https://x", filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Error("a missing CA file must fail")
	}
	bad := filepath.Join(t.TempDir(), "bad.crt")
	_ = os.WriteFile(bad, []byte("not pem"), 0o600)
	if _, err := newAgentHandler("https://x", bad, nil); err == nil {
		t.Error("a CA file without a certificate must fail")
	}
}

func TestRunUsage(t *testing.T) {
	var out bytes.Buffer
	if code := run(nil, &out); code != 2 {
		t.Errorf("no command: %d", code)
	}
	if code := run([]string{"nope"}, &out); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	if code := run([]string{"-h"}, &out); code != 0 {
		t.Errorf("-h: %d", code)
	}
	if code := run([]string{"pod-identity-agent", "-upstream", ""}, &out); code != 2 {
		t.Errorf("missing upstream: %d", code)
	}
}
