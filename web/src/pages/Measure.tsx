import { useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { FlaskConical, Gauge } from "lucide-react";
import { api, errorMessage, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { StackBar } from "../components/Charts";
import { fpath, useFactory } from "../components/FactoryLayout";
import { confirm } from "../components/overlays";
import { Alert, Badge, Button, Card, Code, Empty, Field, PageHeader, Query, StateBadge, TimeAgo } from "../components/ui";
import { fmtTime, percent, samplingLabel, shortID } from "../lib/format";
import { useMutate } from "../lib/mutate";

type Scorer = Schemas["Scorer"];
type Score = Schemas["Score"];
type Suite = Schemas["BenchmarkSuite"];
type BenchmarkRun = Schemas["BenchmarkRun"];
type WorkItem = Schemas["WorkItem"];

/** A proposed definition version (GET /factories/{uid}/definition/proposals). */
export interface Proposal {
  id?: string;
  state?: string;
  base_version_id?: string;
  author_run_id?: string;
  message?: string;
  diff?: string;
  applied_version_id?: string;
  reviewed_by?: string;
  review_comment?: string;
  created_at?: string;
  base_is_active?: boolean;
}

/** Renders arbitrary JSON: scalars inline, arrays of objects as tables, objects as definition lists. */
export function JsonView({ value, depth = 0 }: { value: unknown; depth?: number }): ReactNode {
  if (value === null || value === undefined) return <span className="muted">—</span>;
  if (typeof value !== "object") return <span>{String(value)}</span>;
  if (depth > 4) return <code className="small">{JSON.stringify(value)}</code>;
  if (Array.isArray(value)) {
    if (value.length === 0) return <span className="muted">none</span>;
    if (value.every((v) => v && typeof v === "object" && !Array.isArray(v))) {
      const cols = Array.from(new Set(value.flatMap((v) => Object.keys(v as object)))).slice(0, 10);
      return (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                {cols.map((c) => (
                  <th key={c} scope="col">
                    {c}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {value.map((row, i) => (
                <tr key={i}>
                  {cols.map((c) => (
                    <td key={c}>
                      <JsonView value={(row as Record<string, unknown>)[c]} depth={depth + 1} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
    }
    return <span>{value.map((v) => (typeof v === "object" ? JSON.stringify(v) : String(v))).join(", ")}</span>;
  }
  return (
    <dl className="kv">
      {Object.entries(value as Record<string, unknown>).map(([k, v]) => (
        <div key={k} className="kv-row">
          <dt>{k}</dt>
          <dd>
            <JsonView value={v} depth={depth + 1} />
          </dd>
        </div>
      ))}
    </dl>
  );
}

export function ScorersPage() {
  const f = useFactory();
  const { canEditFactory } = useAuth();
  const canEdit = canEditFactory(f.id);
  const scorers = useQuery({
    queryKey: ["scorers", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/scorers", { params: { path: { uid: f.id } } })).then((r) => (r.items ?? []) as Scorer[]),
  });
  const scores = useQuery({
    queryKey: ["scores", f.id, "30d"],
    queryFn: () => unwrap(api.GET("/factories/{uid}/scores", { params: { path: { uid: f.id }, query: { since: "30d", limit: 200 } } })).then((r) => (r.items ?? []) as Score[]),
  });
  const improvements = useQuery({
    queryKey: ["scores", f.id, "improvements"],
    queryFn: () => unwrap(api.GET("/factories/{uid}/scores", { params: { path: { uid: f.id }, query: { limit: 200 } } })).then((r) => Array.from(new Set(((r.items ?? []) as Score[]).map((s) => s.improvement_run_id).filter(Boolean))) as string[]),
  });
  const improve = useMutate(() => unwrap(api.POST("/factories/{uid}/self-improvement", { params: { path: { uid: f.id } } })), {
    success: "Self-improvement check started.",
    invalidate: [["scores", f.id]],
  });

  const names = new Map((scorers.data ?? []).map((s) => [s.id, s.name || s.slug]));
  return (
    <>
      <PageHeader
        title="Scorers"
        crumbs={[{ label: f.name, to: fpath(f) }]}
        subtitle="Classify finished runs against a rubric; failures can feed the self-improvement loop. Last 30 days."
        actions={
          canEdit && (
            <Button onClick={() => improve.mutate(undefined)} busy={improve.isPending}>
              Run self-improvement now
            </Button>
          )
        }
      />
      <Query q={scorers}>
        {(list) =>
          list.length === 0 ? (
            <Empty
              icon={<Gauge size={18} />}
              title="No scorers yet"
              action={
                <Link className="btn" to={fpath(f, "definition")}>
                  Open the definition
                </Link>
              }
            >
              A scorer grades finished runs against a rubric. Add <code>scorers/&lt;name&gt;/scorer.md</code> to the definition.
            </Empty>
          ) : (
            <ul className="grid-cards">
              {list.map((sc) => {
                const mine = (scores.data ?? []).filter((s) => s.scorer_id === sc.id);
                const passed = mine.filter((s) => s.passed).length;
                const labels = mine.reduce<Record<string, number>>((m, s) => ({ ...m, [s.label ?? ""]: (m[s.label ?? ""] ?? 0) + 1 }), {});
                return (
                  <li key={sc.id} className="card">
                    <h2 className="card-title">
                      {sc.name || sc.slug} {sc.self_improvement && <Badge tone="info">self-improvement</Badge>}
                    </h2>
                    {sc.description && <p className="small">{sc.description}</p>}
                    <p className="metric-value">{mine.length ? percent(passed / mine.length) : "—"}</p>
                    <p className="muted small">
                      {passed} of {mine.length} sampled runs passed · sampling {samplingLabel(sc.sampling_rate)} of runs
                    </p>
                    <StackBar parts={Object.entries(labels).map(([label, c]) => ({ label, count: c }))} />
                  </li>
                );
              })}
            </ul>
          )
        }
      </Query>
      {((scorers.data ?? []).length > 0 || (scores.data ?? []).length > 0) && (
      <section aria-label="Recent results">
        <h2 className="section-title">Recent results</h2>
        <Query q={scores}>
          {(list) =>
            list.length === 0 ? (
              <p className="muted small">No scores in the last 30 days. Scores appear after sampled runs finish.</p>
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">Run</th>
                      <th scope="col">Scorer</th>
                      <th scope="col">Agent</th>
                      <th scope="col">Label</th>
                      <th scope="col">Result</th>
                      <th scope="col">Rationale</th>
                      <th scope="col">When</th>
                    </tr>
                  </thead>
                  <tbody>
                    {[...list]
                      .sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""))
                      .slice(0, 100)
                      .map((s) => (
                        <tr key={s.id}>
                          <td>
                            <Link to={`/runs/${encodeURIComponent(s.run_id ?? "")}`}>{shortID(s.run_id)}</Link>
                          </td>
                          <td>{names.get(s.scorer_id ?? "") ?? s.scorer_id}</td>
                          <td>{s.agent_name}</td>
                          <td>{s.label}</td>
                          <td>{s.passed ? <Badge tone="ok">passed</Badge> : <Badge tone="bad">failed</Badge>}</td>
                          <td className="small">{(s.rationale ?? "").slice(0, 160)}</td>
                          <td>
                            <TimeAgo t={s.created_at} />
                          </td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
            )
          }
        </Query>
      </section>
      )}
      <section aria-label="Self-improvement">
        <h2 className="section-title">Self-improvement</h2>
        <p className="muted small">When scorers mark runs as failing, an improvement run proposes a change to the definition or opens a pull request. Proposals, pull requests and runs are listed here.</p>
        <Proposals canEdit={canEdit} />
        <ImprovementPullRequests />
        <Query q={improvements}>
          {(ids) =>
            ids.length === 0 ? null : (
              <ul className="stack stack-sm" aria-label="Self-improvement runs">
                {ids.map((id) => (
                  <li key={id}>
                    <Link to={`/runs/${encodeURIComponent(id)}`}>Improvement run {shortID(id)}</Link>
                  </li>
                ))}
              </ul>
            )
          }
        </Query>
      </section>
    </>
  );
}

/** Proposed changes of the managed definition, with their diffs. */
export function Proposals({ canEdit }: { canEdit: boolean }) {
  const f = useFactory();
  const [comments, setComments] = useState<Record<string, string>>({});
  const q = useQuery({
    queryKey: ["proposals", f.id],
    queryFn: () =>
      unwrap(api.GET("/factories/{uid}/definition/proposals", { params: { path: { uid: f.id }, query: { state: "all" } } })).then(
        (r) => (r.items ?? []) as Proposal[],
      ),
  });
  const approve = useMutate(
    (id: string) => unwrap(api.POST("/factories/{uid}/definition/proposals/{id}/approve", { params: { path: { uid: f.id, id } } })),
    { invalidate: [["proposals", f.id], ["inbox"]], success: "The change is applied." },
  );
  const reject = useMutate(
    (id: string) =>
      unwrap(
        api.POST("/factories/{uid}/definition/proposals/{id}/reject", {
          params: { path: { uid: f.id, id } },
          body: { comment: comments[id] ?? "" },
        }),
      ),
    { invalidate: [["proposals", f.id], ["inbox"]], success: "The proposal is rejected. The agent gets your comment." },
  );
  return (
    <Query q={q}>
      {(items) =>
        items.length === 0 ? (
          null
        ) : (
          <ul className="stack" aria-label="Proposed definition changes">
            {items.map((p) => (
              <li key={p.id}>
                <Card
                  title={
                    <>
                      {p.message} <StateBadge state={p.state} />
                    </>
                  }
                >
                  <p className="muted small">
                    Version {shortID(p.id)} on base {shortID(p.base_version_id)} · proposed <TimeAgo t={p.created_at} />
                    {p.author_run_id && (
                      <>
                        {" "}
                        by <Link to={`/runs/${encodeURIComponent(p.author_run_id)}`}>run {shortID(p.author_run_id)}</Link>
                      </>
                    )}
                    {p.applied_version_id && <> · applied as {shortID(p.applied_version_id)}</>}
                    {p.review_comment && <> · “{p.review_comment}”</>}
                  </p>
                  <Code>{p.diff ?? ""}</Code>
                  {p.state === "proposed" && canEdit && (
                    <div className="row">
                      {!p.base_is_active && <Alert kind="warning">The active definition changed after this proposal. Approve fails; reject it and ask for a new proposal.</Alert>}
                      <Button variant="primary" onClick={() => approve.mutate(p.id ?? "")} busy={approve.isPending} disabled={!p.base_is_active}>
                        Approve and apply
                      </Button>
                      <input
                        aria-label={`Comment for ${shortID(p.id)}`}
                        placeholder="Reason for the rejection"
                        value={comments[p.id ?? ""] ?? ""}
                        onChange={(e) => setComments({ ...comments, [p.id ?? ""]: e.target.value })}
                      />
                      <Button variant="danger" onClick={() => reject.mutate(p.id ?? "")} busy={reject.isPending}>
                        Reject
                      </Button>
                    </div>
                  )}
                </Card>
              </li>
            ))}
          </ul>
        )
      }
    </Query>
  );
}

/** The pull requests of Self-improvement work items (a git definition, or application code). */
function ImprovementPullRequests() {
  const f = useFactory();
  const q = useQuery({
    queryKey: ["work-items", f.id, "self-improvement"],
    queryFn: () =>
      unwrap(api.GET("/factories/{uid}/work-items", { params: { path: { uid: f.id }, query: { limit: 100 } } })).then((r) =>
        ((r.items ?? []) as WorkItem[]).filter((w) => w.source?.provider === "self_improvement"),
      ),
  });
  return (
    <Query q={q}>
      {(items) => {
        const prs = items.flatMap((w) => (w.pull_requests ?? []).map((pr) => ({ w, pr })));
        if (prs.length === 0) return null;
        return (
          <ul aria-label="Self-improvement pull requests">
            {prs.map(({ w, pr }) => (
              <li key={`${w.id}-${pr.url}`}>
                <a href={pr.url} target="_blank" rel="noreferrer">
                  {pr.repo}#{pr.number}
                </a>{" "}
                <StateBadge state={pr.state} /> <span className="muted small">{w.title}</span>
              </li>
            ))}
          </ul>
        );
      }}
    </Query>
  );
}

export function BenchmarksPage() {
  const f = useFactory();
  const { canEditFactory } = useAuth();
  const canEdit = canEditFactory(f.id);
  const navigate = useNavigate();
  const suites = useQuery({
    queryKey: ["benchmarks", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/benchmarks", { params: { path: { uid: f.id } } })).then((r) => (r.items ?? []) as Suite[]),
  });
  const runs = useQuery({
    queryKey: ["benchmark-runs", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/benchmark-runs", { params: { path: { uid: f.id }, query: { limit: 100 } } })).then((r) => (r.items ?? []) as BenchmarkRun[]),
  });
  const [suiteSlug, setSuiteSlug] = useState("");
  const [configs, setConfigs] = useState<string[]>([]);
  const [reps, setReps] = useState(1);
  const [error, setError] = useState<string | null>(null);
  const suite = (suites.data ?? []).find((s) => s.slug === (suiteSlug || suites.data?.[0]?.slug));
  const suiteConfigs = (suite?.configurations ?? []) as { name?: string }[];
  const start = useMutate(
    (body: Schemas["StartBenchmarkRequest"]) =>
      unwrap(api.POST("/factories/{uid}/benchmarks/{suite}/runs", { params: { path: { uid: f.id, suite: suite!.slug! } }, body })),
    { invalidate: [["benchmark-runs", f.id]], quietErrors: true, success: "Benchmark started.", onSuccess: (r) => navigate(fpath(f, `benchmarks/${r.id}`)) },
  );

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!suite) return setError("Choose a benchmark suite.");
    if (suiteConfigs.length > 0 && configs.length === 0) return setError("Choose at least one configuration.");
    if (reps < 1 || reps > 10) return setError("Repetitions must be 1 through 10.");
    try {
      await start.mutateAsync({
        configurations: suiteConfigs.filter((c) => c.name && configs.includes(c.name)) as Schemas["StartBenchmarkRequest"]["configurations"],
        repetitions: reps,
      });
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  return (
    <>
      <PageHeader title="Benchmarks" crumbs={[{ label: f.name, to: fpath(f) }]} subtitle="Compare model and machine configurations against a fixed set of tasks." />
      {suites.data && suites.data.length === 0 && (runs.data ?? []).length === 0 ? (
        <Empty
          icon={<FlaskConical size={18} />}
          title="No benchmark suites yet"
          action={
            <Link className="btn" to={fpath(f, "definition")}>
              Open the definition
            </Link>
          }
        >
          A suite runs a fixed set of tasks against each configuration. Add <code>benchmarks/&lt;suite&gt;/suite.yaml</code> to the definition.
        </Empty>
      ) : (
      <div className="two-col">
        <section aria-label="Runs" className="stack">
          <h2 className="section-title">Runs</h2>
          <Query q={runs}>
            {(list) =>
              list.length === 0 ? (
                <Empty icon={<FlaskConical size={18} />} title="No benchmark runs yet">
                  Start one from a suite.
                </Empty>
              ) : (
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">Run</th>
                      <th scope="col">Suite</th>
                      <th scope="col">State</th>
                      <th scope="col">Started</th>
                    </tr>
                  </thead>
                  <tbody>
                    {list.map((b) => (
                      <tr key={b.id}>
                        <td>
                          <Link to={fpath(f, `benchmarks/${encodeURIComponent(b.id ?? "")}`)}>{shortID(b.id)}</Link>
                        </td>
                        <td>{b.suite}</td>
                        <td>
                          <StateBadge state={b.state} />
                        </td>
                        <td>
                          <TimeAgo t={b.created_at} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )
            }
          </Query>
        </section>
        <section aria-label="Suites" className="stack">
          <h2 className="section-title">Suites</h2>
          <Query q={suites}>
            {(list) =>
              list.length === 0 ? (
                <Empty title="No benchmark suites">
                  Add <code>benchmarks/&lt;suite&gt;/suite.yaml</code> to the definition.
                </Empty>
              ) : (
                <>
                  <ul className="stack">
                    {list.map((s) => (
                      <li key={s.slug} className="card">
                        <h3 className="card-title">{s.name || s.slug}</h3>
                        {s.description && <p className="small">{s.description}</p>}
                        <p className="muted small">
                          {(s.tasks ?? []).length} tasks · {(s.configurations ?? []).length} configurations{s.agent ? ` · agent ${s.agent}` : ""}
                        </p>
                      </li>
                    ))}
                  </ul>
                  {canEdit && (
                    <Card title="Start a benchmark">
                      <form className="stack" onSubmit={submit}>
                        {error && <Alert kind="error">{error}</Alert>}
                        <Field label="Suite" htmlFor="b-suite">
                          <select
                            id="b-suite"
                            value={suite?.slug ?? ""}
                            onChange={(e) => {
                              setSuiteSlug(e.target.value);
                              setConfigs([]);
                            }}
                          >
                            {list.map((s) => (
                              <option key={s.slug} value={s.slug}>
                                {s.name || s.slug}
                              </option>
                            ))}
                          </select>
                        </Field>
                        {suiteConfigs.length > 0 && (
                          <fieldset className="field">
                            <legend>Configurations</legend>
                            {suiteConfigs.map((c) => (
                              <label key={c.name} className="check">
                                <input
                                  type="checkbox"
                                  checked={configs.includes(c.name ?? "")}
                                  onChange={(e) => setConfigs(e.target.checked ? [...configs, c.name ?? ""] : configs.filter((x) => x !== c.name))}
                                />{" "}
                                {c.name}
                              </label>
                            ))}
                          </fieldset>
                        )}
                        <Field label="Repetitions" htmlFor="b-reps">
                          <input id="b-reps" type="number" min={1} max={10} value={reps} onChange={(e) => setReps(Number(e.target.value))} />
                        </Field>
                        <div className="form-actions">
                          <Button type="submit" variant="primary" busy={start.isPending}>
                            Start benchmark
                          </Button>
                        </div>
                      </form>
                    </Card>
                  )}
                </>
              )
            }
          </Query>
        </section>
      </div>
      )}
    </>
  );
}

export function BenchmarkPage() {
  const f = useFactory();
  const { canEditFactory } = useAuth();
  const { id = "" } = useParams();
  const cancel = useMutate(() => unwrap(api.POST("/benchmark-runs/{id}/cancel", { params: { path: { id } } })), {
    invalidate: [["benchmark-run", id], ["benchmark-runs", f.id]],
    success: "Benchmark cancelled.",
  });
  const q = useQuery({
    queryKey: ["benchmark-run", id],
    queryFn: () => unwrap(api.GET("/benchmark-runs/{id}", { params: { path: { id } } })) as Promise<BenchmarkRun>,
    refetchInterval: (query) => {
      const st = (query.state.data as BenchmarkRun | undefined)?.state;
      return st === "running" || st === "scoring" ? 10_000 : false;
    },
  });
  return (
    <Query q={q}>
      {(b) => (
        <>
          <PageHeader
            title={
              <>
                {b.suite} <StateBadge state={b.state} />
              </>
            }
            crumbs={[
              { label: f.name, to: fpath(f) },
              { label: "Benchmarks", to: fpath(f, "benchmarks") },
            ]}
            subtitle={`Started ${fmtTime(b.created_at)} · finished ${fmtTime(b.finished_at)}`}
            actions={
              canEditFactory(f.id) &&
              (b.state === "running" || b.state === "scoring") && (
                <Button
                  variant="danger"
                  onClick={() => void confirm({ title: "Cancel this benchmark?", body: "Trials and judge runs that are not finished stop.", confirmLabel: "Cancel benchmark", cancelLabel: "Keep running", danger: true }).then((ok) => ok && cancel.mutate(undefined))}
                  busy={cancel.isPending}
                >
                  Cancel benchmark
                </Button>
              )
            }
          />
          <Card title="Results">
            {b.results ? (
              <>
                <JsonView value={b.results} />
                <details>
                  <summary className="small">Raw JSON</summary>
                  <Code>{JSON.stringify(b.results, null, 2)}</Code>
                </details>
              </>
            ) : (
              <p className="muted">No results yet{b.state === "running" ? " — trials are still running" : ""}.</p>
            )}
          </Card>
        </>
      )}
    </Query>
  );
}
