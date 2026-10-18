package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// maxSecretEnvBytes bounds one injected NAME=value entry including the NUL terminator.
const maxSecretEnvBytes = 128 * 1024

var secretTypes = []core.SecretType{
	core.SecretGeneric, core.SecretAnthropicKey, core.SecretAnthropicOAuth, core.SecretBedrockKey, core.SecretBedrockAccess,
	core.SecretOpenAIKey, core.SecretDockerRegistry, core.SecretAWSECR,
}

// ---------- secrets ----------

func (s *Server) secretScope(ctx context.Context, p *Principal, r *http.Request, bodyFactory string) (string, error) {
	fid := firstNonEmpty(bodyFactory, r.URL.Query().Get("factory_id"))
	if fid == "" {
		return "", nil
	}
	f, err := s.factoryFor(ctx, p, fid)
	if err != nil {
		return "", err
	}
	return f.ID, nil
}

func (s *Server) findSecret(ctx context.Context, teamID, factoryID, name string) (*core.Secret, error) {
	ss, err := s.opts.Secrets.List(ctx, teamID, factoryID)
	if err != nil {
		return nil, err
	}
	for i := range ss {
		if ss[i].Name == name && ss[i].FactoryID == factoryID {
			return &ss[i], nil
		}
	}
	return nil, errNotFound("secret " + name)
}

func validateSecretValue(name string, value string) error {
	if value == "" {
		return errInvalid("value is required")
	}
	if len(name)+1+len(value)+1 > maxSecretEnvBytes {
		return errInvalid("secret value too large: NAME=value must fit in %d bytes", maxSecretEnvBytes)
	}
	if strings.ContainsRune(value, 0) {
		return errInvalid("secret values cannot contain NUL bytes")
	}
	return nil
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.Secrets == nil {
		return featureUnavailable("secret storage")
	}
	fid, err := s.secretScope(r.Context(), p, r, "")
	if err != nil {
		return err
	}
	ss, err := s.opts.Secrets.List(r.Context(), p.TeamID, fid)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("secrets", ss, ""))
	return nil
}

type secretRequest struct {
	Name      string          `json:"name"`
	Type      core.SecretType `json:"type,omitempty"`
	Value     string          `json:"value"`
	FactoryID string          `json:"factory_id,omitempty"`
}

func (s *Server) handleCreateSecret(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	if s.opts.Secrets == nil {
		return featureUnavailable("secret storage")
	}
	var req secretRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if !secretNameRe.MatchString(req.Name) {
		return errInvalid("name must start with a letter or '_' and contain only letters, digits and '_' (max 128)")
	}
	if req.Type == "" {
		req.Type = core.SecretGeneric
	}
	if !slices.Contains(secretTypes, req.Type) {
		return errInvalid("unknown secret type %q", req.Type)
	}
	if err := validateSecretValue(req.Name, req.Value); err != nil {
		return err
	}
	fid, err := s.secretScope(ctx, p, r, req.FactoryID)
	if err != nil {
		return err
	}
	if _, err := s.findSecret(ctx, p.TeamID, fid, req.Name); err == nil {
		return Errf(CodeConflict, "secret %q already exists; use PUT to update its value", req.Name)
	}
	sec, err := s.opts.Secrets.Put(ctx, p.TeamID, fid, req.Name, req.Type, []byte(req.Value))
	if err != nil {
		return err
	}
	s.forgeCredentialsChanged(p.TeamID)
	writeJSON(w, http.StatusCreated, sec)
	return nil
}

