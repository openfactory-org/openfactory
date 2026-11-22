// Vitest global setup: bootstraps the target once (POST /api/v1/bootstrap) and
// provides the admin key to every test file. With OF_CONTRACT_API_KEY set, it
// uses that key instead (a target that was bootstrapped another way).
import type { TestProject } from "vitest/node";
import { assertOpenApiResponse } from "./schemas.ts";

export interface AdminContext {
  key: string;
  teamId: string;
  userId: string;
  email: string;
  /** The checked 201 body of the bootstrap, or null when OF_CONTRACT_API_KEY was given. */
  bootstrap: unknown;
}

declare module "vitest" {
  export interface ProvidedContext {
    admin: AdminContext | null;
  }
}

export default async function setup(project: TestProject): Promise<void> {
  const base = (process.env.OF_CONTRACT_BASE_URL ?? "").replace(/\/+$/, "");
  if (!base) {
    project.provide("admin", null);
    return;
  }
  const given = process.env.OF_CONTRACT_API_KEY;
  if (given) {
    const me = await fetch(`${base}/api/v1/me`, { headers: { authorization: `Bearer ${given}` } });
    const body = (await me.json()) as { user?: { id: string; email: string }; team?: { id: string } };
    project.provide("admin", {
      key: given,
      teamId: body.team?.id ?? "",
      userId: body.user?.id ?? "",
      email: body.user?.email ?? "",
      bootstrap: null,
    });
    return;
  }
  const email = "admin@example.com";
  const res = await fetch(`${base}/api/v1/bootstrap`, {
    method: "POST",
    headers: { "content-type": "application/json", accept: "application/json" },
    body: JSON.stringify({
      team_name: "Contract",
      admin_email: email,
      admin_name: "Contract Admin",
      key_name: "contract",
    }),
  });
  const text = await res.text();
  if (res.status !== 201) {
    // Let the suites run (and fail) with a clear message instead of aborting
    // the whole run: the target may not implement bootstrap yet.
    console.error(`contract setup: POST /api/v1/bootstrap returned ${res.status}: ${text.slice(0, 400)}`);
    project.provide("admin", null);
    return;
  }
  const body = JSON.parse(text) as { key: string; team: { id: string }; user: { id: string } };
  assertOpenApiResponse("post", "/bootstrap", 201, body);
  project.provide("admin", { key: body.key, teamId: body.team.id, userId: body.user.id, email, bootstrap: body });
}
