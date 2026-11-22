// Black-box contract suite helpers (WS1). Run the suite with
// tools/contract/run.mjs --target worker|go.
export const CONTRACT_SUITE = "openfactory-contract";
export { Api, ApiError, expectProblem, waitFor } from "./harness/api.ts";
export { FakeRunner, type TaskEnv } from "./harness/runner.ts";
export { FakeWorker, newWorkerKey, signChallenge } from "./harness/worker.ts";
export { SseStream } from "./harness/sse.ts";
export { TAGS, type Tag, type TargetName } from "./harness/target.ts";
