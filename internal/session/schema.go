package session

import (
	"database/sql"
	"fmt"
)

// currentSchema builds a new store at SchemaVersion in one step. Columns keep the order the old
// step-by-step migrations gave them, so a new store and a converted one have the same shape; the
// test TestNewStoreSchemaMatchesTheLastConvertedShape holds the two together.
//
// Why some columns sit where they do: usage is columns on the turn, since each turn has exactly one
// row of it; ACP reports cost as a cumulative session counter, so the session keeps the last one
// and a turn stores only its delta; the escalation floor, rewind and output contract are request
// data of one turn; a session's repository access, network posture, execution mode, source binding
// and job are immutable authority a restarted daemon relaunches it under. Empty strings in the
// later session columns mean "a session from before that field existed".
const currentSchema = `
CREATE TABLE operations (
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

CREATE TABLE sessions (
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
    updated_at INTEGER NOT NULL,
    companions TEXT NOT NULL DEFAULT '[]',
    pull_request_number INTEGER NOT NULL DEFAULT 0,
    pull_request_ref TEXT NOT NULL DEFAULT '',
    pull_request_head_commit TEXT NOT NULL DEFAULT '',
    project_env INTEGER NOT NULL DEFAULT 1,
    project_mcp INTEGER NOT NULL DEFAULT 1,
    usage_cumulative_cost_usd REAL NOT NULL DEFAULT 0,
    usage_cost_recorded INTEGER NOT NULL DEFAULT 0,
    repository_read_only INTEGER NOT NULL DEFAULT 0,
    fork_generation TEXT NOT NULL DEFAULT '',
    responder_endpoint TEXT NOT NULL DEFAULT '',
    responder_token TEXT NOT NULL DEFAULT '',
    workspace_task TEXT NOT NULL DEFAULT '',
    authority_digest TEXT NOT NULL DEFAULT '',
    repository_freshness TEXT NOT NULL DEFAULT '',
    network_mode TEXT NOT NULL DEFAULT 'open',
    network_fingerprint TEXT NOT NULL DEFAULT '',
    network_qualification TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL DEFAULT '',
    source_binding TEXT NOT NULL DEFAULT '',
    job_document TEXT NOT NULL DEFAULT '',
    job_digest TEXT NOT NULL DEFAULT ''
);

CREATE TABLE turns (
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
    usage_input_tokens INTEGER NOT NULL DEFAULT 0,
    usage_cached_input_tokens INTEGER NOT NULL DEFAULT 0,
    usage_output_tokens INTEGER NOT NULL DEFAULT 0,
    usage_reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    usage_cost_usd REAL NOT NULL DEFAULT 0,
    usage_cost_recorded INTEGER NOT NULL DEFAULT 0,
    min_target_index INTEGER NOT NULL DEFAULT 0,
    rewind_target INTEGER NOT NULL DEFAULT 0,
    output_schema BLOB NOT NULL DEFAULT '',
    output_schema_sha256 TEXT NOT NULL DEFAULT '',
    output_semantic_validation INTEGER NOT NULL DEFAULT 0,
    candidate_message TEXT NOT NULL DEFAULT '',
    candidate_sha256 TEXT NOT NULL DEFAULT '',
    validation_attempt INTEGER NOT NULL DEFAULT 0,
    validation_error TEXT NOT NULL DEFAULT '',
    validation_receipt TEXT NOT NULL DEFAULT '',
    runtime_run_id TEXT NOT NULL DEFAULT '',
    responder_endpoint TEXT NOT NULL DEFAULT '',
    responder_token TEXT NOT NULL DEFAULT '',
    UNIQUE(session_id, ordinal)
);

CREATE TABLE events (
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

CREATE INDEX turns_fifo ON turns(session_id, state, ordinal);
CREATE INDEX events_replay ON events(session_id, sequence);

CREATE TABLE turn_artifacts (
    turn_id TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    data BLOB NOT NULL,
    PRIMARY KEY(turn_id, ordinal)
);
CREATE INDEX turn_artifacts_turn ON turn_artifacts(turn_id, ordinal);

CREATE TABLE turn_output_artifacts (
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
CREATE INDEX turn_output_artifacts_turn ON turn_output_artifacts(turn_id, ordinal);

CREATE TABLE store_identity (
    slot INTEGER PRIMARY KEY CHECK (slot = 1),
    id TEXT NOT NULL
);
CREATE TABLE session_owner_bindings (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    store_id TEXT NOT NULL
);
`

// StoreStatus is what this build does with a session store, decided by its schema version alone.
// Open acts on it and CheckStore only reports it, so the two cannot disagree about a store.
type StoreStatus int

const (
	// StoreEmpty: there is no session database yet, or it has no schema; Open creates one.
	StoreEmpty StoreStatus = iota
	// StoreCurrent: Open uses the store as it is.
	StoreCurrent
	// StoreConverts: Open converts the store to SchemaVersion, and builds with an older schema
	// cannot open it afterwards.
	StoreConverts
	// StoreTooOld: the store predates OldestSchemaVersion, so Open refuses it.
	StoreTooOld
	// StoreTooNew: a newer build wrote the store, so Open refuses it.
	StoreTooNew
)

// schemaWindow is the range of store schemas a build opens: it converts oldest up to current.
type schemaWindow struct{ oldest, current int }

var supportedSchemas = schemaWindow{oldest: OldestSchemaVersion, current: SchemaVersion}

func (w schemaWindow) status(version int) StoreStatus {
	switch {
	case version > w.current:
		return StoreTooNew
	case version == w.current:
		return StoreCurrent
	case version == 0:
		return StoreEmpty
	case version < w.oldest:
		return StoreTooOld
	default:
		return StoreConverts
	}
}

// refusal is why Open refuses a store's schema, or nil when Open can use it. A too-old store is
// told which schema converts it, keyed to the state version because untagged builds are deployed;
// the release and commit that convert to schema 26 follow as a hint.
func (w schemaWindow) refusal(version int) error {
	switch status := w.status(version); {
	case version < 0:
		return fmt.Errorf("the session database has an invalid schema version %d", version)
	case status == StoreTooNew:
		return fmt.Errorf("session schema %d is newer than this coop build supports (%d to %d)",
			version, w.oldest, w.current)
	case status == StoreTooOld:
		return fmt.Errorf("session schema %d is older than this coop build supports (%d to %d); "+
			"open it once with a coop build that uses session schema %d and converts older stores "+
			"(v12.1.0, or any build at or before commit 29133b0e), then upgrade, "+
			"or start fresh with a new --state folder",
			version, w.oldest, w.current, w.oldest)
	}
	return nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if err := supportedSchemas.refusal(version); err != nil {
		return err
	}
	if version == SchemaVersion {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()
	switch version {
	case 0:
		if _, err := tx.Exec(currentSchema); err != nil {
			return fmt.Errorf("create session schema %d: %w", SchemaVersion, err)
		}
		if _, err := tx.Exec(`INSERT INTO store_identity (slot, id) VALUES (1, ?)`, randomID("store")); err != nil {
			return fmt.Errorf("initialize session store identity: %w", err)
		}
	default:
		// The refusal above lets through only versions this build has a step for.
		return fmt.Errorf("no conversion from session schema %d", version)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
