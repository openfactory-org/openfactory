# src/lib: Durable Object base

Shared code for every control-plane DO: migrations, the outbox, timers on one
alarm, the DO side of live updates, and list pagination. Owner: WS4. The API
below is stable; ask WS4 before you change it.

## BaseDO

```ts
import { BaseDO } from "../../lib/base";
import type { Migration } from "../../lib/sql/migrate";
import m0001 from "./migrations/0001_init"; // export default `CREATE TABLE ...`;

export class WorkDO extends BaseDO implements WorkRpc {
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    // Register handlers in the constructor.
    this.onMessage("dispatch.grant", (p, msg) => { /* sync, inside the transaction */ });
    this.onTimer("liveness", (t) => { /* sync, inside the transaction */ });
    this.onTimerAsync("poll", async (t) => { /* network I/O allowed */ });
  }
  protected migrations(): readonly Migration[] {
    return [{ version: 1, name: "init", sql: m0001 }];
  }
  async runsCancel(meta: CallMeta, runId: string): Promise<unknown> {
    return this.rpc(() =>
      this.tx(() => {
        // reads + writes, no await
        this.enqueue({ dest: dest.dispatch(this.teamId()), kind: "dispatch.cancel", lane: "dispatch",
                       msgId: `${runId}:1:cancel`, payload: { runId, session: 1 } });
        this.setTimer(`deadline:${runId}`, "deadline", Date.now() + 60_000, { runId });
        return { ok: true };
      }),
    );
  }
}
```

| Member | Use |
|---|---|
| `migrations()` | Ordered list of the DO's own migrations (scope `do`). Applied once each, in `blockConcurrencyWhile`, each in its own `transactionSync`. Base tables (scope `lib`): `meta`, `outbox`, `inbox_dedupe`, `timers`, `schema_migrations`. |
| `init()` | Optional hook after migrations, before the first event. |
| `tx(fn)` | One `transactionSync`, then re-arms the alarm. No `await` inside. |
| `onCommit(fn)` | Runs `fn` after the current step (`tx`, a received message, a sync timer) commits. A rollback drops it. A nested `tx` is a savepoint: its callbacks join the outer step. |
| `enqueue({dest, kind, payload, msgId?, lane?, delayMs?})` | Outbox row. Call inside `tx` with the state change. Same `msgId` twice is ignored. |
| `setTimer(name, kind, dueMs, payload?)`, `clearTimer(name)`, `this.timers` | Named timers on the one alarm (`setEarliest`, `clearPrefix`, `get`). Each `set` gives the row a new `seq`; the alarm deletes or backs off only the row version it ran, so a handler can set its own timer again, also to the same `dueMs`. |
| `onTimer(kind, fn)` / `onTimerAsync(kind, fn)` | Timer handler in a transaction / outside (reschedule from the handler to repeat). A throwing handler is retried with backoff (1 s, doubling, cap 5 min). |
| `onMessage(kind, fn)` | Outbox kind this DO receives. `receive(msg)` (the `OutboxReceiver` RPC) dedupes on `msgId` and runs `fn` in one transaction with the dedupe row. Return `{retry: true, afterMs}` to roll back and keep the sender's row pending. |
| `seen(msgId)`, `markSeen(msgId)` | Dedupe helpers for custom receive flows. Rows are purged after 7 days. |
| `deliverCustom(envelope, row)` | Override for custom destinations (providers, WorkerHubDO calls with extra logic). |
| `onDeadLetter(row)` | Default: enqueue `index.deadLetter` to the team IndexDO. |
| `outboxRedrive(msgId)` | RPC: reset a dead row. |
| `ensureAlarm()` | RPC for the Cron safety net. |
| `rpc(fn)` | Wraps an RPC body so an `OfError` keeps its code over RPC (`toOfError` on the caller side). |
| `doName()`, `teamId()` | DO name (`team:<id>|...`) and its team. |

`alarm()` runs due timers (one transaction each), drains the outbox, catches every
error and always sets the next alarm. A row written with `next_ms = now` makes the
alarm fire at once, so delivery is kicked at commit.

## Outbox (`src/lib/outbox/`)

- Destinations (`dest.*`): `dest.team(t)`, `dest.index(t)`, `dest.dispatch(t)`, `dest.router(t)`,
  `dest.work(t, token)`, `dest.ingress(t, partition)`, `dest.workerHub(t, workerId)`,
  `dest.benchmark(t, token)` (all `do:<BINDING>:<name>`, delivered with `receive`), `dest.jobs()`
  (`queue:JOBS`, payload is the `JobMessage`). Any other string goes to `deliverCustom`.
- Lanes: rows of one non-empty lane go in id order; a dead row does not block its lane.
- Backoff 1 s x 2^n, cap 5 min, jitter +-20 %, dead after 12 attempts. `{retry:true}` does not count.
- A payload above 512 KiB goes to R2 (`outbox/<sha256>`) at delivery and arrives as `payloadR2`;
  `BaseDO.receive` loads it.
- Delivery is at least once. Receivers dedupe on `msgId`.

## SQL (`src/lib/sql/`)

- `applyMigrations(storage, scope, list)`, `schemaSnapshot(sql)` for migration tests.
- `insertMany(sql, table, cols, rows, {verb, suffix})`: chunked under 100 bound parameters and 100 KB of text.
- `chunks(list)` for `IN (...)` lists. `all`, `first`, `run`, `parseData` helpers.
- Migration files: `src/do/<name>/migrations/NNNN_<name>.ts` with `export default \`...SQL...\``.
  TeamDO modules list theirs in the module's `migrations` export (number ranges in 05 §4.5).

## Live (`src/lib/live/`)

`new LiveHub(ctx)`: `accept(tags, attachment?, onOpen?)` answers a WebSocket upgrade from the Worker
with a hibernatable socket; `broadcast(tag, frame, filter?)` sends a `LiveFrame`. The Worker-side
long-poll and SSE helpers are in `src/stream/` (WS5). IndexDO serves `/subscribe/team?factory_id=&topics=`.

## Pages (`src/lib/page.ts`)

`pageFrom(query)`, `sqlPage(sql, {select, where, args, timeCol?, idCol?}, page)`, `paginate(items, key, page)`,
`listResponse(items, next, extra?)`, `encodeCursor(ms, id)` / `decodeCursor`, `msOf(rfc3339)`, `isoOf(ms)`.
Same cursor format as Go (`base64url("<unix-nanos>:<id>")`).

## TeamDO modules

See `src/do/team/index.ts`: each module exports `migrations`, `ops` (keyed by operationId) and
optional `setup(kit)`. `TeamKit` gives `sql`, `env`, `tx`, `enqueue`, timers and message handlers.
An op receives `OpCall` (`params`, `query`, `body`, `principal()`, `authorize(factoryId)`); team-wide
routes are authorized before the op runs, factory-scoped routes must call `authorize`. Return a body
(200) or `created(body)`, `noContent()`, `withStatus(n, body)`. Worker handlers call it with
`teamApi(...)` from `src/http/handler.ts`.

## Testing DOs

- A timer or outbox row due now (or in the past) makes the runtime fire the alarm on its own, right
  after the transaction. Do not assert that a row is still in the outbox, or that
  `runDurableObjectAlarm` returns true: poll for the effect instead (see `eventually` in
  `src/lib/lib.test.ts`).
- Storage is shared by the test files of one run (no isolated storage in Vitest 4). Use unique
  names, and `test/helpers.ts` `bootstrap()` for an admin key of the default team.
