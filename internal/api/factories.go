package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/openfactory-org/openfactory/internal/core"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// ---------- list / get ----------

func (s *Server) handleListFactories(w http.ResponseWriter, r *http.Request, p *Principal) error {
	pg, err := pageFrom(r)
	if err != nil {
		return err
	}
	fs, err := s.opts.Store.ListFactories(r.Context(), p.TeamID, r.URL.Query().Get("search"))
	if err != nil {
		return err
	}
	if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search"))); q != "" {
		// Enforce case-insensitive name/alias matching regardless of store behavior.
		fs = slices.DeleteFunc(fs, func(f core.Factory) bool {
			return !strings.Contains(strings.ToLower(f.Name), q) && !strings.Contains(strings.ToLower(f.Alias), q)
		})
	}
	page, next := paginate(fs, func(f core.Factory) (timeT, string) { return f.CreatedAt, f.ID }, pg)
	views := make([]FactoryView, 0, len(page))
	for i := range page {
		views = append(views, s.factoryView(&page[i]))
	}
	writeJSON(w, http.StatusOK, listResponse("factories", views, next))
	return nil
}

func (s *Server) handleGetFactory(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.factoryView(f))
	return nil
}

// ---------- create ----------

// CreateFactoryRequest is the body of POST /factories.
type CreateFactoryRequest struct {
	Name         string                 `json:"name"`
	Alias        string                 `json:"alias,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Source       core.DefinitionSource  `json:"source,omitempty"` // managed (default) | git
	GitRepo      string                 `json:"git_repo,omitempty"`
	GitBranch    string                 `json:"git_branch,omitempty"`
	GitPath      string                 `json:"git_path,omitempty"`
	Repositories []core.RepoRef         `json:"repositories,omitempty"`
	Integrations []core.IntegrationDecl `json:"integrations,omitempty"`
	// Agents selects default agents besides the foreman; nil selects all of them.
	Agents  *[]string           `json:"agents,omitempty"`
	Model   string              `json:"model,omitempty"`
	Harness *core.HarnessConfig `json:"harness,omitempty"`
	// Definition is either the string "defaults" or {"files": {...}}.
	Definition json.RawMessage `json:"definition,omitempty"`
	// Files is a full definition tree (path -> content); takes precedence over Definition.
	Files map[string]string `json:"files,omitempty"`
}

// FactoryCreated is the response of a factory creation.
type FactoryCreated struct {
	Factory FactoryView             `json:"factory"`
	Version *core.DefinitionVersion `json:"version,omitempty"`
}

var integrationTypes = []string{"slack", "microsoft-teams", "linear", "jira"}
var codeForges = []string{"GITHUB", "GITLAB", "AZURE_DEVOPS"}

func (s *Server) handleCreateFactory(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req CreateFactoryRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	res, err := s.CreateFactory(r.Context(), p, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, res)
	return nil
}

// CreateFactory creates a factory (and applies its initial definition) on
// behalf of p. It is shared by the REST API and the Factory MCP server.
func (s *Server) CreateFactory(ctx context.Context, p *Principal, req CreateFactoryRequest) (*FactoryCreated, error) {
	if !p.Can(core.RoleEditor) {
		return nil, errForbidden("creating a factory requires the team-wide editor role")
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Alias = strings.TrimSpace(req.Alias)
	if req.Source == "" {
		req.Source = core.DefinitionManaged
	}
	if req.Source != core.DefinitionManaged && req.Source != core.DefinitionGit {
		return nil, errInvalid("source must be %q or %q", core.DefinitionManaged, core.DefinitionGit)
	}
	if req.Source == core.DefinitionGit && strings.TrimSpace(req.GitRepo) == "" {
		return nil, errInvalid("git_repo is required for a git-backed definition")
	}
	files, err := s.resolveCreateFiles(&req)
	if err != nil {
		return nil, err
	}
	if len(files) > 0 && s.opts.Compile != nil {
		def, verrs := s.opts.Compile(files)
		if len(verrs) > 0 {
			return nil, validationError("definition is invalid", verrs)
		}
		if err := s.checkManagedMCP(ctx, p.TeamID, def); err != nil {
			return nil, err
		}
		if def != nil {
			if req.Name == "" {
				req.Name = def.Name
			}
			if req.Alias == "" {
				req.Alias = def.Alias
			}
			if req.Description == "" {
				req.Description = def.Description
			}
		}
	}
	if req.Name == "" {
		return nil, errInvalid("name is required")
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		return nil, errInvalid("name must be at most 100 characters")
	}
	if req.Alias == "" {
		req.Alias = AliasFromName(req.Name)
	}
	if !ValidAlias(req.Alias) {
		return nil, errInvalid("alias accepts up to 60 letters, numbers, spaces, '.', '_' and '-'")
	}
	if err := s.checkAliasFree(ctx, p.TeamID, req.Alias, ""); err != nil {
		return nil, err
	}
	now := s.now()
	f := &core.Factory{
		ID:          core.NewID("fac"),
		TeamID:      p.TeamID,
		Name:        req.Name,
		Alias:       req.Alias,
		Description: req.Description,
		Source:      req.Source,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if req.Source == core.DefinitionGit {
		f.GitRepo = strings.TrimSpace(req.GitRepo)
		f.GitBranch = strings.TrimSpace(req.GitBranch)
		if f.GitBranch == "" {
			f.GitBranch = "main"
		}
		f.GitPath = strings.Trim(strings.TrimSpace(req.GitPath), "/")
	}
	if err := s.opts.Store.CreateFactory(ctx, f); err != nil {
		return nil, err
	}
	// The branch of a git source must compile and pass the preflight, as for
	// a change of the source. The check needs the factory record (its team),
	// so a refusal removes the record again.
	if f.Source == core.DefinitionGit && s.opts.CheckGitSource != nil {
		if err := s.checkGitBranch(ctx, f, "the factory was not created"); err != nil {
			_ = s.opts.Store.DeleteFactory(ctx, f.ID)
			return nil, err
		}
	}
	var version *core.DefinitionVersion
	if len(files) > 0 {
		v, err := s.opts.Engine.ApplyDefinition(ctx, f.ID, files, p.ActorID(), "Create factory", "")
		if err != nil || v == nil || !v.Valid {
			_ = s.opts.Store.DeleteFactory(ctx, f.ID)
			if err != nil {
				return nil, applyError("definition could not be applied", v, err)
			}
			var verrs []core.ValidationError
			if v != nil {
				verrs = v.Errors
			}
			return nil, validationError("definition could not be applied", verrs)
		}
		version = v
		if nf, err := s.opts.Store.GetFactory(ctx, f.ID); err == nil {
			f = nf
		}
	}
	s.log.InfoContext(ctx, "factory created", "factory", f.ID, "team", f.TeamID, "actor", p.ActorID())
	return &FactoryCreated{Factory: s.factoryView(f), Version: version}, nil
}

// resolveCreateFiles picks the initial definition tree for a create request.
func (s *Server) resolveCreateFiles(req *CreateFactoryRequest) (map[string]string, error) {
	if len(req.Files) > 0 {
		return req.Files, nil
	}
	useDefaults := req.Source == core.DefinitionManaged
	if d := bytes.TrimSpace(req.Definition); len(d) > 0 && string(d) != "null" {
		var mode string
		if err := json.Unmarshal(d, &mode); err == nil {
			if mode != "defaults" {
				return nil, errInvalid(`definition must be "defaults" or an object with "files"`)
			}
			useDefaults = true
		} else {
			var obj struct {
				Files map[string]string `json:"files"`
			}
			if err := json.Unmarshal(d, &obj); err != nil || len(obj.Files) == 0 {
				return nil, errInvalid(`definition must be "defaults" or an object with a non-empty "files" map`)
			}
			return obj.Files, nil
		}
	}
	if !useDefaults {
		return nil, nil
	}
	agents := DefaultAgentNames
	if req.Agents != nil {
		agents = nil
		for _, a := range *req.Agents {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "foreman" || a == "main" {
				continue
			}
			if !slices.Contains(DefaultAgentNames, a) {
				return nil, errInvalid("unknown default agent %q (choose from %s)", a, strings.Join(DefaultAgentNames, ", "))
			}
			if !slices.Contains(agents, a) {
				agents = append(agents, a)
			}
		}
	}
	trackers := 0
	for i, in := range req.Integrations {
		t := strings.ToLower(strings.TrimSpace(in.Type))
		if t == "teams" || t == "microsoft_teams" {
			t = "microsoft-teams"
		}
		if !slices.Contains(integrationTypes, t) {
			return nil, errInvalid("unknown integration type %q (choose from %s)", in.Type, strings.Join(integrationTypes, ", "))
		}
		if t == "linear" || t == "jira" {
			trackers++
		}
		req.Integrations[i].Type = t
	}
	if trackers > 1 {
		return nil, errInvalid("linear and jira are mutually exclusive: declare at most one issue tracker")
	}
	for i, repo := range req.Repositories {
		if strings.TrimSpace(repo.Owner) == "" || strings.TrimSpace(repo.Name) == "" {
			return nil, errInvalid("repositories[%d]: owner and name are required", i)
		}
		if repo.CodeForge != "" && !slices.Contains(codeForges, strings.ToUpper(repo.CodeForge)) {
			return nil, errInvalid("repositories[%d]: codeForge must be one of %s", i, strings.Join(codeForges, ", "))
		}
	}
	name := req.Name
	if name == "" {
		return nil, errInvalid("name is required")
	}
	alias := req.Alias
	if alias == "" {
		alias = AliasFromName(name)
	}
	dreq := DefaultDefinitionRequest{
		Name: name, Alias: alias, Description: req.Description,
		Repositories: req.Repositories, Integrations: req.Integrations,
		Agents: agents, Model: req.Model, Harness: req.Harness,
	}
	if s.opts.DefaultDefinition != nil {
		return s.opts.DefaultDefinition(dreq), nil
	}
	return GenerateDefaultDefinition(dreq), nil
}

func (s *Server) checkAliasFree(ctx context.Context, teamID, alias, exceptID string) error {
	fs, err := s.opts.Store.ListFactories(ctx, teamID, "")
	if err != nil {
		return err
	}
	for _, f := range fs {
		if f.ID != exceptID && strings.EqualFold(f.Alias, alias) {
			return Errf(CodeConflict, "alias %q is already used by factory %s", alias, f.ID)
		}
	}
	return nil
}

// ---------- update / delete ----------

type updateFactoryRequest struct {
	Name        *string `json:"name"`
	Alias       *string `json:"alias"`
	Description *string `json:"description"`
	GitRepo     *string `json:"git_repo"`
	GitBranch   *string `json:"git_branch"`
	GitPath     *string `json:"git_path"`
	// Source switches where the definition lives: managed or git.
	Source *core.DefinitionSource `json:"source"`
	// CredentialStrategy sets factory.yaml credentialStrategy (EXECUTOR or
	// CREATOR; empty removes it). Managed definitions only.
	CredentialStrategy *string `json:"credential_strategy"`
}

func (s *Server) handleUpdateFactory(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	var req updateFactoryRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	wasSource := f.Source
	yamlEdits := map[string]any{}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || utf8.RuneCountInString(n) > 100 {
			return errInvalid("name must be 1-100 characters")
		}
		f.Name = n
		yamlEdits["name"] = n
	}
	if req.Alias != nil {
		a := strings.TrimSpace(*req.Alias)
		if !ValidAlias(a) {
			return errInvalid("alias accepts up to 60 letters, numbers, spaces, '.', '_' and '-'")
		}
		if err := s.checkAliasFree(ctx, p.TeamID, a, f.ID); err != nil {
			return err
		}
		f.Alias = a
		yamlEdits["alias"] = a
	}
	if req.Description != nil {
		f.Description = strings.TrimSpace(*req.Description)
		if f.Description == "" {
			yamlEdits["description"] = nil
		} else {
			yamlEdits["description"] = f.Description
		}
	}
	if req.CredentialStrategy != nil {
		cs := strings.ToUpper(strings.TrimSpace(*req.CredentialStrategy))
		switch cs {
		case "":
			yamlEdits["credentialStrategy"] = nil
		case "EXECUTOR", "CREATOR":
			yamlEdits["credentialStrategy"] = cs
		default:
			return errInvalid("credential_strategy must be EXECUTOR or CREATOR")
		}
		if f.Source != core.DefinitionManaged {
			return Errf(CodeOperationNotSupported, "credentialStrategy of a git-backed factory is set in its factory.yaml")
		}
	}
	if req.Source != nil {
		switch *req.Source {
		case core.DefinitionManaged:
			f.Source = core.DefinitionManaged
		case core.DefinitionGit:
			repo := f.GitRepo
			if req.GitRepo != nil {
				repo = strings.TrimSpace(*req.GitRepo)
			}
			if repo == "" {
				return errInvalid("git_repo is required to switch the definition source to git")
			}
			f.Source = core.DefinitionGit
			if f.GitBranch == "" {
				f.GitBranch = "main"
			}
		default:
			return errInvalid("source must be managed or git")
		}
	}
	if req.GitPath != nil {
		gp := strings.Trim(strings.TrimSpace(*req.GitPath), "/")
		if strings.Contains(gp, "..") || strings.HasPrefix(gp, "/") {
			return errInvalid("git_path must be a relative directory inside the repository")
		}
	}
	if req.GitRepo != nil || req.GitBranch != nil || req.GitPath != nil {
		if f.Source != core.DefinitionGit {
			return Errf(CodeOperationNotSupported, "git settings apply only to git-backed factories")
		}
		if req.GitRepo != nil {
			if strings.TrimSpace(*req.GitRepo) == "" {
				return errInvalid("git_repo cannot be empty")
			}
			f.GitRepo = strings.TrimSpace(*req.GitRepo)
		}
		if req.GitBranch != nil {
			f.GitBranch = strings.TrimSpace(*req.GitBranch)
			if f.GitBranch == "" {
				f.GitBranch = "main"
			}
		}
		if req.GitPath != nil {
			f.GitPath = strings.Trim(strings.TrimSpace(*req.GitPath), "/")
		}
	}
	// Before the source becomes git (or its location changes), the branch
	// must compile and pass the preflight.
	if f.Source == core.DefinitionGit && s.opts.CheckGitSource != nil &&
		(wasSource != core.DefinitionGit || req.GitRepo != nil || req.GitBranch != nil || req.GitPath != nil) {
		if err := s.checkGitBranch(ctx, f, "the source did not change"); err != nil {
			return err
		}
	}
	// Managed definitions carry name/alias/description in factory.yaml: rewrite and re-apply.
	if f.Source == core.DefinitionManaged && f.ActiveVersionID != "" && len(yamlEdits) > 0 {
		v, err := s.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
		if err != nil {
			return err
		}
		files := cloneFiles(v.Files)
		doc, err := editYAMLDocument(files["factory.yaml"], yamlEdits)
		if err != nil {
			return err
		}
		files["factory.yaml"] = doc
		nv, err := s.opts.Engine.ApplyDefinition(ctx, f.ID, files, p.ActorID(), "Update factory settings", "")
		if err != nil {
			return applyError("updated definition is invalid", nv, err)
		}
		if nv == nil || !nv.Valid {
			return validationError("updated definition is invalid", versionErrors(nv))
		}
		if nf, err := s.opts.Store.GetFactory(ctx, f.ID); err == nil {
			nf.Name, nf.Alias, nf.Description = f.Name, f.Alias, f.Description
			f = nf
		}
	}
	f.UpdatedAt = s.now()
	if err := s.opts.Store.UpdateFactory(ctx, f); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.factoryView(f))
	return nil
}

// checkGitBranch makes sure that the git branch of f compiles and passes the
// preflight. outcome says what the refusal left as it was.
func (s *Server) checkGitBranch(ctx context.Context, f *core.Factory, outcome string) error {
	err := s.opts.CheckGitSource(ctx, f)
	if err == nil {
		return nil
	}
	var ve interface{ ValidationErrors() []core.ValidationError }
	if errors.As(err, &ve) {
		return validationError(fmt.Sprintf("the definition on branch %q of %s is invalid; %s", branchName(f), f.GitRepo, outcome), ve.ValidationErrors())
	}
	return errInvalid("the definition on branch %q of %s could not be checked; %s: %v", branchName(f), f.GitRepo, outcome, err)
}

func (s *Server) handleDeleteFactory(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if err := s.opts.Store.DeleteFactory(r.Context(), f.ID); err != nil {
		return err
	}
	s.log.InfoContext(r.Context(), "factory deleted", "factory", f.ID, "actor", p.ActorID())
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------- dispatch ----------

// DispatchRequest is the body of POST /factories/{uid}/runs.
type DispatchRequest struct {
	Prompt    string               `json:"prompt"`
	Title     string               `json:"title,omitempty"`
	TicketRef string               `json:"ticket_ref,omitempty"` // <source>:<id>, e.g. linear:PAY-123
	TicketURL string               `json:"ticket_url,omitempty"`
	Source    *core.SourceRef      `json:"source,omitempty"`
	Agent     string               `json:"agent,omitempty"` // default foreman
	Metadata  map[string]string    `json:"metadata,omitempty"`
	Overrides *core.AgentOverrides `json:"overrides,omitempty"`
}

// DispatchResponse is returned by a dispatch.
type DispatchResponse struct {
	RunView
	WorkItemURL string `json:"work_item_url"`
}

var ticketRefRe = regexp.MustCompile(`^([a-z][a-z0-9_-]*):(\S+)$`)

func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	var req DispatchRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return errInvalid("prompt is required")
	}
	if err := ValidateMetadata(req.Metadata); err != nil {
		return err
	}
	src := core.SourceRef{Provider: "api", Kind: "task"}
	if req.Source != nil {
		src = *req.Source
		if src.Provider == "" {
			src.Provider = "api"
		}
	}
	if req.TicketRef != "" {
		m := ticketRefRe.FindStringSubmatch(strings.TrimSpace(req.TicketRef))
		if m == nil {
			return errInvalid("ticket_ref must have the form <source>:<id>, e.g. linear:PAY-123")
		}
		src.Provider, src.ExternalID = m[1], m[2]
		if src.Kind == "" || src.Kind == "task" {
			src.Kind = "issue"
		}
	}
	if req.TicketURL != "" {
		src.URL = req.TicketURL
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = deriveTitle(req.Prompt)
	}
	if src.Title == "" {
		src.Title = title
	}
	wi, run, err := s.opts.Engine.Dispatch(ctx, core.DispatchRequest{
		FactoryID:   f.ID,
		Prompt:      req.Prompt,
		Source:      src,
		AgentName:   req.Agent,
		CreatorID:   p.UserID(),
		Title:       title,
		Overrides:   req.Overrides,
		Metadata:    req.Metadata,
		TriggeredBy: requestSource(withRequestSource(ctx, r, p)),
	})
	if err != nil {
		return err
	}
	if run == nil {
		return Errf(CodeInternalError, "engine returned no run")
	}
	resp := DispatchResponse{RunView: s.runView(run)}
	if wi != nil {
		resp.WorkItemURL = s.WorkItemURL(f.ID, wi.ID)
		if resp.WorkItemID == "" {
			resp.WorkItemID = wi.ID
		}
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

var metadataKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidateMetadata enforces run metadata limits: at most 20 keys, keys of
// 1-64 bytes from [A-Za-z0-9._-], values of at most 256 bytes of UTF-8 without
// NUL, and no two keys mapping to the same OF_METADATA_* variable.
func ValidateMetadata(m map[string]string) error {
	if len(m) > 20 {
		return errInvalid("metadata accepts at most 20 keys, got %d", len(m))
	}
	seen := map[string]string{}
	for k, v := range m {
		if !metadataKeyRe.MatchString(k) {
			return errInvalid("metadata key %q must be 1-64 characters from [A-Za-z0-9._-]", k)
		}
		if len(v) > 256 {
			return errInvalid("metadata value for %q exceeds 256 bytes", k)
		}
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return errInvalid("metadata value for %q must be UTF-8 without NUL bytes", k)
		}
		env := MetadataEnvName(k)
		if other, ok := seen[env]; ok {
			return errInvalid("metadata keys %q and %q collide as %s", other, k, env)
		}
		seen[env] = k
	}
	return nil
}

// MetadataEnvName returns the environment variable a metadata key maps to.
func MetadataEnvName(key string) string {
	r := strings.NewReplacer(".", "_", "-", "_")
	return "OF_METADATA_" + strings.ToUpper(r.Replace(key))
}

// ---------- definitions ----------

func (s *Server) handleGetDefinition(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	resp := map[string]any{"factory_id": f.ID, "source": f.Source, "files": map[string]string{}, "version": nil}
	if f.ActiveVersionID != "" {
		v, err := s.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
		if err != nil {
			return err
		}
		resp["files"] = v.Files
		resp["version"] = v
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

type putDefinitionRequest struct {
	Files         map[string]string `json:"files"`
	Message       string            `json:"message,omitempty"`
	BaseVersionID string            `json:"base_version_id,omitempty"`
}

func (s *Server) handlePutDefinition(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if f.Source == core.DefinitionGit {
		return Errf(CodeOperationNotSupported, "the definition of this factory lives in %s; merge a change to branch %q instead", f.GitRepo, f.GitBranch)
	}
	var req putDefinitionRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if len(req.Files) == 0 {
		return errInvalid("files is required")
	}
	if req.BaseVersionID != "" && req.BaseVersionID != f.ActiveVersionID {
		return Errf(CodeConflict, "the definition changed since version %s (active version is %s)", req.BaseVersionID, f.ActiveVersionID)
	}
	if s.opts.Compile != nil {
		def, verrs := s.opts.Compile(req.Files)
		if len(verrs) > 0 {
			return validationError("definition is invalid", verrs)
		}
		if err := s.checkManagedMCP(ctx, p.TeamID, def); err != nil {
			return err
		}
	}
	msg := req.Message
	if msg == "" {
		msg = "Update definition"
	}
	v, err := s.opts.Engine.ApplyDefinition(ctx, f.ID, req.Files, p.ActorID(), msg, "")
	if err != nil {
		return applyError("definition could not be applied; the previous version stays active", v, err)
	}
	if v == nil || !v.Valid {
		return validationError("definition could not be applied; the previous version stays active", versionErrors(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v})
	return nil
}

func branchName(f *core.Factory) string {
	if f.GitBranch == "" {
		return "main"
	}
	return f.GitBranch
}

func versionErrors(v *core.DefinitionVersion) []core.ValidationError {
	if v == nil {
		return nil
	}
	return v.Errors
}

// ValidationResult is the response of a definition validation.
type ValidationResult struct {
	Valid  bool                   `json:"valid"`
	Errors []core.ValidationError `json:"errors"`
	// Warnings and Skipped come from the preflight, which checks the files
	// against the team. They are empty when only the compiler ran.
	Warnings []core.ValidationError `json:"warnings"`
	Skipped  []core.SkippedCheck    `json:"skipped"`
	Summary  *DefinitionSummary     `json:"summary,omitempty"`
}

// definitionChecker is implemented by engines that run the compiler and the
// preflight (engine.Engine.CheckDefinition).
type definitionChecker interface {
	CheckDefinition(ctx context.Context, factoryID string, files map[string]string) (*core.DefinitionCheck, error)
}

// ValidateFactoryFiles runs the compiler and the preflight on a definition
// tree for a factory, without applying it. Without a preflight-capable
// engine only the compiler runs.
func (s *Server) ValidateFactoryFiles(ctx context.Context, factoryID string, files map[string]string) (*ValidationResult, error) {
	chk, ok := s.opts.Engine.(definitionChecker)
	if !ok || chk == nil {
		return s.ValidateFiles(files)
	}
	if len(files) == 0 {
		return nil, errInvalid("files is required")
	}
	c, err := chk.CheckDefinition(ctx, factoryID, files)
	if err != nil {
		return nil, err
	}
	res := &ValidationResult{Valid: c.Valid(), Errors: c.Errors, Warnings: c.Warnings, Skipped: c.Skipped}
	if res.Errors == nil {
		res.Errors = []core.ValidationError{}
	}
	if res.Warnings == nil {
		res.Warnings = []core.ValidationError{}
	}
	if res.Skipped == nil {
		res.Skipped = []core.SkippedCheck{}
	}
	if res.Valid {
		res.Summary = summarize(c.Definition)
	}
	return res, nil
}

// DefinitionSummary lists the resources of a compiled definition.
type DefinitionSummary struct {
	Name        string   `json:"name"`
	Alias       string   `json:"alias,omitempty"`
	Agents      []string `json:"agents"`
	Automations []string `json:"automations"`
	Machines    []string `json:"machines"`
	Scorers     []string `json:"scorers"`
	Webhooks    []string `json:"webhooks"`
	Benchmarks  []string `json:"benchmarks"`
	Skills      []string `json:"skills"`
}

// ValidateFiles compiles a definition tree without applying it.
func (s *Server) ValidateFiles(files map[string]string) (*ValidationResult, error) {
	if s.opts.Compile == nil {
		return nil, featureUnavailable("definition validation")
	}
	if len(files) == 0 {
		return nil, errInvalid("files is required")
	}
	def, verrs := s.opts.Compile(files)
	res := &ValidationResult{Valid: len(verrs) == 0, Errors: verrs, Warnings: []core.ValidationError{}, Skipped: []core.SkippedCheck{}}
	if res.Errors == nil {
		res.Errors = []core.ValidationError{}
	}
	if def != nil && res.Valid {
		res.Summary = summarize(def)
	}
	return res, nil
}

func summarize(def *core.CompiledDefinition) *DefinitionSummary {
	sm := &DefinitionSummary{Name: def.Name, Alias: def.Alias,
		Agents: []string{}, Automations: []string{}, Machines: []string{},
		Scorers: []string{}, Webhooks: []string{}, Benchmarks: []string{}, Skills: []string{}}
	for _, a := range def.Agents {
		sm.Agents = append(sm.Agents, a.Name)
	}
	for _, a := range def.Automations {
		sm.Automations = append(sm.Automations, a.Name)
	}
	for n := range def.Machines {
		sm.Machines = append(sm.Machines, n)
	}
	for _, sc := range def.Scorers {
		sm.Scorers = append(sm.Scorers, sc.Slug)
	}
	for _, wh := range def.Webhooks {
		sm.Webhooks = append(sm.Webhooks, wh.Name)
	}
	for _, b := range def.Benchmarks {
		sm.Benchmarks = append(sm.Benchmarks, b.Slug)
	}
	for _, sk := range def.Skills {
		sm.Skills = append(sm.Skills, sk.Name)
	}
	slices.Sort(sm.Machines)
	return sm
}

func (s *Server) handleValidateDefinition(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	var req struct {
		Files map[string]string `json:"files"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	res, err := s.ValidateFactoryFiles(r.Context(), f.ID, req.Files)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) handleListDefinitionVersions(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	pg, err := pageFrom(r)
	if err != nil {
		return err
	}
	withFiles, err := qBool(r, "include_files")
	if err != nil {
		return err
	}
	vs, err := s.opts.Store.ListDefinitionVersions(ctx, f.ID, pg.listOpts())
	if err != nil {
		return err
	}
	vs, next := trimPage(vs, func(v core.DefinitionVersion) (timeT, string) { return v.CreatedAt, v.ID }, pg.Limit)
	for i := range vs {
		vs[i].State = vs[i].EffectiveState(f.ActiveVersionID)
	}
	if !withFiles {
		for i := range vs {
			vs[i].Files = nil
		}
	}
	writeJSON(w, http.StatusOK, listResponse("versions", vs, next).with(map[string]any{"active_version_id": f.ActiveVersionID}))
	return nil
}

