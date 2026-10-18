import { useEffect, useState, type ReactNode } from "react";
import * as RDialog from "@radix-ui/react-dialog";
import * as RMenu from "@radix-ui/react-dropdown-menu";
import { X } from "lucide-react";
import { Button } from "./ui";

/** A modal dialog with a title, an optional description and a footer. */
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  className,
  hideTitle,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  className?: string;
  hideTitle?: boolean;
}) {
  return (
    <RDialog.Root open={open} onOpenChange={onOpenChange}>
      <RDialog.Portal>
        <RDialog.Overlay className="dialog-overlay" />
        <RDialog.Content className={`dialog ${className ?? ""}`} aria-describedby={description ? undefined : undefined}>
          <RDialog.Title className={hideTitle ? "sr-only" : "dialog-title"}>{title}</RDialog.Title>
          {description ? <RDialog.Description className="dialog-desc">{description}</RDialog.Description> : <RDialog.Description className="sr-only">{title}</RDialog.Description>}
          {children}
          {footer && <div className="dialog-footer">{footer}</div>}
          {!hideTitle && (
            <RDialog.Close asChild>
              <button type="button" className="btn btn-icon dialog-close" aria-label="Close">
                <X size={16} aria-hidden="true" />
              </button>
            </RDialog.Close>
          )}
        </RDialog.Content>
      </RDialog.Portal>
    </RDialog.Root>
  );
}

export type ConfirmOptions = {
  title: string;
  body?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** The action destroys or stops something: the confirm button is red. */
  danger?: boolean;
};

type Pending = ConfirmOptions & { resolve: (ok: boolean) => void };

let host: ((p: Pending) => void) | null = null;

/**
 * Asks the user to confirm an action. Resolves true when the user confirms.
 * Without a mounted <ConfirmHost />, it uses the browser dialog.
 */
export function confirm(opts: ConfirmOptions): Promise<boolean> {
  if (!host) {
    const text = typeof opts.body === "string" ? `${opts.title}\n\n${opts.body}` : opts.title;
    return Promise.resolve(typeof window !== "undefined" && typeof window.confirm === "function" ? !!window.confirm(text) : false);
  }
  return new Promise((resolve) => host!({ ...opts, resolve }));
}

/** Renders the dialog of confirm(). Mount it one time, near the root. */
export function ConfirmHost() {
  const [pending, setPending] = useState<Pending | null>(null);
  useEffect(() => {
    host = setPending;
    return () => {
      host = null;
    };
  }, []);
  const close = (ok: boolean) => {
    pending?.resolve(ok);
    setPending(null);
  };
  return (
    <Dialog
      open={!!pending}
      onOpenChange={(o) => !o && close(false)}
      title={pending?.title ?? ""}
      description={pending?.body}
      footer={
        <>
          <Button onClick={() => close(false)}>{pending?.cancelLabel ?? "Cancel"}</Button>
          <Button variant={pending?.danger ? "danger-solid" : "primary"} onClick={() => close(true)} autoFocus>
            {pending?.confirmLabel ?? "Confirm"}
          </Button>
        </>
      }
    />
  );
}

export const Menu = RMenu.Root;
export const MenuTrigger = RMenu.Trigger;

export function MenuContent({ children, align = "start", side }: { children: ReactNode; align?: "start" | "center" | "end"; side?: "top" | "right" | "bottom" | "left" }) {
  return (
    <RMenu.Portal>
      <RMenu.Content className="menu" align={align} side={side} sideOffset={4} collisionPadding={8}>
        {children}
      </RMenu.Content>
    </RMenu.Portal>
  );
}

export function MenuItem({
  children,
  onSelect,
  icon,
  right,
  danger,
  disabled,
}: {
  children: ReactNode;
  onSelect?: () => void;
  icon?: ReactNode;
  right?: ReactNode;
  danger?: boolean;
  disabled?: boolean;
}) {
  return (
    <RMenu.Item className={`menu-item ${danger ? "danger" : ""}`} onSelect={onSelect} disabled={disabled}>
      {icon}
      <span>{children}</span>
      {right && <span className="menu-right">{right}</span>}
    </RMenu.Item>
  );
}

export function MenuLabel({ children }: { children: ReactNode }) {
  return <RMenu.Label className="menu-label">{children}</RMenu.Label>;
}

export function MenuSep() {
  return <RMenu.Separator className="menu-sep" />;
}
