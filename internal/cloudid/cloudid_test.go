package cloudid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func testIssuer(t *testing.T, seed string) *Issuer {
	t.Helper()
	i, err := NewIssuer([]byte(seed), "https://factory.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	i.SetClock(func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) })
	return i
}

func TestDeterministicKeyAndJWKS(t *testing.T) {
	a, b, c := testIssuer(t, "seed-1"), testIssuer(t, "seed-1"), testIssuer(t, "seed-2")
	if a.KeyID() != b.KeyID() || !a.PublicKey().Equal(b.PublicKey()) {
		t.Fatal("same seed must derive the same key")
	}
	if a.KeyID() == c.KeyID() {
		t.Fatal("different seeds must derive different keys")
	}
	// The JWKS reconstructs the public key.
	k := a.JWKS()["keys"].([]map[string]string)[0]
	x, _ := b64.DecodeString(k["x"])
	y, _ := b64.DecodeString(k["y"])
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(a.PublicKey()) || k["kid"] != a.KeyID() || k["alg"] != "ES256" {
		t.Fatalf("jwks = %v", k)
	}
	if _, err := NewIssuer([]byte("s"), ""); err == nil {
		t.Fatal("empty issuer accepted")
	}
}

func TestRunTokenVerifies(t *testing.T) {
	i := testIssuer(t, "seed")
	r := &core.Run{ID: "run_1", TeamID: "team_1", FactoryID: "fac_1", AgentName: "implement", WorkItemID: "wi_1"}
	tok, err := i.RunToken(r, AWSAudience, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := i.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != "https://factory.example.test" || c.Subject != "team/team_1/factory/fac_1/agent/implement" || c.Audience != AWSAudience || c.RunID != "run_1" || c.Expiry-c.IssuedAt != 3600 {
		t.Fatalf("claims = %+v", c)
	}
	// Signature check uses the JWKS key: tamper with the payload.
	parts := strings.Split(tok, ".")
	parts[1] = b64.EncodeToString([]byte(`{"iss":"https://factory.example.test","sub":"x","exp":9999999999}`))
	if _, err := i.Verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("tampered token verified")
	}
	// Independent verification with the public key.
	parts = strings.Split(tok, ".")
	sig, _ := b64.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(i.PublicKey(), sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("ES256 signature invalid")
	}
}

func TestRunCredentials(t *testing.T) {
	i := testIssuer(t, "seed")
	r := &core.Run{ID: "run_1", TeamID: "team_1", FactoryID: "fac_1", AgentName: "implement"}
	if c, err := i.RunCredentials(r, nil, "/w/.openfactory/cloud"); c != nil || err != nil {
		t.Fatalf("nil providers: %v %v", c, err)
	}
	var cp core.CloudProviders
	if err := json.Unmarshal([]byte(`{"aws":{"roleArn":"arn:aws:iam::123456789012:role/factory"},
		"gcp":{"projectNumber":"123456","workloadIdentityFederationPoolId":"pool","workloadIdentityFederationProviderId":"prov","serviceAccountEmail":"agent@p.iam.gserviceaccount.com"}}`), &cp); err != nil {
		t.Fatal(err)
	}
	c, err := i.RunCredentials(r, &cp, "/w/.openfactory/cloud")
	if err != nil {
		t.Fatal(err)
	}
	if c.Env["AWS_ROLE_ARN"] != "arn:aws:iam::123456789012:role/factory" || c.Env["AWS_WEB_IDENTITY_TOKEN_FILE"] != "/w/.openfactory/cloud/aws-web-identity-token" || c.Env["AWS_ROLE_SESSION_NAME"] != "openfactory-run_1" {
		t.Fatalf("aws env = %v", c.Env)
	}
	awsTok, err := i.Verify(string(c.Files["/w/.openfactory/cloud/aws-web-identity-token"]))
	if err != nil || awsTok.Audience != AWSAudience {
		t.Fatalf("aws token = %+v %v", awsTok, err)
	}
	if c.Env["GOOGLE_APPLICATION_CREDENTIALS"] != "/w/.openfactory/cloud/gcp-credentials.json" {
		t.Fatalf("gcp env = %v", c.Env)
	}
	var ext map[string]any
	if err := json.Unmarshal(c.Files["/w/.openfactory/cloud/gcp-credentials.json"], &ext); err != nil {
		t.Fatal(err)
	}
	aud := "//iam.googleapis.com/projects/123456/locations/global/workloadIdentityPools/pool/providers/prov"
	if ext["type"] != "external_account" || ext["audience"] != aud || ext["token_url"] != "https://sts.googleapis.com/v1/token" ||
		ext["credential_source"].(map[string]any)["file"] != "/w/.openfactory/cloud/gcp-subject-token" ||
		!strings.HasSuffix(ext["service_account_impersonation_url"].(string), "/serviceAccounts/agent@p.iam.gserviceaccount.com:generateAccessToken") {
		t.Fatalf("external account = %v", ext)
	}
	gcpTok, err := i.Verify(string(c.Files["/w/.openfactory/cloud/gcp-subject-token"]))
	if err != nil || gcpTok.Audience != aud {
		t.Fatalf("gcp token = %+v %v", gcpTok, err)
	}
}

func TestWellKnownEndpoints(t *testing.T) {
	i := testIssuer(t, "seed")
	mux := http.NewServeMux()
	i.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var disc map[string]any
	resp, err := http.Get(srv.URL + DiscoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&disc)
	resp.Body.Close()
	if disc["issuer"] != "https://factory.example.test" || disc["jwks_uri"] != "https://factory.example.test/.well-known/jwks.json" {
		t.Fatalf("discovery = %v", disc)
	}
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	resp, err = http.Get(srv.URL + JWKSPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&jwks)
	resp.Body.Close()
	if len(jwks.Keys) != 1 || jwks.Keys[0]["kid"] != i.KeyID() || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("jwks = %v", jwks)
	}
}

