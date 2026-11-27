package fleet

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func (c *Controller) Submit(pool string, raw []byte, prompt string) (string, error) {
	spec, err := workerproto.DecodeJobSpec(raw)
	if err != nil {
		return "", err
	}
	if !identifier.MatchString(pool) || strings.TrimSpace(prompt) == "" || len(prompt) > 96<<10 {
		return "", errors.New("invalid pool or prompt (maximum 96 KiB)")
	}
	if spec.WorkspaceInput != nil {
		return "", errors.New("submit a local workspace input to Ryker with its operator job")
	}
	if spec.ModelCredentials != "" {
		return "", errors.New("controller-managed model access requires submitting the job to Ryker")
	}
	if spec.Runtime != nil && spec.Runtime.Compose != "" && spec.Runtime.MaxServices == 0 {
		return "", errors.New("fleet Compose jobs require runtime.max_services to reserve sidecars")
	}
	turn, err := json.Marshal(map[string]any{"expected_revision": int64(9223372036854775807), "prompt": prompt})
	if err != nil || len(turn) > 256<<10 {
		return "", errors.New("encoded turn exceeds inline transfer size")
	}
	doc, err := spec.CanonicalDocument()
	if err != nil {
		return "", err
	}
	if len(doc) > 96<<10 {
		return "", errors.New("job exceeds inline transfer size")
	}
	targets := slices.Clone(spec.Targets)
	if spec.Preset != nil {
		for _, role := range spec.Preset.Roles {
			targets = append(targets, role.Targets...)
		}
	}
	tx, err := c.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO jobs(id,pool,spec,prompt,stage) VALUES(?,?,?,?,'queued')", spec.JobRef, pool, doc, prompt); err != nil {
		return "", err
	}
	for _, name := range targets {
		t, err := agent.ParseTarget(name)
		if err != nil || t.String() != name || len(t.Accounts) != 1 {
			return "", errors.New("fleet targets require one explicit account")
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO job_accounts VALUES(?,?)", spec.JobRef, t.Provider+"@"+t.Account()); err != nil {
			return "", err
		}
	}
	return spec.JobRef, tx.Commit()
}

// Each job holds a permit for each account it can use, including fallback and
// preset roles. Reserving the complete set avoids spending a fallback twice.
func (c *Controller) SetAccountLimit(account string, permits int) error {
	t, err := agent.ParseTarget(account)
	if err != nil || len(t.Accounts) != 1 || account != t.Provider+"@"+t.Account() || permits < 0 || permits > 10000 {
		return errors.New("expected provider@account and a concurrency from 0 to 10000")
	}
	_, err = c.db.Exec("INSERT INTO account_limits VALUES(?,?) ON CONFLICT(account) DO UPDATE SET permits=excluded.permits", account, permits)
	return err
}

func observeQuota(tx *sql.Tx, report *workerproto.AccountUsage, now time.Time) error {
	if report == nil {
		return nil
	}
	for _, account := range report.Accounts {
		if account.CheckedAt == nil || account.Error != nil || account.SignIn != workerproto.AccountSignedIn || account.CheckedAt.After(now.Add(time.Minute)) || now.Sub(*account.CheckedAt) > 5*time.Minute {
			continue
		}
		var until int64
		for _, limit := range account.Limits {
			exhausted := limit.Available != nil && !*limit.Available || limit.UsedPercent != nil && *limit.UsedPercent >= 100
			if exhausted {
				reset := account.CheckedAt.Add(5 * time.Minute)
				if limit.ResetsAt != nil {
					reset = *limit.ResetsAt
				}
				until = max(until, reset.Unix())
			}
		}
		if _, err := tx.Exec(`INSERT INTO quota VALUES(?,?,?) ON CONFLICT(account) DO UPDATE SET measured=excluded.measured,resume=excluded.resume WHERE excluded.measured>quota.measured`, account.Account, account.CheckedAt.UnixMilli(), until); err != nil {
			return err
		}
	}
	return nil
}

