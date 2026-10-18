import { createContext, useContext } from "react";

/** Actions of the app shell that a page can start. Outside the shell they do nothing. */
export interface Shell {
  /** Opens the "New work item" dialog, with a factory selected when given. */
  openComposer: (factoryID?: string) => void;
  openPalette: () => void;
}

export const ShellContext = createContext<Shell>({ openComposer: () => {}, openPalette: () => {} });

export function useShell(): Shell {
  return useContext(ShellContext);
}
