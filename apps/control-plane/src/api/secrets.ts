// REST handlers: secrets (WS4). Forwarded to TeamDO.api with the route's operationId.
import { teamApi, type Handler } from "../http/handler";

export const listSecrets: Handler = teamApi();
export const createSecret: Handler = teamApi();
export const rewrapSecrets: Handler = teamApi();
export const getSecret: Handler = teamApi();
export const updateSecret: Handler = teamApi();
export const deleteSecret: Handler = teamApi();
