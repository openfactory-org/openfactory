import { describe, expect, it } from "vitest";
import { createApp, DEFAULT_SERVER } from "../src/cli.ts";
import { commandName, type Command } from "../src/framework.ts";
import { json, router, runCli } from "./helpers.ts";

const SECRET = "of_SENTINEL_secret_value_9f3a";
const SECRET_VARS = Object.fromEntries(
  [
    "OF_API_KEY",
    "OF_SERVER",
    "OF_SECRET_KEY",
    "OF_SIGNING_KEY",
    "OF_DATABASE_URL",
    "OF_OIDC_CLIENT_SECRET",
    "OF_BOOTSTRAP_TOKEN",
  ].map((n) => [n, SECRET]),
);

function walk(c: Command, prefix: string[], visit: (path: string[]) => void): void {
  const path = [...prefix, commandName(c)];
  visit(path.slice(1));
  for (const s of c.subcommands ?? []) walk(s, path, visit);
}

describe("command tree", () => {
  it("has every command of the Go CLI that moved to TypeScript", () => {
    const app = createApp();
    const paths: string[][] = [];
    walk(app.root, [], (p) => paths.push(p));
    const have = new Set(paths.map((p) => p.join(" ")));
    for (const want of [
      "worker",
      "worker list",
      "validate",
      "schema",
      "init",
      "bootstrap",
      "mcp",
      "version",
      "factory list",
      "factory get",
      "factory create",
      "factory apply",
      "factory run",
      "run list",
      "run get",
      "run logs",
      "run followup",
      "run cancel",
      "work-item list",
      "work-item get",
      "work-item send",
      "work-item complete",
      "inbox list",
      "inbox resolve",
      "inbox unresolve",
      "secret set",
      "secret list",
      "secret delete",
      "secret rewrap",
      "webhook list",
      "webhook create",
      "webhook deliveries",
      "benchmark run",
      "benchmark list",
      "mcp-registry list",
      "mcp-registry get",
      "mcp-registry add",
      "mcp-registry delete",
      "mcp-registry connect",
      "mcp-registry disconnect",
      "runner",
      "netguard",
      "vm-init",
      "machine",
    ]) {
      expect(have, want).toContain(want);
    }
  });

  it("has no agent command: each run belongs to a factory", () => {
    const names = (createApp().root.subcommands ?? []).map(commandName);
    expect(names).not.toContain("agent");
  });

  it("prints the version", async () => {
    const r = await runCli(["version"]);
    expect(r.code).toBe(0);
    expect(r.out.startsWith("openfactory ")).toBe(true);
    const short = await runCli(["version", "--short"]);
    expect(short.out).toMatch(/^\d+\.\d+\.\d+\S*\n$/);
  });

  it("resolves aliases", async () => {
    const r = await runCli(["factories", "list"], {
      vars: { OF_API_KEY: "k" },
      handler: router({ "GET /factories": json({ items: [], page_info: {} }) }),
    });
    expect(r.code).toBe(0);
    expect(r.out).toBe("UID  NAME  ALIAS  SOURCE  UPDATED\n");
  });

  it("removes serve with a pointer to wrangler", async () => {
    const r = await runCli(["serve"]);
    expect(r.code).toBe(1);
    expect(r.err).toContain("wrangler");
  });
});

describe("help", () => {
  it("never shows the value of an environment variable that holds a secret", () => {
    const app = createApp();
    const paths: Command[][] = [];
    const visit = (c: Command, path: Command[]): void => {
      paths.push([...path, c]);
      for (const s of c.subcommands ?? []) visit(s, [...path, c]);
    };
    visit(app.root, []);
    for (const p of paths) {
      expect(app.help(p), p.map(commandName).join(" ")).not.toContain(SECRET);
    }
  });

  it("prints no secret from the environment through --help", async () => {
    for (const args of [
      ["--help"],
      ["secret", "--help"],
      ["factory", "create", "-h"],
      ["mcp", "--help"],
      ["help", "run"],
    ]) {
      const r = await runCli(args, { vars: SECRET_VARS });
      expect(r.code).toBe(0);
      expect(r.out + r.err).not.toContain(SECRET);
    }
  });

  it("shows the fixed defaults of the global flags", async () => {
    const r = await runCli(["--help"], { vars: { OF_SERVER: "http://env.test", OF_API_KEY: "key-from-env" } });
    expect(r.out).toContain(`(default "${DEFAULT_SERVER}")`);
    expect(r.out).not.toContain("env.test");
    expect(r.out).not.toContain("key-from-env");
  });

  it("names only existing commands in parentheses of the root help", () => {
    const app = createApp();
    const names = new Set((app.root.subcommands ?? []).map(commandName));
    for (const group of (app.root.long ?? "").matchAll(/\(([^)]*)\)/g)) {
      for (const raw of (group[1] ?? "").split(",")) {
        const name = raw.trim();
        if (/^[a-z-]+$/.test(name)) expect(names, name).toContain(name);
      }
    }
  });

  it("lists the groups of the Go CLI", async () => {
    const r = await runCli(["--help"]);
    for (const t of [
      "Running Open Factory:",
      "Local definition tools:",
      "Client commands (talk to a server):",
      "Additional Commands:",
    ]) {
      expect(r.out).toContain(t);
    }
  });

  it("shows persistent flags of a parent under Global Flags", async () => {
    const r = await runCli(["secret", "set", "--help"]);
    expect(r.out).toContain("Usage:\n  openfactory secret set <name> [value] [flags]");
    expect(r.out).toContain("Global Flags:");
    expect(r.out).toMatch(/--factory string\s+factory scope/);
  });
});

