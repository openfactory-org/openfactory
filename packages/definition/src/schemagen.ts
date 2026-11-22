// The v1 JSON Schemas of a definition. Port of
// internal/definition/schemagen_test.go: the schemas are generated from the
// compiler's catalogs (triggers, enums, limits) so that they stay consistent.
// scripts/gen-schemas.ts writes them to schemas/definition/v1/.
// cloudProviders has only gcp and audiences.

import { agentTypes } from "./agents.ts";
import { RESERVED_MCP_SERVER_NAME, SCHEMA_VERSION } from "./common.ts";
import { codeForgeValues, integrationTypes } from "./factory.ts";
import { SIGNATURE_SCHEMES } from "./resources.ts";
import {
  MAX_PATTERN_VALUES,
  type ProviderSpec,
  PROVIDER_TEAMS,
  triggerCatalog,
  triggerProviders,
  uuidPattern,
  ValueKind,
} from "./triggers.ts";

type Obj = Record<string, unknown>;

const draft = "https://json-schema.org/draft/2020-12/schema";

/** SCHEMA_BASE_URL is the base of every schema document's $id. */
export const SCHEMA_BASE_URL = `https://openfactory-org.github.io/schemas/${SCHEMA_VERSION}/`;

const ref = (name: string): Obj => ({ $ref: "common.schema.json#/$defs/" + name });
const local = (name: string): Obj => ({ $ref: "#/$defs/" + name });
const str = (): Obj => ({ type: "string" });
const nonEmpty = (): Obj => ({ type: "string", minLength: 1 });
const nullable = (s: Obj): Obj => ({ anyOf: [s, { type: "null" }] });
const strEnum = (...vals: string[]): Obj => ({ type: "string", enum: vals });
const arr = (items: Obj): Obj => ({ type: "array", items });
const boolean = (): Obj => ({ type: "boolean" });
const intRange = (lo: number, hi: number): Obj => ({ type: "integer", minimum: lo, maximum: hi });
const numRange = (lo: number, hi: number): Obj => ({ type: "number", minimum: lo, maximum: hi });

function object(required: string[] | null, props: Obj): Obj {
  const o: Obj = { type: "object", properties: props, additionalProperties: false };
  if (required && required.length > 0) o["required"] = required;
  return o;
}

function document(name: string, title: string, description: string, body: Obj): Obj {
  body["$schema"] = draft;
  body["$id"] = SCHEMA_BASE_URL + name + ".schema.json";
  body["title"] = title;
  body["description"] = description;
  return body;
}

const withDesc = (s: Obj, d: string): Obj => ({ ...s, description: d });
const withDefault = (s: Obj, v: unknown): Obj => ({ ...s, default: v });

const workerHostValues =
  "Where the agent sessions execute. server: the team's default sandbox provider. A sandbox provider ID (for example cloudflare-sandbox, daytona, boatdev): that provider. A worker ID or name: that enrolled worker. pool:<label>: the least-loaded online worker with that label.";

function execProps(automation: boolean): Obj {
  let secrets =
    "Secret names of the runs. They replace the secrets of agentDefaults. [] removes those defaults. The factory-wide secrets always stay.";
  let mcpServers =
    "MCP servers of the runs. They replace the mcpServers of agentDefaults. {} removes those defaults. The factory-wide servers always stay.";
  const workerHost =
    "Worker that runs the agent. The value is trimmed. null or empty clears an inherited host. " + workerHostValues;
  if (automation) {
    secrets = "Secret names that add to the secrets of the agent for the runs of this automation.";
    mcpServers =
      "MCP servers that add to the servers of the agent for the runs of this automation. A server with the same name replaces the server of the agent.";
  }
  return {
    model: withDesc(
      ref("model"),
      "Model shorthand. A Claude model selects the claude harness. An OpenAI model selects the codex harness. Do not set it together with harness.",
    ),
    harness: withDesc(
      nullable(ref("harnessOverride")),
      "Sparse override of the inherited harness. It changes only the fields that you set. Set auth to null to clear the inherited credential.",
    ),
    machine: withDesc(
      nullable(ref("resourceName")),
      "Machine of the runs: the name of a file machines/<name>.yaml. It replaces the inherited machine.",
    ),
    environmentId: withDesc(nullable(str()), "ID of the environment of the runs. null clears the inherited value."),
    secrets: withDesc(nullable(ref("secretList")), secrets),
    mcpServers: withDesc(nullable(ref("mcpServers")), mcpServers),
    workerHost: withDesc(nullable(str()), workerHost),
    idleTimeoutMinutes: withDesc(
      nullable(intRange(0, 60)),
      "Minutes that a finished run keeps its sandbox and session for follow-ups. 0 ends the session after each turn. Without a value, the server default OF_IDLE_TIMEOUT_DEFAULT applies (10 minutes) to every run. OF_IDLE_TIMEOUT_MAX (60 minutes) is the upper limit.",
    ),
  };
}

const notModelAndHarness = (): Obj => ({
  not: { required: ["model", "harness"], properties: { model: { type: "string" }, harness: { type: "object" } } },
});

