package objectstore

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// credsFixture runs a fake S3 and a fake credentials endpoint. The HTTP
// client sends *.s3.test to the fake S3 and every other host to its real
// address, so the credentials endpoint is reached on 127.0.0.1.
type credsFixture struct {
	s3     *fakeS3
	clk    *clock
	client *http.Client
	rawURL string

	mu       sync.Mutex
	calls    int
	lastAuth string
	lastForm url.Values
	status   int
	expiry   time.Duration // lifetime of issued credentials
}

func newCredsFixture(t *testing.T) *credsFixture {
	t.Helper()
	clk := newClock()
	f := &fakeS3{t: t, bucket: "bkt", creds: awsCreds{"ASIATEMP", "TEMPSECRET", "SESSION"}, region: "eu-north-1", clk: clk, store: newMemStore(clk.Now), pathStyle: true, pageSize: 10}
	s3srv := httptest.NewServer(f)
	t.Cleanup(s3srv.Close)
	s3addr := s3srv.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(addr); strings.HasSuffix(host, "s3.test") {
			addr = s3addr
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	host := "s3.test:" + strings.Split(s3addr, ":")[1]
	return &credsFixture{
		s3: f, clk: clk, client: client, status: http.StatusOK, expiry: time.Hour,
		rawURL: "s3://bkt/p?region=eu-north-1&path_style=true&endpoint=" + url.QueryEscape("http://"+host),
	}
}

func (c *credsFixture) issue() (exp string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.clk.Now().Add(c.expiry).UTC().Format(time.RFC3339), c.status
}

func (c *credsFixture) containerServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.lastAuth = r.Header.Get("Authorization")
		c.mu.Unlock()
		exp, status := c.issue()
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"Code":"AccessDenied","Message":"no association for this service account"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"AccessKeyId": "ASIATEMP", "SecretAccessKey": "TEMPSECRET", "Token": "SESSION", "Expiration": exp,
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/credentials"
}

func (c *credsFixture) stsServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		c.mu.Lock()
		c.lastForm = r.PostForm
		c.mu.Unlock()
		exp, status := c.issue()
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `<ErrorResponse><Error><Code>InvalidIdentityToken</Code><Message>No OpenIDConnect provider found</Message></Error></ErrorResponse>`)
			return
		}
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>`+
			`<AccessKeyId>ASIATEMP</AccessKeyId><SecretAccessKey>TEMPSECRET</SecretAccessKey><SessionToken>SESSION</SessionToken>`+
			`<Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, exp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

