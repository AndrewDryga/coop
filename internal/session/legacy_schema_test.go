package session

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestHistoricalSchemaFixturesStayFrozen(t *testing.T) {
	const want = "216533374394b6b5294c300905f43b1dca9b911f771f7f6c12f5013078340436"
	if len(legacySchemas) != 25 {
		t.Fatalf("historical versions=%d, want25", len(legacySchemas))
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(legacySchemas[24]))); got != "5caf80ce8e079b9cf7d2396db462369c5edb8e83c0c420c76b2caf1add304ea6" {
		t.Fatalf("historical v25 DDL drifted: %s", got)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(legacySchemas[:24], "")))); got != want {
		t.Fatalf("historical DDL drifted: %s", got)
	}
}

// Historical DDL 1–24 frozen from 260ccd2. Do not replace these with current production constants:
// migrations must repair independently authored old database bytes, not their own setup output.
var legacySchemas = []string{
	`
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
`,
	`
ALTER TABLE sessions ADD COLUMN companions TEXT NOT NULL DEFAULT '[]';
`,
	`
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
`,
	`
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
`,
	`
ALTER TABLE turns ADD COLUMN usage_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_cached_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_reasoning_tokens INTEGER NOT NULL DEFAULT 0;
`,
	`
ALTER TABLE sessions ADD COLUMN pull_request_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN pull_request_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN pull_request_head_commit TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN project_env INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN project_mcp INTEGER NOT NULL DEFAULT 1;
`,
	`
ALTER TABLE sessions ADD COLUMN usage_cumulative_cost_usd REAL NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN usage_cost_recorded INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_cost_usd REAL NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN usage_cost_recorded INTEGER NOT NULL DEFAULT 0;
`,
	`
ALTER TABLE turns ADD COLUMN min_target_index INTEGER NOT NULL DEFAULT 0;
`,
	`
ALTER TABLE turns ADD COLUMN rewind_target INTEGER NOT NULL DEFAULT 0;
`,
	`
ALTER TABLE sessions ADD COLUMN repository_read_only INTEGER NOT NULL DEFAULT 0;
`,
	`
ALTER TABLE turns ADD COLUMN output_schema BLOB NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN output_schema_sha256 TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE turns ADD COLUMN output_semantic_validation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN candidate_message TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN candidate_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN validation_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN validation_error TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN validation_receipt TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN fork_generation TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE turns ADD COLUMN runtime_run_id TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN responder_endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN responder_token TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN workspace_task TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE turns ADD COLUMN responder_endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN responder_token TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN authority_digest TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN repository_freshness TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN network_mode TEXT NOT NULL DEFAULT 'open';
ALTER TABLE sessions ADD COLUMN network_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN network_qualification TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN mode TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN source_binding TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE sessions ADD COLUMN job_document TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN job_digest TEXT NOT NULL DEFAULT '';
`,
	// v25 frozen from23fbdc1b; the earlier1-24 prefix remains byte-identical.
	`
CREATE TABLE store_identity (
    slot INTEGER PRIMARY KEY CHECK (slot = 1),
    id TEXT NOT NULL
);
CREATE TABLE session_owner_bindings (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    store_id TEXT NOT NULL
);
`,
}
