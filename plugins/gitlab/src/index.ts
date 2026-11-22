// gitlab plugin Worker. Not implemented yet (WS12). It exports a ForgePlugin and an IngressPlugin entrypoint.
export const PLUGIN_NAME = "gitlab";

export default {
  fetch(): Response {
    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler;
