// Dashboard edits of a managed definition (internal/api/dashboard.go): the
// agent spec form, automation files, factory settings and the integrations
// list. Each edit changes the active files and applies them as a new version.
import { OfError } from "@openfactory/core";
import type { AgentConfig } from "@openfactory/definition";
import { Document, isMap, isSeq, parseDocument, YAMLMap, YAMLSeq, Scalar } from "yaml";
import { bodyObject, optString, optStringArray, str, type JsonObject } from "../../../lib/body";
import { API_NAME_RE } from "../../../secrets/validate";
import { first } from "../../../lib/sql/query";
import { noContent, type TeamKit, type TeamOp } from "../kit";
import { applyFiles, validateFiles, validationError } from "./definitions";
import {
  activeFiles,
  activeMachines,
  activeRuntime,
  agentFile,
  automationFile,
  factoryFor,
  getFactoryById,
  getFactoryIntegrations,
  listAutomations,
  setFactoryIntegrations,
  type FactoryRecord,
} from "./repo";
import {
  frontGet,
  mdString,
  nodeString,
  parseMarkdownDoc,
  setOrDelete,
  yamlMapping,
  yamlText,
  type MdDoc,
} from "./yaml-edit";

// ---------- agent spec ----------

export interface AgentSpec {
  name: string;
  path: string;
  has_file: boolean;
  editable: boolean;
  description: string;
  harness_type: string;
  model: string;
  reasoning_level: string;
  auth_secret: string;
  machine: string;
  worker_host: string;
  environment_id: string;
  secrets: string[];
  mcp_servers: string;
  idle_timeout_minutes: string;
  prompt: string;
  machines: string[];
}

function fillAgentSpec(s: AgentSpec, d: MdDoc): void {
  const f = d.front;
  s.description = nodeString(frontGet(f, ["description"]));
  s.model = nodeString(frontGet(f, ["model"]));
  s.machine = nodeString(frontGet(f, ["machine"]));
  s.worker_host = nodeString(frontGet(f, ["workerHost"]));
  s.environment_id = nodeString(frontGet(f, ["environmentId"]));
  const sec = frontGet(f, ["secrets"]);
  if (isSeq(sec)) s.secrets = sec.items.map((x) => nodeString(x));
  const mcp = frontGet(f, ["mcpServers"]);
  if (mcp) s.mcp_servers = new Document(mcp).toString({ indent: 2, lineWidth: 0 });
  s.idle_timeout_minutes = nodeString(frontGet(f, ["idleTimeoutMinutes"]));
  s.prompt = d.body.replace(/^\n+/, "");
  const h = frontGet(f, ["harness"]);
  if (isMap(h)) {
    s.harness_type = nodeString(h.get("type", true));
    if (s.harness_type === "claude-code") s.harness_type = "claude";
    s.model = nodeString(h.get("model", true));
    s.reasoning_level = nodeString(h.get("reasoningLevel", true));
    const a = h.get("auth", true);
    if (isMap(a)) s.auth_secret = nodeString(a.get("secretName", true));
  }
}

function applyAgentSpec(d: MdDoc, u: JsonObject): void {
  const f = d.front;
  const s = (k: string) => str(u, k);
  setOrDelete(f, ["description"], s("description"));
  switch (s("harness_type")) {
    case "":
      f.delete("harness");
      setOrDelete(f, ["model"], s("model"));
      break;
    case "claude":
    case "codex": {
      f.delete("model");
      if (!isMap(f.get("harness", true))) f.set("harness", new YAMLMap());
      f.setIn(["harness", "type"], s("harness_type"));
      setOrDelete(f, ["harness", "model"], s("model"));
      setOrDelete(f, ["harness", "reasoningLevel"], s("reasoning_level"));
      if (s("auth_secret").trim() !== "") {
        const a = new YAMLMap();
        a.set("source", "managedSecret");
        a.set("secretName", s("auth_secret").trim());
        f.setIn(["harness", "auth"], a);
      } else f.deleteIn(["harness", "auth"]);
      break;
    }
    default:
      throw new OfError(
        "invalid_request",
        `unknown harness ${JSON.stringify(s("harness_type"))} (use claude or codex)`,
      );
  }
  setOrDelete(f, ["machine"], s("machine"));
  setOrDelete(f, ["workerHost"], s("worker_host"));
  setOrDelete(f, ["environmentId"], s("environment_id"));
  const secrets: string[] = [];
  for (const raw of optStringArray(u, "secrets") ?? []) {
    const sn = raw.trim();
    if (sn === "" || secrets.includes(sn)) continue;
    if (!API_NAME_RE.test(sn)) throw new OfError("invalid_request", `invalid secret name ${JSON.stringify(sn)}`);
    secrets.push(sn);
  }
  if (secrets.length) {
    const seq = new YAMLSeq();
    for (const x of secrets) seq.add(new Scalar(x));
    f.set("secrets", seq);
  } else f.delete("secrets");
  if (s("mcp_servers").trim() !== "") {
    const m = parseDocument(s("mcp_servers"));
    if (m.errors.length || !isMap(m.contents)) {
      throw new OfError("invalid_request", `mcpServers: ${m.errors[0]?.message ?? "must be a YAML mapping"}`);
    }
    f.set("mcpServers", m.contents);
  } else f.delete("mcpServers");
  const t = s("idle_timeout_minutes").trim();
  if (t !== "") {
    const n = /^[+-]?\d+$/.test(t) ? Number(t) : NaN;
    if (!Number.isInteger(n) || n < 0 || n > 60)
      throw new OfError("invalid_request", "idle_timeout_minutes must be 0 through 60");
    f.set("idleTimeoutMinutes", n);
  } else f.delete("idleTimeoutMinutes");
  const prompt = optString(u, "prompt");
  if (prompt !== undefined) d.body = prompt.replace(/\r\n/g, "\n");
}

