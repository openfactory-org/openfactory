import { chmodSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { findOnPath } from "../src/commands/runtime.ts";
import { json, router, runCli } from "./helpers.ts";

function fakeRuntime(): string {
  const dir = mkdtempSync(join(tmpdir(), "of-cli-bin-"));
  const bin = join(dir, "openfactory-runtime");
  writeFileSync(bin, "#!/bin/sh\nexit 0\n");
  chmodSync(bin, 0o755);
  return dir;
}

describe.skipIf(process.platform === "win32")("runtime commands", () => {
  it.each(["runner", "netguard", "vm-init", "worker"])(
    "forwards %s with all arguments to openfactory-runtime",
    async (cmd) => {
      const dir = fakeRuntime();
      const r = await runCli([cmd, "--id", "w1", "extra"], { vars: { PATH: dir } });
      expect(r.code).toBe(0);
      expect(r.ran).toEqual([{ file: join(dir, "openfactory-runtime"), args: [cmd, "--id", "w1", "extra"] }]);
    },
  );

  it.each(["enroll", "run", "status", "drain", "doctor"])("forwards worker %s to openfactory-runtime", async (sub) => {
    const dir = fakeRuntime();
    const r = await runCli(["worker", sub], { vars: { PATH: dir } });
    expect(r.code).toBe(0);
    expect(r.ran[0]?.args).toEqual(["worker", sub]);
  });

  it("has no machine command: host commands are under worker", async () => {
    const dir = fakeRuntime();
    const r = await runCli(["machine", "doctor"], { vars: { PATH: dir } });
    expect(r.code).not.toBe(0);
    expect(r.ran).toHaveLength(0);
  });

  it("starts a worker without arguments", async () => {
    const dir = fakeRuntime();
    const r = await runCli(["worker"], { vars: { PATH: dir } });
    expect(r.ran[0]?.args).toEqual(["worker"]);
  });

  it("keeps worker list in the CLI", async () => {
    const dir = fakeRuntime();
    const r = await runCli(["worker", "list"], {
      vars: { PATH: dir, OF_API_KEY: "k" },
      handler: router({ "GET /workers": json({ items: [], page_info: {} }) }),
    });
    expect(r.code).toBe(0);
    expect(r.ran).toHaveLength(0);
    expect(r.calls).toHaveLength(1);
  });

  it("passes the exit code of the runtime on", async () => {
    const dir = fakeRuntime();
    const r = await runCli(["runner"], { vars: { PATH: dir }, programExit: 7 });
    expect(r.code).toBe(7);
    expect(r.err).toBe("");
  });

  it("gives a clear error when openfactory-runtime is not on PATH", async () => {
    const r = await runCli(["runner"], { vars: { PATH: mkdtempSync(join(tmpdir(), "of-cli-empty-")) } });
    expect(r.code).toBe(1);
    expect(r.err).toContain('"openfactory runner" needs openfactory-runtime, which is not on PATH');
    expect(r.ran).toHaveLength(0);
  });

  it("finds an executable on PATH", () => {
    const dir = fakeRuntime();
    expect(findOnPath("openfactory-runtime", { PATH: `/nonexistent:${dir}` })).toBe(join(dir, "openfactory-runtime"));
    expect(findOnPath("missing-binary", { PATH: dir })).toBeNull();
    expect(findOnPath("openfactory-runtime", {})).toBeNull();
  });

  it("shows help for a forwarded command without starting the runtime", async () => {
    const r = await runCli(["runner", "--help"]);
    expect(r.code).toBe(0);
    expect(r.out).toContain("openfactory-runtime");
    expect(r.ran).toHaveLength(0);
  });
});
