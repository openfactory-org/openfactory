package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Service accounts. A service account is a
// principal that is not a person: a CI job or a bot. It has a role and API
// keys (prefix ofa_), and no email and no sign-in. It is stored as a user of
// kind service_account. Only an admin creates, changes or deletes one, and
// only an admin reads them.

// serviceAccountView is a service account in API responses.
type serviceAccountView struct {
	ID           string               `json:"id"`
	TeamID       string               `json:"team_id"`
	Name         string               `json:"name"`
	Role         core.Role            `json:"role"`
	FactoryRoles map[string]core.Role `json:"factory_roles,omitempty"`
	CreatedAt    timeT                `json:"created_at"`
}

func viewServiceAccount(u *core.User) serviceAccountView {
	return serviceAccountView{ID: u.ID, TeamID: u.TeamID, Name: u.Name, Role: core.NormalizeRole(u.Role), FactoryRoles: u.FactoryRoles, CreatedAt: u.CreatedAt}
}

// serviceAccountFor returns the service account id (or name) of the team of p.
func (s *Server) serviceAccountFor(ctx context.Context, p *Principal, ref string) (*core.User, error) {
	u, err := s.findServiceAccount(ctx, p, ref)
	if err == nil && u == nil {
		err = errNotFound("service account " + ref)
	}
	return u, err
}

// findServiceAccount is serviceAccountFor that returns nil, nil when the
// account does not exist.
func (s *Server) findServiceAccount(ctx context.Context, p *Principal, ref string) (*core.User, error) {
	u, err := s.opts.Store.GetUser(ctx, ref)
	if err == nil && u.TeamID == p.TeamID && u.IsServiceAccount() {
		return u, nil
	}
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	list, err := s.opts.Store.ListUsers(ctx, p.TeamID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].IsServiceAccount() && list[i].Name == ref {
			return &list[i], nil
		}
	}
	return nil, nil
}

func validServiceAccountName(n string) error {
	if n == "" || len(n) > 64 || strings.ContainsAny(n, " \t\n/:") {
		return errInvalid("name must be 1-64 characters without spaces, '/' or ':'")
	}
	return nil
}

func (s *Server) handleListServiceAccounts(w http.ResponseWriter, r *http.Request, p *Principal) error {
	us, err := s.opts.Store.ListUsers(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	out := []serviceAccountView{}
	for i := range us {
		if us[i].IsServiceAccount() {
			out = append(out, viewServiceAccount(&us[i]))
		}
	}
	writeJSON(w, http.StatusOK, listResponse("service_accounts", out, ""))
	return nil
}

func (s *Server) handleCreateServiceAccount(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req struct {
		Name      string    `json:"name"`
		Role      core.Role `json:"role"`
		CreateKey bool      `json:"create_api_key,omitempty"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validServiceAccountName(req.Name); err != nil {
		return err
	}
	if req.Role == "" {
		req.Role = core.RoleUser
	}
	if !ValidRole(req.Role) {
		return errInvalid("role must be admin, editor or user")
	}
	if dup, err := s.findServiceAccount(ctx, p, req.Name); err != nil {
		return err
	} else if dup != nil {
		return Errf(CodeConflict, "a service account named %s already exists", req.Name)
	}
	u := &core.User{ID: core.NewID(core.ServiceAccountIDPrefix), TeamID: p.TeamID, Name: req.Name,
		Role: core.NormalizeRole(req.Role), Kind: core.UserKindServiceAccount, CreatedAt: s.now()}
	if err := s.opts.Store.CreateUser(ctx, u); err != nil {
		return err
	}
	resp := map[string]any{"service_account": viewServiceAccount(u)}
	if req.CreateKey {
		k, plain, err := NewServiceAccountKey(p.TeamID, u.ID, "default")
		if err != nil {
			return err
		}
		if err := s.opts.Store.CreateAPIKey(ctx, k); err != nil {
			return err
		}
		resp["api_key"] = k
		resp["key"] = plain
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

func (s *Server) handleGetServiceAccount(w http.ResponseWriter, r *http.Request, p *Principal) error {
	u, err := s.serviceAccountFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, viewServiceAccount(u))
	return nil
}

func (s *Server) handleUpdateServiceAccount(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	u, err := s.serviceAccountFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var req updateUserRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if err := validServiceAccountName(n); err != nil {
			return err
		}
		if n != u.Name {
			if dup, err := s.findServiceAccount(ctx, p, n); err != nil {
				return err
			} else if dup != nil {
				return Errf(CodeConflict, "a service account named %s already exists", n)
			}
		}
		u.Name = n
	}
	if req.Role != nil {
		if !ValidRole(*req.Role) {
			return errInvalid("role must be admin, editor or user")
		}
		u.Role = core.NormalizeRole(*req.Role)
	}
	if err := s.applyFactoryRoles(ctx, p, u, req.FactoryRoles); err != nil {
		return err
	}
	if err := s.opts.Store.UpdateUser(ctx, u); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, viewServiceAccount(u))
	return nil
}

// handleDeleteServiceAccount deletes a service account and its API keys.
func (s *Server) handleDeleteServiceAccount(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	u, err := s.serviceAccountFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	d, ok := s.opts.Store.(core.UserDeleter)
	if !ok {
		return Errf(CodeOperationNotSupported, "this store cannot delete service accounts")
	}
	if err := d.DeleteUser(ctx, u.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) serviceAccountKeys(ctx context.Context, p *Principal, accountID string) ([]core.APIKey, error) {
	ks, err := s.opts.Store.ListAPIKeys(ctx, p.TeamID)
	if err != nil {
		return nil, err
	}
	out := []core.APIKey{}
	for _, k := range ks {
		if k.UserID == accountID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *Server) handleListServiceAccountKeys(w http.ResponseWriter, r *http.Request, p *Principal) error {
	u, err := s.serviceAccountFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	ks, err := s.serviceAccountKeys(r.Context(), p, u.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("api_keys", ks, ""))
	return nil
}

func (s *Server) handleCreateServiceAccountKey(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	u, err := s.serviceAccountFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		return errInvalid("name must be 1-100 characters")
	}
	k, plain, err := NewServiceAccountKey(p.TeamID, u.ID, req.Name)
	if err != nil {
		return err
	}
	if err := s.opts.Store.CreateAPIKey(ctx, k); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": k, "key": plain})
	return nil
}

func (s *Server) handleDeleteServiceAccountKey(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	u, err := s.serviceAccountFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	ks, err := s.serviceAccountKeys(ctx, p, u.ID)
	if err != nil {
		return err
	}
	id := r.PathValue("keyId")
	for _, k := range ks {
		if k.ID == id {
			if err := s.opts.Store.DeleteAPIKey(ctx, id); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
	}
	return errNotFound("API key " + id)
}
