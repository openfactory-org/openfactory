import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { NavLink, Outlet, useLocation, useMatch, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, ChevronRight, ChevronsUpDown, Info, Keyboard, LogOut, Monitor, Moon, Plus, Search, SquarePen, Sun } from "lucide-react";
import { api, apiURL, unwrap, type Factory } from "../api/client";
import { useEventStream } from "../api/sse";
import { useAuth } from "../auth/AuthContext";
import { FACTORIES_KEY, MOD, useFactories, useInboxCount } from "../lib/queries";
import { BrandLogo } from "./BrandMark";
import { CommandPalette, type Theme } from "./CommandPalette";
import { ComposerDialog } from "./Composer";
import { factoryColor, factoryNav, TEAM_NAV, WORK_NAV, type NavItem } from "./nav";
import { Dialog, Menu, MenuContent, MenuItem, MenuLabel, MenuSep, MenuTrigger } from "./overlays";
import { ShellContext } from "./shell";
import { IconButton, Keys, NAV_TOGGLE_EVENT } from "./ui";

export { FACTORIES_KEY, useFactories, useInboxCount };

function readTheme(): Theme {
  try {
    const t = localStorage.getItem("of-theme");
    return t === "light" || t === "dark" ? t : "system";
  } catch {
    return "system";
  }
}

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(readTheme);
  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", theme);
    try {
      localStorage.setItem("of-theme", theme);
    } catch {
      /* private mode */
    }
  }, [theme]);
  return [theme, setTheme];
}

/** A boolean that is kept in localStorage, for example an open sidebar group. */
function useStored(key: string, initial: boolean): [boolean, (v: boolean) => void] {
  const [v, setV] = useState(() => {
    try {
      const s = localStorage.getItem(key);
      return s === null ? initial : s === "1";
    } catch {
      return initial;
    }
  });
  const set = useCallback(
    (next: boolean) => {
      setV(next);
      try {
        localStorage.setItem(key, next ? "1" : "0");
      } catch {
        /* private mode */
      }
    },
    [key],
  );
  return [v, set];
}

/** Invalidates cached queries when the team's live stream reports changes. */
function LiveUpdates() {
  const qc = useQueryClient();
  useEventStream(apiURL("/stream", { topics: "inbox,workitem,run,definition" }), {
    inbox: () => void qc.invalidateQueries({ queryKey: ["inbox"] }),
    workitem: (data) => {
      const wi = data as { id?: string; factory_id?: string };
      void qc.invalidateQueries({ queryKey: ["workitems", wi.factory_id] });
      if (wi.id) void qc.invalidateQueries({ queryKey: ["workitem", wi.id] });
    },
    run: (data) => {
      const r = data as { run_id?: string };
      void qc.invalidateQueries({ queryKey: ["runs"] });
      if (r.run_id) void qc.invalidateQueries({ queryKey: ["run", r.run_id] });
    },
    definition: (data) => {
      const f = data as { id?: string };
      void qc.invalidateQueries({ queryKey: FACTORIES_KEY });
      if (f.id) void qc.invalidateQueries({ queryKey: ["factory", f.id] });
    },
  });
  return null;
}

/** `active` replaces the match of the router, for a page that is outside the path of its link. */
function SideLink({ item, badge, active }: { item: NavItem; badge?: number; active?: boolean }) {
  return (
    <li>
      <NavLink to={item.to} end={item.end} className={({ isActive }) => `nav-link ${(active ?? isActive) ? "active" : ""}`}>
        <item.icon size={15} aria-hidden="true" />
        <span className="nav-label">{item.label}</span>
        {badge ? (
          <span className="nav-badge" aria-label={`${badge} waiting`}>
            {badge > 99 ? "99+" : badge}
          </span>
        ) : null}
      </NavLink>
    </li>
  );
}

function FactoryItem({ factory, active, onRun }: { factory: Factory; active: boolean; onRun: boolean }) {
  const [open, setOpen] = useState(active);
  useEffect(() => {
    if (active) setOpen(true);
  }, [active]);
  const nav = factoryNav(factory);
  return (
    <li>
      <button type="button" className="nav-link" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
        <span className="factory-dot" style={{ background: factoryColor(factory.id) }} aria-hidden="true">
          {factory.name.slice(0, 1)}
        </span>
        <span className="nav-label">{factory.name}</span>
        <ChevronRight size={13} className="nav-caret" aria-hidden="true" />
      </button>
      {open && (
        <ul className="nav-list nav-sub" aria-label={`${factory.name} pages`}>
          {nav.work.map((n) => (
            <SideLink key={n.to} item={n} active={onRun && n.label === "Runs" ? true : undefined} />
          ))}
          <li className="nav-group-label">Configure</li>
          {nav.configure.map((n) => (
            <SideLink key={n.to} item={n} />
          ))}
          <li className="nav-group-label">Measure</li>
          {nav.measure.map((n) => (
            <SideLink key={n.to} item={n} />
          ))}
        </ul>
      )}
    </li>
  );
}