func (s *Server) handleGetSecret(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.Secrets == nil {
		return featureUnavailable("secret storage")
	}
	fid, err := s.secretScope(r.Context(), p, r, "")
	if err != nil {
		return err
	}
	sec, err := s.findSecret(r.Context(), p.TeamID, fid, r.PathValue("name"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, sec)
	return nil
}

func (s *Server) handleUpdateSecret(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	if s.opts.Secrets == nil {
		return featureUnavailable("secret storage")
	}
	var req secretRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	name := r.PathValue("name")
	fid, err := s.secretScope(ctx, p, r, req.FactoryID)
	if err != nil {
		return err
	}
	cur, err := s.findSecret(ctx, p.TeamID, fid, name)
	if err != nil {
		return err
	}
	if err := validateSecretValue(name, req.Value); err != nil {
		return err
	}
	typ := cur.Type
	if req.Type != "" {
		if !slices.Contains(secretTypes, req.Type) {
			return errInvalid("unknown secret type %q", req.Type)
		}
		typ = req.Type
	}
	sec, err := s.opts.Secrets.Put(ctx, p.TeamID, fid, name, typ, []byte(req.Value))
	if err != nil {
		return err
	}
	s.forgeCredentialsChanged(p.TeamID)
	writeJSON(w, http.StatusOK, sec)
	return nil
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	if s.opts.Secrets == nil {
		return featureUnavailable("secret storage")
	}
	fid, err := s.secretScope(ctx, p, r, "")
	if err != nil {
		return err
	}
	sec, err := s.findSecret(ctx, p.TeamID, fid, r.PathValue("name"))
	if err != nil {
		return err
	}
	if err := s.opts.Secrets.Delete(ctx, sec.ID); err != nil {
		return err
	}
	s.forgeCredentialsChanged(p.TeamID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------- environments ----------

type environmentRequest struct {
	Name          *string           `json:"name"`
	Description   *string           `json:"description"`
	Repos         []string          `json:"repos"`
	Image         *string           `json:"image"`
	SetupCommands []string          `json:"setup_commands"`
	EnvVars       map[string]string `json:"env_vars"`
}

func validateRepos(repos []string) error {
	for _, r := range repos {
		ref := r
		if i := strings.Index(r, ":"); i >= 0 {
			ref = r[i+1:]
		}
		owner, name, ok := strings.Cut(ref, "/")
		if !ok || owner == "" || name == "" {
			return errInvalid("repo %q must have the form [forge:]owner/name", r)
		}
	}
	return nil
}

func (s *Server) environmentFor(ctx context.Context, p *Principal, id string) (*core.Environment, error) {
	e, err := s.opts.Store.GetEnvironment(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, errNotFound("environment " + id)
		}
		return nil, err
	}
	if e.TeamID != p.TeamID {
		return nil, errNotFound("environment " + id)
	}
	return e, nil
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request, p *Principal) error {
	es, err := s.opts.Store.ListEnvironments(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("environments", es, ""))
	return nil
}

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req environmentRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		return errInvalid("name is required")
	}
	if err := validateRepos(req.Repos); err != nil {
		return err
	}
	e := &core.Environment{
		ID: core.NewID("env"), TeamID: p.TeamID, Name: strings.TrimSpace(*req.Name),
		Repos: req.Repos, SetupCommands: req.SetupCommands, EnvVars: req.EnvVars,
	}
	if e.Repos == nil {
		e.Repos = []string{}
	}
	if req.Description != nil {
		e.Description = *req.Description
	}
	if req.Image != nil {
		e.Image = *req.Image
	}
	if err := s.opts.Store.CreateEnvironment(r.Context(), e); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, e)
	return nil
}

func (s *Server) handleGetEnvironment(w http.ResponseWriter, r *http.Request, p *Principal) error {
	e, err := s.environmentFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, e)
	return nil
}

