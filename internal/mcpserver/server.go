// Package mcpserver implements the Factory MCP server: a streamable-HTTP MCP
// endpoint that lets local coding agents list and create factories, validate
// definition files, send work in, pull work items down to continue
// locally, talk to a work item's foreman, and hand results back.
//
// Authentication uses the same bearer API keys as the REST API, or the
// access token of a browser sign-in (internal/mcpauth).
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/mcpauth"
)

// Paths the server is mounted at.
const (
	Path      = "/mcp"
	AliasPath = "/api/v1/mcp/factory"
)

// Repository is a repository available through a connected code host.
type Repository struct {
	CodeForge     string `json:"codeForge"` // GITHUB | GITLAB
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Private       bool   `json:"private,omitempty"`
	URL           string `json:"url,omitempty"`
}

// TrackerScope is a Linear team or Jira project that can route issues to a factory.
type TrackerScope struct {
	Provider string `json:"provider"` // linear | jira
	ID       string `json:"id"`
	Key      string `json:"key,omitempty"`
	Name     string `json:"name"`
}

// Connection is the state of an integration authorization flow.
type Connection struct {
	ID           string `json:"connection_id"`
	Provider     string `json:"provider"`
	Status       string `json:"status"` // pending | connected | failed
	AuthorizeURL string `json:"authorize_url,omitempty"`
	Integration  string `json:"integration_id,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// Options injects the server's dependencies. Store and Engine are required.
type Options struct {
	Store   core.Store
	Engine  core.Engine
	Compile core.DefinitionCompiler
	Schemas func() map[string]json.RawMessage
	// API is the REST API server whose service methods (factory creation,
	// work-item operations, validation) are reused. When nil, one is built
	// from the fields above.
	API *api.Server

	// Optional integrations with code hosts and trackers.
	ListRepositories  func(ctx context.Context, teamID, codeForge, query string) ([]Repository, error)
	ListTrackerScopes func(ctx context.Context, teamID, provider string) ([]TrackerScope, error)
	StartConnection   func(ctx context.Context, teamID, provider string) (*Connection, error)
	ConnectionStatus  func(ctx context.Context, teamID, connectionID string) (*Connection, error)

	PublicURL string
	Version   string
	Logger    *slog.Logger
	// OAuth is the browser sign-in. With it, /mcp also accepts its
	// access tokens, and a 401 names the protected resource metadata, so that
	// a client can start the sign-in. API keys keep working. Optional.
	OAuth *mcpauth.Service
}

// Server is the Factory MCP server.
type Server struct {
	opts    Options
	api     *api.Server
	log     *slog.Logger
	mcp     *mcp.Server
	handler http.Handler
	alias   http.Handler
}

// New builds the server and registers every tool.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "1.0.0"
	}
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	if opts.API == nil {
		opts.API = api.New(api.Options{Store: opts.Store, Engine: opts.Engine, Compile: opts.Compile,
			Schemas: opts.Schemas, PublicURL: opts.PublicURL, Logger: opts.Logger})
	}
	s := &Server{opts: opts, api: opts.API, log: opts.Logger.With("component", "mcpserver")}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "openfactory", Title: "Open Factory", Version: opts.Version},
		&mcp.ServerOptions{Instructions: instructions, Logger: opts.Logger})
	s.registerTools()
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, &mcp.StreamableHTTPOptions{Logger: opts.Logger})
	protect := func(path string) http.Handler {
		o := &auth.RequireBearerTokenOptions{AllowMissingExpiration: true}
		if opts.OAuth.Enabled() {
			o.ResourceMetadataURL = opts.OAuth.ResourceMetadataURL(path)
		}
		return auth.RequireBearerToken(s.verify, o)(stream)
	}
	s.handler, s.alias = protect(Path), protect(AliasPath)
	return s
}

const instructions = `Factory MCP connects you to the user's Open Factory. A factory is a team of agents bound to repositories;
work sent to it becomes a work item coordinated by the factory's foreman agent.

- send_work_item creates a work item (state the goal, context, constraints and work already done in the note), or hands work back to an
  existing work item with its ID, the pushed branch or pull request URL and a note covering what changed, what was validated and what remains.
- To continue a work item locally: find it (list_work_items, search_work_items, or get_work_item with any reference), call get_work_item with start_working=true,
  follow the returned Git setup, keep the foreman informed with message_foreman / get_conversation, push, then hand back with send_work_item.
- Picking up a work item does not claim, lock or pause it. complete_work_item closes a work item when nothing remains.
- Factory MCP never modifies local files; run the suggested commands yourself.`

// Register mounts the server at /mcp and /api/v1/mcp/factory.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle(Path, s.handler)
	mux.Handle(AliasPath, s.alias)
}

// Handler returns the authenticated streamable HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// MCP returns the underlying MCP server.
func (s *Server) MCP() *mcp.Server { return s.mcp }

func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if strings.HasPrefix(token, mcpauth.AccessTokenPrefix) && s.opts.OAuth != nil {
		g, u, err := s.opts.OAuth.Verify(ctx, token)
		if err != nil {
			if errors.Is(err, mcpauth.ErrInvalidToken) {
				return nil, fmt.Errorf("%w: invalid or expired sign-in token", auth.ErrInvalidToken)
			}
			return nil, err
		}
		role := core.NormalizeRole(u.Role)
		if !api.ValidRole(role) {
			role = core.RoleUser
		}
		p := &api.Principal{User: u, TeamID: g.TeamID, Role: role}
		return &auth.TokenInfo{UserID: u.ID, Extra: map[string]any{principalKey: p, "mcp_sign_in": g.ID}}, nil
	}
	p, err := api.Authenticate(ctx, s.opts.Store, token)
	if err != nil {
		if errors.Is(err, core.ErrUnauthorized) {
			return nil, fmt.Errorf("%w: invalid API key", auth.ErrInvalidToken)
		}
		return nil, err
	}
	return &auth.TokenInfo{UserID: p.User.ID, Extra: map[string]any{principalKey: p}}, nil
}

const principalKey = "principal"

func principalOf(req *mcp.CallToolRequest) (*api.Principal, error) {
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
		if p, ok := req.Extra.TokenInfo.Extra[principalKey].(*api.Principal); ok {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: authentication required", core.ErrUnauthorized)
}

// ---------- results ----------

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

// errorResult reports a tool-level failure the model can act on.
func errorResult(err error) (*mcp.CallToolResult, any, error) {
	code, msg := classify(err)
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": msg}})
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func classify(err error) (string, string) {
	var ae *api.Error
	if errors.As(err, &ae) {
		if ae.Detail != "" {
			return ae.Code, ae.Detail
		}
		return ae.Code, ae.Code
	}
	switch {
	case errors.Is(err, core.ErrNotFound):
		return api.CodeResourceNotFound, err.Error()
	case errors.Is(err, core.ErrInvalid):
		return api.CodeInvalidRequest, err.Error()
	case errors.Is(err, core.ErrConflict):
		return api.CodeConflict, err.Error()
	case errors.Is(err, core.ErrUnauthorized):
		return api.CodeAuthenticationRequired, err.Error()
	case errors.Is(err, core.ErrForbidden):
		return api.CodeNotAuthorized, err.Error()
	}
	return api.CodeInternalError, err.Error()
}

func invalid(format string, args ...any) error {
	return api.Errf(api.CodeInvalidRequest, format, args...)
}

// tool wraps a typed handler with authentication and error conversion.
func tool[In any](s *Server, name, desc string, readOnly bool, h func(ctx context.Context, p *api.Principal, in In) (any, error)) {
	ann := &mcp.ToolAnnotations{ReadOnlyHint: readOnly}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: name, Description: desc, Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			p, err := principalOf(req)
			if err != nil {
				return errorResult(err)
			}
			out, err := h(ctx, p, in)
			if err != nil {
				if _, known := classifyKnown(err); !known {
					s.log.ErrorContext(ctx, "mcp tool failed", "tool", name, "err", err)
				}
				return errorResult(err)
			}
			return jsonResult(out)
		})
}

func classifyKnown(err error) (string, bool) {
	code, _ := classify(err)
	return code, code != api.CodeInternalError
}
