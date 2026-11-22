// The task index (OfTaskRegistry): the platform has no API that lists Durable
// Object instances or containers, so the provider indexes tasks for `list`.

import type { KvLike } from "./task-sandbox";

const PREFIX = "task:";

export interface RegistryPage {
  taskIds: string[];
  cursor?: string;
}

/** A KV with prefix listing (ctx.storage.kv). */
export interface ListableKv extends KvLike {
  list<T>(options?: { prefix?: string; startAfter?: string; limit?: number }): Iterable<[string, T]>;
}

export class Registry {
  constructor(private readonly kv: ListableKv) {}

  add(taskId: string, now: number): void {
    if (this.kv.get(PREFIX + taskId) === undefined) this.kv.put(PREFIX + taskId, { addedAt: now });
  }

  remove(taskId: string): void {
    this.kv.delete(PREFIX + taskId);
  }

  list(cursor?: string, limit = 500): RegistryPage {
    const opts: { prefix: string; limit: number; startAfter?: string } = { prefix: PREFIX, limit: limit + 1 };
    if (cursor) opts.startAfter = PREFIX + cursor;
    const ids = [...this.kv.list(opts)].map(([k]) => k.slice(PREFIX.length));
    const page: RegistryPage = { taskIds: ids.slice(0, limit) };
    if (ids.length > limit) page.cursor = page.taskIds.at(-1)!;
    return page;
  }
}
