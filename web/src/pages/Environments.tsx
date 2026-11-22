// Team pages: reusable sandbox environments (image, repositories, setup commands).
import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Boxes, Plus } from "lucide-react";
import { api, errorMessage, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { confirm } from "../components/overlays";
import { Alert, Button, Empty, Field, PageHeader, Query } from "../components/ui";
import { kvLines, lines, parseKVLines } from "../lib/format";
import { useMutate } from "../lib/mutate";

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
    quietErrors: true,
    onSuccess: () => navigate("/environments"),
  });
  // The server refuses (409) to delete an environment that an agent uses; show why.
  async function removeIt() {
    setError(null);
    try {
      await del.mutateAsync(undefined);
    } catch (err) {
      setError(errorMessage(err));
    }
  }

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
            <Button variant="danger" onClick={() => void confirm({ title: "Delete this environment?", body: "You cannot delete it while an agent uses it.", confirmLabel: "Delete environment", danger: true }).then((ok) => (ok ? removeIt() : undefined))} busy={del.isPending}>
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
  return isNew ? (
    body
  ) : (
    <Query q={q} notFound={{ title: "Environment not found", text: "The team has no environment with this ID.", back: { label: "Go to environments", to: "/environments" } }}>
      {() => body}
    </Query>
  );
}
