// Team page: encrypted secrets of the team and of each factory.
import { useEffect, useState, type FormEvent } from "react";
import { useSearchParams } from "react-router-dom";
import { useQueries, useQuery } from "@tanstack/react-query";
import { KeyRound, Plus } from "lucide-react";
import { api, errorMessage, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { confirm, Dialog } from "../components/overlays";
import { Alert, Button, Card, Empty, Field, PageHeader, Query, TimeAgo } from "../components/ui";
import { useMutate } from "../lib/mutate";
import { useFactories } from "../lib/queries";
import { factorySecretsQuery, teamSecretsQuery } from "../lib/secrets";

type Secret = Schemas["Secret"];

const SECRET_TYPES: { id: NonNullable<Schemas["SecretRequest"]["type"]>; label: string }[] = [
  { id: "generic", label: "Generic" },
  { id: "anthropic_api_key", label: "Anthropic API key" },
  { id: "anthropic_oauth_token", label: "Claude Code OAuth token" },
  { id: "openai_api_key", label: "OpenAI API key" },
  { id: "docker_registry", label: "Docker registry" },
];
const typeLabel = (t?: string) => SECRET_TYPES.find((x) => x.id === t)?.label ?? t ?? "";

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
