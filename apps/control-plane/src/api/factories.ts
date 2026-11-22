// REST handlers: factories (WS4). Forwarded to TeamDO.api with the route's
// operationId; the TeamDO authorizes with the factory role.
import { notImplementedHandler, teamApi, type Handler } from "../http/handler";

export const listFactories: Handler = teamApi();
export const createFactory: Handler = teamApi();
export const getFactory: Handler = teamApi();
export const updateFactory: Handler = teamApi();
export const deleteFactory: Handler = teamApi();
export const listAgents: Handler = teamApi();
export const getAgent: Handler = teamApi();
export const listAutomations: Handler = teamApi();
export const getFactoryIntegrations: Handler = teamApi();
export const putFactoryIntegrations: Handler = teamApi();
export const getFactorySettings: Handler = teamApi();
export const getAgentSpec: Handler = teamApi();
export const putAgentSpec: Handler = teamApi();
export const getAutomation: Handler = teamApi();
export const putAutomation: Handler = teamApi();
export const deleteAutomation: Handler = teamApi();
export const validateAutomation: Handler = teamApi();

/**
 * POST /factories/{uid}/test-filter. Needs a stored webhook delivery
 * (IngressDO) and the webhook record (WS10 module); not ported yet.
 */
export const testFactoryFilter: Handler = notImplementedHandler;
