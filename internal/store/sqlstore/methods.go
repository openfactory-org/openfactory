package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

func now() time.Time { return storeutil.Now() }

func defaultID(id *string, prefix string) {
	if *id == "" {
		*id = core.NewID(prefix)
	}
}

func defaultTime(t *time.Time) {
	if t.IsZero() {
		*t = now()
	}
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func normAlias(a string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(a), "@"))
}

// ---------- Teams / users / auth ----------

func (s *Store) CreateTeam(ctx context.Context, t *core.Team) error {
	defaultID(&t.ID, "team")
	defaultTime(&t.CreatedAt)
	r, err := docRow(t.ID, t)
	if err != nil {
		return err
	}
	r.set("created_ns", t.CreatedAt.UnixNano())
	return s.insert(ctx, s.conn(ctx), "teams", r)
}

func (s *Store) GetTeam(ctx context.Context, id string) (*core.Team, error) {
	return getDoc[core.Team](ctx, s, s.conn(ctx), "SELECT data FROM teams WHERE id=?", id)
}

func (s *Store) ListTeams(ctx context.Context) ([]core.Team, error) {
	return listDocs[core.Team](ctx, s, s.conn(ctx), "SELECT data FROM teams ORDER BY created_ns, id")
}

func (s *Store) CreateUser(ctx context.Context, u *core.User) error {
	defaultID(&u.ID, "usr")
	defaultTime(&u.CreatedAt)
	r, err := docRow(u.ID, u)
	if err != nil {
		return err
	}
	r.set("team_id", u.TeamID).set("email", normEmail(u.Email)).set("created_ns", u.CreatedAt.UnixNano())
	return s.insert(ctx, s.conn(ctx), "users", r)
}

func (s *Store) GetUser(ctx context.Context, id string) (*core.User, error) {
	return getDoc[core.User](ctx, s, s.conn(ctx), "SELECT data FROM users WHERE id=?", id)
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*core.User, error) {
	e := normEmail(email)
	if e == "" {
		return nil, core.ErrNotFound
	}
	return getDoc[core.User](ctx, s, s.conn(ctx), "SELECT data FROM users WHERE email=?", e)
}

// UpdateUser replaces a user's record; CreatedAt is preserved.
func (s *Store) UpdateUser(ctx context.Context, u *core.User) error {
	cur, err := s.GetUser(ctx, u.ID)
	if err != nil {
		return err
	}
	v := *u
	v.CreatedAt = cur.CreatedAt
	r, err := docRow(v.ID, &v)
	if err != nil {
		return err
	}
	r.set("team_id", v.TeamID).set("email", normEmail(v.Email)).set("created_ns", v.CreatedAt.UnixNano())
	return s.update(ctx, s.conn(ctx), "users", r)
}

func (s *Store) ListUsers(ctx context.Context, teamID string) ([]core.User, error) {
	w := &where{}
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	return listDocs[core.User](ctx, s, s.conn(ctx), "SELECT data FROM users"+w.sql()+" ORDER BY created_ns, id", w.args...)
}

func (s *Store) UpsertExternalIdentity(ctx context.Context, x *core.ExternalIdentity) error {
	if x.Provider == "" || x.ExternalID == "" {
		return fmt.Errorf("%w: provider and external id are required", core.ErrInvalid)
	}
	data, err := marshal(x)
	if err != nil {
		return err
	}
	r := &row{}
	r.set("provider", x.Provider).set("external_id", x.ExternalID).set("user_id", x.UserID).set("data", data)
	return s.upsert(ctx, s.conn(ctx), "external_identities", []string{"provider", "external_id"}, r)
}

func (s *Store) FindExternalIdentity(ctx context.Context, provider, externalID string) (*core.ExternalIdentity, error) {
	return getDoc[core.ExternalIdentity](ctx, s, s.conn(ctx),
		"SELECT data FROM external_identities WHERE provider=? AND external_id=?", provider, externalID)
}

func (s *Store) CreateAPIKey(ctx context.Context, k *core.APIKey) error {
	defaultID(&k.ID, "key")
	defaultTime(&k.CreatedAt)
	r, err := docRow(k.ID, k)
	if err != nil {
		return err
	}
	r.set("team_id", k.TeamID).set("user_id", k.UserID).set("prefix", k.Prefix).
		set("hashed_key", k.HashedKey).set("created_ns", k.CreatedAt.UnixNano())
	return s.insert(ctx, s.conn(ctx), "api_keys", r)
}

func (s *Store) scanAPIKeys(ctx context.Context, q querier, query string, args ...any) ([]core.APIKey, error) {
	rows, err := q.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := []core.APIKey{}
	for rows.Next() {
		var data []byte
		var hashed string
		if err := rows.Scan(&data, &hashed); err != nil {
			return nil, s.mapErr(err)
		}
		var k core.APIKey
		if err := json.Unmarshal(data, &k); err != nil {
			return nil, fmt.Errorf("%s: decode api key: %w", s.d.Name, err)
		}
		k.HashedKey = hashed
		out = append(out, k)
	}
	return out, s.mapErr(rows.Err())
}

func (s *Store) GetAPIKeyByPrefix(ctx context.Context, prefix string) (*core.APIKey, error) {
	keys, err := s.scanAPIKeys(ctx, s.conn(ctx), "SELECT data, hashed_key FROM api_keys WHERE prefix=?", prefix)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, core.ErrNotFound
	}
	return &keys[0], nil
}

func (s *Store) ListAPIKeys(ctx context.Context, teamID string) ([]core.APIKey, error) {
	w := &where{}
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	return s.scanAPIKeys(ctx, s.conn(ctx), "SELECT data, hashed_key FROM api_keys"+w.sql()+" ORDER BY created_ns, id", w.args...)
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	return s.execOne(ctx, s.conn(ctx), "DELETE FROM api_keys WHERE id=?", id)
}

