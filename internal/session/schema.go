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
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
