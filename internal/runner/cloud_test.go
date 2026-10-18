package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// cloudTokenServer answers the cloud-token request with tok-1, tok-2, ...
func cloudTokenServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var auds []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != runnerproto.PathCloudToken {
			http.NotFound(w, r)
			return
		}
		var req runnerproto.CloudTokenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		n++
		auds = append(auds, req.Audience)
		tok := fmt.Sprintf("tok-%d", n)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(runnerproto.CloudTokenResponse{Token: tok, ExpiresAt: time.Now().Add(time.Hour)})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auds...)
	}
}

func newCloudRunner(t *testing.T, url, command string) *runner {
	t.Helper()
	c, err := runnerclient.New(runnerclient.Options{BaseURL: url, RunnerVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetToken("ofs_test")
	return &runner{cfg: Config{StateDir: t.TempDir(), CloudTokenCommand: command}, client: c, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
}

func TestSetupCloudWritesFilesWithoutSessionToken(t *testing.T) {
	srv, auds := cloudTokenServer(t)
	r := newCloudRunner(t, srv.URL, "'/bin/openfactory' runner cloud-token --state-dir '/state'")
	ci := &runnerproto.CloudIdentity{
		AWS: &runnerproto.CloudAWS{RoleARN: "arn:aws:iam::1:role/x", Audience: "sts.amazonaws.com", SessionName: "openfactory-run_1"},
		GCP: &runnerproto.CloudGCP{Audience: "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/q", ProjectNumber: "1"},
	}
	if _, err := r.setupCloud(context.Background(), ci); err != nil {
		t.Fatal(err)
	}
	awsFile := r.cloudEnv["AWS_WEB_IDENTITY_TOKEN_FILE"]
	if b, err := os.ReadFile(awsFile); err != nil || string(b) != "tok-1" {
		t.Fatalf("aws token file %q: %q, %v", awsFile, b, err)
	}
	if r.cloudEnv["AWS_ROLE_ARN"] != "arn:aws:iam::1:role/x" || r.cloudEnv["GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES"] != "1" || r.cloudEnv["OF_CLOUD_TOKEN_COMMAND"] == "" {
		t.Fatalf("env %v", r.cloudEnv)
	}
	cred, err := os.ReadFile(r.cloudEnv["GOOGLE_APPLICATION_CREDENTIALS"])
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		CredentialSource struct {
			Executable struct {
				Command string `json:"command"`
			} `json:"executable"`
		} `json:"credential_source"`
	}
	if err := json.Unmarshal(cred, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.CredentialSource.Executable.Command, "--audience '//iam.googleapis.com/projects/1/") || !strings.HasSuffix(doc.CredentialSource.Executable.Command, "--format gcp") {
		t.Fatalf("gcp command %q", doc.CredentialSource.Executable.Command)
	}
	if strings.Contains(string(cred), "ofs_test") {
		t.Fatal("the credential file holds the session token")
	}
	// GCP uses the executable source, so only the AWS token is fetched now.
	if got := auds(); len(got) != 1 || got[0] != "sts.amazonaws.com" {
		t.Fatalf("audiences %v", got)
	}
	if len(r.cloudFiles) != 1 {
		t.Fatalf("refreshed files %v", r.cloudFiles)
	}
}

// TestCloudTokenFileRefreshedDuringLongTurn shows that the AWS token file
// gets a new token while a turn runs, so a turn longer than the token life
// keeps its cloud access.
func TestCloudTokenFileRefreshedDuringLongTurn(t *testing.T) {
	old := cloudRefreshEvery
	cloudRefreshEvery = 5 * time.Millisecond
	t.Cleanup(func() { cloudRefreshEvery = old })
	srv, _ := cloudTokenServer(t)
	r := newCloudRunner(t, srv.URL, "")
	ci := &runnerproto.CloudIdentity{
		AWS: &runnerproto.CloudAWS{RoleARN: "arn:aws:iam::1:role/x", Audience: "sts.amazonaws.com"},
		GCP: &runnerproto.CloudGCP{Audience: "gcp-aud", ProjectNumber: "1"},
	}
	if _, err := r.setupCloud(context.Background(), ci); err != nil {
		t.Fatal(err)
	}
	// Without a helper command, GCP reads a token file too.
	if len(r.cloudFiles) != 2 {
		t.Fatalf("files %v", r.cloudFiles)
	}
	file := r.cloudEnv["AWS_WEB_IDENTITY_TOKEN_FILE"]
	first, _ := os.ReadFile(file)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.refreshCloud(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, _ := os.ReadFile(file)
		if len(cur) > 0 && string(cur) != string(first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the token file was not refreshed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(r.cfg.StateDir, "cloud", "gcp-subject-token")); err != nil {
		t.Fatal(err)
	}
}