const SHORTCUTS: [string, string][] = [
  ["Open the command palette", `${MOD} K`],
  ["New work item", "C"],
  ["Search the page", "/"],
  ["Go to the inbox", "G I"],
  ["Go to runs", "G R"],
  ["Go to all factories", "G F"],
  ["Go to the work items of the factory", "G W"],
  ["Go to the overview of the factory", "G O"],
  ["Move in a list", "J K"],
  ["Open the selected row", "↵"],
  ["Send a message or a request", `${MOD} ↵`],
  ["Show this list", "?"],
];

function isTyping(el: EventTarget | null): boolean {
  const n = el as HTMLElement | null;
  if (!n || !n.tagName) return false;
  return n.tagName === "INPUT" || n.tagName === "TEXTAREA" || n.tagName === "SELECT" || n.isContentEditable;
}

export function Layout() {
  const { me, signOut, canEdit } = useAuth();
  const factories = useFactories();
  const inbox = useInboxCount();
  const [theme, setTheme] = useTheme();
  const [navOpen, setNavOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  const [composer, setComposer] = useState<{ open: boolean; fid?: string }>({ open: false });
  const [teamOpen, setTeamOpen] = useStored("of-nav-team", false);
  const navigate = useNavigate();
  const location = useLocation();
  const mainRef = useRef<HTMLElement>(null);
  const factoryMatch = useMatch("/f/:fid/*");
  const runMatch = useMatch("/runs/:id");
  // A run page is outside /f/:fid. Its run tells which factory to keep open.
  const runID = runMatch?.params.id ?? "";
  const run = useQuery({
    queryKey: ["run", runID],
    enabled: !!runID,
    queryFn: () => unwrap(api.GET("/runs/{runId}", { params: { path: { runId: runID } } })),
  });
  const activeFid = factoryMatch?.params.fid ?? (runID ? (run.data?.factory_id ?? undefined) : undefined);
  const activeFactory = (factories.data ?? []).find((f) => f.id === activeFid);
  const onTeamPage = TEAM_NAV.some((n) => location.pathname === n.to || location.pathname.startsWith(`${n.to}/`));
  const version = useQuery({ queryKey: ["version"], queryFn: () => unwrap(api.GET("/version")), staleTime: Infinity });

  const openComposer = useCallback((fid?: string) => setComposer({ open: true, fid }), []);
  const shell = useMemo(() => ({ openComposer, openPalette: () => setPaletteOpen(true) }), [openComposer]);

  // A new page starts at its top, and the mobile sidebar closes.
  useEffect(() => {
    mainRef.current?.scrollTo?.(0, 0);
    setNavOpen(false);
  }, [location.pathname]);

  useEffect(() => {
    const toggle = () => setNavOpen((o) => !o);
    window.addEventListener(NAV_TOGGLE_EVENT, toggle);
    return () => window.removeEventListener(NAV_TOGGLE_EVENT, toggle);
  }, []);

  const stateRef = useRef({ activeFid, firstFid: factories.data?.[0]?.id });
  stateRef.current = { activeFid, firstFid: factories.data?.[0]?.id };
  useEffect(() => {
    let pendingG = 0;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((o) => !o);
        return;
      }
      if (e.metaKey || e.ctrlKey || e.altKey || isTyping(e.target)) return;
      if (document.querySelector("[role=dialog]")) return;
      const key = e.key.toLowerCase();
      if (pendingG && Date.now() - pendingG < 1200) {
        pendingG = 0;
        const fid = stateRef.current.activeFid ?? stateRef.current.firstFid;
        const to: Record<string, string | undefined> = {
          i: "/inbox",
          r: "/runs",
          f: "/factories",
          w: fid && `/f/${encodeURIComponent(fid)}/work`,
          o: fid && `/f/${encodeURIComponent(fid)}`,
        };
        if (to[key]) {
          e.preventDefault();
          navigate(to[key]!);
        }
        return;
      }
      if (key === "g") {
        pendingG = Date.now();
      } else if (key === "c") {
        e.preventDefault();
        setComposer({ open: true, fid: stateRef.current.activeFid });
      } else if (e.key === "?") {
        e.preventDefault();
        setShortcutsOpen(true);
      } else if (key === "j" || key === "k") {
        const rows = Array.from(mainRef.current?.querySelectorAll<HTMLElement>("[data-row]") ?? []);
        if (rows.length === 0) return;
        const at = rows.indexOf(document.activeElement as HTMLElement);
        const next = at < 0 ? 0 : Math.max(0, Math.min(rows.length - 1, at + (key === "j" ? 1 : -1)));
        e.preventDefault();
        rows[next].focus();
        rows[next].scrollIntoView?.({ block: "nearest" });
      } else if (e.key === "/") {
        const search = mainRef.current?.querySelector<HTMLInputElement>('input[type="search"]');
        e.preventDefault();
        if (search) search.focus();
        else setPaletteOpen(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [navigate]);

  const themeItem = (t: Theme, label: string, Icon: typeof Sun) => (
    <MenuItem icon={<Icon size={15} aria-hidden="true" />} onSelect={() => setTheme(t)} right={theme === t ? <Check size={14} aria-label="selected" /> : undefined}>
      {label}
    </MenuItem>
  );

  return (
    <ShellContext.Provider value={shell}>
      <div className="app">
        <a href="#main" className="skip-link">
          Skip to content
        </a>
        <LiveUpdates />
        <nav id="sidebar" className={`sidebar ${navOpen ? "open" : ""}`} aria-label="Main">
          <div className="ws-row">
            <Menu>
              <MenuTrigger asChild>
                <button type="button" className="ws-button" aria-label={`${me?.team.name ?? "Team"} menu`}>
                  <BrandLogo size={18} />
                  <span className="grow" />
                  <ChevronsUpDown size={13} className="chev" aria-hidden="true" />
                </button>
              </MenuTrigger>
              <MenuContent>
                <MenuLabel>
                  {me?.team.name} · {me?.user.name || me?.user.email} · {me?.role}
                </MenuLabel>
                <MenuSep />
                {themeItem("system", "System theme", Monitor)}
                {themeItem("light", "Light theme", Sun)}
                {themeItem("dark", "Dark theme", Moon)}
                <MenuSep />
                <MenuItem icon={<Keyboard size={15} aria-hidden="true" />} onSelect={() => setShortcutsOpen(true)} right={<Keys k="?" />}>
                  Keyboard shortcuts
                </MenuItem>
                <MenuItem icon={<Info size={15} aria-hidden="true" />} onSelect={() => navigate("/about")}>
                  About
                </MenuItem>
                <MenuSep />
                <MenuItem icon={<LogOut size={15} aria-hidden="true" />} onSelect={() => void signOut()}>
                  Sign out
                </MenuItem>
              </MenuContent>
            </Menu>
            <IconButton label="Search and commands" keys={`${MOD} K`} onClick={() => setPaletteOpen(true)}>
              <Search size={15} aria-hidden="true" />
            </IconButton>
          </div>
          <div className="side-actions">
            <button type="button" className="btn btn-compose" onClick={() => openComposer(activeFid)}>
              <SquarePen size={14} aria-hidden="true" />
              <span className="grow" style={{ textAlign: "left" }}>
                New work item
              </span>
              <Keys k="C" />
            </button>
          </div>
          <ul className="nav-list">
            {WORK_NAV.map((n) => (
              <SideLink key={n.to} item={n} badge={n.to === "/inbox" ? inbox.data : undefined} active={n.to === "/runs" && runID && activeFid ? false : undefined} />
            ))}
          </ul>
          <div className="nav-section">
            <NavLink to="/factories" className="muted">
              Factories
            </NavLink>
            {canEdit && (
              <IconButton label="New factory" onClick={() => navigate("/factories/new")}>
                <Plus size={14} aria-hidden="true" />
              </IconButton>
            )}
          </div>
          <ul className="nav-list">
            {(factories.data ?? []).map((f) => (
              <FactoryItem key={f.id} factory={f} active={activeFid === f.id} onRun={!!runID && activeFid === f.id} />
            ))}
            {factories.data?.length === 0 && (
              <li className="nav-group-label">No factories yet</li>
            )}
          </ul>
          <div className="nav-section">
            <button type="button" className="btn-link-plain" aria-expanded={teamOpen || onTeamPage} onClick={() => setTeamOpen(!teamOpen)}>
              Team settings
            </button>
          </div>
          {(teamOpen || onTeamPage) && (
            <ul className="nav-list" aria-label="Team settings">
              {TEAM_NAV.map((n) => (
                <SideLink key={n.to} item={n} />
              ))}
            </ul>
          )}
          <p className="sidebar-foot">
            <NavLink to="/about">Open Factory {version.data?.version ?? ""}</NavLink>
          </p>
        </nav>
        <button type="button" className="sidebar-scrim" aria-label="Close navigation" onClick={() => setNavOpen(false)} />
        <div className="main-wrap">
          <main id="main" className="main" tabIndex={-1} ref={mainRef}>
            <Outlet />
          </main>
        </div>
        <ComposerDialog open={composer.open} onOpenChange={(open) => setComposer((c) => ({ ...c, open }))} factoryID={composer.fid} />
        <CommandPalette
          open={paletteOpen}
          onOpenChange={setPaletteOpen}
          factories={factories.data ?? []}
          activeFactory={activeFactory}
          canCreateFactory={canEdit}
          onCompose={() => openComposer(activeFid)}
          onShortcuts={() => setShortcutsOpen(true)}
          onTheme={setTheme}
          onSignOut={() => void signOut()}
        />
        <Dialog open={shortcutsOpen} onOpenChange={setShortcutsOpen} title="Keyboard shortcuts">
          <dl className="shortcut-list">
            {SHORTCUTS.map(([label, k]) => (
              <div key={label} style={{ display: "contents" }}>
                <dt>{label}</dt>
                <dd>
                  <Keys k={k} />
                </dd>
              </div>
            ))}
          </dl>
        </Dialog>
      </div>
    </ShellContext.Provider>
  );
}