const occupied = "('creating','operation','session','starting','running','cancelling','cancel_revision','uncertain')"

// This predicate is shared by placement and queue diagnostics. An uncertain
// turn continues to hold its permits until its outcome is reconciled.
const blockedAccounts = `SELECT a.account FROM job_accounts a
    LEFT JOIN account_limits l USING(account) LEFT JOIN quota q USING(account)
    WHERE a.job=j.id AND (q.resume>? OR
    (SELECT count(*) FROM job_accounts held JOIN jobs active ON active.id=held.job
      WHERE held.account=a.account AND active.stage IN ` + occupied + `)>=coalesce(l.permits,1))`

type JobStatus struct {
	WaitReason string          `json:"wait_reason,omitempty"`
	Job        string          `json:"job"`
	Stage      string          `json:"stage"`
	Worker     string          `json:"worker,omitempty"`
	Session    string          `json:"session,omitempty"`
	Turn       string          `json:"turn,omitempty"`
	Result     json.RawMessage `json:"latest_result,omitempty"`
	Output     json.RawMessage `json:"transferred_response,omitempty"`
}

func (c *Controller) Status(job string) (JobStatus, error) {
	var s JobStatus
	var raw, body []byte
	now := time.Now()
	// Read the job, its result and its queue diagnostics in one SQLite snapshot.
	err := c.db.QueryRow(`WITH online AS (
            SELECT w.hello,w.draining FROM workers w JOIN jobs j ON w.pool=j.pool
            WHERE j.id=? AND w.revoked=0 AND w.last_seen>=?
        )
        SELECT j.id,j.stage,coalesce(j.worker,''),coalesce(j.session,''),coalesce(j.turn,''),c.result,b.data,
          CASE WHEN j.stage!='queued' THEN ''
            WHEN NOT EXISTS (SELECT 1 FROM online) THEN 'No worker in this pool has polled in the past minute.'
            WHEN NOT EXISTS (SELECT 1 FROM online WHERE draining=0) THEN 'All online workers in this pool are draining.'
            WHEN EXISTS (`+blockedAccounts+`) THEN 'Waiting for an account concurrency permit or provider quota reset.'
            WHEN NOT EXISTS (SELECT 1 FROM online w WHERE draining=0 AND NOT EXISTS
              (SELECT 1 FROM job_accounts a WHERE a.job=j.id AND NOT EXISTS
                (SELECT 1 FROM json_each(CAST(w.hello AS TEXT),'$.account_usage.accounts') u
                 WHERE u.value->>'account'=a.account AND u.value->>'sign_in'=?)))
              THEN 'No available worker reports all requested model accounts signed in.'
            ELSE 'Waiting for a compatible worker to poll with sufficient free capacity.'
          END
        FROM jobs j
        LEFT JOIN commands c ON c.id=(SELECT id FROM commands WHERE job=j.id AND result IS NOT NULL ORDER BY rowid DESC LIMIT 1)
        LEFT JOIN bodies b ON b.command=c.id AND b.sha=json_extract(CAST(c.result AS TEXT),'$.resource.body_ref.sha256')
        WHERE j.id=?`, job, now.Add(-time.Minute).Unix(), now.Unix(), workerproto.AccountSignedIn, job).
		Scan(&s.Job, &s.Stage, &s.Worker, &s.Session, &s.Turn, &raw, &body, &s.WaitReason)
	s.Result = raw
	if json.Valid(body) {
		s.Output = body
	}
	return s, err
}

