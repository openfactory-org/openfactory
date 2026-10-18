import {
  Activity,
  BarChart3,
  Bot,
  Boxes,
  CircleDot,
  FileCode2,
  FlaskConical,
  Gauge,
  Inbox,
  KeyRound,
  Plug,
  Server,
  Settings,
  SquareTerminal,
  Webhook,
  Workflow,
  Zap,
  type LucideIcon,
} from "lucide-react";
import type { Factory } from "../api/client";

export type NavItem = { label: string; to: string; icon: LucideIcon; end?: boolean };

/** Pages of the team that people use every day. */
export const WORK_NAV: NavItem[] = [
  { label: "Inbox", to: "/inbox", icon: Inbox },
  { label: "Runs", to: "/runs", icon: Activity },
];

/** Team resources that an editor or an admin sets up. */
export const TEAM_NAV: NavItem[] = [
  { label: "Secrets", to: "/secrets", icon: KeyRound },
  { label: "Integrations", to: "/integrations", icon: Plug },
  { label: "Environments", to: "/environments", icon: Boxes },
  { label: "MCP servers", to: "/mcp-servers", icon: SquareTerminal },
  { label: "Workers", to: "/workers", icon: Server },
];

export type FactoryNav = { work: NavItem[]; configure: NavItem[]; measure: NavItem[] };

/** The pages of one factory, in three groups. */
export function factoryNav(f: Factory): FactoryNav {
  const base = `/f/${encodeURIComponent(f.id)}`;
  const configure: NavItem[] = [
    { label: "Agents", to: `${base}/agents`, icon: Bot },
    { label: "Automations", to: `${base}/automations`, icon: Zap },
    { label: "Webhooks", to: `${base}/webhooks`, icon: Webhook },
  ];
  if (f.source !== "git") configure.push({ label: "Factory definition", to: `${base}/definition`, icon: FileCode2 });
  configure.push({ label: "Integrations", to: `${base}/integrations`, icon: Plug }, { label: "Settings", to: `${base}/settings`, icon: Settings });
  return {
    work: [
      { label: "Overview", to: base, icon: BarChart3, end: true },
      { label: "Work items", to: `${base}/work`, icon: CircleDot },
      { label: "Inbox", to: `${base}/inbox`, icon: Inbox },
      { label: "Runs", to: `${base}/runs`, icon: Activity },
    ],
    configure,
    measure: [
      { label: "Scorers", to: `${base}/scorers`, icon: Gauge },
      { label: "Benchmarks", to: `${base}/benchmarks`, icon: FlaskConical },
    ],
  };
}

export const FACTORY_ICON = Workflow;

const DOT_COLORS = ["#5b67d8", "#12a17a", "#d97706", "#9b4dca", "#d6457b", "#2f8fbf", "#64748b"];

/** A stable colour for a factory, from its id. */
export function factoryColor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return DOT_COLORS[h % DOT_COLORS.length];
}