// DeleteUser implements core.UserDeleter: it deletes the user and its API
// keys in one transaction.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := s.exec(ctx, tx, "DELETE FROM api_keys WHERE user_id=?", id); err != nil {
			return err
		}
		return s.execOne(ctx, tx, "DELETE FROM users WHERE id=?", id)
	})
}

func (s *Store) TouchAPIKey(ctx context.Context, id string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		k, err := getDoc[core.APIKey](ctx, s, tx, "SELECT data FROM api_keys WHERE id=?", id)
		if err != nil {
			return err
		}
		k.LastUsedAt = &at
		r, err := docRow(id, k)
		if err != nil {
			return err
		}
		return s.update(ctx, tx, "api_keys", r)
	})
}

// ---------- Factories & definitions ----------

func factoryRow(f *core.Factory) (*row, error) {
	r, err := docRow(f.ID, f)
	if err != nil {
		return nil, err
	}
	r.set("team_id", f.TeamID).set("alias", normAlias(f.Alias)).set("name", f.Name).
		set("created_ns", f.CreatedAt.UnixNano())
	return r, nil
}

func (s *Store) CreateFactory(ctx context.Context, f *core.Factory) error {
	defaultID(&f.ID, "fac")
	defaultTime(&f.CreatedAt)
	if f.UpdatedAt.IsZero() {
		f.UpdatedAt = f.CreatedAt
	}
	r, err := factoryRow(f)
	if err != nil {
		return err
	}
	return s.insert(ctx, s.conn(ctx), "factories", r)
}

func (s *Store) UpdateFactory(ctx context.Context, f *core.Factory) error {
	f.UpdatedAt = now()
	r, err := factoryRow(f)
	if err != nil {
		return err
	}
	return s.update(ctx, s.conn(ctx), "factories", r)
}

func (s *Store) GetFactory(ctx context.Context, id string) (*core.Factory, error) {
	return getDoc[core.Factory](ctx, s, s.conn(ctx), "SELECT data FROM factories WHERE id=?", id)
}

func (s *Store) GetFactoryByAlias(ctx context.Context, teamID, alias string) (*core.Factory, error) {
	a := normAlias(alias)
	if a == "" {
		return nil, core.ErrNotFound
	}
	w := &where{}
	w.add("alias=?", a)
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	return getDoc[core.Factory](ctx, s, s.conn(ctx), "SELECT data FROM factories"+w.sql()+" ORDER BY created_ns LIMIT 1", w.args...)
}

func (s *Store) ListFactories(ctx context.Context, teamID, search string) ([]core.Factory, error) {
	w := &where{}
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	if search = strings.TrimSpace(search); search != "" {
		p := likePattern(search)
		w.add("(name "+s.d.ILike+" ? ESCAPE '\\' OR alias "+s.d.ILike+" ? ESCAPE '\\')", p, p)
	}
	return listDocs[core.Factory](ctx, s, s.conn(ctx), "SELECT data FROM factories"+w.sql()+" ORDER BY name, id", w.args...)
}

func (s *Store) DeleteFactory(ctx context.Context, id string) error {
	return s.execOne(ctx, s.conn(ctx), "DELETE FROM factories WHERE id=?", id)
}

func defVersionRow(v *core.DefinitionVersion) (*row, error) {
	r, err := docRow(v.ID, v)
	if err != nil {
		return nil, err
	}
	r.set("factory_id", v.FactoryID).set("created_ns", v.CreatedAt.UnixNano())
	return r, nil
}

func (s *Store) CreateDefinitionVersion(ctx context.Context, v *core.DefinitionVersion) error {
	defaultID(&v.ID, "defv")
	defaultTime(&v.CreatedAt)
	r, err := defVersionRow(v)
	if err != nil {
		return err
	}
	return s.insert(ctx, s.conn(ctx), "definition_versions", r)
}

func (s *Store) UpdateDefinitionVersion(ctx context.Context, v *core.DefinitionVersion) error {
	r, err := defVersionRow(v)
	if err != nil {
		return err
	}
	return s.update(ctx, s.conn(ctx), "definition_versions", r)
}

func (s *Store) GetDefinitionVersion(ctx context.Context, id string) (*core.DefinitionVersion, error) {
	return getDoc[core.DefinitionVersion](ctx, s, s.conn(ctx), "SELECT data FROM definition_versions WHERE id=?", id)
}

func (s *Store) ListDefinitionVersions(ctx context.Context, factoryID string, opts core.ListOpts) ([]core.DefinitionVersion, error) {
	w := &where{}
	w.add("factory_id=?", factoryID)
	suffix, err := w.page(opts)
	if err != nil {
		return nil, err
	}
	return listDocs[core.DefinitionVersion](ctx, s, s.conn(ctx), "SELECT data FROM definition_versions"+w.sql()+suffix, w.args...)
}

// ---------- Materialized resources ----------

func (s *Store) requireFactory(ctx context.Context, q querier, id string) error {
	var one int
	err := q.QueryRowContext(ctx, s.rebind("SELECT 1 FROM factories WHERE id=?"), id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: factory %s", core.ErrNotFound, id)
	}
	return s.mapErr(err)
}

