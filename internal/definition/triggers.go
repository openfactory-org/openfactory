package definition

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

// valueKind describes how a filter key's values are validated.
type valueKind int

const (
	vString       valueKind = iota // strings; list or {in, not_in}
	vUUID                          // UUID strings; list or {in, not_in}
	vInt                           // integers; list or {in, not_in}
	vEnum                          // strings from a fixed set; list or {in, not_in}
	vSkipBots                      // scalar: all | self | none
	vInOnlyOne                     // exactly one value; list or {in}
	vInOnlyMany                    // at least one value; list or {in}
	vWebhookIDs                    // exactly one webhook UID or name; list or {in}
	vPayload                       // JSON payload pattern
	vNameAlias                     // plain list of names (resolved at apply time)
	vNameAliasInt                  // plain list of positive integers (Linear issue numbers)
)

type keySpec struct {
	kind   valueKind
	values []string // for vEnum
}

type aliasSpec struct {
	native string
	rename bool // true: pure rename (rewritten to native key, supports matchers); false: name lookup alias
	kind   valueKind
}

type providerSpec struct {
	events      map[string][]string
	keys        map[string]keySpec // non-default key kinds
	aliases     map[string]aliasSpec
	managed     map[string]bool   // server-managed keys (rejected)
	unavailable map[string]string // events in the catalog that cannot be authored
	requireKeys []string          // keys required whenever a filter is required
	filter      filterRule
}

type filterRule int

const (
	filterOptional filterRule = iota
	filterRequired
	filterForbidden
)

func events(m map[string][]string, names []string, keys ...string) {
	for _, n := range names {
		m[n] = keys
	}
}

// Canonical provider names. Microsoft Teams is authored as "teams" (accepted
// alias "microsoft_teams") and stored as core's "microsoft_teams".
const (
	ProviderGitHub      = "github"
	ProviderGitLab      = "gitlab"
	ProviderAzureDevOps = "azure_devops"
	ProviderSlack       = "slack"
	ProviderTeams       = "microsoft_teams"
	ProviderLinear      = "linear"
	ProviderJira        = "jira"
	ProviderSchedule    = "schedule"
	ProviderWebhook     = "webhook"
)

var triggerCatalog = buildTriggerCatalog()

