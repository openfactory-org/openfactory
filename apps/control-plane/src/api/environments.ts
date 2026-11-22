// REST handlers: environments (WS4). Forwarded to TeamDO.api with the route's operationId.
import { teamApi, type Handler } from "../http/handler";

export const listEnvironments: Handler = teamApi();
export const createEnvironment: Handler = teamApi();
export const getEnvironment: Handler = teamApi();
export const updateEnvironment: Handler = teamApi();
export const deleteEnvironment: Handler = teamApi();
