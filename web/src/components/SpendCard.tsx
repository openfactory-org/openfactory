import { useQuery } from "@tanstack/react-query";
import { api, unwrap } from "../api/client";
import type { operations } from "../api/schema";
import { money, percent } from "../lib/format";
import { Card, Metric } from "./ui";

type SpendReport = operations["getFactorySpend"]["responses"]["200"]["content"]["application/json"];
type SpendLimit = NonNullable<SpendReport["limits"]>[number];

const WARN_AT = 0.8;

function limitLabel(l: SpendLimit): string {
  const who = l.scope === "team" ? "Team" : "Factory";
  return `${who} ${l.period === "monthly" ? "monthly" : "daily"} limit`;
}

function SpendMeter({ l }: { l: SpendLimit }) {
  const spent = l.spent_usd ?? 0;
  const limit = l.limit_usd ?? 0;
  const ratio = limit > 0 ? spent / limit : 0;
  const tone = ratio >= 1 ? "bad" : ratio >= WARN_AT ? "warn" : "ok";
  const resets = l.resets_at ? new Date(l.resets_at).toLocaleString(undefined, { timeZone: "UTC", dateStyle: "medium", timeStyle: "short" }) : "";
  return (
    <li className="spend-limit">
      <div className="spend-limit-head">
        <span>{limitLabel(l)}</span>
        <span className="mono">
          {money(spent)} of {money(limit)}
        </span>
      </div>
      <div
        className={`spend-meter spend-${tone}`}
        role="meter"
        aria-label={limitLabel(l)}
        aria-valuemin={0}
        aria-valuemax={limit}
        aria-valuenow={Math.min(spent, limit)}
        aria-valuetext={`${percent(ratio)} used`}
      >
        <span style={{ width: `${Math.min(100, Math.round(ratio * 100))}%` }} />
      </div>
      <div className="metric-hint muted">
        {ratio >= 1 ? "Reached. New runs wait in the queue; running runs stop at their next turn end." : `${percent(ratio)} used`}
        {resets && ` · resets ${resets} UTC`}
      </div>
    </li>
  );
}

/** SpendCard shows the model spend of a factory and its spend limits (GET /factories/{uid}/spend). */
export function SpendCard({ fid }: { fid: string }) {
  const q = useQuery({
    queryKey: ["spend", fid],
    queryFn: () => unwrap(api.GET("/factories/{uid}/spend", { params: { path: { uid: fid } } })),
    refetchInterval: 60_000,
    retry: false,
  });
  if (!q.data) return null; // the route is optional; hide the card when it fails
  const rep = q.data;
  const limits = rep.limits ?? [];
  return (
    <Card title="Spend">
      <ul className="spend-row">
        <Metric label="Today (UTC)" value={money(rep.factory_daily_usd)} />
        <Metric label="This month (UTC)" value={money(rep.factory_monthly_usd)} />
      </ul>
      {limits.length > 0 ? (
        <ul className="spend-limits">
          {limits.map((l) => (
            <SpendMeter key={`${l.scope}-${l.period}`} l={l} />
          ))}
        </ul>
      ) : (
        <p className="muted small">
          No spend limit. Set <code>spendLimit.daily</code> or <code>spendLimit.monthly</code> in factory.yaml.
        </p>
      )}
    </Card>
  );
}