// Events emits one JSON object per retained event in sequence order. Paging
// releases the database connection before writing to a slow terminal.
func (c *Controller) Events(job string, after int64, to io.Writer) error {
	var exists int
	if err := c.db.QueryRow("SELECT 1 FROM jobs WHERE id=?", job).Scan(&exists); err != nil {
		return err
	}
	for {
		rows, err := c.db.Query("SELECT sequence,data FROM events WHERE job=? AND sequence>? ORDER BY sequence LIMIT 32", job, after)
		if err != nil {
			return err
		}
		var page [][]byte
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&after, &raw); err != nil {
				rows.Close()
				return err
			}
			page = append(page, raw)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, raw := range page {
			if _, err = to.Write(append(raw, '\n')); err != nil {
				return err
			}
		}
		if len(page) < 32 {
			return nil
		}
	}
}

// Cancel persists intent while a worker is offline. A later worker receipt
// determines whether a turn actually started and supplies its cancellation identity.
func (c *Controller) Cancel(job string) error {
	res, err := c.db.Exec("UPDATE jobs SET cancel_requested=1,stage=CASE WHEN stage='queued' THEN 'cancelled' ELSE stage END WHERE id=? AND stage IN ('queued','creating','operation','session','starting','running','cancel_revision','cancelling')", job)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if n == 0 && err == nil {
		return errors.New("job is unknown or no longer active")
	}
	return err
}

func dispatch(tx *sql.Tx, p workerproto.Poll) (workerproto.Response, error) {
	now := time.Now().UTC()
	out := workerproto.Response{Version: workerproto.Version, PollRef: p.PollRef, ServerTime: now, Commands: []workerproto.Command{}, AcknowledgedResultCommandIDs: []string{}, EventAcknowledgements: []workerproto.EventAcknowledgement{}}
	if err := observeQuota(tx, p.Worker.AccountUsage, now); err != nil {
		return out, err
	}
	for _, receipt := range p.CommandResults {
		var job string
		var prior []byte
		err := tx.QueryRow("SELECT job,result FROM commands WHERE id=? AND worker=?", receipt.CommandID, p.Worker.ID).Scan(&job, &prior)
		if err != nil || receipt.OperationKey != receipt.CommandID {
			return out, errors.New("unknown worker receipt")
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return out, err
		}
		if prior == nil {
			if _, err = tx.Exec("UPDATE commands SET result=? WHERE id=?", raw, receipt.CommandID); err != nil {
				return out, err
			}
			if err = advance(tx, job, p.Worker.ID, receipt); err != nil {
				return out, err
			}
		} else if !bytes.Equal(raw, prior) {
			return out, errors.New("conflicting worker receipt")
		}
		out.AcknowledgedResultCommandIDs = append(out.AcknowledgedResultCommandIDs, receipt.CommandID)
	}
	for _, batch := range p.EventBatches {
		var last int64
		if err := tx.QueryRow("SELECT coalesce((SELECT max(sequence) FROM events WHERE job=jobs.id),0) FROM jobs WHERE id=? AND worker=?", batch.SessionRef, p.Worker.ID).Scan(&last); err != nil {
			return out, err
		}
		if batch.PlacementGeneration != 1 || batch.AfterSequence > last {
			return out, errors.New("invalid event cursor")
		}
		for _, ev := range batch.Events {
			raw, err := json.Marshal(ev)
			if err != nil {
				return out, err
			}
			if ev.Sequence <= last {
				var saved []byte
				if err = tx.QueryRow("SELECT data FROM events WHERE job=? AND sequence=?", batch.SessionRef, ev.Sequence).Scan(&saved); err != nil {
					return out, err
				}
				if !bytes.Equal(saved, raw) {
					return out, errors.New("conflicting worker event")
				}
				continue
			}
			if _, err = tx.Exec("INSERT INTO events VALUES(?,?,?)", batch.SessionRef, ev.Sequence, raw); err != nil {
				return out, err
			}
		}
		if n := len(batch.Events); n > 0 {
			out.EventAcknowledgements = append(out.EventAcknowledgements, workerproto.EventAcknowledgement{SessionRef: batch.SessionRef, PlacementGeneration: 1, Sequence: batch.Events[n-1].Sequence})
		}
	}
	var id, job string
	var payload []byte
	find := func() error {
		return tx.QueryRow("SELECT id,job,payload FROM commands WHERE worker=? AND ready_at<=? AND result IS NULL ORDER BY rowid LIMIT 1", p.Worker.ID, now.UnixMilli()).Scan(&id, &job, &payload)
	}
	err := find()
	if errors.Is(err, sql.ErrNoRows) && p.Worker.State == "eligible" && p.Worker.Capacity.State == "eligible" && p.Worker.Capacity.TurnSlotsFree > 0 {
		if err = place(tx, p.Worker); err != nil {
			return out, err
		}
		err = find()
	}
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	cmd := workerproto.Command{WorkerID: p.Worker.ID, SessionRef: job, CommandID: id, IdempotencyKey: id, PlacementGeneration: 1, CommandVersion: workerproto.Version, Kind: "api_request", Payload: payload, LeaseRef: id, LeaseExpiresAt: now.Add(time.Minute)}
	if err = cmd.Validate(); err != nil {
		return out, err
	}
	out.Commands = append(out.Commands, cmd)
	return out, nil
}