func (s *Store) ReplaceFactoryResources(ctx context.Context, factoryID string, agents []core.AgentConfig, automations []core.Automation, scorers []core.Scorer, webhooks []core.Webhook) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.requireFactory(ctx, tx, factoryID); err != nil {
			return err
		}
		// Update each resource by its ID and delete only the resources that
		// are gone, so that rows of unchanged resources stay in place.
		keep := map[string][]string{}
		for _, a := range agents {
			keep["agents"] = append(keep["agents"], a.ID)
		}
		for _, a := range automations {
			keep["automations"] = append(keep["automations"], a.ID)
		}
		for _, sc := range scorers {
			keep["scorers"] = append(keep["scorers"], sc.ID)
		}
		for _, wh := range webhooks {
			keep["webhooks"] = append(keep["webhooks"], wh.ID)
		}
		for _, t := range []string{"agents", "automations", "scorers", "webhooks"} {
			q := "DELETE FROM " + t + " WHERE factory_id=?"
			args := []any{factoryID}
			var ids []string
			for _, id := range keep[t] {
				if id != "" {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				q += " AND id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
				for _, id := range ids {
					args = append(args, id)
				}
			}
			if _, err := s.exec(ctx, tx, q, args...); err != nil {
				return err
			}
		}
		for i, a := range agents {
			a.FactoryID = factoryID
			defaultID(&a.ID, "agt")
			r, err := docRow(a.ID, a)
			if err != nil {
				return err
			}
			r.set("factory_id", factoryID).set("name", a.Name).set("ord", i)
			if err := s.upsert(ctx, tx, "agents", []string{"id"}, r); err != nil {
				return err
			}
		}
		for i, a := range automations {
			a.FactoryID = factoryID
			defaultID(&a.ID, "auto")
			r, err := docRow(a.ID, a)
			if err != nil {
				return err
			}
			r.set("factory_id", factoryID).set("name", a.Name).set("enabled", a.Enabled).set("ord", i)
			if err := s.upsert(ctx, tx, "automations", []string{"id"}, r); err != nil {
				return err
			}
		}
		for i, sc := range scorers {
			sc.FactoryID = factoryID
			defaultID(&sc.ID, "scr")
			r, err := docRow(sc.ID, sc)
			if err != nil {
				return err
			}
			r.set("factory_id", factoryID).set("ord", i)
			if err := s.upsert(ctx, tx, "scorers", []string{"id"}, r); err != nil {
				return err
			}
		}
		for i, wh := range webhooks {
			wh.FactoryID = factoryID
			defaultID(&wh.ID, "wh")
			defaultTime(&wh.CreatedAt)
			r, err := docRow(wh.ID, wh)
			if err != nil {
				return err
			}
			r.set("factory_id", factoryID).set("name", wh.Name).set("ord", i)
			if err := s.upsert(ctx, tx, "webhooks", []string{"id"}, r); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListAgents(ctx context.Context, factoryID string) ([]core.AgentConfig, error) {
	return listDocs[core.AgentConfig](ctx, s, s.conn(ctx), "SELECT data FROM agents WHERE factory_id=? ORDER BY ord", factoryID)
}

func (s *Store) GetAgent(ctx context.Context, id string) (*core.AgentConfig, error) {
	return getDoc[core.AgentConfig](ctx, s, s.conn(ctx), "SELECT data FROM agents WHERE id=?", id)
}

func (s *Store) GetAgentByName(ctx context.Context, factoryID, name string) (*core.AgentConfig, error) {
	return getDoc[core.AgentConfig](ctx, s, s.conn(ctx), "SELECT data FROM agents WHERE factory_id=? AND name=?", factoryID, name)
}

func (s *Store) ListAutomations(ctx context.Context, factoryID string) ([]core.Automation, error) {
	return listDocs[core.Automation](ctx, s, s.conn(ctx), "SELECT data FROM automations WHERE factory_id=? ORDER BY ord", factoryID)
}

func (s *Store) ListAllEnabledAutomations(ctx context.Context) ([]core.Automation, error) {
	return listDocs[core.Automation](ctx, s, s.conn(ctx), "SELECT data FROM automations WHERE enabled=? ORDER BY factory_id, ord", true)
}

func (s *Store) ListScorers(ctx context.Context, factoryID string) ([]core.Scorer, error) {
	return listDocs[core.Scorer](ctx, s, s.conn(ctx), "SELECT data FROM scorers WHERE factory_id=? ORDER BY ord", factoryID)
}

func (s *Store) ListWebhooks(ctx context.Context, factoryID string) ([]core.Webhook, error) {
	return listDocs[core.Webhook](ctx, s, s.conn(ctx), "SELECT data FROM webhooks WHERE factory_id=? ORDER BY ord", factoryID)
}

func (s *Store) GetWebhook(ctx context.Context, id string) (*core.Webhook, error) {
	return getDoc[core.Webhook](ctx, s, s.conn(ctx), "SELECT data FROM webhooks WHERE id=?", id)
}

// ---------- Work items ----------

func workItemRow(w *core.WorkItem) (*row, error) {
	w.SourceKey = storeutil.SourceKey(w)
	r, err := docRow(w.ID, w)
	if err != nil {
		return nil, err
	}
	r.set("factory_id", w.FactoryID).set("stage", string(w.Stage)).set("source_key", w.SourceKey).
		set("creator_id", w.CreatorID).set("title", w.Title).set("created_ns", w.CreatedAt.UnixNano()).
		set("version", w.Version)
	return r, nil
}

func (s *Store) CreateWorkItem(ctx context.Context, w *core.WorkItem) error {
	defaultID(&w.ID, "wi")
	defaultTime(&w.CreatedAt)
	if w.UpdatedAt.IsZero() {
		w.UpdatedAt = w.CreatedAt
	}
	if w.Stage == "" {
		w.Stage = core.StageIntake
	}
	w.Version = 1
	r, err := workItemRow(w)
	if err == nil {
		err = s.insert(ctx, s.conn(ctx), "work_items", r)
	}
	if err != nil {
		w.Version = 0
	}
	return err
}

// UpdateWorkItem implements the version check of core.Store.UpdateWorkItem.
// The version is kept in the version column and in the JSON document, so
// reads of the document return it.
func (s *Store) UpdateWorkItem(ctx context.Context, w *core.WorkItem) error {
	old, oldUpdated := w.Version, w.UpdatedAt
	w.UpdatedAt = now()
	w.Version = old + 1
	r, err := workItemRow(w)
	if err == nil {
		err = s.updateVersioned(ctx, "work_items", "work item", r, w.ID, old)
	}
	if err != nil {
		w.Version, w.UpdatedAt = old, oldUpdated
	}
	return err
}

func (s *Store) GetWorkItem(ctx context.Context, id string) (*core.WorkItem, error) {
	return getDoc[core.WorkItem](ctx, s, s.conn(ctx), "SELECT data FROM work_items WHERE id=?", id)
}

func (s *Store) FindWorkItemBySource(ctx context.Context, factoryID, sourceKey string) (*core.WorkItem, error) {
	if sourceKey == "" {
		return nil, core.ErrNotFound
	}
	return getDoc[core.WorkItem](ctx, s, s.conn(ctx),
		"SELECT data FROM work_items WHERE factory_id=? AND source_key=? AND stage NOT IN (?,?) ORDER BY created_ns DESC, id DESC LIMIT 1",
		factoryID, sourceKey, string(core.StageComplete), string(core.StageCancelled))
}

func (s *Store) ListWorkItems(ctx context.Context, f core.WorkItemFilter) ([]core.WorkItem, error) {
	w := &where{}
	if f.FactoryID != "" {
		w.add("factory_id=?", f.FactoryID)
	}
	stages := make([]string, len(f.Stages))
	for i, st := range f.Stages {
		stages[i] = string(st)
	}
	w.in("stage", stages)
	if f.CreatorID != "" {
		w.add("creator_id=?", f.CreatorID)
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		p := likePattern(q)
		w.add("(title "+s.d.ILike+" ? ESCAPE '\\' OR source_key "+s.d.ILike+" ? ESCAPE '\\' OR id=?)", p, p, q)
	}
	if f.Since != nil {
		w.add("created_ns>=?", f.Since.UnixNano())
	}
	suffix, err := w.page(f.ListOpts)
	if err != nil {
		return nil, err
	}
	return listDocs[core.WorkItem](ctx, s, s.conn(ctx), "SELECT data FROM work_items"+w.sql()+suffix, w.args...)
}

func (s *Store) AppendWorkItemEvent(ctx context.Context, e *core.WorkItemEvent) error {
	defaultID(&e.ID, "wie")
	defaultTime(&e.CreatedAt)
	r, err := docRow(e.ID, e)
	if err != nil {
		return err
	}
	r.set("work_item_id", e.WorkItemID).set("created_ns", e.CreatedAt.UnixNano())
	return s.insert(ctx, s.conn(ctx), "work_item_events", r)
}

func (s *Store) ListWorkItemEvents(ctx context.Context, workItemID string) ([]core.WorkItemEvent, error) {
	return listDocs[core.WorkItemEvent](ctx, s, s.conn(ctx), "SELECT data FROM work_item_events WHERE work_item_id=? ORDER BY seq", workItemID)
}

// ---------- Runs ----------

func runRow(r *core.Run) (*row, error) {
	rr, err := docRow(r.ID, r)
	if err != nil {
		return nil, err
	}
	rr.set("team_id", r.TeamID).set("factory_id", r.FactoryID).set("work_item_id", r.WorkItemID).
		set("parent_run_id", r.ParentRunID).set("agent_name", r.AgentName).set("name", r.Name).
		set("state", string(r.State)).set("worker_host", storeutil.WorkerHost(r.Config.WorkerHost)).set("worker_id", r.WorkerID).
		set("created_ns", r.CreatedAt.UnixNano()).set("finished_ns", storeutil.NS(r.FinishedAt)).
		set("version", r.Version).set("creator_id", r.CreatorID())
	return rr, nil
}

func (s *Store) CreateRun(ctx context.Context, r *core.Run) error {
	if r.FactoryID == "" {
		return fmt.Errorf("%w: a run needs a factory", core.ErrInvalid)
	}
	defaultID(&r.ID, "run")
	defaultTime(&r.CreatedAt)
	if r.State == "" {
		r.State = core.RunQueued
	}
	r.Version = 1
	rr, err := runRow(r)
	if err == nil {
		err = s.insert(ctx, s.conn(ctx), "runs", rr)
	}
	if err != nil {
		r.Version = 0
	}
	return err
}

// UpdateRun implements the version check of core.Store.UpdateRun.
func (s *Store) UpdateRun(ctx context.Context, r *core.Run) error {
	old := r.Version
	r.Version = old + 1
	rr, err := runRow(r)
	if err == nil {
		err = s.updateVersioned(ctx, "runs", "run", rr, r.ID, old)
	}
	if err != nil {
		r.Version = old
	}
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (*core.Run, error) {
	var data []byte
	var version int64
	if err := s.conn(ctx).QueryRowContext(ctx, s.rebind("SELECT data, version FROM runs WHERE id=?"), id).Scan(&data, &version); err != nil {
		return nil, s.mapErr(err)
	}
	r := new(core.Run)
	if err := json.Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("%s: decode record: %w", s.d.Name, err)
	}
	r.Version = version
	return r, nil
}

func (s *Store) ListRuns(ctx context.Context, f core.RunFilter) ([]core.Run, error) {
	w := &where{}
	for _, c := range []struct{ col, val string }{
		{"team_id", f.TeamID}, {"factory_id", f.FactoryID}, {"work_item_id", f.WorkItemID},
		{"parent_run_id", f.ParentRunID}, {"agent_name", f.AgentName}, {"name", f.Name},
		{"creator_id", f.CreatorID},
	} {
		if c.val != "" {
			w.add(c.col+"=?", c.val)
		}
	}
	states := make([]string, len(f.States))
	for i, st := range f.States {
		states[i] = string(st)
	}
	w.in("state", states)
	if f.Since != nil {
		w.add("created_ns>=?", f.Since.UnixNano())
	}
	suffix, err := w.page(f.ListOpts)
	if err != nil {
		return nil, err
	}
	rows, err := s.conn(ctx).QueryContext(ctx, s.rebind("SELECT data, version FROM runs"+w.sql()+suffix), w.args...)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := []core.Run{}
	for rows.Next() {
		var data []byte
		var version int64
		if err := rows.Scan(&data, &version); err != nil {
			return nil, s.mapErr(err)
		}
		var r core.Run
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("%s: decode record: %w", s.d.Name, err)
		}
		r.Version = version
		out = append(out, r)
	}
	return out, s.mapErr(rows.Err())
}

func (s *Store) AppendRunMessage(ctx context.Context, m *core.RunMessage) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var seq int64
		err := tx.QueryRowContext(ctx, s.rebind("UPDATE runs SET msg_seq=msg_seq+1 WHERE id=? RETURNING msg_seq"), m.RunID).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: run %s", core.ErrNotFound, m.RunID)
		}
		if err != nil {
			return s.mapErr(err)
		}
		msg := *m
		msg.Seq = seq
		defaultID(&msg.ID, "msg")
		defaultTime(&msg.CreatedAt)
		data, err := marshal(&msg)
		if err != nil {
			return err
		}
		r := &row{}
		r.set("id", msg.ID).set("run_id", msg.RunID).set("seq", seq).set("data", data)
		if err := s.insert(ctx, tx, "run_messages", r); err != nil {
			return err
		}
		*m = msg
		return nil
	})
}

