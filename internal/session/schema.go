package session

import (
	"database/sql"
	"fmt"
)

const schemaV1 = `
CREATE TABLE IF NOT EXISTS operations (
    id TEXT PRIMARY KEY,
    method TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_hash TEXT NOT NULL,
    state TEXT NOT NULL,
    resource_type TEXT NOT NULL DEFAULT '',
    resource_id TEXT NOT NULL DEFAULT '',
    result BLOB,
    error_code TEXT NOT NULL DEFAULT '',
    error_detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    external_ref TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL,
    policy TEXT NOT NULL DEFAULT '',
    policy_digest TEXT NOT NULL DEFAULT '',
    repository TEXT NOT NULL DEFAULT '',
    workspace TEXT NOT NULL DEFAULT '',
    fork_name TEXT NOT NULL DEFAULT '',
    base_commit TEXT NOT NULL DEFAULT '',
    native_session_id TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL,
    state TEXT NOT NULL,
    activity TEXT NOT NULL,
    max_turns INTEGER NOT NULL,
    max_queued_turns INTEGER NOT NULL,
    max_queued_bytes INTEGER NOT NULL,
    turn_timeout INTEGER NOT NULL DEFAULT 3600000000000,
    max_patch_bytes INTEGER NOT NULL DEFAULT 1048576,
    turns_used INTEGER NOT NULL DEFAULT 0,
    queued_turn_count INTEGER NOT NULL DEFAULT 0,
    queued_prompt_bytes INTEGER NOT NULL DEFAULT 0,
    active_turn_id TEXT NOT NULL DEFAULT '',
    next_ordinal INTEGER NOT NULL DEFAULT 1,
    last_event_sequence INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS turns (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    state TEXT NOT NULL,
    send_state TEXT NOT NULL DEFAULT 'none',
    prompt TEXT NOT NULL,
    queued_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER,
    stop_reason TEXT NOT NULL DEFAULT '',
    assistant_message TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    error_detail TEXT NOT NULL DEFAULT '',
    UNIQUE(session_id, ordinal)
);

CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL,
    turn_id TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL,
    version INTEGER NOT NULL,
    occurred_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    UNIQUE(session_id, sequence)
);

CREATE INDEX IF NOT EXISTS turns_fifo ON turns(session_id, state, ordinal);
CREATE INDEX IF NOT EXISTS events_replay ON events(session_id, sequence);
`

const schemaV2 = `
ALTER TABLE sessions ADD COLUMN companions TEXT NOT NULL DEFAULT '[]';
`

const schemaV3 = `
CREATE TABLE IF NOT EXISTS turn_artifacts (
    turn_id TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    data BLOB NOT NULL,
    PRIMARY KEY(turn_id, ordinal)
);
CREATE INDEX IF NOT EXISTS turn_artifacts_turn ON turn_artifacts(turn_id, ordinal);
`

const schemaV4 = `
CREATE TABLE IF NOT EXISTS turn_output_artifacts (
    turn_id TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    id TEXT NOT NULL,
    name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    data BLOB NOT NULL,
    PRIMARY KEY(turn_id, id),
    UNIQUE(turn_id, ordinal),
    UNIQUE(turn_id, sha256)
);
CREATE INDEX IF NOT EXISTS turn_output_artifacts_turn ON turn_output_artifacts(turn_id, ordinal);
`

// Usage columns, added rather than a table: exactly one row of usage exists per
// turn, so a join would buy nothing and a nullable set of columns says
// "provider reported nothing" as naturally as a missing row would.
const schemaV5 = `
ALTER TABLE turns ADD COLUMN usage_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_cached_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_reasoning_tokens INTEGER NOT NULL DEFAULT 0;
`

const schemaV6 = `
ALTER TABLE sessions ADD COLUMN pull_request_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN pull_request_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN pull_request_head_commit TEXT NOT NULL DEFAULT '';
`

const schemaV7 = `
ALTER TABLE sessions ADD COLUMN project_env INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN project_mcp INTEGER NOT NULL DEFAULT 1;
`

// ACP reports monetary cost as a cumulative session counter. Keep the last
// counter on the session so turn completion can atomically persist only this
// turn's delta, including across daemon restarts and adapter process resets.
const schemaV8 = `
ALTER TABLE sessions ADD COLUMN usage_cumulative_cost_usd REAL NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN usage_cost_recorded INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_cost_usd REAL NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_cost_recorded INTEGER NOT NULL DEFAULT 0;
`

