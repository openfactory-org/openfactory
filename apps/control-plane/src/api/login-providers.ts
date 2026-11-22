// REST handler: GET /login-providers (public).
import { loginProviders } from "../auth/routes";
import { json, type Handler } from "../http/handler";

export const listLoginProviders: Handler = async (_request, env) => json({ login_providers: loginProviders(env) });