func (s *Store) ListRunMessages(ctx context.Context, runID string, afterSeq int64) ([]core.RunMessage, error) {
	return listDocs[core.RunMessage](ctx, s, s.conn(ctx), "SELECT data FROM run_messages WHERE run_id=? AND seq>? ORDER BY seq", runID, afterSeq)
}

// ---------- Inbox ----------

func inboxRow(i *core.InboxItem) (*row, error) {
	r, err := docRow(i.ID, i)
	if err != nil {
		return nil, err
	}
	r.set("factory_id", i.FactoryID).set("work_item_id", i.WorkItemID).set("run_id", i.RunID).
		set("resolved", i.Resolved).set("created_ns", i.CreatedAt.UnixNano()).set("resolved_ns", storeutil.NS(i.ResolvedAt)).
		set("version", i.Version)
	return r, nil
}

func (s *Store) CreateInboxItem(ctx context.Context, i *core.InboxItem) error {
	defaultID(&i.ID, "inbox")
	defaultTime(&i.CreatedAt)
	i.Version = 1
	r, err := inboxRow(i)
	if err == nil {
		err = s.insert(ctx, s.conn(ctx), "inbox_items", r)
	}
	if err != nil {
		i.Version = 0
	}
	return err
}

// UpdateInboxItem implements the version check of core.Store.UpdateInboxItem.
func (s *Store) UpdateInboxItem(ctx context.Context, i *core.InboxItem) error {
	old := i.Version
	i.Version = old + 1
	r, err := inboxRow(i)
	if err == nil {
		err = s.updateVersioned(ctx, "inbox_items", "inbox item", r, i.ID, old)
	}
	if err != nil {
		i.Version = old
	}
	return err
}