func TestPreviousSeedInJWKSAndVerify(t *testing.T) {
	mk := func(seed string, prev ...[]byte) *Issuer {
		i, err := NewIssuer([]byte(seed), "https://factory.example.test/", prev...)
		if err != nil {
			t.Fatal(err)
		}
		i.SetClock(func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) })
		return i
	}
	oldI := mk("old")
	r := &core.Run{ID: "run_1", TeamID: "team_1", FactoryID: "fac_1", AgentName: "implement"}
	oldTok, err := oldI.RunToken(r, AWSAudience, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	rotated := mk("new", []byte("old"), []byte("old"))
	keys := rotated.JWKS()["keys"].([]map[string]string)
	if len(keys) != 2 || keys[0]["kid"] != rotated.KeyID() || keys[1]["kid"] != oldI.KeyID() {
		t.Fatalf("jwks = %v, want the new key first and the old key second", keys)
	}
	// New tokens carry the kid of the current key.
	newTok, _ := rotated.RunToken(r, AWSAudience, time.Hour)
	hdr, _ := b64.DecodeString(strings.Split(newTok, ".")[0])
	if !strings.Contains(string(hdr), rotated.KeyID()) {
		t.Fatalf("header %s does not name the current key", hdr)
	}
	if _, err := rotated.Verify(oldTok); err != nil {
		t.Fatalf("token of the previous key: %v", err)
	}
	// Without the previous seed the old token fails and the JWKS has one key.
	removed := mk("new")
	if _, err := removed.Verify(oldTok); err == nil {
		t.Fatal("old token verified after the previous key was removed")
	}
	if n := len(removed.JWKS()["keys"].([]map[string]string)); n != 1 {
		t.Fatalf("jwks has %d keys", n)
	}
	if _, err := NewIssuer([]byte("new"), "https://x.test", nil); err == nil {
		t.Fatal("empty previous seed accepted")
	}
}