function commonSchema(): Obj {
  const hostname = {
    type: "string",
    pattern:
      "^[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?(?:\\.[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?)*\\.?$",
    maxLength: 254,
  };
  const ipv6 = { type: "string", pattern: "^[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*$" };
  const policyRef = {
    type: "string",
    pattern: "^builtin/[a-z0-9-]+@(latest|\\^[0-9]+(\\.[0-9]+){0,2}|[0-9]+\\.[0-9]+\\.[0-9]+)$",
  };
  const harnessProps = {
    type: withDesc(ref("harnessType"), "Harness that runs the agent. claude-code is an alias of claude."),
    model: withDesc(ref("model"), "Model name. The harness gets it as it is."),
    reasoningLevel: withDesc(
      { type: "string", pattern: "^[a-z][a-z0-9_-]{0,31}$" },
      "Reasoning level that the harness passes to the model.",
    ),
    auth: withDesc(
      nullable(ref("harnessAuth")),
      "Model credential of the harness. null clears the inherited credential.",
    ),
  };
  const full = object(["type", "model"], harnessProps);
  const sparse = object(null, harnessProps);

  const mcp = object(null, {
    managedId: withDesc(
      nonEmpty(),
      "Name or ID of a server in the MCP server registry of the team, or a built-in server: builtin/jira, builtin/linear, builtin/slack or builtin/teams. An unknown builtin/ server is an error.",
    ),
    command: withDesc(nonEmpty(), "Executable that starts inside the sandbox. The server uses the stdio transport."),
    args: withDesc(arr(str()), "Arguments of command. Valid only with command."),
    env: withDesc(
      { type: "object", additionalProperties: str() },
      "Environment variables of command. Valid only with command. Values can use {{SECRET}} placeholders.",
    ),
    url: withDesc(
      { type: "string", pattern: "^https?://[^/\\s]+" },
      "Absolute http or https URL with a host. The server uses streamable HTTP, or legacy SSE when the path ends in /sse.",
    ),
    headers: withDesc(
      { type: "object", additionalProperties: str() },
      "HTTP headers of url. Valid only with url. Values can use {{SECRET}} placeholders.",
    ),
    allowWrite: withDefault(
      {
        type: "boolean",
        description:
          "Enables the write tools of a built-in integration server. Valid only with managedId builtin/<provider>.",
      },
      false,
    ),
  });
  mcp["oneOf"] = [{ required: ["managedId"] }, { required: ["command"] }, { required: ["url"] }];
  mcp["dependentRequired"] = { args: ["command"], env: ["command"], headers: ["url"], allowWrite: ["managedId"] };

  const auth = object(["source"], {
    source: withDesc(
      strEnum("managedSecret", "workerEnvironment"),
      "Where the credential comes from. managedSecret reads a secret. workerEnvironment uses the environment of a self-hosted worker, so the effective workerHost must not be empty.",
    ),
    secretName: withDesc(
      ref("secretName"),
      "Secret that holds the credential. Required for managedSecret. Not allowed for workerEnvironment.",
    ),
  });
  auth["allOf"] = [
    { if: { properties: { source: { const: "managedSecret" } } }, then: { required: ["secretName"] } },
    { if: { properties: { source: { const: "workerEnvironment" } } }, then: { not: { required: ["secretName"] } } },
  ];

  return document(
    "common",
    "Open Factory common definitions",
    "Shared field definitions referenced by the other v1 schemas.",
    {
      $defs: {
        resourceName: { type: "string", pattern: "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" },
        secretName: { type: "string", pattern: "^[A-Za-z_][A-Za-z0-9_]{0,127}$" },
        secretList: { type: "array", items: ref("secretName"), uniqueItems: true },
        model: withDesc(nonEmpty(), "A model name that the harness CLI accepts."),
        harnessType: strEnum("claude", "codex", "claude-code"),
        harnessAuth: auth,
        harness: full,
        harnessOverride: withDesc(
          sparse,
          "Sparse override of the inherited harness. Set auth to null to clear inherited authentication.",
        ),
        mcpServer: mcp,
        mcpServers: {
          type: "object",
          propertyNames: { pattern: "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", not: { const: RESERVED_MCP_SERVER_NAME } },
          additionalProperties: ref("mcpServer"),
        },
        host: { anyOf: [hostname, ipv6] },
        networkPolicy: nullable(
          object(null, {
            extends: withDesc(
              { anyOf: [policyRef, { type: "array", items: policyRef, uniqueItems: true }] },
              "Built-in policies to start from, for example builtin/package-registries@^1, builtin/claude-code@1.0.0 or builtin/codex@latest. Each reference must resolve.",
            ),
            allowedHosts: withDesc(
              arr(ref("host")),
              "Hosts that the run can reach: host names or literal IP addresses. No URLs, ports, CIDR ranges or wildcards. Stored in lowercase. Duplicates are removed.",
            ),
            deniedHosts: withDesc(
              arr(ref("host")),
              "Hosts that the run cannot reach. The rules of allowedHosts apply. A host that any level denies is removed.",
            ),
          }),
        ),
        codeForge: strEnum(...codeForgeValues),
        credential: strEnum("EXECUTOR", "CREATOR"),
      },
    },
  );
}

