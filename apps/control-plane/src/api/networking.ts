// REST handler: GET /networking/policies. The built-in policy catalog.
import { catalog } from "@openfactory/core";
import { json, type Handler } from "../http/handler";

export const listNetworkPolicies: Handler = async () => json(catalog());
