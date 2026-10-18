package cloudid

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func cloudProviders(t *testing.T, js string) *core.CloudProviders {
	t.Helper()
	var cp core.CloudProviders
	if err := json.Unmarshal([]byte(js), &cp); err != nil {
		t.Fatal(err)
	}
	return &cp
}

func TestTokenClaimsAndLimits(t *testing.T) {
	i := testIssuer(t, "seed")
	r := &core.Run{ID: "run_1", TeamID: "team_1", FactoryID: "fac_1", AgentName: "implement", WorkItemID: "wi_1",
		Config: core.AgentConfig{Type: core.AgentImplement}}
	tok, exp, err := i.Token(r, "aud-1", 5*time.Hour, TokenOptions{Creator: "usr_1", WorkerHost: "wrk_1"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := i.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "team/team_1/factory/fac_1/agent/implement" || c.AgentType != "IMPLEMENT" || c.Creator != "usr_1" ||
		c.RunAs != "user:usr_1" || c.WorkerHost != "wrk_1" || c.Audience != "aud-1" {
		t.Fatalf("claims %+v", c)
	}
	if got := time.Unix(c.Expiry, 0).Sub(time.Unix(c.IssuedAt, 0)); got != MaxTTL || !exp.Equal(time.Unix(c.Expiry, 0)) {
		t.Fatalf("life %v, expiry %v", got, exp)
	}
	tok, _, _ = i.Token(r, "aud-1", 0, TokenOptions{})
	if c, _ := i.Verify(tok); c.RunAs != "factory:fac_1" || c.Creator != "" {
		t.Fatalf("claims without creator %+v", c)
	}
	if _, _, err := i.Token(r, " ", 0, TokenOptions{}); err == nil {
		t.Fatal("want an error for an empty audience")
	}
	for _, claim := range []string{"agent_type", "run_as", "creator", "worker_host"} {
		found := false
		for _, s := range i.Discovery()["claims_supported"].([]string) {
			found = found || s == claim
		}
		if !found {
			t.Errorf("discovery does not list %s", claim)
		}
	}
}

// TestTokenOfLongTurn shows the life of a token: a token from the start of a
// 90-minute turn is expired at its end, and a token that is asked for at the
// end is valid. The runner and the executor therefore ask again during the
// turn (RefreshInterval).
func TestTokenOfLongTurn(t *testing.T) {
	i := testIssuer(t, "seed")
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := start
	i.SetClock(func() time.Time { return now })
	r := &core.Run{ID: "run_1", TeamID: "t", FactoryID: "f", AgentName: "a"}
	first, _, err := i.Token(r, AWSAudience, 0, TokenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for now = start; now.Before(start.Add(90 * time.Minute)); now = now.Add(RefreshInterval) {
		if _, err := i.Verify(first); now.Sub(start) < DefaultTTL && err != nil {
			t.Fatalf("at %v: %v", now.Sub(start), err)
		}
	}
	now = start.Add(90 * time.Minute)
	if _, err := i.Verify(first); err == nil {
		t.Fatal("the first token must be expired after 90 minutes")
	}
	fresh, _, _ := i.Token(r, AWSAudience, 0, TokenOptions{})
	if _, err := i.Verify(fresh); err != nil {
		t.Fatalf("fresh token at the end of the turn: %v", err)
	}
	if RefreshInterval >= DefaultTTL/2 {
		t.Fatalf("refresh interval %v is too close to the token life", RefreshInterval)
	}
}

func TestAllowedAudience(t *testing.T) {
	cp := cloudProviders(t, `{"aws":{"roleArn":"arn:aws:iam::1:role/x"},"gcp":{"projectNumber":"7","workloadIdentityFederationPoolId":"p","workloadIdentityFederationProviderId":"q"},"audiences":["https://vault.example.test"]}`)
	for aud, want := range map[string]bool{
		AWSAudience:                  true,
		GCPAudience("7", "p", "q"):   true,
		"https://vault.example.test": true,
		"https://other.example.test": false,
		"":                           false,
	} {
		if got := AllowedAudience(cp, aud); got != want {
			t.Errorf("%q: got %v", aud, got)
		}
	}
	if AllowedAudience(nil, AWSAudience) {
		t.Error("no providers must allow nothing")
	}
}

func TestGCPCredentialFileSources(t *testing.T) {
	g := &GCPSetup{Audience: "aud", ProjectNumber: "7", ServiceAccountEmail: "sa@p.iam.gserviceaccount.com"}
	b, err := GCPCredentialFile(g, "helper --audience aud", "/tok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"executable"`) || !strings.Contains(string(b), "helper --audience aud") || strings.Contains(string(b), `"/tok"`) {
		t.Fatalf("executable source: %s", b)
	}
	if !strings.Contains(string(b), "sa@p.iam.gserviceaccount.com:generateAccessToken") {
		t.Fatalf("impersonation: %s", b)
	}
	b, _ = GCPCredentialFile(g, "", "/tok")
	if !strings.Contains(string(b), `"file": "/tok"`) {
		t.Fatalf("file source: %s", b)
	}
	if SetupFor(cloudProviders(t, `{"audiences":["x"]}`)) != nil {
		t.Fatal("audiences alone are not a provider")
	}
}
