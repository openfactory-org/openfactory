// REST handlers: users. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/users: List team users (owner DO: TeamDO). */
export const listUsers: Handler = notImplementedHandler;

/** POST /api/v1/users: Create a user (owner DO: TeamDO). */
export const createUser: Handler = notImplementedHandler;

/** GET /api/v1/users/{id}: Get user (owner DO: TeamDO). */
export const getUser: Handler = notImplementedHandler;

/** PATCH /api/v1/users/{id}: Update a user's name or role (admin) (owner DO: TeamDO). */
export const updateUser: Handler = notImplementedHandler;
