import { Command } from "cmdk";
import { useNavigate } from "react-router-dom";
import { Info, Keyboard, LogOut, Monitor, Moon, Plus, SquarePen, Sun } from "lucide-react";
import type { Factory } from "../api/client";
import { FACTORY_ICON, factoryNav, TEAM_NAV, WORK_NAV } from "./nav";
import { Keys } from "./ui";

export type Theme = "system" | "light" | "dark";

/**
 * The command palette: go to a page, start an action. Opens with ⌘K.
 * The pages of the active factory come first.
 */
export function CommandPalette({
  open,
  onOpenChange,
  factories,
  activeFactory,
  canCreateFactory,
  onCompose,
  onShortcuts,
  onTheme,
  onSignOut,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  factories: Factory[];
  activeFactory?: Factory;
  canCreateFactory: boolean;
  onCompose: () => void;
  onShortcuts: () => void;
  onTheme: (t: Theme) => void;
  onSignOut: () => void;
}) {
  const navigate = useNavigate();
  const run = (fn: () => void) => () => {
    onOpenChange(false);
    fn();
  };
  const go = (to: string) => run(() => navigate(to));
  const active = activeFactory ? factoryNav(activeFactory) : null;

  return (
    <Command.Dialog open={open} onOpenChange={onOpenChange} label="Command palette" overlayClassName="dialog-overlay" contentClassName="dialog dialog-top palette">
      <Command.Input placeholder="Type a command or search…" />
      <Command.List>
        <Command.Empty>No results.</Command.Empty>
        <Command.Group heading="Actions">
          <Command.Item onSelect={run(onCompose)}>
            <SquarePen size={16} aria-hidden="true" />
            New work item
            <span className="menu-right">
              <Keys k="C" />
            </span>
          </Command.Item>
          {canCreateFactory && (
            <Command.Item onSelect={go("/factories/new")}>
              <Plus size={16} aria-hidden="true" />
              New factory
            </Command.Item>
          )}
        </Command.Group>
        {active && activeFactory && (
          <Command.Group heading={activeFactory.name}>
            {[...active.work, ...active.configure, ...active.measure].map((n) => (
              <Command.Item key={n.to} value={`${activeFactory.name} ${n.label}`} onSelect={go(n.to)}>
                <n.icon size={16} aria-hidden="true" />
                {n.label}
              </Command.Item>
            ))}
          </Command.Group>
        )}
        <Command.Group heading="Go to">
          {WORK_NAV.map((n) => (
            <Command.Item key={n.to} value={`go ${n.label}`} onSelect={go(n.to)}>
              <n.icon size={16} aria-hidden="true" />
              {n.label}
              <span className="menu-right">
                <Keys k={`G ${n.label[0]}`} />
              </span>
            </Command.Item>
          ))}
          <Command.Item value="go all factories" onSelect={go("/factories")}>
            <FACTORY_ICON size={16} aria-hidden="true" />
            All factories
            <span className="menu-right">
              <Keys k="G F" />
            </span>
          </Command.Item>
          {factories
            .filter((f) => f.id !== activeFactory?.id)
            .map((f) => (
              <Command.Item key={f.id} value={`factory ${f.name} ${f.alias ?? ""}`} onSelect={go(`/f/${encodeURIComponent(f.id)}/work`)}>
                <FACTORY_ICON size={16} aria-hidden="true" />
                {f.name}
              </Command.Item>
            ))}
        </Command.Group>
        <Command.Group heading="Team settings">
          {TEAM_NAV.map((n) => (
            <Command.Item key={n.to} value={`team ${n.label}`} onSelect={go(n.to)}>
              <n.icon size={16} aria-hidden="true" />
              {n.label}
            </Command.Item>
          ))}
        </Command.Group>
        <Command.Group heading="Preferences">
          <Command.Item value="theme light" onSelect={run(() => onTheme("light"))}>
            <Sun size={16} aria-hidden="true" />
            Light theme
          </Command.Item>
          <Command.Item value="theme dark" onSelect={run(() => onTheme("dark"))}>
            <Moon size={16} aria-hidden="true" />
            Dark theme
          </Command.Item>
          <Command.Item value="theme system" onSelect={run(() => onTheme("system"))}>
            <Monitor size={16} aria-hidden="true" />
            System theme
          </Command.Item>
          <Command.Item value="keyboard shortcuts help" onSelect={run(onShortcuts)}>
            <Keyboard size={16} aria-hidden="true" />
            Keyboard shortcuts
            <span className="menu-right">
              <Keys k="?" />
            </span>
          </Command.Item>
          <Command.Item value="about version" onSelect={go("/about")}>
            <Info size={16} aria-hidden="true" />
            About Open Factory
          </Command.Item>
          <Command.Item value="sign out log out" onSelect={run(onSignOut)}>
            <LogOut size={16} aria-hidden="true" />
            Sign out
          </Command.Item>
        </Command.Group>
      </Command.List>
    </Command.Dialog>
  );
}
