package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/harness"
)

// preflightDeps holds the optional dependencies of the preflight. They are
// set after New because the integrations and the sandbox backends are built
// after the engine.
type preflightDeps struct {
	mu       sync.RWMutex
	resolver core.NameResolver
	hosts    []string
}

// SetNameResolver sets the resolver that maps the names in automation
// filters to provider IDs. Without one, the preflight reports name filters
// as warnings and the names are compared on each event.
func (e *Engine) SetNameResolver(r core.NameResolver) {
	e.pre.mu.Lock()
	defer e.pre.mu.Unlock()
	e.pre.resolver = r
}

// SetWorkerHosts sets the names of the sandbox backends that are configured
// on this server. The preflight accepts them as worker hosts, in addition to
// the built-in hosts and the registered workers of the team.
func (e *Engine) SetWorkerHosts(hosts []string) {
	e.pre.mu.Lock()
	defer e.pre.mu.Unlock()
	e.pre.hosts = slices.Clone(hosts)
}

func (e *Engine) preDeps() (core.NameResolver, []string) {
	e.pre.mu.RLock()
	defer e.pre.mu.RUnlock()
	return e.pre.resolver, e.pre.hosts
}

// CheckDefinition runs the compiler and the preflight on a definition tree
// for a factory. It does not change anything. The result is valid when the
// files compile and the preflight found no errors.
func (e *Engine) CheckDefinition(ctx context.Context, factoryID string, files map[string]string) (*core.DefinitionCheck, error) {
	if e.compile == nil {
		return nil, errors.New("engine: no definition compiler configured")
	}
	f, err := e.store.GetFactory(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	chk, _ := e.checkDefinition(ctx, f, files)
	return chk, nil
}

// checkDefinition compiles files and, when they compile, runs the alias
// check and the preflight. It also returns the resolved trigger filters.
func (e *Engine) checkDefinition(ctx context.Context, f *core.Factory, files map[string]string) (*core.DefinitionCheck, resolvedFilters) {
	chk := &core.DefinitionCheck{Errors: []core.ValidationError{}, Warnings: []core.ValidationError{}, Skipped: []core.SkippedCheck{}}
	def, verrs := e.compile(files)
	if def == nil && len(verrs) == 0 {
		verrs = []core.ValidationError{{File: "factory.yaml", Message: "definition could not be compiled"}}
	}
	if len(verrs) > 0 {
		chk.Errors = append(chk.Errors, verrs...)
		return chk, nil
	}
	chk.Definition = def
	chk.Errors = append(chk.Errors, e.checkAlias(ctx, f, def)...)
	resolved := e.preflight(ctx, f, def, files, chk)
	addIssueLines(files, chk.Errors)
	addIssueLines(files, chk.Warnings)
	return chk, resolved
}

// addIssueLines gives a line to each issue that names a file and a key path
// but has no line. The compiler sets the line of its own errors; the
// preflight runs on the compiled definition and finds the line here.
func addIssueLines(files map[string]string, issues []core.ValidationError) {
	for i := range issues {
		is := &issues[i]
		if is.Line != 0 || is.Field == "" {
			continue
		}
		if content, ok := files[is.File]; ok {
			is.Line = definition.KeyLine(is.File, content, is.Field)
		}
	}
}

// resolvedFilters maps an automation name to the filters of its triggers
// after name resolution. A nil entry keeps the compiled filter.
type resolvedFilters map[string][]json.RawMessage

// preflight checks a compiled definition against the team of f. It adds
// errors, warnings and skipped checks to chk.
func (e *Engine) preflight(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, files map[string]string, chk *core.DefinitionCheck) resolvedFilters {
	e.preflightSecrets(ctx, f, def, chk)
	e.preflightEnvironments(ctx, f, def, files, chk)
	e.preflightWorkerHosts(ctx, f, def, chk)
	e.preflightMCPServers(ctx, f, def, chk)
	e.preflightWebhooks(ctx, f, def, chk)
	e.preflightWebhookFilters(ctx, f, def, chk)
	e.preflightRunAs(ctx, f, def, chk)
	e.preflightApprovers(ctx, f, def, chk)
	preflightMachines(def, chk)
	preflightModels(def, chk)
	e.preflightCredentials(ctx, f, def, chk)
	resolved := e.preflightNameFilters(ctx, f, def, chk)
	sortIssues(chk.Errors)
	sortIssues(chk.Warnings)
	return resolved
}

func sortIssues(v []core.ValidationError) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].File != v[j].File {
			return v[i].File < v[j].File
		}
		return v[i].Field < v[j].Field
	})
}

