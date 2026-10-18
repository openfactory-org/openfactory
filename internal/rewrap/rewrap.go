// Package rewrap wraps the data keys of all stored secrets again with the
// current key. It is the work of the command "openfactory secret rewrap".
//
// The package finds the sealed values in three places: the secret store, the
// OAuth tokens of registered MCP servers and the personal tokens of users for
// code forges. It only changes the wrapped data key. The sealed value stays.
package rewrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
	"github.com/openfactory-org/openfactory/internal/mcpregistry"
	"github.com/openfactory-org/openfactory/internal/secrets"
)

// Report counts the results of a run. In a dry run, Rewrapped counts the
// items that a real run would change.
type Report struct {
	Rewrapped int
	Current   int
	// Changed counts the items that a replica wrote or deleted during the
	// run. The run leaves them as they are.
	Changed int
	Failed  int
	// Errors names each failed item and the reason. It holds no secret value.
	Errors []string
}

func (r *Report) fail(item string, err error) {
	r.Failed++
	r.Errors = append(r.Errors, item+": "+err.Error())
}

// Run wraps the data keys of all sealed values again with the current key of
// s. It does not stop at the first failure. When dryRun is true, it writes nothing.
//
// Each write happens only when the stored value is still the value that the
// run read (core.CompareAndSwapper), so a replica that writes or deletes a
// value during the run keeps its change.
func Run(ctx context.Context, db core.Store, s *secrets.Store, dryRun bool) (Report, error) {
	var rep Report
	cas, ok := db.(core.CompareAndSwapper)
	if !ok && !dryRun {
		return rep, errors.New("the store does not support conditional writes")
	}
	teams, err := db.ListTeams(ctx)
	if err != nil {
		return rep, fmt.Errorf("list teams: %w", err)
	}
	for _, t := range teams {
		if err := secretsOf(ctx, db, cas, s, t.ID, dryRun, &rep); err != nil {
			return rep, err
		}
		if err := mcpTokensOf(ctx, db, cas, s, t.ID, dryRun, &rep); err != nil {
			return rep, err
		}
		if err := userTokensOf(ctx, db, cas, s, t.ID, dryRun, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func secretsOf(ctx context.Context, db core.Store, cas core.CompareAndSwapper, s *secrets.Store, teamID string, dryRun bool, rep *Report) error {
	factories, err := db.ListFactories(ctx, teamID, "")
	if err != nil {
		return fmt.Errorf("list factories of team %s: %w", teamID, err)
	}
	scopes := []string{""}
	for _, f := range factories {
		scopes = append(scopes, f.ID)
	}
	for _, scope := range scopes {
		list, err := db.ListSecrets(ctx, teamID, scope)
		if err != nil {
			return fmt.Errorf("list secrets of team %s: %w", teamID, err)
		}
		for _, meta := range list {
			item := "secret " + meta.Name
			if scope != "" {
				item += " (factory " + scope + ")"
			}
			sec, ct, err := db.GetSecret(ctx, teamID, scope, meta.Name)
			if err != nil || sec.FactoryID != scope {
				if err == nil {
					err = errors.New("the secret has a different scope")
				}
				rep.fail(item, err)
				continue
			}
			out, changed, err := s.Rewrap(ctx, sec.TeamID, sec.FactoryID, sec.Name, sec.Type, ct)
			if err != nil {
				rep.fail(item, err)
				continue
			}
			if !changed {
				rep.Current++
				continue
			}
			if dryRun {
				rep.Rewrapped++
				continue
			}
			// A replica can write or delete the secret between the read and
			// the write. The write then does not happen.
			set, err := cas.SecretCompareAndSet(ctx, sec.ID, ct, out)
			switch {
			case err != nil:
				rep.fail(item, err)
			case !set:
				rep.Changed++
			default:
				rep.Rewrapped++
			}
		}
	}
	return nil
}

func mcpTokensOf(ctx context.Context, db core.Store, cas core.CompareAndSwapper, s *secrets.Store, teamID string, dryRun bool, rep *Report) error {
	servers, err := db.ListMCPServers(ctx, teamID)
	if err != nil {
		return fmt.Errorf("list MCP servers of team %s: %w", teamID, err)
	}
	for _, m := range servers {
		key, name, typ := mcpregistry.TokenStorage(m.ID)
		rewrapKV(ctx, db, cas, s, teamID, key, name, typ, "OAuth token of MCP server "+m.Name, dryRun, rep)
	}
	return nil
}

func userTokensOf(ctx context.Context, db core.Store, cas core.CompareAndSwapper, s *secrets.Store, teamID string, dryRun bool, rep *Report) error {
	users, err := db.ListUsers(ctx, teamID)
	if err != nil {
		return fmt.Errorf("list users of team %s: %w", teamID, err)
	}
	for _, u := range users {
		for _, f := range forge.UserTokenForges {
			key, name, typ := forge.UserTokenStorage(teamID, u.ID, f)
			rewrapKV(ctx, db, cas, s, teamID, key, name, typ, "token of user "+u.ID+" for "+forge.IntegrationProvider(f), dryRun, rep)
		}
	}
	return nil
}

// rewrapKV handles one sealed value in the KV space. A missing or empty
// value is not an item.
func rewrapKV(ctx context.Context, db core.Store, cas core.CompareAndSwapper, s *secrets.Store, teamID, key, name string, typ core.SecretType, item string, dryRun bool, rep *Report) {
	raw, err := db.KVGet(ctx, key)
	if errors.Is(err, core.ErrNotFound) || (err == nil && raw == "") {
		return
	}
	if err != nil {
		rep.fail(item, err)
		return
	}
	env, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		rep.fail(item, fmt.Errorf("stored value is not base64: %w", err))
		return
	}
	out, changed, err := s.Rewrap(ctx, teamID, "", name, typ, env)
	if err != nil {
		rep.fail(item, err)
		return
	}
	if !changed {
		rep.Current++
		return
	}
	if dryRun {
		rep.Rewrapped++
		return
	}
	// See secretsOf: a value that changed or was deleted stays as it is.
	set, err := cas.KVCompareAndSet(ctx, key, raw, base64.StdEncoding.EncodeToString(out))
	switch {
	case err != nil:
		rep.fail(item, err)
	case !set:
		rep.Changed++
	default:
		rep.Rewrapped++
	}
}