// The escalation floor is per-turn request data, so it lives on the turn rather
// than the session: a caller re-delivering a corrected turn on a higher rung is
// making a claim about that turn, not editing the session's ladder.
const schemaV9 = `
ALTER TABLE turns ADD COLUMN min_target_index INTEGER NOT NULL DEFAULT 0;
`

// A turn may explicitly return a durably escalated session to its first policy
// rung. This is request data like the escalation floor and must survive the
// asynchronous admission/lease boundary.
const schemaV10 = `
ALTER TABLE turns ADD COLUMN rewind_target INTEGER NOT NULL DEFAULT 0;
`

// Repository access is immutable session authority. It must survive daemon
// restarts rather than being re-read from an operator policy that may have
// changed while the session was open.
const schemaV11 = `
ALTER TABLE sessions ADD COLUMN repository_read_only INTEGER NOT NULL DEFAULT 0;
`

// The output contract is execution authority for completion, not an input
// artifact the runner may forget after admission. Persist the exact bytes and
// their caller-supplied digest on the turn that must satisfy them.
const schemaV12 = `
ALTER TABLE turns ADD COLUMN output_schema BLOB NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN output_schema_sha256 TEXT NOT NULL DEFAULT '';
`

// A schema-valid model result is still only a candidate when the caller owns
// semantic rules that depend on frozen external state. Keep that candidate
// separate from the published assistant message until its exact digest is
// accepted.
const schemaV13 = `
ALTER TABLE turns ADD COLUMN output_semantic_validation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN candidate_message TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN candidate_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN validation_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN validation_error TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN validation_receipt TEXT NOT NULL DEFAULT '';
`

// Fork names are reusable; a durable remote session owns one exact workspace incarnation.
const schemaV14 = `
ALTER TABLE sessions ADD COLUMN fork_generation TEXT NOT NULL DEFAULT '';
`

// A turn can finish successfully before its container cleanup succeeds. Keep
// the exact runtime identity independently of turn state until teardown is
// proven, including when the turn borrowed a session's warm container.
const schemaV15 = `
ALTER TABLE turns ADD COLUMN runtime_run_id TEXT NOT NULL DEFAULT '';
`

// The controller may bind exactly one HTTPS Responder state MCP endpoint to a
// session. This is separate from the operator-owned shared MCP catalog and
// survives daemon restarts and provider-native session resets.
const schemaV16 = `
ALTER TABLE sessions ADD COLUMN responder_endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN responder_token TEXT NOT NULL DEFAULT '';
`

// A remote engineering session binds one controller-approved task projection. The JSON is a
// compact immutable identity, not a mirror of the task files or their mutable lifecycle state.
const schemaV17 = `
ALTER TABLE sessions ADD COLUMN workspace_task TEXT NOT NULL DEFAULT '';
`

// Responder state-tool authority is logical-turn scoped. A native session may
// continue across many turns, but a completed or replaced turn's bearer must
// not authorize its successor.
const schemaV18 = `
ALTER TABLE turns ADD COLUMN responder_endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN responder_token TEXT NOT NULL DEFAULT '';
`

// Policy identity includes model and budget choices. Keep the model-independent authority digest
// beside it so a controller can prove that differently sized execution lanes cannot widen access.
const schemaV19 = `
ALTER TABLE sessions ADD COLUMN authority_digest TEXT NOT NULL DEFAULT '';
`

// Repository resolution is performed by Coop before a remote session exists.
// Keep its exact non-secret receipts beside the immutable session bindings so
// controllers never have to infer freshness from a later worker heartbeat.
const schemaV20 = `
ALTER TABLE sessions ADD COLUMN repository_freshness TEXT NOT NULL DEFAULT '';
`

// A session's network posture is frozen when it is created, not re-resolved per
// turn: the mode plus, for a filtered session, the owner-keyed snapshot
// fingerprint and the host qualification its runs must match. An open or
// offline session stores its mode and two empty strings.
const schemaV21 = `
ALTER TABLE sessions ADD COLUMN network_mode TEXT NOT NULL DEFAULT 'open';
ALTER TABLE sessions ADD COLUMN network_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN network_qualification TEXT NOT NULL DEFAULT '';
`