func agentFile(name string) string      { return "agents/" + name + "/agent.md" }
func automationFile(name string) string { return "automations/" + name + "/automation.md" }
func scorerFile(slug string) string     { return "scorers/" + slug + "/scorer.md" }
func machineFile(name string) string    { return "machines/" + name + ".yaml" }
func webhookFileOf(name string) string  { return "webhooks/" + name + ".yaml" }

type defRef struct {
	file, field, name string
}

// ---------- secrets ----------

func secretRefs(def *core.CompiledDefinition) []defRef {
	var refs []defRef
	add := func(file, field, name string) {
		if name != "" {
			refs = append(refs, defRef{file, field, name})
		}
	}
	auth := func(file, field string, a *core.HarnessAuth) {
		if a != nil && a.Source == "managedSecret" {
			add(file, field+".auth.secretName", a.SecretName)
		}
	}
	for _, s := range def.Secrets {
		add("factory.yaml", "secrets", s)
	}
	machineNames := make([]string, 0, len(def.Machines))
	for n := range def.Machines {
		machineNames = append(machineNames, n)
	}
	sort.Strings(machineNames)
	for _, a := range def.Agents {
		for _, s := range a.Secrets {
			add(agentFile(a.Name), "secrets", s)
		}
		auth(agentFile(a.Name), "harness", a.Harness.Auth)
	}
	for _, au := range def.Automations {
		if au.Overrides == nil {
			continue
		}
		for _, s := range au.Overrides.Secrets {
			add(automationFile(au.Name), "secrets", s)
		}
		if au.Overrides.Harness != nil {
			auth(automationFile(au.Name), "harness", au.Overrides.Harness.Auth)
		}
	}
	if sd := def.ScorerDefaults; sd != nil {
		for _, s := range sd.Secrets {
			add("factory.yaml", "scorerDefaults.secrets", s)
		}
		if sd.Harness != nil {
			auth("factory.yaml", "scorerDefaults.harness", sd.Harness.Auth)
		}
	}
	for _, sc := range def.Scorers {
		for _, s := range sc.Secrets {
			add(scorerFile(sc.Slug), "secrets", s)
		}
	}
	for _, w := range def.Webhooks {
		add(webhookFileOf(w.Name), "secretName", w.SecretName)
	}
	return refs
}

func (e *Engine) preflightSecrets(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	refs := secretRefs(def)
	if len(refs) == 0 {
		return
	}
	if e.secrets == nil {
		chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "secrets", Reason: "no secret store is configured"})
		return
	}
	known := map[string]bool{}
	for _, scope := range []string{"", f.ID} {
		list, err := e.secrets.List(ctx, f.TeamID, scope)
		if err != nil {
			chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "secrets", Reason: "could not list the secrets: " + err.Error()})
			return
		}
		for _, s := range list {
			if s.FactoryID == "" || s.FactoryID == f.ID {
				known[s.Name] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if known[r.name] || seen[r.name] {
			continue
		}
		seen[r.name] = true
		chk.Errors = append(chk.Errors, core.ValidationError{File: r.file, Field: r.field,
			Message: fmt.Sprintf("secret %q does not exist in the team or the factory; add it under Secrets first", r.name)})
	}
}

// ---------- environments ----------

