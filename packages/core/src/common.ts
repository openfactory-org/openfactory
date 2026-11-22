// Shapes and helpers that every domain module uses: times, list pages and
// closed lists of values.

/** An RFC 3339 time string. */
export type Timestamp = string;

export interface ListOpts {
  limit?: number;
  cursor?: string;
  ascending?: boolean;
}

/** The list page shape of every list endpoint. */
export interface PageInfo {
  has_next_page: boolean;
  next_cursor?: string;
}

export interface ListPage<T> {
  items: T[];
  page_info: PageInfo;
}

/** Reports whether v is one of the values of a closed list, and narrows it to that list's type. */
export function isOneOf<T extends string>(values: readonly T[], v: string): v is T {
  return (values as readonly string[]).includes(v);
}
