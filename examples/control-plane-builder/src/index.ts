// A custom control plane Worker. It is the control plane of this repository
// plus two extras: one HTTP route and one Cron hook. The enterprise build
// follows the same shape.
//
// `wrangler` reads the Durable Object classes from the exports of this module,
// so the module re-exports everything of @openfactory/control-plane.
import { registerCronHook, registerServerRoute } from "@openfactory/control-plane";

export * from "@openfactory/control-plane";
export { default } from "@openfactory/control-plane";

// Server routes outside /api/v1 must start with a path that the control plane
// sends to its server handlers. /plugins/ is meant for extensions.
registerServerRoute("/plugins/hello", async (request, _env, _ctx, url) => {
  // Return null to let the next handler (or the 404 of the control plane) answer.
  if (request.method !== "GET" || url.pathname !== "/plugins/hello") return null;
  return new Response("hello from a custom build\n", { headers: { "content-type": "text/plain; charset=utf-8" } });
});

// Runs once per minute, after the control plane re-arms its own alarms.
// A failing hook is logged and does not stop the other steps.
registerCronHook(async (_env, _ctx) => {
  console.log("custom cron hook", new Date().toISOString());
});
