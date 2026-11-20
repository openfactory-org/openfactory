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
type Schedule = Schemas["Schedule"];

/** A time in UTC, for example "2026-11-20 09:00 UTC". Cron schedules run in UTC. */
export function fmtUTC(t?: string | null): string {
  if (!t) return "—";
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) return "—";
  return `${d.toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

/**
 * Sets `enabled:` in the YAML frontmatter of automation.md. It adds the field
 * when the frontmatter does not have it.
 */
export function setFrontmatterEnabled(content: string, enabled: boolean): string {
  const m = /^---\r?\n([\s\S]*?)\r?\n---/.exec(content);
  if (!m) return `---\nenabled: ${enabled}\n---\n\n${content}`;
  const front = m[1];
  const next = /^enabled:.*$/m.test(front) ? front.replace(/^enabled:.*$/m, `enabled: ${enabled}`) : `enabled: ${enabled}\n${front}`;
  return content.slice(0, m.index) + m[0].replace(front, next) + content.slice(m.index + m[0].length);
}

function useSchedules(fid: string) {
  return useQuery({
    queryKey: ["schedules", fid],
    queryFn: () => unwrap(api.GET("/factories/{uid}/schedules", { params: { path: { uid: fid } } })).then((r) => (r.items ?? []) as Schedule[]),
  });
}

function schedulesOf(all: Schedule[] | undefined, a: Pick<Automation, "id" | "name">): Schedule[] {
  return (all ?? []).filter((s) => (a.id && s.automation_id === a.id) || s.automation_name === a.name);
}

/** The state of a schedule as one short text. */
function scheduleState(s: Schedule): string {
  if (!s.enabled) return "automation disabled";
  if (s.paused) return "paused";
  return "active";
}

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
  const schedules = useSchedules(f.id);
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
                    <th scope="col">Next run</th>
                    <th scope="col">Last run</th>
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
                        <ScheduleTimes schedules={schedulesOf(schedules.data, a)} />
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

function ScheduleTimes({ schedules }: { schedules: Schedule[] }) {
  if (schedules.length === 0) {
    return (
      <>
        <td className="muted small">—</td>
        <td className="muted small">—</td>
      </>
    );
  }
  return (
    <>
      <td className="small">
        {schedules.map((s) => (
          <div key={s.id}>{s.next_run_at ? fmtUTC(s.next_run_at) : scheduleState(s) === "active" ? "—" : `None (${scheduleState(s)})`}</div>
        ))}
      </td>
      <td className="small">
        {schedules.map((s) => (
          <div key={s.id}>{s.last_run_at ? fmtUTC(s.last_run_at) : "Never"}</div>
        ))}
      </td>
    </>
  );
}

/** The schedules of one automation, with Pause and Resume. */
function SchedulesCard({ factoryID, schedules, canPause }: { factoryID: string; schedules: Schedule[]; canPause: boolean }) {
  const pause = useMutate(
    ({ id, paused }: { id: string; paused: boolean }) =>
      paused
        ? unwrap(api.POST("/factories/{uid}/schedules/{id}/pause", { params: { path: { uid: factoryID, id } } }))
        : unwrap(api.POST("/factories/{uid}/schedules/{id}/resume", { params: { path: { uid: factoryID, id } } })),
    {
      invalidate: [["schedules", factoryID]],
      success: (_, v) => (v.paused ? "Schedule paused." : "Schedule resumed."),
    },
  );
  if (schedules.length === 0) return null;
  return (
    <Card title="Schedules">
      <ul className="stack stack-sm">
        {schedules.map((s) => (
          <li key={s.id} className="small">
            <div>
              <code>{s.cron}</code> {s.name ? <span className="muted">({s.name})</span> : null} <Badge tone={scheduleState(s) === "active" ? "ok" : "neutral"}>{scheduleState(s)}</Badge>
            </div>
            <div>Next run: {s.next_run_at ? fmtUTC(s.next_run_at) : "none"}</div>
            <div>Last run: {s.last_run_at ? fmtUTC(s.last_run_at) : "never"}</div>
            {s.last_error && <div>Last error: {s.last_error}</div>}
            {canPause && (
              <Button size="sm" onClick={() => pause.mutate({ id: s.id, paused: !s.paused })} busy={pause.isPending && pause.variables?.id === s.id}>
                {s.paused ? "Resume" : "Pause"}
              </Button>
            )}
          </li>
        ))}
      </ul>
    </Card>
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
  const schedules = useSchedules(f.id);
  const save = useMutate(
    (body: { content: string; enabled?: boolean }) => unwrap(api.PUT("/factories/{uid}/automations/{name}", { params: { path: { uid: f.id, name: target } }, body: { content: body.content } })),
    {
      invalidate: [["automations", f.id], ["automation", f.id, target], ["definition", f.id], ["schedules", f.id]],
      success: (_, v) => (v.enabled === undefined ? `Automation ${target} saved.` : `Automation ${target} ${v.enabled ? "enabled" : "disabled"}.`),
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
      await save.mutateAsync({ content });
    } catch (err) {
      setVerrs(err instanceof ApiError && err.validationErrors.length ? err.validationErrors : [{ file: path, message: (err as Error).message }]);
    }
  }

  // Enable or disable: edit `enabled:` in the saved file and save it.
  async function setEnabled(enabled: boolean) {
    setVerrs([]);
    setResult(null);
    const next = setFrontmatterEnabled(q.data?.content ?? content, enabled);
    try {
      await save.mutateAsync({ content: next, enabled });
      setContent(next);
    } catch (err) {
      setVerrs(err instanceof ApiError && err.validationErrors.length ? err.validationErrors : [{ file: path, message: (err as Error).message }]);
    }
  }
  const active = q.data?.automation;

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
            <>
              {active && (
                <Button onClick={() => void setEnabled(!active.enabled)} busy={save.isPending && save.variables?.enabled !== undefined}>
                  {active.enabled ? "Disable" : "Enable"}
                </Button>
              )}
              <Button variant="danger" onClick={() => void confirm({ title: `Delete automation ${routeName}?`, body: "Its triggers stop starting runs. You cannot undo this.", confirmLabel: "Delete automation", danger: true }).then((ok) => ok && del.mutate(undefined))} busy={del.isPending}>
                Delete
              </Button>
            </>
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
              <Button type="submit" variant="primary" busy={save.isPending && save.variables?.enabled === undefined}>
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
          {active && <SchedulesCard factoryID={f.id} schedules={schedulesOf(schedules.data, { id: active.id, name: active.name || routeName })} canPause={canEditFactory(f.id)} />}
          <TestFilterPanel factoryID={f.id} automation={content} automationEnabled={active ? active.enabled : undefined} />
        </aside>
      </div>
    </>
  );
  if (isNew) return body;
  return <Query q={q}>{() => body}</Query>;
}
