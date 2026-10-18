package storetest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
)

// Release schemas: the migration files that shipped with each earlier
// release, with their sha256 per dialect. Released migration files must
// never change (add a new migration instead); ReleaseMigrations fails the
// test when one does. No release exists yet, so TestUpgradeFromEarlierReleases
// has no subtests; TestUpgradeRefusesPreSquashDatabase still runs. Add an
// entry for each release, for example:
//
//	{Name: "v0.1.0", Files: map[string]map[string]string{
//		"sqlite":   {"0001_baseline.sql": "<sha256>"},
//		"postgres": {"0001_baseline.sql": "<sha256>"},
//	}},
var releases = []struct {
	Name  string
	Files map[string]map[string]string // dialect -> file -> sha256
}{}

// Releases is the number of earlier releases with a recorded schema.
func Releases() int { return len(releases) }

// ReleaseName names release i.
func ReleaseName(i int) string { return releases[i].Name }

// ReleaseMigrations returns the migrations of release i, read from the
// dialect's embedded migrations, after checking they did not change.
func ReleaseMigrations(t *testing.T, dialect string, all fs.FS, i int) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	for name, want := range releases[i].Files[dialect] {
		b, err := fs.ReadFile(all, name)
		if err != nil {
			t.Fatalf("release %q: %v", releases[i].Name, err)
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
			t.Fatalf("released migration %s/%s changed (sha256 %x, want %s): add a new migration instead", dialect, name, sum, want)
		}
		out[name] = &fstest.MapFile{Data: b}
	}
	if len(out) == 0 {
		t.Fatalf("release %q has no %s migrations", releases[i].Name, dialect)
	}
	return out
}

// MigrateTo applies the migrations in fsys to s (an *sqlstore.Store).
func MigrateTo(t *testing.T, s core.Store, fsys fs.FS) {
	t.Helper()
	ss, ok := s.(*sqlstore.Store)
	if !ok {
		t.Fatalf("store %T is not an *sqlstore.Store", s)
	}
	if err := ss.Migrate(context.Background(), fsys); err != nil {
		t.Fatal(err)
	}
}

