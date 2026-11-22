// REST handlers: definitions (WS4). Apply = compile + preflight in TeamDO,
// then one transaction (plan rule 2). Network work that a definition needs
// (name resolution, skill fetches) is a Worker step before the call; none is
// wired yet.
import { teamApi, type Handler } from "../http/handler";

export const getDefinition: Handler = teamApi();
export const putDefinition: Handler = teamApi();
export const validateDefinition: Handler = teamApi();
export const listDefinitionVersions: Handler = teamApi();
export const getDefinitionVersion: Handler = teamApi();
export const restoreDefinitionVersion: Handler = teamApi();