func place(tx *sql.Tx, w workerproto.WorkerHello) error {
	var drain bool
	if err := tx.QueryRow("SELECT draining FROM workers WHERE id=?", w.ID).Scan(&drain); err != nil {
		return err
	}
	if drain || (w.Storage != nil && w.Storage.Allocation == workerproto.StorageAllocationRefused) {
		return nil
	}
	var reserved, active int
	var reservedCPU, reservedMemory, pendingCPU, pendingMemory int64
	const containers = "(1+coalesce(json_extract(CAST(spec AS TEXT),'$.runtime.max_services'),0))"
	if err := tx.QueryRow(`WITH claims AS (SELECT stage IN ('creating','operation','session','starting') AS pending,json_extract(CAST(spec AS TEXT),'$.resources.cpu_millis')*`+containers+` AS cpu,json_extract(CAST(spec AS TEXT),'$.resources.memory_bytes')*`+containers+` AS memory FROM jobs WHERE worker=? AND stage IN `+occupied+`) SELECT count(*),coalesce(sum(pending),0),coalesce(sum(cpu),0),coalesce(sum(memory),0),coalesce(sum(cpu*pending),0),coalesce(sum(memory*pending),0) FROM claims`, w.ID).Scan(&reserved, &active, &reservedCPU, &reservedMemory, &pendingCPU, &pendingMemory); err != nil {
		return err
	}
	if reserved >= w.Capacity.TurnSlotsTotal || active >= w.Capacity.TurnSlotsFree {
		return nil
	}
	var id string
	var doc []byte
	has := func(name, version string) bool {
		return slices.Contains(w.Capabilities, workerproto.Capability{Name: name, Version: version})
	}
	if !has("job-setup", "2") {
		return nil
	}
	cpu, memory := int64(math.MaxInt64), int64(math.MaxInt64)
	if m := w.Capacity.Machine; m != nil {
		cpu = min(m.CPUMillis-reservedCPU, m.FreeCPUMillis-pendingCPU)
		memory = min(m.MemoryBytes-reservedMemory, m.FreeMemoryBytes-pendingMemory)
	}
	var signedIn []string
	if w.AccountUsage != nil {
		for _, account := range w.AccountUsage.Accounts {
			if account.SignIn == workerproto.AccountSignedIn {
				signedIn = append(signedIn, account.Account)
			}
		}
	}
	if len(signedIn) == 0 {
		return nil
	}
	accounts, err := json.Marshal(signedIn)
	if err != nil {
		return err
	}
	err = tx.QueryRow(`SELECT id,spec FROM jobs j WHERE stage='queued' AND pool=?
		AND json_extract(CAST(spec AS TEXT),'$.resources.cpu_millis')*`+containers+`<=?
		AND json_extract(CAST(spec AS TEXT),'$.resources.memory_bytes')*`+containers+`<=?
		AND (json_extract(CAST(spec AS TEXT),'$.runtime.max_services') IS NULL OR ?)
		AND (json_extract(CAST(spec AS TEXT),'$.runtime.max_services') IS NULL OR ?)
		AND json_extract(CAST(spec AS TEXT),'$.model_credentials') IS NULL
		AND NOT EXISTS (SELECT 1 FROM job_accounts a WHERE a.job=j.id AND a.account NOT IN (SELECT value FROM json_each(?)))
		AND (json_type(CAST(spec AS TEXT),'$.runtime') IS NULL OR ?)
		AND (json_type(CAST(spec AS TEXT),'$.preset') IS NULL OR ?)
		AND (? OR NOT EXISTS (SELECT 1 FROM json_each(CAST(spec AS TEXT),'$.targets') WHERE value LIKE '%+fast@%') AND NOT EXISTS (SELECT 1 FROM json_each(CAST(spec AS TEXT),'$.preset.roles') roles,json_each(roles.value,'$.targets') targets WHERE targets.value LIKE '%+fast@%'))
		AND NOT EXISTS (`+blockedAccounts+`)
		ORDER BY j.rowid LIMIT 1`, w.WorkspaceRef, cpu, memory, w.Capacity.Machine != nil, has("project-service-budget", "1"), string(accounts), has("project-runtime", "1"), has("job-preset", "1"), has("fast-target", "1"), time.Now().Unix()).Scan(&id, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	spec, err := workerproto.DecodeJobSpec(doc)
	if err != nil {
		return err
	}
	digest, err := spec.Digest()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE jobs SET stage='creating',worker=? WHERE id=?", w.ID, id); err != nil {
		return err
	}
	return enqueue(tx, id, w.ID, "POST", "/v1/sessions", map[string]any{"job": json.RawMessage(doc), "task": id, "expected_job_digest": digest})
}
func enqueue(tx *sql.Tx, job, worker, method, path string, body any) error {
	req := workerproto.APIRequest{Path: path, Method: method}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req.Body = b
	}
	if err := req.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var ready int64
	if method == "GET" && (strings.Contains(path, "/turns/") || strings.HasPrefix(path, "/v1/operations/")) {
		ready = time.Now().Add(2 * time.Second).UnixMilli()
	}
	_, err = tx.Exec("INSERT INTO commands(id,job,worker,payload,ready_at) VALUES(?,?,?,?,?)", rand.Text(), job, worker, payload, ready)
	return err
}