// envField is the key that sets the environment of an agent or an automation.
const envField = "environmentId"

func (e *Engine) preflightEnvironments(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, files map[string]string, chk *core.DefinitionCheck) {
	var refs []defRef
	// An agent without its own environmentId inherits
	// agentDefaults.environmentId of factory.yaml: report the error there.
	defaultsSet := definition.KeyLine("factory.yaml", files["factory.yaml"], "agentDefaults."+envField) > 0
	for _, a := range def.Agents {
		if a.EnvironmentID == "" {
			continue
		}
		file, field := agentFile(a.Name), envField
		if defaultsSet && definition.KeyLine(file, files[file], envField) == 0 {
			file, field = "factory.yaml", "agentDefaults."+envField
		}
		refs = append(refs, defRef{file, field, a.EnvironmentID})
	}
	for _, au := range def.Automations {
		if au.Overrides != nil && au.Overrides.EnvironmentID != nil && *au.Overrides.EnvironmentID != "" {
			refs = append(refs, defRef{automationFile(au.Name), envField, *au.Overrides.EnvironmentID})
		}
	}
	if len(refs) == 0 {
		return
	}
	list, err := e.store.ListEnvironments(ctx, f.TeamID)
	if err != nil {
		chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "environments", Reason: "could not list the environments: " + err.Error()})
		return
	}
	known := map[string]bool{}
	for _, en := range list {
		known[en.ID] = true
	}
	seen := map[string]bool{}
	for _, r := range refs {
		key := r.file + "\x00" + r.name
		if known[r.name] || seen[key] {
			continue
		}
		seen[key] = true
		chk.Errors = append(chk.Errors, core.ValidationError{File: r.file, Field: r.field,
			Message: fmt.Sprintf("environment %q does not exist in the team", r.name)})
	}
}

// ---------- worker hosts ----------

// isBuiltinHost tells whether a worker host names a built-in backend and not a
// remote worker. The dispatcher uses the same list.
func isBuiltinHost(host string) bool {
	switch host {
	case "", "server", "kubernetes", "direct", "managed":
		return true
	}
	return false
}

func (e *Engine) preflightWorkerHosts(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	_, hosts := e.preDeps()
	var refs []defRef
	add := func(file, host string) {
		if host == "" || isBuiltinHost(host) || slices.Contains(hosts, host) {
			return
		}
		refs = append(refs, defRef{file, "workerHost", host})
	}
	for _, a := range def.Agents {
		add(agentFile(a.Name), a.WorkerHost)
	}
	for _, au := range def.Automations {
		if au.Overrides != nil && au.Overrides.WorkerHost != nil {
			add(automationFile(au.Name), *au.Overrides.WorkerHost)
		}
	}
	if len(refs) == 0 {
		return
	}
	list, err := e.store.ListWorkers(ctx, f.TeamID)
	if err != nil {
		chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "worker hosts", Reason: "could not list the workers: " + err.Error()})
		return
	}
	known := map[string]bool{}
	for _, w := range list {
		known[w.ID], known[w.Name] = true, true
	}
	seen := map[string]bool{}
	for _, r := range refs {
		key := r.file + "\x00" + r.name
		if known[r.name] || seen[key] {
			continue
		}
		seen[key] = true
		chk.Errors = append(chk.Errors, core.ValidationError{File: r.file, Field: r.field,
			Message: fmt.Sprintf("worker host %q is not a registered worker of the team or a sandbox backend of this server", r.name)})
	}
}

// ---------- registered MCP servers ----------