function factorySchema(): Obj {
  const agentDefaults = withDesc(
    object(null, {
      model: withDesc(ref("model"), "Model shorthand for every agent. Set exactly one of model or harness."),
      harness: withDesc(
        ref("harness"),
        "Complete harness object for every agent: type and model are required. Set exactly one of model or harness.",
      ),
      machine: withDesc(
        ref("resourceName"),
        "Default machine: the name of a file machines/<name>.yaml. It must be a Linux machine.",
      ),
      environmentId: withDesc(str(), "ID of the default environment."),
      secrets: withDesc(ref("secretList"), "Default secret names for agents without their own secrets."),
      mcpServers: withDesc(
        ref("mcpServers"),
        "Default MCP servers for agents without their own mcpServers. A name must not also be in the factory-wide mcpServers.",
      ),
      workerHost: withDesc(nullable(str()), "Default worker host. An empty value means server. " + workerHostValues),
    }),
    "Execution settings that every agent inherits. idleTimeoutMinutes is not allowed here: set it per agent or per automation.",
  );
  agentDefaults["oneOf"] = [{ required: ["model"] }, { required: ["harness"] }];
  const repoTriple = (d: string): Obj =>
    withDesc(
      object(["codeForge", "owner", "name"], {
        codeForge: withDesc({ const: "GITHUB" }, "Code host of the repository. Only GITHUB."),
        owner: withDesc(nonEmpty(), "Owner of the repository."),
        name: withDesc(nonEmpty(), "Name of the repository."),
      }),
      d,
    );
  const gcp = withDesc(
    object(["projectNumber", "workloadIdentityFederationPoolId", "workloadIdentityFederationProviderId"], {
      projectNumber: withDesc({ type: "string", pattern: "^[0-9]+$" }, "Number of the Google Cloud project."),
      workloadIdentityFederationPoolId: withDesc(nonEmpty(), "ID of the workload identity pool."),
      workloadIdentityFederationProviderId: withDesc(nonEmpty(), "ID of the provider in the workload identity pool."),
      serviceAccountEmail: withDesc(
        str(),
        "Service account that the credentials impersonate. Must be an email address.",
      ),
    }),
    "Google Cloud workload identity federation.",
  );
  const cloud = withDesc(
    object(null, {
      gcp,
      audiences: withDesc(
        { type: "array", items: nonEmpty(), uniqueItems: true },
        "More audiences for which a run can get a token, for other services that accept OIDC tokens.",
      ),
    }),
    "Cloud identity federation. Every run gets short-lived credentials for the declared clouds.",
  );
  const replies = (t: string): Obj =>
    withDesc(
      object(null, {
        autoRespondToThreadReplies: withDefault(
          withDesc(
            boolean(),
            "If true, a reply in the thread of a work item continues the work item. If false, a reply without a mention does not.",
          ),
          true,
        ),
      }),
      "Settings of the " + t + " entry. Valid only when type is " + t + ".",
    );
  const integration = object(["type"], {
    type: withDesc(
      strEnum(...integrationTypes),
      "Integration type. Each type can occur only once. linear and jira are mutually exclusive.",
    ),
    slack: replies("slack"),
    "microsoft-teams": replies("microsoft-teams"),
    linear: withDesc(
      object(null, { teamIds: withDesc(arr(str()), "Accepted for compatibility and ignored.") }),
      "Settings of the linear entry. Valid only when type is linear.",
    ),
    jira: withDesc(
      object(null, { projectKeys: withDesc(arr(str()), "Accepted and ignored.") }),
      "Settings of the jira entry. Valid only when type is jira.",
    ),
  });
  const si = withDesc(
    object(null, {
      failedRunThreshold: withDefault(
        withDesc(
          intRange(1, 50),
          "Number of distinct unreviewed failures of one agent that starts a Self-improvement run.",
        ),
        25,
      ),
      reviewerType: withDefault(
        withDesc(
          strEnum("admins", "team", "custom", "none"),
          "Who reviews a proposed change: admins (team admins), team (any team member), custom (the members in reviewerEmails) or none.",
        ),
        "admins",
      ),
      reviewerEmails: withDesc(
        { type: "array", items: str(), minItems: 1, uniqueItems: true },
        "Emails of the reviewers. Required when reviewerType is custom, and allowed only then. Each entry must be an email address.",
      ),
      analysisModel: withDesc(
        nonEmpty(),
        "The model that examines failed runs. The default is the model of the foreman.",
      ),
    }),
    "Settings of the Self-improvement runs, which examine failed runs and propose changes.",
  );
  si["allOf"] = [
    {
      if: { properties: { reviewerType: { const: "custom" } }, required: ["reviewerType"] },
      then: { required: ["reviewerEmails"] },
      else: { not: { required: ["reviewerEmails"] } },
    },
  ];
  const f = object(["schemaVersion", "name", "repositories", "agentDefaults"], {
    schemaVersion: withDesc({ const: SCHEMA_VERSION }, "Schema version of the definition."),
    name: withDesc(nonEmpty(), "Name of the factory."),
    description: withDesc(str(), "Short description of the factory."),
    alias: withDesc(
      { type: "string", pattern: "^[A-Za-z0-9 ._-]{1,60}$" },
      "Handle of the foreman in chat and on code hosts. It must not be blank and must be unique in the team. Default: the current alias of the factory, else a value made from name.",
    ),
    credentialStrategy: withDesc(
      ref("credential"),
      "Whose code-forge credentials the runs use. Agents inherit it. Without a value at any level, the strategy is EXECUTOR.",
    ),
    maxConcurrentRuns: withDesc(
      { type: "integer", minimum: 1 },
      "Most open sessions that the factory can have at one time. A queued run waits while the factory is at its limit. Without a value, there is no limit.",
    ),
    approvers: withDesc(
      { type: "array", items: nonEmpty(), minItems: 1, uniqueItems: true },
      "Emails of users or names of service accounts. If set, only they can resolve an approval, from every tool. Other members can still answer questions. Without a value, each team member can approve.",
    ),
    restrictFollowupsToStarter: withDefault(
      withDesc(
        boolean(),
        "If true, only the user who started a work item can send it follow-up messages. Other members can still resolve its inbox items.",
      ),
      false,
    ),
    requireLinkedActor: withDefault(
      withDesc(
        boolean(),
        "If true, events from a forge, chat or tracker account that is not linked to an active team member are dropped. Custom webhooks, schedules, the API and MCP are not affected.",
      ),
      false,
    ),
    allowAgentMerge: withDefault(
      withDesc(
        boolean(),
        "If true, agents can call the merge tools of MCP servers through the control plane, and the audit log records each call. If false, the control plane refuses each merge call.",
      ),
      false,
    ),
    spendLimit: withDesc(
      object(null, {
        daily: withDesc({ type: "number", exclusiveMinimum: 0 }, "Most model spend in one day, in US dollars."),
        monthly: withDesc({ type: "number", exclusiveMinimum: 0 }, "Most model spend in one month, in US dollars."),
      }),
      "Model spend limit of the factory. Set daily, monthly or both.",
    ),
    automations: withDesc(
      object(null, {
        githubOnboardingNotices: withDefault(
          withDesc(
            boolean(),
            "If true, Open Factory comments once on the issue or pull request when a GitHub account that cannot start work sends an event. If false, it ignores the event without a comment.",
          ),
          true,
        ),
      }),
      "Factory settings for automations. The automations themselves are files automations/<name>/automation.md.",
    ),
    github: withDesc(
      object(null, {
        factoryConfigChecks: withDefault(
          withDesc(
            strEnum("main", "all"),
            "Pull requests that get the openfactory/definition check: main (pull requests to the production branch) or all (pull requests to every branch). Open Factory reads it from the active definition.",
          ),
          "main",
        ),
      }),
      "GitHub settings of the factory.",
    ),
    codeForges: withDesc(
      { type: "array", items: ref("codeForge"), uniqueItems: true },
      "Code hosts of the repositories. If it is [], repositories must be empty.",
    ),
    repositories: withDesc(
      arr(
        object(["owner", "name"], {
          codeForge: withDesc(
            ref("codeForge"),
            "Code host of the repository. Required when codeForges lists more than one value. If codeForges is set, the value must be in it. Default: the only codeForges value, else GITHUB.",
          ),
          owner: withDesc(
            nonEmpty(),
            "Owner of the repository. No spaces or tabs, no leading or trailing /, no //. For GITHUB, no /. For GITLAB, it can be a group path (acme/platform). For AZURE_DEVOPS, it is organization/project.",
          ),
          name: withDesc(nonEmpty(), "Name of one repository: no /, space or tab."),
        }),
      ),
      "Repositories that the factory works on. It can be []. A repository can be listed only once (case-insensitive).",
    ),
    benchmarkRepoSubstitutions: withDesc(
      arr(
        object(["source", "target"], {
          source: repoTriple("Repository to replace. It must be one of the repositories of the factory."),
          target: repoTriple("Repository that the trials use. It must not be one of the repositories of the factory."),
        }),
      ),
      "Repositories that benchmark trials use in place of production repositories.",
    ),
    secrets: withDesc(
      ref("secretList"),
      "Factory-wide secret names. Every agent gets them, and no level can remove them.",
    ),
    networkPolicy: withDesc(
      ref("networkPolicy"),
      "Outbound network policy of every run. An agent policy merges with it. Without a policy at any level, outbound access is unrestricted.",
    ),
    mcpServers: withDesc(
      ref("mcpServers"),
      "Factory-wide MCP servers. Every agent gets them. A lower level must not declare the same name again.",
    ),
    cloudProviders: cloud,
    integrations: withDesc(
      nullable(arr(integration)),
      "Chat and tracker integrations of the factory. If absent, the factory keeps its integrations. null or [] removes them all. When it is set, each slack, teams, linear or jira trigger needs its integration in the list.",
    ),
    scorerDefaults: withDesc(
      object(null, {
        machine: withDesc(
          ref("resourceName"),
          "Machine of the judge runs: the name of a file machines/<name>.yaml. A scorer's machine replaces it.",
        ),
        secrets: withDesc(ref("secretList"), "Secret names of the judge runs. A scorer's secrets add to them."),
        mcpServers: withDesc(
          ref("mcpServers"),
          "MCP servers of the judge runs. A scorer's servers add to them. A scorer server with the same name replaces the server.",
        ),
        model: withDesc(ref("model"), "The model of the judge runs. It replaces the model of the harness."),
        harness: withDesc(
          nullable(ref("harnessOverride")),
          "The harness of the judge runs, over the harness of agentDefaults.",
        ),
      }),
      "Execution settings of the judge runs of every scorer and of the built-in Correctness Scorer of benchmarks. Without it, the judge uses the harness of agentDefaults.",
    ),
    selfImprovement: si,
    agentDefaults,
  });
  return document("factory", "factory.yaml", "The root document of an Open Factory definition.", f);
}