func (c *credsFixture) open(t *testing.T, env map[string]string) Store {
	t.Helper()
	s, err := Open(context.Background(), c.rawURL, Options{HTTPClient: c.client, Now: c.clk.Now, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (c *credsFixture) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func writeToken(t *testing.T, path, v string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func putGet(t *testing.T, s Store, key string) {
	t.Helper()
	ctx := context.Background()
	if err := s.Put(ctx, key, bytes.NewReader([]byte("data")), 4, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, _, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "data" {
		t.Fatalf("Get returned %q", b)
	}
}

func TestS3ContainerCredentials(t *testing.T) {
	fx := newCredsFixture(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "pod-token-1")
	s := fx.open(t, map[string]string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     fx.containerServer(t),
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": tokenFile,
	})
	if n := fx.callCount(); n != 0 {
		t.Fatalf("Open fetched credentials %d times; want lazy", n)
	}

	putGet(t, s, "a.txt")
	if n := fx.callCount(); n != 1 {
		t.Fatalf("credential fetches = %d, want 1 (cached)", n)
	}
	if fx.lastAuth != "pod-token-1" {
		t.Fatalf("Authorization = %q, want the token from the file", fx.lastAuth)
	}

	// The kubelet rotates the token file; the next fetch must read it again.
	writeToken(t, tokenFile, "pod-token-2")
	fx.clk.Advance(56 * time.Minute) // 4 minutes left: inside the refresh margin
	putGet(t, s, "b.txt")
	if n := fx.callCount(); n != 2 {
		t.Fatalf("credential fetches = %d, want 2 after expiry", n)
	}
	if fx.lastAuth != "pod-token-2" {
		t.Fatalf("Authorization = %q, want the rotated token", fx.lastAuth)
	}

	// A presigned URL must not outlive its credentials: 40 minutes left is
	// not enough for a 45-minute URL.
	fx.clk.Advance(20 * time.Minute)
	u, err := s.PresignGet(context.Background(), "b.txt", 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n := fx.callCount(); n != 3 {
		t.Fatalf("credential fetches = %d, want 3 for a long presign", n)
	}
	if !strings.Contains(u, "X-Amz-Security-Token=SESSION") {
		t.Fatalf("presigned URL has no session token: %s", u)
	}
	resp, err := fx.client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status %d", resp.StatusCode)
	}
}

func TestS3ContainerCredentialsError(t *testing.T) {
	fx := newCredsFixture(t)
	fx.status = http.StatusForbidden
	s := fx.open(t, map[string]string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": fx.containerServer(t),
		"AWS_CONTAINER_AUTHORIZATION_TOKEN":  "static-token",
	})
	err := s.Put(context.Background(), "x", bytes.NewReader(nil), 0, "")
	if err == nil || !strings.Contains(err.Error(), "status 403") || !strings.Contains(err.Error(), "no association") {
		t.Fatalf("Put error = %v, want the endpoint's 403 message", err)
	}
	if fx.lastAuth != "static-token" {
		t.Fatalf("Authorization = %q", fx.lastAuth)
	}
	// A failed fetch is not cached.
	fx.status = http.StatusOK
	putGet(t, s, "x")
}

func TestS3WebIdentityCredentials(t *testing.T) {
	fx := newCredsFixture(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "sa-jwt")
	env := map[string]string{
		"AWS_ROLE_ARN":                "arn:aws:iam::123456789012:role/factory-runs",
		"AWS_WEB_IDENTITY_TOKEN_FILE": tokenFile,
		"AWS_ROLE_SESSION_NAME":       "server-0",
		"AWS_ENDPOINT_URL_STS":        fx.stsServer(t),
		// Web identity wins over the container endpoint, as in the AWS SDKs.
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials",
	}
	s := fx.open(t, env)
	putGet(t, s, "w.txt")
	putGet(t, s, "w2.txt")
	if n := fx.callCount(); n != 1 {
		t.Fatalf("STS calls = %d, want 1", n)
	}
	f := fx.lastForm
	for k, want := range map[string]string{
		"Action": "AssumeRoleWithWebIdentity", "RoleArn": env["AWS_ROLE_ARN"],
		"RoleSessionName": "server-0", "WebIdentityToken": "sa-jwt", "Version": "2011-06-15",
	} {
		if f.Get(k) != want {
			t.Errorf("form %s = %q, want %q", k, f.Get(k), want)
		}
	}

	fx.status = http.StatusBadRequest
	fx.clk.Advance(time.Hour)
	err := s.Put(context.Background(), "x", bytes.NewReader(nil), 0, "")
	if err == nil || !strings.Contains(err.Error(), "InvalidIdentityToken") {
		t.Fatalf("Put error = %v, want the STS error code", err)
	}
}

func TestS3CredentialSourceSelection(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"no source", map[string]string{}, "s3 credentials missing"},
		{"key id without secret", map[string]string{"AWS_ACCESS_KEY_ID": "a"}, "incomplete"},
		{"secret without key id", map[string]string{"AWS_SECRET_ACCESS_KEY": "b"}, "incomplete"},
		{"role without token file", map[string]string{"AWS_ROLE_ARN": "arn:aws:iam::1:role/x"}, "needs both"},
		{"plain http to another host", map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://example.com/creds"}, "loopback"},
		{"bad scheme", map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "ftp://127.0.0.1/creds"}, "http or https"},
		{"pod identity agent", map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials"}, ""},
		{"pod identity agent IPv6", map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://[fd00:ec2::23]/v1/credentials"}, ""},
		{"ecs relative", map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc"}, ""},
		{"https anywhere", map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "https://creds.example.com/x"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(ctx, "s3://bkt", Options{Getenv: func(k string) string { return tc.env[k] }})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Open error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	// Static keys win over the container endpoint, which is never called.
	fx := newCredsFixture(t)
	s := fx.open(t, map[string]string{
		"AWS_ACCESS_KEY_ID": "ASIATEMP", "AWS_SECRET_ACCESS_KEY": "TEMPSECRET", "AWS_SESSION_TOKEN": "SESSION",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": fx.containerServer(t),
	})
	putGet(t, s, "k")
	if n := fx.callCount(); n != 0 {
		t.Fatalf("container endpoint called %d times with static keys set", n)
	}
}

func TestS3CABundle(t *testing.T) {
	clk := newClock()
	f := &fakeS3{t: t, bucket: "bkt", creds: awsCreds{"AKID", "SECRET", ""}, region: "us-east-1", clk: clk, store: newMemStore(clk.Now), pathStyle: true, pageSize: 10}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "ca.pem")
	certPEM := pemEncodeCert(srv.Certificate().Raw)
	if err := os.WriteFile(bundle, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := "s3://bkt/p?region=us-east-1&path_style=true&endpoint=" + url.QueryEscape(srv.URL)
	env := map[string]string{"AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": "SECRET"}
	open := func() (Store, error) {
		return Open(context.Background(), raw, Options{Now: clk.Now, Getenv: func(k string) string { return env[k] }})
	}

	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "k", bytes.NewReader([]byte("x")), 1, ""); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("Put without the bundle: %v, want a certificate error", err)
	}

	env["AWS_CA_BUNDLE"] = bundle
	s, err = open()
	if err != nil {
		t.Fatal(err)
	}
	putGet(t, s, "k")

	empty := filepath.Join(dir, "empty.pem")
	_ = os.WriteFile(empty, []byte("not a cert"), 0o600)
	env["AWS_CA_BUNDLE"] = empty
	if _, err := open(); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("Open with a bad bundle: %v", err)
	}
	env["AWS_CA_BUNDLE"] = filepath.Join(dir, "missing.pem")
	if _, err := open(); err == nil || !strings.Contains(err.Error(), "AWS_CA_BUNDLE") {
		t.Fatalf("Open with a missing bundle: %v", err)
	}
}

func pemEncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
