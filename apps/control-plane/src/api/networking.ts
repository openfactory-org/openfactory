// REST handler: GET /networking/policies (WS4). The built-in policy catalog.
import { networkPolicy } from "@openfactory/definition";
import { json, type Handler } from "../http/handler";

export const listNetworkPolicies: Handler = async () => json(networkPolicy.catalog());
