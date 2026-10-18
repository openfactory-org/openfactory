package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// A selected record that is not active fails with
// integration_disabled. A reply must not post with a disabled record.
func TestReplyDisabledRecord(t *testing.T) {
	f := newFixture(t)
	f.store.Integrations[0].Status = core.IntegrationDisabled
	r := NewResponder(ResponderOptions{Store: f.store, Secrets: forgetest.NewSecrets(map[string]string{"SLACK_BOT": "xoxb-test"}), APIBaseURL: f.srv.URL})
	err := r.Reply(context.Background(), "team_1", core.OutboundMessage{WorkItemID: "wi_1", FactoryID: "fac_1", Kind: "question", Text: "hi",
		Source: core.SourceRef{Provider: "slack", Channel: "C1", ThreadID: "1.0", Workspace: "T1"}})
	if n := len(f.api.get("chat.postMessage")); n != 0 {
		t.Errorf("posted %d messages with a disabled record", n)
	}
	if !errors.Is(err, core.ErrIntegrationDisabled) {
		t.Errorf("err = %v, want ErrIntegrationDisabled", err)
	}
}

// Intake with a disabled record must not start work.
func TestIntakeDisabledRecord(t *testing.T) {
	f := newFixture(t)
	f.store.Integrations[0].Status = core.IntegrationDisabled
	f.store.Integrations[0].Config[ConfigLinkByEmail] = "true"
	f.event(t, map[string]any{"type": "app_mention", "user": "U1", "text": "<@UBOT> fix it", "ts": "100.1", "channel": "C1", "channel_type": "channel"})
	if ev := f.bus.Find("app_mention"); ev != nil {
		t.Errorf("event from a disabled record was published: team=%s factory=%s", ev.TeamID, ev.FactoryID)
	}
}
