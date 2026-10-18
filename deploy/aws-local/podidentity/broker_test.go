package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOwnToken = "broker-own-token"
	testKeyID    = "EMULATORKEY"
	testRoleARN  = "arn:aws:sts::000000000000:assumed-role/of-local-server/pod-identity"
)

var testNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

type rig struct {
	b         *broker
	cluster   *httptest.Server
	mu        sync.Mutex
	clusterOK map[string]bool // namespace/sa pairs created in the emulated cluster
	calls     []string        // emulator and cluster calls in order
	realExp   time.Time       // expiration that the emulator returns
	clusterCA []byte          // CA that DescribeCluster returns
	logs      *syncBuffer
}

func (r *rig) record(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{clusterOK: map[string]bool{}, realExp: testNow.Add(time.Hour), logs: &syncBuffer{}}

	// The real cluster: TokenReview.
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" || req.Method != http.MethodPost {
			http.NotFound(w, req)
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+testOwnToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var tr struct {
			Spec struct {
				Token     string   `json:"token"`
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		json.NewDecoder(req.Body).Decode(&tr)
		if len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != podIdentityAudience {
			t.Errorf("TokenReview audiences = %v", tr.Spec.Audiences)
		}
		status := map[string]any{"authenticated": false}
		switch tr.Spec.Token {
		case "allowed", "denied":
			status = map[string]any{"authenticated": true, "audiences": []string{podIdentityAudience},
				"user": map[string]any{"username": "system:serviceaccount:pi-test:pi-" + tr.Spec.Token}}
		case "wrong-audience":
			status = map[string]any{"authenticated": true, "audiences": []string{"https://kubernetes.default.svc"},
				"user": map[string]any{"username": "system:serviceaccount:pi-test:pi-allowed"}}
		case "user-token":
			status = map[string]any{"authenticated": true, "audiences": []string{podIdentityAudience},
				"user": map[string]any{"username": "alice"}}
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"status": status})
	}))
	t.Cleanup(kube.Close)

	// The emulated cluster API (TLS, its own CA).
	r.cluster = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth := req.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(auth, "Bearer k8s-aws-v1.")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || !strings.Contains(string(raw), "X-Amz-SignedHeaders=host%3Bx-k8s-aws-id") ||
			!strings.Contains(string(raw), "Action=GetCallerIdentity") {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		r.record(req.Method + " " + req.URL.Path)
		var body struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		json.NewDecoder(req.Body).Decode(&body)
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case req.URL.Path == "/api/v1/namespaces":
			key := body.Metadata.Name
			if r.clusterOK[key] {
				w.WriteHeader(http.StatusConflict)
				return
			}
			r.clusterOK[key] = true
			w.WriteHeader(http.StatusCreated)
		case len(parts) == 5 && parts[4] == "serviceaccounts":
			key := parts[3] + "/" + body.Metadata.Name
			if r.clusterOK[key] {
				w.WriteHeader(http.StatusConflict)
				return
			}
			r.clusterOK[key] = true
			w.WriteHeader(http.StatusCreated)
		case len(parts) == 7 && parts[6] == "token":
			if !r.clusterOK[parts[3]+"/"+parts[5]] || len(body.Spec.Audiences) != 1 || body.Spec.Audiences[0] != podIdentityAudience {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"token": "sa-token/" + parts[3] + "/" + parts[5]}})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.cluster.Close)
	r.clusterCA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.cluster.Certificate().Raw})

	// The emulator: DescribeCluster and AssumeRoleForPodIdentity.
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth := req.Header.Get("Authorization")
		if !strings.HasPrefix(auth, sigAlgorithm+" Credential="+testKeyID+"/") {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		switch req.URL.Path {
		case "/clusters/of-local":
			r.record("DescribeCluster")
			if !strings.Contains(auth, "/us-east-1/eks/aws4_request") {
				http.Error(w, "bad scope", http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"cluster": map[string]any{
				"name": "of-local", "status": "ACTIVE", "endpoint": r.cluster.URL,
				"certificateAuthority": map[string]any{"data": base64.StdEncoding.EncodeToString(r.clusterCA)},
			}})
		case "/clusters/of-local/assume-role-for-pod-identity":
			r.record("AssumeRoleForPodIdentity")
			if !strings.Contains(auth, "/us-east-1/eks-auth/aws4_request") {
				http.Error(w, "bad scope", http.StatusForbidden)
				return
			}
			var body struct{ Token string }
			json.NewDecoder(req.Body).Decode(&body)
			if body.Token != "sa-token/pi-test/pi-allowed" {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"message": "no association for the service account"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"subject":         map[string]string{"namespace": "pi-test", "serviceAccount": "pi-allowed"},
				"assumedRoleUser": map[string]string{"arn": testRoleARN, "assumeRoleId": "x"},
				"credentials": map[string]any{
					"accessKeyId": "ASIAFAKE", "secretAccessKey": "fake-secret", "sessionToken": "fake-session",
					"expiration": float64(r.realExp.Unix()),
				},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(emu.Close)

	r.b = &broker{
		cluster: "of-local", region: "us-east-1", stsHost: "sts.us-east-1.amazonaws.com",
		emuURL: emu.URL, emuHTTP: http.DefaultClient,
		emuKeys:   func() (awsKeys, error) { return awsKeys{AccessKeyID: testKeyID, SecretAccessKey: "emu-secret"}, nil },
		kubeURL:   kube.URL,
		kubeHTTP:  http.DefaultClient,
		kubeToken: func() (string, error) { return testOwnToken, nil },
		now:       func() time.Time { return testNow },
	}
	log.SetOutput(r.logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return r
}

func (r *rig) get(token string) (int, map[string]string) {
	req := httptest.NewRequest(http.MethodGet, "/v1/credentials", nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	r.b.handleCredentials(w, req)
	var out map[string]string
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestIssue(t *testing.T) {
	r := newRig(t)
	code, out := r.get("allowed")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %v", code, out)
	}
	if out["AccessKeyId"] != "ASIAFAKE" || out["SecretAccessKey"] != "fake-secret" || out["Token"] != "fake-session" {
		t.Fatalf("credentials: %v", out)
	}
	if want := testNow.Add(time.Hour).Format(time.RFC3339); out["Expiration"] != want {
		t.Fatalf("Expiration = %q, want %q", out["Expiration"], want)
	}
	if want := "DescribeCluster POST /api/v1/namespaces POST /api/v1/namespaces/pi-test/serviceaccounts POST /api/v1/namespaces/pi-test/serviceaccounts/pi-allowed/token AssumeRoleForPodIdentity"; strings.Join(r.calls, " ") != want {
		t.Fatalf("calls:\n got %s\nwant %s", strings.Join(r.calls, " "), want)
	}
	if got := r.logs.String(); !strings.Contains(got, "issued ns=pi-test sa=pi-allowed role="+testRoleARN+" expires="+out["Expiration"]) {
		t.Fatalf("log: %q", got)
	}
	// Keys and tokens never reach the log.
	for _, secret := range []string{"fake-secret", "fake-session", "allowed\n", "emu-secret", testOwnToken} {
		if strings.Contains(r.logs.String(), secret) {
			t.Fatalf("log has %q: %s", secret, r.logs.String())
		}
	}
	// A second call finds the namespace and the service account (409) and still works.
	if code, _ := r.get("Bearer allowed"); code != http.StatusOK {
		t.Fatalf("second call: status %d", code)
	}
}

func TestExpiryCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  time.Duration
		real time.Duration
		want time.Duration
	}{
		{"cap applies", 6 * time.Minute, time.Hour, 6 * time.Minute},
		{"shorter real wins", 6 * time.Minute, 2 * time.Minute, 2 * time.Minute},
		{"no cap", 0, time.Hour, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.b.expiryCap = tc.cap
			r.realExp = testNow.Add(tc.real)
			code, out := r.get("allowed")
			if code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			if want := testNow.Add(tc.want).Format(time.RFC3339); out["Expiration"] != want {
				t.Fatalf("Expiration = %q, want %q", out["Expiration"], want)
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		status      int
		code        string
		log         string
	}{
		{"no token", "", http.StatusBadRequest, "InvalidRequest", "refused ns=- sa=- reason=the Authorization header has no token"},
		{"unauthenticated token", "garbage", http.StatusForbidden, "InvalidToken", "refused ns=- sa=- reason=the token is not valid"},
		{"wrong audience", "wrong-audience", http.StatusForbidden, "InvalidToken", "reason=the token does not have the audience pods.eks.amazonaws.com"},
		{"not a service account", "user-token", http.StatusForbidden, "InvalidToken", "reason=the token is not a service account token"},
		{"no association", "denied", http.StatusForbidden, "AccessDenied", "refused ns=pi-test sa=pi-denied reason=no credentials for the service account: no association"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			code, out := r.get(tc.token)
			if code != tc.status || out["code"] != tc.code || out["message"] == "" {
				t.Fatalf("status %d body %v, want %d %s", code, out, tc.status, tc.code)
			}
			if out["AccessKeyId"] != "" {
				t.Fatalf("credentials in an error: %v", out)
			}
			if !strings.Contains(r.logs.String(), tc.log) {
				t.Fatalf("log %q lacks %q", r.logs.String(), tc.log)
			}
		})
	}
}

func TestOtherMethodRefused(t *testing.T) {
	r := newRig(t)
	w := httptest.NewRecorder()
	r.b.handleCredentials(w, httptest.NewRequest(http.MethodPost, "/v1/credentials", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", w.Code)
	}
}

func TestInternalFailuresAre500(t *testing.T) {
	t.Run("TokenReview not allowed", func(t *testing.T) {
		r := newRig(t)
		r.b.kubeToken = func() (string, error) { return "wrong", nil }
		if code, out := r.get("allowed"); code != http.StatusInternalServerError || out["code"] != "InternalFailure" {
			t.Fatalf("status %d body %v", code, out)
		}
	})
	t.Run("no cluster", func(t *testing.T) {
		r := newRig(t)
		r.b.cluster = "missing"
		if code, _ := r.get("allowed"); code != http.StatusInternalServerError {
			t.Fatalf("status %d", code)
		}
	})
}

// The cluster API certificate is verified against the CA of DescribeCluster.
func TestClusterCAIsVerified(t *testing.T) {
	r := newRig(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "another CA"},
		NotBefore: testNow.Add(-time.Hour), NotAfter: testNow.Add(24 * 365 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	r.clusterCA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	code, out := r.get("allowed")
	if code != http.StatusInternalServerError {
		t.Fatalf("status %d body %v: a certificate from another CA must fail", code, out)
	}
	if !strings.Contains(r.logs.String(), "certificate") {
		t.Fatalf("log: %s", r.logs.String())
	}
}

func TestEKSToken(t *testing.T) {
	b := &broker{cluster: "of-local", region: "us-east-1", stsHost: "sts.us-east-1.amazonaws.com", now: func() time.Time { return testNow }}
	tok := b.eksToken(awsKeys{AccessKeyID: "AKID", SecretAccessKey: "s"})
	raw, ok := strings.CutPrefix(tok, "k8s-aws-v1.")
	if !ok {
		t.Fatal("no prefix")
	}
	dec, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(string(dec))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "sts.us-east-1.amazonaws.com" || q.Get("Action") != "GetCallerIdentity" || q.Get("X-Amz-SignedHeaders") != "host;x-k8s-aws-id" ||
		q.Get("X-Amz-Expires") != "60" || q.Get("X-Amz-Signature") == "" {
		t.Fatalf("token URL %s", dec)
	}
}

func TestReadEmulatorKeys(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/emulator_credentials"
	if err := writeFile(path, "[default]\naws_access_key_id = AKID\naws_secret_access_key = SECRET\n"); err != nil {
		t.Fatal(err)
	}
	k, err := readEmulatorKeys(path)
	if err != nil || k.AccessKeyID != "AKID" || k.SecretAccessKey != "SECRET" {
		t.Fatalf("keys %+v, err %v", k, err)
	}
	if err := writeFile(path, "[default]\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := readEmulatorKeys(path); err == nil {
		t.Fatal("empty file accepted")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }

// syncBuffer is a log buffer that the test reads while the HTTP server
// goroutines write to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