func (e *Engine) preflightMCPServers(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	type mref struct{ file, name, id string }
	var refs []mref
	add := func(file string, m map[string]core.MCPServer) {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			id := m[n].ManagedID
			if id != "" && !strings.HasPrefix(id, core.BuiltinMCPPrefix) {
				refs = append(refs, mref{file, n, id})
			}
		}
	}
	add("factory.yaml", def.MCPServers)
	for _, a := range def.Agents {
		add(agentFile(a.Name), a.MCPServers)
	}
	for _, au := range def.Automations {
		if au.Overrides != nil {
			add(automationFile(au.Name), au.Overrides.MCPServers)
		}
	}
	for _, sc := range def.Scorers {
		add(scorerFile(sc.Slug), sc.MCPServers)
	}
	if def.ScorerDefaults != nil {
		add("factory.yaml", def.ScorerDefaults.MCPServers)
	}
	if len(refs) == 0 {
		return
	}
	list, err := e.store.ListMCPServers(ctx, f.TeamID)
	if err != nil {
		chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "MCP servers", Reason: "could not list the registered MCP servers: " + err.Error()})
		return
	}
	known := map[string]bool{}
	for _, m := range list {
		known[m.ID], known[m.Name] = true, true
	}
	seen := map[string]bool{}
	for _, r := range refs {
		// Factory-wide servers also appear on each agent: report one time.
		key := r.name + "/" + r.id
		if known[r.id] || seen[key] {
			continue
		}
		seen[key] = true
		chk.Errors = append(chk.Errors, core.ValidationError{File: r.file, Field: "mcpServers." + r.name + ".managedId",
			Message: fmt.Sprintf("no MCP server %q is registered for this team (register it under MCP servers first)", r.id)})
	}
}

// ---------- webhooks ----------

func (e *Engine) preflightWebhooks(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	if len(def.Webhooks) == 0 {
		return
	}
	old, err := e.store.ListWebhooks(ctx, f.ID)
	if err != nil {
		chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "webhooks", Reason: "could not list the webhooks: " + err.Error()})
		return
	}
	byName := make(map[string]core.Webhook, len(old))
	for _, w := range old {
		byName[w.Name] = w
	}
	for _, w := range def.Webhooks {
		file := webhookFileOf(w.Name)
		if prev, ok := byName[w.Name]; ok {
			if prev.AuthMode != w.AuthMode {
				chk.Errors = append(chk.Errors, core.ValidationError{File: file, Field: "authMode",
					Message: fmt.Sprintf("authMode cannot change after the first apply (%s to %s); remove the webhook, apply, and add a new one", prev.AuthMode, w.AuthMode)})
			}
			if prev.SignatureScheme != w.SignatureScheme {
				chk.Errors = append(chk.Errors, core.ValidationError{File: file, Field: "signatureScheme",
					Message: fmt.Sprintf("signatureScheme cannot change after the first apply (%q to %q); remove the webhook, apply, and add a new one", prev.SignatureScheme, w.SignatureScheme)})
			}
		}
		if w.AuthMode != "url_token" || w.SecretName == "" || e.secrets == nil {
			continue
		}
		v, err := e.secrets.Get(ctx, f.TeamID, f.ID, w.SecretName)
		if err != nil {
			continue // a missing secret is reported by the secret check
		}
		if !definition.ValidURLTokenSecret(string(v)) {
			chk.Errors = append(chk.Errors, core.ValidationError{File: file, Field: "secretName",
				Message: fmt.Sprintf("the value of secret %q cannot be used in a URL; a url_token secret accepts only letters, digits, '-', '.', '_' and '~'", w.SecretName)})
		}
	}
}

