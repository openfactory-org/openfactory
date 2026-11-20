import { Fragment, useId, useSyncExternalStore, type ButtonHTMLAttributes, type ReactNode } from "react";
import { Link } from "react-router-dom";
import type { UseQueryResult } from "@tanstack/react-query";
import * as RTooltip from "@radix-ui/react-tooltip";
import { ChevronRight, Menu as MenuIcon } from "lucide-react";
import { ApiError, errorMessage, formatValidationError, type ValidationError } from "../api/client";
import { ago, fmtTime, stateLabel } from "../lib/format";
import { useDocumentTitle } from "../lib/title";

/** Event that opens the sidebar on small screens (the shell listens for it). */
export const NAV_TOGGLE_EVENT = "of:toggle-nav";

/**
 * The bar at the top of every page: breadcrumbs, the page title and the page
 * actions. With `display`, the bar keeps the breadcrumbs and the title moves
 * into the page as a large heading (detail pages). The header also sets the
 * browser tab title; pass `docTitle` when `title` is not plain text.
 */
export function PageHeader({
  title,
  subtitle,
  crumbs,
  actions,
  display,
  docTitle,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  crumbs?: { label: string; to: string }[];
  actions?: ReactNode;
  display?: boolean;
  docTitle?: string;
}) {
  useDocumentTitle(docTitle ?? (typeof title === "string" ? title : undefined));
  return (
    <>
      <header className="page-bar">
        <button type="button" className="btn btn-icon nav-toggle" aria-label="Open navigation" onClick={() => window.dispatchEvent(new Event(NAV_TOGGLE_EVENT))}>
          <MenuIcon size={16} aria-hidden="true" />
        </button>
        <div className="page-bar-main">
          {crumbs && crumbs.length > 0 && (
            <nav className="crumbs" aria-label="Breadcrumb">
              {crumbs.map((c) => (
                <Fragment key={c.to}>
                  <Link to={c.to}>{c.label}</Link>
                  <ChevronRight size={12} className="crumb-sep" aria-hidden="true" />
                </Fragment>
              ))}
            </nav>
          )}
          {display ? <span className="truncate muted" aria-hidden="true">{typeof title === "string" ? title : null}</span> : <h1>{title}</h1>}
        </div>
        {actions && <div className="actions">{actions}</div>}
      </header>
      {display ? (
        <div className="page-head">
          <h1 className="page-title">{title}</h1>
          {subtitle && <div className="page-meta">{subtitle}</div>}
        </div>
      ) : (
        subtitle && <div className="page-desc">{subtitle}</div>
      )}
    </>
  );
}

export function Card({ title, children, actions, className }: { title?: ReactNode; children: ReactNode; actions?: ReactNode; className?: string }) {
  const id = useId();
  return (
    <section className={`card ${className ?? ""}`} aria-labelledby={title ? id : undefined}>
      {(title || actions) && (
        <div className="card-head">
          {title && (
            <h2 id={id} className="card-title">
              {title}
            </h2>
          )}
          {actions}
        </div>
      )}
      {children}
    </section>
  );
}

type Variant = "primary" | "secondary" | "danger" | "danger-solid" | "ghost" | "link";

export function Button({
  variant = "secondary",
  size,
  busy,
  children,
  className,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: "sm"; busy?: boolean }) {
  return (
    <button
      type="button"
      className={`btn btn-${variant} ${size ? `btn-${size}` : ""} ${className ?? ""}`}
      disabled={busy || rest.disabled}
      aria-busy={busy || undefined}
      {...rest}
    >
      {children}
    </button>
  );
}

/** Keyboard hint, for example <Keys k="G I" /> or <Keys k="⌘ ↵" />. */
export function Keys({ k }: { k: string }) {
  return (
    <span className="kbd-group" aria-hidden="true">
      {k.split(" ").map((key, i) => (
        <kbd key={i} className="kbd">
          {key}
        </kbd>
      ))}
    </span>
  );
}

export function Tooltip({ label, keys, children, side }: { label: ReactNode; keys?: string; children: ReactNode; side?: "top" | "right" | "bottom" | "left" }) {
  return (
    <RTooltip.Provider delayDuration={350}>
      <RTooltip.Root>
        <RTooltip.Trigger asChild>{children}</RTooltip.Trigger>
        <RTooltip.Portal>
          <RTooltip.Content className="tooltip" side={side} sideOffset={6}>
            {label}
            {keys && <Keys k={keys} />}
          </RTooltip.Content>
        </RTooltip.Portal>
      </RTooltip.Root>
    </RTooltip.Provider>
  );
}

