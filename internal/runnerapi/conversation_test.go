package runnerapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/conversation"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
)

func TestBuildConversation(t *testing.T) {
	run := &core.Run{FactoryID: "fac_test", ID: "run_1"}
	msgs := []core.RunMessage{
		{ID: "m1", Role: "user", Content: "do it"},
		{ID: "m2", Role: "assistant", Content: "looking"},
		{ID: "m3", Role: "tool_call", Content: "bash", Data: json.RawMessage(`{"call_id":"c1","category":"command","arguments":{"cmd":"ls"}}`)},
		{ID: "m4", Role: "tool_result", Content: "a.txt", Data: json.RawMessage(`{"call_id":"c1","is_error":false}`)},
		{ID: "m5", Role: "tool_call", Content: "bash", Data: json.RawMessage(`{"call_id":"c2"}`)},
		{ID: "m6", Role: "tool_result", Content: "fail", Data: json.RawMessage(`{"call_id":"c2","is_error":true}`)},
		{ID: "m7", Role: "assistant", Content: "done"},
		{ID: "m8", Role: "log", Content: "noise"},
		{ID: "m9", Role: "system", Content: "note"},
		{ID: "m10", Role: "user", Content: "again"},
		{ID: "m11", Role: "assistant", Content: "sure"},
	}
	c := conversation.Build(run, msgs)
	if c.ConversationID != "run_1" || len(c.Steps) != 2 {
		t.Fatalf("conversation %+v", c)
	}
	s1 := c.Steps[0]
	if s1.StepID != "step_1" || c.Steps[1].StepID != "step_2" || len(s1.Messages) != 3 {
		t.Fatalf("step 1: %+v", s1)
	}
	if u := s1.Messages[0]; u.Role != "user" || u.Kind != "user" || len(u.Blocks) != 1 || u.Blocks[0].Text != "do it" {
		t.Fatalf("user message %+v", u)
	}
	a := s1.Messages[1]
	if a.Role != "assistant" || len(a.Blocks) != 6 {
		t.Fatalf("assistant message %+v", a)
	}
	act := a.Blocks[1]
	if act.Type != "action" || act.Name != "bash" || act.CallID != "c1" || act.Category != "command" || act.State != "completed" || string(act.Input) != `{"cmd":"ls"}` {
		t.Fatalf("action %+v", act)
	}
	if r := a.Blocks[2]; r.Type != "action_result" || r.CallID != "c1" || r.Text != "a.txt" || r.IsError {
		t.Fatalf("result %+v", r)
	}
	if r := a.Blocks[4]; r.Type != "action_result" || !r.IsError {
		t.Fatalf("error result %+v", r)
	}
	if a.Blocks[5].Text != "done" {
		t.Fatalf("last block %+v", a.Blocks[5])
	}
	if s := s1.Messages[2]; s.Role != "system" || s.Kind != "system" || s.Blocks[0].Text != "note" {
		t.Fatalf("system message %+v", s)
	}
	if m := c.Steps[1].Messages; len(m) != 2 || m[1].Role != "assistant" {
		t.Fatalf("step 2: %+v", m)
	}
}

func TestBuildConversationEmpty(t *testing.T) {
	c := conversation.Build(&core.Run{FactoryID: "fac_test", ID: "r"}, nil)
	if c.Steps == nil || len(c.Steps) != 0 {
		t.Fatalf("empty conversation: %+v", c)
	}
	b, _ := json.Marshal(c)
	if string(b) != `{"conversation_id":"r","steps":[]}` {
		t.Fatalf("json %s", b)
	}
	c = conversation.Build(&core.Run{FactoryID: "fac_test", ID: "r"}, []core.RunMessage{
		{Role: "system", Content: "summary"},
		{Role: "assistant", Content: "x", Data: json.RawMessage(`not json`)},
	})
	if len(c.Steps) != 1 || len(c.Steps[0].Messages) != 2 {
		t.Fatalf("conversation with a bad Data field: %+v", c)
	}
}

func TestConversationRoute(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	for _, m := range []core.RunMessage{
		{RunID: e.run.ID, Role: "user", Content: "hello"},
		{RunID: e.run.ID, Role: "assistant", Content: "hi"},
	} {
		m := m
		if err := e.mem.AppendRunMessage(context.Background(), &m); err != nil {
			t.Fatal(err)
		}
	}
	conv, err := c.Conversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if conv.ConversationID != e.run.ID || len(conv.Steps) != 1 || len(conv.Steps[0].Messages) != 2 {
		t.Fatalf("conversation %+v", conv)
	}
	if conv.Steps[0].Messages[1].Blocks[0].Text != "hi" {
		t.Fatalf("messages %+v", conv.Steps[0].Messages)
	}

	// Without a valid session token the route answers 401.
	bad := e.client()
	bad.SetToken("nope")
	if _, err := bad.Conversation(context.Background()); !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401, got %v", err)
	}
}

type fakeCloudTokens struct {
	audience string
	ttl      time.Duration
	session  int
	err      error
}

func (f *fakeCloudTokens) CloudToken(_ context.Context, _ *core.Run, session *core.RunSession, audience string, ttl time.Duration) (string, time.Time, error) {
	f.audience, f.ttl, f.session = audience, ttl, session.Session
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "jwt", time.Unix(2000000000, 0).UTC(), nil
}

func TestCloudTokenRoute(t *testing.T) {
	src := &fakeCloudTokens{}
	e := newEnv(t, func(c *Config) { c.CloudTokens = src })
	c := e.started()
	ctx := context.Background()

	resp, err := c.CloudToken(ctx, "sts.amazonaws.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Token != "jwt" || !resp.ExpiresAt.Equal(time.Unix(2000000000, 0)) {
		t.Fatalf("response %+v", resp)
	}
	if src.audience != "sts.amazonaws.com" || src.ttl != time.Hour || src.session != 1 {
		t.Fatalf("source got %+v", src)
	}
	if _, err := c.CloudToken(ctx, "a", 600); err != nil || src.ttl != 10*time.Minute {
		t.Fatalf("ttl %v err %v", src.ttl, err)
	}

	for name, tc := range map[string]struct {
		aud string
		ttl int
	}{"no audience": {"", 0}, "ttl low": {"a", 59}, "ttl high": {"a", 3601}, "ttl negative": {"a", -5}} {
		_, err := c.CloudToken(ctx, tc.aud, tc.ttl)
		var se *runnerclient.StatusError
		if !asStatus(err, &se) || se.Status != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}
	if _, err := c.CloudToken(ctx, "a", 3600); err != nil {
		t.Fatalf("max ttl: %v", err)
	}

	for _, tc := range []struct {
		err  error
		want int
	}{{core.ErrForbidden, 403}, {core.ErrInvalid, 400}, {errors.New("boom"), 500}} {
		src.err = tc.err
		_, err := c.CloudToken(ctx, "a", 0)
		var se *runnerclient.StatusError
		if !asStatus(err, &se) || se.Status != tc.want {
			t.Errorf("%v: want %d, got %v", tc.err, tc.want, err)
		}
	}
}

func TestCloudTokenRouteNotImplemented(t *testing.T) {
	e := newEnv(t, nil)
	_, err := e.started().CloudToken(context.Background(), "a", 0)
	var se *runnerclient.StatusError
	if !asStatus(err, &se) || se.Status != http.StatusNotImplemented {
		t.Fatalf("want 501, got %v", err)
	}
	// A bad token is refused before the source is checked.
	bad := e.client()
	bad.SetToken("nope")
	if _, err := bad.CloudToken(context.Background(), "a", 0); !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401, got %v", err)
	}
}