function agentSchema(): Obj {
  const p = execProps(false);
  p["description"] = withDesc(str(), "Short description that list_agents shows to the foreman.");
  p["agentType"] = withDefault(
    withDesc(
      strEnum(...agentTypes),
      "Role of the agent. MAIN is an alias of FOREMAN. Exactly one agent must be the foreman.",
    ),
    "CUSTOM",
  );
  p["credentialStrategy"] = withDesc(
    ref("credential"),
    "Whose code-forge credentials the runs use. It overrides the factory value.",
  );
  p["snapshot"] = withDefault(
    withDesc(nullable(boolean()), "If false, a snapshot stores only the harness state, not the workspace."),
    true,
  );
  p["spawnableBy"] = withDesc(
    nullable({ type: "array", items: ref("resourceName"), uniqueItems: true }),
    "Agents that can start this agent with run_agents. Without a value, only the foreman can. [] means no agent. Each name must be an agent of the definition.",
  );
  p["networkPolicy"] = withDesc(
    ref("networkPolicy"),
    "Network policy of the agent. It merges with the policy of the factory.",
  );
  const a = object(null, p);
  a["allOf"] = [notModelAndHarness()];
  return document("agent", "Agent frontmatter", "YAML frontmatter of agents/<name>/agent.md.", a);
}

