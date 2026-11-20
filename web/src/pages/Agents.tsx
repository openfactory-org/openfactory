import { useEffect, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError, unwrap, type Factory, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { fpath, useFactory } from "../components/FactoryLayout";
import { Alert, Badge, Button, Card, Empty, Field, PageHeader, Query, Tabs, ValidationList } from "../components/ui";
import { Bot, Plus } from "lucide-react";
import { useMutate } from "../lib/mutate";
import { useUsableSecrets } from "../lib/secrets";

type AgentSpec = Schemas["AgentSpec"];
type AgentConfig = Schemas["AgentConfig"];

export function ReadOnlyNotice({ f }: { f: Factory }) {
  if (f.source !== "git") return null;
  return (
    <Alert kind="info">
      This factory's definition lives in <code>{f.git_repo}</code>
      {f.git_branch ? ` (${f.git_branch})` : ""}. Change it through a pull request there; this page is read-only.
    </Alert>
  );
}

export function AgentsPage() {
  const f = useFactory();
  const q = useQuery({
    queryKey: ["agents", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/agents", { params: { path: { uid: f.id } } })).then((r) => (r.items ?? []) as AgentConfig[]),
  });
  return (
    <>
      <PageHeader
        title="Agents"
        crumbs={[{ label: f.name, to: fpath(f) }]}
        actions={
          f.source !== "git" && (
            <Link className="btn btn-secondary" to={`${fpath(f, "definition")}?new=1`}>
              <Plus size={14} aria-hidden="true" /> New agent file
            </Link>
          )
        }
      />
      <ReadOnlyNotice f={f} />
      <Query q={q}>
        {(agents) => {
          const sorted = [...agents].sort((a, b) => Number(b.type === "FOREMAN") - Number(a.type === "FOREMAN") || a.name.localeCompare(b.name));
          return sorted.length === 0 ? (
            <Empty icon={<Bot size={18} />} title="No agents">
              Apply a definition with at least one agent.
            </Empty>
          ) : (
            <ul className="list bleed" aria-label="Agents">
              {sorted.map((a) => (
                <li key={a.name}>
                  <div className="list-row">
                    <Bot size={15} className="muted" aria-hidden="true" />
                    <Link to={fpath(f, `agents/${encodeURIComponent(a.name)}`)} className="row-title" data-row style={{ color: "inherit" }}>
                      {a.name}
                      {a.description && (
                        <span className="muted" style={{ fontWeight: 400 }}>
                          {" "}
                          · {a.description}
                        </span>
                      )}
                    </Link>
                    <span className="row-meta">
                      {(a.type === "FOREMAN" || (a.type ?? "").toLowerCase() !== a.name.toLowerCase()) && (
                        <Badge tone={a.type === "FOREMAN" ? "info" : "neutral"}>{titleCase(a.type)}</Badge>
                      )}
                      <span className="hide-sm">{a.harness?.type}</span>
                      <span className="hide-sm faint">{a.harness?.model}</span>
                    </span>
                  </div>
                </li>
              ))}
            </ul>
          );
        }}
      </Query>
    </>
  );
}

type Form = Omit<AgentSpec, "name" | "path" | "has_file" | "editable" | "machines">;

export function AgentPage() {
  const f = useFactory();
  const { name = "" } = useParams();
  const { canEditFactory } = useAuth();
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "prompt" ? "prompt" : "settings";
  const specQ = useQuery({
    queryKey: ["agent-spec", f.id, name],
    queryFn: () => unwrap(api.GET("/factories/{uid}/agents/{name}/spec", { params: { path: { uid: f.id, name } } })),
  });
  const agentQ = useQuery({
    queryKey: ["agent", f.id, name],
    queryFn: () => unwrap(api.GET("/factories/{uid}/agents/{name}", { params: { path: { uid: f.id, name } } })) as Promise<AgentConfig>,
  });
  // GET /secrets?factory_id= returns only the factory's own secrets; agents can also use team secrets.
  const secrets = useUsableSecrets(f.id);
  const workers = useQuery({ queryKey: ["workers"], queryFn: () => unwrap(api.GET("/workers")).then((r) => r.items ?? []) });
  const envs = useQuery({ queryKey: ["environments"], queryFn: () => unwrap(api.GET("/environments")).then((r) => r.items ?? []) });

  const [form, setForm] = useState<Form | null>(null);
  const [extraSecrets, setExtraSecrets] = useState("");
  const [verrs, setVerrs] = useState<Schemas["ValidationError"][]>([]);
  useEffect(() => {
    if (specQ.data) {
      const { name: _n, path: _p, has_file: _h, editable: _e, machines: _r, ...rest } = specQ.data;
      setForm(rest);
    }
  }, [specQ.data]);

  const save = useMutate(
    (body: Schemas["AgentSpecUpdate"]) => unwrap(api.PUT("/factories/{uid}/agents/{name}/spec", { params: { path: { uid: f.id, name } }, body })),
    {
      invalidate: [["agent-spec", f.id, name], ["agent", f.id, name], ["agents", f.id], ["definition", f.id]],
      success: `Agent ${name} saved.`,
      quietErrors: true,
      onSuccess: () => {
        setVerrs([]);
        setExtraSecrets("");
      },
    },
  );

  const spec = specQ.data;
  const agent = agentQ.data;
  const secretScope = new Map((secrets.data ?? []).map((s) => [s.name, s.scope]));
  const editable = !!spec?.editable && canEditFactory(f.id);
  const set = <K extends keyof Form>(k: K, v: Form[K]) => setForm((p) => (p ? { ...p, [k]: v } : p));
  const explicitHarness = form?.harness_type === "claude" || form?.harness_type === "codex";

  async function submit() {
    if (!form) return;
    setVerrs([]);
    const extra = extraSecrets.split(/[\s,]+/).filter(Boolean);
    try {
      await save.mutateAsync({ ...form, secrets: Array.from(new Set([...(form.secrets ?? []), ...extra])) });
    } catch (err) {
      setVerrs(err instanceof ApiError && err.validationErrors.length ? err.validationErrors : [{ file: spec?.path ?? "", message: (err as Error).message }]);
    }
  }

  return (
    <Query q={specQ} notFound={{ title: "Agent not found", text: `This factory has no agent named "${name}".`, back: { label: "Go to agents", to: fpath(f, "agents") } }}>
      {(s) => (
        <>
          <PageHeader
            title={
              <>
                {s.name} {agent && <Badge>{agent.type}</Badge>}
              </>
            }
            crumbs={[
              { label: f.name, to: fpath(f) },
              { label: "Agents", to: fpath(f, "agents") },
            ]}
            subtitle={
              s.has_file ? (
                <>
                  Defined in <code>{s.path}</code>
                </>
              ) : (
                "Built-in agent (no agent file in the definition)."
              )
            }
          />
          <ReadOnlyNotice f={f} />
          <ValidationList errors={verrs} />
          <Tabs
            label="Agent sections"
            value={tab}
            onChange={(t) => setParams(t === "settings" ? {} : { tab: t }, { replace: true })}
            tabs={[
              { id: "settings", label: "Settings" },
              { id: "prompt", label: "Prompt" },
            ]}
          />
          {form && (
            <form
              className="stack"
              onSubmit={(e) => {
                e.preventDefault();
                void submit();
              }}
            >
              <fieldset disabled={!editable} className="plain-fieldset">
                {tab === "prompt" ? (
                  <Card title="Instructions">
                    <label className="sr-only" htmlFor="ag-prompt">
                      Instructions
                    </label>
                    <textarea id="ag-prompt" rows={24} className="mono" value={form.prompt} onChange={(e) => set("prompt", e.target.value)} />
                    <p className="hint">The Markdown body of the agent file. Settings are kept as they are.</p>
                  </Card>
                ) : (
                  <Card className="narrow">
                    <Field label="Description" htmlFor="ag-description">
                      <input id="ag-description" value={form.description} onChange={(e) => set("description", e.target.value)} />
                    </Field>
                    <div className="grid-2">
                      <Field label="Harness" htmlFor="ag-harness_type">
                        <select id="ag-harness_type" value={form.harness_type} onChange={(e) => set("harness_type", e.target.value as Form["harness_type"])}>
                          <option value="">Inherit (factory default{agent?.harness?.type ? `: ${agent.harness.type}` : ""})</option>
                          <option value="claude">Claude Code (claude)</option>
                          <option value="codex">Codex (codex)</option>
                        </select>
                      </Field>
                      <Field label="Model" htmlFor="ag-model">
                        <input id="ag-model" value={form.model} onChange={(e) => set("model", e.target.value)} placeholder={agent?.harness?.model ? `Inherited: ${agent.harness.model}` : "e.g. claude-sonnet-5-5"} />
                      </Field>
                      {explicitHarness && (
                        <>
                          <Field
                            label="Auth"
                            htmlFor="ag-auth_secret"
                            hint={
                              <>
                                Claude Code accepts an Anthropic API key, Bedrock API key or Bedrock access key; Codex accepts an OpenAI API key. <Link to="/secrets">New auth secret</Link>
                              </>
                            }
                          >
                            <select id="ag-auth_secret" value={form.auth_secret} onChange={(e) => set("auth_secret", e.target.value)}>
                              <option value="">None (inherit or worker environment)</option>
                              {(secrets.data ?? []).map((sec) => (
                                <option key={sec.id ?? sec.name} value={sec.name}>
                                  {sec.name} · {sec.type}
                                  {sec.scope === "factory" ? " · factory" : ""}
                                </option>
                              ))}
                              {form.auth_secret && secrets.data && !secrets.data.some((x) => x.name === form.auth_secret) && <option value={form.auth_secret}>Missing secret ({form.auth_secret})</option>}
                            </select>
                          </Field>
                          <Field label="Reasoning level" htmlFor="ag-reasoning_level">
                            <input id="ag-reasoning_level" value={form.reasoning_level} onChange={(e) => set("reasoning_level", e.target.value)} placeholder="e.g. high" />
                          </Field>
                        </>
                      )}
                      <Field label="Machine" htmlFor="ag-machine">
                        <select id="ag-machine" value={form.machine} onChange={(e) => set("machine", e.target.value)}>
                          <option value="">Inherit{agent?.machine?.name ? ` (${agent.machine.name})` : ""}</option>
                          {s.machines.map((r) => (
                            <option key={r} value={r}>
                              {r}
                            </option>
                          ))}
                          {form.machine && !s.machines.includes(form.machine) && <option value={form.machine}>Missing machine ({form.machine})</option>}
                        </select>
                      </Field>
                      <Field label="Host" htmlFor="ag-worker_host">
                        <input id="ag-worker_host" list="workers-list" value={form.worker_host} onChange={(e) => set("worker_host", e.target.value)} placeholder={agent?.worker_host ? `Inherited: ${agent.worker_host}` : "server"} />
                        <datalist id="workers-list">
                          <option value="server" />
                          {(workers.data ?? []).map((w) => (
                            <option key={w.id} value={w.id}>
                              {w.name}
                            </option>
                          ))}
                        </datalist>
                      </Field>
                      <Field label="Environment" htmlFor="ag-environment_id">
                        <select id="ag-environment_id" value={form.environment_id} onChange={(e) => set("environment_id", e.target.value)}>
                          <option value="">Inherit / managed from repositories</option>
                          {(envs.data ?? []).map((env) => (
                            <option key={env.id} value={env.id}>
                              {env.name}
                            </option>
                          ))}
                          {/* A deleted environment stays visible, so that the user can change it. */}
                          {form.environment_id && envs.data && !envs.data.some((env) => env.id === form.environment_id) && <option value={form.environment_id}>Missing environment ({form.environment_id})</option>}
                        </select>
                      </Field>
                      <Field label="Idle timeout (minutes)" htmlFor="ag-idle_timeout">
                        <input id="ag-idle_timeout" type="number" min={0} max={60} value={form.idle_timeout_minutes} onChange={(e) => set("idle_timeout_minutes", e.target.value)} placeholder="Inherit" />
                      </Field>
                    </div>
                    <fieldset className="field">
                      <legend>Secrets</legend>
                      <p className="hint">
                        Injected as environment variables with the secret's name. An agent-level list replaces <code>agentDefaults.secrets</code>.
                      </p>
                      <div className="checks-inline">
                        {Array.from(new Set([...(secrets.data ?? []).map((x) => x.name), ...(form.secrets ?? [])])).map((n) => (
                          <div key={n} className="check">
                            <input
                              id={`ag-secret-${n}`}
                              type="checkbox"
                              checked={(form.secrets ?? []).includes(n)}
                              onChange={(e) => set("secrets", e.target.checked ? [...(form.secrets ?? []), n] : (form.secrets ?? []).filter((x) => x !== n))}
                            />
                            <label htmlFor={`ag-secret-${n}`}>
                              {n}
                              {secretScope.get(n) === "factory" && <span className="muted small"> (factory)</span>}
                            </label>
                          </div>
                        ))}
                      </div>
                      <label htmlFor="ag-secrets_extra" className="small">
                        Other secret names (comma separated)
                      </label>
                      <input id="ag-secrets_extra" value={extraSecrets} onChange={(e) => setExtraSecrets(e.target.value)} />
                    </fieldset>
                    <Field
                      label="MCP servers"
                      htmlFor="ag-mcp_servers"
                      hint={
                        <>
                          YAML map of server name to transport (<code>managedId</code> with the name of a team MCP server, <code>command</code>/<code>args</code>/<code>env</code>, or <code>url</code>/<code>headers</code>). Reference secrets as{" "}
                          <code>{"{{SECRET_NAME}}"}</code>.
                        </>
                      }
                    >
                      <textarea id="ag-mcp_servers" rows={6} className="mono" value={form.mcp_servers} onChange={(e) => set("mcp_servers", e.target.value)} placeholder={"docs-search:\n  managedId: docs-search"} />
                    </Field>
                  </Card>
                )}
              </fieldset>
              {editable && (
                <div className="form-actions">
                  <Button type="submit" variant="primary" busy={save.isPending}>
                    Save and apply
                  </Button>
                </div>
              )}
            </form>
          )}
        </>
      )}
    </Query>
  );
}

/** "SELF_IMPROVEMENT" → "Self improvement". */
function titleCase(s?: string | null): string {
  const t = (s ?? "").toLowerCase().replace(/_/g, " ");
  return t.charAt(0).toUpperCase() + t.slice(1);
}