// preflightWebhookFilters checks the webhook UIDs in the webhook_ids filters
// of webhook triggers. The compiler checks webhook names against the
// webhooks/<name>.yaml files and leaves the entries that look like a UID to
// this check. A UID is valid when it belongs to a stored webhook whose file is
// still in the definition: the apply keeps the UID of a webhook while its
// name stays, and a webhook file that was removed gets a new UID.
func (e *Engine) preflightWebhookFilters(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	type ref struct{ file, field, value string }
	names := make(map[string]bool, len(def.Webhooks))
	for _, w := range def.Webhooks {
		names[w.Name] = true
	}
	var refs []ref
	for _, au := range def.Automations {
		for i, t := range au.Triggers {
			if t.Provider != "webhook" || len(t.Filter) == 0 {
				continue
			}
			field, ids := webhookFilterIDs(t.Filter)
			for j, id := range ids {
				if !names[id] && strings.HasPrefix(id, definition.WebhookUIDPrefix) {
					refs = append(refs, ref{automationFile(au.Name), fmt.Sprintf("triggers[%d].filter.%s[%d]", i, field, j), id})
				}
			}
		}
	}
	if len(refs) == 0 {
		return
	}
	stored, err := e.store.ListWebhooks(ctx, f.ID)
	if err != nil {
		chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "webhook filters", Reason: "could not list the webhooks: " + err.Error()})
		return
	}
	known := map[string]bool{}
	for _, w := range stored {
		if names[w.Name] {
			known[w.ID] = true
		}
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	hint := strings.Join(list, ", ")
	if hint == "" {
		hint = "none; add webhooks/<name>.yaml"
	}
	for _, r := range refs {
		if !known[r.value] {
			chk.Errors = append(chk.Errors, core.ValidationError{File: r.file, Field: r.field,
				Message: fmt.Sprintf("webhook %q does not exist; known: %s", r.value, hint)})
		}
	}
}

// webhookFilterIDs returns the entries of the webhook_ids key of a compiled
// webhook filter and the key path of the list ("webhook_ids" or
// "webhook_ids.in").
func webhookFilterIDs(filter json.RawMessage) (string, []string) {
	var flt struct {
		WebhookIDs json.RawMessage `json:"webhook_ids"`
	}
	if json.Unmarshal(filter, &flt) != nil || len(flt.WebhookIDs) == 0 {
		return "", nil
	}
	var ids []string
	if json.Unmarshal(flt.WebhookIDs, &ids) == nil {
		return "webhook_ids", ids
	}
	var in struct {
		In []string `json:"in"`
	}
	if json.Unmarshal(flt.WebhookIDs, &in) == nil {
		return "webhook_ids.in", in.In
	}
	return "", nil
}

// ---------- approvers ----------

// preflightApprovers checks that each entry of approvers names a person of
// the team (by email) or a service account of the team (by name).
func (e *Engine) preflightApprovers(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	if len(def.Approvers) == 0 {
		return
	}
	users, err := e.store.ListUsers(ctx, f.TeamID)
	if err != nil {
		chk.Skipped = append(chk.Skipped, core.SkippedCheck{Check: "approvers", Reason: "could not list the users: " + err.Error()})
		return
	}
	for _, a := range def.Approvers {
		found := false
		for i := range users {
			if IsApprover([]string{a}, &users[i]) {
				found = true
				break
			}
		}
		if !found {
			chk.Errors = append(chk.Errors, core.ValidationError{File: "factory.yaml", Field: "approvers",
				Message: fmt.Sprintf("approver %q is not a user email or a service account name of the team", a)})
		}
	}
}

// ---------- credentials ----------

