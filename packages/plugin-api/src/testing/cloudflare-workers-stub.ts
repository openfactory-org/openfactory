// Stand-in for "cloudflare:workers" in node tests (aliased by the vitest configs).

export class WorkerEntrypoint<E = unknown> {
  constructor(
    readonly ctx: unknown,
    readonly env: E,
  ) {}
}

export class RpcTarget {}

export class DurableObject<E = unknown> {
  constructor(
    readonly ctx: unknown,
    readonly env: E,
  ) {}
}

export const env = {};
