/** Small dependency-free SVG charts (bar and line) for the metrics pages. */

export type Series = { name: string; values: number[] };

const W = 480;
const H = 160;
const PAD = { l: 36, r: 8, t: 8, b: 22 };

function niceMax(v: number): number {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

function fmt(v: number, unit?: string): string {
  if (unit === "$") return `$${v >= 100 ? v.toFixed(0) : v.toFixed(2)}`;
  return v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(Math.round(v * 100) / 100);
}

export function Chart({
  title,
  kind,
  labels,
  series,
  unit,
}: {
  title: string;
  kind: "bar" | "line";
  labels: string[];
  series: Series[];
  unit?: string;
}) {
  const n = labels.length;
  const max = niceMax(Math.max(0, ...series.flatMap((s) => s.values)));
  const iw = W - PAD.l - PAD.r;
  const ih = H - PAD.t - PAD.b;
  const x = (i: number) => PAD.l + (n <= 1 ? iw / 2 : (i * iw) / (n - 1));
  const y = (v: number) => PAD.t + ih - (v / max) * ih;
  const total = series.reduce((a, s) => a + s.values.reduce((x2, v) => x2 + v, 0), 0);
  const summary = `${title}: ${series.map((s) => `${s.name} total ${fmt(s.values.reduce((a, v) => a + v, 0), unit)}`).join(", ")}`;
  const tickEvery = Math.max(1, Math.ceil(n / 6));
  const bw = n > 0 ? Math.max(2, (iw / n) * 0.7) / Math.max(1, series.length) : 0;

  return (
    <figure className="card chart-card">
      <figcaption className="card-title">{title}</figcaption>
      {total === 0 ? (
        <p className="muted small chart-empty">No data in this range.</p>
      ) : (
        <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={summary} className="chart">
          {[0, 0.5, 1].map((f) => (
            <g key={f}>
              <line x1={PAD.l} x2={W - PAD.r} y1={y(max * f)} y2={y(max * f)} className="chart-grid" />
              <text x={PAD.l - 4} y={y(max * f) + 3} textAnchor="end" className="chart-axis">
                {fmt(max * f, unit)}
              </text>
            </g>
          ))}
          {labels.map((l, i) =>
            i % tickEvery === 0 ? (
              <text key={i} x={kind === "bar" ? PAD.l + ((i + 0.5) * iw) / n : x(i)} y={H - 6} textAnchor="middle" className="chart-axis">
                {l}
              </text>
            ) : null,
          )}
          {series.map((s, si) =>
            kind === "line" ? (
              <polyline key={s.name} fill="none" className={`chart-line c${si}`} points={s.values.map((v, i) => `${x(i)},${y(v)}`).join(" ")} />
            ) : (
              <g key={s.name} className={`chart-bar c${si}`}>
                {s.values.map((v, i) => (
                  <rect key={i} x={PAD.l + (i * iw) / n + ((iw / n) * 0.15) + si * bw} y={y(v)} width={bw} height={Math.max(0, PAD.t + ih - y(v))}>
                    <title>{`${labels[i]}: ${s.name} ${fmt(v, unit)}`}</title>
                  </rect>
                ))}
              </g>
            ),
          )}
        </svg>
      )}
      {series.length > 1 && (
        <ul className="legend">
          {series.map((s, i) => (
            <li key={s.name}>
              <span className={`swatch c${i}`} aria-hidden="true" />
              {s.name}
            </li>
          ))}
        </ul>
      )}
    </figure>
  );
}

/** Stacked proportional bar with legend (scorer label breakdowns). */
export function StackBar({ parts }: { parts: { label: string; count: number }[] }) {
  const total = parts.reduce((a, p) => a + p.count, 0);
  if (total === 0) return null;
  return (
    <>
      <div className="bar-stack" role="img" aria-label={parts.map((p) => `${p.label}: ${p.count}`).join(", ")}>
        {parts.map((p, i) => (
          <span key={p.label} className={`seg c${i % 6}`} style={{ width: `${(p.count / total) * 100}%` }} title={`${p.label}: ${p.count}`} />
        ))}
      </div>
      <ul className="legend">
        {parts.map((p, i) => (
          <li key={p.label}>
            <span className={`swatch c${i % 6}`} aria-hidden="true" />
            {p.label} ({p.count})
          </li>
        ))}
      </ul>
    </>
  );
}
