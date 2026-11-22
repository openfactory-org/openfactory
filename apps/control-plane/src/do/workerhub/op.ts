// Result helpers for TeamDO.api calls. They live in src/lib/rpc, which does
// not import the TeamDO class and its route table (an import cycle through
// src/http/routes.ts).
export { isOpError, type OpError as OpErrorResult } from "../../lib/rpc";
