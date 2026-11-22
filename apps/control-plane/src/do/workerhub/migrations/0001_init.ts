// WorkerHubDO tables. The task cache survives hibernation and restarts.
//
// hub_tasks: tasks leased to this worker (task.assign went through this hub).
//   status     JSON TaskStatus (camelCase)
//   destroyed  1 = destroy asked while the worker was away; sent at the next hello
// hub_state: k/v of the hub (record, generation, current connection, last hello).
export default `
CREATE TABLE hub_tasks (
  task_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  session INTEGER NOT NULL,
  status TEXT NOT NULL,
  destroyed INTEGER NOT NULL DEFAULT 0,
  leased_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL
);
CREATE TABLE hub_state (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`;
