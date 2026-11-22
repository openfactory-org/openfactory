// Team page: integration records of forges, chat and issue trackers.
import { useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Plug, Plus } from "lucide-react";
import { api, errorMessage, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { confirm, Dialog } from "../components/overlays";
import { Alert, Badge, Button, Card, Empty, Field, PageHeader, Query } from "../components/ui";
import { parseKVLines } from "../lib/format";
import { useMutate } from "../lib/mutate";
import { useFactories } from "../lib/queries";

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
