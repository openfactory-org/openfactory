// The consumer of Queue messages `compaction.run`.
import type { JobMessage } from "../contracts/do-messages";
import type { Env } from "../contracts/env";

export type CompactionJob = Extract<JobMessage, { kind: "compaction.run" }>;

/**
 * Calls the WorkDO of the message. A step that left rows behind sends the next
 * step as a new message, so one run never holds the DO for long.
 */
export async function handleCompactionJob(env: Env, msg: CompactionJob): Promise<void> {
  const stub = env.WORK.getByName(msg.doName);
  const out = await stub.compactRun(msg.runId);
  if (out.more) {
    await env.JOBS.send({ kind: "compaction.run", msgId: `${msg.msgId}+`, doName: msg.doName, runId: msg.runId });
  }
}
