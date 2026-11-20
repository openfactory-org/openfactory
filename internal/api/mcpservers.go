package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/mcpregistry"
)

// MCPRegistry is the OAuth side of the MCP server registry
// (*mcpregistry.Service). CRUD goes through the store; without a registry
// the OAuth routes report that OAuth is unavailable.
type MCPRegistry interface {
	StartOAuth(ctx context.Context, m *core.RegisteredMCPServer, userID string) (string, error)
	CompleteOAuth(ctx context.Context, teamID, userID, state, code, providerErr string) (*core.RegisteredMCPServer, error)
	Disconnect(ctx context.Context, m *core.RegisteredMCPServer) error
	Forget(ctx context.Context, serverID string)
}

// mcpServerRequest creates or patches a registry entry. Absent fields are
// unchanged on PATCH; an empty string clears url or command (to switch
// transports); "oauth": null removes OAuth.
type mcpServerRequest struct {
	Name        *string           `json:"name"`
	Description *string           `json:"description"`
	URL         *string           `json:"url"`
	Headers     map[string]string `json:"headers"`
	Command     *string           `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	// Connection is "proxy" (the default for a url server) or "direct".
	Connection *string         `json:"connection"`
	OAuth      json.RawMessage `json:"oauth"`
}

// mcpServerResponse is a registry entry with the warnings of a create or
// an update.
type mcpServerResponse struct {
	*core.RegisteredMCPServer
	Warnings []string `json:"warnings,omitempty"`
}

func mcpServerOut(m *core.RegisteredMCPServer, warnings ...string) mcpServerResponse {
	var ws []string
	for _, w := range warnings {
		if w != "" {
			ws = append(ws, w)
		}
	}
	return mcpServerResponse{RegisteredMCPServer: m, Warnings: ws}
}

type mcpOAuthRequest struct {
	Scopes                []string `json:"scopes"`
	ClientID              string   `json:"client_id"`
	ClientSecretName      string   `json:"client_secret_name"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
}

// builtinMCPServer describes a built-in server in list responses.
type builtinMCPServer struct {
	ManagedID string `json:"managed_id"`
	Provider  string `json:"provider"`
	// Configured reports whether the team has an integration record for the
	// provider. Factories must also attach the integration.
	Configured bool `json:"configured"`
}

func (s *Server) mcpServerFor(ctx context.Context, p *Principal, ref string) (*core.RegisteredMCPServer, error) {
	m, err := s.opts.Store.GetMCPServer(ctx, ref)
	if err == nil && m.TeamID == p.TeamID {
		return m, nil
	}
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	list, err := s.opts.Store.ListMCPServers(ctx, p.TeamID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == ref {
			return &list[i], nil
		}
	}
	return nil, errNotFound("MCP server " + ref)
}

func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request, p *Principal) error {
	list, err := s.opts.Store.ListMCPServers(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	ints, err := s.opts.Store.ListIntegrations(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	builtin := make([]builtinMCPServer, 0, len(core.BuiltinMCPProviders))
	for _, prov := range core.BuiltinMCPProviders {
		b := builtinMCPServer{ManagedID: core.BuiltinMCPPrefix + prov, Provider: prov}
		b.Configured = slices.ContainsFunc(ints, func(in core.Integration) bool { return core.SameIntegrationProvider(in.Provider, prov) })
		builtin = append(builtin, b)
	}
	writeJSON(w, http.StatusOK, listResponse("mcp_servers", list, "").with(map[string]any{"builtin": builtin}))
	return nil
}

func (s *Server) handleCreateMCPServer(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req mcpServerRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.Name == nil {
		return errInvalid("name is required")
	}
	now := s.now().UTC()
	m := &core.RegisteredMCPServer{ID: core.NewID("mcp"), TeamID: p.TeamID, CreatedBy: p.ActorID(), CreatedAt: now, UpdatedAt: now}
	if _, err := applyMCPServerRequest(m, &req); err != nil {
		return err
	}
	warning := mcpregistry.DefaultConnection(m)
	if err := mcpregistry.Validate(m); err != nil {
		return err
	}
	if err := s.checkMCPServerNameFree(r.Context(), p.TeamID, m.Name, ""); err != nil {
		return err
	}
	if err := s.opts.Store.CreateMCPServer(r.Context(), m); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, mcpServerOut(m, warning))
	return nil
}

func (s *Server) handleGetMCPServer(w http.ResponseWriter, r *http.Request, p *Principal) error {
	m, err := s.mcpServerFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mcpServerOut(m))
	return nil
}