async function agentSpec(
  kit: TeamKit,
  f: FactoryRecord,
  name: string,
): Promise<{ spec: AgentSpec; files: Record<string, string> }> {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? AND name = ?", f.id, name);
  if (!r) throw new OfError("resource_not_found", `agent ${name} not found`);
  const agent = JSON.parse(r.data) as AgentConfig;
  const files = await activeFiles(kit, f);
  const spec: AgentSpec = {
    name: agent.name,
    path: agentFile(name),
    has_file: false,
    editable: false,
    description: agent.description,
    harness_type: "",
    model: "",
    reasoning_level: "",
    auth_secret: "",
    machine: "",
    worker_host: "",
    environment_id: "",
    secrets: [],
    mcp_servers: "",
    idle_timeout_minutes: "",
    prompt: agent.instructions,
    machines: [],
  };
  const src = files[spec.path];
  if (src !== undefined) {
    spec.has_file = true;
    try {
      fillAgentSpec(spec, parseMarkdownDoc(src));
    } catch {
      /* keep the compiled values */
    }
  }
  spec.editable = spec.has_file && f.source !== "git";
  // The machine names of the active runtime (written with the active version).
  spec.machines = Object.keys(activeMachines(kit, f.id) ?? {}).sort();
  return { spec, files };
}

// ---------- automation files ----------

const RESOURCE_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$/;

async function automationFiles(
  kit: TeamKit,
  f: FactoryRecord,
  name: string,
  content0: string,
): Promise<{ files: Record<string, string>; path: string }> {
  if (!RESOURCE_NAME_RE.test(name) || name === "new") {
    throw new OfError("invalid_request", `automation names use letters, numbers, '-' and '_' (and cannot be "new")`);
  }
  const content = content0.replace(/\r\n/g, "\n");
  if (content.trim() === "") throw new OfError("invalid_request", "content is empty");
  if (content.length > 512 * 1024) throw new OfError("invalid_request", "content is larger than 512 KB");
  const path = automationFile(name);
  try {
    parseMarkdownDoc(content);
  } catch (e) {
    throw validationError("automation file cannot be parsed", [{ file: path, message: (e as Error).message }]);
  }
  const files = await activeFiles(kit, f);
  files[path] = content;
  return { files, path };
}

export const INTEGRATION_TYPES = [
  { type: "slack", label: "Slack", hint: "Mention the foreman in channels and threads." },
  { type: "microsoft-teams", label: "Microsoft Teams", hint: "Mention the foreman in Teams channels." },
  { type: "linear", label: "Linear", hint: "Assign or mention the foreman on Linear issues." },
  { type: "jira", label: "Jira", hint: "Route Jira issues to the factory." },
];

// ---------- ops ----------

