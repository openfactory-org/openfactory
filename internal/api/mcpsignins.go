package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/mcpauth"
)

// MCPSignIns lists and revokes the browser sign-ins of the Factory MCP
// server. *mcpauth.Service implements it.
type MCPSignIns interface {
	ListGrants(ctx context.Context, userID string) ([]mcpauth.Grant, error)
	ListTeamGrants(ctx context.Context, teamID string) ([]mcpauth.Grant, error)
	GetGrant(ctx context.Context, id string) (*mcpauth.Grant, error)
	Revoke(ctx context.Context, id string) error
}

// handleListMCPSignIns lists the active MCP sign-ins of the caller. With
// scope=team, an admin gets every sign-in of the team.
func (s *Server) handleListMCPSignIns(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.MCPSignIns == nil {
		return featureUnavailable("MCP sign-in")
	}
	var (
		list []mcpauth.Grant
		err  error
	)
	if r.URL.Query().Get("scope") == "team" {
		if !p.Can(core.RoleAdmin) {
			return errForbidden("only admins can list the MCP sign-ins of the team")
		}
		list, err = s.opts.MCPSignIns.ListTeamGrants(r.Context(), p.TeamID)
	} else {
		list, err = s.opts.MCPSignIns.ListGrants(r.Context(), p.UserID())
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("mcp_sign_ins", list, ""))
	return nil
}

// handleRevokeMCPSignIn ends one MCP sign-in: its tokens stop working at
// once. A user revokes their own; an admin revokes any of the team.
func (s *Server) handleRevokeMCPSignIn(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.MCPSignIns == nil {
		return featureUnavailable("MCP sign-in")
	}
	id := r.PathValue("id")
	g, err := s.opts.MCPSignIns.GetGrant(r.Context(), id)
	if errors.Is(err, core.ErrNotFound) || (err == nil && g.TeamID != p.TeamID) {
		return errNotFound("MCP sign-in " + id)
	}
	if err != nil {
		return err
	}
	if g.UserID != p.UserID() && !p.Can(core.RoleAdmin) {
		return errForbidden("only admins can end the MCP sign-ins of other users")
	}
	if err := s.opts.MCPSignIns.Revoke(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
