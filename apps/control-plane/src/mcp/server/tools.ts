// Port of internal/mcpserver/tools.go and resolve.go: the Factory MCP tools.
// Each tool calls the same REST operations as the dashboard (backend.api).
// The tools live in ./tools/ by domain; this file fixes their order.
import type { Tool } from "../jsonrpc";
import type { FactoryCtx } from "./tools/common";
import { createFactory, listFactories } from "./tools/factories";
import { listNotificationRoutes } from "./tools/notifications";
import {
  createTeam,
  getConnectionStatus,
  joinTeam,
  listForgeRepositories,
  listTeams,
  listTrackerScopes,
  startConnection,
} from "./tools/onboarding";
import { getFactoryFileSchema, validateFactoryFiles } from "./tools/schema";
import {
  completeWorkItem,
  getConversation,
  getWorkItem,
  listWorkItems,
  messageForeman,
  searchWorkItems,
  sendWorkItem,
} from "./tools/work-items";

export type { FactoryCtx } from "./tools/common";

export const FACTORY_MCP_INSTRUCTIONS = `Factory MCP connects you to the user's Open Factory. A factory is a team of agents bound to repositories;
work sent to it becomes a work item coordinated by the factory's foreman agent.

- send_work_item creates a work item (state the goal, context, constraints and work already done in the note), or hands work back to an
  existing work item with its ID, the pushed branch or pull request URL and a note covering what changed, what was validated and what remains.
- To continue a work item locally: find it (list_work_items, search_work_items, or get_work_item with any reference), call get_work_item with start_working=true,
  follow the returned Git setup, keep the foreman informed with message_foreman / get_conversation, push, then hand back with send_work_item.
- Picking up a work item does not claim, lock or pause it. complete_work_item closes a work item when nothing remains.
- Factory MCP never modifies local files; run the suggested commands yourself.`;

/** The Factory MCP tools in the order tools/list returns them. */
export const FACTORY_TOOLS: readonly Tool<FactoryCtx>[] = [
  listFactories,
  getFactoryFileSchema,
  validateFactoryFiles,
  listTeams,
  createTeam,
  joinTeam,
  listForgeRepositories,
  listTrackerScopes,
  startConnection,
  getConnectionStatus,
  createFactory,
  listWorkItems,
  searchWorkItems,
  getWorkItem,
  messageForeman,
  getConversation,
  sendWorkItem,
  listNotificationRoutes,
  completeWorkItem,
];