function matcherDefs(): Obj {
  const matcher = (item: Obj): Obj => ({
    anyOf: [{ type: "null" }, arr(item), object(null, { in: nullable(arr(item)), not_in: nullable(arr(item)) })],
  });
  const inOnly = (min: number, max: number): Obj => {
    const list: Obj = { type: "array", items: nonEmpty(), minItems: min };
    if (max > 0) list["maxItems"] = max;
    return { anyOf: [list, object(["in"], { in: list })] };
  };
  const scalar = { type: ["string", "number", "boolean", "null"] };
  const values = { type: "array", items: scalar, maxItems: MAX_PATTERN_VALUES };
  return {
    stringMatcher: matcher(nonEmpty()),
    uuidMatcher: matcher({ type: "string", pattern: uuidPattern }),
    intMatcher: matcher({ type: "integer", minimum: 1 }),
    nameList: nullable(arr(nonEmpty())),
    nameIntList: nullable(arr({ type: "integer", minimum: 1 })),
    skipBots: nullable(strEnum("all", "self", "none")),
    inOnlyOne: inOnly(1, 1),
    inOnlyMany: inOnly(1, 0),
    payloadPattern: {
      type: "object",
      additionalProperties: {
        anyOf: [
          values,
          object(null, { in: nullable(values), not_in: nullable(values), exists: boolean() }),
          local("payloadPattern"),
        ],
      },
    },
  };
}

function matcherFor(ps: ProviderSpec, key: string): Obj {
  const ks = ps.keys.get(key) ?? { kind: ValueKind.String };
  switch (ks.kind) {
    case ValueKind.UUID:
      return local("uuidMatcher");
    case ValueKind.Int:
      return local("intMatcher");
    case ValueKind.Enum: {
      const item = strEnum(...(ks.values ?? []));
      return {
        anyOf: [{ type: "null" }, arr(item), object(null, { in: nullable(arr(item)), not_in: nullable(arr(item)) })],
      };
    }
    case ValueKind.SkipBots:
      return local("skipBots");
    case ValueKind.InOnlyOne:
    case ValueKind.WebhookIDs:
      return local("inOnlyOne");
    case ValueKind.InOnlyMany:
      return local("inOnlyMany");
    case ValueKind.Payload:
      return local("payloadPattern");
  }
  return local("stringMatcher");
}

function authoredProviders(p: string): string[] {
  return p === PROVIDER_TEAMS ? ["teams", "microsoft_teams"] : [p];
}

