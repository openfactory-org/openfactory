package teams

import (
	"context"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestPostMessage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	in := e.store.Integrations[0]

	// A channel that the bot never saw: no service URL, not selected.
	if err := e.h.PostMessage(ctx, in, channel, "", "hi"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("unknown channel: %v", err)
	}
	// The bot saw the channel: the recorded service URL is used.
	_ = e.store.KVSet(ctx, "teams:service_url:"+channel, e.srv.URL+"/svc")
	if err := e.h.PostMessage(ctx, in, channel, "1700", "Deployed"); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	if len(e.paths) != 1 || e.paths[0] != channel+";messageid=1700" || e.posted[0]["text"] != "Deployed" {
		t.Fatalf("paths = %v posted = %v", e.paths, e.posted)
	}
	e.mu.Unlock()

	// A record with selected channels allows only those.
	sel := in
	sel.Config = map[string]string{"app_id": appID, "tenant_id": tenant, "channel_ids": "19:other@thread.tacv2, " + channel}
	if err := e.h.PostMessage(ctx, sel, "19:third@thread.tacv2", "", "x"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("channel outside the selection: %v", err)
	}
	if err := e.h.PostMessage(ctx, sel, channel, "", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := e.h.PostMessage(ctx, sel, " ", "", "x"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("empty channel: %v", err)
	}
}
