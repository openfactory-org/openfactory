package runnerclient

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestConversation(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK, `{"conversation_id":"c","steps":[{"step_id":"step_1","session":2,"messages":[{"role":"user","kind":"user","blocks":[{"type":"text","text":"hi"}]}]}]}`)
	c, err := New(Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.SetToken("tok")
	conv, err := c.Conversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := rec.last(t)
	if got.method != http.MethodGet || got.path != runnerproto.PathConversation || got.header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("request %+v", got)
	}
	if conv.ConversationID != "c" || conv.Steps[0].Session != 2 || conv.Steps[0].Messages[0].Blocks[0].Text != "hi" {
		t.Fatalf("conversation %+v", conv)
	}
}

func TestCloudToken(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK, `{"token":"jwt","expires_at":"2030-01-01T00:00:00Z"}`)
	c, err := New(Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.SetToken("tok")
	resp, err := c.CloudToken(context.Background(), "aud", 900)
	if err != nil {
		t.Fatal(err)
	}
	got := rec.last(t)
	if got.method != http.MethodPost || got.path != runnerproto.PathCloudToken || !strings.Contains(got.body, `"audience":"aud"`) || !strings.Contains(got.body, `"ttl_seconds":900`) {
		t.Fatalf("request %+v", got)
	}
	if resp.Token != "jwt" || resp.ExpiresAt.Year() != 2030 {
		t.Fatalf("response %+v", resp)
	}
}
