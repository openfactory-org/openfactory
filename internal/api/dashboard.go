package api

// Endpoints that back dashboard actions which edit definition files on the
// caller's behalf (agent settings, automations, integrations, settings,
// version restore) plus on-demand scoring and self-improvement. Every action
// the dashboard offers is available here so the React app needs nothing but
// the REST API.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

var resourceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// IntegrationType is one integration a factory can declare in factory.yaml.
type IntegrationType struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
}

// IntegrationTypes is the catalog of declarable factory integrations.
var IntegrationTypes = []IntegrationType{
	{"slack", "Slack", "Mention the foreman in channels and threads."},
	{"microsoft-teams", "Microsoft Teams", "Mention the foreman in Teams channels."},
	{"linear", "Linear", "Assign or mention the foreman on Linear issues."},
	{"jira", "Jira", "Route Jira issues to the factory."},
}

// activeFiles returns a copy of the files of the factory's active definition
// (an empty map when nothing has been applied).
func (s *Server) activeFiles(r *http.Request, f *core.Factory) (map[string]string, error) {
	if f.ActiveVersionID == "" {
		return map[string]string{}, nil
	}
	v, err := s.opts.Store.GetDefinitionVersion(r.Context(), f.ActiveVersionID)
	if err != nil {
		return nil, err
	}
	return cloneFiles(v.Files), nil
}

// ---------- settings ----------

// FactorySettings is the response of GET /factories/{uid}/settings.
type FactorySettings struct {
	FactoryID          string             `json:"factory_id"`
	Editable           bool               `json:"editable"`
	CredentialStrategy string             `json:"credential_strategy"`
	Repositories       []core.RepoRef     `json:"repositories"`
	Machines           []core.MachineSpec `json:"machines"`
	IntegrationTypes   []IntegrationType  `json:"integration_types"`
}

