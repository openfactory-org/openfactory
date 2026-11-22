// Pagination over list endpoints. Port of the iterators of pkg/sdk (iter.go).

import type { components } from "./schema";

export type PageInfo = components["schemas"]["PageInfo"];

/** The body of each list answer: {"items": [...], "page_info": {...}}. */
export interface ListPage<T> {
  items: T[] | null;
  page_info?: PageInfo;
}

/** Fetches one page; cursor is "" for the first page. */
export type PageFetcher<T> = (cursor: string, signal?: AbortSignal) => Promise<ListPage<T>>;

/** A lazy walk over every page of a list endpoint. */
export interface Paginator<T> extends AsyncIterable<T> {
  /** Iterates items; the signal aborts page fetches. */
  items(signal?: AbortSignal): AsyncGenerator<T>;
  /** Iterates pages. */
  pages(signal?: AbortSignal): AsyncGenerator<T[]>;
  /** Drains every item into an array. */
  collect(signal?: AbortSignal): Promise<T[]>;
}

/**
 * A paginator over a page fetcher. It stops after a page without a
 * next_cursor, or after an empty page without a cursor.
 */
export function paginate<T>(fetchPage: PageFetcher<T>): Paginator<T> {
  async function* pages(signal?: AbortSignal): AsyncGenerator<T[]> {
    let cursor = "";
    for (;;) {
      const page = await fetchPage(cursor, signal);
      const items = page.items ?? [];
      const next = page.page_info?.next_cursor ?? "";
      if (items.length > 0) yield items;
      if (next === "" || next === cursor) return;
      cursor = next;
    }
  }
  async function* items(signal?: AbortSignal): AsyncGenerator<T> {
    for await (const p of pages(signal)) yield* p;
  }
  return {
    pages,
    items,
    async collect(signal?: AbortSignal): Promise<T[]> {
      const out: T[] = [];
      for await (const it of items(signal)) out.push(it);
      return out;
    },
    [Symbol.asyncIterator]: () => items(),
  };
}
