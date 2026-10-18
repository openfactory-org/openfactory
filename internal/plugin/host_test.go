package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
	"github.com/openfactory-org/openfactory/pkg/plugin/sdk"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// testLauncher runs an SDK plugin in process over a real gRPC connection.
func testLauncher(t *testing.T, p func() sdk.Plugin, launches *atomic.Int32, kills *[]func()) launcher {
	var mu sync.Mutex
	return func(context.Context) (*wire.Conn, func(), func() bool, error) {
		launches.Add(1)
		client, server := goplugin.TestPluginGRPCConn(t, false, wire.PluginSet(sdk.GRPCPlugin(p())))
		raw, err := client.Dispense(wire.PluginKey)
		if err != nil {
			return nil, nil, nil, err
		}
		var dead atomic.Bool
		kill := func() {
			if dead.CompareAndSwap(false, true) {
				// Stop the server first: client.Close sends a Shutdown RPC
				// whose handler also calls server.Stop, and GRPCServer.Stop
				// is not safe for concurrent use.
				server.Stop()
				_ = client.Close()
			}
		}
		mu.Lock()
		*kills = append(*kills, kill)
		mu.Unlock()
		t.Cleanup(kill)
		return raw.(*wire.Conn), kill, dead.Load, nil
	}
}

// fakeForge records calls.
type fakeForge struct {
	core.Forge
	token string
	mu    sync.Mutex
	calls []string
}

func (f *fakeForge) record(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeForge) Name() string { return "FAKE" }
func (f *fakeForge) CloneURL(_ context.Context, owner, repo string) (string, error) {
	return "https://x:" + f.token + "@fake/" + owner + "/" + repo + ".git", nil
}
func (f *fakeForge) DefaultBranch(context.Context, string, string) (string, error) {
	return "trunk", nil
}
func (f *fakeForge) CreatePullRequest(_ context.Context, in core.PullRequestInput) (*core.PullRequestRef, error) {
	return &core.PullRequestRef{Forge: "FAKE", Repo: in.Owner + "/" + in.Repo, Number: 9, Branch: in.Head, State: "draft"}, nil
}
func (f *fakeForge) GetPullRequest(_ context.Context, owner, repo string, n int) (*core.PullRequestRef, error) {
	if n == 404 {
		return nil, &forgeutil.APIError{Method: "GET", URL: "https://fake/pr/404", Status: 404, Body: "missing"}
	}
	return &core.PullRequestRef{Forge: "FAKE", Repo: owner + "/" + repo, Number: n, State: "merged"}, nil
}
func (f *fakeForge) Comment(_ context.Context, _, _ string, n int, body string) error {
	f.record(fmt.Sprintf("comment %d %s", n, body))
	return nil
}
func (f *fakeForge) SetCommitStatus(_ context.Context, _, _, sha string, st core.CommitStatus) error {
	f.record("status " + sha + " " + st.State)
	return nil
}
func (f *fakeForge) GetFiles(context.Context, string, string, string, string) (map[string]string, string, error) {
	return map[string]string{"factory.yaml": "name: x\n"}, "sha1", nil
}

type fakeResponder struct {
	mu   sync.Mutex
	msgs []core.OutboundMessage
}

func (r *fakeResponder) Provider() string { return "fakechat" }
func (r *fakeResponder) Reply(_ context.Context, _ string, m core.OutboundMessage) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	return nil
}

type testEnv struct {
	host      *Host
	forge     *fakeForge
	responder *fakeResponder
	published chan core.Event
	launches  atomic.Int32
	kills     []func()
	store     *forgetest.Store
}