func (s *Server) handleUpdateEnvironment(w http.ResponseWriter, r *http.Request, p *Principal) error {
	e, err := s.environmentFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var req environmentRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			return errInvalid("name cannot be empty")
		}
		e.Name = strings.TrimSpace(*req.Name)
	}
	if req.Description != nil {
		e.Description = *req.Description
	}
	if req.Repos != nil {
		if err := validateRepos(req.Repos); err != nil {
			return err
		}
		e.Repos = req.Repos
	}
	if req.Image != nil {
		e.Image = *req.Image
	}
	if req.SetupCommands != nil {
		e.SetupCommands = req.SetupCommands
	}
	if req.EnvVars != nil {
		e.EnvVars = req.EnvVars
	}
	if err := s.opts.Store.UpdateEnvironment(r.Context(), e); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, e)
	return nil
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request, p *Principal) error {
	e, err := s.environmentFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := s.opts.Store.DeleteEnvironment(r.Context(), e.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------- machines / workers ----------

// MachineView is a machine declared by a factory definition.
type MachineView struct {
	FactoryID   string `json:"factory_id"`
	FactoryName string `json:"factory_name"`
	core.MachineSpec
}

func (s *Server) handleListMachines(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var fs []core.Factory
	if fid := r.URL.Query().Get("factory_id"); fid != "" {
		f, err := s.factoryFor(ctx, p, fid)
		if err != nil {
			return err
		}
		fs = []core.Factory{*f}
	} else {
		var err error
		if fs, err = s.opts.Store.ListFactories(ctx, p.TeamID, ""); err != nil {
			return err
		}
	}
	out := []MachineView{}
	for i := range fs {
		def, _, err := s.activeDefinition(ctx, &fs[i])
		if err != nil {
			return err
		}
		if def == nil {
			continue
		}
		names := make([]string, 0, len(def.Machines))
		for n := range def.Machines {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			spec := def.Machines[n]
			if spec.Name == "" {
				spec.Name = n
			}
			out = append(out, MachineView{FactoryID: fs[i].ID, FactoryName: fs[i].Name, MachineSpec: spec})
		}
	}
	writeJSON(w, http.StatusOK, listResponse("machines", out, ""))
	return nil
}

// WorkerView is a worker with a derived connection status.
type WorkerView struct {
	core.Worker
	Status string `json:"status"` // online | offline
}

func workerView(wk core.Worker, now time.Time) WorkerView {
	st := "offline"
	if now.Sub(wk.LastHeartbeat) < 2*time.Minute {
		st = "online"
	}
	return WorkerView{Worker: wk, Status: st}
}

func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ws, err := s.opts.Store.ListWorkers(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	now := s.now()
	out := make([]WorkerView, 0, len(ws))
	for _, wk := range ws {
		out = append(out, workerView(wk, now))
	}
	writeJSON(w, http.StatusOK, listResponse("workers", out, ""))
	return nil
}

func (s *Server) handleGetWorker(w http.ResponseWriter, r *http.Request, p *Principal) error {
	wk, err := s.opts.Store.GetWorker(r.Context(), r.PathValue("id"))
	if err != nil || wk.TeamID != p.TeamID {
		if err == nil || isNotFound(err) {
			return errNotFound("worker " + r.PathValue("id"))
		}
		return err
	}
	writeJSON(w, http.StatusOK, workerView(*wk, s.now()))
	return nil
}

// ---------- integrations ----------

var integrationProviders = []string{"github", "gitlab", "azure_devops", "slack", "microsoft_teams", "linear", "jira"}

type integrationRequest struct {
	Provider  *string           `json:"provider"`
	Name      *string           `json:"name"`
	Config    map[string]string `json:"config"`
	SecretRef *string           `json:"secret_ref"`
	FactoryID *string           `json:"factory_id"`
	Status    *string           `json:"status"`
}

// applyIntegrationStatus sets the status that an admin asks for: active or
// disabled. auth_failed is set only by the server.
func (s *Server) applyIntegrationStatus(in *core.Integration, status *string) error {
	if status == nil {
		return nil
	}
	switch *status {
	case core.IntegrationActive, core.IntegrationDisabled:
	default:
		return errInvalid("status must be active or disabled")
	}
	if in.EffectiveStatus() == *status {
		return nil
	}
	in.Status = *status
	in.StatusReason = ""
	if *status == core.IntegrationDisabled {
		in.StatusReason = "disabled by an admin"
	}
	t := s.now().UTC()
	in.StatusUpdatedAt = &t
	return nil
}

// forgeProviders are the integration providers of code forges. A record of
// a forge is one connection: one owner (config.org) and one credential.
var forgeProviders = []string{"github", "gitlab", "azure_devops"}

// checkForgeRecord checks a forge record before it is written: config.org
// is necessary, and so is a credential. A token (secret_ref) is a
// credential. For GitHub an app is one too: app_id with the private key in
// private_key_ref or secret_ref. A chat or tracker record goes to
// checkWorkspaceRecord.
func (s *Server) checkForgeRecord(in *core.Integration) error {
	if !slices.Contains(forgeProviders, in.Provider) {
		return checkWorkspaceRecord(in)
	}
	org := strings.TrimSpace(in.Config["org"])
	if org == "" {
		return errInvalid("config.org is necessary for a %s record: set it to the owner that the record serves (an organization, a user name or, for GitLab, the top-level group)", in.Provider)
	}
	if strings.Contains(org, "/") {
		return errInvalid("config.org %q must be one name without a slash (for GitLab, the top-level group)", org)
	}
	in.Config["org"] = org
	if in.Provider == "github" && in.Config["app_id"] != "" {
		if in.SecretRef == "" && in.Config["private_key_ref"] == "" {
			return errInvalid("a github app record needs its private key: set private_key_ref (or secret_ref) to the name of a team secret")
		}
		return nil
	}
	if in.SecretRef != "" {
		return nil
	}
	return errInvalid("a %s record needs a credential: set secret_ref to the name of a team secret that holds a token", in.Provider)
}

// checkWorkspaceRecord checks a chat or tracker record before it is
// written: the key that names its workspace (core.IntegrationWorkspaceKey:
// Slack and Teams team_id, Linear organization_id, Jira site) is necessary.
// A Jira site is kept as its host name (or cloud ID).
func checkWorkspaceRecord(in *core.Integration) error {
	key := core.IntegrationWorkspaceKey(in.Provider)
	if key == "" {
		return nil
	}
	v := core.NormalizeWorkspace(in.Provider, in.Config[key])
	if v == "" {
		what := map[string]string{
			"slack":           "the Slack workspace ID (T…)",
			"microsoft_teams": "the Microsoft Graph team ID",
			"linear":          "the Linear organization ID",
			"jira":            "the host name of the Jira site (acme.atlassian.net) or its cloud ID",
		}[in.Provider]
		return errInvalid("config.%s is necessary for a %s record: set it to %s that the record serves", key, in.Provider, what)
	}
	in.Config[key] = v
	return nil
}

// checkIntegrationFactory checks the factory of a new record: it must be a
// factory of the team.
func (s *Server) checkIntegrationFactory(ctx context.Context, p *Principal, factoryID string) error {
	if factoryID == "" {
		return nil
	}
	f, err := s.opts.Store.GetFactory(ctx, factoryID)
	if err != nil || f.TeamID != p.TeamID {
		return errInvalid("factory_id %q is not a factory of this team", factoryID)
	}
	return nil
}

// checkIntegrationUnique checks that the team has at most one record for
// each provider, factory and (for a forge) config.org or (for a chat or
// tracker) its workspace key.
// A second record gets 409. The case of org is ignored.
func (s *Server) checkIntegrationUnique(ctx context.Context, in *core.Integration) error {
	isForge := slices.Contains(forgeProviders, in.Provider)
	wsKey := core.IntegrationWorkspaceKey(in.Provider)
	if !isForge && wsKey == "" && in.FactoryID == "" {
		return nil
	}
	all, err := s.opts.Store.ListIntegrations(ctx, in.TeamID)
	if err != nil {
		return err
	}
	for _, x := range all {
		if x.ID == in.ID || x.Provider != in.Provider || x.FactoryID != in.FactoryID {
			continue
		}
		where := "the team"
		if in.FactoryID != "" {
			where = "factory " + in.FactoryID
		}
		if wsKey != "" {
			if !strings.EqualFold(core.IntegrationWorkspace(x), in.Config[wsKey]) {
				continue
			}
			return Errf(CodeConflict, "%s already has a %s record for config.%s %q (%s)", where, in.Provider, wsKey, in.Config[wsKey], x.ID)
		}
		if !isForge {
			return Errf(CodeConflict, "factory %s already has a %s integration (%s)", in.FactoryID, in.Provider, x.ID)
		}
		if !strings.EqualFold(strings.TrimSpace(x.Config["org"]), in.Config["org"]) {
			continue
		}
		return Errf(CodeConflict, "%s already has a %s record for the owner %q (%s)", where, in.Provider, in.Config["org"], x.ID)
	}
	return nil
}

func (s *Server) integrationFor(ctx context.Context, p *Principal, id string) (*core.Integration, error) {
	in, err := s.opts.Store.GetIntegration(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, errNotFound("integration " + id)
		}
		return nil, err
	}
	if in.TeamID != p.TeamID {
		return nil, errNotFound("integration " + id)
	}
	return in, nil
}

func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ins, err := s.opts.Store.ListIntegrations(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	if prov := r.URL.Query().Get("provider"); prov != "" {
		ins = slices.DeleteFunc(ins, func(i core.Integration) bool { return i.Provider != prov })
	}
	writeJSON(w, http.StatusOK, listResponse("integrations", ins, ""))
	return nil
}

func (s *Server) handleCreateIntegration(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req integrationRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.Provider == nil || !slices.Contains(integrationProviders, *req.Provider) {
		return errInvalid("provider must be one of %s", strings.Join(integrationProviders, ", "))
	}
	in := &core.Integration{ID: core.NewID("int"), TeamID: p.TeamID, Provider: *req.Provider,
		Config: req.Config, CreatedAt: s.now()}
	in.Name = *req.Provider
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		in.Name = strings.TrimSpace(*req.Name)
	}
	if req.SecretRef != nil {
		if *req.SecretRef != "" && !secretNameRe.MatchString(*req.SecretRef) {
			return errInvalid("invalid secret_ref %q", *req.SecretRef)
		}
		in.SecretRef = *req.SecretRef
	}
	if in.Config == nil {
		in.Config = map[string]string{}
	}
	if req.FactoryID != nil {
		in.FactoryID = strings.TrimSpace(*req.FactoryID)
	}
	if err := s.checkIntegrationFactory(r.Context(), p, in.FactoryID); err != nil {
		return err
	}
	if err := s.checkForgeRecord(in); err != nil {
		return err
	}
	if err := s.checkIntegrationUnique(r.Context(), in); err != nil {
		return err
	}
	if err := s.applyIntegrationStatus(in, req.Status); err != nil {
		return err
	}
	if err := s.opts.Store.UpsertIntegration(r.Context(), in); err != nil {
		return err
	}
	s.forgeCredentialsChanged(p.TeamID)
	writeJSON(w, http.StatusCreated, in)
	return nil
}