func (s *Store) GetInboxItem(ctx context.Context, id string) (*core.InboxItem, error) {
	return getDoc[core.InboxItem](ctx, s, s.conn(ctx), "SELECT data FROM inbox_items WHERE id=?", id)
}

func (s *Store) ListInboxItems(ctx context.Context, factoryID string, includeResolved bool) ([]core.InboxItem, error) {
	w := &where{}
	if factoryID != "" {
		w.add("factory_id=?", factoryID)
	}
	if !includeResolved {
		w.add("resolved=?", false)
	}
	return listDocs[core.InboxItem](ctx, s, s.conn(ctx), "SELECT data FROM inbox_items"+w.sql()+" ORDER BY created_ns DESC, id DESC", w.args...)
}

// ---------- Secrets ----------

func (s *Store) PutSecret(ctx context.Context, sec *core.Secret, ciphertext []byte) error {
	if sec.TeamID == "" || sec.Name == "" {
		return fmt.Errorf("%w: secret team and name are required", core.ErrInvalid)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		existing, err := getDoc[core.Secret](ctx, s, tx,
			"SELECT data FROM secrets WHERE team_id=? AND factory_id=? AND name=?", sec.TeamID, sec.FactoryID, sec.Name)
		switch {
		case err == nil:
			sec.ID = existing.ID
			sec.CreatedAt = existing.CreatedAt
			sec.UpdatedAt = now()
		case errors.Is(err, core.ErrNotFound):
			defaultID(&sec.ID, "sec")
			defaultTime(&sec.CreatedAt)
			if sec.UpdatedAt.IsZero() {
				sec.UpdatedAt = sec.CreatedAt
			}
		default:
			return err
		}
		if sec.Type == "" {
			sec.Type = core.SecretGeneric
		}
		r, err := docRow(sec.ID, sec)
		if err != nil {
			return err
		}
		r.set("team_id", sec.TeamID).set("factory_id", sec.FactoryID).set("name", sec.Name).
			set("ciphertext", ciphertext).set("created_ns", sec.CreatedAt.UnixNano())
		if existing != nil {
			return s.update(ctx, tx, "secrets", r)
		}
		return s.insert(ctx, tx, "secrets", r)
	})
}