export const dashboardOps: Record<string, TeamOp> = {
  putFactoryIntegrations: async (kit, c) => {
    const f = factoryFor(kit, c);
    const b = bodyObject(c.body);
    const req = optStringArray(b, "integrations") ?? [];
    for (const t of req)
      if (!INTEGRATION_TYPES.some((x) => x.type === t))
        throw new OfError("invalid_request", `unknown integration ${JSON.stringify(t)}`);
    const providers = INTEGRATION_TYPES.map((x) => x.type).filter((t) => req.includes(t));
    if (providers.includes("linear") && providers.includes("jira")) {
      throw new OfError("invalid_request", "declare at most one issue tracker: linear and jira are mutually exclusive");
    }
    if (f.source === "git")
      throw new OfError(
        "operation_not_supported",
        "integrations of a git-backed factory are declared in its factory.yaml",
      );
    if (f.active_version_id) {
      const files = await activeFiles(kit, f);
      let fy: string;
      try {
        const doc = yamlMapping(files["factory.yaml"] ?? "");
        const existing = new Map<string, unknown>();
        const seq0 = doc.get("integrations", true);
        if (isSeq(seq0))
          for (const item of seq0.items) if (isMap(item)) existing.set(nodeString(item.get("type", true)), item);
        const seq = new YAMLSeq();
        for (const p of providers) {
          if (existing.has(p)) seq.add(existing.get(p));
          else {
            const m = new YAMLMap();
            m.set("type", p);
            seq.add(m);
          }
        }
        doc.set("integrations", seq);
        fy = yamlText(doc);
      } catch (e) {
        throw new OfError("invalid_request", `factory.yaml: ${e instanceof OfError ? e.detail : (e as Error).message}`);
      }
      files["factory.yaml"] = fy;
      await applyFiles(kit, c, f, files, "Update integrations");
    }
    kit.tx(() => {
      const current = getFactoryIntegrations(kit, f.id);
      setFactoryIntegrations(kit, f.id, providers, current);
    });
    return { factory_id: f.id, integrations: getFactoryIntegrations(kit, f.id) };
  },

  getFactorySettings: async (kit, c) => {
    const f = factoryFor(kit, c);
    const out = {
      factory_id: f.id,
      editable: f.source !== "git",
      credential_strategy: "EXECUTOR",
      repositories: [] as unknown[],
      machines: [] as unknown[],
      integration_types: INTEGRATION_TYPES,
    };
    // The runtime row holds these values of the active version, compiled at apply.
    const rt = activeRuntime(kit, f.id);
    if (rt) {
      if (rt.credential_strategy) out.credential_strategy = rt.credential_strategy;
      if (rt.repositories) out.repositories = rt.repositories;
      const machines = (rt.machines ?? {}) as Record<string, JsonObject>;
      for (const n of Object.keys(machines).sort()) {
        const rs = { ...machines[n] };
        if (!rs["name"]) rs["name"] = n;
        out.machines.push(rs);
      }
    }
    return out;
  },

  getAgentSpec: async (kit, c) => (await agentSpec(kit, factoryFor(kit, c), c.params["name"]!)).spec,

  putAgentSpec: async (kit, c) => {
    const f = factoryFor(kit, c);
    const name = c.params["name"]!;
    if (f.source === "git") {
      throw new OfError(
        "operation_not_supported",
        `this factory's definition lives in ${f.git_repo}: edit ${agentFile(name)} through a pull request`,
      );
    }
    const u = bodyObject(c.body);
    const { spec, files } = await agentSpec(kit, f, name);
    if (!spec.has_file)
      throw new OfError("operation_not_supported", `agent ${name} has no ${spec.path} in the active definition`);
    let doc: MdDoc;
    try {
      doc = parseMarkdownDoc(files[spec.path]!);
    } catch (e) {
      throw validationError("agent file cannot be parsed", [{ file: spec.path, message: (e as Error).message }]);
    }
    applyAgentSpec(doc, u);
    files[spec.path] = mdString(doc);
    await applyFiles(kit, c, f, files, str(u, "message").trim() || "Update agent " + name);
    return (await agentSpec(kit, getFactoryById(kit, f.id)!, name)).spec;
  },

  getAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    const name = c.params["name"]!;
    const files = await activeFiles(kit, f);
    const path = automationFile(name);
    const content = files[path];
    if (content === undefined) throw new OfError("resource_not_found", `automation ${name} not found`);
    const au = listAutomations(kit, f.id).find((a) => a.name === name);
    return { name, path, content, editable: f.source !== "git", ...(au ? { automation: au } : {}) };
  },

  putAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    if (f.source === "git")
      throw new OfError(
        "operation_not_supported",
        `automations of a git-backed factory change through a pull request in ${f.git_repo}`,
      );
    const b = bodyObject(c.body);
    const name = c.params["name"]!;
    const { files, path } = await automationFiles(kit, f, name, str(b, "content"));
    await applyFiles(kit, c, f, files, str(b, "message").trim() || "Save automation " + name);
    const au = listAutomations(kit, f.id).find((a) => a.name === name);
    return { name, path, content: files[path], editable: true, ...(au ? { automation: au } : {}) };
  },

  deleteAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    if (f.source === "git")
      throw new OfError(
        "operation_not_supported",
        `automations of a git-backed factory change through a pull request in ${f.git_repo}`,
      );
    const name = c.params["name"]!;
    const files = await activeFiles(kit, f);
    const prefix = `automations/${name}/`;
    let found = false;
    for (const k of Object.keys(files)) {
      if (k.startsWith(prefix)) {
        delete files[k];
        found = true;
      }
    }
    if (!found) throw new OfError("resource_not_found", `automation ${name} not found`);
    await applyFiles(kit, c, f, files, "Delete automation " + name);
    return noContent();
  },

  validateAutomation: async (kit, c) => {
    const f = factoryFor(kit, c);
    const b = bodyObject(c.body);
    try {
      const { files } = await automationFiles(kit, f, c.params["name"]!, str(b, "content"));
      return await validateFiles(kit, f, files);
    } catch (e) {
      if (e instanceof OfError && Array.isArray(e.extra?.["errors"]))
        return { valid: false, errors: e.extra!["errors"] };
      throw e;
    }
  },
};