func (s *Server) handleGetDefinitionVersion(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	v, err := s.opts.Store.GetDefinitionVersion(ctx, r.PathValue("versionId"))
	if err != nil || v.FactoryID != f.ID {
		if err == nil || isNotFound(err) {
			return errNotFound("definition version " + r.PathValue("versionId"))
		}
		return err
	}
	v.State = v.EffectiveState(f.ActiveVersionID)
	writeJSON(w, http.StatusOK, v)
	return nil
}

// activeDefinition compiles the factory's active definition (nil when none).
func (s *Server) activeDefinition(ctx context.Context, f *core.Factory) (*core.CompiledDefinition, map[string]string, error) {
	if f.ActiveVersionID == "" {
		return nil, nil, nil
	}
	v, err := s.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
	if err != nil {
		return nil, nil, err
	}
	if s.opts.Compile == nil {
		return nil, v.Files, nil
	}
	def, _ := s.opts.Compile(v.Files)
	return def, v.Files, nil
}

func cloneFiles(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ---------- agents / automations / integrations ----------

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	as, err := s.opts.Store.ListAgents(r.Context(), f.ID)
	if err != nil {
		return err
	}
	if t := r.URL.Query().Get("type"); t != "" {
		as = slices.DeleteFunc(as, func(a core.AgentConfig) bool { return !strings.EqualFold(string(a.Type), t) })
	}
	writeJSON(w, http.StatusOK, listResponse("agents", as, ""))
	return nil
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	a, err := s.opts.Store.GetAgentByName(ctx, f.ID, name)
	if isNotFound(err) {
		a, err = s.opts.Store.GetAgent(ctx, name)
		if err == nil && a.FactoryID != f.ID {
			err = core.ErrNotFound
		}
	}
	if err != nil {
		if isNotFound(err) {
			return errNotFound("agent " + name)
		}
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}

func (s *Server) handleListAutomations(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	as, err := s.opts.Store.ListAutomations(r.Context(), f.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("automations", as, ""))
	return nil
}

func (s *Server) handleGetFactoryIntegrations(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	ints, err := s.opts.Store.GetFactoryIntegrations(r.Context(), f.ID)
	if err != nil {
		return err
	}
	if ints == nil {
		ints = map[string]map[string]string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"factory_id": f.ID, "integrations": ints})
	return nil
}
