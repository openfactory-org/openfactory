// Stand-in for the "cloudflare:workers" module in Node unit tests.
export class DurableObject<Env = unknown> {
  constructor(
    readonly ctx: DurableObjectState,
    readonly env: Env,
  ) {}
}

export class WorkerEntrypoint<Env = unknown, Props = unknown> {
  constructor(
    readonly ctx: ExecutionContext<Props>,
    readonly env: Env,
  ) {}
}
