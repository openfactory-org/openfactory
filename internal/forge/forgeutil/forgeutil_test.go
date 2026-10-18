package forgeutil

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

func TestExtractMentions(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"simple", "hey @alice can you look", []string{"alice"}},
		{"team and bot", "cc @acme/backend and @openfactory[bot].", []string{"acme/backend", "openfactory[bot]"}},
		{"code ignored", "```\n@ignored\n```\nand `@inline` but @real", []string{"real"}},
		{"quote ignored", "> @quoted said\n@me", []string{"me"}},
		{"email not mention", "mail bob@example.com", nil},
		{"dedupe", "@a @A @b", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractMentions(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
	if !HasMention("ping @OpenFactory[bot]", "openfactory") {
		t.Fatal("expected HasMention to match bot suffix")
	}
}

func TestHMAC(t *testing.T) {
	sig := HMACSHA256Hex([]byte("s3cret"), []byte("body"))
	if !VerifyHMACSHA256Hex([]byte("s3cret"), []byte("body"), "sha256="+sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifyHMACSHA256Hex([]byte("other"), []byte("body"), sig) || VerifyHMACSHA256Hex(nil, []byte("body"), sig) {
		t.Fatal("invalid signature accepted")
	}
}

func TestJWTRoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := SignRS256(key, map[string]any{"kid": "k1"}, map[string]any{"iss": "x", "aud": []string{"a", "b"}, "exp": now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	j, err := ParseJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := NewJWK("k1", &key.PublicKey).RSAPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.VerifyRS256(pub); err != nil {
		t.Fatal(err)
	}
	if !j.HasAudience("b") || j.HasAudience("c") || j.HeaderString("kid") != "k1" {
		t.Fatal("claims mismatch")
	}
	if err := j.VerifyTime(now.Add(2*time.Minute), 0); err == nil {
		t.Fatal("expired token accepted")
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if j.VerifyRS256(&other.PublicKey) == nil {
		t.Fatal("wrong key accepted")
	}
	hs, _ := SignHS256([]byte("k"), map[string]any{"exp": now.Add(time.Minute).Unix()})
	hj, _ := ParseJWT(hs)
	if hj.VerifyHS256([]byte("k")) != nil || hj.VerifyHS256([]byte("x")) == nil {
		t.Fatal("hs256 verification wrong")
	}
}

func TestClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "1" {
			t.Errorf("missing auth header")
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"slow down"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Authorize: func(_ context.Context, r *http.Request) error { r.Header.Set("X-Test", "1"); return nil }}
	err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || !ae.Temporary() || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestFormatOutbound(t *testing.T) {
	got := FormatOutbound(core.OutboundMessage{Kind: "approval_request", Text: "Ship the plan?", Options: []string{"approve", "reject"}})
	for _, want := range []string{"**Approval needed**", "Ship the plan?", "- `approve`", "- `reject`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	o, n := SplitRepo("acme/platform/api")
	if o != "acme/platform" || n != "api" {
		t.Fatalf("got %s %s", o, n)
	}
}

func TestSelectIntegration(t *testing.T) {
	org := func(o string) map[string]string { return map[string]string{"org": o} }
	ints := []core.Integration{
		{ID: "team-acme", Config: org("acme")},
		{ID: "fac1-acme", FactoryID: "fac_1", Config: org("ACME")},
		{ID: "fac3-other", FactoryID: "fac_3", Config: org("other")},
	}
	for _, c := range []struct{ factory, owner, want string }{
		{"fac_1", "acme", "fac1-acme"}, // the record of the factory wins, case does not matter
		{"fac_2", "acme", "team-acme"}, // another factory gets the team record
		{"", "Acme", "team-acme"},      // no factory: the team record
	} {
		got, err := SelectIntegration(ints, "github", c.factory, c.owner)
		if err != nil || got.ID != c.want {
			t.Errorf("factory %q owner %q: got %q (%v), want %q", c.factory, c.owner, got.ID, err, c.want)
		}
	}
	for _, fac := range []string{"fac_1", "fac_2", ""} {
		_, err := SelectIntegration(ints, "github", fac, "other")
		if !errors.Is(err, ErrNoIntegration) || !strings.Contains(err.Error(), `"other"`) {
			t.Errorf("factory %q owner other: err = %v, want ErrNoIntegration naming other", fac, err)
		}
	}
	// No owner (Azure DevOps "project/name"): one record of the tier.
	az := []core.Integration{{ID: "a", Config: org("org1")}}
	if got, err := SelectIntegration(az, "azure_devops", "fac_1", ""); err != nil || got.ID != "a" {
		t.Errorf("azure one record: %q %v", got.ID, err)
	}
	az = append(az, core.Integration{ID: "b", Config: org("org2")})
	if _, err := SelectIntegration(az, "azure_devops", "", ""); !errors.Is(err, ErrNoIntegration) {
		t.Errorf("azure two records: err = %v", err)
	}
	for _, c := range []struct{ provider, repo, want string }{
		{"github", "acme/api", "acme"},
		{"gitlab", "acme/platform/api", "acme"},
		{"azure_devops", "Payments/api", ""},
		{"azure_devops", "org1/Payments/api", "org1"},
		{"git", "acme/api", ""},
		{"github", "api", ""},
	} {
		if got := RepoOwner(c.provider, c.repo); got != c.want {
			t.Errorf("RepoOwner(%s, %s) = %q, want %q", c.provider, c.repo, got, c.want)
		}
	}
}

// IntakeVerified picks the team by the record whose secret verifies the
// delivery, whatever the order of the teams in the store.
func TestIntakeVerified(t *testing.T) {
	org := func(o, sec string) map[string]string { return map[string]string{"org": o, "sec": sec} }
	for _, order := range [][]string{{"team_a", "team_b"}, {"team_b", "team_a"}} {
		store := forgetest.NewStore()
		store.Teams = nil
		for _, id := range order {
			store.Teams = append(store.Teams, core.Team{ID: id})
		}
		store.Integrations = []core.Integration{
			{ID: "a", TeamID: "team_a", Provider: "gitlab", Config: org("acme", "sa")},
			{ID: "a_fac", TeamID: "team_a", Provider: "gitlab", FactoryID: "fac_a", Config: org("acme", "sa")},
			{ID: "b_fac", TeamID: "team_b", Provider: "gitlab", FactoryID: "fac_b", Config: org("ACME", "sb")},
			{ID: "c", TeamID: "team_b", Provider: "gitlab", Config: org("other", "sc")},
		}
		with := func(sec string) func(core.Integration) bool {
			return func(in core.Integration) bool { return in.Config["sec"] == sec }
		}
		ctx := context.Background()
		for sec, want := range map[string]string{"sa": "a", "sb": "b_fac"} {
			got, err := IntakeVerified(ctx, store, "gitlab", []string{"acme"}, with(sec))
			if err != nil || got.ID != want {
				t.Errorf("%v %s: got %v (%v), want %s", order, sec, got, err, want)
			}
		}
		if _, err := IntakeVerified(ctx, store, "gitlab", []string{"acme"}, with("sc")); !errors.Is(err, ErrDeliveryUnverified) {
			t.Errorf("%v: secret of another owner: err = %v", order, err)
		}
		if _, err := IntakeVerified(ctx, store, "gitlab", []string{"nobody"}, with("sa")); !errors.Is(err, ErrDeliveryUnverified) || !errors.Is(err, ErrNoIntegration) {
			t.Errorf("%v: unknown owner: err = %v", order, err)
		}
		if _, err := IntakeVerified(ctx, store, "gitlab", []string{"acme"}, func(core.Integration) bool { return true }); !errors.Is(err, ErrDeliveryAmbiguous) {
			t.Errorf("%v: shared secret: err = %v", order, err)
		}
	}
}