/** A button that shows only an icon. `label` is its accessible name and its tooltip. */
export function IconButton({
  label,
  keys,
  children,
  className,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { label: string; keys?: string }) {
  return (
    <Tooltip label={label} keys={keys}>
      <button type="button" className={`btn btn-icon ${className ?? ""}`} aria-label={label} {...rest}>
        {children}
      </button>
    </Tooltip>
  );
}

const TONES: Record<string, string> = {
  queued: "neutral",
  pending: "neutral",
  claimed: "info",
  starting: "info",
  running: "info",
  blocked: "warn",
  error: "bad",
  idle: "warn",
  succeeded: "ok",
  complete: "ok",
  accepted: "ok",
  online: "ok",
  failed: "bad",
  rejected: "bad",
  offline: "neutral",
  cancelled: "neutral",
  deduplicated: "neutral",
  awaiting_approval: "warn",
  handoff: "warn",
  approval: "warn",
  question: "info",
  failure: "bad",
  review: "info",
};

export function Badge({ children, tone }: { children: ReactNode; tone?: string }) {
  const key = typeof children === "string" ? children.toLowerCase() : "";
  return <span className={`badge badge-${tone ?? TONES[key] ?? "neutral"}`}>{children}</span>;
}

type IconKind = "idle" | "progress" | "run" | "wait" | "done" | "fail" | "off";

/** How a run state, a work item stage or a resource state is drawn. */
const ICONS: Record<string, { kind: IconKind; fill?: number }> = {
  intake: { kind: "idle" },
  queued: { kind: "idle" },
  pending: { kind: "idle" },
  triage: { kind: "progress", fill: 0.2 },
  planning: { kind: "progress", fill: 0.4 },
  building: { kind: "progress", fill: 0.6 },
  reviewing: { kind: "progress", fill: 0.8 },
  handoff: { kind: "progress", fill: 0.92 },
  claimed: { kind: "run" },
  starting: { kind: "run" },
  running: { kind: "run" },
  awaiting_approval: { kind: "wait" },
  blocked: { kind: "wait" },
  idle: { kind: "wait" },
  succeeded: { kind: "done" },
  complete: { kind: "done" },
  accepted: { kind: "done" },
  online: { kind: "done" },
  failed: { kind: "fail" },
  error: { kind: "fail" },
  rejected: { kind: "fail" },
  cancelled: { kind: "off" },
  offline: { kind: "off" },
  deduplicated: { kind: "off" },
};

/** A 14px icon for a run state or a work item stage. Decorative: pair it with a label. */
export function StatusIcon({ state, size = 14 }: { state?: string | null; size?: number }) {
  const { kind, fill = 0 } = ICONS[(state ?? "").toLowerCase()] ?? { kind: "idle" as IconKind };
  const common = { width: size, height: size, viewBox: "0 0 14 14", "aria-hidden": true, focusable: false } as const;
  switch (kind) {
    case "progress": {
      const c = 2 * Math.PI * 2.5;
      return (
        <svg {...common} className="status-icon">
          <circle cx="7" cy="7" r="6" fill="none" stroke="var(--st-active)" strokeWidth="1.5" />
          <circle cx="7" cy="7" r="2.5" fill="none" stroke="var(--st-active)" strokeWidth="5" strokeDasharray={`${fill * c} ${c}`} transform="rotate(-90 7 7)" />
        </svg>
      );
    }
    case "run":
      return (
        <svg {...common} className="status-icon spin">
          <circle cx="7" cy="7" r="6" fill="none" stroke="var(--st-run)" strokeOpacity="0.3" strokeWidth="1.5" />
          <path className="arc" d="M7 1a6 6 0 0 1 6 6" fill="none" stroke="var(--st-run)" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case "wait":
      return (
        <svg {...common} className="status-icon">
          <circle cx="7" cy="7" r="7" fill="var(--st-wait)" />
          <path d="M5.4 4.4v5.2M8.6 4.4v5.2" stroke="var(--surface)" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case "done":
      return (
        <svg {...common} className="status-icon">
          <circle cx="7" cy="7" r="7" fill="var(--st-done)" />
          <path d="M4.2 7.2 6.2 9.2 9.9 5" fill="none" stroke="var(--surface)" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case "fail":
      return (
        <svg {...common} className="status-icon">
          <circle cx="7" cy="7" r="7" fill="var(--st-fail)" />
          <path d="m4.8 4.8 4.4 4.4M9.2 4.8 4.8 9.2" stroke="var(--surface)" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case "off":
      return (
        <svg {...common} className="status-icon">
          <circle cx="7" cy="7" r="7" fill="var(--st-idle)" />
          <path d="M4.4 7h5.2" stroke="var(--surface)" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    default:
      return (
        <svg {...common} className="status-icon">
          <circle cx="7" cy="7" r="6" fill="none" stroke="var(--st-idle)" strokeWidth="1.5" strokeDasharray="2.2 2.2" />
        </svg>
      );
  }
}

/** A state or a stage, as an icon with its name. */
export function StateBadge({ state }: { state?: string | null }) {
  if (!state) return null;
  const label = stateLabel(state);
  return (
    <span className="state">
      <StatusIcon state={state} />
      <span className="state-label">{label}</span>
    </span>
  );
}

export function Empty({ title, children, icon, action }: { title: string; children?: ReactNode; icon?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      {icon && (
        <div className="empty-icon" aria-hidden="true">
          {icon}
        </div>
      )}
      <p className="empty-title">{title}</p>
      {children && <div>{children}</div>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  );
}

export function ErrorAlert({ error, title }: { error: unknown; title?: string }) {
  if (!error) return null;
  const errs = error instanceof ApiError ? error.validationErrors : [];
  if (errs.length === 0) {
    return (
      <div className="alert alert-error" role="alert">
        {title && <strong>{title}: </strong>}
        {errorMessage(error)}
      </div>
    );
  }
  // A refused definition change: list the reasons, one per line.
  const shown = errs.slice(0, 5);
  return (
    <div className="alert alert-error" role="alert">
      {title && <strong>{title}: </strong>}
      {(error as ApiError).message}
      <ul className="validation-list">
        {shown.map((e, i) => (
          <li key={i}>{formatValidationError(e)}</li>
        ))}
      </ul>
      {errs.length > shown.length && <p className="muted">And {errs.length - shown.length} more.</p>}
    </div>
  );
}

/** True when the error is an API answer 404 (the resource does not exist or the caller cannot see it). */
export function isNotFound(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404;
}

export type NotFoundInfo = { title: string; text: ReactNode; back: { label: string; to: string } };

/** The page for a resource that does not exist, for example a deleted factory. */
export function NotFound({ title, text, back }: NotFoundInfo) {
  return (
    <>
      <PageHeader title={title} />
      <div className="empty">
        <p>{text}</p>
        <div className="empty-action">
          <Link className="btn btn-secondary" to={back.to}>
            {back.label}
          </Link>
        </div>
      </div>
    </>
  );
}

export function Alert({ kind = "info", children }: { kind?: "info" | "warning" | "error" | "success"; children: ReactNode }) {
  return (
    <div className={`alert alert-${kind}`} role={kind === "error" ? "alert" : "status"}>
      {children}
    </div>
  );
}

export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <p className="loading muted" role="status">
      <span className="spinner" aria-hidden="true" /> {label}
    </p>
  );
}

const SKELETON_WIDTHS = ["72%", "54%", "63%", "41%", "58%"];

/** Placeholder lines shown while a list or a page loads. */
export function Skeleton({ rows = 4, label = "Loading…" }: { rows?: number; label?: string }) {
  return (
    <div className="skeleton" role="status">
      <span className="sr-only">{label}</span>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton-line" style={{ width: SKELETON_WIDTHS[i % SKELETON_WIDTHS.length] }} />
      ))}
    </div>
  );
}

/**
 * Renders loading / error states of a query, then `children(data)`. With
 * `notFound`, an API 404 shows the not-found page instead of the error.
 */
export function Query<T>({ q, children, loading, notFound }: { q: UseQueryResult<T>; children: (data: T) => ReactNode; loading?: string; notFound?: NotFoundInfo }) {
  if (q.isLoading) return <Skeleton label={loading} />;
  if (q.isError && notFound && isNotFound(q.error)) return <NotFound {...notFound} />;
  if (q.isError) return <ErrorAlert error={q.error} />;
  if (q.data === undefined) return null;
  return <>{children(q.data)}</>;
}

export function Field({
  label,
  hint,
  children,
  htmlFor,
}: {
  label: ReactNode;
  hint?: ReactNode;
  children: ReactNode;
  htmlFor?: string;
}) {
  return (
    <div className="field">
      <label htmlFor={htmlFor}>{label}</label>
      {children}
      {hint && <p className="hint">{hint}</p>}
    </div>
  );
}

export function KV({ items }: { items: [ReactNode, ReactNode][] }) {
  return (
    <dl className="kv">
      {items.map(([k, v], i) => (
        <div key={i} className="kv-row">
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

/** How often relative times such as "2 minutes ago" advance. */
export const CLOCK_TICK_MS = 30_000;

// One timer serves every mounted TimeAgo. It runs only while one is mounted.
const clockListeners = new Set<() => void>();
let clockNow = Date.now();
let clockTimer: ReturnType<typeof setInterval> | undefined;

function subscribeClock(fn: () => void): () => void {
  clockListeners.add(fn);
  if (clockTimer === undefined) {
    clockNow = Date.now();
    clockTimer = setInterval(() => {
      clockNow = Date.now();
      clockListeners.forEach((l) => l());
    }, CLOCK_TICK_MS);
  }
  return () => {
    clockListeners.delete(fn);
    if (clockListeners.size === 0 && clockTimer !== undefined) {
      clearInterval(clockTimer);
      clockTimer = undefined;
    }
  };
}

/** The current time, updated every CLOCK_TICK_MS by a timer that all callers share. */
export function useNow(): number {
  return useSyncExternalStore(subscribeClock, () => clockNow);
}

export function TimeAgo({ t }: { t?: string | null }) {
  useNow(); // re-render on each tick so the text advances
  if (!t) return <span className="muted">—</span>;
  return (
    <time dateTime={t} title={fmtTime(t)}>
      {ago(t)}
    </time>
  );
}

export function ValidationList({ errors, title = "Validation errors" }: { errors?: ValidationError[]; title?: string }) {
  if (!errors || errors.length === 0) return null;
  return (
    <div className="alert alert-error" role="alert">
      <strong>{title}</strong>
      <ul className="validation-list">
        {errors.map((e, i) => (
          <li key={i}>
            <code>
              {e.file}
              {e.line ? `:${e.line}` : ""}
            </code>
            {e.field ? ` ${e.field}` : ""}: {e.message}
          </li>
        ))}
      </ul>
    </div>
  );
}

export function Tabs<T extends string>({ value, onChange, tabs, label }: { value: T; onChange: (v: T) => void; tabs: { id: T; label: string }[]; label: string }) {
  return (
    <div className="tabs" role="tablist" aria-label={label}>
      {tabs.map((t) => (
        <button
          key={t.id}
          type="button"
          role="tab"
          aria-selected={value === t.id}
          className={`tab ${value === t.id ? "tab-active" : ""}`}
          onClick={() => onChange(t.id)}
        >
          {t.label}
        </button>
      ))}
    </div>
  );
}

/** A small set of exclusive choices, shown as joined buttons. */
export function Segmented<T extends string>({ value, onChange, options, label }: { value: T; onChange: (v: T) => void; options: { id: T; label: string }[]; label: string }) {
  return (
    <div className="segmented" role="group" aria-label={label}>
      {options.map((o) => (
        <button key={o.id} type="button" aria-pressed={value === o.id} onClick={() => onChange(o.id)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Code({ children }: { children: ReactNode }) {
  return <pre className="code">{children}</pre>;
}

export function Metric({ label, value, hint }: { label: string; value: ReactNode; hint?: ReactNode }) {
  return (
    <li className="card metric">
      <span className="metric-label">{label}</span>
      <span className="metric-value">{value}</span>
      {hint && <span className="metric-hint muted">{hint}</span>}
    </li>
  );
}

/** Plain text with `inline code` spans, for agent-written text that uses Markdown backticks. */
export function InlineText({ text }: { text: string }) {
  const parts = text.split(/(`[^`\n]+`)/g);
  return (
    <>
      {parts.map((p, i) =>
        p.length > 2 && p.startsWith("`") && p.endsWith("`") ? (
          <code key={i}>
            {p.slice(1, -1)}
          </code>
        ) : (
          p
        ),
      )}
    </>
  );
}