func buildTriggerCatalog() map[string]*providerSpec {
	cat := map[string]*providerSpec{}

	gh := map[string][]string{}
	events(gh, []string{"issue_created"}, "repos", "labels", "assignees", "authors", "keywords", "skip_bots")
	events(gh, []string{"issue_labeled", "issue_assigned", "pull_request_assigned"}, "repos", "labels", "assignees", "authors", "skip_bots")
	events(gh, []string{"issue_commented", "pull_request_commented"}, "repos", "labels", "authors", "keywords", "skip_bots")
	events(gh, []string{"issue_mentioned", "pull_request_mentioned"}, "repos", "mentioned", "labels", "authors", "keywords", "skip_bots")
	events(gh, []string{"pull_request_opened", "pull_request_ready", "pull_request_reopened"}, "repos", "base_branches", "pr_numbers", "paths", "assignees", "authors", "labels", "keywords", "skip_bots")
	events(gh, []string{"pull_request_closed", "pull_request_dequeued", "pull_request_enqueued", "pull_request_labeled", "pull_request_merged", "pull_request_synchronized"}, "repos", "base_branches", "pr_numbers", "paths", "assignees", "authors", "labels", "skip_bots")
	events(gh, []string{"pull_request_review_requested"}, "repos", "reviewers", "reviewer_teams", "authors", "skip_bots")
	events(gh, []string{"pull_request_review_submitted"}, "repos", "mentioned", "labels", "review_states", "authors", "keywords", "skip_bots")
	events(gh, []string{"push"}, "repos", "branches", "paths", "skip_bots")
	events(gh, []string{"check_suite_completed"}, "repos", "conclusions", "branches", "labels", "authors", "skip_bots")
	events(gh, []string{"workflow_run_completed"}, "repos", "conclusions", "branches", "workflows", "labels", "authors", "skip_bots")
	events(gh, []string{"commit_status"}, "repos", "states", "contexts", "skip_bots")
	events(gh, []string{"check_run_rerequested", "check_suite_rerequested"}, "repos", "skip_bots")
	cat[ProviderGitHub] = &providerSpec{
		events: gh,
		keys: map[string]keySpec{
			"skip_bots":     {kind: vSkipBots},
			"pr_numbers":    {kind: vInt},
			"review_states": {kind: vEnum, values: []string{"approved", "changes_requested", "commented"}},
		},
		aliases: map[string]aliasSpec{
			"baseBranches": {native: "base_branches", rename: true},
			"prNumbers":    {native: "pr_numbers", rename: true},
		},
	}

	gl := map[string][]string{}
	events(gl, []string{"bot_mentioned"}, "repos")
	events(gl, []string{"merge_request"}, "repos", "actions", "base_branches")
	events(gl, []string{"push"}, "repos", "branches")
	cat[ProviderGitLab] = &providerSpec{events: gl, managed: map[string]bool{"mentioned": true}}

	ado := map[string][]string{}
	events(ado, []string{"pull_request_created", "pull_request_updated", "pull_request_commented", "pull_request_closed", "pull_request_merged"}, "repos", "base_branches")
	events(ado, []string{"pull_request_mentioned"}, "repos", "mentioned")
	events(ado, []string{"work_item_created", "work_item_assigned", "work_item_labeled"}, "work_item_types", "labels", "assignees", "authors")
	events(ado, []string{"work_item_mentioned"}, "mentioned")
	cat[ProviderAzureDevOps] = &providerSpec{
		events:      ado,
		keys:        map[string]keySpec{"repos": {kind: vUUID}, "mentioned": {kind: vUUID}},
		managed:     map[string]bool{"source_repos": true},
		unavailable: map[string]string{"push": "azure_devops push events are not available for factory automations"},
	}

	lin := map[string][]string{}
	issueKeys := []string{"team_ids", "project_ids", "labels", "state_ids", "assignee_ids", "mentioned_user_ids", "creator_ids"}
	events(lin, []string{"issue_labeled", "issue_created", "issue_assigned", "issue_state_changed"}, issueKeys...)
	events(lin, []string{"comment_created"}, "team_ids", "project_ids", "labels", "state_ids", "issue_ids", "mentioned_user_ids", "creator_ids")
	events(lin, []string{"agent_session_created"}, "team_ids", "project_ids", "labels", "creator_ids", "keywords")
	cat[ProviderLinear] = &providerSpec{
		events: lin,
		keys: map[string]keySpec{
			"team_ids": {kind: vUUID}, "project_ids": {kind: vUUID}, "state_ids": {kind: vUUID},
			"issue_ids": {kind: vUUID}, "assignee_ids": {kind: vUUID}, "creator_ids": {kind: vUUID},
		},
		aliases: map[string]aliasSpec{
			"teams":    {native: "team_ids", kind: vNameAlias},
			"projects": {native: "project_ids", kind: vNameAlias},
			"states":   {native: "state_ids", kind: vNameAlias},
			"issues":   {native: "issue_ids", kind: vNameAliasInt},
		},
	}

	sl := map[string][]string{}
	events(sl, []string{"app_mention", "message_dm", "message_im", "message_mpim", "message_posted"}, "channel_ids", "user_ids", "keywords")
	events(sl, []string{"reaction_added"}, "channel_ids", "user_ids", "keywords", "emojis", "item_user_ids")
	events(sl, []string{"member_joined_channel"}, "channel_ids", "user_ids")
	cat[ProviderSlack] = &providerSpec{
		events: sl,
		aliases: map[string]aliasSpec{
			"channels":  {native: "channel_ids", kind: vNameAlias},
			"users":     {native: "user_ids", kind: vNameAlias},
			"itemUsers": {native: "item_user_ids", kind: vNameAlias},
		},
	}

	jr := map[string][]string{}
	events(jr, []string{"issue_created", "issue_labeled"}, "project_keys", "labels")
	events(jr, []string{"status_changed"}, "project_keys", "status_ids")
	events(jr, []string{"agent_session_created"}, "project_keys", "labels", "keywords")
	cat[ProviderJira] = &providerSpec{
		events: jr,
		aliases: map[string]aliasSpec{
			"projects": {native: "project_keys", kind: vNameAlias},
			"statuses": {native: "status_ids", kind: vNameAlias},
		},
	}

	tm := map[string][]string{}
	events(tm, []string{"app_mention", "message_posted"}, "team_ids", "channel_ids", "user_ids", "keywords")
	cat[ProviderTeams] = &providerSpec{
		events: tm,
		keys:   map[string]keySpec{"team_ids": {kind: vInOnlyOne}, "channel_ids": {kind: vInOnlyMany}, "user_ids": {kind: vUUID}},
		aliases: map[string]aliasSpec{
			"channels": {native: "channel_ids", kind: vNameAlias},
			"users":    {native: "user_ids", kind: vNameAlias},
		},
		filter:      filterRequired,
		requireKeys: []string{"team_ids", "channel_ids"},
	}

	cat[ProviderSchedule] = &providerSpec{
		events:  map[string][]string{"cron_fired": {}},
		managed: map[string]bool{"schedule_ids": true},
		filter:  filterForbidden,
	}
	cat[ProviderWebhook] = &providerSpec{
		events:      map[string][]string{"received": {"webhook_ids", "payload"}},
		keys:        map[string]keySpec{"webhook_ids": {kind: vWebhookIDs}, "payload": {kind: vPayload}},
		filter:      filterRequired,
		requireKeys: []string{"webhook_ids"},
	}
	return cat
}

