import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { Chart, StackBar } from "../components/Charts";
import { fpath, useFactory } from "../components/FactoryLayout";
import { useShell } from "../components/shell";
import { SpendCard } from "../components/SpendCard";
import { Button, Card, ErrorAlert, Metric, PageHeader, Query, Segmented, StatusIcon } from "../components/ui";
import { coverageGaps, describeGaps, hasApplicableForge } from "../lib/coverage";
import { count, duration, money, percent } from "../lib/format";
import { factoryCredentialState, useUsableSecrets } from "../lib/secrets";
import { WorkTable } from "./Work";

type FactoryMetrics = Schemas["FactoryMetrics"];

const RANGES = [
  { key: "7d", label: "Last 7 days", short: "7 days", days: 7 },
  { key: "30d", label: "Last 30 days", short: "30 days", days: 30 },
  { key: "90d", label: "Last 90 days", short: "90 days", days: 90 },
];

function shortDate(d: string): string {
  const t = new Date(`${d}T00:00:00Z`);
  return Number.isNaN(t.getTime()) ? d : t.toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });
}

function countsChart(title: string, m: Record<string, number> | undefined) {
  const entries = Object.entries(m ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return <Chart title={title} kind="bar" labels={entries.map(([k]) => k)} series={[{ name: "Runs", values: entries.map(([, v]) => v) }]} />;
}

export function MetricsView({ m }: { m: FactoryMetrics }) {
  const daily = m.daily ?? [];
  const labels = daily.map((d) => shortDate(d.date ?? ""));
  const pr = m.pull_requests ?? {};
  const cycle = m.cycle_time ?? {};
  const prCycle = cycle.pull_requests ?? {};
  const autonomy = m.autonomy ?? {};
  const sizes = (pr.by_size ?? []).filter((b) => (b.count ?? 0) > 0);
  const shapes = Object.entries(m.cost?.compute_by_shape ?? {}).sort(([a], [b]) => a.localeCompare(b));
  const [showExpensive, setShowExpensive] = useState(false);
  // Pull request tiles only carry data after a code host reports pull requests.
  const hasPR = !!autonomy.facts_since || (pr.opened ?? 0) > 0 || (autonomy.merged_prs ?? 0) > 0;
  // The API keys runs.by_state by the stored state, in lower case ("failed", "error").
  const byState = m.runs?.by_state ?? {};
  const failedRuns = (byState.failed ?? byState.FAILED ?? 0) + (byState.error ?? byState.ERROR ?? 0);
  return (
    <>
      <ul className={hasPR ? "metric-grid" : "metric-grid cols-4"}>
        {hasPR && (
          <Metric
            label="Autonomy"
            value={autonomy.merged_prs ? percent(autonomy.pr_rate) : "—"}
            hint={`${autonomy.autonomous_merged_prs ?? 0} of ${autonomy.merged_prs ?? 0} merged PRs had no push by a person`}
          />
        )}
        <Metric
          label="Work item autonomy"
          value={percent(autonomy.rate)}
          hint={`${autonomy.autonomous_work_items ?? 0} of ${autonomy.completed_work_items ?? 0} completed work items needed no person`}
        />
        {hasPR && (
          <Metric
            label="PR cycle time"
            value={prCycle.count ? duration(prCycle.start_to_merge_ms) : "—"}
            hint={`Median from start to merge of ${prCycle.count ?? 0} merged PRs`}
          />
        )}
        <Metric label="Work item cycle time" value={duration(cycle.median_ms)} hint={`Median of ${cycle.count ?? 0} completed · p90 ${duration(cycle.p90_ms)}`} />
        {hasPR && <Metric label="Cost per PR" value={pr.opened ? money(pr.median_cost_usd) : "—"} hint="Median inference cost, for comparison, not an invoice total" />}
        {hasPR && <Metric label="PRs opened / merged" value={`${pr.opened ?? 0} / ${pr.merged ?? 0}`} hint={`Merge rate ${percent(pr.merge_rate)}`} />}
        <Metric label="Work items" value={count(m.work_items?.created)} hint={`${m.work_items?.completed ?? 0} completed`} />
        <Metric label="Runs" value={count(m.runs?.total)} />
        <Metric label="Failed runs" value={count(failedRuns)} hint={m.runs?.total ? `${percent(failedRuns / m.runs.total)} of runs` : undefined} />
        <Metric label="Total cost" value={money(m.cost?.total?.cost_usd)} hint={`${count(m.cost?.total?.input_tokens)} in / ${count(m.cost?.total?.output_tokens)} out tokens`} />
        <Metric
          label="Compute time"
          value={(m.cost?.total?.compute_seconds ?? 0) > 0 ? duration((m.cost?.total?.compute_seconds ?? 0) * 1000) : "—"}
          hint="Sandbox time of the runs, next to the inference cost"
        />
        <Metric label="Waiting on people" value={count(m.human_wait?.pending)} hint={`Median wait ${duration(m.human_wait?.median_ms)}`} />
      </ul>
      <p className="muted small">
        {autonomy.facts_since
          ? `Pull request figures start when the forge was connected (first fact ${new Date(autonomy.facts_since).toLocaleDateString()}).`
          : "Pull request figures start when the forge is connected."}
        {(autonomy.merged_prs_without_facts ?? 0) > 0 && ` ${autonomy.merged_prs_without_facts} merged ${autonomy.merged_prs_without_facts === 1 ? "PR from before is" : "PRs from before are"} not counted.`}
      </p>
      {(prCycle.count ?? 0) > 0 && (
        <Card title="Pull request cycle">
          <p className="muted small">Independent medians; they do not sum to the headline.</p>
          <ul className="inline-list">
            <li>
              <span className="muted">start to PR</span> <strong>{duration(prCycle.start_to_pr_ms)}</strong>
            </li>
            <li>
              <span className="muted">PR to first review</span> <strong>{prCycle.reviewed ? duration(prCycle.pr_to_first_review_ms) : "—"}</strong>
            </li>
            <li>
              <span className="muted">first review to merge</span> <strong>{prCycle.reviewed ? duration(prCycle.first_review_to_merge_ms) : "—"}</strong>
            </li>
          </ul>
        </Card>
      )}
      {sizes.length > 0 && (
        <Card title="Cost by PR size">
          <table className="table">
            <thead>
              <tr>
                <th>Size</th>
                <th>Changed lines</th>
                <th className="num">PRs</th>
                <th className="num">Median cost</th>
                <th className="num">Total cost</th>
              </tr>
            </thead>
            <tbody>
              {sizes.map((b) => (
                <tr key={b.size}>
                  <td>{b.size}</td>
                  <td>{b.lines || "not known"}</td>
                  <td className="num">{count(b.count)}</td>
                  <td className="num">{money(b.median_cost_usd)}</td>
                  <td className="num">{money(b.total_cost_usd)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      {shapes.length > 0 && (
        <Card title="Compute by instance shape">
          <table className="table">
            <thead>
              <tr>
                <th>Instance shape</th>
                <th className="num">Sessions</th>
                <th className="num">Time</th>
              </tr>
            </thead>
            <tbody>
              {shapes.map(([shape, c]) => (
                <tr key={shape}>
                  <td>{shape}</td>
                  <td className="num">{count(c.sessions)}</td>
                  <td className="num">{duration((c.seconds ?? 0) * 1000)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      {cycle.stage_median_ms && Object.keys(cycle.stage_median_ms).length > 0 && (
        <Card title="Cycle time by stage">
          <p className="muted small">Independent medians; they do not sum to the headline.</p>
          <ul className="inline-list">
            {Object.entries(cycle.stage_median_ms).map(([stage, ms]) => (
              <li key={stage}>
                <span className="muted">{stage.replace(/_/g, " ")}</span> <strong>{duration(ms)}</strong>
              </li>
            ))}
          </ul>
        </Card>
      )}
      <section className="chart-grid" aria-label="Charts">
        <Chart
          title="PR activity"
          kind="line"
          labels={labels}
          series={[
            { name: "Opened", values: daily.map((d) => d.prs_opened ?? 0) },
            { name: "Merged", values: daily.map((d) => d.prs_merged ?? 0) },
          ]}
        />
        <Chart title="Runs" kind="bar" labels={labels} series={[{ name: "Runs", values: daily.map((d) => d.runs ?? 0) }]} />
        <Chart title="Cost (USD)" kind="line" unit="$" labels={labels} series={[{ name: "Cost", values: daily.map((d) => d.cost_usd ?? 0) }]} />
        {countsChart("Runs by state", m.runs?.by_state)}
        {countsChart("Runs by agent", m.runs?.by_agent)}
      </section>
      {(pr.most_expensive?.length ?? 0) > 0 && (
        <Card
          title="Most expensive PRs"
          actions={
            <Button variant="link" aria-expanded={showExpensive} onClick={() => setShowExpensive((s) => !s)}>
              {showExpensive ? "Hide" : "Show"}
            </Button>
          }
        >
          {showExpensive && (
            <table className="table">
              <thead>
                <tr>
                  <th>Pull request</th>
                  <th>State</th>
                  <th className="num">Cost</th>
                </tr>
              </thead>
              <tbody>
                {pr.most_expensive!.map((p) => (
                  <tr key={`${p.repo}#${p.number}`}>
                    <td>{p.url ? <a href={p.url}>{`${p.repo}#${p.number}`}</a> : `${p.repo}#${p.number}`}</td>
                    <td>{p.state}</td>
                    <td className="num">{money(p.cost_usd)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>
      )}
    </>
  );
}

export function ScorerCards({ fid, scorers }: { fid: string; scorers: NonNullable<FactoryMetrics["scorers"]> }) {
  if (scorers.length === 0) return null;
  return (
    <section aria-label="Scorers">
      <h2 className="section-title">Scorers</h2>
      <ul className="grid-cards">
        {scorers.map((s) => (
          <li key={s.scorer_id} className="card">
            <h3 className="card-title">
              <Link to={fpath(fid, "scorers")}>{s.name || s.slug}</Link>
            </h3>
            <p className="metric-value">{s.total ? percent(s.pass_rate) : "—"}</p>
            <p className="muted small">
              {s.passed ?? 0} of {s.total ?? 0} sampled runs passed
            </p>
            <StackBar parts={Object.entries(s.labels ?? {}).map(([label, c]) => ({ label, count: c }))} />
          </li>
        ))}
      </ul>
    </section>
  );
}

export function DashboardPage() {
  const f = useFactory();
  // Sending work needs the user role, like POST /factories/{uid}/runs.
  const { me } = useAuth();
  const shell = useShell();
  const [params, setParams] = useSearchParams();
  const rangeKey = RANGES.some((r) => r.key === params.get("range")) ? params.get("range")! : "30d";
  const days = RANGES.find((r) => r.key === rangeKey)!.days;
  const metrics = useQuery({
    queryKey: ["metrics", f.id, rangeKey],
    queryFn: () => unwrap(api.GET("/factories/{uid}/metrics", { params: { path: { uid: f.id }, query: { since: `${days}d` } } })),
  });
  const recent = useQuery({
    queryKey: ["workitems", f.id, "recent"],
    queryFn: () => unwrap(api.GET("/factories/{uid}/work-items", { params: { path: { uid: f.id }, query: { limit: 8 } } })).then((r) => r.items ?? []),
  });
  const integrations = useQuery({
    queryKey: ["integrations"],
    queryFn: () => unwrap(api.GET("/integrations")).then((r) => r.items ?? []),
    retry: false,
  });
  // The same query as the Settings page: the repositories of the active definition.
  const settings = useQuery({
    queryKey: ["settings", f.id, f.active_version_id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/settings", { params: { path: { uid: f.id } } })),
    retry: false,
  });
  // Team and factory secrets: GET /secrets?factory_id= alone lists only factory-scoped ones.
  const secrets = useUsableSecrets(f.id);
  // A code host is connected when an active record that applies to this
  // factory serves each of its repositories (the server rule of forgeutil).
  const repos = settings.data?.repositories ?? [];
  const gaps = integrations.data && settings.data ? coverageGaps(repos, integrations.data, f.id) : [];
  const hasForge = !!integrations.data && (settings.data && repos.length > 0 ? gaps.length === 0 : hasApplicableForge(integrations.data, f.id));
  const credential = secrets.data ? factoryCredentialState(secrets.data) : undefined;
  // A factory without work shows what to do first, in place of empty figures.
  const fresh = recent.data !== undefined && recent.data.length === 0;

  return (
    <>
      <PageHeader
        title="Overview"
        crumbs={[{ label: f.name, to: fpath(f) }]}
        actions={
          <>
            {!fresh && <Segmented label="Date range" value={rangeKey} onChange={(v) => setParams({ range: v })} options={RANGES.map((r) => ({ id: r.key, label: r.short }))} />}
            {!!me && (
              <Button variant="primary" onClick={() => shell.openComposer(f.id)}>
                New work item
              </Button>
            )}
          </>
        }
      />
      <div className="page-head">
        <h2 className="page-title">{f.name}</h2>
        <div className="page-meta">
          <span>
            Foreman <strong>@{f.alias}</strong>
          </span>
          <span>{f.source === "git" ? `Definition in ${f.git_repo}` : "Managed definition"}</span>
          {f.description && <span>{f.description}</span>}
        </div>
      </div>
      {fresh && (
        <section aria-label="Get started" className="stack stack-sm narrow" style={{ alignSelf: "flex-start" }}>
          <h2 className="section-title">Get started</h2>
          <ol className="checklist">
            <li className={hasForge ? "done" : ""}>
              <StatusIcon state={hasForge ? "complete" : "intake"} />
              <span className="step-text">
                <strong>Connect a code host</strong>
                <span className="muted small">The factory needs it to read the repositories and open pull requests.</span>
                {describeGaps(gaps).map((line) => (
                  <span key={line} className="small">
                    {line}
                  </span>
                ))}
              </span>
              {!hasForge && (
                <Link to="/integrations" className="btn btn-sm">
                  Connect
                </Link>
              )}
            </li>
            <li>
              <StatusIcon state="intake" />
              <span className="step-text">
                <strong>Review the agents</strong>
                <span className="muted small">Set the model and the instructions of the foreman and its agents.</span>
              </span>
              <Link to={fpath(f, "agents")} className="btn btn-sm">
                Open agents
              </Link>
            </li>
            <li>
              <StatusIcon state="intake" />
              <span className="step-text">
                <strong>Send the first request</strong>
                <span className="muted small">Describe one small, verifiable change. @{f.alias || "foreman"} takes it from there.</span>
              </span>
              {!!me && (
                <Button size="sm" variant="primary" onClick={() => shell.openComposer(f.id)}>
                  New work item
                </Button>
              )}
            </li>
          </ol>
          {credential && !secrets.isLoading && credential.kind === "none" && (
            <p className="muted small">
              A run needs a model credential. If the server does not provide one, add it under <Link to="/secrets">Secrets</Link>.
            </p>
          )}
          {credential && !secrets.isLoading && credential.kind === "ambiguous" && (
            <p className="muted small">
              More than one {credential.scope} secret can be the model credential ({credential.names.join(", ")}), so runs get none. Set harness.auth on the agents, or keep one of them under{" "}
              <Link to="/secrets">Secrets</Link>.
            </p>
          )}
        </section>
      )}
      {!fresh && <SpendCard fid={f.id} />}
      {fresh ? null : metrics.isError ? (
        <ErrorAlert error={metrics.error} title="Metrics are unavailable" />
      ) : (
        <Query q={metrics}>
          {(r) => (
            <>
              <MetricsView m={r.metrics} />
              <ScorerCards fid={f.id} scorers={r.metrics.scorers ?? []} />
            </>
          )}
        </Query>
      )}
      {!fresh && (
        <section aria-label="Recent work" className="bleed">
          <div className="section-head" style={{ paddingInline: "var(--gutter)" }}>
            <h2 className="section-title">Recent work</h2>
            <Link to={fpath(f, "work")} className="small">
              All work items
            </Link>
          </div>
          <div style={{ borderTop: "1px solid var(--border)" }}>
            <Query q={recent}>{(items) => <WorkTable items={items} fid={f.id} />}</Query>
          </div>
        </section>
      )}
    </>
  );
}
