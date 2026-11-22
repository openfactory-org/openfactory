import { unwrap } from "@openfactory/api-client";
import { emit } from "../format.ts";
import { makeClient, isJSON } from "../client.ts";
import { CliError, noArgs, type Command } from "../framework.ts";

export const bootstrapCommand: Command = {
  use: "bootstrap",
  short: "Create the first team, admin user and API key on a fresh server",
  long: `Bootstrap a server that has no teams yet. Prints the admin API key once;
store it safely (export OF_API_KEY=...). Fails when the server is already
bootstrapped.`,
  args: noArgs,
  flags: [
    { name: "team", type: "string", default: "Default", usage: "team name" },
    { name: "email", type: "string", usage: "admin e-mail (required)" },
    { name: "name", type: "string", usage: "admin display name" },
  ],
  async run({ flags, globals, env }) {
    const email = flags.string("email");
    if (email === "") throw new CliError("--email is required");
    const c = makeClient(globals, env, true);
    const res = await unwrap(
      c.api.POST("/bootstrap", {
        body: { team_name: flags.string("team"), admin_email: email, admin_name: flags.string("name") },
      }),
    );
    if (isJSON(globals)) return emit(env.stdout, res);
    env.stdout.write(
      `Team:    ${res.team.name} (${res.team.id})\nAdmin:   ${res.user.email}\nAPI key: ${res.key}\n\nThe key is shown only once:\n  export OF_SERVER=${globals.server} OF_API_KEY=${res.key}\n`,
    );
  },
};