// normalizeProvider maps an authored provider name to its canonical form.
func normalizeProvider(p string) string {
	switch p {
	case "teams", "microsoft_teams":
		return ProviderTeams
	}
	return p
}

// integrationFor returns the integration type a trigger provider requires.
func integrationFor(provider string) string {
	switch provider {
	case ProviderSlack:
		return "slack"
	case ProviderTeams:
		return "microsoft-teams"
	case ProviderLinear:
		return "linear"
	case ProviderJira:
		return "jira"
	}
	return ""
}

// EventSpec describes one authorable provider event and its filter keys.
type EventSpec struct {
	Event        string            `json:"event"`
	FilterFields []string          `json:"filter_fields"`
	Aliases      map[string]string `json:"aliases,omitempty"` // authoring alias -> native key
	FilterRule   string            `json:"filter"`            // optional | required | forbidden
}

// TriggerEvents returns the authorable events of a provider ("teams" is accepted
// for Microsoft Teams). It returns nil for an unknown provider.
func TriggerEvents(provider string) []EventSpec {
	ps := triggerCatalog[normalizeProvider(provider)]
	if ps == nil {
		return nil
	}
	var out []EventSpec
	for ev, keys := range ps.events {
		es := EventSpec{Event: ev, FilterFields: append([]string{}, keys...), FilterRule: [...]string{"optional", "required", "forbidden"}[ps.filter]}
		for a, s := range ps.aliases {
			if contains(keys, s.native) {
				if es.Aliases == nil {
					es.Aliases = map[string]string{}
				}
				es.Aliases[a] = s.native
			}
		}
		out = append(out, es)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Event < out[j].Event })
	return out
}

