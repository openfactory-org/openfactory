// @openfactory/worker-proto: worker protocol v2 frame types, validators and helpers.
// src/generated/ is written by `npm run gen:protos` from schemas/worker/v2. Do not edit it.
export const WORKER_PROTOCOL_VERSION = 2;
/** WebSocket subprotocol of GET /api/v1/workers/connect. */
export const WORKER_SUBPROTOCOL = "openfactory.worker.v2";

export type * from "./generated/index";
export * from "./validate";
export * from "./frames";
