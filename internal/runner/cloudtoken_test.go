package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestCloudTokenHelper(t *testing.T) {
	var got runnerproto.CloudTokenRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != runnerproto.PathCloudToken {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got.Audience == "denied" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"forbidden"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(runnerproto.CloudTokenResponse{Token: "jwt-abc", ExpiresAt: time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	state := t.TempDir()
	sf, _ := json.Marshal(map[string]string{"control_url": srv.URL, "session_token": "ofs_test", "runner_version": "1.0.0"})
	if err := os.WriteFile(filepath.Join(state, "session.json"), sf, 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runner.CloudToken(context.Background(), state, "sts.amazonaws.com", 900, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "jwt-abc" {
		t.Fatalf("output %q, want only the token", buf.String())
	}
	if got.Audience != "sts.amazonaws.com" || got.TTLSeconds != 900 || auth != "Bearer ofs_test" {
		t.Fatalf("request %+v auth %q", got, auth)
	}

	buf.Reset()
	if err := runner.CloudToken(context.Background(), state, "denied", 0, &buf); err == nil || buf.Len() != 0 {
		t.Fatalf("want an error and no output, got %v %q", err, buf.String())
	}
	if err := runner.CloudToken(context.Background(), state, "", 0, &buf); err == nil {
		t.Fatal("want an error for an empty audience")
	}
	if err := runner.CloudToken(context.Background(), t.TempDir(), "a", 0, &buf); err == nil {
		t.Fatal("want an error without a session file")
	}
	if err := runner.CloudToken(context.Background(), state, "denied", 0, &buf); err != nil && strings.Contains(err.Error(), "ofs_test") {
		t.Fatal("the error shows the session token")
	}
}

func TestCloudTokenHelperGCPFormat(t *testing.T) {
	exp := time.Unix(2000000000, 0).UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(runnerproto.CloudTokenResponse{Token: "jwt-gcp", ExpiresAt: exp})
	}))
	defer srv.Close()
	state := t.TempDir()
	sf, _ := json.Marshal(map[string]string{"control_url": srv.URL, "session_token": "ofs_test", "runner_version": "1.0.0"})
	if err := os.WriteFile(filepath.Join(state, "session.json"), sf, 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runner.CloudTokenAs(context.Background(), state, "aud", 0, runner.CloudTokenFormatGCP, &buf); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Version   int    `json:"version"`
		Success   bool   `json:"success"`
		TokenType string `json:"token_type"`
		IDToken   string `json:"id_token"`
		Exp       int64  `json:"expiration_time"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Version != 1 || !out.Success || out.IDToken != "jwt-gcp" || out.Exp != exp.Unix() || out.TokenType != "urn:ietf:params:oauth:token-type:jwt" {
		t.Fatalf("output %+v", out)
	}
	if err := runner.CloudTokenAs(context.Background(), state, "aud", 0, "xml", &buf); err == nil {
		t.Fatal("want an error for an unknown format")
	}
}
