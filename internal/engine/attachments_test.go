package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestSendMessageAttachmentsOnRunnerPath(t *testing.T) {
	ctx := context.Background()
	route := &fakeRoute{}
	eng, st := newRouteEngine(t, route)
	run, err := startTestRun(ctx, eng, "team1", core.AgentConfig{Name: "a"}, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SendMessage(ctx, run.ID, "see file", "user_1", []string{"att_1", "att_2"}); err != nil {
		t.Fatal(err)
	}
	if len(route.sent) != 1 || len(route.sent[0].Attachments) != 2 || route.sent[0].Attachments[1] != "att_2" {
		t.Fatalf("sent = %+v", route.sent)
	}
	msgs, _ := st.ListRunMessages(ctx, run.ID, 0)
	last := msgs[len(msgs)-1]
	var d struct {
		Actor       string   `json:"actor"`
		Attachments []string `json:"attachments"`
	}
	if err := json.Unmarshal(last.Data, &d); err != nil || d.Actor != "user_1" || len(d.Attachments) != 2 {
		t.Fatalf("data = %s err %v", last.Data, err)
	}
}
