// Opt-in live end-to-end scenarios (WS1). Tests skip without their env vars.
export const E2E_SUITE = "openfactory-e2e";
export { api, describeLive, fixture, renameFactory, waitFor, waitRunDone } from "./live.ts";