// MakePreSquashDatabase makes db look like a database made before the
// migration history was replaced by one baseline: schema_migrations records
// version 1 as "init", with a later version too.
func MakePreSquashDatabase(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_ns BIGINT NOT NULL)`,
		`INSERT INTO schema_migrations (version, name, applied_ns) VALUES (1, 'init', 1), (2, 'coordination', 2)`,
		`CREATE TABLE teams (id TEXT PRIMARY KEY, name TEXT NOT NULL)`,
	} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("make pre-squash database: %v", err)
		}
	}
}

// RequireOldDatabaseRefused fails the test unless err is the error Open
// returns for a database made by an earlier migration history.
func RequireOldDatabaseRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("opening a pre-squash database succeeded, want it refused")
	}
	if !strings.Contains(err.Error(), `recorded migration 1 as "init"`) || !strings.Contains(err.Error(), "different schema") {
		t.Fatalf("opening a pre-squash database: %v, want the different-schema error", err)
	}
}

// UpgradeFixture is the data seeded into a database at an earlier schema.
type UpgradeFixture struct {
	Team      *core.Team
	User      *core.User
	Key       *core.APIKey
	Factory   *core.Factory
	Version   *core.DefinitionVersion
	WorkItem  *core.WorkItem
	Run       *core.Run
	Messages  []string
	Inbox     *core.InboxItem
	SecretID  string
	KVKey     string
	KVValue   string
	CreatedAt time.Time
}

// SeedForUpgrade writes one of every main record kind into s, which is at
// an earlier release's schema.
func SeedForUpgrade(t *testing.T, s core.Store) *UpgradeFixture {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fx := &UpgradeFixture{CreatedAt: at, KVKey: "upgrade:" + uid("kv"), KVValue: "kept"}

	fx.Team = &core.Team{Name: "Upgrade team", CreatedAt: at}
	ok(t, s.CreateTeam(ctx, fx.Team))
	fx.User = &core.User{TeamID: fx.Team.ID, Email: uid("u") + "@example.test", Name: "Upgrader", Role: core.RoleAdmin, CreatedAt: at}
	ok(t, s.CreateUser(ctx, fx.User))
	fx.Key = &core.APIKey{TeamID: fx.Team.ID, UserID: fx.User.ID, Name: "ci", Prefix: uid("ofk"), HashedKey: "hash", CreatedAt: at}
	ok(t, s.CreateAPIKey(ctx, fx.Key))
	fx.Factory = &core.Factory{TeamID: fx.Team.ID, Name: "Upgrade factory", Alias: uid("bot"), Source: core.DefinitionManaged, CreatedAt: at}
	ok(t, s.CreateFactory(ctx, fx.Factory))
	fx.Version = &core.DefinitionVersion{FactoryID: fx.Factory.ID, Files: map[string]string{"factory.yaml": "schemaVersion: v1\nname: Upgrade factory\n"}, Valid: true, CreatedAt: at}
	ok(t, s.CreateDefinitionVersion(ctx, fx.Version))
	fx.Factory.ActiveVersionID = fx.Version.ID
	ok(t, s.UpdateFactory(ctx, fx.Factory))
	fx.WorkItem = &core.WorkItem{FactoryID: fx.Factory.ID, Title: "Survive the upgrade", Stage: core.StageBuilding, CreatedAt: at, UpdatedAt: at}
	ok(t, s.CreateWorkItem(ctx, fx.WorkItem))
	fx.Run = &core.Run{TeamID: fx.Team.ID, FactoryID: fx.Factory.ID, WorkItemID: fx.WorkItem.ID, AgentName: "foreman", Name: "root",
		Prompt: "do it", State: core.RunIdle, CreatedAt: at}
	ok(t, s.CreateRun(ctx, fx.Run))
	for _, c := range []string{"hello", "working on it", "done"} {
		ok(t, s.AppendRunMessage(ctx, &core.RunMessage{RunID: fx.Run.ID, Role: "assistant", Content: c, CreatedAt: at}))
		fx.Messages = append(fx.Messages, c)
	}
	fx.Inbox = &core.InboxItem{FactoryID: fx.Factory.ID, WorkItemID: fx.WorkItem.ID, Kind: core.InboxApproval, Title: "Approve?", CreatedAt: at}
	ok(t, s.CreateInboxItem(ctx, fx.Inbox))
	sec := &core.Secret{TeamID: fx.Team.ID, Name: "TOKEN", CreatedAt: at}
	ok(t, s.PutSecret(ctx, sec, []byte("ciphertext")))
	fx.SecretID = sec.ID
	ok(t, s.KVSet(ctx, fx.KVKey, fx.KVValue))
	return fx
}

// VerifyUpgrade checks that every seeded record survived the migration to
// the current schema and that the store is fully usable afterwards.
func VerifyUpgrade(t *testing.T, s core.Store, fx *UpgradeFixture) {
	t.Helper()
	ctx := context.Background()

	tm, err := s.GetTeam(ctx, fx.Team.ID)
	ok(t, err)
	eq(t, tm.Name, fx.Team.Name, "team name")
	u, err := s.GetUserByEmail(ctx, fx.User.Email)
	ok(t, err)
	eq(t, u.ID, fx.User.ID, "user id")
	eq(t, u.Role, core.RoleAdmin, "user role")
	k, err := s.GetAPIKeyByPrefix(ctx, fx.Key.Prefix)
	ok(t, err)
	eq(t, k.HashedKey, "hash", "api key hash")
	f, err := s.GetFactory(ctx, fx.Factory.ID)
	ok(t, err)
	eq(t, f.Alias, fx.Factory.Alias, "factory alias")
	eq(t, f.ActiveVersionID, fx.Version.ID, "active version")
	v, err := s.GetDefinitionVersion(ctx, fx.Version.ID)
	ok(t, err)
	eq(t, v.Files["factory.yaml"], fx.Version.Files["factory.yaml"], "definition files")
	wi, err := s.GetWorkItem(ctx, fx.WorkItem.ID)
	ok(t, err)
	eq(t, wi.Stage, core.StageBuilding, "work item stage")
	prevWi := wi.Version
	ok(t, s.UpdateWorkItem(ctx, wi))
	eq(t, wi.Version, prevWi+1, "work item version after an update")
	r, err := s.GetRun(ctx, fx.Run.ID)
	ok(t, err)
	eq(t, r.State, core.RunIdle, "run state")
	eq(t, r.Prompt, "do it", "run prompt")
	prevR := r.Version
	ok(t, s.UpdateRun(ctx, r))
	eq(t, r.Version, prevR+1, "run version after an update")
	msgs, err := s.ListRunMessages(ctx, fx.Run.ID, 0)
	ok(t, err)
	eq(t, len(msgs), len(fx.Messages), "run messages")
	for i, m := range msgs {
		eq(t, m.Content, fx.Messages[i], "run message content")
	}
	in, err := s.GetInboxItem(ctx, fx.Inbox.ID)
	ok(t, err)
	eq(t, in.Title, "Approve?", "inbox title")
	prevIn := in.Version
	ok(t, s.UpdateInboxItem(ctx, in))
	eq(t, in.Version, prevIn+1, "inbox item version after an update")
	sec, ct, err := s.GetSecret(ctx, fx.Team.ID, "", "TOKEN")
	ok(t, err)
	eq(t, sec.ID, fx.SecretID, "secret id")
	eq(t, string(ct), "ciphertext", "secret ciphertext")
	val, err := s.KVGet(ctx, fx.KVKey)
	ok(t, err)
	eq(t, val, fx.KVValue, "kv value")

	// Writes keep working: sequence numbers continue, new rows are accepted.
	m := &core.RunMessage{RunID: fx.Run.ID, Role: "user", Content: "after upgrade", CreatedAt: time.Now().UTC()}
	ok(t, s.AppendRunMessage(ctx, m))
	if m.Seq <= msgs[len(msgs)-1].Seq {
		t.Fatalf("message seq after upgrade = %d, want > %d", m.Seq, msgs[len(msgs)-1].Seq)
	}
	ok(t, s.CreateWorkItem(ctx, &core.WorkItem{FactoryID: fx.Factory.ID, Title: "new"}))

	// Features added after the release work on the upgraded database.
	c, isCoord := s.(core.Coordinator)
	if !isCoord {
		t.Fatalf("upgraded store %T does not implement core.Coordinator", s)
	}
	now := time.Now().UTC()
	got, err := c.AcquireLeadership(ctx, "upgrade-test-"+uid(""), "replica-a", now, now.Add(time.Minute))
	ok(t, err)
	if !got {
		t.Fatal("could not acquire leadership on the upgraded database")
	}
}
