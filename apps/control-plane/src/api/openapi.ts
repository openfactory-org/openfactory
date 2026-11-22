// REST handler: GET /openapi.json (public, WS4). Serves the bundled document.
import doc from "../../../../api/openapi.json";
import { json, type Handler } from "../http/handler";

export const getOpenAPI: Handler = async () => json(doc, 200, { "cache-control": "public, max-age=300" });