function triggerSchema(): Obj {
  const variants: Obj[] = [];
  const providers: string[] = [];
  for (const p of triggerProviders()) {
    const ps = triggerCatalog.get(p)!;
    const names = authoredProviders(p);
    providers.push(...names);
    const evs = [...ps.events.keys()].sort();
    for (const ev of evs) {
      const keys = ps.events.get(ev)!;
      const props: Obj = {};
      for (const k of keys) {
        if (ps.managed.has(k)) continue;
        props[k] = matcherFor(ps, k);
      }
      for (const [a, s] of ps.aliases) {
        if (!keys.includes(s.native)) continue;
        if (s.rename) props[a] = matcherFor(ps, s.native);
        else if (s.kind === ValueKind.NameAliasInt) props[a] = local("nameIntList");
        else props[a] = local("nameList");
      }
      const filter = object(null, props);
      const variantProps: Obj = { provider: strEnum(...names), event: { const: ev } };
      const variant: Obj = { properties: variantProps };
      const notAlias: { required: string[] }[] = [];
      for (const [a, s] of ps.aliases) if (keys.includes(s.native)) notAlias.push({ required: [a, s.native] });
      if (notAlias.length > 0) {
        notAlias.sort((x, y) => (x.required[0]! < y.required[0]! ? -1 : x.required[0]! > y.required[0]! ? 1 : 0));
        filter["not"] = { anyOf: notAlias };
      }
      if (ps.filter === "forbidden") {
        variant["required"] = ["schedule"];
        variant["not"] = { required: ["filter"] };
      } else if (ps.filter === "required") {
        const req: string[] = [];
        const anyOf: Obj[] = [];
        for (const rk of ps.requireKeys) {
          const aliasNames: string[] = [];
          for (const [a, s] of ps.aliases) if (s.native === rk && keys.includes(rk)) aliasNames.push(a);
          if (aliasNames.length === 0) {
            req.push(rk);
            continue;
          }
          aliasNames.sort();
          anyOf.push({ anyOf: [{ required: [rk] }, ...aliasNames.map((n) => ({ required: [n] }))] });
        }
        filter["required"] = req;
        if (anyOf.length > 0) filter["allOf"] = anyOf;
        variant["required"] = ["filter"];
        variantProps["filter"] = filter;
        variant["not"] = { required: ["schedule"] };
      } else {
        variantProps["filter"] = nullable(filter);
        variant["not"] = { required: ["schedule"] };
      }
      variants.push(variant);
    }
  }
  providers.sort();
  const t = object(["provider", "event"], {
    provider: withDesc(strEnum(...providers), "Source of the events. teams and microsoft_teams are the same provider."),
    event: withDesc(nonEmpty(), "Event of the provider."),
    filter: withDesc(
      {},
      "Filter of the event. The provider and the event decide the valid keys, and whether a filter is required, optional or not allowed.",
    ),
    schedule: withDesc(
      local("schedule"),
      "Schedule of the trigger. Required for provider schedule (event cron_fired), and allowed only there.",
    ),
    concurrency: withDefault(
      withDesc(
        strEnum("allow", "skip"),
        "Schedule triggers only. allow starts a run for every slot. skip drops a slot while a run of the automation is not finished.",
      ),
      "allow",
    ),
  });
  t["oneOf"] = variants;
  return t;
}

function automationSchema(): Obj {
  const p = execProps(true);
  p["enabled"] = withDefault(withDesc(boolean(), "If false, the automation never matches an event."), true);
  p["agent"] = withDesc(
    ref("resourceName"),
    "Agent of the runs. It must be an agent of the definition. Default: the foreman.",
  );
  p["displayName"] = withDesc(nullable(str()), "Name that the dashboard shows.");
  p["runAs"] = withDefault(
    withDesc(
      { type: "string", pattern: "^(factory|user:[^@\\s]+@[^@\\s]+|serviceAccount:[^\\s/:]+)$" },
      "The principal of the runs that the automation starts: factory, user:<email> or serviceAccount:<name>. The principal must exist in the team when you apply the definition.",
    ),
    "factory",
  );
  p["triggers"] = withDesc(
    { type: "array", items: local("trigger"), minItems: 1 },
    "Events and schedules that start the automation.",
  );
  const a = object(["triggers"], p);
  a["allOf"] = [notModelAndHarness()];
  const defs = matcherDefs();
  defs["trigger"] = triggerSchema();
  defs["schedule"] = object(["cron"], {
    name: withDesc(
      nonEmpty(),
      "Name of the schedule. Names must be unique in the automation. At most one schedule trigger of an automation can omit its name.",
    ),
    cron: withDesc(
      nonEmpty(),
      "Five-field UTC cron expression or a descriptor such as @daily or @every 1h. No TZ= prefix.",
    ),
  });
  a["$defs"] = defs;
  return document("automation", "Automation frontmatter", "YAML frontmatter of automations/<name>/automation.md.", a);
}