func (s *Server) handleUpdateMCPServer(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	m, err := s.mcpServerFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var req mcpServerRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	connected := m.OAuth != nil && m.OAuth.Status == core.MCPOAuthConnected
	wasURL := m.URL != ""
	authChanged, err := applyMCPServerRequest(m, &req)
	if err != nil {
		return err
	}
	if m.URL == "" && req.Connection == nil {
		// A url server that becomes a command server has no connection mode.
		m.Connection = ""
	}
	var warning string
	if !wasURL {
		// A command server that becomes a url server gets the default mode.
		warning = mcpregistry.DefaultConnection(m)
	}
	if err := mcpregistry.Validate(m); err != nil {
		return err
	}
	if err := s.checkMCPServerNameFree(ctx, p.TeamID, m.Name, m.ID); err != nil {
		return err
	}
	if authChanged && connected && m.OAuth != nil {
		// The stored token was issued for the old server or client.
		m.OAuth.Status, m.OAuth.ConnectedBy, m.OAuth.ConnectedAt, m.OAuth.Error = core.MCPOAuthNotConnected, "", nil, ""
	}
	m.UpdatedAt = s.now().UTC()
	if err := s.opts.Store.UpdateMCPServer(ctx, m); err != nil {
		return err
	}
	if authChanged && s.opts.MCPRegistry != nil {
		s.opts.MCPRegistry.Forget(ctx, m.ID)
	}
	writeJSON(w, http.StatusOK, mcpServerOut(m, warning))
	return nil
}

func (s *Server) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	m, err := s.mcpServerFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	uses, err := s.teamResourceUses(ctx, p.TeamID, func(def *core.CompiledDefinition) []string {
		return mcpServerUses(def, m.ID, m.Name)
	})
	if err != nil {
		return err
	}
	if err := inUseError("MCP server", m.Name, uses); err != nil {
		return err
	}
	if err := s.opts.Store.DeleteMCPServer(ctx, m.ID); err != nil {
		return err
	}
	if s.opts.MCPRegistry != nil {
		s.opts.MCPRegistry.Forget(ctx, m.ID)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleConnectMCPServer(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.MCPRegistry == nil {
		return errInvalid("OAuth for MCP servers is not available on this server")
	}
	if p.User == nil {
		return errForbidden("connecting an MCP server needs a signed-in user")
	}
	m, err := s.mcpServerFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if m.OAuth == nil {
		return errInvalid("MCP server %s does not use OAuth", m.Name)
	}
	u, err := s.opts.MCPRegistry.StartOAuth(r.Context(), m, p.User.ID)
	if err != nil {
		if _, mapped := toAPIError(err); mapped {
			return err
		}
		// Discovery, registration or network failures at the provider.
		return Errf(CodeIntegrationNotConfigured, "could not start the OAuth connection: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorization_url": u})
	return nil
}

func (s *Server) handleDisconnectMCPServer(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.MCPRegistry == nil {
		return errInvalid("OAuth for MCP servers is not available on this server")
	}
	m, err := s.mcpServerFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := s.opts.MCPRegistry.Disconnect(r.Context(), m); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mcpServerOut(m))
	return nil
}

// handleMCPOAuthCallback is the provider's redirect target. It finishes the
// flow and sends the browser back to the server's dashboard page.
func (s *Server) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.MCPRegistry == nil {
		return errInvalid("OAuth for MCP servers is not available on this server")
	}
	if p.User == nil {
		return errForbidden("the OAuth callback needs a signed-in user")
	}
	q := r.URL.Query()
	providerErr := q.Get("error")
	if d := q.Get("error_description"); providerErr != "" && d != "" {
		providerErr += ": " + d
	}
	m, err := s.opts.MCPRegistry.CompleteOAuth(r.Context(), p.TeamID, p.User.ID, q.Get("state"), q.Get("code"), providerErr)
	target := "/mcp-servers"
	if m != nil {
		target += "/" + url.PathEscape(m.ID)
	}
	v := url.Values{}
	if err != nil {
		if m == nil && !errors.Is(err, core.ErrInvalid) && !errors.Is(err, core.ErrNotFound) {
			return err
		}
		v.Set("oauth", "error")
		v.Set("message", cleanCoreMessage(err, core.ErrInvalid))
	} else {
		v.Set("oauth", "connected")
	}
	http.Redirect(w, r, s.opts.PublicURL+target+"?"+v.Encode(), http.StatusFound)
	return nil
}

// checkMCPServerNameFree refuses a second MCP server whose name differs from
// an existing one only in case, as for environments. The store's unique
// index compares the exact name; this check also catches "QA-MCP" next to
// "qa-mcp".
func (s *Server) checkMCPServerNameFree(ctx context.Context, teamID, name, exceptID string) error {
	list, err := s.opts.Store.ListMCPServers(ctx, teamID)
	if err != nil {
		return err
	}
	for _, m := range list {
		if m.ID != exceptID && strings.EqualFold(m.Name, name) {
			return Errf(CodeConflict, "an MCP server named %q already exists", m.Name)
		}
	}
	return nil
}

