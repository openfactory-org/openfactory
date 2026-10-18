import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { fpath, useFactory } from "../components/FactoryLayout";
import { Plus, Zap } from "lucide-react";
import { confirm } from "../components/overlays";
import { Alert, Badge, Button, Card, Empty, Field, PageHeader, Query, ValidationList } from "../components/ui";
import { useMutate } from "../lib/mutate";
import { ReadOnlyNotice } from "./Agents";
import { TestFilterPanel } from "./Webhooks";

type Automation = Schemas["Automation"];
type ValidationResult = Schemas["ValidationResult"];

export const NEW_AUTOMATION_TEMPLATE = `---
enabled: true
agent: foreman
triggers:
  - provider: schedule
    event: cron_fired
    schedule:
      cron: "0 9 * * 1"
---

Describe what the agent should do when this automation fires.
`;

function triggerSummary(t: Record<string, unknown>): string {
  const sched = t.schedule as { cron?: string } | undefined;
  if (t.provider === "schedule" && sched?.cron) return `schedule ${sched.cron}`;
  return [t.provider, t.event].filter(Boolean).join(" · ");
}

export function AutomationsPage() {
  const f = useFactory();
  const q = useQuery({
    queryKey: ["automations", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/automations", { params: { path: { uid: f.id } } })).then((r) => (r.items ?? []) as Automation[]),
  });
  const hooks = useQuery({
    queryKey: ["webhooks", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/webhooks", { params: { path: { uid: f.id } } })).then((r) => r.items ?? []),
  });
  return (
    <>
      <PageHeader
        title="Automations"
        crumbs={[{ label: f.name, to: fpath(f) }]}
        subtitle="Triggers that start runs: schedules, forge, tracker and chat events, or custom webhook deliveries."
        actions={
          <>
            <Link className="btn btn-ghost hide-sm" to={fpath(f, "webhooks")}>
              Webhooks ({hooks.data?.length ?? 0})
            </Link>
            {f.source !== "git" && (
              <Link className="btn btn-primary" to={fpath(f, "automations/new")}>
                <Plus size={14} aria-hidden="true" /> New automation
              </Link>
            )}
          </>
        }
      />
      <ReadOnlyNotice f={f} />
      <Query q={q}>
        {(autos) =>
          autos.length === 0 ? (
            <Empty
              icon={<Zap size={18} />}
              title="No automations"
              action={
                f.source !== "git" ? (
                  <Link className="btn btn-primary" to={fpath(f, "automations/new")}>
                    New automation
                  </Link>
                ) : undefined
              }
            >
              Work only arrives when someone sends it. Add an automation to react to events.
            </Empty>
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Automation</th>
                    <th scope="col">Status</th>
                    <th scope="col">Agent</th>
                    <th scope="col">Triggers</th>
                  </tr>
                </thead>
                <tbody>
                  {[...autos]
                    .sort((a, b) => a.name.localeCompare(b.name))
                    .map((a) => (
                      <tr key={a.name}>
                        <td>
                          <Link to={fpath(f, `automations/${encodeURIComponent(a.name)}`)}>{a.display_name || a.name}</Link>
                        </td>
                        <td>{a.enabled ? <Badge tone="ok">enabled</Badge> : <Badge>disabled</Badge>}</td>
                        <td>{a.agent || "foreman"}</td>
                        <td className="small">{(a.triggers ?? []).map((t) => triggerSummary(t)).join(", ")}</td>
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

export function AutomationPage() {
  const f = useFactory();
  const { name: routeName = "" } = useParams();
  const isNew = routeName === "new";
  const navigate = useNavigate();
  const { canEditFactory } = useAuth();
  const q = useQuery({
    queryKey: ["automation", f.id, routeName],
    enabled: !isNew,
    queryFn: () => unwrap(api.GET("/factories/{uid}/automations/{name}", { params: { path: { uid: f.id, name: routeName } } })),
  });
  const [name, setName] = useState("");
  const [content, setContent] = useState(isNew ? NEW_AUTOMATION_TEMPLATE : "");
  const [result, setResult] = useState<ValidationResult | null>(null);
  const [verrs, setVerrs] = useState<Schemas["ValidationError"][]>([]);
  useEffect(() => {
    if (q.data) setContent(q.data.content);
  }, [q.data]);
  const target = isNew ? name.trim().toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "") : routeName;
  const path = `automations/${target || "<name>"}/automation.md`;
  const editable = canEditFactory(f.id) && f.source !== "git";

  const validate = useMutate(
    () => unwrap(api.POST("/factories/{uid}/automations/{name}/validate", { params: { path: { uid: f.id, name: target || "draft" } }, body: { content } })),
    { onSuccess: (r) => setResult(r) },
  );
  const save = useMutate(
    () => unwrap(api.PUT("/factories/{uid}/automations/{name}", { params: { path: { uid: f.id, name: target } }, body: { content } })),
    {
      invalidate: [["automations", f.id], ["automation", f.id, target], ["definition", f.id]],
      success: `Automation ${target} saved.`,
      quietErrors: true,
      onSuccess: () => {
        setVerrs([]);
        if (isNew) navigate(fpath(f, `automations/${encodeURIComponent(target)}`), { replace: true });
      },
    },
  );
  const del = useMutate(() => unwrap(api.DELETE("/factories/{uid}/automations/{name}", { params: { path: { uid: f.id, name: routeName } } })), {
    invalidate: [["automations", f.id], ["definition", f.id]],
    success: `Automation ${routeName} deleted.`,
    onSuccess: () => navigate(fpath(f, "automations")),
  });

  async function submit() {
    setVerrs([]);
    setResult(null);
    if (isNew && !target) {
      setVerrs([{ file: path, message: "Enter an automation name (letters, numbers, '-' and '_')." }]);
      return;
    }
    try {
      await save.mutateAsync(undefined);
    } catch (err) {
      setVerrs(err instanceof ApiError && err.validationErrors.length ? err.validationErrors : [{ file: path, message: (err as Error).message }]);
    }
  }

  const body = (
    <>
      <PageHeader
        title={isNew ? "New automation" : q.data?.automation?.display_name || routeName}
        crumbs={[
          { label: f.name, to: fpath(f) },
          { label: "Automations", to: fpath(f, "automations") },
        ]}
        subtitle={
          <>
            Defined in <code>{path}</code>
          </>
        }
        actions={
          editable &&
          !isNew && (
            <Button variant="danger" onClick={() => void confirm({ title: `Delete automation ${routeName}?`, body: "Its triggers stop starting runs. You cannot undo this.", confirmLabel: "Delete automation", danger: true }).then((ok) => ok && del.mutate(undefined))} busy={del.isPending}>
              Delete
            </Button>
          )
        }
      />
      <ReadOnlyNotice f={f} />
      <ValidationList errors={verrs} />
      <div className="two-col">
        <form
          className="stack"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          {isNew && (
            <Field label="Name" htmlFor="aname" hint="Lowercase letters, numbers, '-' and '_'.">
              <input id="aname" required value={name} onChange={(e) => setName(e.target.value)} />
            </Field>
          )}
          <Card title="automation.md">
            <label className="sr-only" htmlFor="content">
              Automation file
            </label>
            <textarea id="content" rows={22} className="mono" spellCheck={false} value={content} readOnly={!editable} onChange={(e) => setContent(e.target.value)} />
            <p className="hint">YAML frontmatter (enabled, agent, triggers, overrides) followed by the prompt the agent receives.</p>
          </Card>
          {result &&
            (result.valid ? (
              <Alert kind="success">The definition is valid with this automation.</Alert>
            ) : (
              <ValidationList errors={result.errors} title="This automation does not validate" />
            ))}
          <div className="form-actions">
            <Button onClick={() => validate.mutate(undefined)} busy={validate.isPending}>
              Validate
            </Button>
            {editable && (
              <Button type="submit" variant="primary" busy={save.isPending}>
                Save and apply
              </Button>
            )}
          </div>
        </form>
        <aside className="stack">
          {q.data?.automation && (
            <Card title="Active">
              <p className="small">
                {q.data.automation.enabled ? <Badge tone="ok">enabled</Badge> : <Badge>disabled</Badge>} · agent {q.data.automation.agent || "foreman"}
              </p>
            </Card>
          )}
          <TestFilterPanel factoryID={f.id} automation={content} />
        </aside>
      </div>
    </>
  );
  if (isNew) return body;
  return <Query q={q}>{() => body}</Query>;
}