function scorerSchema(): Obj {
  const agent = {
    anyOf: [
      ref("resourceName"),
      object(["name"], {
        name: withDesc(ref("resourceName"), "Name of the agent."),
        includeDescendants: withDefault(
          withDesc(boolean(), "If true, the transcripts of all descendant runs are added as evidence."),
          false,
        ),
      }),
    ],
  };
  const label = object(["value", "score"], {
    value: withDesc(nonEmpty(), "Name of the label. Unique in the scorer."),
    description: withDesc(str(), "What the label means."),
    score: withDesc(numRange(0, 1), "Score of the label."),
  });
  const s = object(["name", "agents", "labels", "passingScore"], {
    name: withDesc(nonEmpty(), "Name of the scorer. Unique among the scorers."),
    description: withDesc(str(), "Short description of the scorer."),
    output: withDefault(withDesc({ const: "classification" }, "Kind of output."), "classification"),
    agents: withDesc(
      { type: "array", items: agent, minItems: 1 },
      "Agents whose runs the scorer samples. Each entry is an agent name or {name, includeDescendants}. Each name must be an agent of the definition, listed once.",
    ),
    labels: withDesc(
      { type: "array", items: label, minItems: 1, maxItems: 20 },
      "Labels that the judge can give a run.",
    ),
    passingScore: withDesc(
      numRange(0, 1),
      "Lowest passing score. At least one label must have a score at or above it, and at least one label a score below it.",
    ),
    samplingRate: withDefault(
      withDesc(
        numRange(0, 100),
        "Percent of finished runs to score, rounded to two decimals. 0 turns off automatic scoring.",
      ),
      10,
    ),
    model: withDesc(
      ref("model"),
      "Model of the judge run. Required unless harness is set. Without harness, the model name selects the harness. With harness, it replaces the model of that harness.",
    ),
    selfImprovement: withDefault(
      withDesc(boolean(), "If true, the failures of this scorer go to Self-improvement."),
      false,
    ),
    machine: withDesc(ref("resourceName"), "Machine of the judge run. It replaces scorerDefaults.machine."),
    secrets: withDesc(ref("secretList"), "Secret names of the judge run. They add to scorerDefaults.secrets."),
    mcpServers: withDesc(
      ref("mcpServers"),
      "MCP servers of the judge run. They add to scorerDefaults.mcpServers. A server with the same name replaces the server of scorerDefaults.",
    ),
    harness: withDesc(
      nullable(ref("harnessOverride")),
      "The harness of the judge run, over scorerDefaults and agentDefaults.",
    ),
  });
  s["anyOf"] = [{ required: ["model"] }, { required: ["harness"] }];
  return document("scorer", "Scorer frontmatter", "YAML frontmatter of scorers/<slug>/scorer.md.", s);
}

function machineSchema(): Obj {
  const shape = withDesc(
    object(["vcpus", "memoryGb"], {
      vcpus: withDesc({ type: "integer", minimum: 1 }, "Number of virtual CPUs."),
      memoryGb: withDesc({ type: "integer", minimum: 1 }, "Memory in GB."),
    }),
    "CPU and memory of the sandbox. Both values must be powers of two.",
  );
  const platform = withDesc(
    object(["linux"], {
      os: withDefault(withDesc(strEnum("linux"), "Operating system of the sandbox. Machines run Linux only."), "linux"),
      arch: withDefault(withDesc(strEnum("x86_64", "aarch64"), "CPU architecture."), "x86_64"),
      linux: withDesc(
        object(["image"], {
          image: withDesc(nonEmpty(), "Container image of the sandbox. No spaces or tabs."),
          registryCredentialSecretName: withDesc(
            ref("secretName"),
            "Secret with the credential to pull a private image.",
          ),
          user: withDefault(
            withDesc(
              { anyOf: [{ type: "integer", minimum: 1 }, { const: "image" }] },
              "User of the sandbox. A number sets the user and group IDs. image keeps the user of the image. Root (0) is not allowed.",
            ),
            1000,
          ),
        }),
        "Linux settings.",
      ),
    }),
    "Operating system, architecture and image of the sandbox.",
  );
  const r = object(["platform"], {
    description: withDesc(str(), "Short description of the machine."),
    setupCommands: withDesc(arr(nonEmpty()), "Commands that run in order after the repositories are checked out."),
    failureSessionRetentionMinutes: withDesc(
      intRange(1, 60),
      "Minutes that the sandbox of a failed run is kept. Without a value, the sandbox is released at once.",
    ),
    instanceShape: shape,
    platform,
  });
  return document("machine", "Machine", "machines/<name>.yaml: compute for agent runs.", r);
}

function webhookSchema(): Obj {
  const w = object(["secretName"], {
    authMode: withDefault(
      withDesc(
        strEnum("token", "url_token", "signature"),
        "How the sender authenticates: token (the secret in an Authorization: Bearer header), url_token (the secret as the last segment of the URL path) or signature (a provider signature). It cannot change after the first apply.",
      ),
      "token",
    ),
    signatureScheme: withDesc(
      strEnum(...SIGNATURE_SCHEMES),
      "Signature format of the provider. Required when authMode is signature, and allowed only then. It cannot change after the first apply.",
    ),
    secretName: withDesc(ref("secretName"), "Secret that authenticates the requests."),
    deliveryIdHeader: withDesc(
      { type: "string", pattern: "^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$" },
      "Request header that holds the delivery ID. Open Factory uses the ID to find repeated deliveries. It must not be a credential header, for example Authorization, Cookie or a signature header. Stored in canonical form.",
    ),
    enabled: withDefault(withDesc(boolean(), "If false, the webhook never starts an automation."), true),
  });
  w["allOf"] = [
    {
      if: { properties: { authMode: { const: "signature" } }, required: ["authMode"] },
      then: { required: ["signatureScheme"] },
      else: { not: { required: ["signatureScheme"] } },
    },
  ];
  return document(
    "webhook",
    "Webhook",
    "webhooks/<name>.yaml: an authenticated ingress URL for JSON POST requests.",
    w,
  );
}