// applyMCPServerRequest copies set fields into m. authChanged reports a
// change that invalidates a stored OAuth token (url, oauth client or endpoints).
func applyMCPServerRequest(m *core.RegisteredMCPServer, req *mcpServerRequest) (authChanged bool, err error) {
	if req.Name != nil {
		m.Name = *req.Name
	}
	if req.Description != nil {
		m.Description = strings.TrimSpace(*req.Description)
	}
	if req.URL != nil {
		authChanged = authChanged || strings.TrimSpace(*req.URL) != m.URL
		m.URL = *req.URL
	}
	if req.Headers != nil {
		m.Headers = req.Headers
	}
	if req.Command != nil {
		m.Command = *req.Command
	}
	if req.Args != nil {
		m.Args = req.Args
	}
	if req.Env != nil {
		m.Env = req.Env
	}
	if len(req.OAuth) > 0 {
		if string(req.OAuth) == "null" {
			authChanged = authChanged || m.OAuth != nil
			m.OAuth = nil
		} else {
			var o mcpOAuthRequest
			dec := json.NewDecoder(strings.NewReader(string(req.OAuth)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&o); err != nil {
				return false, errInvalid("oauth: %v", err)
			}
			next := &core.MCPOAuthConfig{Scopes: o.Scopes, ClientID: o.ClientID, ClientSecretName: o.ClientSecretName,
				AuthorizationEndpoint: o.AuthorizationEndpoint, TokenEndpoint: o.TokenEndpoint, Status: core.MCPOAuthNotConnected}
			if prev := m.OAuth; prev != nil {
				same := prev.ClientID == next.ClientID && prev.ClientSecretName == next.ClientSecretName &&
					prev.AuthorizationEndpoint == next.AuthorizationEndpoint && prev.TokenEndpoint == next.TokenEndpoint &&
					slices.Equal(prev.Scopes, next.Scopes)
				if same {
					next.Status, next.ConnectedBy, next.ConnectedAt, next.Error = prev.Status, prev.ConnectedBy, prev.ConnectedAt, prev.Error
				} else {
					authChanged = true
				}
			}
			m.OAuth = next
		}
	}
	if req.Connection != nil {
		m.Connection = strings.ToLower(strings.TrimSpace(*req.Connection))
	}
	return authChanged, nil
}

// checkManagedMCP reports managedId references of a compiled definition that
// name no registry entry of the team. Built-in ids are checked by the compiler.
func (s *Server) checkManagedMCP(ctx context.Context, teamID string, def *core.CompiledDefinition) error {
	if def == nil {
		return nil
	}
	type ref struct{ where, name, id string }
	var refs []ref
	add := func(where string, m map[string]core.MCPServer) {
		for name, srv := range m {
			if srv.ManagedID != "" && !strings.HasPrefix(srv.ManagedID, core.BuiltinMCPPrefix) {
				refs = append(refs, ref{where, name, srv.ManagedID})
			}
		}
	}
	add("factory.yaml", def.MCPServers)
	for _, a := range def.Agents {
		add("agent "+a.Name, a.MCPServers)
	}
	for _, a := range def.Automations {
		if a.Overrides != nil {
			add("automation "+a.Name, a.Overrides.MCPServers)
		}
	}
	for _, sc := range def.Scorers {
		add("scorer "+sc.Name, sc.MCPServers)
	}
	if def.ScorerDefaults != nil {
		add("scorerDefaults", def.ScorerDefaults.MCPServers)
	}
	if len(refs) == 0 {
		return nil
	}
	list, err := s.opts.Store.ListMCPServers(ctx, teamID)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, m := range list {
		known[m.Name], known[m.ID] = true, true
	}
	var verrs []core.ValidationError
	seen := map[string]bool{}
	for _, r := range refs {
		key := r.name + "/" + r.id // factory-wide servers also appear on every agent
		if known[r.id] || seen[key] {
			continue
		}
		seen[key] = true
		verrs = append(verrs, core.ValidationError{File: r.where, Field: "mcpServers." + r.name + ".managedId",
			Message: fmt.Sprintf("no MCP server %q is registered for this team (register it under MCP servers first)", r.id)})
	}
	if len(verrs) == 0 {
		return nil
	}
	slices.SortFunc(verrs, func(a, b core.ValidationError) int { return strings.Compare(a.File+a.Field, b.File+b.Field) })
	return validationError("definition references unregistered MCP servers", verrs)
}