func (s *Store) getSecret(ctx context.Context, teamID, factoryID, name string) (*core.Secret, []byte, error) {
	var data, ct []byte
	err := s.conn(ctx).QueryRowContext(ctx, s.rebind("SELECT data, ciphertext FROM secrets WHERE team_id=? AND factory_id=? AND name=?"),
		teamID, factoryID, name).Scan(&data, &ct)
	if err != nil {
		return nil, nil, s.mapErr(err)
	}
	var sec core.Secret
	if err := json.Unmarshal(data, &sec); err != nil {
		return nil, nil, fmt.Errorf("%s: decode secret: %w", s.d.Name, err)
	}
	return &sec, ct, nil
}

func (s *Store) GetSecret(ctx context.Context, teamID, factoryID, name string) (*core.Secret, []byte, error) {
	if factoryID != "" {
		sec, ct, err := s.getSecret(ctx, teamID, factoryID, name)
		if !errors.Is(err, core.ErrNotFound) {
			return sec, ct, err
		}
	}
	return s.getSecret(ctx, teamID, "", name)
}

func (s *Store) ListSecrets(ctx context.Context, teamID, factoryID string) ([]core.Secret, error) {
	return listDocs[core.Secret](ctx, s, s.conn(ctx), "SELECT data FROM secrets WHERE team_id=? AND factory_id=? ORDER BY name", teamID, factoryID)
}

func (s *Store) DeleteSecret(ctx context.Context, id string) error {
	return s.execOne(ctx, s.conn(ctx), "DELETE FROM secrets WHERE id=?", id)
}

// ---------- Webhook deliveries ----------

func deliveryRow(d *core.WebhookDelivery) (*row, error) {
	r, err := docRow(d.ID, d)
	if err != nil {
		return nil, err
	}
	dup := 0
	if d.Status == core.WebhookDeliveryDeduplicated {
		dup = 1 // outside the unique (webhook_id, delivery_id) index
	}
	r.set("webhook_id", d.WebhookID).set("delivery_id", d.DeliveryID).set("received_ns", d.ReceivedAt.UnixNano()).set("duplicate", dup)
	return r, nil
}

func (s *Store) CreateWebhookDelivery(ctx context.Context, d *core.WebhookDelivery) error {
	defaultID(&d.ID, "whd")
	defaultTime(&d.ReceivedAt)
	r, err := deliveryRow(d)
	if err != nil {
		return err
	}
	return s.insert(ctx, s.conn(ctx), "webhook_deliveries", r)
}

func (s *Store) UpdateWebhookDelivery(ctx context.Context, d *core.WebhookDelivery) error {
	r, err := deliveryRow(d)
	if err != nil {
		return err
	}
	return s.update(ctx, s.conn(ctx), "webhook_deliveries", r)
}

func (s *Store) FindWebhookDelivery(ctx context.Context, webhookID, deliveryID string) (*core.WebhookDelivery, error) {
	if deliveryID == "" {
		return nil, core.ErrNotFound
	}
	return getDoc[core.WebhookDelivery](ctx, s, s.conn(ctx),
		"SELECT data FROM webhook_deliveries WHERE webhook_id=? AND delivery_id=? ORDER BY received_ns DESC LIMIT 1", webhookID, deliveryID)
}

func (s *Store) ListWebhookDeliveries(ctx context.Context, webhookID string, limit int) ([]core.WebhookDelivery, error) {
	return listDocs[core.WebhookDelivery](ctx, s, s.conn(ctx),
		fmt.Sprintf("SELECT data FROM webhook_deliveries WHERE webhook_id=? ORDER BY received_ns DESC, id DESC LIMIT %d", storeutil.Limit(limit)), webhookID)
}

func (s *Store) GetWebhookDelivery(ctx context.Context, id string) (*core.WebhookDelivery, error) {
	return getDoc[core.WebhookDelivery](ctx, s, s.conn(ctx), "SELECT data FROM webhook_deliveries WHERE id=?", id)
}

// ---------- Integrations ----------

func (s *Store) UpsertIntegration(ctx context.Context, i *core.Integration) error {
	defaultID(&i.ID, "int")
	return s.tx(ctx, func(tx *sql.Tx) error {
		if i.CreatedAt.IsZero() {
			existing, err := getDoc[core.Integration](ctx, s, tx, "SELECT data FROM integrations WHERE id=?", i.ID)
			switch {
			case err == nil:
				i.CreatedAt = existing.CreatedAt
			case errors.Is(err, core.ErrNotFound):
				i.CreatedAt = now()
			default:
				return err
			}
		}
		r, err := docRow(i.ID, i)
		if err != nil {
			return err
		}
		r.set("team_id", i.TeamID).set("provider", i.Provider).set("created_ns", i.CreatedAt.UnixNano())
		return s.upsert(ctx, tx, "integrations", []string{"id"}, r)
	})
}

func (s *Store) ListIntegrations(ctx context.Context, teamID string) ([]core.Integration, error) {
	w := &where{}
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	return listDocs[core.Integration](ctx, s, s.conn(ctx), "SELECT data FROM integrations"+w.sql()+" ORDER BY created_ns, id", w.args...)
}

func (s *Store) GetIntegration(ctx context.Context, id string) (*core.Integration, error) {
	return getDoc[core.Integration](ctx, s, s.conn(ctx), "SELECT data FROM integrations WHERE id=?", id)
}

func (s *Store) DeleteIntegration(ctx context.Context, id string) error {
	return s.execOne(ctx, s.conn(ctx), "DELETE FROM integrations WHERE id=?", id)
}