// preflightCredentials adds a warning for each agent that has no harness
// credential. The harness CLI runs in the sandbox and calls the model
// provider itself, so a run needs a credential. An agent without
// harness.auth uses the default credential: the one factory or team secret
// whose type the harness accepts (findDefaultCredential). The check warns
// when there is no such secret, or more than one at the same scope. It is a
// warning, because a sandbox can hold a credential of its own (a signed-in
// CLI on a workstation in development mode).
func (e *Engine) preflightCredentials(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	found := map[core.HarnessType]HarnessCredential{}
	for _, a := range def.Agents {
		if a.Harness.Auth != nil {
			continue
		}
		h := harness.NormalizeType(string(a.Harness.Type))
		cred, ok := found[h]
		if !ok {
			var err error
			if cred, err = findDefaultCredential(ctx, e.secrets, f.TeamID, f.ID, h); err != nil {
				e.log.WarnContext(ctx, "preflight: list secrets for the default model credential", "factory", f.ID, "err", err)
			}
			found[h] = cred
		}
		var msg string
		switch {
		case cred.IsDefault():
			continue
		case cred.Reason == CredentialReasonAmbiguous:
			msg = fmt.Sprintf("the %s harness of agent %q has no harness.auth, and more than one %s secret fits (%s), so runs get no credential; set harness.auth (for example in agentDefaults) to one of them",
				h, a.Name, cred.Scope, strings.Join(cred.Candidates, ", "))
		case len(harness.CredentialSecretTypes(h)) == 0:
			msg = fmt.Sprintf("the %s harness of agent %q has no credential; set harness.auth (for example in agentDefaults) to a managed secret with the model key before the first run", h, a.Name)
		default:
			msg = fmt.Sprintf("the %s harness of agent %q has no credential; add a secret of type %s under Secrets, or set harness.auth (for example in agentDefaults) to a managed secret with the model key, before the first run",
				h, a.Name, credentialTypeList(h))
		}
		chk.Warnings = append(chk.Warnings, core.ValidationError{File: agentFile(a.Name), Field: "harness.auth", Message: msg})
	}
}

