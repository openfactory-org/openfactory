// Cloudflare Sandbox provider Worker. Not implemented yet (WS7).
// Exports SandboxProvider (WorkerEntrypoint), SandboxDO, SandboxIndexDO and Egress.
export const PROVIDER_ID = "cloudflare-sandbox";

export default {
  fetch(): Response {
    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler;