func (s *Store) SetFactoryIntegrations(ctx context.Context, factoryID string, providers []string, settings map[string]map[string]string) error {
	set := map[string]bool{}
	for _, p := range providers {
		if p != "" {
			set[p] = true
		}
	}
	for p := range settings {
		if p != "" {
			set[p] = true
		}
	}
	names := make([]string, 0, len(set))
	for p := range set {
		names = append(names, p)
	}
	sort.Strings(names)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.requireFactory(ctx, tx, factoryID); err != nil {
			return err
		}
		if _, err := s.exec(ctx, tx, "DELETE FROM factory_integrations WHERE factory_id=?", factoryID); err != nil {
			return err
		}
		for _, p := range names {
			cfg := settings[p]
			if cfg == nil {
				cfg = map[string]string{}
			}
			data, err := marshal(cfg)
			if err != nil {
				return err
			}
			r := &row{}
			r.set("factory_id", factoryID).set("provider", p).set("settings", data)
			if err := s.insert(ctx, tx, "factory_integrations", r); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetFactoryIntegrations(ctx context.Context, factoryID string) (map[string]map[string]string, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, s.rebind("SELECT provider, settings FROM factory_integrations WHERE factory_id=?"), factoryID)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var p string
		var data []byte
		if err := rows.Scan(&p, &data); err != nil {
			return nil, s.mapErr(err)
		}
		cfg := map[string]string{}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("%s: decode integration settings: %w", s.d.Name, err)
		}
		out[p] = cfg
	}
	return out, s.mapErr(rows.Err())
}

// ---------- Scoring ----------

// scoreFactory resolves the factory a score belongs to (via its scorer, then
// its run) so scores stay queryable after scorers are replaced.
func (s *Store) scoreFactory(ctx context.Context, q querier, sc *core.Score) (string, error) {
	for _, l := range []struct{ query, id string }{
		{"SELECT factory_id FROM scorers WHERE id=?", sc.ScorerID},
		{"SELECT factory_id FROM runs WHERE id=?", sc.RunID},
	} {
		if l.id == "" {
			continue
		}
		var f string
		err := q.QueryRowContext(ctx, s.rebind(l.query), l.id).Scan(&f)
		if err == nil && f != "" {
			return f, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", s.mapErr(err)
		}
	}
	return "", nil
}

func (s *Store) CreateScore(ctx context.Context, sc *core.Score) error {
	defaultID(&sc.ID, "score")
	defaultTime(&sc.CreatedAt)
	factoryID, err := s.scoreFactory(ctx, s.conn(ctx), sc)
	if err != nil {
		return err
	}
	r, err := docRow(sc.ID, sc)
	if err != nil {
		return err
	}
	r.set("factory_id", factoryID).set("scorer_id", sc.ScorerID).set("run_id", sc.RunID).set("created_ns", sc.CreatedAt.UnixNano())
	return s.insert(ctx, s.conn(ctx), "scores", r)
}

func (s *Store) UpdateScore(ctx context.Context, sc *core.Score) error {
	r, err := docRow(sc.ID, sc)
	if err != nil {
		return err
	}
	r.set("scorer_id", sc.ScorerID).set("run_id", sc.RunID).set("created_ns", sc.CreatedAt.UnixNano())
	return s.update(ctx, s.conn(ctx), "scores", r)
}

func (s *Store) ListScores(ctx context.Context, factoryID, scorerID string, since *time.Time) ([]core.Score, error) {
	w := &where{}
	if factoryID != "" {
		w.add("factory_id=?", factoryID)
	}
	if scorerID != "" {
		w.add("scorer_id=?", scorerID)
	}
	if since != nil {
		w.add("created_ns>=?", since.UnixNano())
	}
	return listDocs[core.Score](ctx, s, s.conn(ctx), "SELECT data FROM scores"+w.sql()+" ORDER BY created_ns DESC, id DESC", w.args...)
}

// GetScore returns the most recent score scorerID gave runID.
func (s *Store) GetScore(ctx context.Context, scorerID, runID string) (*core.Score, error) {
	return getDoc[core.Score](ctx, s, s.conn(ctx), "SELECT data FROM scores WHERE scorer_id=? AND run_id=? ORDER BY created_ns DESC, id DESC LIMIT 1", scorerID, runID)
}

func benchRow(b *core.BenchmarkRun) (*row, error) {
	r, err := docRow(b.ID, b)
	if err != nil {
		return nil, err
	}
	r.set("factory_id", b.FactoryID).set("created_ns", b.CreatedAt.UnixNano())
	return r, nil
}

func (s *Store) CreateBenchmarkRun(ctx context.Context, b *core.BenchmarkRun) error {
	defaultID(&b.ID, "bench")
	defaultTime(&b.CreatedAt)
	r, err := benchRow(b)
	if err != nil {
		return err
	}
	return s.insert(ctx, s.conn(ctx), "benchmark_runs", r)
}

func (s *Store) UpdateBenchmarkRun(ctx context.Context, b *core.BenchmarkRun) error {
	r, err := benchRow(b)
	if err != nil {
		return err
	}
	return s.update(ctx, s.conn(ctx), "benchmark_runs", r)
}

func (s *Store) GetBenchmarkRun(ctx context.Context, id string) (*core.BenchmarkRun, error) {
	return getDoc[core.BenchmarkRun](ctx, s, s.conn(ctx), "SELECT data FROM benchmark_runs WHERE id=?", id)
}

func (s *Store) ListBenchmarkRuns(ctx context.Context, factoryID string) ([]core.BenchmarkRun, error) {
	w := &where{}
	if factoryID != "" {
		w.add("factory_id=?", factoryID)
	}
	return listDocs[core.BenchmarkRun](ctx, s, s.conn(ctx), "SELECT data FROM benchmark_runs"+w.sql()+" ORDER BY created_ns DESC, id DESC", w.args...)
}

// ---------- Workers & environments ----------