function benchmarkSuiteSchema(): Obj {
  const pin = object(["agent"], {
    agent: withDesc(
      ref("resourceName"),
      "Agent to pin. It must be an agent of the definition, once per configuration.",
    ),
    model: withDesc(ref("model"), "Model of the pinned agent. Set exactly one of model or harness."),
    harness: withDesc(
      ref("harness"),
      "Complete harness of the pinned agent: type and model are required. Set exactly one of model or harness.",
    ),
  });
  pin["oneOf"] = [{ required: ["model"] }, { required: ["harness"] }];
  const cfg = object(["name"], {
    name: withDesc(nonEmpty(), "Name of the configuration. Unique in the suite."),
    agents: withDesc(arr(pin), "Agent pins of the configuration."),
    machine: withDesc(
      ref("resourceName"),
      "Machine of every agent in the trials of this configuration: the name of a file machines/<name>.yaml.",
    ),
  });
  const s = object(["name", "agent"], {
    name: withDesc(nonEmpty(), "Name of the suite. Unique among the suites."),
    description: withDesc(str(), "Short description of the suite."),
    agent: withDesc(ref("resourceName"), "Agent that the trials start. It must be an agent of the definition."),
    configurations: withDesc(
      { type: "array", items: cfg, minItems: 1, maxItems: 6 },
      "Configurations that the suite compares.",
    ),
    tasks: withDesc(
      { type: "array", items: ref("resourceName"), uniqueItems: true },
      "Task names. Each name needs a file benchmarks/<suite>/tasks/<name>.yaml. Each task file must be in this list.",
    ),
  });
  return document("benchmark_suite", "Benchmark suite", "benchmarks/<suite-slug>/suite.yaml.", s);
}

function benchmarkTaskSchema(): Obj {
  const sha = { type: "string", pattern: "^[0-9a-f]{40}$" };
  const ref1 = {
    anyOf: [
      { type: "string", pattern: "^(github\\.com|gitlab\\.com):[^@\\s]+/[^@/\\s]+@[0-9a-f]{40}$" },
      object(["codeForge", "owner", "repo", "ref"], {
        codeForge: strEnum("GITHUB", "GITLAB"),
        owner: nonEmpty(),
        repo: nonEmpty(),
        ref: sha,
      }),
    ],
  };
  const t = object(["title", "prompt", "successCriteria"], {
    title: withDesc(nonEmpty(), "Title of the task."),
    prompt: withDesc(nonEmpty(), "Prompt of the trial."),
    successCriteria: withDesc(nonEmpty(), "What the trial must do to pass."),
    sourceRunId: withDesc(nonEmpty(), "Run that the task reproduces."),
    tags: withDesc(arr(nonEmpty()), "Free-form tags."),
    trackerSeed: withDesc({}, "The start state of the tracker for the trial. Any YAML value."),
    linearSeed: withDesc({}, "Older name of trackerSeed. Set only one of them."),
    startingRepoRefs: withDesc(
      arr(ref1),
      "Commits that the trial starts from. Each entry is a string github.com:OWNER/REPO@SHA or gitlab.com:OWNER/REPO@SHA, or an object {codeForge, owner, repo, ref}. codeForge is GITHUB or GITLAB. ref is a full 40-character lowercase commit SHA.",
    ),
  });
  return document("benchmark_suite_task", "Benchmark task", "benchmarks/<suite-slug>/tasks/<task-slug>.yaml.", t);
}

/** SCHEMA_NAMES lists the v1 schema documents (without ".schema.json"). */
export const SCHEMA_NAMES = [
  "factory",
  "agent",
  "automation",
  "scorer",
  "machine",
  "webhook",
  "benchmark_suite",
  "benchmark_suite_task",
  "common",
];

/** generatedSchemas builds every v1 schema document. */
export function generatedSchemas(): Record<string, Obj> {
  return {
    common: commonSchema(),
    factory: factorySchema(),
    agent: agentSchema(),
    automation: automationSchema(),
    scorer: scorerSchema(),
    machine: machineSchema(),
    webhook: webhookSchema(),
    benchmark_suite: benchmarkSuiteSchema(),
    benchmark_suite_task: benchmarkTaskSchema(),
  };
}

/** goJSONIndent encodes like Go's json.Encoder with SetIndent("", "  ") and no HTML escaping: sorted keys, trailing newline. */
export function goJSONIndent(v: unknown): string {
  const enc = (x: unknown, ind: string): string => {
    if (x === null || x === undefined) return "null";
    if (typeof x === "string") return JSON.stringify(x).replaceAll("\u2028", "\\u2028").replaceAll("\u2029", "\\u2029");
    if (typeof x !== "object") return JSON.stringify(x);
    const next = ind + "  ";
    if (Array.isArray(x)) {
      if (x.length === 0) return "[]";
      return "[\n" + x.map((e) => next + enc(e, next)).join(",\n") + "\n" + ind + "]";
    }
    const keys = Object.keys(x).sort();
    if (keys.length === 0) return "{}";
    return (
      "{\n" + keys.map((k) => next + JSON.stringify(k) + ": " + enc((x as Obj)[k], next)).join(",\n") + "\n" + ind + "}"
    );
  };
  return enc(v, "") + "\n";
}
