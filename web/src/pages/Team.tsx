import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useQueries, useQuery } from "@tanstack/react-query";
import { api, errorMessage, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { DeadEvents } from "../components/DeadEvents";
import { Boxes, KeyRound, Plug, Plus, Server } from "lucide-react";
import { confirm, Dialog } from "../components/overlays";
import { useFactories } from "../lib/queries";
import { Alert, Badge, Button, Card, Empty, Field, PageHeader, Query, TimeAgo } from "../components/ui";
import { kvLines, lines, parseKVLines } from "../lib/format";
import { useMutate } from "../lib/mutate";
import { factorySecretsQuery, teamSecretsQuery } from "../lib/secrets";

type Secret = Schemas["Secret"];

const SECRET_TYPES: { id: NonNullable<Schemas["SecretRequest"]["type"]>; label: string }[] = [
  { id: "generic", label: "Generic" },
  { id: "anthropic_api_key", label: "Anthropic API key" },
  { id: "anthropic_oauth_token", label: "Claude Code OAuth token" },
  { id: "anthropic_bedrock_api_key", label: "Bedrock API key" },
  { id: "anthropic_bedrock_access_key", label: "Bedrock access key" },
  { id: "openai_api_key", label: "OpenAI API key" },
  { id: "docker_registry", label: "Docker registry" },
  { id: "aws_ecr_credential", label: "AWS ECR credential" },
];
const typeLabel = (t?: string) => SECRET_TYPES.find((x) => x.id === t)?.label ?? t ?? "";

// ---------- secrets ----------

export function SecretsPage() {
  const { canEdit } = useAuth();
  const factories = useFactories();
  const [params] = useSearchParams();
  const team = useQuery(teamSecretsQuery);
  const perFactory = useQueries({ queries: (factories.data ?? []).map((f) => factorySecretsQuery(f.id)) });
  const [replacing, setReplacing] = useState<Secret | null>(null);
  const [form, setForm] = useState({ name: "", type: "generic", value: "", factory_id: params.get("factory") ?? "" });
  const [error, setError] = useState<string | null>(null);
  const create = useMutate((body: Schemas["SecretRequest"]) => unwrap(api.POST("/secrets", { body })), {
    invalidate: [["secrets"]],
    quietErrors: true,
    success: (s) => `Secret ${s.name} saved.`,
    onSuccess: () => {
      setForm((f) => ({ ...f, name: "", value: "" }));
      setOpen(false);
    },
  });
  const [open, setOpen] = useState(!!params.get("factory"));
  const del = useMutate(
    (s: Secret) => unwrap(api.DELETE("/secrets/{name}", { params: { path: { name: s.name }, query: { factory_id: s.factory_id || undefined } } })),
    { invalidate: [["secrets"]], success: "Secret deleted." },
  );

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!/^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(form.name)) return setError("Secret names use letters, digits and underscores, and can't start with a digit.");
    if (!form.value) return setError("Enter the secret's value.");
    try {
      await create.mutateAsync({ name: form.name, type: form.type as Schemas["SecretRequest"]["type"], value: form.value, factory_id: form.factory_id || undefined });
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  const groups: { title: string; secrets: Secret[] }[] = [{ title: "Team", secrets: (team.data ?? []).filter((s) => !s.factory_id) }];
  (factories.data ?? []).forEach((f, i) => {
    const list = perFactory[i]?.data ?? [];
    if (list.length) groups.push({ title: `Factory · ${f.name}`, secrets: list });
  });

  return (
    <>
      <PageHeader
        title="Secrets"
        subtitle="Encrypted values agents receive as environment variables. Values are never shown again after saving."
        actions={
          canEdit && (
            <Button variant="primary" onClick={() => setOpen(true)}>
              <Plus size={14} aria-hidden="true" /> New secret
            </Button>
          )
        }
      />
      <div className="stack">
        <div className="stack">
          <Query q={team}>
            {() => (
              <>
                {groups.every((g) => g.secrets.length === 0) && (
                  <Empty
                    icon={<KeyRound size={18} />}
                    title="No secrets yet"
                    action={
                      canEdit && (
                        <Button variant="primary" onClick={() => setOpen(true)}>
                          New secret
                        </Button>
                      )
                    }
                  >
                    Add a model credential or a token that agents need. A run reads it as an environment variable.
                  </Empty>
                )}
                {groups.filter((g) => g.secrets.length > 0).map((g) => (
                  <Card key={g.title} title={g.title}>
                    {(
                      <table className="table">
                        <thead>
                          <tr>
                            <th scope="col">Name</th>
                            <th scope="col">Type</th>
                            <th scope="col">Updated</th>
                            <th scope="col">
                              <span className="sr-only">Actions</span>
                            </th>
                          </tr>
                        </thead>
                        <tbody>
                          {g.secrets.map((s) => (
                            <tr key={s.id ?? `${s.factory_id}/${s.name}`}>
                              <td>
                                <code>{s.name}</code>
                              </td>
                              <td>{typeLabel(s.type)}</td>
                              <td>
                                <TimeAgo t={s.updated_at ?? s.created_at} />
                              </td>
                              <td className="right">
                                {canEdit && (
                                  <Button variant="ghost" onClick={() => setReplacing(s)}>
                                    Replace value
                                  </Button>
                                )}
                                {canEdit && (
                                  <Button variant="ghost" onClick={() => void confirm({ title: `Delete secret ${s.name}?`, body: "Agents that use it will fail. You cannot undo this.", confirmLabel: "Delete secret", danger: true }).then((ok) => ok && del.mutate(s))}>
                                    Delete
                                  </Button>
                                )}
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    )}
                  </Card>
                ))}
              </>
            )}
          </Query>
        </div>
        {canEdit && (
          <Dialog open={open} onOpenChange={setOpen} title="New secret" description="The value is encrypted. You cannot read it again after you save it.">
            <form className="stack stack-sm" onSubmit={submit} autoComplete="off">
              {error && <Alert kind="error">{error}</Alert>}
              <Field label="Name" htmlFor="s-name">
                <input id="s-name" className="mono" required value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="GITHUB_TOKEN" />
              </Field>
              <Field label="Type" htmlFor="s-type">
                <select id="s-type" value={form.type} onChange={(e) => setForm({ ...form, type: e.target.value })}>
                  {SECRET_TYPES.map((t) => (
                    <option key={t.id} value={t.id}>
                      {t.label}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="Value" htmlFor="s-value">
                <textarea id="s-value" rows={3} className="mono" required value={form.value} onChange={(e) => setForm({ ...form, value: e.target.value })} />
              </Field>
              <Field label="Scope" htmlFor="s-scope" hint="A factory secret overrides a team secret with the same name.">
                <select id="s-scope" value={form.factory_id} onChange={(e) => setForm({ ...form, factory_id: e.target.value })}>
                  <option value="">Team (every factory)</option>
                  {(factories.data ?? []).map((f) => (
                    <option key={f.id} value={f.id}>
                      {f.name}
                    </option>
                  ))}
                </select>
              </Field>
              <div className="dialog-footer">
                <Button onClick={() => setOpen(false)}>Cancel</Button>
                <Button type="submit" variant="primary" busy={create.isPending}>
                  Save secret
                </Button>
              </div>
            </form>
          </Dialog>
        )}
        {canEdit && <ReplaceSecretDialog secret={replacing} onClose={() => setReplacing(null)} />}
      </div>
    </>
  );
}

/** Replaces the value of an existing secret. The name, type and scope stay the same. */
function ReplaceSecretDialog({ secret, onClose }: { secret: Secret | null; onClose: () => void }) {
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setValue("");
    setError(null);
  }, [secret]);
  const update = useMutate(
    (s: Secret) => unwrap(api.PUT("/secrets/{name}", { params: { path: { name: s.name }, query: { factory_id: s.factory_id || undefined } }, body: { value, factory_id: s.factory_id || undefined } })),
    {
      invalidate: [["secrets"]],
      quietErrors: true,
      success: (s) => `Secret ${s.name} updated.`,
      onSuccess: () => {
        setValue("");
        onClose();
      },
    },
  );

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!secret) return;
    setError(null);
    if (!value) return setError("Enter the new value.");
    try {
      await update.mutateAsync(secret);
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  return (
    <Dialog
      open={!!secret}
      onOpenChange={(o) => !o && onClose()}
      title={secret ? `Replace value of ${secret.name}` : "Replace value"}
      description="The new value is encrypted and replaces the old one. New runs receive the new value."
    >
      <form className="stack stack-sm" onSubmit={submit} autoComplete="off">
        {error && <Alert kind="error">{error}</Alert>}
        <Field label="New value" htmlFor="s-new-value">
          <input id="s-new-value" type="password" className="mono" required autoComplete="new-password" value={value} onChange={(e) => setValue(e.target.value)} />
        </Field>
        <div className="dialog-footer">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" busy={update.isPending}>
            Save value
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// ---------- integrations ----------

const PROVIDERS = ["github", "gitlab", "azure_devops", "slack", "microsoft_teams", "linear", "jira"] as const;
/** FORGES are the providers whose record serves one owner, config.org. */
const FORGES: readonly string[] = ["github", "gitlab", "azure_devops"];
/** ORG_HINT names the owner that config.org holds for each forge. */
const ORG_HINT: Record<string, string> = {
  github: "The organization or user name that owns the repositories.",
  gitlab: "The top-level group. One record serves the group and its subgroups.",
  azure_devops: "The organization.",
};
/** CONFIG_EXAMPLE is the placeholder of the other settings of a provider. */
const CONFIG_EXAMPLE: Record<string, string> = {
  github: "app_id=12345\ninstallation_id=67890\napp_slug=acme-factory",
  gitlab: "base_url=https://gitlab.example.com",
  azure_devops: "base_url=https://dev.azure.com",
};

const INTEGRATION_STATUS: Record<string, { label: string; tone: string }> = {
  active: { label: "Active", tone: "ok" },
  disabled: { label: "Disabled", tone: "warn" },
  auth_failed: { label: "Auth failed", tone: "bad" },
};

// IntegrationStatus shows the status of an integration record. An empty
// status means active.
export function IntegrationStatus({ status, reason }: { status?: string; reason?: string }) {
  const s = INTEGRATION_STATUS[status || "active"] ?? { label: status ?? "", tone: "neutral" };
  return (
    <span title={reason || undefined}>
      <Badge tone={s.tone}>{s.label}</Badge>
      {reason && status !== "active" ? <span className="muted small"> {reason}</span> : null}
    </span>
  );
}

// linkProvider maps an integration provider to the provider name of an
// account link.
function linkProvider(provider: string): string {
  return provider === "microsoft_teams" ? "teams" : provider;
}

// LinkAccounts lists the providers whose sign-in is configured. A link
// proves with the sign-in of the provider that you control the account.
function LinkAccounts({ items }: { items: Schemas["Integration"][] }) {
  const providers = Array.from(new Set(items.filter((i) => i.config?.oauth_client_id && (i.status ?? "active") === "active").map((i) => linkProvider(i.provider))));
  if (providers.length === 0) return null;
  return (
    <Card title="Link your account">
      <p className="muted small">Link your account at a provider to your Open Factory account. You sign in to the provider to show that the account is yours.</p>
      <ul>
        {providers.map((p) => (
          <li key={p}>
            <a href={`/integrations/link-start/${encodeURIComponent(p)}`}>Link your {p} account</a>
          </li>
        ))}
      </ul>
    </Card>
  );
}

export function IntegrationsPage() {
  const { isAdmin } = useAuth();
  const [params] = useSearchParams();
  const factories = useFactories();
  const factoryName = (id?: string) => (id ? (factories.data ?? []).find((f) => f.id === id)?.name ?? id : "All factories");
  // The status changes on a webhook of the provider. A refresh every 30
  // seconds shows a removed app within one minute.
  const q = useQuery({ queryKey: ["integrations"], queryFn: () => unwrap(api.GET("/integrations")).then((r) => r.items ?? []), refetchInterval: 30_000 });
  const preset = params.get("provider")?.replace(/-/g, "_") ?? "";
  const [form, setForm] = useState({
    provider: (PROVIDERS as readonly string[]).includes(preset) ? preset : "github",
    name: params.get("connection") ?? "",
    org: "",
    config: "",
    secret_ref: "",
    factory_id: "",
  });
  const [error, setError] = useState<string | null>(null);
  const create = useMutate((body: Schemas["IntegrationRequest"]) => unwrap(api.POST("/integrations", { body })), {
    invalidate: [["integrations"]],
    quietErrors: true,
    success: (i) => `Connected ${i.name || i.provider}.`,
    onSuccess: () => {
      setForm((f) => ({ ...f, name: "", org: "", config: "", secret_ref: "", factory_id: "" }));
      setOpen(false);
    },
  });
  const [open, setOpen] = useState(!!preset);
  const setStatus = useMutate(
    ({ id, status }: { id: string; status: "active" | "disabled" }) => unwrap(api.PATCH("/integrations/{id}", { params: { path: { id } }, body: { status } })),
    { invalidate: [["integrations"]], success: "Integration updated." },
  );
  const del = useMutate((id: string) => unwrap(api.DELETE("/integrations/{id}", { params: { path: { id } } })), {
    invalidate: [["integrations"]],
    success: "Integration removed.",
  });

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    let config: Record<string, string>;
    try {
      config = parseKVLines(form.config);
    } catch (err) {
      return setError(`Configuration: ${(err as Error).message}`);
    }
    if (FORGES.includes(form.provider)) {
      const org = form.org.trim();
      if (!org || org.includes("/")) return setError("Owner: give one name, without a slash.");
      config = { ...config, org };
    }
    try {
      await create.mutateAsync({ provider: form.provider as Schemas["IntegrationRequest"]["provider"], name: form.name.trim() || undefined, config, secret_ref: form.secret_ref.trim() || undefined, factory_id: form.factory_id || undefined });
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  return (
    <>
      <PageHeader
        title="Integrations"
        subtitle="Team connections to code hosts, chat and issue trackers. Attach chat and tracker providers to a factory from its Integrations page."
        actions={
          isAdmin && (
            <Button variant="primary" onClick={() => setOpen(true)}>
              <Plus size={14} aria-hidden="true" /> Connect a provider
            </Button>
          )
        }
      />
      {q.data && <LinkAccounts items={q.data} />}
      <div className="stack">
        <Query q={q}>
          {(items) =>
            items.length === 0 ? (
              <Empty
                icon={<Plug size={18} />}
                title="No integrations connected"
                action={
                  isAdmin ? (
                    <Button variant="primary" onClick={() => setOpen(true)}>
                      Connect a provider
                    </Button>
                  ) : undefined
                }
              >
                {isAdmin ? "Connect a code host first. Then add chat and issue trackers." : "Only team admins can connect or remove integrations."}
              </Empty>
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">Name</th>
                      <th scope="col">Provider</th>
                      <th scope="col">Factory</th>
                      <th scope="col">Status</th>
                      <th scope="col">Configuration</th>
                      <th scope="col">Token secret</th>
                      <th scope="col">
                        <span className="sr-only">Actions</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {items.map((i) => (
                      <tr key={i.id}>
                        <td>{i.name}</td>
                        <td>{i.provider}</td>
                        <td>{factoryName(i.factory_id)}</td>
                        <td>
                          <IntegrationStatus status={i.status} reason={i.status_reason} />
                        </td>
                        <td className="small cell-wrap">
                          {Object.entries(i.config ?? {}).map(([k, v]) => (
                            <code key={k}>
                              {k}={v}{" "}
                            </code>
                          ))}
                        </td>
                        <td className="cell-wrap">{i.secret_ref ? <code>{i.secret_ref}</code> : "—"}</td>
                        <td className="right nowrap">
                          {isAdmin && (
                            <>
                              {(i.status ?? "active") === "active" ? (
                                <Button variant="ghost" onClick={() => setStatus.mutate({ id: i.id, status: "disabled" })}>
                                  Disable
                                </Button>
                              ) : (
                                <Button variant="ghost" onClick={() => setStatus.mutate({ id: i.id, status: "active" })}>
                                  Enable
                                </Button>
                              )}
                              <Button variant="ghost" onClick={() => void confirm({ title: `Disconnect ${i.name || i.provider}?`, body: "Factories that use this connection stop receiving its events.", confirmLabel: "Disconnect", danger: true }).then((ok) => ok && del.mutate(i.id))}>
                                Remove
                              </Button>
                            </>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )
          }
        </Query>
        {isAdmin && (
          <Dialog open={open} onOpenChange={setOpen} title="Connect a provider">
            <form className="stack stack-sm" onSubmit={submit}>
              {error && <Alert kind="error">{error}</Alert>}
              <Field label="Provider" htmlFor="i-prov">
                <select id="i-prov" value={form.provider} onChange={(e) => setForm({ ...form, provider: e.target.value })}>
                  {PROVIDERS.map((p) => (
                    <option key={p} value={p}>
                      {p}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="Factory" htmlFor="i-fac" hint="A record for one factory wins over the team record. Its token reaches only what you allow it at the provider.">
                <select id="i-fac" value={form.factory_id} onChange={(e) => setForm({ ...form, factory_id: e.target.value })}>
                  <option value="">All factories (team record)</option>
                  {(factories.data ?? []).map((f) => (
                    <option key={f.id} value={f.id}>
                      {f.name}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="Name" htmlFor="i-name">
                <input id="i-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="acme GitHub" />
              </Field>
              {FORGES.includes(form.provider) && (
                <Field label="Owner" htmlFor="i-org" hint={<>{ORG_HINT[form.provider]} Stored as <code>config.org</code>. A token for two owners needs two records.</>}>
                  <input id="i-org" className="mono" required value={form.org} onChange={(e) => setForm({ ...form, org: e.target.value })} placeholder="acme" />
                </Field>
              )}
              <Field label="Configuration" htmlFor="i-cfg" hint={<>Other non-secret settings, one <code>KEY=VALUE</code> per line.</>}>
                <textarea id="i-cfg" rows={4} className="mono" value={form.config} onChange={(e) => setForm({ ...form, config: e.target.value })} placeholder={CONFIG_EXAMPLE[form.provider] ?? "KEY=VALUE"} />
              </Field>
              <Field label="Token secret" htmlFor="i-sec" hint={<>Name of a secret holding the provider credential. <Link to="/secrets">Manage secrets</Link></>}>
                <input id="i-sec" className="mono" value={form.secret_ref} onChange={(e) => setForm({ ...form, secret_ref: e.target.value })} placeholder="GITHUB_APP_PRIVATE_KEY" />
              </Field>
              <div className="dialog-footer">
                <Button onClick={() => setOpen(false)}>Cancel</Button>
                <Button type="submit" variant="primary" busy={create.isPending}>
                  Connect
                </Button>
              </div>
            </form>
          </Dialog>
        )}
      </div>
    </>
  );
}

// ---------- environments ----------

export function EnvironmentsPage() {
  const { canEdit } = useAuth();
  const q = useQuery({ queryKey: ["environments"], queryFn: () => unwrap(api.GET("/environments")).then((r) => r.items ?? []) });
  return (
    <>
      <PageHeader
        title="Environments"
        subtitle="Reusable sandbox images, repositories and setup commands that agents run in."
        actions={
          canEdit && (
            <Link className="btn btn-primary" to="/environments/new">
              <Plus size={14} aria-hidden="true" /> New environment
            </Link>
          )
        }
      />
      <Query q={q}>
        {(envs) =>
          envs.length === 0 ? (
            <Empty icon={<Boxes size={18} />} title="No environments">
              Agents run in a managed environment built from the factory's repositories.
            </Empty>
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Image</th>
                    <th scope="col">Repositories</th>
                    <th scope="col">ID</th>
                  </tr>
                </thead>
                <tbody>
                  {envs.map((e) => (
                    <tr key={e.id}>
                      <td>
                        <Link to={`/environments/${encodeURIComponent(e.id)}`}>{e.name}</Link>
                        {e.description && <div className="muted small">{e.description}</div>}
                      </td>
                      <td>
                        {e.image ? <code className="small">{e.image}</code> : <span className="muted small">Server default</span>}
                      </td>
                      <td className="small">{(e.repos ?? []).join(", ")}</td>
                      <td>
                        <code className="small">{e.id}</code>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )
        }
      </Query>
    </>
  );
}

export function EnvironmentPage() {
  const { id = "" } = useParams();
  const isNew = id === "new";
  const navigate = useNavigate();
  const { canEdit } = useAuth();
  const q = useQuery({ queryKey: ["environment", id], enabled: !isNew, queryFn: () => unwrap(api.GET("/environments/{id}", { params: { path: { id } } })) });
  const [form, setForm] = useState({ name: "", description: "", image: "", repos: "", setup_commands: "", env_vars: "" });
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const e = q.data;
    if (e)
      setForm({
        name: e.name,
        description: e.description ?? "",
        image: e.image ?? "",
        repos: (e.repos ?? []).join("\n"),
        setup_commands: (e.setup_commands ?? []).join("\n"),
        env_vars: kvLines(e.env_vars),
      });
  }, [q.data]);
  const save = useMutate(
    (body: Schemas["EnvironmentRequest"]) =>
      isNew ? unwrap(api.POST("/environments", { body })) : unwrap(api.PATCH("/environments/{id}", { params: { path: { id } }, body })),
    {
      invalidate: [["environments"], ["environment", id]],
      quietErrors: true,
      success: (e) => `Environment ${e.name} saved.`,
      onSuccess: (e) => isNew && navigate(`/environments/${encodeURIComponent(e.id)}`, { replace: true }),
    },
  );
  const del = useMutate(() => unwrap(api.DELETE("/environments/{id}", { params: { path: { id } } })), {
    invalidate: [["environments"]],
    success: "Environment deleted.",
    onSuccess: () => navigate("/environments"),
  });

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!form.name.trim()) return setError("Enter a name.");
    let env_vars: Record<string, string>;
    try {
      env_vars = parseKVLines(form.env_vars);
    } catch (err) {
      return setError(`Environment variables: ${(err as Error).message}`);
    }
    try {
      await save.mutateAsync({
        name: form.name.trim(),
        description: form.description.trim(),
        image: form.image.trim(),
        repos: lines(form.repos),
        setup_commands: lines(form.setup_commands),
        env_vars,
      });
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  const body = (
    <>
      <PageHeader
        title={isNew ? "New environment" : form.name || id}
        crumbs={[{ label: "Environments", to: "/environments" }]}
        actions={
          canEdit &&
          !isNew && (
            <Button variant="danger" onClick={() => void confirm({ title: "Delete this environment?", body: "Agents that use it fall back to the default environment.", confirmLabel: "Delete environment", danger: true }).then((ok) => ok && del.mutate(undefined))} busy={del.isPending}>
              Delete
            </Button>
          )
        }
      />
      <form className="stack narrow" onSubmit={submit}>
        {error && <Alert kind="error">{error}</Alert>}
        <fieldset disabled={!canEdit} className="plain-fieldset stack">
          <Field label="Name" htmlFor="e-name">
            <input id="e-name" required value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </Field>
          <Field label="Description" htmlFor="e-desc">
            <input id="e-desc" value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} />
          </Field>
          <Field label="Docker image" htmlFor="e-image" hint="Leave empty to use the server default image.">
            <input id="e-image" className="mono" value={form.image} onChange={(e) => setForm({ ...form, image: e.target.value })} placeholder="ghcr.io/acme/dev:latest" />
          </Field>
          <Field label="Repositories" htmlFor="e-repos" hint="One owner/name per line; cloned into the workspace.">
            <textarea id="e-repos" rows={3} className="mono" value={form.repos} onChange={(e) => setForm({ ...form, repos: e.target.value })} />
          </Field>
          <Field label="Setup commands" htmlFor="e-setup" hint="Run in order before the agent starts.">
            <textarea id="e-setup" rows={4} className="mono" value={form.setup_commands} onChange={(e) => setForm({ ...form, setup_commands: e.target.value })} />
          </Field>
          <Field label="Environment variables" htmlFor="e-vars" hint={<>One <code>KEY=VALUE</code> per line. Use secrets for credentials.</>}>
            <textarea id="e-vars" rows={4} className="mono" value={form.env_vars} onChange={(e) => setForm({ ...form, env_vars: e.target.value })} />
          </Field>
        </fieldset>
        {canEdit && (
          <div className="form-actions">
            <Button type="submit" variant="primary" busy={save.isPending}>
              {isNew ? "Create environment" : "Save"}
            </Button>
          </div>
        )}
      </form>
    </>
  );
  return isNew ? body : <Query q={q}>{() => body}</Query>;
}

// ---------- workers ----------

export function WorkersPage() {
  const q = useQuery({ queryKey: ["workers"], queryFn: () => unwrap(api.GET("/workers")).then((r) => r.items ?? []), refetchInterval: 30_000 });
  return (
    <>
      <PageHeader title="Workers" subtitle="Remote workers that run sandboxes on other hosts over the worker protocol. The server also runs sandboxes with its own backend; it is not listed here." />
      <Query q={q}>
        {(ws) =>
          ws.length === 0 ? (
            <Empty icon={<Server size={18} />} title="No remote workers have connected">
              Runs use the sandbox backend of the server. To run sandboxes on another host, start <code>openfactory worker</code> there.
            </Empty>
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Worker</th>
                    <th scope="col">Backend</th>
                    <th scope="col">Status</th>
                    <th scope="col">Load</th>
                    <th scope="col">Last heartbeat</th>
                    <th scope="col">Version</th>
                  </tr>
                </thead>
                <tbody>
                  {ws.map((w) => (
                    <tr key={w.id}>
                      <td>
                        {w.name}
                        <br />
                        <code className="small muted">{w.id}</code>
                      </td>
                      <td>{w.backend}</td>
                      <td>{w.status === "online" ? <Badge tone="ok">Online</Badge> : <Badge>Offline</Badge>}</td>
                      <td>
                        <meter min={0} max={w.capacity || 1} value={w.active ?? 0} aria-label="Active sandboxes" /> {w.active ?? 0}/{w.capacity ?? 0}
                      </td>
                      <td>
                        <TimeAgo t={w.last_heartbeat} />
                      </td>
                      <td className="muted">{w.version}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )
        }
      </Query>
      <DeadEvents />
    </>
  );
}