type createOperation struct {
	ID           string
	State        string
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

func followCreate(tx *sql.Tx, job, worker string, op createOperation) (string, error) {
	if !identifier.MatchString(op.ID) {
		return "uncertain", nil
	}
	switch op.State {
	case "reserved", "running":
		return "operation", enqueue(tx, job, worker, "GET", "/v1/operations/"+op.ID, nil)
	case "succeeded":
		if op.ResourceType == "session" && identifier.MatchString(op.ResourceID) {
			return "session", enqueue(tx, job, worker, "GET", "/v1/sessions/"+op.ResourceID, nil)
		}
	case "failed":
		return "failed", nil
	}
	return "uncertain", nil
}

func advance(tx *sql.Tx, job, worker string, result workerproto.CommandResult) error {
	var stage, prompt, sessionID, turnID string
	var cancel bool
	if err := tx.QueryRow("SELECT stage,prompt,coalesce(session,''),coalesce(turn,''),cancel_requested FROM jobs WHERE id=?", job).Scan(&stage, &prompt, &sessionID, &turnID, &cancel); err != nil {
		return err
	}
	var resp workerproto.APIResponse
	var resource struct {
		Operation createOperation
		Session   struct {
			ID       string
			Revision int
		}
		Turn struct{ ID, State string }
	}
	next := "failed"
	if result.State == "succeeded" {
		if err := json.Unmarshal(result.Resource, &resp); err != nil {
			next = "uncertain"
		}
	}
	if result.State == "uncertain" {
		next = "uncertain"
	} else if result.State == "succeeded" && stage == "cancelling" && resp.Status == 409 {
		// A target rotation may advance the revision, or the turn may already
		// be terminal. Re-read it before deciding whether to cancel again.
		next = "running"
		if err := enqueue(tx, job, worker, "GET", fmt.Sprintf("/v1/sessions/%s/turns/%s", sessionID, turnID), nil); err != nil {
			return err
		}
	} else if result.State == "succeeded" && resp.Status >= 200 && resp.Status < 300 {
		if resp.BodyRef != nil {
			var body []byte
			if err := tx.QueryRow("SELECT data FROM bodies WHERE command=? AND sha=? AND length(data)=?", result.CommandID, resp.BodyRef.SHA256, resp.BodyRef.ByteSize).Scan(&body); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			resp.Body = body
		}
		if json.Unmarshal(resp.Body, &resource) != nil {
			stage = "invalid"
		}
		switch stage {
		case "creating", "operation", "session":
			if stage == "operation" || (stage == "creating" && resp.Status == 202) {
				if stage == "operation" {
					_ = json.Unmarshal(resp.Body, &resource.Operation)
				}
				var err error
				next, err = followCreate(tx, job, worker, resource.Operation)
				if err != nil {
					return err
				}
				break
			}
			if stage == "session" {
				_ = json.Unmarshal(resp.Body, &resource.Session)
			}
			if !identifier.MatchString(resource.Session.ID) || resource.Session.Revision < 1 {
				next = "uncertain"
				break
			}
			sessionID = resource.Session.ID
			if cancel {
				next = "cancelled"
				break
			}
			err := enqueue(tx, job, worker, "POST", "/v1/sessions/"+sessionID+"/turns", map[string]any{"expected_revision": resource.Session.Revision, "prompt": prompt})
			if err != nil {
				return err
			}
			next = "starting"
		case "starting", "running", "cancelling":
			if stage == "running" {
				if err := json.Unmarshal(resp.Body, &resource.Turn); err != nil {
					next = "uncertain"
					break
				}
			}
			if !identifier.MatchString(resource.Turn.ID) || (turnID != "" && resource.Turn.ID != turnID) {
				next = "uncertain"
				resource.Turn.ID = ""
				break
			}
			switch resource.Turn.State {
			case "queued", "starting", "running":
				if cancel && stage != "cancelling" {
					next = "cancel_revision"
					if err := enqueue(tx, job, worker, "GET", "/v1/sessions/"+sessionID, nil); err != nil {
						return err
					}
					break
				}
				next = "running"
				if err := enqueue(tx, job, worker, "GET", fmt.Sprintf("/v1/sessions/%s/turns/%s", sessionID, resource.Turn.ID), nil); err != nil {
					return err
				}
			case "completed", "failed", "cancelled", "interrupted", "budget_exhausted", "awaiting_validation":
				next = resource.Turn.State
			default:
				next = "uncertain"
			}
		case "cancel_revision":
			if err := json.Unmarshal(resp.Body, &resource.Session); err != nil || resource.Session.ID != sessionID || resource.Session.Revision < 1 {
				next = "uncertain"
				break
			}
			if err := enqueue(tx, job, worker, "POST", fmt.Sprintf("/v1/sessions/%s/turns/%s/cancel", sessionID, turnID), map[string]int{"expected_revision": resource.Session.Revision}); err != nil {
				return err
			}
			next = "cancelling"
		default:
			next = "uncertain"
		}
	}
	_, err := tx.Exec("UPDATE jobs SET stage=?,session=?,turn=coalesce(nullif(?,''),turn) WHERE id=?", next, sessionID, resource.Turn.ID, job)
	return err
}
