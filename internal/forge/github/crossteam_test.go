package github

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// Team B signs installation events with its own webhook secret and names a
// repository of its own owner, but the installation of team A. The events
// must not change the records of team A: deleted must not disable them, and
// created must not enable them again.
func TestInstallationEventCrossTeam(t *testing.T) {
	for _, c := range []struct {
		action, start string
	}{{"deleted", core.IntegrationActive}, {"created", core.IntegrationDisabled}} {
		store := forgetest.NewStore()
		store.Teams = []core.Team{{ID: "team_a"}, {ID: "team_b"}}
		store.Integrations = []core.Integration{
			{ID: "int_a", TeamID: "team_a", Provider: "github", Status: c.start, Config: map[string]string{"org": "acme", "app_id": "7", "installation_id": "42", "webhook_secret_ref": "A_WEBHOOK"}},
			{ID: "int_b", TeamID: "team_b", Provider: "github", SecretRef: "B_TOKEN", Config: map[string]string{"org": "evil", "webhook_secret_ref": "B_WEBHOOK"}},
		}
		h := NewWebhookHandler(WebhookOptions{Store: store, Bus: forgetest.NewBus(),
			Secrets: forgetest.NewSecrets(map[string]string{"A_WEBHOOK": "secret-a", "B_WEBHOOK": "secret-b"})})
		rec := deliver(t, h, "installation", "secret-b", map[string]any{"action": c.action,
			"installation": map[string]any{"id": 42}, "repository": map[string]any{"full_name": "evil/x"}})
		a, _ := store.GetIntegration(context.Background(), "int_a")
		if a.EffectiveStatus() != c.start {
			t.Errorf("%s: team_b's signed installation event changed team_a's record: status=%s reason=%q (http %d %s)",
				c.action, a.EffectiveStatus(), a.StatusReason, rec.Code, rec.Body.String())
		}
	}
}

// Two teams each have a record for the owner "acme" with their own webhook
// secret. A genuine delivery signed with team A's secret must be accepted for
// team A, whatever the order of the teams in the store.
func TestIntakeTwoTeamsSameOwner(t *testing.T) {
	for _, order := range [][]string{{"team_a", "team_b"}, {"team_b", "team_a"}} {
		store := forgetest.NewStore()
		store.Teams = nil
		for _, id := range order {
			store.Teams = append(store.Teams, core.Team{ID: id})
		}
		store.Integrations = []core.Integration{
			{ID: "int_a", TeamID: "team_a", Provider: "github", SecretRef: "A_TOKEN", Config: map[string]string{"org": "acme", "webhook_secret_ref": "A_WEBHOOK"}},
			{ID: "int_b", TeamID: "team_b", Provider: "github", SecretRef: "B_TOKEN", Config: map[string]string{"org": "acme", "webhook_secret_ref": "B_WEBHOOK"}},
		}
		bus := forgetest.NewBus()
		h := NewWebhookHandler(WebhookOptions{Store: store, Bus: bus,
			Secrets: forgetest.NewSecrets(map[string]string{"A_WEBHOOK": "secret-a", "B_WEBHOOK": "secret-b"})})
		rec := deliver(t, h, "issues", "secret-a", map[string]any{"action": "opened", "repository": repo,
			"issue": map[string]any{"number": 1, "title": "x", "user": map[string]any{"login": "u", "id": 1}}, "sender": map[string]any{"login": "u", "id": 1}})
		if rec.Code != http.StatusAccepted {
			t.Errorf("teams %v: delivery signed by team_a: status %d, body %s", order, rec.Code, rec.Body.String())
			continue
		}
		for _, ev := range bus.Events() {
			if ev.TeamID != "team_a" {
				t.Errorf("teams %v: event of team_a's delivery went to %s", order, ev.TeamID)
			}
		}
	}
}

// Two teams share one webhook secret for one owner: the delivery is rejected,
// the server does not pick one of the teams.
func TestIntakeTwoTeamsSharedSecret(t *testing.T) {
	store := forgetest.NewStore()
	store.Teams = []core.Team{{ID: "team_a"}, {ID: "team_b"}}
	store.Integrations = []core.Integration{
		{ID: "int_a", TeamID: "team_a", Provider: "github", SecretRef: "A_TOKEN", Config: map[string]string{"org": "acme", "webhook_secret_ref": "A_WEBHOOK"}},
		{ID: "int_b", TeamID: "team_b", Provider: "github", SecretRef: "B_TOKEN", Config: map[string]string{"org": "acme", "webhook_secret_ref": "B_WEBHOOK"}},
	}
	bus := forgetest.NewBus()
	h := NewWebhookHandler(WebhookOptions{Store: store, Bus: bus,
		Secrets: forgetest.NewSecrets(map[string]string{"A_WEBHOOK": "shared", "B_WEBHOOK": "shared"})})
	rec := deliver(t, h, "issues", "shared", map[string]any{"action": "opened", "repository": repo,
		"issue": map[string]any{"number": 1, "title": "x", "user": map[string]any{"login": "u", "id": 1}}, "sender": map[string]any{"login": "u", "id": 1}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ambiguous_integration") {
		t.Errorf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if n := len(bus.Events()); n != 0 {
		t.Errorf("published %d events", n)
	}
	rec = deliver(t, h, "issues", "other", map[string]any{"action": "opened", "repository": repo})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned by any team: status %d", rec.Code)
	}
}
