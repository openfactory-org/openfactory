// Ports of internal/dispatch/eventfiles_test.go (the event files of a message
// reach its input item, also for the first prompt, which has its own key;
// the references go when the run ends) and of internal/engine
// eventfiles_test.go TestEventContextReleasedOnCancel. WorkDO keeps the R2
// reference in event_context; the runner API expands it to files.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newRoutedId, newToken, type Event } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { build, storeBundle } from "../../eventctx/index";
import type { AgentConfig, Run } from "../../engine/types";
import { FakeRunner, installFakeSandbox, pump, workStub, type FakeProvider } from "../../../test/fakes/runner";

const TEAM = "default";
const meta = { requestId: "t" };

const foreman: AgentConfig = {
  id: "agt_ef",
  factory_id: "fac_ef",
  name: "foreman",
  description: "",
  type: "FOREMAN",
  instructions: "lead",
  harness: { type: "claude", model: "m" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
};

let provider: FakeProvider;
let uninstall: () => void;
beforeEach(() => {
  ({ provider, uninstall } = installFakeSandbox());
});
afterEach(() => uninstall());

const ev = { provider: "webhook", type: "received", payload: { a: 1 } } as unknown as Event;

async function rootRun(token: string): Promise<Run> {
  const out = await workStub(TEAM, token).runsCreate(meta, {
    dispatch: {
      factoryId: "fac_ef",
      config: foreman,
      prompt: "go",
      actor: "usr_1",
      title: "go",
      workItemId: newRoutedId("wi", token),
    },
  });
  return out.run as Run;
}

/** The event_context rows of a WorkDO. */
async function rows(token: string): Promise<{ message_id: string; run_id: string; r2_key: string }[]> {
  return runInDurableObject(env.WORK.getByName(doName.work(TEAM, token)), (_i, state) =>
    state.storage.sql
      .exec<{ message_id: string; run_id: string; r2_key: string }>(
        "SELECT message_id, run_id, r2_key FROM event_context ORDER BY message_id",
      )
      .toArray(),
  );
}

async function addRow(token: string, messageKey: string, runId: string, ref: string): Promise<void> {
  await runInDurableObject(env.WORK.getByName(doName.work(TEAM, token)), (_i, state) => {
    state.storage.sql.exec(
      "INSERT INTO event_context (message_id, run_id, r2_key, bytes, created_ms) VALUES (?, ?, ?, 0, ?)",
      messageKey,
      runId,
      ref,
      Date.now(),
    );
  });
}

describe("event files of a session", () => {
  it("each message gets its event files; the first prompt uses its own key", async () => {
    const token = newToken("w");
    const run = await rootRun(token);
    const firstRef = await storeBundle(env.DATA, (await build("msg_first", ev, true))!);
    const later = newRoutedId("msg", token);
    const laterRef = await storeBundle(env.DATA, (await build(later, ev, false))!);
    await addRow(token, `prompt:${run.id}`, run.id, firstRef);
    await addRow(token, later, run.id, laterRef);
    await workStub(TEAM, token).runsMessage(meta, run.id, { message: "later", actor: "usr_1", messageId: later });
    await workStub(TEAM, token).runsMessage(meta, run.id, { message: "plain", actor: "usr_1" });

    await pump(TEAM, token);
    const fr = new FakeRunner(provider.taskFor(run.id));
    const ts = await fr.ready();
    const items = ts.data!.input!;
    const by = (content: string) => items.find((i) => i.content === content)!;
    expect(items.every((i) => !("event_files_ref" in i))).toBe(true);
    const first = by("go").files ?? [];
    expect(first.map((f) => f.path)).toEqual([
      ".openfactory/events/msg_first/envelope.json",
      ".openfactory/events/msg_first/payload.json",
    ]);
    const lf = by("later").files ?? [];
    expect(lf).toHaveLength(2);
    expect(lf[1]!.path).toBe(`.openfactory/events/${later}/payload.json`);
    expect(atob(lf[1]!.data)).toBe('{"a":1}');
    expect(by("plain").files ?? []).toHaveLength(0);
  });

  it("a gone bundle gives no files and no error", async () => {
    const token = newToken("w");
    const run = await rootRun(token);
    await addRow(token, `prompt:${run.id}`, run.id, "eventctx/" + "0".repeat(64));
    await pump(TEAM, token);
    const fr = new FakeRunner(provider.taskFor(run.id));
    const ts = await fr.ready();
    expect(ts.data!.input![0]!.files ?? []).toHaveLength(0);
  });

  it("the references stay while the run is active and go when it ends", async () => {
    const token = newToken("w");
    const run = await rootRun(token);
    await addRow(token, `prompt:${run.id}`, run.id, "eventctx/a");
    await addRow(token, "msg_first", run.id, "eventctx/b");
    await pump(TEAM, token);
    const fr = new FakeRunner(provider.taskFor(run.id));
    const turnId = (await fr.ready()).data!.turn_id!;
    await fr.events([{ type: "turn.started", turn_id: turnId }]);
    expect(await rows(token)).toHaveLength(2);
    await fr.events([{ type: "turn.ended", turn_id: turnId, data: { outcome: "done", result: "ok" } }]);
    expect((await workStub(TEAM, token).getRun(run.id)).state).toBe("succeeded");
    expect(await rows(token)).toEqual([]);
  });

  it("a cancel removes the references of the run only", async () => {
    const token = newToken("w");
    const run = await rootRun(token);
    const side = (
      await workStub(TEAM, token).runsCreate(meta, {
        run: { factoryId: "fac_ef", config: foreman, prompt: "side", actor: "api" },
      } as never)
    ).run as Run;
    await addRow(token, `prompt:${run.id}`, run.id, "eventctx/a");
    await addRow(token, `prompt:${side.id}`, side.id, "eventctx/c");
    await workStub(TEAM, token).runsCancel(meta, run.id, false);
    expect((await rows(token)).map((r) => r.run_id)).toEqual([side.id]);
    await pump(TEAM, token);
  });
});
