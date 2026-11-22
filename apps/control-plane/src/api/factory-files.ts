// REST handlers: factory-files. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factory-files/schemas: List schema versions (owner DO: Worker (static)). */
export const listSchemaVersions: Handler = notImplementedHandler;

/** GET /api/v1/factory-files/schemas/{version}: All schema documents of a version, keyed by document name (owner DO: Worker (static)). */
export const getSchemaVersion: Handler = notImplementedHandler;

/** GET /api/v1/factory-files/schemas/{version}/{document}: One schema document (owner DO: Worker (static)). */
export const getSchemaDocument: Handler = notImplementedHandler;
