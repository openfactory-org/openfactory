package definition

// The v1 JSON Schemas under schemas/v1 are generated from this
// file so that they stay consistent with the compiler (trigger catalog, enum
// values, limits). Regenerate with:
//
//	UPDATE_SCHEMAS=1 go test ./internal/definition -run TestSchemasUpToDate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type obj = map[string]any

const draft = "https://json-schema.org/draft/2020-12/schema"

func ref(name string) obj         { return obj{"$ref": "common.schema.json#/$defs/" + name} }
func local(name string) obj       { return obj{"$ref": "#/$defs/" + name} }
func str() obj                    { return obj{"type": "string"} }
func nonEmpty() obj               { return obj{"type": "string", "minLength": 1} }
func nullable(s obj) obj          { return obj{"anyOf": []any{s, obj{"type": "null"}}} }
func strEnum(vals ...string) obj  { return obj{"type": "string", "enum": vals} }
func arr(items obj) obj           { return obj{"type": "array", "items": items} }
func boolean() obj                { return obj{"type": "boolean"} }
func intRange(lo, hi int) obj     { return obj{"type": "integer", "minimum": lo, "maximum": hi} }
func numRange(lo, hi float64) obj { return obj{"type": "number", "minimum": lo, "maximum": hi} }

func object(required []string, props obj) obj {
	o := obj{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func document(name, title, description string, body obj) obj {
	body["$schema"] = draft
	body["$id"] = SchemaBaseURL + name + ".schema.json"
	body["title"] = title
	body["description"] = description
	return body
}

func withDesc(s obj, d string) obj {
	out := obj{}
	for k, v := range s {
		out[k] = v
	}
	out["description"] = d
	return out
}

// withDefault returns a copy of s with a default annotation. The default is
// what the compiler uses when the key is absent.
func withDefault(s obj, v any) obj {
	out := obj{}
	for k, val := range s {
		out[k] = val
	}
	out["default"] = v
	return out
}

// execProps are the execution settings of an agent or an automation. They
// override the inherited values (agentDefaults, then the agent).
func execProps(automation bool) obj {
	secrets := "Secret names of the runs. They replace the secrets of agentDefaults. [] removes those defaults. The factory-wide secrets always stay."
	mcpServers := "MCP servers of the runs. They replace the mcpServers of agentDefaults. {} removes those defaults. The factory-wide servers always stay."
	if automation {
		secrets = "Secret names that add to the secrets of the agent for the runs of this automation."
		mcpServers = "MCP servers that add to the servers of the agent for the runs of this automation. A server with the same name replaces the server of the agent."
	}
	return obj{
		"model": withDesc(ref("model"), "Model shorthand. A Claude model selects the claude harness. An OpenAI model selects the codex harness. Do not set it together with harness."),
		"harness": withDesc(nullable(ref("harnessOverride")),
			"Sparse override of the inherited harness. It changes only the fields that you set. Set auth to null to clear the inherited credential."),
		"machine":       withDesc(nullable(ref("resourceName")), "Machine of the runs: the name of a file machines/<name>.yaml. It replaces the inherited machine."),
		"environmentId": withDesc(nullable(str()), "ID of the environment of the runs. null clears the inherited value."),
		"secrets":       withDesc(nullable(ref("secretList")), secrets),
		"mcpServers":    withDesc(nullable(ref("mcpServers")), mcpServers),
		"workerHost":    withDesc(nullable(str()), "Worker that runs the agent. The value is trimmed. null or empty clears an inherited host."),
		"idleTimeoutMinutes": withDesc(nullable(intRange(0, 60)),
			"Minutes that a finished run keeps its sandbox and session for follow-ups. 0 ends the session after each turn. Without a value, the server default OF_IDLE_TIMEOUT_DEFAULT applies (10 minutes) to every run. OF_IDLE_TIMEOUT_MAX (60 minutes) is the upper limit."),
	}
}

var notModelAndHarness = obj{"not": obj{
	"required":   []string{"model", "harness"},
	"properties": obj{"model": obj{"type": "string"}, "harness": obj{"type": "object"}},
}}

func commonSchema() obj {
	hostname := obj{"type": "string", "pattern": `^[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?(?:\.[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?)*\.?$`, "maxLength": 254}
	ipv6 := obj{"type": "string", "pattern": `^[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*$`}
	policyRef := obj{"type": "string", "pattern": `^builtin/[a-z0-9-]+@(latest|\^[0-9]+(\.[0-9]+){0,2}|[0-9]+\.[0-9]+\.[0-9]+)$`}
	harnessProps := obj{
		"type":  withDesc(ref("harnessType"), "Harness that runs the agent. claude-code is an alias of claude."),
		"model": withDesc(ref("model"), "Model name. The harness gets it as it is."),
		"reasoningLevel": withDesc(obj{"type": "string", "pattern": `^[a-z][a-z0-9_-]{0,31}$`},
			"Reasoning level that the harness passes to the model."),
		"auth": withDesc(nullable(ref("harnessAuth")), "Model credential of the harness. null clears the inherited credential."),
	}
	full := object([]string{"type", "model"}, harnessProps)
	sparse := object(nil, harnessProps)

	mcp := object(nil, obj{
		"managedId": withDesc(nonEmpty(),
			"Name or ID of a server in the MCP server registry of the team, or a built-in server: builtin/jira, builtin/linear, builtin/slack or builtin/teams. An unknown builtin/ server is an error."),
		"command": withDesc(nonEmpty(), "Executable that starts inside the sandbox. The server uses the stdio transport."),
		"args":    withDesc(arr(str()), "Arguments of command. Valid only with command."),
		"env": withDesc(obj{"type": "object", "additionalProperties": str()},
			"Environment variables of command. Valid only with command. Values can use {{SECRET}} placeholders."),
		"url": withDesc(obj{"type": "string", "pattern": `^https?://[^/\s]+`},
			"Absolute http or https URL with a host. The server uses streamable HTTP, or legacy SSE when the path ends in /sse."),
		"headers": withDesc(obj{"type": "object", "additionalProperties": str()},
			"HTTP headers of url. Valid only with url. Values can use {{SECRET}} placeholders."),
		"allowWrite": withDefault(obj{"type": "boolean", "description": "Enables the write tools of a built-in integration server. Valid only with managedId builtin/<provider>."}, false),
	})
	mcp["oneOf"] = []any{
		obj{"required": []string{"managedId"}},
		obj{"required": []string{"command"}},
		obj{"required": []string{"url"}},
	}
	mcp["dependentRequired"] = obj{"args": []string{"command"}, "env": []string{"command"}, "headers": []string{"url"}, "allowWrite": []string{"managedId"}}

	auth := object([]string{"source"}, obj{
		"source": withDesc(strEnum("managedSecret", "workerEnvironment"),
			"Where the credential comes from. managedSecret reads a secret. workerEnvironment uses the environment of a self-hosted worker, so the effective workerHost must not be empty."),
		"secretName": withDesc(ref("secretName"), "Secret that holds the credential. Required for managedSecret. Not allowed for workerEnvironment."),
	})
	auth["allOf"] = []any{
		obj{"if": obj{"properties": obj{"source": obj{"const": "managedSecret"}}}, "then": obj{"required": []string{"secretName"}}},
		obj{"if": obj{"properties": obj{"source": obj{"const": "workerEnvironment"}}}, "then": obj{"not": obj{"required": []string{"secretName"}}}},
	}

	return document("common", "Open Factory common definitions", "Shared field definitions referenced by the other v1 schemas.", obj{
		"$defs": obj{
			"resourceName": obj{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`},
			"secretName":   obj{"type": "string", "pattern": `^[A-Za-z_][A-Za-z0-9_]{0,127}$`},
			"secretList":   obj{"type": "array", "items": ref("secretName"), "uniqueItems": true},
			"model":        withDesc(nonEmpty(), "A model name that the harness CLI accepts."),
			"harnessType":  strEnum("claude", "codex", "claude-code"),
			"harnessAuth":  auth,
			"harness":      full,
			"harnessOverride": withDesc(sparse,
				"Sparse override of the inherited harness. Set auth to null to clear inherited authentication."),
			"mcpServer": mcp,
			"mcpServers": obj{
				"type":                 "object",
				"propertyNames":        obj{"pattern": `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, "not": obj{"const": ReservedMCPServerName}},
				"additionalProperties": ref("mcpServer"),
			},
			"host": obj{"anyOf": []any{hostname, ipv6}},
			"networkPolicy": nullable(object(nil, obj{
				"extends": withDesc(obj{"anyOf": []any{policyRef, obj{"type": "array", "items": policyRef, "uniqueItems": true}}},
					"Built-in policies to start from, for example builtin/package-registries@^1, builtin/claude-code@1.0.0 or builtin/codex@latest. Each reference must resolve."),
				"allowedHosts": withDesc(arr(ref("host")),
					"Hosts that the run can reach: host names or literal IP addresses. No URLs, ports, CIDR ranges or wildcards. Stored in lowercase. Duplicates are removed."),
				"deniedHosts": withDesc(arr(ref("host")),
					"Hosts that the run cannot reach. The rules of allowedHosts apply. A host that any level denies is removed."),
			})),
			"codeForge":  strEnum(codeForgeValues...),
			"credential": strEnum("EXECUTOR", "CREATOR"),
		},
	})
}

func factorySchema() obj {
	agentDefaults := withDesc(object(nil, obj{
		"model":         withDesc(ref("model"), "Model shorthand for every agent. Set exactly one of model or harness."),
		"harness":       withDesc(ref("harness"), "Complete harness object for every agent: type and model are required. Set exactly one of model or harness."),
		"machine":       withDesc(ref("resourceName"), "Default machine: the name of a file machines/<name>.yaml. It must be a Linux machine."),
		"environmentId": withDesc(str(), "ID of the default environment."),
		"secrets":       withDesc(ref("secretList"), "Default secret names for agents without their own secrets."),
		"mcpServers": withDesc(ref("mcpServers"),
			"Default MCP servers for agents without their own mcpServers. A name must not also be in the factory-wide mcpServers."),
		"workerHost": withDesc(nullable(str()), "Default worker host. An empty value means the server default (server)."),
	}), "Execution settings that every agent inherits. idleTimeoutMinutes is not allowed here: set it per agent or per automation.")
	agentDefaults["oneOf"] = []any{obj{"required": []string{"model"}}, obj{"required": []string{"harness"}}}
	repoTriple := func(d string) obj {
		return withDesc(object([]string{"codeForge", "owner", "name"}, obj{
			"codeForge": withDesc(obj{"const": "GITHUB"}, "Code host of the repository. Only GITHUB."),
			"owner":     withDesc(nonEmpty(), "Owner of the repository."),
			"name":      withDesc(nonEmpty(), "Name of the repository."),
		}), d)
	}
	gcp := withDesc(object([]string{"projectNumber", "workloadIdentityFederationPoolId", "workloadIdentityFederationProviderId"}, obj{
		"projectNumber":                        withDesc(obj{"type": "string", "pattern": "^[0-9]+$"}, "Number of the Google Cloud project."),
		"workloadIdentityFederationPoolId":     withDesc(nonEmpty(), "ID of the workload identity pool."),
		"workloadIdentityFederationProviderId": withDesc(nonEmpty(), "ID of the provider in the workload identity pool."),
		"serviceAccountEmail":                  withDesc(str(), "Service account that the credentials impersonate. Must be an email address."),
	}), "Google Cloud workload identity federation.")
	aws := withDesc(object([]string{"roleArn"}, obj{
		"roleArn": withDesc(obj{"type": "string", "pattern": "^arn:"}, "ARN of the IAM role that the runs assume. It must start with arn: and contain at least 5 : characters."),
	}), "AWS web identity federation.")
	cloud := withDesc(object(nil, obj{"gcp": gcp, "aws": aws,
		"audiences": withDesc(obj{"type": "array", "items": nonEmpty(), "uniqueItems": true},
			"More audiences for which a run can get a token, for other services that accept OIDC tokens."),
	}), "Cloud identity federation. Every run gets short-lived credentials for the declared clouds.")
	replies := func(t string) obj {
		return withDesc(object(nil, obj{
			"autoRespondToThreadReplies": withDefault(withDesc(boolean(),
				"If true, a reply in the thread of a work item continues the work item. If false, a reply without a mention does not."), true),
		}), "Settings of the "+t+" entry. Valid only when type is "+t+".")
	}
	integration := object([]string{"type"}, obj{
		"type": withDesc(strEnum(integrationTypes...),
			"Integration type. Each type can occur only once. linear and jira are mutually exclusive."),
		"slack":           replies("slack"),
		"microsoft-teams": replies("microsoft-teams"),
		"linear": withDesc(object(nil, obj{"teamIds": withDesc(arr(str()), "Accepted for compatibility and ignored.")}),
			"Settings of the linear entry. Valid only when type is linear."),
		"jira": withDesc(object(nil, obj{"projectKeys": withDesc(arr(str()), "Accepted and ignored.")}),
			"Settings of the jira entry. Valid only when type is jira."),
	})
	si := withDesc(object(nil, obj{
		"failedRunThreshold": withDefault(withDesc(intRange(1, 50),
			"Number of distinct unreviewed failures of one agent that starts a Self-improvement run."), 25),
		"reviewerType": withDefault(withDesc(strEnum("admins", "team", "custom", "none"),
			"Who reviews a proposed change: admins (team admins), team (any team member), custom (the members in reviewerEmails) or none."), "admins"),
		"reviewerEmails": withDesc(obj{"type": "array", "items": str(), "minItems": 1, "uniqueItems": true},
			"Emails of the reviewers. Required when reviewerType is custom, and allowed only then. Each entry must be an email address."),
		"analysisModel": withDesc(nonEmpty(), "The model that examines failed runs. The default is the model of the foreman."),
	}), "Settings of the Self-improvement runs, which examine failed runs and propose changes.")
	si["allOf"] = []any{obj{
		"if":   obj{"properties": obj{"reviewerType": obj{"const": "custom"}}, "required": []string{"reviewerType"}},
		"then": obj{"required": []string{"reviewerEmails"}},
		"else": obj{"not": obj{"required": []string{"reviewerEmails"}}},
	}}
	f := object([]string{"schemaVersion", "name", "repositories", "agentDefaults"}, obj{
		"schemaVersion": withDesc(obj{"const": SchemaVersion}, "Schema version of the definition."),
		"name":          withDesc(nonEmpty(), "Name of the factory."),
		"description":   withDesc(str(), "Short description of the factory."),
		"alias": withDesc(obj{"type": "string", "pattern": `^[A-Za-z0-9 ._-]{1,60}$`},
			"Handle of the foreman in chat and on code hosts. It must not be blank and must be unique in the team. Default: the current alias of the factory, else a value made from name."),
		"credentialStrategy": withDesc(ref("credential"),
			"Whose code-forge credentials the runs use. Agents inherit it. Without a value at any level, the strategy is EXECUTOR."),
		"maxConcurrentRuns": withDesc(obj{"type": "integer", "minimum": 1},
			"Most open sessions that the factory can have at one time. A queued run waits while the factory is at its limit. Without a value, there is no limit."),
		"approvers": withDesc(obj{"type": "array", "items": nonEmpty(), "minItems": 1, "uniqueItems": true},
			"Emails of users or names of service accounts. If set, only they can resolve an approval, from every tool. Other members can still answer questions. Without a value, each team member can approve."),
		"restrictFollowupsToStarter": withDefault(withDesc(boolean(),
			"If true, only the user who started a work item can send it follow-up messages. Other members can still resolve its inbox items."), false),
		"requireLinkedActor": withDefault(withDesc(boolean(),
			"If true, events from a forge, chat or tracker account that is not linked to an active team member are dropped. Custom webhooks, schedules, the API and MCP are not affected."), false),
		"allowAgentMerge": withDefault(withDesc(boolean(),
			"If true, agents can call the merge tools of MCP servers through the control plane, and the audit log records each call. If false, the control plane refuses each merge call."), false),
		"spendLimit": withDesc(object(nil, obj{
			"daily":   withDesc(obj{"type": "number", "exclusiveMinimum": 0}, "Most model spend in one day, in US dollars."),
			"monthly": withDesc(obj{"type": "number", "exclusiveMinimum": 0}, "Most model spend in one month, in US dollars."),
		}), "Model spend limit of the factory. Set daily, monthly or both."),
		"automations": withDesc(object(nil, obj{
			"githubOnboardingNotices": withDefault(withDesc(boolean(),
				"If true, Open Factory comments once on the issue or pull request when a GitHub account that cannot start work sends an event. If false, it ignores the event without a comment."), true),
		}), "Factory settings for automations. The automations themselves are files automations/<name>/automation.md."),
		"github": withDesc(object(nil, obj{
			"factoryConfigChecks": withDefault(withDesc(strEnum("main", "all"),
				"Pull requests that get the openfactory/definition check: main (pull requests to the production branch) or all (pull requests to every branch). Open Factory reads it from the active definition."), "main"),
		}), "GitHub settings of the factory."),
		"codeForges": withDesc(obj{"type": "array", "items": ref("codeForge"), "uniqueItems": true},
			"Code hosts of the repositories. If it is [], repositories must be empty."),
		"repositories": withDesc(arr(object([]string{"owner", "name"}, obj{
			"codeForge": withDesc(ref("codeForge"),
				"Code host of the repository. Required when codeForges lists more than one value. If codeForges is set, the value must be in it. Default: the only codeForges value, else GITHUB."),
			"owner": withDesc(nonEmpty(),
				"Owner of the repository. No spaces or tabs, no leading or trailing /, no //. For GITHUB, no /. For GITLAB, it can be a group path (acme/platform). For AZURE_DEVOPS, it is organization/project."),
			"name": withDesc(nonEmpty(), "Name of one repository: no /, space or tab."),
		})), "Repositories that the factory works on. It can be []. A repository can be listed only once (case-insensitive)."),
		"benchmarkRepoSubstitutions": withDesc(arr(object([]string{"source", "target"}, obj{
			"source": repoTriple("Repository to replace. It must be one of the repositories of the factory."),
			"target": repoTriple("Repository that the trials use. It must not be one of the repositories of the factory."),
		})), "Repositories that benchmark trials use in place of production repositories."),
		"secrets": withDesc(ref("secretList"), "Factory-wide secret names. Every agent gets them, and no level can remove them."),
		"networkPolicy": withDesc(ref("networkPolicy"),
			"Outbound network policy of every run. An agent policy merges with it. Without a policy at any level, outbound access is unrestricted."),
		"mcpServers": withDesc(ref("mcpServers"),
			"Factory-wide MCP servers. Every agent gets them. A lower level must not declare the same name again."),
		"cloudProviders": cloud,
		"integrations": withDesc(nullable(arr(integration)),
			"Chat and tracker integrations of the factory. If absent, the factory keeps its integrations. null or [] removes them all. When it is set, each slack, teams, linear or jira trigger needs its integration in the list."),
		"scorerDefaults": withDesc(object(nil, obj{
			"machine": withDesc(ref("resourceName"), "Machine of the judge runs: the name of a file machines/<name>.yaml. A scorer's machine replaces it."),
			"secrets": withDesc(ref("secretList"), "Secret names of the judge runs. A scorer's secrets add to them."),
			"mcpServers": withDesc(ref("mcpServers"),
				"MCP servers of the judge runs. A scorer's servers add to them. A scorer server with the same name replaces the server."),
			"model":   withDesc(ref("model"), "The model of the judge runs. It replaces the model of the harness."),
			"harness": withDesc(nullable(ref("harnessOverride")), "The harness of the judge runs, over the harness of agentDefaults."),
		}), "Execution settings of the judge runs of every scorer and of the built-in Correctness Scorer of benchmarks. Without it, the judge uses the harness of agentDefaults."),
		"selfImprovement": si,
		"agentDefaults":   agentDefaults,
	})
	return document("factory", "factory.yaml", "The root document of an Open Factory definition.", f)
}

func agentSchema() obj {
	p := execProps(false)
	p["description"] = withDesc(str(), "Short description that list_agents shows to the foreman.")
	p["agentType"] = withDefault(withDesc(strEnum(agentTypes...),
		"Role of the agent. MAIN is an alias of FOREMAN. Exactly one agent must be the foreman."), "CUSTOM")
	p["credentialStrategy"] = withDesc(ref("credential"), "Whose code-forge credentials the runs use. It overrides the factory value.")
	p["snapshot"] = withDefault(withDesc(nullable(boolean()), "If false, a snapshot stores only the harness state, not the workspace."), true)
	p["spawnableBy"] = withDesc(nullable(obj{"type": "array", "items": ref("resourceName"), "uniqueItems": true}),
		"Agents that can start this agent with run_agents. Without a value, only the foreman can. [] means no agent. Each name must be an agent of the definition.")
	p["networkPolicy"] = withDesc(ref("networkPolicy"), "Network policy of the agent. It merges with the policy of the factory.")
	a := object(nil, p)
	a["allOf"] = []any{notModelAndHarness}
	return document("agent", "Agent frontmatter", "YAML frontmatter of agents/<name>/agent.md.", a)
}

// matcherDefs are the filter value shapes used by trigger filters.
func matcherDefs() obj {
	matcher := func(item obj) obj {
		return obj{"anyOf": []any{
			obj{"type": "null"},
			arr(item),
			object(nil, obj{"in": nullable(arr(item)), "not_in": nullable(arr(item))}),
		}}
	}
	inOnly := func(min, max int) obj {
		list := obj{"type": "array", "items": nonEmpty(), "minItems": min}
		if max > 0 {
			list["maxItems"] = max
		}
		return obj{"anyOf": []any{list, object([]string{"in"}, obj{"in": list})}}
	}
	scalar := obj{"type": []string{"string", "number", "boolean", "null"}}
	values := obj{"type": "array", "items": scalar, "maxItems": maxPatternValues}
	return obj{
		"stringMatcher": matcher(nonEmpty()),
		"uuidMatcher":   matcher(obj{"type": "string", "pattern": uuidRe.String()}),
		"intMatcher":    matcher(obj{"type": "integer", "minimum": 1}),
		"nameList":      nullable(arr(nonEmpty())),
		"nameIntList":   nullable(arr(obj{"type": "integer", "minimum": 1})),
		"skipBots":      nullable(strEnum("all", "self", "none")),
		"inOnlyOne":     inOnly(1, 1),
		"inOnlyMany":    inOnly(1, 0),
		"payloadPattern": obj{
			"type": "object",
			"additionalProperties": obj{"anyOf": []any{
				values,
				object(nil, obj{"in": nullable(values), "not_in": nullable(values), "exists": boolean()}),
				local("payloadPattern"),
			}},
		},
	}
}

func matcherFor(ps *providerSpec, key string) obj {
	ks := keySpecFor(ps, key)
	switch ks.kind {
	case vUUID:
		return local("uuidMatcher")
	case vInt:
		return local("intMatcher")
	case vEnum:
		item := strEnum(ks.values...)
		return obj{"anyOf": []any{obj{"type": "null"}, arr(item), object(nil, obj{"in": nullable(arr(item)), "not_in": nullable(arr(item))})}}
	case vSkipBots:
		return local("skipBots")
	case vInOnlyOne, vWebhookIDs:
		return local("inOnlyOne")
	case vInOnlyMany:
		return local("inOnlyMany")
	case vPayload:
		return local("payloadPattern")
	}
	return local("stringMatcher")
}

// authoredProviders maps the canonical provider to the names accepted in files.
func authoredProviders(p string) []string {
	if p == ProviderTeams {
		return []string{"teams", "microsoft_teams"}
	}
	return []string{p}
}

func triggerSchema() obj {
	var variants []any
	var providers []string
	for _, p := range TriggerProviders() {
		ps := triggerCatalog[p]
		names := authoredProviders(p)
		providers = append(providers, names...)
		var evs []string
		for e := range ps.events {
			evs = append(evs, e)
		}
		sort.Strings(evs)
		for _, ev := range evs {
			keys := ps.events[ev]
			props := obj{}
			for _, k := range keys {
				if ps.managed[k] {
					continue
				}
				props[k] = matcherFor(ps, k)
			}
			for a, s := range ps.aliases {
				if contains(keys, s.native) {
					if s.rename {
						props[a] = matcherFor(ps, s.native)
					} else if s.kind == vNameAliasInt {
						props[a] = local("nameIntList")
					} else {
						props[a] = local("nameList")
					}
				}
			}
			filter := object(nil, props)
			variant := obj{
				"properties": obj{
					"provider": strEnum(names...),
					"event":    obj{"const": ev},
				},
			}
			var notAlias []any
			for a, s := range ps.aliases {
				if contains(keys, s.native) {
					notAlias = append(notAlias, obj{"required": []string{a, s.native}})
				}
			}
			if len(notAlias) > 0 {
				sort.Slice(notAlias, func(i, j int) bool {
					return notAlias[i].(obj)["required"].([]string)[0] < notAlias[j].(obj)["required"].([]string)[0]
				})
				filter["not"] = obj{"anyOf": notAlias}
			}
			switch ps.filter {
			case filterForbidden:
				variant["required"] = []string{"schedule"}
				variant["not"] = obj{"required": []string{"filter"}}
			case filterRequired:
				// A name alias can stand for a required key.
				var req []string
				var anyOf []any
				for _, rk := range ps.requireKeys {
					var names []string
					for a, s := range ps.aliases {
						if s.native == rk && contains(ps.events[ev], rk) {
							names = append(names, a)
						}
					}
					if len(names) == 0 {
						req = append(req, rk)
						continue
					}
					sort.Strings(names)
					alts := []any{obj{"required": []string{rk}}}
					for _, n := range names {
						alts = append(alts, obj{"required": []string{n}})
					}
					anyOf = append(anyOf, obj{"anyOf": alts})
				}
				filter["required"] = req
				if len(anyOf) > 0 {
					filter["allOf"] = anyOf
				}
				variant["required"] = []string{"filter"}
				variant["properties"].(obj)["filter"] = filter
				variant["not"] = obj{"required": []string{"schedule"}}
			default:
				variant["properties"].(obj)["filter"] = nullable(filter)
				variant["not"] = obj{"required": []string{"schedule"}}
			}
			variants = append(variants, variant)
		}
	}
	sort.Strings(providers)
	t := object([]string{"provider", "event"}, obj{
		"provider": withDesc(strEnum(providers...), "Source of the events. teams and microsoft_teams are the same provider."),
		"event":    withDesc(nonEmpty(), "Event of the provider."),
		"filter": withDesc(obj{},
			"Filter of the event. The provider and the event decide the valid keys, and whether a filter is required, optional or not allowed."),
		"schedule": withDesc(local("schedule"), "Schedule of the trigger. Required for provider schedule (event cron_fired), and allowed only there."),
		"concurrency": withDefault(withDesc(strEnum("allow", "skip"),
			"Schedule triggers only. allow starts a run for every slot. skip drops a slot while a run of the automation is not finished."), "allow"),
	})
	t["oneOf"] = variants
	return t
}

func automationSchema() obj {
	p := execProps(true)
	p["enabled"] = withDefault(withDesc(boolean(), "If false, the automation never matches an event."), true)
	p["agent"] = withDesc(ref("resourceName"), "Agent of the runs. It must be an agent of the definition. Default: the foreman.")
	p["displayName"] = withDesc(nullable(str()), "Name that the dashboard shows.")
	p["runAs"] = withDefault(withDesc(obj{"type": "string", "pattern": `^(factory|user:[^@\s]+@[^@\s]+|serviceAccount:[^\s/:]+)$`},
		"The principal of the runs that the automation starts: factory, user:<email> or serviceAccount:<name>. The principal must exist in the team when you apply the definition."), "factory")
	p["triggers"] = withDesc(obj{"type": "array", "items": local("trigger"), "minItems": 1}, "Events and schedules that start the automation.")
	a := object([]string{"triggers"}, p)
	a["allOf"] = []any{notModelAndHarness}
	defs := matcherDefs()
	defs["trigger"] = triggerSchema()
	defs["schedule"] = object([]string{"cron"}, obj{
		"name": withDesc(nonEmpty(), "Name of the schedule. Names must be unique in the automation. At most one schedule trigger of an automation can omit its name."),
		"cron": withDesc(nonEmpty(), "Five-field UTC cron expression or a descriptor such as @daily or @every 1h. No TZ= prefix."),
	})
	a["$defs"] = defs
	return document("automation", "Automation frontmatter", "YAML frontmatter of automations/<name>/automation.md.", a)
}

func scorerSchema() obj {
	agent := obj{"anyOf": []any{ref("resourceName"), object([]string{"name"}, obj{
		"name":               withDesc(ref("resourceName"), "Name of the agent."),
		"includeDescendants": withDefault(withDesc(boolean(), "If true, the transcripts of all descendant runs are added as evidence."), false),
	})}}
	label := object([]string{"value", "score"}, obj{
		"value":       withDesc(nonEmpty(), "Name of the label. Unique in the scorer."),
		"description": withDesc(str(), "What the label means."),
		"score":       withDesc(numRange(0, 1), "Score of the label."),
	})
	s := object([]string{"name", "agents", "labels", "passingScore"}, obj{
		"name":        withDesc(nonEmpty(), "Name of the scorer. Unique among the scorers."),
		"description": withDesc(str(), "Short description of the scorer."),
		"output":      withDefault(withDesc(obj{"const": "classification"}, "Kind of output."), "classification"),
		"agents": withDesc(obj{"type": "array", "items": agent, "minItems": 1},
			"Agents whose runs the scorer samples. Each entry is an agent name or {name, includeDescendants}. Each name must be an agent of the definition, listed once."),
		"labels": withDesc(obj{"type": "array", "items": label, "minItems": 1, "maxItems": 20}, "Labels that the judge can give a run."),
		"passingScore": withDesc(numRange(0, 1),
			"Lowest passing score. At least one label must have a score at or above it, and at least one label a score below it."),
		"samplingRate": withDefault(withDesc(numRange(0, 100),
			"Percent of finished runs to score, rounded to two decimals. 0 turns off automatic scoring."), 10),
		"model": withDesc(ref("model"),
			"Model of the judge run. Required unless harness is set. Without harness, the model name selects the harness. With harness, it replaces the model of that harness."),
		"selfImprovement": withDefault(withDesc(boolean(), "If true, the failures of this scorer go to Self-improvement."), false),
		"machine":         withDesc(ref("resourceName"), "Machine of the judge run. It replaces scorerDefaults.machine."),
		"secrets":         withDesc(ref("secretList"), "Secret names of the judge run. They add to scorerDefaults.secrets."),
		"mcpServers": withDesc(ref("mcpServers"),
			"MCP servers of the judge run. They add to scorerDefaults.mcpServers. A server with the same name replaces the server of scorerDefaults."),
		"harness": withDesc(nullable(ref("harnessOverride")), "The harness of the judge run, over scorerDefaults and agentDefaults."),
	})
	s["anyOf"] = []any{obj{"required": []string{"model"}}, obj{"required": []string{"harness"}}}
	return document("scorer", "Scorer frontmatter", "YAML frontmatter of scorers/<slug>/scorer.md.", s)
}

func machineSchema() obj {
	shape := withDesc(object([]string{"vcpus", "memoryGb"}, obj{
		"vcpus":    withDesc(obj{"type": "integer", "minimum": 1}, "Number of virtual CPUs."),
		"memoryGb": withDesc(obj{"type": "integer", "minimum": 1}, "Memory in GB."),
	}), "CPU and memory of the sandbox. For linux and windows, both values must be powers of two. For macos, the shape must be 4/7, 6/14, 8/14, 12/28 or 12/56 (vCPUs/GB).")
	platform := withDesc(object(nil, obj{
		"os": withDefault(withDesc(strEnum("linux", "macos", "windows"), "Operating system of the sandbox."), "linux"),
		"arch": withDesc(strEnum("x86_64", "aarch64"),
			"CPU architecture. macos allows only aarch64. windows allows only x86_64. Default: x86_64, or aarch64 for macos."),
		"linux": withDesc(object([]string{"image"}, obj{
			"image": withDesc(nonEmpty(), "Container image of the sandbox. No spaces or tabs."),
			"registryCredentialSecretName": withDesc(ref("secretName"),
				"Secret with the credential to pull a private image. The kubernetes backend ignores it and pulls with the imagePullSecrets of the pod template."),
			"user": withDefault(withDesc(obj{"anyOf": []any{obj{"type": "integer", "minimum": 1}, obj{"const": "image"}}},
				"User of the sandbox on the kubernetes backend. A number sets runAsUser, runAsGroup and fsGroup. image keeps the user of the image. Root (0) is not allowed."), 1000),
		}), "Linux settings. Required when os is linux, and allowed only then."),
		"mac": withDesc(object(nil, obj{
			"version": withDefault(withDesc(strEnum(macVersions...), "macOS version. Quote the value."), DefaultMacVersion),
		}), "macOS settings. Allowed only when os is macos."),
	}), "Operating system, architecture and image of the sandbox.")
	isOS := func(os string) obj {
		return obj{"properties": obj{"os": obj{"const": os}}, "required": []string{"os"}}
	}
	platform["allOf"] = []any{
		obj{"if": isOS("macos"), "then": obj{"properties": obj{"arch": obj{"const": "aarch64"}}, "not": obj{"required": []string{"linux"}}}},
		obj{"if": isOS("windows"), "then": obj{"properties": obj{"arch": obj{"const": "x86_64"}}, "not": obj{"anyOf": []any{obj{"required": []string{"linux"}}, obj{"required": []string{"mac"}}}}}},
		obj{"if": obj{"anyOf": []any{isOS("linux"), obj{"not": obj{"required": []string{"os"}}}}}, "then": obj{"required": []string{"linux"}, "not": obj{"required": []string{"mac"}}}},
	}
	r := object([]string{"platform"}, obj{
		"description":   withDesc(str(), "Short description of the machine."),
		"setupCommands": withDesc(arr(nonEmpty()), "Commands that run in order after the repositories are checked out."),
		"failureSessionRetentionMinutes": withDesc(intRange(1, 60),
			"Minutes that the sandbox of a failed run is kept. Without a value, the sandbox is released at once."),
		"instanceShape": shape,
		"platform":      platform,
	})
	return document("machine", "Machine", "machines/<name>.yaml: compute for agent runs.", r)
}

func webhookSchema() obj {
	w := object([]string{"secretName"}, obj{
		"authMode": withDefault(withDesc(strEnum("token", "url_token", "signature"),
			"How the sender authenticates: token (the secret in an Authorization: Bearer header), url_token (the secret as the last segment of the URL path) or signature (a provider signature). It cannot change after the first apply."), "token"),
		"signatureScheme": withDesc(strEnum(SignatureSchemes...),
			"Signature format of the provider. Required when authMode is signature, and allowed only then. It cannot change after the first apply."),
		"secretName": withDesc(ref("secretName"), "Secret that authenticates the requests."),
		"deliveryIdHeader": withDesc(obj{"type": "string", "pattern": "^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$"},
			"Request header that holds the delivery ID. Open Factory uses the ID to find repeated deliveries. It must not be a credential header, for example Authorization, Cookie or a signature header. Stored in canonical form."),
		"enabled": withDefault(withDesc(boolean(), "If false, the webhook never starts an automation."), true),
	})
	w["allOf"] = []any{obj{
		"if":   obj{"properties": obj{"authMode": obj{"const": "signature"}}, "required": []string{"authMode"}},
		"then": obj{"required": []string{"signatureScheme"}},
		"else": obj{"not": obj{"required": []string{"signatureScheme"}}},
	}}
	return document("webhook", "Webhook", "webhooks/<name>.yaml: an authenticated ingress URL for JSON POST requests.", w)
}

func benchmarkSuiteSchema() obj {
	pin := object([]string{"agent"}, obj{
		"agent":   withDesc(ref("resourceName"), "Agent to pin. It must be an agent of the definition, once per configuration."),
		"model":   withDesc(ref("model"), "Model of the pinned agent. Set exactly one of model or harness."),
		"harness": withDesc(ref("harness"), "Complete harness of the pinned agent: type and model are required. Set exactly one of model or harness."),
	})
	pin["oneOf"] = []any{obj{"required": []string{"model"}}, obj{"required": []string{"harness"}}}
	cfg := object([]string{"name"}, obj{
		"name":    withDesc(nonEmpty(), "Name of the configuration. Unique in the suite."),
		"agents":  withDesc(arr(pin), "Agent pins of the configuration."),
		"machine": withDesc(ref("resourceName"), "Machine of every agent in the trials of this configuration: the name of a file machines/<name>.yaml."),
	})
	s := object([]string{"name", "agent"}, obj{
		"name":           withDesc(nonEmpty(), "Name of the suite. Unique among the suites."),
		"description":    withDesc(str(), "Short description of the suite."),
		"agent":          withDesc(ref("resourceName"), "Agent that the trials start. It must be an agent of the definition."),
		"configurations": withDesc(obj{"type": "array", "items": cfg, "minItems": 1, "maxItems": 6}, "Configurations that the suite compares."),
		"tasks": withDesc(obj{"type": "array", "items": ref("resourceName"), "uniqueItems": true},
			"Task names. Each name needs a file benchmarks/<suite>/tasks/<name>.yaml. Each task file must be in this list."),
	})
	return document("benchmark_suite", "Benchmark suite", "benchmarks/<suite-slug>/suite.yaml.", s)
}

func benchmarkTaskSchema() obj {
	sha := obj{"type": "string", "pattern": "^[0-9a-f]{40}$"}
	ref1 := obj{"anyOf": []any{
		obj{"type": "string", "pattern": `^(github\.com|gitlab\.com):[^@\s]+/[^@/\s]+@[0-9a-f]{40}$`},
		object([]string{"codeForge", "owner", "repo", "ref"}, obj{"codeForge": strEnum("GITHUB", "GITLAB"), "owner": nonEmpty(), "repo": nonEmpty(), "ref": sha}),
	}}
	t := object([]string{"title", "prompt", "successCriteria"}, obj{
		"title":           withDesc(nonEmpty(), "Title of the task."),
		"prompt":          withDesc(nonEmpty(), "Prompt of the trial."),
		"successCriteria": withDesc(nonEmpty(), "What the trial must do to pass."),
		"sourceRunId":     withDesc(nonEmpty(), "Run that the task reproduces."),
		"tags":            withDesc(arr(nonEmpty()), "Free-form tags."),
		"trackerSeed":     withDesc(obj{}, "The start state of the tracker for the trial. Any YAML value."),
		"linearSeed":      withDesc(obj{}, "Older name of trackerSeed. Set only one of them."),
		"startingRepoRefs": withDesc(arr(ref1),
			"Commits that the trial starts from. Each entry is a string github.com:OWNER/REPO@SHA or gitlab.com:OWNER/REPO@SHA, or an object {codeForge, owner, repo, ref}. codeForge is GITHUB or GITLAB. ref is a full 40-character lowercase commit SHA."),
	})
	return document("benchmark_suite_task", "Benchmark task", "benchmarks/<suite-slug>/tasks/<task-slug>.yaml.", t)
}

func generatedSchemas() map[string]obj {
	return map[string]obj{
		"common":               commonSchema(),
		"factory":              factorySchema(),
		"agent":                agentSchema(),
		"automation":           automationSchema(),
		"scorer":               scorerSchema(),
		"machine":              machineSchema(),
		"webhook":              webhookSchema(),
		"benchmark_suite":      benchmarkSuiteSchema(),
		"benchmark_suite_task": benchmarkTaskSchema(),
	}
}

func TestSchemasUpToDate(t *testing.T) {
	update := os.Getenv("UPDATE_SCHEMAS") != ""
	for name, s := range generatedSchemas() {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(s); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join("schemas", SchemaVersion, name+".schema.json")
		if update {
			if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%v (run with UPDATE_SCHEMAS=1)", err)
		}
		if !bytes.Equal(got, buf.Bytes()) {
			t.Errorf("%s is out of date; run UPDATE_SCHEMAS=1 go test ./internal/definition -run TestSchemasUpToDate", file)
		}
	}
}
