// Cached name lists of the name resolvers (port of integrations/namelist.go).
export const LIST_TTL_MS = 5 * 60_000;
export const MIN_REFRESH_MS = 30_000;
export const ERROR_BACKOFF_MS = 30_000;
export const STALE_MAX_MS = 60 * 60_000;

export type NameMap = Record<string, string[]>;
type Fetcher = () => Promise<NameMap>;

interface Entry {
  byName?: NameMap;
  fetched: number;
  err?: Error;
  errAt: number;
  /** Serializes the reads of one list. */
  chain: Promise<unknown>;
}

export class NameLists {
  private readonly lists = new Map<string, Entry>();
  constructor(private readonly now: () => number = Date.now) {}

  /** Reads the list when the cache is older than maxAge. After a failed read, returns the old list (up to STALE_MAX_MS old) or the error. */
  get(key: string, maxAgeMs: number, fetch: Fetcher): Promise<NameMap> {
    let l = this.lists.get(key);
    if (!l) {
      l = { fetched: 0, errAt: 0, chain: Promise.resolve() };
      this.lists.set(key, l);
    }
    const entry = l;
    const run = entry.chain.then(() => this.read(entry, maxAgeMs, fetch));
    entry.chain = run.catch(() => undefined);
    return run;
  }

  private async read(l: Entry, maxAgeMs: number, fetch: Fetcher): Promise<NameMap> {
    const now = this.now();
    if (l.byName && now - l.fetched < maxAgeMs) return l.byName;
    if (l.err && now - l.errAt < ERROR_BACKOFF_MS) return this.stale(l, now, l.err);
    try {
      const byName = await fetch();
      l.byName = byName;
      l.fetched = now;
      l.err = undefined;
      return byName;
    } catch (e) {
      l.err = e as Error;
      l.errAt = now;
      return this.stale(l, now, e as Error);
    }
  }

  private stale(l: Entry, now: number, err: Error): NameMap {
    if (l.byName && now - l.fetched < STALE_MAX_MS) return l.byName;
    throw err;
  }

  /** Get with the rules of a name check: reads again once when a wanted key is missing and the list is older than MIN_REFRESH_MS. */
  async lookup(key: string, keys: string[], fetch: Fetcher): Promise<NameMap> {
    const byName = await this.get(key, LIST_TTL_MS, fetch);
    for (const k of keys) {
      if (!(k in byName)) {
        try {
          return await this.get(key, MIN_REFRESH_MS, fetch);
        } catch {
          break;
        }
      }
    }
    return byName;
  }
}

/** Adds id to the entry of name, lower case and trimmed. An empty name or ID is left out. */
export function addName(m: NameMap, name: string, id: string): void {
  const k = name.trim().toLowerCase();
  if (k !== "" && id !== "") addId(m, k, id);
}

export function addId(m: NameMap, key: string, id: string): void {
  const list = (m[key] ??= []);
  if (!list.includes(id)) list.push(id);
}

export function sortIds(m: NameMap): void {
  for (const k of Object.keys(m)) m[k] = [...m[k]!].sort();
}