describe("global flags and environment", () => {
  const handler = router({ "GET /factories": json({ items: [], page_info: {} }) });

  it("reads the server and the API key from the environment", async () => {
    const r = await runCli(["factory", "list"], {
      vars: { OF_SERVER: "http://env.test", OF_API_KEY: "key-from-env" },
      handler,
    });
    expect(r.code).toBe(0);
    expect(r.calls[0]?.url).toBe("http://env.test/api/v1/factories");
    expect(r.calls[0]?.headers.get("Authorization")).toBe("Bearer key-from-env");
  });

  it("lets a flag win over the environment, in any position", async () => {
    const r = await runCli(["--server", "http://flag.test/", "factory", "--api-key=key-from-flag", "list"], {
      vars: { OF_SERVER: "http://env.test", OF_API_KEY: "key-from-env" },
      handler,
    });
    expect(r.code).toBe(0);
    expect(r.calls[0]?.url).toBe("http://flag.test/api/v1/factories");
    expect(r.calls[0]?.headers.get("Authorization")).toBe("Bearer key-from-flag");
  });

  it("sends a User-Agent", async () => {
    const r = await runCli(["factory", "list"], { vars: { OF_API_KEY: "k" }, handler });
    expect(r.calls[0]?.headers.get("User-Agent")).toMatch(/^openfactory\/\S+ openfactory-ts\//);
  });

  it("needs an API key", async () => {
    const r = await runCli(["factory", "list"], { handler });
    expect(r.code).toBe(1);
    expect(r.err).toBe("error: no API key: pass --api-key or set OF_API_KEY\n");
    expect(r.calls).toHaveLength(0);
  });

  it("prints JSON with -o json", async () => {
    const r = await runCli(["-o", "json", "factory", "list"], { vars: { OF_API_KEY: "k" }, handler });
    expect(r.out).toBe("[]\n");
  });
});

describe("argument errors", () => {
  it("rejects unknown flags", async () => {
    const r = await runCli(["factory", "list", "--nope"]);
    expect(r.code).toBe(1);
    expect(r.err).toBe("error: unknown flag: --nope\n");
  });

  it("rejects the wrong number of arguments", async () => {
    expect((await runCli(["factory", "get"])).err).toBe("error: accepts 1 arg(s), received 0\n");
    expect((await runCli(["run", "followup", "r1"])).err).toBe("error: requires at least 2 arg(s), only received 1\n");
    expect((await runCli(["secret", "set"])).err).toBe("error: accepts between 1 and 2 arg(s), received 0\n");
    expect((await runCli(["schema", "a", "b"])).err).toBe("error: accepts at most 1 arg(s), received 2\n");
  });

  it("rejects an unknown subcommand of a group", async () => {
    const r = await runCli(["factory", "nope"]);
    expect(r.code).toBe(1);
    expect(r.err).toBe('error: unknown command "nope" for "openfactory factory"\n');
  });

  it("prints the help of a group without a subcommand", async () => {
    const r = await runCli(["factory"]);
    expect(r.code).toBe(0);
    expect(r.out).toContain("Available Commands:");
  });

  it("rejects a bad integer", async () => {
    const r = await runCli(["run", "list", "--limit", "x"], { vars: { OF_API_KEY: "k" } });
    expect(r.code).toBe(1);
    expect(r.err).toContain('invalid argument "x" for "--limit" flag');
  });
});
