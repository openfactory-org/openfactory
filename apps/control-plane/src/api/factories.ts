// REST handlers: factories. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories: List factories (owner DO: TeamDO). */
export const listFactories: Handler = notImplementedHandler;

/** POST /api/v1/factories: Create a factory (owner DO: TeamDO). */
export const createFactory: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}: Get a factory (owner DO: TeamDO). */
export const getFactory: Handler = notImplementedHandler;

/** PATCH /api/v1/factories/{uid}: Update a factory (owner DO: TeamDO). */
export const updateFactory: Handler = notImplementedHandler;

/** DELETE /api/v1/factories/{uid}: Delete a factory (owner DO: TeamDO). */
export const deleteFactory: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/agents: List agents (owner DO: TeamDO). */
export const listAgents: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/agents/{name}: Get an agent by name or ID (owner DO: TeamDO). */
export const getAgent: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/automations: List automations (owner DO: TeamDO). */
export const listAutomations: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/integrations: Integrations attached to the factory (owner DO: TeamDO). */
export const getFactoryIntegrations: Handler = notImplementedHandler;

/** PUT /api/v1/factories/{uid}/integrations: Set the integrations a factory declares (owner DO: TeamDO). */
export const putFactoryIntegrations: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/settings: Factory settings derived from the active definition (owner DO: TeamDO). */
export const getFactorySettings: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/agents/{name}/spec: Editable settings of an agent (agent.md frontmatter) (owner DO: TeamDO). */
export const getAgentSpec: Handler = notImplementedHandler;

/** PUT /api/v1/factories/{uid}/agents/{name}/spec: Update an agent's settings and instructions (owner DO: TeamDO). */
export const putAgentSpec: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/automations/{name}: Get an automation file (owner DO: TeamDO). */
export const getAutomation: Handler = notImplementedHandler;

/** PUT /api/v1/factories/{uid}/automations/{name}: Create or replace an automation (owner DO: TeamDO). */
export const putAutomation: Handler = notImplementedHandler;

/** DELETE /api/v1/factories/{uid}/automations/{name}: Delete an automation (owner DO: TeamDO). */
export const deleteAutomation: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/automations/{name}/validate: Validate the definition with an edited automation (owner DO: TeamDO). */
export const validateAutomation: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/test-filter: Test a webhook trigger filter against a stored delivery (owner DO: Worker (pure)). */
export const testFactoryFilter: Handler = notImplementedHandler;
