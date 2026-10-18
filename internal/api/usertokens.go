package api

import (
	"context"
	"net/http"
	"strings"
)

// UserForgeTokenStore stores personal code-forge tokens (forge.UserTokens).
type UserForgeTokenStore interface {
	Set(ctx context.Context, teamID, userID, codeForge, token string) error
	Delete(ctx context.Context, teamID, userID, codeForge string) error
	List(ctx context.Context, teamID, userID string) (forges, unreadable []string, err error)
}

func (s *Server) userForgeTokens(p *Principal) (UserForgeTokenStore, string, error) {
	if s.opts.UserForgeTokens == nil {
		return nil, "", featureUnavailable("personal forge tokens")
	}
	if p == nil || p.User == nil {
		return nil, "", errForbidden("personal forge tokens require an API key that belongs to a user")
	}
	return s.opts.UserForgeTokens, p.User.ID, nil
}

// GET /me/forge-tokens lists the forges for which the caller stored a token,
// and the stored tokens that cannot be decrypted.
func (s *Server) handleListUserForgeTokens(w http.ResponseWriter, r *http.Request, p *Principal) error {
	st, uid, err := s.userForgeTokens(p)
	if err != nil {
		return err
	}
	forges, unreadable, err := st.List(r.Context(), p.TeamID, uid)
	if err != nil {
		return err
	}
	if forges == nil {
		forges = []string{}
	}
	if unreadable == nil {
		unreadable = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"forges": forges, "unreadable": unreadable})
	return nil
}

// PUT /me/forge-tokens/{forge} {"token": "..."} stores the caller's token.
func (s *Server) handleSetUserForgeToken(w http.ResponseWriter, r *http.Request, p *Principal) error {
	st, uid, err := s.userForgeTokens(p)
	if err != nil {
		return err
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if strings.TrimSpace(req.Token) == "" {
		return errInvalid("token is required")
	}
	if err := st.Set(r.Context(), p.TeamID, uid, strings.ToUpper(r.PathValue("forge")), req.Token); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// DELETE /me/forge-tokens/{forge} removes the caller's token.
func (s *Server) handleDeleteUserForgeToken(w http.ResponseWriter, r *http.Request, p *Principal) error {
	st, uid, err := s.userForgeTokens(p)
	if err != nil {
		return err
	}
	if err := st.Delete(r.Context(), p.TeamID, uid, strings.ToUpper(r.PathValue("forge"))); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