func (s *Store) UpsertWorker(ctx context.Context, wk *core.Worker) error {
	defaultID(&wk.ID, "wkr")
	r, err := docRow(wk.ID, wk)
	if err != nil {
		return err
	}
	r.set("team_id", wk.TeamID).set("name", wk.Name)
	return s.upsert(ctx, s.conn(ctx), "workers", []string{"id"}, r)
}

func (s *Store) GetWorker(ctx context.Context, id string) (*core.Worker, error) {
	return getDoc[core.Worker](ctx, s, s.conn(ctx), "SELECT data FROM workers WHERE id=?", id)
}

func (s *Store) ListWorkers(ctx context.Context, teamID string) ([]core.Worker, error) {
	w := &where{}
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	return listDocs[core.Worker](ctx, s, s.conn(ctx), "SELECT data FROM workers"+w.sql()+" ORDER BY name, id", w.args...)
}

func envRow(e *core.Environment) (*row, error) {
	r, err := docRow(e.ID, e)
	if err != nil {
		return nil, err
	}
	r.set("team_id", e.TeamID).set("name", e.Name)
	return r, nil
}

func (s *Store) CreateEnvironment(ctx context.Context, e *core.Environment) error {
	defaultID(&e.ID, "env")
	r, err := envRow(e)
	if err != nil {
		return err
	}
	return s.insert(ctx, s.conn(ctx), "environments", r)
}

func (s *Store) UpdateEnvironment(ctx context.Context, e *core.Environment) error {
	r, err := envRow(e)
	if err != nil {
		return err
	}
	return s.update(ctx, s.conn(ctx), "environments", r)
}

func (s *Store) GetEnvironment(ctx context.Context, id string) (*core.Environment, error) {
	return getDoc[core.Environment](ctx, s, s.conn(ctx), "SELECT data FROM environments WHERE id=?", id)
}

func (s *Store) ListEnvironments(ctx context.Context, teamID string) ([]core.Environment, error) {
	w := &where{}
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	return listDocs[core.Environment](ctx, s, s.conn(ctx), "SELECT data FROM environments"+w.sql()+" ORDER BY name, id", w.args...)
}

func (s *Store) DeleteEnvironment(ctx context.Context, id string) error {
	return s.execOne(ctx, s.conn(ctx), "DELETE FROM environments WHERE id=?", id)
}

// ---------- MCP server registry ----------

func mcpServerRow(m *core.RegisteredMCPServer) (*row, error) {
	r, err := docRow(m.ID, m)
	if err != nil {
		return nil, err
	}
	r.set("team_id", m.TeamID).set("name", m.Name)
	return r, nil
}

func (s *Store) CreateMCPServer(ctx context.Context, m *core.RegisteredMCPServer) error {
	defaultID(&m.ID, "mcp")
	r, err := mcpServerRow(m)
	if err != nil {
		return err
	}
	return withConflictMessage(s.insert(ctx, s.conn(ctx), "mcp_servers", r), fmt.Sprintf("an MCP server named %q already exists", m.Name))
}

func (s *Store) UpdateMCPServer(ctx context.Context, m *core.RegisteredMCPServer) error {
	r, err := mcpServerRow(m)
	if err != nil {
		return err
	}
	return withConflictMessage(s.update(ctx, s.conn(ctx), "mcp_servers", r), fmt.Sprintf("an MCP server named %q already exists", m.Name))
}

func (s *Store) GetMCPServer(ctx context.Context, id string) (*core.RegisteredMCPServer, error) {
	return getDoc[core.RegisteredMCPServer](ctx, s, s.conn(ctx), "SELECT data FROM mcp_servers WHERE id=?", id)
}

func (s *Store) ListMCPServers(ctx context.Context, teamID string) ([]core.RegisteredMCPServer, error) {
	w := &where{}
	if teamID != "" {
		w.add("team_id=?", teamID)
	}
	return listDocs[core.RegisteredMCPServer](ctx, s, s.conn(ctx), "SELECT data FROM mcp_servers"+w.sql()+" ORDER BY name, id", w.args...)
}

func (s *Store) DeleteMCPServer(ctx context.Context, id string) error {
	return s.execOne(ctx, s.conn(ctx), "DELETE FROM mcp_servers WHERE id=?", id)
}

// ---------- KV ----------

func (s *Store) KVGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.conn(ctx).QueryRowContext(ctx, s.rebind("SELECT value FROM kv WHERE key=?"), key).Scan(&v)
	if err != nil {
		return "", s.mapErr(err)
	}
	return v, nil
}

func (s *Store) KVSet(ctx context.Context, key, value string) error {
	r := &row{}
	r.set("key", key).set("value", value).set("updated_ns", time.Now().UnixNano())
	return s.upsert(ctx, s.conn(ctx), "kv", []string{"key"}, r)
}

// ---------- Retention ----------

// PruneBefore implements storeutil.Pruner.
func (s *Store) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	ns := cutoff.UnixNano()
	var total int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		stmts := []struct {
			q    string
			args []any
		}{
			{"DELETE FROM run_messages WHERE run_id IN (SELECT id FROM runs WHERE state IN (?,?,?) AND finished_ns>0 AND finished_ns<?)",
				[]any{string(core.RunSucceeded), string(core.RunFailed), string(core.RunCancelled), ns}},
			{"DELETE FROM runs WHERE state IN (?,?,?) AND finished_ns>0 AND finished_ns<?",
				[]any{string(core.RunSucceeded), string(core.RunFailed), string(core.RunCancelled), ns}},
			{"DELETE FROM inbox_items WHERE resolved=? AND resolved_ns>0 AND resolved_ns<?", []any{true, ns}},
			{"DELETE FROM webhook_deliveries WHERE received_ns<?", []any{ns}},
		}
		for _, st := range stmts {
			res, err := s.exec(ctx, tx, st.q, st.args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return nil
	})
	return total, err
}
