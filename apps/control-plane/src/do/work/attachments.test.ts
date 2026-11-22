// A message with attachments on the runner path: the queue entry for the
// runner and the stored message carry the attachment IDs and the actor
// (internal/engine/attachments_test.go).
import { afterEach, describe, expect, it } from "vitest";
import { buildRuns, settle, withFx, workStub } from "../../orchestration/fixture.test-util";

afterEach(settle);

describe("sendMessage attachments", () => {
  it("reach the runner input and the stored message", async () => {
    const f = await buildRuns({}, { a: { state: "succeeded" } });
    const rec = await workStub(f.token).runsMessage({ requestId: "t" }, f.ids.a!, {
      message: "see file",
      actor: "user_1",
      attachments: ["att_1", "att_2"],
    });
    const { inputs, messages } = await withFx(f.token, (fx) => ({
      inputs: fx.repo.listInputs(f.ids.a!),
      messages: fx.repo.listMessages(f.ids.a!),
    }));
    const sent = inputs.filter((i) => i.message_id === rec.message_id);
    expect(sent).toHaveLength(1);
    expect(sent[0]!.attachments).toEqual(["att_1", "att_2"]);
    expect(sent[0]!.content).toBe("see file");
    const last = messages.at(-1)!;
    expect(last.id).toBe(rec.message_id);
    expect(last.data).toEqual({ actor: "user_1", attachments: ["att_1", "att_2"] });
  });
});
