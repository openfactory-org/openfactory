package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

type nopTaskBackend struct{}

func (nopTaskBackend) Name() string { return "nop" }
func (nopTaskBackend) Start(context.Context, core.Task) (core.TaskStatus, error) {
	return core.TaskStatus{}, core.ErrUnavailable
}
func (nopTaskBackend) Status(context.Context, string) (core.TaskStatus, error) {
	return core.TaskStatus{}, core.ErrNotFound
}
func (nopTaskBackend) Watch(context.Context, string) (<-chan core.TaskStatus, error) {
	return nil, core.ErrNotFound
}
func (nopTaskBackend) Cancel(context.Context, string, int) error       { return nil }
func (nopTaskBackend) Destroy(context.Context, string) error           { return nil }
func (nopTaskBackend) List(context.Context) ([]core.TaskStatus, error) { return nil, nil }
func (nopTaskBackend) Preflight(context.Context) error                 { return nil }

// A team member with the lowest role (user) connects a worker with the ID of
// the team's worker and takes over its connection (and its future tasks).
func TestUserRoleKeyCannotTakeOverWorker(t *testing.T) {
	ctx := t.Context()
	srv, err := New(ctx, testConfig(t), WithLogger(quietLogger()), WithVersion("test"), WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	team := &core.Team{ID: core.NewID("team"), Name: "Acme"}
	if err := srv.Store.CreateTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	mk := func(role core.Role) string {
		u := &core.User{ID: core.NewID("usr"), TeamID: team.ID, Email: string(role) + "@example.test", Name: string(role), Role: role, CreatedAt: time.Now()}
		if err := srv.Store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		k, plain, err := api.NewAPIKey(team.ID, u.ID, "k")
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Store.CreateAPIKey(ctx, k); err != nil {
			t.Fatal(err)
		}
		return plain
	}
	adminKey, userKey := mk(core.RoleAdmin), mk(core.RoleUser)
	run := func(key string) context.CancelFunc {
		wctx, cancel := context.WithCancel(ctx)
		go func() {
			_ = direct.RunWorkerV2(wctx, direct.WorkerV2Options{ServerURL: ts.URL, APIKey: key, WorkerID: "edge-1",
				Backend: nopTaskBackend{}, Logger: quietLogger(), MinBackoff: time.Hour, MaxBackoff: time.Hour})
		}()
		return cancel
	}
	connID := func() string {
		for _, w := range srv.Hub.Workers() {
			if w.Hello.WorkerID == "edge-1" {
				return w.ConnectionID
			}
		}
		return ""
	}
	wait := func(cond func() bool) bool {
		for range 200 {
			if cond() {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	defer run(adminKey)()
	if !wait(func() bool { return connID() != "" }) {
		t.Fatal("admin worker did not connect")
	}
	legit := connID()
	defer run(userKey)()
	if wait(func() bool { c := connID(); return c != "" && c != legit }) {
		t.Fatalf("a key of a user with role %q replaced worker edge-1 (connection %s -> %s)", core.RoleUser, legit, connID())
	}
}