// credentialTypeList names the credential secret types of a harness for a
// message, for example "anthropic_api_key or anthropic_oauth_token".
func credentialTypeList(h core.HarnessType) string {
	types := harness.CredentialSecretTypes(h)
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// ---------- models ----------

// preflightModels adds a warning for a model that is not in the model list of
// its harness. The lists are advisory: a harness passes other names to its
// runtime, so this is a warning and not an error.
func preflightModels(def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	seen := map[string]bool{}
	check := func(file, field string, t core.HarnessType, model string) {
		if model == "" || knownModel(t, model) {
			return
		}
		key := file + "\x00" + field + "\x00" + model
		if seen[key] {
			return
		}
		seen[key] = true
		chk.Warnings = append(chk.Warnings, core.ValidationError{File: file, Field: field,
			Message: fmt.Sprintf("model %q is not in the model list of the %s harness; check the spelling", model, harness.NormalizeType(string(t)))})
	}
	for _, a := range def.Agents {
		check(agentFile(a.Name), "harness.model", a.Harness.Type, a.Harness.Model)
	}
	for _, au := range def.Automations {
		o := au.Overrides
		if o == nil {
			continue
		}
		if o.Harness != nil {
			check(automationFile(au.Name), "harness.model", o.Harness.Type, o.Harness.Model)
		}
		if o.Model != "" {
			for _, a := range def.Agents {
				if a.Name == au.Agent {
					check(automationFile(au.Name), "model", a.Harness.Type, o.Model)
				}
			}
		}
	}
	for _, sc := range def.Scorers {
		if sc.Harness != nil {
			check(scorerFile(sc.Slug), "harness.model", sc.Harness.Type, sc.Harness.Model)
		}
	}
}

var datedModel = regexp.MustCompile(`-\d{8}$`)

// knownModel reports whether model is in the model list of harness type t. A
// harness without a list accepts any name. A date suffix and the [1m] suffix
// are ignored.
func knownModel(t core.HarnessType, model string) bool {
	list := harness.Models(t)
	if list == nil {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSuffix(m, "[1m]")
	for _, c := range []string{m, datedModel.ReplaceAllString(m, "")} {
		for _, known := range list {
			if strings.ToLower(known.ID) == c {
				return true
			}
		}
	}
	return false
}

// ---------- name filters ----------

// nameAlias is a filter key that holds names, and the native key that holds
// the provider IDs of those names.
type nameAlias struct{ alias, native string }

// nameAliases lists the name filters of each provider. The order matters:
// a key is resolved after the keys that give it scope (teams before states
// and issues).
var nameAliases = map[string][]nameAlias{
	"linear": {{"teams", "team_ids"}, {"projects", "project_ids"}, {"states", "state_ids"}, {"issues", "issue_ids"}},
	"slack":  {{"channels", "channel_ids"}, {"users", "user_ids"}, {"itemUsers", "item_user_ids"}},
	"jira":   {{"projects", "project_keys"}, {"statuses", "status_ids"}},
	// Teams triggers name the Graph team in team_ids. Channel and user names
	// are looked up in that team.
	"microsoft_teams": {{"channels", "channel_ids"}, {"users", "user_ids"}},
}

// scopeKeys are native filter keys that limit the lookup of other names (the
// team of a project, a state, an issue or a channel). A trigger can write
// them as IDs. The preflight uses them as scope even when they are not an
// alias that it resolved.
var scopeKeys = []string{"team_ids"}

func (e *Engine) preflightNameFilters(ctx context.Context, f *core.Factory, def *core.CompiledDefinition, chk *core.DefinitionCheck) resolvedFilters {
	resolver, _ := e.preDeps()
	var connected map[string]bool
	out := resolvedFilters{}
	for _, au := range def.Automations {
		file := automationFile(au.Name)
		var filters []json.RawMessage
		changed := false
		for ti, tr := range au.Triggers {
			filters = append(filters, nil)
			aliases := nameAliases[tr.Provider]
			if len(aliases) == 0 || len(tr.Filter) == 0 {
				continue
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(tr.Filter, &m) != nil {
				continue
			}
			scope := map[string][]string{}
			for _, k := range scopeKeys {
				if raw, ok := m[k]; ok {
					if ids := scopeIDs(raw); len(ids) > 0 {
						scope[k] = ids
					}
				}
			}
			rewrote := false
			for _, al := range aliases {
				raw, ok := m[al.alias]
				if !ok {
					continue
				}
				names, ok := filterNames(raw)
				if !ok || len(names) == 0 {
					continue
				}
				field := fmt.Sprintf("triggers[%d].filter.%s", ti, al.alias)
				if connected == nil {
					connected = e.connectedProviders(ctx, f.TeamID)
				}
				var reason string
				switch {
				case !connected[tr.Provider]:
					reason = fmt.Sprintf("the %s integration is not connected", tr.Provider)
				case resolver == nil:
					reason = fmt.Sprintf("this server cannot list %s objects", tr.Provider)
				}
				if reason != "" {
					chk.Warnings = append(chk.Warnings, core.ValidationError{File: file, Field: field,
						Message: "names are not checked now, " + reason + "; they are compared with the current names on each event"})
					continue
				}
				ids, err := resolver.ResolveNames(ctx, f.TeamID, tr.Provider, al.alias, names, scope)
				if err != nil {
					chk.Warnings = append(chk.Warnings, core.ValidationError{File: file, Field: field,
						Message: "names are not checked now: " + err.Error() + "; they are compared with the current names on each event"})
					continue
				}
				var resolved []string
				good := true
				for _, n := range names {
					switch got := ids[n]; len(got) {
					case 0:
						good = false
						chk.Errors = append(chk.Errors, core.ValidationError{File: file, Field: field,
							Message: fmt.Sprintf("no %s %s is named %q", tr.Provider, singular(al.alias), n)})
					case 1:
						resolved = append(resolved, got[0])
					default:
						good = false
						chk.Errors = append(chk.Errors, core.ValidationError{File: file, Field: field,
							Message: fmt.Sprintf("%d %s %s are named %q; use the ID in %s", len(got), tr.Provider, al.alias, n, al.native)})
					}
				}
				if !good {
					continue
				}
				scope[al.native] = resolved
				if _, taken := m[al.native]; taken {
					continue // both keys set: keep the names as written
				}
				b, _ := json.Marshal(resolved)
				m[al.native] = b
				delete(m, al.alias)
				rewrote = true
			}
			if rewrote {
				b, err := json.Marshal(m)
				if err == nil {
					filters[ti] = b
					changed = true
				}
			}
		}
		if changed {
			out[au.Name] = filters
		}
	}
	return out
}

// scopeIDs reads the IDs of a native filter key: a plain list, or the "in"
// list of a matcher object.
func scopeIDs(raw json.RawMessage) []string {
	if ids, ok := filterNames(raw); ok {
		return ids
	}
	var op struct {
		In json.RawMessage `json:"in"`
	}
	if json.Unmarshal(raw, &op) == nil && len(op.In) > 0 {
		if ids, ok := filterNames(op.In); ok {
			return ids
		}
	}
	return nil
}

func singular(alias string) string {
	switch alias {
	case "statuses":
		return "status"
	case "itemUsers":
		return "user"
	case "issues":
		return "issue"
	}
	return strings.TrimSuffix(alias, "s")
}

// filterNames reads a plain list of names (or issue numbers) of a filter.
func filterNames(raw json.RawMessage) ([]string, bool) {
	var vals []any
	if json.Unmarshal(raw, &vals) != nil {
		return nil, false
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case float64:
			out = append(out, strconv.FormatInt(int64(x), 10))
		default:
			return nil, false
		}
	}
	return out, true
}

func (e *Engine) connectedProviders(ctx context.Context, teamID string) map[string]bool {
	out := map[string]bool{}
	list, err := e.store.ListIntegrations(ctx, teamID)
	if err != nil {
		return out
	}
	for _, in := range list {
		out[strings.ToLower(in.Provider)] = true
	}
	return out
}

// applyResolvedFilters replaces the trigger filters of automations with the
// filters that the preflight resolved to provider IDs.
func applyResolvedFilters(automations []core.Automation, resolved resolvedFilters) {
	for i := range automations {
		fs, ok := resolved[automations[i].Name]
		if !ok {
			continue
		}
		trs := slices.Clone(automations[i].Triggers)
		for ti := range trs {
			if ti < len(fs) && fs[ti] != nil {
				trs[ti].Filter = fs[ti]
			}
		}
		automations[i].Triggers = trs
	}
}

// ---------- machines ----------

// preflightMachines warns about machine settings that have no effect.
//
//   - registryCredentialSecretName: the kubernetes and local backends ignore it,
//     and the platform does not look the secret up for them. The check of the
//     secret name is therefore off, and a missing secret is not an error.
//   - A macOS or Windows machine: only Linux sandboxes exist. The run uses a
//     Linux sandbox, so a command that needs Xcode or PowerShell fails.
func preflightMachines(def *core.CompiledDefinition, chk *core.DefinitionCheck) {
	check := func(file, field string, r core.MachineSpec) {
		if name := r.RegistryCredentialSecretName; name != "" {
			chk.Warnings = append(chk.Warnings, core.ValidationError{File: file, Field: field + ".registryCredentialSecretName",
				Message: fmt.Sprintf("registryCredentialSecretName %q has no effect on the kubernetes backend: the kubelet pulls images with the imagePullSecrets of the pod template", name)})
		}
		switch os := strings.ToLower(strings.TrimSpace(r.OS)); os {
		case "macos", "windows":
			chk.Warnings = append(chk.Warnings, core.ValidationError{File: file, Field: field + ".os",
				Message: fmt.Sprintf("the machine asks for %s, but only Linux sandboxes exist: the run uses a Linux sandbox, and a command that needs %s fails", os, osTool(os))})
		}
	}
	names := make([]string, 0, len(def.Machines))
	for n := range def.Machines {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		check(machineFile(n), "platform", def.Machines[n])
	}
	for _, a := range def.Agents {
		// A named machine has its own file and is checked there.
		if a.Machine != nil && !hasMachine(def, a.Machine.Name) {
			check(agentFile(a.Name), "machine", *a.Machine)
		}
	}
}

func hasMachine(def *core.CompiledDefinition, name string) bool {
	_, ok := def.Machines[name]
	return name != "" && ok
}

func osTool(os string) string {
	if os == "macos" {
		return "macOS tools such as Xcode"
	}
	return "Windows tools such as PowerShell"
}
