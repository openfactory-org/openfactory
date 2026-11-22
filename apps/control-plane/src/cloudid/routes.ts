// GET /.well-known/openid-configuration and GET /.well-known/jwks.json.
import type { Env } from "../contracts/env";
import { DISCOVERY_PATH, JWKS_PATH, issuerFor } from "./issuer";

export async function handleCloudIdRoute(req: Request, env: Env, url: URL): Promise<Response | null> {
  if (req.method !== "GET" && req.method !== "HEAD") return null;
  if (url.pathname !== DISCOVERY_PATH && url.pathname !== JWKS_PATH) return null;
  let iss;
  try {
    iss = await issuerFor(env);
  } catch (e) {
    console.warn("cloud identity disabled", (e as Error).message);
    return null;
  }
  const doc = url.pathname === DISCOVERY_PATH ? iss.discovery() : iss.jwks();
  return new Response(JSON.stringify(doc) + "\n", {
    headers: { "content-type": "application/json", "cache-control": "public, max-age=3600" },
  });
}