func (s *Server) handleGetIntegration(w http.ResponseWriter, r *http.Request, p *Principal) error {
	in, err := s.integrationFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, in)
	return nil
}

func (s *Server) handleUpdateIntegration(w http.ResponseWriter, r *http.Request, p *Principal) error {
	in, err := s.integrationFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var req integrationRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.Provider != nil && *req.Provider != in.Provider {
		return errInvalid("provider cannot change")
	}
	if req.FactoryID != nil && strings.TrimSpace(*req.FactoryID) != in.FactoryID {
		return errInvalid("factory_id cannot change")
	}
	if err := s.applyIntegrationStatus(in, req.Status); err != nil {
		return err
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		in.Name = strings.TrimSpace(*req.Name)
	}
	if req.Config != nil {
		in.Config = req.Config
	}
	if in.Config == nil {
		in.Config = map[string]string{}
	}
	if req.SecretRef != nil {
		if *req.SecretRef != "" && !secretNameRe.MatchString(*req.SecretRef) {
			return errInvalid("invalid secret_ref %q", *req.SecretRef)
		}
		in.SecretRef = *req.SecretRef
	}
	if err := s.checkForgeRecord(in); err != nil {
		return err
	}
	if err := s.checkIntegrationUnique(r.Context(), in); err != nil {
		return err
	}
	if err := s.opts.Store.UpsertIntegration(r.Context(), in); err != nil {
		return err
	}
	s.forgeCredentialsChanged(p.TeamID)
	writeJSON(w, http.StatusOK, in)
	return nil
}