// TriggerProviders returns every canonical trigger provider name.
func TriggerProviders() []string {
	out := make([]string, 0, len(triggerCatalog))
	for p := range triggerCatalog {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// triggerEnv is the definition context a trigger is validated against.
type triggerEnv struct {
	integrationsSet bool
	integrations    map[string]bool
	// webhooks holds the names of the webhooks/<name>.yaml files. A
	// webhook_ids entry must name one of them, or be a webhook UID, which
	// the preflight checks against the stored webhooks.
	webhooks map[string]bool
}

// WebhookUIDPrefix is the prefix of the UIDs that the server gives to
// webhooks. A webhook_ids entry with this prefix that does not name a
// webhook file is a UID; the preflight checks it.
const WebhookUIDPrefix = "whk_"

// knownWebhooks returns the webhook names of env for an error message.
func (env triggerEnv) knownWebhooks() string {
	if len(env.webhooks) == 0 {
		return "none; add webhooks/<name>.yaml"
	}
	names := make([]string, 0, len(env.webhooks))
	for n := range env.webhooks {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func parseTrigger(c *doc, n *yaml.Node, path string, env triggerEnv) (core.Trigger, bool) {
	var t core.Trigger
	m := c.mapping(n, path)
	if m == nil {
		return t, false
	}
	defer m.finish()
	ok := true
	pv, ev := m.require("provider"), m.require("event")
	fv, sv, cv := m.get("filter"), m.get("schedule"), m.get("concurrency")
	if pv == nil || ev == nil {
		return t, false
	}
	provider, good := c.nonEmptyStr(pv, path+".provider")
	event, good2 := c.nonEmptyStr(ev, path+".event")
	if !good || !good2 {
		return t, false
	}
	t.Provider = normalizeProvider(provider)
	t.Event = event
	ps := triggerCatalog[t.Provider]
	if ps == nil {
		c.errf(pv, path+".provider", "unknown trigger provider %q (expected one of github, gitlab, azure_devops, slack, teams, linear, jira, schedule, webhook)", provider)
		return t, false
	}
	if why, bad := ps.unavailable[event]; bad {
		c.errf(ev, path+".event", "%s", why)
		return t, false
	}
	keys, known := ps.events[event]
	if !known {
		var names []string
		for e := range ps.events {
			names = append(names, e)
		}
		sort.Strings(names)
		c.errf(ev, path+".event", "unknown %s event %q (expected one of %s)", provider, event, strings.Join(names, ", "))
		return t, false
	}
	if integ := integrationFor(t.Provider); integ != "" && env.integrationsSet && !env.integrations[integ] {
		c.errf(pv, path+".provider", "%s triggers require the %s integration; add it to integrations in factory.yaml", provider, integ)
		ok = false
	}

	// schedule
	if t.Provider == ProviderSchedule {
		if sv == nil || isNull(sv) {
			c.errf(m.node, path+".schedule", "%s.schedule is required for schedule/cron_fired triggers", path)
			ok = false
		} else if s, good := parseSchedule(c, sv, path+".schedule"); good {
			t.Schedule = s
		} else {
			ok = false
		}
	} else if sv != nil && !isNull(sv) {
		c.errf(sv, path+".schedule", "%s.schedule is only valid for schedule/cron_fired triggers", path)
		ok = false
	}
	if cv != nil && !isNull(cv) {
		switch v, good := c.nonEmptyStr(cv, path+".concurrency"); {
		case !good:
			ok = false
		case t.Provider != ProviderSchedule:
			c.errf(cv, path+".concurrency", "%s.concurrency is only valid for schedule/cron_fired triggers", path)
			ok = false
		case v != core.ScheduleConcurrencyAllow && v != core.ScheduleConcurrencySkip:
			c.errf(cv, path+".concurrency", "%s.concurrency must be allow or skip, got %q", path, v)
			ok = false
		default:
			t.Concurrency = v
		}
	}

	// filter
	hasFilter := fv != nil && !isNull(fv)
	switch {
	case ps.filter == filterForbidden && fv != nil:
		c.errf(fv, path+".filter", "%s triggers accept no filter", provider)
		return t, false
	case ps.filter == filterRequired && !hasFilter:
		c.errf(m.node, path+".filter", "%s.filter is required for %s/%s triggers", path, provider, event)
		return t, false
	case !hasFilter:
		return t, ok
	}
	filter, good := parseFilter(c, fv, path+".filter", t.Provider, ps, keys, env)
	if !good {
		return t, false
	}
	if len(filter) > 0 {
		raw, err := json.Marshal(filter)
		if err != nil {
			c.errf(fv, path+".filter", "filter cannot be encoded as JSON: %v", err)
			return t, false
		}
		t.Filter = raw
	}
	return t, ok
}

func parseSchedule(c *doc, n *yaml.Node, path string) (*core.Schedule, bool) {
	m := c.mapping(n, path)
	if m == nil {
		return nil, false
	}
	defer m.finish()
	s := &core.Schedule{}
	if v := m.get("name"); v != nil && !isNull(v) {
		name, ok := c.nonEmptyStr(v, path+".name")
		if !ok {
			return nil, false
		}
		s.Name = name
	}
	v := m.require("cron")
	if v == nil {
		return nil, false
	}
	expr, ok := c.nonEmptyStr(v, path+".cron")
	if !ok {
		return nil, false
	}
	if err := ValidateCron(expr); err != nil {
		c.errf(v, path+".cron", "%s %q: %v", path+".cron", expr, err)
		return nil, false
	}
	s.Cron = strings.TrimSpace(expr)
	return s, true
}

// ValidateCron validates a five-field UTC cron expression or a descriptor
// such as @daily or @every 1h.
func ValidateCron(expr string) error {
	e := strings.TrimSpace(expr)
	if strings.HasPrefix(e, "TZ=") || strings.HasPrefix(e, "CRON_TZ=") {
		return fmt.Errorf("schedules are always UTC; time zone prefixes are not supported")
	}
	if !strings.HasPrefix(e, "@") && len(strings.Fields(e)) != 5 {
		return fmt.Errorf("must be a five-field cron expression or a descriptor such as @daily or @every 1h")
	}
	if _, err := cron.ParseStandard(e); err != nil {
		return err
	}
	return nil
}

func parseFilter(c *doc, n *yaml.Node, path, provider string, ps *providerSpec, keys []string, env triggerEnv) (map[string]any, bool) {
	m := c.mapping(n, path)
	if m == nil {
		return nil, false
	}
	ok := true
	out := map[string]any{}
	for _, key := range m.order {
		v := m.get(key)
		f := m.field(key)
		kn := m.keyNode(key)
		if ps.managed[key] {
			c.errf(kn, f, "%s is server-managed; don't set it", f)
			ok = false
			continue
		}
		if a, isAlias := ps.aliases[key]; isAlias && contains(keys, a.native) {
			if m.has(a.native) {
				c.errf(kn, f, "don't combine %s with its native key %s", key, a.native)
				ok = false
				continue
			}
			if a.rename {
				val, good := validateMatcher(c, v, f, keySpecFor(ps, a.native), env)
				ok = ok && good
				if val != nil {
					out[a.native] = val
				}
				continue
			}
			val, good := validateMatcher(c, v, f, keySpec{kind: a.kind}, env)
			ok = ok && good
			if val != nil {
				out[key] = val
			}
			continue
		}
		if !contains(keys, key) {
			c.errf(kn, f, "unknown filter key %q for %s triggers (accepted: %s)", key, provider, strings.Join(acceptedKeys(ps, keys), ", "))
			ok = false
			continue
		}
		val, good := validateMatcher(c, v, f, keySpecFor(ps, key), env)
		ok = ok && good
		if val != nil {
			out[key] = val
		}
	}
	for _, rk := range ps.requireKeys {
		_, present := out[rk]
		for a, s := range ps.aliases {
			if _, has := out[a]; has && s.native == rk {
				present = true // a name alias stands for the key
			}
		}
		if !present && ok {
			c.errf(m.node, path+"."+rk, "%s.%s is required", path, rk)
			ok = false
		}
	}
	return out, ok
}

func acceptedKeys(ps *providerSpec, keys []string) []string {
	out := append([]string{}, keys...)
	for a, s := range ps.aliases {
		if contains(keys, s.native) {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

func keySpecFor(ps *providerSpec, key string) keySpec {
	if ks, ok := ps.keys[key]; ok {
		return ks
	}
	return keySpec{kind: vString}
}

// validateMatcher validates one filter value and returns its JSON form
// (nil for an omitted/empty matcher, which adds no restriction).
func validateMatcher(c *doc, n *yaml.Node, path string, ks keySpec, env triggerEnv) (any, bool) {
	n = resolve(n)
	if isNull(n) {
		return nil, true
	}
	switch ks.kind {
	case vSkipBots:
		s, ok := c.enum(n, path, "all", "self", "none")
		if !ok {
			return nil, false
		}
		return s, true
	case vPayload:
		leaves := 0
		if !validatePayloadPattern(c, n, path, 1, &leaves) {
			return nil, false
		}
		return toValue(n), true
	}

	plainOnly := ks.kind == vNameAlias || ks.kind == vNameAliasInt
	inOnly := ks.kind == vInOnlyOne || ks.kind == vInOnlyMany || ks.kind == vWebhookIDs
	var in, notIn []any
	var inNode, notInNode *yaml.Node
	switch n.Kind {
	case yaml.SequenceNode:
		inNode = n
	case yaml.MappingNode:
		if plainOnly {
			c.errf(n, path, "%s accepts only a plain list of names", path)
			return nil, false
		}
		m := c.mapping(n, path)
		if m == nil {
			return nil, false
		}
		for _, k := range m.order {
			switch k {
			case "in":
				inNode = m.get(k)
			case "not_in":
				notInNode = m.get(k)
				if inOnly {
					c.errf(m.keyNode(k), path+".not_in", "%s supports only in", path)
					return nil, false
				}
			default:
				m.get(k)
				c.errf(m.keyNode(k), path+"."+k, "unknown matcher operator %q (expected in or not_in)", k)
				return nil, false
			}
		}
	default:
		c.errf(n, path, "%s must be a list of values or a matcher object with in/not_in", path)
		return nil, false
	}
	ok := true
	collect := func(node *yaml.Node, p string) []any {
		if isNull(node) {
			return nil
		}
		items := c.seq(node, p)
		vals := make([]any, 0, len(items))
		for i, it := range items {
			if v, good := validateValue(c, it, indexPath(p, i), ks, env); good {
				vals = append(vals, v)
			} else {
				ok = false
			}
		}
		return vals
	}
	inPath, notInPath := path, path+".not_in"
	if n.Kind == yaml.MappingNode {
		inPath = path + ".in"
	}
	in = collect(inNode, inPath)
	notIn = collect(notInNode, notInPath)
	if !ok {
		return nil, false
	}
	for _, a := range in {
		for _, b := range notIn {
			if fmt.Sprint(a) == fmt.Sprint(b) {
				c.errf(n, path, "%v appears in both in and not_in of %s", a, path)
				return nil, false
			}
		}
	}
	switch ks.kind {
	case vInOnlyOne:
		if len(in) != 1 {
			c.errf(n, path, "%s must include exactly one value", path)
			return nil, false
		}
	case vInOnlyMany:
		if len(in) < 1 {
			c.errf(n, path, "%s must include at least one value", path)
			return nil, false
		}
	case vWebhookIDs:
		if len(in) != 1 {
			c.errf(n, path, "%s must be a one-item list with one webhook; add one trigger per webhook", path)
			return nil, false
		}
	}
	if len(in) == 0 && len(notIn) == 0 {
		return nil, true // empty matcher adds no restriction
	}
	if n.Kind == yaml.SequenceNode {
		return in, true
	}
	out := map[string]any{}
	if len(in) > 0 {
		out["in"] = in
	}
	if len(notIn) > 0 {
		out["not_in"] = notIn
	}
	return out, true
}

func validateValue(c *doc, n *yaml.Node, path string, ks keySpec, env triggerEnv) (any, bool) {
	switch ks.kind {
	case vInt:
		i, ok := c.integer(n, path)
		if ok && i <= 0 {
			c.errf(n, path, "%s must be a positive integer", path)
			return nil, false
		}
		return i, ok
	case vNameAliasInt:
		i, ok := c.integer(n, path)
		if ok && i <= 0 {
			c.errf(n, path, "%s must be a positive issue number such as 42", path)
			return nil, false
		}
		return i, ok
	}
	s, ok := c.nonEmptyStr(n, path)
	if !ok {
		return nil, false
	}
	switch ks.kind {
	case vUUID:
		if !uuidRe.MatchString(s) {
			c.errf(n, path, "%s %q must be a UUID", path, s)
			return nil, false
		}
	case vEnum:
		if !contains(ks.values, s) {
			c.errf(n, path, "%s must be one of %s (got %q)", path, strings.Join(ks.values, ", "), s)
			return nil, false
		}
	case vNameAlias:
		s = strings.TrimSpace(s)
	case vWebhookIDs:
		if strings.HasSuffix(s, ".yaml") || strings.HasSuffix(s, ".yml") || strings.Contains(s, "/") {
			c.errf(n, path, "%s takes a webhook UID or webhook name, not a file name (got %q)", path, s)
			return nil, false
		}
		if !env.webhooks[s] && !strings.HasPrefix(s, WebhookUIDPrefix) {
			c.errf(n, path, "webhook %q does not exist; known: %s", s, env.knownWebhooks())
			return nil, false
		}
	}
	return s, true
}

// Payload pattern limits.
const (
	maxPatternDepth  = 10
	maxPatternLeaves = 64
	maxPatternValues = 32
)

func validatePayloadPattern(c *doc, n *yaml.Node, path string, depth int, leaves *int) bool {
	n = resolve(n)
	if depth > maxPatternDepth {
		c.errf(n, path, "payload pattern nests deeper than %d levels", maxPatternDepth)
		return false
	}
	if n.Kind != yaml.MappingNode {
		c.errf(n, path, "%s must be an object", path)
		return false
	}
	m := c.mapping(n, path)
	if m == nil {
		return false
	}
	ok := true
	for _, k := range m.order {
		v := resolve(m.get(k))
		f := path + "." + k
		switch {
		case v.Kind == yaml.SequenceNode:
			*leaves++
			if !checkPatternValues(c, v, f) {
				ok = false
			}
		case v.Kind == yaml.MappingNode && isOperatorObject(v):
			*leaves++
			if !checkOperatorObject(c, v, f) {
				ok = false
			}
		case v.Kind == yaml.MappingNode:
			if len(v.Content) == 0 {
				c.errf(v, f, "%s must not be an empty object", f)
				ok = false
				continue
			}
			if !validatePayloadPattern(c, v, f, depth+1, leaves) {
				ok = false
			}
		default:
			c.errf(v, f, "%s must be a list of allowed values, an operator object (in, not_in, exists) or a nested pattern", f)
			ok = false
		}
		if *leaves > maxPatternLeaves {
			c.errf(v, f, "payload pattern holds more than %d leaf fields", maxPatternLeaves)
			return false
		}
	}
	return ok
}

func isOperatorObject(n *yaml.Node) bool {
	if len(n.Content) == 0 {
		return false
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch resolve(n.Content[i]).Value {
		case "in", "not_in", "exists":
		default:
			return false
		}
	}
	return true
}

func checkPatternValues(c *doc, n *yaml.Node, path string) bool {
	if len(n.Content) > maxPatternValues {
		c.errf(n, path, "%s lists more than %d values", path, maxPatternValues)
		return false
	}
	for i, it := range n.Content {
		it = resolve(it)
		if it.Kind != yaml.ScalarNode {
			c.errf(it, indexPath(path, i), "payload pattern values must be strings, numbers, booleans or null")
			return false
		}
	}
	return true
}

func checkOperatorObject(c *doc, n *yaml.Node, path string) bool {
	m := c.mapping(n, path)
	if m == nil {
		return false
	}
	var in, notIn []any
	existsFalse := false
	ok := true
	for _, k := range m.order {
		v := m.get(k)
		switch k {
		case "in", "not_in":
			if isNull(v) {
				continue
			}
			rv := resolve(v)
			if rv.Kind != yaml.SequenceNode {
				c.errf(rv, path+"."+k, "%s.%s must be a list", path, k)
				ok = false
				continue
			}
			if !checkPatternValues(c, rv, path+"."+k) {
				ok = false
				continue
			}
			vals := toValue(rv).([]any)
			if k == "in" {
				in = vals
			} else {
				notIn = vals
			}
		case "exists":
			b, good := c.boolean(v, path+".exists")
			if !good {
				ok = false
			}
			existsFalse = good && !b
		}
	}
	for _, a := range in {
		for _, b := range notIn {
			if fmt.Sprint(a) == fmt.Sprint(b) {
				c.errf(n, path, "%v appears in both in and not_in of %s", a, path)
				ok = false
			}
		}
	}
	if existsFalse && len(in) > 0 {
		c.errf(n, path, "%s combines exists: false with a non-empty in", path)
		ok = false
	}
	return ok
}
