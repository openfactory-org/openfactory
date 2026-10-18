-- Open Factory baseline schema (SQLite).
-- Each table stores the full record as JSON in "data"; lookup/filter/order
-- fields are promoted to indexed columns. *_ns columns are unix nanoseconds.

CREATE TABLE teams (
	id TEXT PRIMARY KEY,
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);

CREATE TABLE users (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX users_team ON users (team_id);
CREATE UNIQUE INDEX users_email ON users (email) WHERE email <> '';

CREATE TABLE external_identities (
	provider TEXT NOT NULL,
	external_id TEXT NOT NULL,
	user_id TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL,
	PRIMARY KEY (provider, external_id)
);
CREATE INDEX external_identities_user ON external_identities (user_id);

CREATE TABLE api_keys (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	user_id TEXT NOT NULL DEFAULT '',
	prefix TEXT NOT NULL UNIQUE,
	hashed_key TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX api_keys_team ON api_keys (team_id);

CREATE TABLE factories (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	alias TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX factories_team ON factories (team_id, name);
CREATE UNIQUE INDEX factories_alias ON factories (team_id, alias) WHERE alias <> '';

CREATE TABLE definition_versions (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL REFERENCES factories (id) ON DELETE CASCADE,
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX definition_versions_factory ON definition_versions (factory_id, created_ns DESC, id DESC);

CREATE TABLE agents (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL REFERENCES factories (id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	ord INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE UNIQUE INDEX agents_factory_name ON agents (factory_id, name);

CREATE TABLE automations (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL REFERENCES factories (id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 0,
	ord INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX automations_factory ON automations (factory_id, ord);
CREATE INDEX automations_enabled ON automations (enabled);

CREATE TABLE scorers (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL REFERENCES factories (id) ON DELETE CASCADE,
	ord INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX scorers_factory ON scorers (factory_id, ord);

CREATE TABLE webhooks (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL REFERENCES factories (id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT '',
	ord INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX webhooks_factory ON webhooks (factory_id, ord);

CREATE TABLE work_items (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL DEFAULT '',
	stage TEXT NOT NULL,
	source_key TEXT NOT NULL DEFAULT '',
	creator_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX work_items_factory ON work_items (factory_id, created_ns DESC, id DESC);
CREATE INDEX work_items_source ON work_items (factory_id, source_key, created_ns DESC);
CREATE INDEX work_items_stage ON work_items (stage);
CREATE INDEX work_items_created ON work_items (created_ns DESC, id DESC);

CREATE TABLE work_item_events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL UNIQUE,
	work_item_id TEXT NOT NULL REFERENCES work_items (id) ON DELETE CASCADE,
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX work_item_events_item ON work_item_events (work_item_id, seq);

CREATE TABLE runs (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	factory_id TEXT NOT NULL CHECK (factory_id <> ''),
	work_item_id TEXT NOT NULL DEFAULT '',
	parent_run_id TEXT NOT NULL DEFAULT '',
	agent_name TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	worker_host TEXT NOT NULL DEFAULT 'server',
	worker_id TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	finished_ns INTEGER NOT NULL DEFAULT 0,
	msg_seq INTEGER NOT NULL DEFAULT 0,
	data TEXT NOT NULL,
	input_seq INTEGER NOT NULL DEFAULT 0,
	version INTEGER NOT NULL DEFAULT 0,
	creator_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runs_claim ON runs (state, worker_host, created_ns);
CREATE INDEX runs_factory ON runs (factory_id, created_ns DESC, id DESC);
CREATE INDEX runs_work_item ON runs (work_item_id, created_ns DESC);
CREATE INDEX runs_parent ON runs (parent_run_id, created_ns DESC);
CREATE INDEX runs_team ON runs (team_id, created_ns DESC);
CREATE INDEX runs_created ON runs (created_ns DESC, id DESC);
CREATE INDEX runs_finished ON runs (finished_ns);
CREATE INDEX runs_creator ON runs (creator_id, created_ns);

CREATE TABLE run_messages (
	run_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	id TEXT NOT NULL UNIQUE,
	data TEXT NOT NULL,
	PRIMARY KEY (run_id, seq)
);

CREATE TABLE inbox_items (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL DEFAULT '',
	work_item_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	resolved INTEGER NOT NULL DEFAULT 0,
	created_ns INTEGER NOT NULL,
	resolved_ns INTEGER NOT NULL DEFAULT 0,
	data TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX inbox_items_factory ON inbox_items (factory_id, resolved, created_ns DESC);

CREATE TABLE secrets (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL,
	factory_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	ciphertext BLOB,
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE UNIQUE INDEX secrets_scope_name ON secrets (team_id, factory_id, name);

CREATE TABLE webhook_deliveries (
	id TEXT PRIMARY KEY,
	webhook_id TEXT NOT NULL,
	delivery_id TEXT NOT NULL DEFAULT '',
	received_ns INTEGER NOT NULL,
	data TEXT NOT NULL,
	stripped INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX webhook_deliveries_webhook ON webhook_deliveries (webhook_id, received_ns DESC);
CREATE UNIQUE INDEX webhook_deliveries_dedupe ON webhook_deliveries (webhook_id, delivery_id) WHERE delivery_id <> '';
CREATE INDEX webhook_deliveries_received ON webhook_deliveries (received_ns);

CREATE TABLE integrations (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX integrations_team ON integrations (team_id, provider);

CREATE TABLE factory_integrations (
	factory_id TEXT NOT NULL REFERENCES factories (id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	settings TEXT NOT NULL,
	PRIMARY KEY (factory_id, provider)
);

CREATE TABLE scores (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL DEFAULT '',
	scorer_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX scores_factory ON scores (factory_id, created_ns DESC);
CREATE INDEX scores_scorer ON scores (scorer_id, created_ns DESC);
CREATE INDEX scores_run ON scores (run_id);

CREATE TABLE benchmark_runs (
	id TEXT PRIMARY KEY,
	factory_id TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX benchmark_runs_factory ON benchmark_runs (factory_id, created_ns DESC);

CREATE TABLE workers (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL
);
CREATE INDEX workers_team ON workers (team_id, name);

CREATE TABLE environments (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL
);
CREATE INDEX environments_team ON environments (team_id, name);

CREATE TABLE kv (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_ns INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX kv_updated ON kv (updated_ns);

-- Coordination of replicas: the outbox, the leader lease and
-- distributed locks. The claim takes only the first pending message of each
-- partition key, in rowid (insert) order.

CREATE TABLE outbox (
	id TEXT PRIMARY KEY,
	topic TEXT NOT NULL,
	payload TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	next_ns INTEGER NOT NULL,
	claimed_by TEXT NOT NULL DEFAULT '',
	claimed_until_ns INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	delivered_ns INTEGER NOT NULL DEFAULT 0,
	partition_key TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outbox_due ON outbox (state, topic, next_ns);
CREATE INDEX outbox_order ON outbox (state, created_ns, id);
CREATE INDEX outbox_delivered ON outbox (delivered_ns);
CREATE INDEX outbox_partition ON outbox (partition_key, state) WHERE partition_key <> '';

CREATE TABLE leader_leases (
	name TEXT PRIMARY KEY,
	holder TEXT NOT NULL,
	expires_ns INTEGER NOT NULL
);

CREATE TABLE coord_locks (
	lock_key TEXT PRIMARY KEY,
	holder TEXT NOT NULL,
	expires_ns INTEGER NOT NULL
);

-- Team MCP server registry: entries that mcpServers.<name>.managedId names.

CREATE TABLE mcp_servers (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL
);
CREATE UNIQUE INDEX mcp_servers_team_name ON mcp_servers (team_id, name);

-- Runner protocol: the sessions, events and commands of a run, and its message queue.

CREATE TABLE run_sessions (
	run_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
	session INTEGER NOT NULL,
	state TEXT NOT NULL,
	bootstrap_hash TEXT NOT NULL DEFAULT '',
	version INTEGER NOT NULL,
	event_ack INTEGER NOT NULL DEFAULT 0,
	command_seq INTEGER NOT NULL DEFAULT 0,
	command_ack INTEGER NOT NULL DEFAULT 0,
	last_seen_ns INTEGER NOT NULL DEFAULT 0,
	data TEXT NOT NULL,
	PRIMARY KEY (run_id, session)
);
CREATE UNIQUE INDEX run_sessions_bootstrap ON run_sessions (bootstrap_hash) WHERE bootstrap_hash <> '';
CREATE INDEX run_sessions_open ON run_sessions (state, last_seen_ns);

CREATE TABLE runner_events (
	run_id TEXT NOT NULL,
	session INTEGER NOT NULL,
	seq INTEGER NOT NULL,
	data TEXT NOT NULL,
	PRIMARY KEY (run_id, session, seq),
	FOREIGN KEY (run_id, session) REFERENCES run_sessions (run_id, session) ON DELETE CASCADE
);

CREATE TABLE runner_commands (
	run_id TEXT NOT NULL,
	session INTEGER NOT NULL,
	seq INTEGER NOT NULL,
	data TEXT NOT NULL,
	PRIMARY KEY (run_id, session, seq),
	FOREIGN KEY (run_id, session) REFERENCES run_sessions (run_id, session) ON DELETE CASCADE
);

CREATE TABLE run_inputs (
	id TEXT NOT NULL UNIQUE,
	run_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	state TEXT NOT NULL,
	session INTEGER NOT NULL DEFAULT 0,
	turn_id TEXT NOT NULL DEFAULT '',
	accepted INTEGER NOT NULL DEFAULT 0,
	dedupe_key TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL,
	PRIMARY KEY (run_id, seq)
);
CREATE UNIQUE INDEX run_inputs_dedupe ON run_inputs (run_id, dedupe_key) WHERE dedupe_key <> '';
CREATE INDEX run_inputs_state ON run_inputs (run_id, state);

-- Timeline of a run: control-plane lifecycle events. The event ID makes a second write harmless.

CREATE TABLE run_timeline (
	run_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
	event_id TEXT NOT NULL,
	session INTEGER NOT NULL DEFAULT 0,
	type TEXT NOT NULL,
	at_ns INTEGER NOT NULL,
	data TEXT NOT NULL,
	PRIMARY KEY (run_id, event_id)
);
CREATE INDEX run_timeline_at ON run_timeline (run_id, at_ns);

-- Facts about pull requests for the metrics: the opener, the pushes with the
-- kind of their actor, the first review, the merge and the size.

CREATE TABLE pull_request_facts (
	id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	pr_key TEXT NOT NULL DEFAULT '',
	repo TEXT NOT NULL DEFAULT '',
	head_branch TEXT NOT NULL DEFAULT '',
	is_open INTEGER NOT NULL DEFAULT 1,
	data TEXT NOT NULL
);
CREATE INDEX pull_request_facts_team ON pull_request_facts (team_id, pr_key);
CREATE INDEX pull_request_facts_branch ON pull_request_facts (team_id, repo, head_branch);

-- Dispatch ledger: one row for each
-- action given to one target. The unique key makes a dispatch happen one time,
-- even when the event job is delivered again. The leader removes old rows.

CREATE TABLE dispatches (
	action_key TEXT NOT NULL,
	target TEXT NOT NULL,
	ref TEXT NOT NULL DEFAULT '',
	created_ns INTEGER NOT NULL,
	UNIQUE (action_key, target)
);
CREATE INDEX dispatches_created ON dispatches (created_ns);