func (s *Server) handleDeleteIntegration(w http.ResponseWriter, r *http.Request, p *Principal) error {
	in, err := s.integrationFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := s.opts.Store.DeleteIntegration(r.Context(), in.ID); err != nil {
		return err
	}
	s.forgeCredentialsChanged(p.TeamID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------- identity: me, teams, users, API keys ----------

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, p *Principal) error {
	team, err := s.opts.Store.GetTeam(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	factoryRoles := map[string]core.Role{}
	if p.User != nil {
		for fid, role := range p.User.FactoryRoles {
			if ValidRole(role) {
				factoryRoles[fid] = core.NormalizeRole(role)
			}
		}
	}
	body := map[string]any{"user": p.User, "team": team, "role": core.NormalizeRole(p.Role),
		"factory_roles": factoryRoles, "login_providers": s.loginProviders(r)}
	if p.APIKey != nil {
		body["api_key"] = p.APIKey
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

// handleLoginProviders lists the interactive sign-in options (unauthenticated),
// so the dashboard login page can render SSO buttons.
func (s *Server) handleLoginProviders(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	writeJSON(w, http.StatusOK, map[string]any{"login_providers": s.loginProviders(r)})
	return nil
}

// handleVersion reports the server build (unauthenticated).
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	v := s.opts.Version
	if v.Version == "" {
		v.Version = "dev"
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request, p *Principal) error {
	team, err := s.opts.Store.GetTeam(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("teams", []core.Team{*team}, ""))
	return nil
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if r.PathValue("id") != p.TeamID {
		return errNotFound("team " + r.PathValue("id"))
	}
	team, err := s.opts.Store.GetTeam(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, team)
	return nil
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, p *Principal) error {
	us, err := s.opts.Store.ListUsers(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	// Service accounts are listed by /service-accounts.
	us = slices.DeleteFunc(us, func(u core.User) bool { return u.IsServiceAccount() })
	writeJSON(w, http.StatusOK, listResponse("users", us, ""))
	return nil
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request, p *Principal) error {
	u, err := s.opts.Store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil || u.TeamID != p.TeamID {
		if err == nil || isNotFound(err) {
			return errNotFound("user " + r.PathValue("id"))
		}
		return err
	}
	writeJSON(w, http.StatusOK, u)
	return nil
}

type updateUserRequest struct {
	Name *string    `json:"name,omitempty"`
	Role *core.Role `json:"role,omitempty"`
	// FactoryRoles changes role overrides for single factories. Each key is
	// a factory ID; a role sets the override and null removes it. Factories
	// that are not named keep their override.
	FactoryRoles map[string]*core.Role `json:"factory_roles,omitempty"`
}

// handleUpdateUser changes a team member's name, team-wide role or role
// overrides for single factories. The last admin of a team cannot be
// demoted.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req updateUserRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	u, err := s.opts.Store.GetUser(ctx, r.PathValue("id"))
	if err != nil || u.TeamID != p.TeamID {
		if err == nil || isNotFound(err) {
			return errNotFound("user " + r.PathValue("id"))
		}
		return err
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 200 {
			return errInvalid("name must be 1-200 characters")
		}
		u.Name = name
	}
	if req.Role != nil {
		if !ValidRole(*req.Role) {
			return errInvalid("role must be admin, editor or user")
		}
		*req.Role = core.NormalizeRole(*req.Role)
		if u.Role == core.RoleAdmin && *req.Role != core.RoleAdmin {
			users, err := s.opts.Store.ListUsers(ctx, p.TeamID)
			if err != nil {
				return err
			}
			admins := 0
			for _, x := range users {
				if x.Role == core.RoleAdmin && !x.IsServiceAccount() {
					admins++
				}
			}
			if admins <= 1 {
				return Errf(CodeConflict, "cannot demote the team's last admin")
			}
		}
		u.Role = *req.Role
	}
	if err := s.applyFactoryRoles(ctx, p, u, req.FactoryRoles); err != nil {
		return err
	}
	if err := s.opts.Store.UpdateUser(ctx, u); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, u)
	return nil
}

// applyFactoryRoles sets or removes role overrides for single factories.
// Each factory must belong to the team of the caller.
func (s *Server) applyFactoryRoles(ctx context.Context, p *Principal, u *core.User, in map[string]*core.Role) error {
	for fid, role := range in {
		if _, err := s.factoryFor(ctx, p, fid); err != nil {
			return errInvalid("factory_roles: factory %s does not exist", fid)
		}
		if role == nil {
			delete(u.FactoryRoles, fid)
			continue
		}
		if !ValidRole(*role) {
			return errInvalid("factory_roles.%s: role must be admin, editor or user", fid)
		}
		if u.FactoryRoles == nil {
			u.FactoryRoles = map[string]core.Role{}
		}
		u.FactoryRoles[fid] = core.NormalizeRole(*role)
	}
	if len(u.FactoryRoles) == 0 {
		u.FactoryRoles = nil
	}
	return nil
}

type createUserRequest struct {
	Email string    `json:"email"`
	Name  string    `json:"name"`
	Role  core.Role `json:"role"`
	// CreateKey also mints an API key for the new user.
	CreateKey bool `json:"create_api_key,omitempty"`
}

func normalizeEmail(e string) (string, error) {
	a, err := mail.ParseAddress(strings.TrimSpace(e))
	if err != nil || a.Address != strings.TrimSpace(e) {
		return "", errInvalid("invalid email address %q", e)
	}
	return strings.ToLower(a.Address), nil
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req createUserRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		return err
	}
	if req.Role == "" {
		req.Role = core.RoleEditor
	}
	if !ValidRole(req.Role) {
		return errInvalid("role must be admin, editor or user")
	}
	req.Role = core.NormalizeRole(req.Role)
	if _, err := s.opts.Store.GetUserByEmail(ctx, email); err == nil {
		return Errf(CodeConflict, "a user with email %s already exists", email)
	} else if !isNotFound(err) {
		return err
	}
	u := &core.User{ID: core.NewID("usr"), TeamID: p.TeamID, Email: email, Name: strings.TrimSpace(req.Name),
		Role: req.Role, CreatedAt: s.now()}
	if u.Name == "" {
		u.Name = email
	}
	if err := s.opts.Store.CreateUser(ctx, u); err != nil {
		return err
	}
	resp := map[string]any{"user": u}
	if req.CreateKey {
		k, plain, err := NewAPIKey(p.TeamID, u.ID, "default")
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

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ks, err := s.opts.Store.ListAPIKeys(r.Context(), p.TeamID)
	if err != nil {
		return err
	}
	if !p.Can(core.RoleAdmin) {
		ks = slices.DeleteFunc(ks, func(k core.APIKey) bool { return k.UserID != p.UserID() })
	}
	writeJSON(w, http.StatusOK, listResponse("api_keys", ks, ""))
	return nil
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req struct {
		Name   string `json:"name"`
		UserID string `json:"user_id,omitempty"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		return errInvalid("name must be 1-100 characters")
	}
	uid := p.UserID()
	if req.UserID != "" && req.UserID != uid {
		if !p.Can(core.RoleAdmin) {
			return errForbidden("only admins can create API keys for other users")
		}
		u, err := s.opts.Store.GetUser(ctx, req.UserID)
		if err != nil || u.TeamID != p.TeamID {
			if err == nil || isNotFound(err) {
				return errNotFound("user " + req.UserID)
			}
			return err
		}
		if u.IsServiceAccount() {
			return errInvalid("user %s is a service account: use POST /service-accounts/{id}/keys", u.ID)
		}
		uid = u.ID
	}
	if p.User != nil && p.User.IsServiceAccount() && uid == p.UserID() {
		return errForbidden("a service account cannot create its own API keys: an admin uses POST /service-accounts/{id}/keys")
	}
	k, plain, err := NewAPIKey(p.TeamID, uid, req.Name)
	if err != nil {
		return err
	}
	if err := s.opts.Store.CreateAPIKey(ctx, k); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": k, "key": plain})
	return nil
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	ks, err := s.opts.Store.ListAPIKeys(ctx, p.TeamID)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	idx := slices.IndexFunc(ks, func(k core.APIKey) bool { return k.ID == id })
	if idx < 0 {
		return errNotFound("API key " + id)
	}
	if ks[idx].UserID != p.UserID() && !p.Can(core.RoleAdmin) {
		return errForbidden("only admins can revoke other users' API keys")
	}
	if err := s.opts.Store.DeleteAPIKey(ctx, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------- networking, schemas ----------

func (s *Server) handleNetworkPolicies(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.NetworkPolicies == nil {
		return featureUnavailable("network policy catalog")
	}
	writeJSON(w, http.StatusOK, s.opts.NetworkPolicies())
	return nil
}

func (s *Server) handleSchemaVersions(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	if s.opts.Schemas == nil {
		return featureUnavailable("factory-file schemas")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_version": s.opts.SchemaVersion,
		"versions":        []string{s.opts.SchemaVersion},
	})
	return nil
}

// SchemaDocuments returns the schema documents of a version.
func (s *Server) SchemaDocuments(version string) (map[string]json.RawMessage, error) {
	if s.opts.Schemas == nil {
		return nil, featureUnavailable("factory-file schemas")
	}
	if version != s.opts.SchemaVersion {
		return nil, errNotFound("schema version " + version)
	}
	return s.opts.Schemas(), nil
}

// SchemaDocument returns one schema document; the ".schema.json" suffix is optional.
func (s *Server) SchemaDocument(version, doc string) (json.RawMessage, error) {
	docs, err := s.SchemaDocuments(version)
	if err != nil {
		return nil, err
	}
	for _, cand := range []string{doc, doc + ".schema.json", doc + ".json"} {
		if d, ok := docs[cand]; ok {
			return d, nil
		}
	}
	return nil, errNotFound("schema document " + doc)
}

func (s *Server) handleSchemaVersion(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	docs, err := s.SchemaDocuments(r.PathValue("version"))
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, docs)
	return nil
}

func (s *Server) handleSchemaDocument(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	d, err := s.SchemaDocument(r.PathValue("version"), r.PathValue("document"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/schema+json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(d) //nolint:gosec // G705: built-in JSON schema bytes, served as application/schema+json
	return nil
}

// ---------- bootstrap ----------

type bootstrapRequest struct {
	TeamName   string `json:"team_name"`
	AdminEmail string `json:"admin_email"`
	AdminName  string `json:"admin_name,omitempty"`
	KeyName    string `json:"key_name,omitempty"`
}

// BootstrapResult is returned by the one-time bootstrap.
type BootstrapResult struct {
	Team   *core.Team   `json:"team"`
	User   *core.User   `json:"user"`
	APIKey *core.APIKey `json:"api_key"`
	Key    string       `json:"key"`
}

// Bootstrap creates the first team, its admin user and an API key. It fails
// with a conflict once any team exists.
func (s *Server) Bootstrap(ctx context.Context, teamName, email, name, keyName string) (*BootstrapResult, error) {
	s.bootstrapM.Lock()
	defer s.bootstrapM.Unlock()
	teams, err := s.opts.Store.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	if len(teams) > 0 {
		return nil, Errf(CodeConflict, "this server is already bootstrapped")
	}
	teamName = strings.TrimSpace(teamName)
	if teamName == "" {
		return nil, errInvalid("team_name is required")
	}
	email, err = normalizeEmail(email)
	if err != nil {
		return nil, err
	}
	now := s.now()
	team := &core.Team{ID: core.NewID("team"), Name: teamName, CreatedAt: now}
	if err := s.opts.Store.CreateTeam(ctx, team); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		name = email
	}
	u := &core.User{ID: core.NewID("usr"), TeamID: team.ID, Email: email, Name: strings.TrimSpace(name), Role: core.RoleAdmin, CreatedAt: now}
	if err := s.opts.Store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	if keyName == "" {
		keyName = "bootstrap"
	}
	k, plain, err := NewAPIKey(team.ID, u.ID, keyName)
	if err != nil {
		return nil, err
	}
	if err := s.opts.Store.CreateAPIKey(ctx, k); err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "bootstrapped server", "team", team.ID, "admin", u.Email)
	return &BootstrapResult{Team: team, User: u, APIKey: k, Key: plain}, nil
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	var req bootstrapRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	res, err := s.Bootstrap(r.Context(), req.TeamName, req.AdminEmail, req.AdminName, req.KeyName)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, res)
	return nil
}
