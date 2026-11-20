import { createContext, useContext, useEffect } from "react";

/** The name of the product, the last part of every browser tab title. */
export const APP_TITLE = "Open Factory";

/**
 * The scope of the pages below it, for example the factory name. The factory
 * layout sets it, so a page title reads "Agents · Payments · Open Factory".
 */
export const TitleScopeContext = createContext<string | undefined>(undefined);

/** Joins the parts of a tab title with " · ". Empty parts and repeats of the previous part are dropped. */
export function documentTitle(parts: (string | null | undefined)[]): string {
  const out: string[] = [];
  for (const p of [...parts, APP_TITLE]) {
    const t = p?.trim();
    if (t && out[out.length - 1] !== t) out.push(t);
  }
  return out.join(" · ");
}

/** Sets the browser tab title to "<page> · <scope> · Open Factory" while the caller is mounted. */
export function useDocumentTitle(page?: string | null) {
  const scope = useContext(TitleScopeContext);
  const title = documentTitle([page, scope]);
  useEffect(() => {
    document.title = title;
    return () => {
      document.title = APP_TITLE;
    };
  }, [title]);
}