// The execution mode is immutable session authority like the read-only bit above it: a
// restarted daemon relaunches the session under the mode it was created with, never under
// a policy edited since. A row from before modes existed stores an empty mode and reads as normal.
const schemaV22 = `
ALTER TABLE sessions ADD COLUMN mode TEXT NOT NULL DEFAULT '';
`

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	if version == SchemaVersion {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()
	if version < 1 {
		if _, err := tx.Exec(schemaV1); err != nil {
			return fmt.Errorf("create schema v1: %w", err)
		}
		version = 1
	}
	if version < 2 {
		if _, err := tx.Exec(schemaV2); err != nil {
			return fmt.Errorf("migrate schema v2: %w", err)
		}
		version = 2
	}
	if version < 3 {
		if _, err := tx.Exec(schemaV3); err != nil {
			return fmt.Errorf("migrate schema v3: %w", err)
		}
		version = 3
	}
	if version < 4 {
		if _, err := tx.Exec(schemaV4); err != nil {
			return fmt.Errorf("migrate schema v4: %w", err)
		}
		version = 4
	}
	if version < 5 {
		if _, err := tx.Exec(schemaV5); err != nil {
			return fmt.Errorf("migrate schema v5: %w", err)
		}
		version = 5
	}
	if version < 6 {
		if _, err := tx.Exec(schemaV6); err != nil {
			return fmt.Errorf("migrate schema v6: %w", err)
		}
		version = 6
	}
	if version < 7 {
		if _, err := tx.Exec(schemaV7); err != nil {
			return fmt.Errorf("migrate schema v7: %w", err)
		}
		version = 7
	}
	if version < 8 {
		if _, err := tx.Exec(schemaV8); err != nil {
			return fmt.Errorf("migrate schema v8: %w", err)
		}
		version = 8
	}
	if version < 9 {
		if _, err := tx.Exec(schemaV9); err != nil {
			return fmt.Errorf("migrate schema v9: %w", err)
		}
		version = 9
	}
	if version < 10 {
		if _, err := tx.Exec(schemaV10); err != nil {
			return fmt.Errorf("migrate schema v10: %w", err)
		}
		version = 10
	}
	if version < 11 {
		if _, err := tx.Exec(schemaV11); err != nil {
			return fmt.Errorf("migrate schema v11: %w", err)
		}
		version = 11
	}
	if version < 12 {
		if _, err := tx.Exec(schemaV12); err != nil {
			return fmt.Errorf("migrate schema v12: %w", err)
		}
		version = 12
	}
	if version < 13 {
		if _, err := tx.Exec(schemaV13); err != nil {
			return fmt.Errorf("migrate schema v13: %w", err)
		}
		version = 13
	}
	if version < 14 {
		if _, err := tx.Exec(schemaV14); err != nil {
			return fmt.Errorf("migrate schema v14: %w", err)
		}
		version = 14
	}
	if version < 15 {
		if _, err := tx.Exec(schemaV15); err != nil {
			return fmt.Errorf("migrate schema v15: %w", err)
		}
		version = 15
	}
	if version < 16 {
		if _, err := tx.Exec(schemaV16); err != nil {
			return fmt.Errorf("migrate schema v16: %w", err)
		}
		version = 16
	}
	if version < 17 {
		if _, err := tx.Exec(schemaV17); err != nil {
			return fmt.Errorf("migrate schema v17: %w", err)
		}
		version = 17
	}
	if version < 18 {
		if _, err := tx.Exec(schemaV18); err != nil {
			return fmt.Errorf("migrate schema v18: %w", err)
		}
		version = 18
	}
	if version < 19 {
		if _, err := tx.Exec(schemaV19); err != nil {
			return fmt.Errorf("migrate schema v19: %w", err)
		}
		version = 19
	}
	if version < 20 {
		if _, err := tx.Exec(schemaV20); err != nil {
			return fmt.Errorf("migrate schema v20: %w", err)
		}
		version = 20
	}
	if version < 21 {
		if _, err := tx.Exec(schemaV21); err != nil {
			return fmt.Errorf("migrate schema v21: %w", err)
		}
		version = 21
	}
	if version < 22 {
		if _, err := tx.Exec(schemaV22); err != nil {
			return fmt.Errorf("migrate schema v22: %w", err)
		}
		version = 22
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
