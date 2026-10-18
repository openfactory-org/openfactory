package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/api/apitest"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
	"github.com/openfactory-org/openfactory/pkg/sdk"
)

type fixture struct {
	store  *sqlstore.Store
	bus    *apitest.Bus
	ts     *httptest.Server
	client *sdk.Client
	viewer *sdk.Client
	team   *core.Team
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store := testdb.New(t)
	bus := apitest.NewBus()
	srv := api.New(api.Options{
		Store: store, Engine: apitest.NewEngine(store, bus), Secrets: &apitest.Secrets{Store: store},
		Compile: apitest.Compile, Bus: bus, PublicURL: "https://factory.example.test", SSEHeartbeat: 20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: func() map[string]json.RawMessage {
			return map[string]json.RawMessage{"factory.schema.json": json.RawMessage(`{"type":"object"}`)}
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	team := &core.Team{ID: core.NewID("team"), Name: "Acme"}
	_ = store.CreateTeam(ctx, team)
	editor, err := apitest.SeedUser(ctx, store, team.ID, core.RoleEditor, api.NewAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	viewer, _ := apitest.SeedUser(ctx, store, team.ID, core.RoleUser, api.NewAPIKey)
	fast := sdk.WithBackoff(time.Millisecond, 5*time.Millisecond)
	return &fixture{store: store, bus: bus, ts: ts, team: team,
		client: sdk.NewClient(ts.URL, editor.Key, fast),
		viewer: sdk.NewClient(ts.URL+"/api/v1/", viewer.Key, fast)}
}

func TestFactoriesAndPagination(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var uids []string
	for _, n := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		res, err := f.client.Factories.Create(ctx, sdk.CreateFactoryParams{Name: n, Repositories: []sdk.RepoRef{{Owner: "acme", Name: n}}})
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, res.Factory.UID)
		time.Sleep(time.Millisecond)
	}
	all, err := f.viewer.Factories.List(ctx, &sdk.FactoryListOptions{PageSize: 2}).Collect(ctx)
	if err != nil || len(all) != 5 {
		t.Fatalf("collected %d factories, err %v", len(all), err)
	}
	n := 0
	for fac, err := range f.viewer.Factories.List(ctx, &sdk.FactoryListOptions{Search: "ALP"}).All(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		if fac.Name != "alpha" {
			t.Fatalf("search returned %q", fac.Name)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("search count %d", n)
	}
	got, err := f.viewer.Factories.Get(ctx, uids[0])
	if err != nil || got.Name != "alpha" {
		t.Fatal(got, err)
	}
	name := "Alpha Prime"
	if up, err := f.client.Factories.Update(ctx, uids[0], sdk.UpdateFactoryParams{Name: &name}); err != nil || up.Name != name {
		t.Fatal(up, err)
	}
	agents, err := f.viewer.Factories.Agents(ctx, uids[0])
	if err != nil || len(agents) != 5 {
		t.Fatalf("agents %d %v", len(agents), err)
	}
	def, err := f.viewer.Definitions.Get(ctx, uids[0])
	if err != nil || def.Files["factory.yaml"] == "" {
		t.Fatal(def, err)
	}
	vr, err := f.viewer.Definitions.Validate(ctx, uids[0], map[string]string{"factory.yaml": "name: x"})
	if err != nil || vr.Valid || len(vr.Errors) == 0 {
		t.Fatal(vr, err)
	}
	_, err = f.client.Definitions.Put(ctx, uids[0], sdk.PutDefinitionParams{Files: map[string]string{"factory.yaml": "name: x"}})
	var apiErr *sdk.Error
	if !sdk.IsInvalid(err) || !asError(err, &apiErr) || len(apiErr.Errors) == 0 || apiErr.StatusCode != 400 {
		t.Fatalf("expected validation error, got %v", err)
	}
	versions, err := f.viewer.Definitions.Versions(ctx, uids[0], false).Collect(ctx)
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions %d %v", len(versions), err)
	}
}

func asError(err error, target **sdk.Error) bool {
	return errors.As(err, target)
}

func TestTypedErrors(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.viewer.Factories.Get(ctx, "fac_missing"); !sdk.IsNotFound(err) {
		t.Fatalf("want not found, got %v", err)
	}
	if _, err := f.viewer.Factories.Create(ctx, sdk.CreateFactoryParams{Name: "x"}); !sdk.IsForbidden(err) {
		t.Fatalf("want forbidden, got %v", err)
	}
	bad := sdk.NewClient(f.ts.URL, "of_abcdefgh_"+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if _, err := bad.Users.Me(ctx); !sdk.IsUnauthorized(err) {
		t.Fatalf("want unauthorized, got %v", err)
	}
	if me, err := f.viewer.Users.Me(ctx); err != nil || me.Role != "user" {
		t.Fatal(me, err)
	}
}

func TestRunsAndStreaming(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	fac, err := f.client.Factories.Create(ctx, sdk.CreateFactoryParams{Name: "runs"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.viewer.Factories.Dispatch(ctx, fac.Factory.UID, sdk.DispatchParams{Prompt: "Fix the flaky test", TicketRef: "linear:PAY-1"})
	if err != nil || run.State != sdk.StateQueued || run.WorkItemURL == "" {
		t.Fatal(run, err)
	}
	wi, err := f.viewer.WorkItems.Get(ctx, run.WorkItemID)
	if err != nil || len(wi.Runs) != 1 {
		t.Fatal(wi, err)
	}
	items, err := f.viewer.WorkItems.List(ctx, fac.Factory.UID, &sdk.WorkItemListOptions{Creator: "me"}).Collect(ctx)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}

	sr, err := f.viewer.Runs.Create(ctx, sdk.CreateRunParams{Prompt: "Audit the code", FactoryID: fac.Factory.UID, AgentName: "triage",
		Config: &sdk.RunConfig{Harness: &sdk.HarnessConfig{Type: "codex", Model: "gpt-5"}}})
	if err != nil || sr.FactoryID != fac.Factory.UID {
		t.Fatal(sr, err)
	}
	runs, err := f.viewer.Runs.List(ctx, &sdk.RunListOptions{Name: "triage"}).Collect(ctx)
	if err != nil || len(runs) != 1 || runs[0].RunID != sr.RunID {
		t.Fatal(runs, err)
	}

	stream, err := f.viewer.Runs.Stream(ctx, sr.RunID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	go func() {
		time.Sleep(30 * time.Millisecond)
		r, _ := f.store.GetRun(ctx, sr.RunID)
		m := &core.RunMessage{RunID: sr.RunID, Role: "assistant", Content: "done auditing"}
		_ = f.store.AppendRunMessage(ctx, m)
		f.bus.Publish(ctx, core.TopicRunMessage, m)
		r.State, r.Result = core.RunSucceeded, "done auditing"
		_ = f.store.UpdateRun(ctx, r)
		snap := *r
		f.bus.Publish(ctx, core.TopicRunUpdated, &snap)
	}()
	var types []string
	var lastMsg string
	for stream.Next() {
		ev := stream.Event()
		types = append(types, ev.Type)
		if ev.Message != nil {
			lastMsg = ev.Message.Content
		}
		if ev.Type == sdk.EventDone && (ev.Run == nil || ev.Run.State != sdk.StateSucceeded) {
			t.Fatalf("done event = %+v", ev.Run)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(types) < 3 || types[0] != sdk.EventRun || types[len(types)-1] != sdk.EventDone || lastMsg != "done auditing" {
		t.Fatalf("events %v, last message %q", types, lastMsg)
	}
	msgs, err := f.viewer.Runs.Messages(ctx, sr.RunID, 0)
	if err != nil || len(msgs) != 2 {
		t.Fatal(msgs, err)
	}
	final, err := f.viewer.Runs.Wait(ctx, sr.RunID, time.Millisecond, false)
	if err != nil || !final.Done() {
		t.Fatal(final, err)
	}
	// A cancel is accepted in each state; a finished run keeps its state.
	if err := f.viewer.Runs.Cancel(ctx, sr.RunID); err != nil {
		t.Fatalf("cancel finished run: %v", err)
	}
}

func TestPlatformResources(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.client.Secrets.Create(ctx, "API_TOKEN", "", "v", ""); err != nil {
		t.Fatal(err)
	}
	secrets, err := f.viewer.Secrets.List(ctx, "")
	if err != nil || len(secrets) != 1 || secrets[0].Name != "API_TOKEN" {
		t.Fatal(secrets, err)
	}
	if _, err := f.client.Secrets.Update(ctx, "API_TOKEN", "v2", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Secrets.Delete(ctx, "API_TOKEN", ""); err != nil {
		t.Fatal(err)
	}
	name := "ci"
	env, err := f.client.Environments.Create(ctx, sdk.EnvironmentParams{Name: &name, Repos: []string{"github:acme/api"}})
	if err != nil || env.Name != "ci" {
		t.Fatal(env, err)
	}
	mname, murl := "sentry", "https://mcp.sentry.test/mcp"
	reg, err := f.client.MCPServers.Create(ctx, sdk.MCPServerParams{Name: &mname, URL: &murl, OAuth: &sdk.MCPOAuthConfig{Scopes: []string{"read"}}})
	if err != nil || reg.Connection != "proxy" || reg.OAuth == nil || reg.OAuth.Status != "not_connected" {
		t.Fatal(reg, err)
	}
	if list, err := f.viewer.MCPServers.List(ctx); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if b, err := f.viewer.MCPServers.Builtin(ctx); err != nil || len(b) == 0 {
		t.Fatal(b, err)
	}
	if up, err := f.client.MCPServers.Update(ctx, "sentry", sdk.MCPServerParams{RemoveOAuth: true}); err != nil || up.OAuth != nil || up.Connection != "proxy" {
		t.Fatal(up, err)
	}
	if err := f.client.MCPServers.Delete(ctx, "sentry"); err != nil {
		t.Fatal(err)
	}
	key, err := f.viewer.APIKeys.Create(ctx, "script", "")
	if err != nil || key.Key == "" {
		t.Fatal(key, err)
	}
	cur, versions, err := f.viewer.Platform.SchemaVersions(ctx)
	if err != nil || cur != "v1" || len(versions) != 1 {
		t.Fatal(cur, versions, err)
	}
	doc, err := f.viewer.Platform.Schema(ctx, "v1", "factory.schema.json")
	if err != nil || string(doc) == "" {
		t.Fatal(err)
	}
	fac, _ := f.client.Factories.Create(ctx, sdk.CreateFactoryParams{Name: "hooks"})
	wh, err := f.client.Webhooks.Create(ctx, fac.Factory.UID, sdk.CreateWebhookParams{Name: "ci"})
	if err != nil || wh.Secret == "" {
		t.Fatal(wh, err)
	}
	off := false
	if up, err := f.client.Webhooks.Update(ctx, wh.Webhook.ID, sdk.UpdateWebhookParams{Enabled: &off}); err != nil || up.Enabled {
		t.Fatal(up, err)
	}
	if hooks, err := f.viewer.Webhooks.List(ctx, fac.Factory.UID); err != nil || len(hooks) != 1 {
		t.Fatal(hooks, err)
	}
}

func TestRetries(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"resource_unavailable","title":"busy","status":503,"retryable":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"uid":"fac_1","name":"ok"}`))
	}))
	defer ts.Close()
	c := sdk.NewClient(ts.URL, "k", sdk.WithBackoff(time.Millisecond, 2*time.Millisecond))
	got, err := c.Factories.Get(context.Background(), "fac_1")
	if err != nil || got.Name != "ok" || calls.Load() != 3 {
		t.Fatalf("got %v err %v after %d calls", got, err, calls.Load())
	}

	// POST is not retried on a 500 (it may have been processed).
	calls.Store(0)
	ts500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts500.Close()
	c = sdk.NewClient(ts500.URL, "k", sdk.WithBackoff(time.Millisecond, 2*time.Millisecond))
	_, err = c.Runs.Create(context.Background(), sdk.CreateRunParams{Prompt: "x"})
	if sdk.ErrorCode(err) != sdk.CodeInternalError || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}
	// GET is retried up to MaxRetries.
	calls.Store(0)
	c = sdk.NewClient(ts500.URL, "k", sdk.WithBackoff(time.Millisecond, 2*time.Millisecond), sdk.WithMaxRetries(2))
	_, _ = c.Factories.Get(context.Background(), "x")
	if calls.Load() != 3 {
		t.Fatalf("GET attempts = %d", calls.Load())
	}
}