func newTestEnv(t *testing.T, mutate func(*Options)) *testEnv {
	env := &testEnv{forge: &fakeForge{}, responder: &fakeResponder{}, published: make(chan core.Event, 16)}
	env.store = forgetest.NewStore()
	env.store.Integrations = []core.Integration{
		{ID: "int_1", TeamID: "team_1", Provider: "fake", Config: map[string]string{"hook_secret_ref": "HOOK", "org": "acme"}, SecretRef: "TOKEN"},
		{ID: "int_2", TeamID: "team_1", Provider: "github", Config: map[string]string{"org": "acme"}, SecretRef: "GH"},
	}
	env.store.Factories = []core.Factory{{ID: "fac_1", TeamID: "team_1", Alias: "payments"}}
	secrets := forgetest.NewSecrets(map[string]string{"HOOK": "s3cret", "TOKEN": "tok", "GH": "ghp"})
	opts := Options{
		Store: env.store, Secrets: secrets,
		Publish:        func(_ context.Context, ev core.Event) error { env.published <- ev; return nil },
		Config:         map[string]map[string]string{"fake": {"greeting": "hi"}},
		HealthInterval: 50 * time.Millisecond, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		FactoryRepos: func(_ context.Context, id string) ([]core.RepoRef, error) {
			return []core.RepoRef{{CodeForge: "FAKE", Owner: "acme", Name: id}}, nil
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	env.host = New(opts)
	t.Cleanup(func() { _ = env.host.Close() })
	return env
}

// fakePlugin builds the in-process plugin. Its webhook handler verifies a
// shared secret read through the Host service and emits one event.
func (env *testEnv) fakePlugin(configured *atomic.Value) func() sdk.Plugin {
	return func() sdk.Plugin {
		host := &sdk.DeferredHost{}
		hook := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			ints, err := sdk.Store(host).ListIntegrations(ctx, "team_1")
			if err != nil || len(ints) != 1 {
				http.Error(w, fmt.Sprintf("integrations %v %v", ints, err), 500)
				return
			}
			sec, err := forgeutil.IntegrationSecret(ctx, sdk.Secrets(host), ints[0], "hook_secret_ref")
			if err != nil || r.Header.Get("X-Token") != string(sec) {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			sdk.Logger(host).Info("delivery", "bytes", len(body))
			sdk.Bus().Publish(ctx, core.TopicInboundEvent, &core.Event{
				TeamID: "team_1", Provider: "fake", Type: "push", Text: string(body),
				Actor:      core.Actor{Provider: "fake", ExternalID: "u1", Handle: "alice"},
				Source:     core.SourceRef{Provider: "fake", Kind: "pull_request", Repo: "acme/api", Number: 4},
				Attributes: map[string]any{eventattr.Labels: []string{"bug"}, eventattr.PRNumber: 4, eventattr.Alias: "payments"},
			})
			w.Header().Set("X-Seen", r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"accepted"}`)
		})
		return sdk.Plugin{
			Name: "fake", Version: "1.2.3", Host: host,
			ForgeProviders: []string{"fake"},
			Forge: func(_ context.Context, tg sdk.ForgeTarget) (core.Forge, error) {
				if tg.Token == "" {
					return nil, fmt.Errorf("%w: token required", core.ErrInvalid)
				}
				env.forge.token = tg.Token
				return env.forge, nil
			},
			Responders: []core.Responder{env.responder},
			Ingress:    hook,
			Configure: func(_ context.Context, _ sdk.Host, cfg map[string]string) error {
				if configured != nil {
					configured.Store(cfg["greeting"])
				}
				return nil
			},
		}
	}
}

func TestHostEndToEndInProcess(t *testing.T) {
	env := newTestEnv(t, nil)
	var configured atomic.Value
	ctx := context.Background()
	if err := env.host.launch(ctx, "fake-plugin", testLauncher(t, env.fakePlugin(&configured), &env.launches, &env.kills)); err != nil {
		t.Fatal(err)
	}
	if configured.Load() != "hi" {
		t.Fatalf("configure config = %v", configured.Load())
	}
	infos := env.host.Plugins()
	if len(infos) != 1 || infos[0].Name != "fake" || infos[0].Version != "1.2.3" || !infos[0].Running || len(infos[0].Capabilities) != 3 {
		t.Fatalf("plugins = %+v", infos)
	}

	t.Run("forge", func(t *testing.T) {
		if !env.host.HasForge("fake") || env.host.HasForge("gitlab") {
			t.Fatal("HasForge")
		}
		if _, err := env.host.ForgeFor(ctx, forge.ForgeTarget{Provider: "fake"}); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("missing token: %v", err)
		}
		f, err := env.host.ForgeFor(ctx, forge.ForgeTarget{Provider: "fake", Token: "tok", Config: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		if f.Name() != "FAKE" {
			t.Fatalf("name = %s", f.Name())
		}
		if u, _ := f.CloneURL(ctx, "acme", "api"); u != "https://x:tok@fake/acme/api.git" {
			t.Fatalf("clone = %s", u)
		}
		if b, _ := f.DefaultBranch(ctx, "acme", "api"); b != "trunk" {
			t.Fatalf("branch = %s", b)
		}
		pr, err := f.CreatePullRequest(ctx, core.PullRequestInput{Owner: "acme", Repo: "api", Head: "x", Draft: true})
		if err != nil || pr.Number != 9 || pr.State != "draft" || pr.Branch != "x" {
			t.Fatalf("pr = %+v %v", pr, err)
		}
		_, err = f.GetPullRequest(ctx, "acme", "api", 404)
		if !errors.Is(err, core.ErrNotFound) || !forgeutil.IsNotFound(err) {
			t.Fatalf("404 not mapped: %v", err)
		}
		// Issue comments fall back to Comment when the plugin forge has none.
		if err := f.(forge.IssueCommenter).CommentIssue(ctx, "acme", "api", 5, "hello"); err != nil {
			t.Fatal(err)
		}
		if err := f.(forge.WorkItemCommenter).CommentWorkItem(ctx, "p", 1, "x"); !errors.Is(err, ErrUnimplemented) {
			t.Fatalf("work item comment: %v", err)
		}
		if err := f.SetCommitStatus(ctx, "acme", "api", "abc", core.CommitStatus{State: "success"}); err != nil {
			t.Fatal(err)
		}
		files, sha, err := f.GetFiles(ctx, "acme", "api", "", "")
		if err != nil || sha != "sha1" || files["factory.yaml"] != "name: x\n" {
			t.Fatalf("files = %v %s %v", files, sha, err)
		}
		if got := strings.Join(env.forge.calls, "|"); got != "comment 5 hello|status abc success" {
			t.Fatalf("calls = %s", got)
		}
		// forge.Resolver resolves the provider through the host.
		r := forge.NewResolver(env.store, forgetest.NewSecrets(map[string]string{"TOKEN": "tok"}), forge.Options{Plugins: env.host})
		rf, err := r.Forge(ctx, core.ForgeLookup{TeamID: "team_1", CodeForge: "fake", Repo: "acme/api"})
		if err != nil || rf.Name() != "FAKE" {
			t.Fatalf("resolver forge = %v %v", rf, err)
		}
	})

	t.Run("responder", func(t *testing.T) {
		rs := env.host.Responders()
		if len(rs) != 1 || rs[0].Provider() != "fakechat" {
			t.Fatalf("responders = %v", rs)
		}
		msg := core.OutboundMessage{WorkItemID: "wi_1", Text: "done", Kind: "question", Options: []string{"yes", "no"},
			Source: core.SourceRef{Provider: "fakechat", Channel: "c1", ThreadID: "t1"}}
		if err := rs[0].Reply(ctx, "team_1", msg); err != nil {
			t.Fatal(err)
		}
		got := env.responder.msgs[0]
		if got.WorkItemID != "wi_1" || got.Source.ThreadID != "t1" || len(got.Options) != 2 {
			t.Fatalf("reply = %+v", got)
		}
	})

	t.Run("ingress", func(t *testing.T) {
		mux := http.NewServeMux()
		env.host.Register(mux)
		srv := httptest.NewServer(mux)
		defer srv.Close()

		post := func(path, token string) *http.Response {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader("payload"))
			req.Header.Set("X-Token", token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			return resp
		}
		if resp := post("/plugins/fake/webhook", "wrong"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad token status = %d", resp.StatusCode)
		}
		if resp := post("/plugins/nope/webhook", "s3cret"); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown plugin status = %d", resp.StatusCode)
		}
		for _, path := range []string{"/plugins/fake/webhook"} {
			resp := post(path, "s3cret")
			if resp.StatusCode != http.StatusAccepted || resp.Header.Get("X-Seen") != path {
				t.Fatalf("%s: status = %d header = %q", path, resp.StatusCode, resp.Header.Get("X-Seen"))
			}
			select {
			case ev := <-env.published:
				labels, _ := ev.Attributes[eventattr.Labels].([]string)
				if ev.ID == "" || ev.Text != "payload" || ev.FactoryID != "fac_1" || len(labels) != 1 || ev.Attributes[eventattr.PRNumber] != 4 || ev.Source.Number != 4 {
					t.Fatalf("event = %+v", ev)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no event published")
			}
		}
	})

	t.Run("host scoping", func(t *testing.T) {
		svc := newHostService(env.host, env.host.forgePlugin("fake").pluginInfo())
		if _, err := svc.secret(ctx, "team_1", "", "GH"); err == nil {
			t.Fatal("plugin read a secret of another provider's integration")
		}
		if _, err := svc.integrations(ctx, "", "github"); err == nil {
			t.Fatal("plugin listed another provider's integrations")
		}
		v, err := svc.secret(ctx, "team_1", "", "TOKEN")
		if err != nil || string(v) != "tok" {
			t.Fatalf("own secret = %q %v", v, err)
		}
	})

	t.Run("restart after crash", func(t *testing.T) {
		before := env.launches.Load()
		env.kills[len(env.kills)-1]()
		deadline := time.Now().Add(5 * time.Second)
		for env.launches.Load() == before || !env.host.Plugins()[0].Running {
			if time.Now().After(deadline) {
				t.Fatalf("plugin not restarted: %+v", env.host.Plugins())
			}
			time.Sleep(10 * time.Millisecond)
		}
		f, err := env.host.ForgeFor(ctx, forge.ForgeTarget{Provider: "fake", Token: "tok"})
		if err != nil {
			t.Fatal(err)
		}
		if b, err := f.DefaultBranch(ctx, "a", "b"); err != nil || b != "trunk" {
			t.Fatalf("after restart: %s %v", b, err)
		}
		if env.host.Plugins()[0].Restarts < 1 {
			t.Fatal("restart not counted")
		}
	})

	if err := env.host.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := env.host.ForgeFor(ctx, forge.ForgeTarget{Provider: "fake", Token: "tok"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("after close: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode uint32) {
		if err := writeFile(dir+"/"+name, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("openfactory-plugin-b", 0o755)
	write("openfactory-plugin-a", 0o755)
	write("openfactory-plugin-noexec", 0o644)
	write("other-binary", 0o755)
	h := New(Options{Dir: dir, Paths: []string{"/opt/x/custom"}})
	got, err := h.Discover()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/x/custom", dir + "/openfactory-plugin-a", dir + "/openfactory-plugin-b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("discover = %v want %v", got, want)
	}
	if got, err := New(Options{Dir: dir + "/missing"}).Discover(); err != nil || len(got) != 0 {
		t.Fatalf("missing dir = %v %v", got, err)
	}
}

func TestDuplicateNameIsFatal(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	if err := env.host.launch(ctx, "one", testLauncher(t, env.fakePlugin(nil), &env.launches, &env.kills)); err != nil {
		t.Fatal(err)
	}
	err := env.host.launch(ctx, "two", testLauncher(t, env.fakePlugin(nil), &env.launches, &env.kills))
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("duplicate name: %v", err)
	}
}

func TestEventAttributeNormalization(t *testing.T) {
	ev := &core.Event{TeamID: "t", Attributes: map[string]any{
		"labels": []string{"a"}, "n": 3, "f": 1.5, "mixed": []any{"a", 1}, "empty": []string{}, "b": true,
	}, Payload: json.RawMessage(`{"x":1}`)}
	pe, err := wire.EventToPB(ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := wire.EventFromPB(pe)
	if err != nil {
		t.Fatal(err)
	}
	a := got.Attributes
	if l, ok := a["labels"].([]string); !ok || l[0] != "a" {
		t.Fatalf("labels = %#v", a["labels"])
	}
	if a["n"] != 3 || a["f"] != 1.5 || a["b"] != true {
		t.Fatalf("scalars = %#v", a)
	}
	if m, ok := a["mixed"].([]any); !ok || m[1] != 1 {
		t.Fatalf("mixed = %#v", a["mixed"])
	}
	if e, ok := a["empty"].([]string); !ok || len(e) != 0 {
		t.Fatalf("empty = %#v", a["empty"])
	}
	if got.ID == "" || string(got.Payload) != `{"x":1}` || got.ReceivedAt.IsZero() {
		t.Fatalf("event = %+v", got)
	}
}

func writeFile(path string, mode uint32) error {
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, os.FileMode(mode))
}