func (s *Server) handleGetFactorySettings(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	out := FactorySettings{
		FactoryID: f.ID, Editable: f.Source != core.DefinitionGit, CredentialStrategy: "EXECUTOR",
		Repositories: []core.RepoRef{}, Machines: []core.MachineSpec{}, IntegrationTypes: IntegrationTypes,
	}
	def, _, err := s.activeDefinition(r.Context(), f)
	if err != nil {
		return err
	}
	if def != nil {
		if def.CredentialStrategy != "" {
			out.CredentialStrategy = def.CredentialStrategy
		}
		if def.Repositories != nil {
			out.Repositories = def.Repositories
		}
		names := make([]string, 0, len(def.Machines))
		for k := range def.Machines {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, n := range names {
			rs := def.Machines[n]
			if rs.Name == "" {
				rs.Name = n
			}
			out.Machines = append(out.Machines, rs)
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// ---------- factory integrations ----------

type putIntegrationsRequest struct {
	Integrations []string `json:"integrations"`
}

func (s *Server) handlePutFactoryIntegrations(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	var req putIntegrationsRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	var providers []string
	for _, it := range IntegrationTypes {
		if slices.Contains(req.Integrations, it.Type) {
			providers = append(providers, it.Type)
		}
	}
	for _, t := range req.Integrations {
		if !slices.ContainsFunc(IntegrationTypes, func(it IntegrationType) bool { return it.Type == t }) {
			return errInvalid("unknown integration %q", t)
		}
	}
	if slices.Contains(providers, "linear") && slices.Contains(providers, "jira") {
		return errInvalid("declare at most one issue tracker: linear and jira are mutually exclusive")
	}
	if f.Source == core.DefinitionGit {
		return Errf(CodeOperationNotSupported, "integrations of a git-backed factory are declared in its factory.yaml")
	}
	if f.ActiveVersionID != "" {
		files, err := s.activeFiles(r, f)
		if err != nil {
			return err
		}
		fy, err := setFactoryYAMLIntegrations(files["factory.yaml"], providers)
		if err != nil {
			return errInvalid("factory.yaml: %v", err)
		}
		files["factory.yaml"] = fy
		if err := s.applyFiles(ctx, p, f, files, "Update integrations"); err != nil {
			return err
		}
	}
	current, err := s.opts.Store.GetFactoryIntegrations(ctx, f.ID)
	if err != nil {
		return err
	}
	if err := s.opts.Store.SetFactoryIntegrations(ctx, f.ID, providers, current); err != nil {
		return err
	}
	ints, err := s.opts.Store.GetFactoryIntegrations(ctx, f.ID)
	if err != nil {
		return err
	}
	if ints == nil {
		ints = map[string]map[string]string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"factory_id": f.ID, "integrations": ints})
	return nil
}

// setFactoryYAMLIntegrations rewrites factory.yaml's integrations list,
// keeping provider-specific settings of entries that stay.
func setFactoryYAMLIntegrations(src string, providers []string) (string, error) {
	root, err := parseYAMLMapping(src)
	if err != nil {
		return "", err
	}
	existing := map[string]*yaml.Node{}
	if seq := mapGet(root, "integrations"); seq != nil && seq.Kind == yaml.SequenceNode {
		for _, item := range seq.Content {
			existing[nodeString(mapGet(item, "type"))] = item
		}
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, p := range providers {
		if item, ok := existing[p]; ok {
			seq.Content = append(seq.Content, item)
			continue
		}
		m := yamlMap()
		mapSet(m, "type", scalarNode(p))
		seq.Content = append(seq.Content, m)
	}
	mapSet(root, "integrations", seq)
	return encodeYAMLNode(root), nil
}

// ---------- definition versions ----------

func (s *Server) handleRestoreDefinitionVersion(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if f.Source == core.DefinitionGit {
		return Errf(CodeOperationNotSupported, "this factory's definition lives in %s: revert it there", f.GitRepo)
	}
	id := r.PathValue("versionId")
	v, err := s.opts.Store.GetDefinitionVersion(ctx, id)
	if err != nil || v.FactoryID != f.ID {
		if err == nil || isNotFound(err) {
			return errNotFound("definition version " + id)
		}
		return err
	}
	nv, err := s.opts.Engine.ApplyDefinition(ctx, f.ID, cloneFiles(v.Files), p.ActorID(), "Restore version "+shortID(v.ID), "")
	if err != nil {
		return applyError("that version no longer validates", nv, err)
	}
	if nv == nil || !nv.Valid {
		return validationError("that version no longer validates", versionErrors(nv))
	}
	writeJSON(w, http.StatusOK, nv)
	return nil
}

func shortID(id string) string {
	if i := strings.IndexByte(id, '_'); i >= 0 && len(id) > i+9 {
		return id[i+1 : i+9]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ---------- agent spec ----------

// AgentSpec is the editable frontmatter of agents/<name>/agent.md.
type AgentSpec struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// HasFile is false when the agent is built in (no agent.md in the definition).
	HasFile  bool `json:"has_file"`
	Editable bool `json:"editable"`

	Description string `json:"description"`
	// HarnessType is "" (inherit agentDefaults / model shorthand), "claude" or "codex".
	HarnessType        string   `json:"harness_type"`
	Model              string   `json:"model"`
	ReasoningLevel     string   `json:"reasoning_level"`
	AuthSecret         string   `json:"auth_secret"`
	Machine            string   `json:"machine"`
	WorkerHost         string   `json:"worker_host"`
	EnvironmentID      string   `json:"environment_id"`
	Secrets            []string `json:"secrets"`
	MCPServers         string   `json:"mcp_servers"` // YAML mapping
	IdleTimeoutMinutes string   `json:"idle_timeout_minutes"`
	Prompt             string   `json:"prompt"`
	// Machines lists the machine names declared in the definition (for pickers).
	Machines []string `json:"machines"`
}

// AgentSpecUpdate is the body of PUT /factories/{uid}/agents/{name}/spec.
type AgentSpecUpdate struct {
	Description        string   `json:"description"`
	HarnessType        string   `json:"harness_type"`
	Model              string   `json:"model"`
	ReasoningLevel     string   `json:"reasoning_level"`
	AuthSecret         string   `json:"auth_secret"`
	Machine            string   `json:"machine"`
	WorkerHost         string   `json:"worker_host"`
	EnvironmentID      string   `json:"environment_id"`
	Secrets            []string `json:"secrets"`
	MCPServers         string   `json:"mcp_servers"`
	IdleTimeoutMinutes string   `json:"idle_timeout_minutes"`
	// Prompt replaces the instructions body; nil keeps it.
	Prompt  *string `json:"prompt"`
	Message string  `json:"message,omitempty"`
}

func agentPath(name string) string { return "agents/" + name + "/agent.md" }

func (s *Server) agentSpec(r *http.Request, f *core.Factory, name string) (*AgentSpec, map[string]string, error) {
	ctx := r.Context()
	agent, err := s.opts.Store.GetAgentByName(ctx, f.ID, name)
	if err != nil {
		if isNotFound(err) {
			return nil, nil, errNotFound("agent " + name)
		}
		return nil, nil, err
	}
	files, err := s.activeFiles(r, f)
	if err != nil {
		return nil, nil, err
	}
	spec := &AgentSpec{Name: agent.Name, Path: agentPath(name), Description: agent.Description, Prompt: agent.Instructions, Secrets: []string{}, Machines: []string{}}
	if src, ok := files[spec.Path]; ok {
		spec.HasFile = true
		if d, err := parseMarkdownDoc(src); err == nil {
			fillAgentSpec(spec, d)
		}
	}
	spec.Editable = spec.HasFile && f.Source != core.DefinitionGit
	if s.opts.Compile != nil && len(files) > 0 {
		if def, _ := s.opts.Compile(files); def != nil {
			for k := range def.Machines {
				spec.Machines = append(spec.Machines, k)
			}
			sort.Strings(spec.Machines)
		}
	}
	return spec, files, nil
}

func fillAgentSpec(f *AgentSpec, d *mdDoc) {
	f.Description = nodeString(mapGet(d.front, "description"))
	f.Model = nodeString(mapGet(d.front, "model"))
	f.Machine = nodeString(mapGet(d.front, "machine"))
	f.WorkerHost = nodeString(mapGet(d.front, "workerHost"))
	f.EnvironmentID = nodeString(mapGet(d.front, "environmentId"))
	if ss := nodeStrings(mapGet(d.front, "secrets")); ss != nil {
		f.Secrets = ss
	}
	if n := mapGet(d.front, "mcpServers"); n != nil {
		f.MCPServers = encodeYAMLNode(n)
	}
	f.IdleTimeoutMinutes = nodeString(mapGet(d.front, "idleTimeoutMinutes"))
	f.Prompt = strings.TrimLeft(d.body, "\n")
	if h := mapGet(d.front, "harness"); h != nil && h.Kind == yaml.MappingNode {
		f.HarnessType = nodeString(mapGet(h, "type"))
		if f.HarnessType == "claude-code" {
			f.HarnessType = "claude"
		}
		f.Model = nodeString(mapGet(h, "model"))
		f.ReasoningLevel = nodeString(mapGet(h, "reasoningLevel"))
		if a := mapGet(h, "auth"); a != nil {
			f.AuthSecret = nodeString(mapGet(a, "secretName"))
		}
	}
}

// applyAgentSpec writes the update into the agent document, preserving every
// frontmatter key it does not manage.
func applyAgentSpec(d *mdDoc, f AgentSpecUpdate) error {
	setOrDelete(d.front, "description", f.Description)
	switch f.HarnessType {
	case "":
		mapDelete(d.front, "harness")
		setOrDelete(d.front, "model", f.Model)
	case "claude", "codex":
		mapDelete(d.front, "model")
		h := mapGet(d.front, "harness")
		if h == nil || h.Kind != yaml.MappingNode {
			h = yamlMap()
		}
		mapSet(h, "type", scalarNode(f.HarnessType))
		setOrDelete(h, "model", f.Model)
		setOrDelete(h, "reasoningLevel", f.ReasoningLevel)
		if strings.TrimSpace(f.AuthSecret) != "" {
			a := yamlMap()
			mapSet(a, "source", scalarNode("managedSecret"))
			mapSet(a, "secretName", scalarNode(strings.TrimSpace(f.AuthSecret)))
			mapSet(h, "auth", a)
		} else {
			mapDelete(h, "auth")
		}
		mapSet(d.front, "harness", h)
	default:
		return errInvalid("unknown harness %q (use claude or codex)", f.HarnessType)
	}
	setOrDelete(d.front, "machine", f.Machine)
	setOrDelete(d.front, "workerHost", f.WorkerHost)
	setOrDelete(d.front, "environmentId", f.EnvironmentID)
	var secrets []string
	for _, sn := range f.Secrets {
		if sn = strings.TrimSpace(sn); sn != "" && !slices.Contains(secrets, sn) {
			if !secretNameRe.MatchString(sn) {
				return errInvalid("invalid secret name %q", sn)
			}
			secrets = append(secrets, sn)
		}
	}
	if len(secrets) > 0 {
		mapSet(d.front, "secrets", seqNode(secrets))
	} else {
		mapDelete(d.front, "secrets")
	}
	if strings.TrimSpace(f.MCPServers) != "" {
		mcp, err := parseYAMLMapping(f.MCPServers)
		if err != nil {
			return errInvalid("mcp_servers: %v", err)
		}
		mapSet(d.front, "mcpServers", mcp)
	} else {
		mapDelete(d.front, "mcpServers")
	}
	if t := strings.TrimSpace(f.IdleTimeoutMinutes); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > 60 {
			return errInvalid("idle_timeout_minutes must be 0 through 60")
		}
		mapSet(d.front, "idleTimeoutMinutes", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)})
	} else {
		mapDelete(d.front, "idleTimeoutMinutes")
	}
	if f.Prompt != nil {
		d.body = strings.ReplaceAll(*f.Prompt, "\r\n", "\n")
	}
	return nil
}

func (s *Server) handleGetAgentSpec(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	spec, _, err := s.agentSpec(r, f, r.PathValue("name"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, spec)
	return nil
}

func (s *Server) handlePutAgentSpec(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	if f.Source == core.DefinitionGit {
		return Errf(CodeOperationNotSupported, "this factory's definition lives in %s: edit %s through a pull request", f.GitRepo, agentPath(name))
	}
	var req AgentSpecUpdate
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	spec, files, err := s.agentSpec(r, f, name)
	if err != nil {
		return err
	}
	if !spec.HasFile {
		return Errf(CodeOperationNotSupported, "agent %s has no %s in the active definition", name, spec.Path)
	}
	doc, err := parseMarkdownDoc(files[spec.Path])
	if err != nil {
		return validationError("agent file cannot be parsed", []core.ValidationError{{File: spec.Path, Message: err.Error()}})
	}
	if err := applyAgentSpec(doc, req); err != nil {
		return err
	}
	out, err := doc.String()
	if err != nil {
		return err
	}
	files[spec.Path] = out
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "Update agent " + name
	}
	if err := s.applyFiles(ctx, p, f, files, msg); err != nil {
		return err
	}
	if nf, err := s.opts.Store.GetFactory(ctx, f.ID); err == nil {
		f = nf
	}
	spec, _, err = s.agentSpec(r, f, name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, spec)
	return nil
}

// ---------- automations ----------

// AutomationFile is the response of GET /factories/{uid}/automations/{name}.
type AutomationFile struct {
	Name       string           `json:"name"`
	Path       string           `json:"path"`
	Content    string           `json:"content"`
	Editable   bool             `json:"editable"`
	Automation *core.Automation `json:"automation,omitempty"`
}

// NewAutomationTemplate is the starting content of a new automation.
const NewAutomationTemplate = `---
enabled: true
agent: foreman
triggers:
  - provider: schedule
    event: cron_fired
    schedule:
      cron: "0 9 * * 1"
---

Describe what the agent should do when this automation fires.
`

func automationPath(name string) string { return "automations/" + name + "/automation.md" }

type automationRequest struct {
	Content string `json:"content"`
	Message string `json:"message,omitempty"`
}

func (s *Server) handleGetAutomation(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	files, err := s.activeFiles(r, f)
	if err != nil {
		return err
	}
	content, ok := files[automationPath(name)]
	if !ok {
		return errNotFound("automation " + name)
	}
	out := AutomationFile{Name: name, Path: automationPath(name), Content: content, Editable: f.Source != core.DefinitionGit}
	if autos, err := s.opts.Store.ListAutomations(ctx, f.ID); err == nil {
		for i := range autos {
			if autos[i].Name == name {
				out.Automation = &autos[i]
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// automationFiles returns the active files with the automation replaced by content.
func (s *Server) automationFiles(r *http.Request, f *core.Factory, name, content string) (map[string]string, string, error) {
	if !resourceNameRe.MatchString(name) || name == "new" {
		return nil, "", errInvalid("automation names use letters, numbers, '-' and '_' (and cannot be \"new\")")
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		return nil, "", errInvalid("content is empty")
	}
	if len(content) > 512*1024 {
		return nil, "", errInvalid("content is larger than 512 KB")
	}
	path := automationPath(name)
	if _, err := parseMarkdownDoc(content); err != nil {
		return nil, path, validationError("automation file cannot be parsed", []core.ValidationError{{File: path, Message: err.Error()}})
	}
	files, err := s.activeFiles(r, f)
	if err != nil {
		return nil, path, err
	}
	files[path] = content
	return files, path, nil
}

func (s *Server) handlePutAutomation(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if f.Source == core.DefinitionGit {
		return Errf(CodeOperationNotSupported, "automations of a git-backed factory change through a pull request in %s", f.GitRepo)
	}
	var req automationRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	name := r.PathValue("name")
	files, path, err := s.automationFiles(r, f, name, req.Content)
	if err != nil {
		return err
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "Save automation " + name
	}
	if err := s.applyFiles(ctx, p, f, files, msg); err != nil {
		return err
	}
	out := AutomationFile{Name: name, Path: path, Content: files[path], Editable: true}
	if autos, err := s.opts.Store.ListAutomations(ctx, f.ID); err == nil {
		for i := range autos {
			if autos[i].Name == name {
				out.Automation = &autos[i]
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleDeleteAutomation(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if f.Source == core.DefinitionGit {
		return Errf(CodeOperationNotSupported, "automations of a git-backed factory change through a pull request in %s", f.GitRepo)
	}
	name := r.PathValue("name")
	files, err := s.activeFiles(r, f)
	if err != nil {
		return err
	}
	prefix := "automations/" + name + "/"
	found := false
	for k := range files {
		if strings.HasPrefix(k, prefix) {
			delete(files, k)
			found = true
		}
	}
	if !found {
		return errNotFound("automation " + name)
	}
	if err := s.applyFiles(ctx, p, f, files, "Delete automation "+name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleValidateAutomation(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	var req automationRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	files, _, err := s.automationFiles(r, f, r.PathValue("name"), req.Content)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) && ae.Extra != nil {
			if errs, ok := ae.Extra["errors"].([]core.ValidationError); ok {
				writeJSON(w, http.StatusOK, ValidationResult{Valid: false, Errors: errs})
				return nil
			}
		}
		return err
	}
	res, err := s.ValidateFactoryFiles(r.Context(), f.ID, files)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

// ---------- factory-level filter test ----------

// factoryTestFilterRequest is the body of POST /factories/{uid}/test-filter: a
// stored delivery of any of the factory's webhooks, and either a trigger filter
// (JSON object, or a JSON/YAML string as written in definition files) or the
// content of an automation.md whose webhook triggers are tried in order.
type factoryTestFilterRequest struct {
	DeliveryID string          `json:"delivery_id"`
	Filter     json.RawMessage `json:"filter,omitempty"`
	Automation string          `json:"automation,omitempty"`
}

func (s *Server) handleFactoryTestFilter(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.TestFilter == nil {
		return featureUnavailable("payload filter evaluation")
	}
	var req factoryTestFilterRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.DeliveryID == "" {
		return errInvalid("delivery_id is required: choose a stored delivery to test against")
	}
	d, err := s.opts.Store.GetWebhookDelivery(ctx, req.DeliveryID)
	if err != nil {
		if isNotFound(err) {
			return errNotFound("delivery " + req.DeliveryID)
		}
		return err
	}
	wh, err := s.opts.Store.GetWebhook(ctx, d.WebhookID)
	if err != nil || wh.FactoryID != f.ID {
		return errNotFound("delivery " + req.DeliveryID)
	}
	if len(d.Body) == 0 {
		return errInvalid("delivery %s has no stored payload (only accepted deliveries keep their body)", d.ID)
	}
	var filters []json.RawMessage
	switch {
	case len(bytes.TrimSpace(req.Filter)) > 0 && string(bytes.TrimSpace(req.Filter)) != "null":
		flt, err := normalizeFilter(req.Filter)
		if err != nil {
			return err
		}
		filters = append(filters, flt)
	case strings.TrimSpace(req.Automation) != "":
		fs, err := webhookFiltersFromAutomation(req.Automation)
		if err != nil {
			return errInvalid("automation: %v", err)
		}
		filters = fs
	}
	if len(filters) == 0 {
		return errInvalid("no webhook trigger to test: pass filter, or an automation with a webhook trigger")
	}
	var res TestFilterResult
	for _, flt := range filters {
		res, err = s.evaluateFilter(wh, flt, d.Body)
		if err != nil {
			return err
		}
		if res.WouldTrigger {
			break
		}
	}
	res.DeliveryID = d.ID
	writeJSON(w, http.StatusOK, res)
	return nil
}

// normalizeFilter accepts a JSON object, or a JSON string holding JSON or YAML.
func normalizeFilter(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		return raw, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, errInvalid("filter must be an object or a JSON/YAML string")
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err == nil {
		if _, ok := v.(map[string]any); !ok {
			return nil, errInvalid("filter must be a mapping")
		}
		return json.RawMessage(text), nil
	}
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		return nil, errInvalid("filter is neither valid JSON nor YAML")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, errInvalid("filter must be a mapping")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errInvalid("filter could not be converted: %v", err)
	}
	return b, nil
}

// evaluateFilter tests one trigger filter ({"webhook_ids", "payload"}) against a body.
// webhook_ids entries may name the webhook by UID or by name.
func (s *Server) evaluateFilter(wh *core.Webhook, filter, body json.RawMessage) (TestFilterResult, error) {
	var flt struct {
		WebhookIDs json.RawMessage `json:"webhook_ids"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(filter, &flt); err != nil {
		return TestFilterResult{}, errInvalid("filter must be an object: %v", err)
	}
	res := TestFilterResult{}
	if len(flt.WebhookIDs) == 0 || string(flt.WebhookIDs) == "null" {
		res.WebhookCheck = CheckResult{Passed: true, Detail: "no webhook_ids constraint"}
	} else {
		ids, err := parseWebhookIDs(flt.WebhookIDs)
		if err != nil {
			return res, err
		}
		if slices.Contains(ids, wh.ID) || (wh.Name != "" && slices.Contains(ids, wh.Name)) {
			res.WebhookCheck = CheckResult{Passed: true, Detail: "webhook " + wh.ID + " is selected"}
		} else {
			res.WebhookCheck = CheckResult{Passed: false, Detail: "webhook_ids does not include " + wh.ID}
		}
	}
	pattern := bytes.TrimSpace(flt.Payload)
	if len(pattern) == 0 || string(pattern) == "null" {
		res.PayloadCheck = CheckResult{Passed: true, Detail: "no payload filter: every delivery matches"}
	} else {
		ok, err := s.opts.TestFilter(json.RawMessage(pattern), body)
		if err != nil {
			return res, errInvalid("invalid payload filter: %v", err)
		}
		if ok {
			res.PayloadCheck = CheckResult{Passed: true, Detail: "payload matches the filter"}
		} else {
			res.PayloadCheck = CheckResult{Passed: false, Detail: "payload does not match the filter"}
		}
	}
	res.WouldTrigger = res.WebhookCheck.Passed && res.PayloadCheck.Passed && wh.Enabled
	if !wh.Enabled {
		res.WebhookCheck.Detail += " (webhook is disabled)"
	}
	res.Result = "Would not trigger"
	if res.WouldTrigger {
		res.Result = "Would trigger"
	}
	return res, nil
}

// webhookFiltersFromAutomation extracts the filters of webhook triggers from automation.md.
func webhookFiltersFromAutomation(content string) ([]json.RawMessage, error) {
	doc, err := parseMarkdownDoc(content)
	if err != nil {
		return nil, err
	}
	var front struct {
		Triggers []struct {
			Provider string `yaml:"provider"`
			Filter   any    `yaml:"filter"`
		} `yaml:"triggers"`
	}
	if err := doc.front.Decode(&front); err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, t := range front.Triggers {
		if t.Provider != "webhook" {
			continue
		}
		flt := t.Filter
		if flt == nil {
			flt = map[string]any{}
		}
		b, err := json.Marshal(flt)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ---------- scoring & self-improvement ----------

func (s *Server) handleScoreRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	if s.opts.ScoreRun == nil {
		return featureUnavailable("on-demand scoring")
	}
	var req struct {
		ScorerID string `json:"scorer_id"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	scorers, err := s.opts.Store.ListScorers(ctx, run.FactoryID)
	if err != nil {
		return err
	}
	idx := slices.IndexFunc(scorers, func(sc core.Scorer) bool { return sc.ID == req.ScorerID || sc.Slug == req.ScorerID })
	if idx < 0 {
		return errNotFound("scorer " + req.ScorerID)
	}
	sc, err := s.opts.ScoreRun(ctx, scorers[idx].ID, run.ID)
	if err != nil {
		return err
	}
	if sc.ID == "" {
		// The judge is a run: the score is stored when it finishes.
		writeJSON(w, http.StatusAccepted, sc)
		return nil
	}
	writeJSON(w, http.StatusOK, sc)
	return nil
}

func (s *Server) handleSelfImprovement(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.RunSelfImprovement == nil {
		return featureUnavailable("self-improvement")
	}
	// Self-improvement reads failed scores of scorers that have
	// selfImprovement turned on. Say so when no such scorer exists, so the
	// caller does not wait for a run that never starts.
	scorers, err := s.opts.Store.ListScorers(r.Context(), f.ID)
	if err != nil {
		return err
	}
	notStarted := func(reason string) error {
		writeJSON(w, http.StatusOK, map[string]any{"factory_id": f.ID, "started": false, "run_ids": []string{}, "reason": reason})
		return nil
	}
	if len(scorers) == 0 {
		return notStarted("The factory has no scorers. Add a scorer with selfImprovement turned on.")
	}
	if !slices.ContainsFunc(scorers, func(sc core.Scorer) bool { return sc.SelfImprovement }) {
		return notStarted("No scorer of the factory has selfImprovement turned on.")
	}
	runIDs, err := s.opts.RunSelfImprovement(r.Context(), f.ID)
	if err != nil {
		return err
	}
	if len(runIDs) == 0 {
		return notStarted("No failed scores wait for review.")
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"factory_id": f.ID, "started": true, "run_ids": runIDs})
	return nil
}

// ---------- frontmatter documents & yaml.Node helpers ----------

// mdDoc is a Markdown file with YAML frontmatter (agent.md, automation.md).
type mdDoc struct {
	front *yaml.Node // mapping node
	body  string
}

func parseMarkdownDoc(src string) (*mdDoc, error) {
	d := &mdDoc{front: yamlMap()}
	s := strings.ReplaceAll(src, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") && s != "---" {
		d.body = s
		return d, nil
	}
	rest := strings.TrimPrefix(s, "---")
	rest = strings.TrimPrefix(rest, "\n")
	end := -1
	switch {
	case strings.HasPrefix(rest, "---\n") || rest == "---":
		end = 0
	case strings.Contains(rest, "\n---\n"):
		end = strings.Index(rest, "\n---\n") + 1
	case strings.HasSuffix(rest, "\n---"):
		end = len(rest) - 3
	}
	if end < 0 {
		return nil, errors.New("frontmatter is not terminated by a '---' line")
	}
	fm := rest[:end]
	body := strings.TrimPrefix(rest[end:], "---")
	body = strings.TrimPrefix(body, "\n")
	if strings.TrimSpace(fm) != "" {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
			return nil, fmt.Errorf("frontmatter: %w", err)
		}
		if len(doc.Content) == 1 {
			if doc.Content[0].Kind != yaml.MappingNode {
				return nil, errors.New("frontmatter must be a YAML mapping")
			}
			d.front = doc.Content[0]
		}
	}
	d.body = body
	return d, nil
}

func (d *mdDoc) String() (string, error) {
	var buf strings.Builder
	buf.WriteString("---\n")
	if len(d.front.Content) > 0 {
		buf.WriteString(encodeYAMLNode(d.front))
	}
	buf.WriteString("---\n")
	if body := strings.TrimLeft(d.body, "\n"); body != "" {
		buf.WriteString("\n")
		buf.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			buf.WriteString("\n")
		}
	}
	return buf.String(), nil
}

// parseYAMLMapping parses a YAML document that must be a mapping (an empty
// document yields an empty mapping).
func parseYAMLMapping(src string) (*yaml.Node, error) {
	if strings.TrimSpace(src) == "" {
		return yamlMap(), nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("must be a YAML mapping")
	}
	return doc.Content[0], nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, scalarNode(key), value)
}

func mapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func seqNode(vals []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, v := range vals {
		n.Content = append(n.Content, scalarNode(v))
	}
	return n
}

func setOrDelete(m *yaml.Node, key, v string) {
	if strings.TrimSpace(v) == "" {
		mapDelete(m, key)
		return
	}
	mapSet(m, key, scalarNode(strings.TrimSpace(v)))
}

func nodeString(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

func nodeStrings(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}
