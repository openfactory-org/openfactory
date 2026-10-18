package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
	"github.com/openfactory-org/openfactory/pkg/plugin/sdk"
)

// contextForge adds the optional review context, login and reviewer
// operations to fakeForge.
type contextForge struct {
	*fakeForge
}

func (f contextForge) Login(context.Context) (string, error) { return "id-42", nil }

func (f contextForge) RequestReviewers(_ context.Context, owner, repo string, n int, logins []string) error {
	f.record(fmt.Sprintf("reviewers %s/%s#%d %s", owner, repo, n, strings.Join(logins, ",")))
	return nil
}

func (f contextForge) ReviewThread(_ context.Context, _, _ string, n int, ref string) (*core.ReviewThread, error) {
	if ref == "gone" {
		return nil, core.ErrNotFound
	}
	return &core.ReviewThread{Ref: ref, Path: "a/b.go", Line: 7, Resolved: true, Comments: []core.ThreadComment{
		{ID: "1", Author: "dana", Body: "rename this", Path: "a/b.go", Line: 7, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{ID: "2", Author: "bot", Body: "done"},
	}}, nil
}

func (f contextForge) FileDiff(_ context.Context, _, _ string, n int, path string) (*core.FileDiff, error) {
	return &core.FileDiff{Path: path, Patch: "@@ -1 +1 @@\n-a\n+b\n", Truncated: true}, nil
}

// TestPluginReviewContextAndReviewers checks the reviewer request and review context operations over
// the plugin protocol, and that a plugin that lacks them gives
// operation_not_supported.
func TestPluginReviewContextAndReviewers(t *testing.T) {
	ctx := context.Background()
	for _, withOps := range []bool{true, false} {
		env := newTestEnv(t, nil)
		base := env.fakePlugin(nil)
		plug := func() sdk.Plugin {
			p := base()
			p.Forge = func(context.Context, sdk.ForgeTarget) (core.Forge, error) {
				if withOps {
					return contextForge{env.forge}, nil
				}
				return env.forge, nil
			}
			return p
		}
		if err := env.host.launch(ctx, "fake-plugin", testLauncher(t, plug, &env.launches, &env.kills)); err != nil {
			t.Fatal(err)
		}
		f, err := env.host.ForgeFor(ctx, forge.ForgeTarget{Provider: "fake", Token: "tok", Config: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		lr, _ := f.(core.LoginReader)
		rr, _ := f.(core.ReviewRequester)
		rc, _ := f.(core.ReviewContextReader)
		if lr == nil || rr == nil || rc == nil {
			t.Fatalf("the plugin forge lacks an interface: %v %v %v", lr, rr, rc)
		}
		if !withOps {
			if _, err := lr.Login(ctx); !errors.Is(err, core.ErrOperationNotSupported) {
				t.Fatalf("login: %v", err)
			}
			if err := rr.RequestReviewers(ctx, "acme", "api", 4, []string{"x"}); !errors.Is(err, core.ErrOperationNotSupported) {
				t.Fatalf("reviewers: %v", err)
			}
			if _, err := rc.ReviewThread(ctx, "acme", "api", 4, "1"); !errors.Is(err, core.ErrOperationNotSupported) {
				t.Fatalf("thread: %v", err)
			}
			if _, err := rc.FileDiff(ctx, "acme", "api", 4, "a.go"); !errors.Is(err, core.ErrOperationNotSupported) {
				t.Fatalf("diff: %v", err)
			}
			continue
		}
		if l, err := lr.Login(ctx); err != nil || l != "id-42" {
			t.Fatalf("login = %q, %v", l, err)
		}
		if err := rr.RequestReviewers(ctx, "acme", "api", 4, []string{"id-42"}); err != nil {
			t.Fatal(err)
		}
		if got := env.forge.calls[len(env.forge.calls)-1]; got != "reviewers acme/api#4 id-42" {
			t.Fatalf("call = %q", got)
		}
		th, err := rc.ReviewThread(ctx, "acme", "api", 4, "9")
		if err != nil {
			t.Fatal(err)
		}
		if th.Ref != "9" || th.Path != "a/b.go" || th.Line != 7 || !th.Resolved || len(th.Comments) != 2 ||
			th.Comments[0].Author != "dana" || !th.Comments[0].CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || !th.Comments[1].CreatedAt.IsZero() {
			t.Fatalf("thread = %+v", th)
		}
		if _, err := rc.ReviewThread(ctx, "acme", "api", 4, "gone"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("missing thread: %v", err)
		}
		d, err := rc.FileDiff(ctx, "acme", "api", 4, "a.go")
		if err != nil || d.Path != "a.go" || !d.Truncated || !strings.Contains(d.Patch, "+b") {
			t.Fatalf("diff = %+v, %v", d, err)
		}
	}
}

// TestPluginEventActionKey checks that the action key of a plugin event
// reaches the host, and that an event without one stays empty (the engine
// derives it from the delivery ID).
func TestPluginEventActionKey(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	plug := func() sdk.Plugin {
		p := env.fakePlugin(nil)()
		host := &sdk.DeferredHost{}
		p.Host = host
		p.Ingress = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, k := range []string{"fake:d-1", ""} {
				sdk.Bus().Publish(r.Context(), core.TopicInboundEvent, &core.Event{
					TeamID: "team_1", Provider: "fake", Type: "comment", DeliveryID: "d-1:comment", ActionKey: k,
				})
			}
			w.WriteHeader(http.StatusAccepted)
		})
		return p
	}
	if err := env.host.launch(ctx, "fake-plugin", testLauncher(t, plug, &env.launches, &env.kills)); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	env.host.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/plugins/fake/webhook", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var got []string
	for range 2 {
		select {
		case ev := <-env.published:
			got = append(got, ev.ActionKey)
		case <-time.After(5 * time.Second):
			t.Fatal("no event published")
		}
	}
	if fmt.Sprint(got) != "[fake:d-1 ]" {
		t.Fatalf("action keys = %q", got)
	}
}
