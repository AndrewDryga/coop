package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// stageRecord is one JSON-Lines row per loop stage — the raw material for measuring the harness
// itself: which target ACTUALLY ran (post-rotation, not the configured ladder), whether a review
// reopened anything, how many retries a stage cost, and what the queue and HEAD did across it.
// Written best-effort to .agent/runs/<run>.jsonl (gitignored working state); a write error never
// blocks or fails an iteration. This is phase 1 (emit) — a replay/canary set over the archive is a
// separate follow-on.
type stageRecord struct {
	Run         string   `json:"run"`
	Stage       string   `json:"stage"`    // preflight | work | between | signoff
	Provider    string   `json:"provider"` // the EFFECTIVE target, after any rate-limit rotation
	Model       string   `json:"model,omitempty"`
	Effort      string   `json:"effort,omitempty"`
	Account     string   `json:"account,omitempty"`
	Coop        string   `json:"coop"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Exit        int      `json:"exit"`
	Retries     int      `json:"retries,omitempty"`
	CostUSD     float64  `json:"cost_usd,omitempty"` // the stage's result-event cost (lead + its native subagents)
	InTok       int      `json:"in_tok,omitempty"`   // input tokens (fresh + cache write + cache read)
	OutTok      int      `json:"out_tok,omitempty"`  // output tokens
	HeadBefore  string   `json:"head_before,omitempty"`
	HeadAfter   string   `json:"head_after,omitempty"`
	Reopened    int      `json:"reopened,omitempty"`    // review stages: task folders moved back to in_progress
	Finished    []string `json:"finished,omitempty"`    // work stage: task ids this iteration moved to done
	Untrailered []string `json:"untrailered,omitempty"` // finished ids with NO Coop-Task commit in range (unbindable)
	QueueTodo   int      `json:"queue_todo"`
	QueueDoing  int      `json:"queue_doing"`
	QueueDone   int      `json:"queue_done"`
}

// buildStageRecord assembles a record from a stage's EFFECTIVE target (the post-rotation Target, so
// the row shows what ran, not what was configured) and its outcome. Pure — unit-tested.
func buildStageRecord(run, stage, coopVer string, tgt agents.Target, start, end time.Time, exit, retries, reopened int, headBefore, headAfter string, q taskCounts, finished, untrailered []string) stageRecord {
	return stageRecord{
		Run:         run,
		Stage:       stage,
		Provider:    tgt.Provider,
		Model:       tgt.Model,
		Effort:      tgt.Effort,
		Account:     tgt.Account(),
		Coop:        coopVer,
		Start:       start.UTC().Format(time.RFC3339),
		End:         end.UTC().Format(time.RFC3339),
		Exit:        exit,
		Retries:     retries,
		HeadBefore:  headBefore,
		HeadAfter:   headAfter,
		Reopened:    reopened,
		Finished:    finished,
		Untrailered: untrailered,
		QueueTodo:   q.Todo,
		QueueDoing:  q.Doing,
		QueueDone:   q.Done,
	}
}

// appendStageRecord appends one record to .agent/runs/<run>.jsonl under repo. Best-effort: the
// error is returned only so the caller can warn once — it must never fail a loop iteration.
func appendStageRecord(repo, run string, rec stageRecord) error {
	dir := filepath.Join(repo, ".agent", "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, run+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// recordStage builds and appends a stage record, stamping end-time, HEAD-after, and the queue
// counts at emit time. Best-effort — a write failure is warned once and swallowed, so telemetry can
// never break the run it observes.
func (a *app) recordStage(repo, run, stage string, tgt agents.Target, start time.Time, exit, retries, reopened int, headBefore string, hosts, finished, untrailered []string, res *iterResult) {
	cnt, _ := queueProgress(hosts)
	rec := buildStageRecord(run, stage, resolveVersion(), tgt, start, time.Now(), exit, retries, reopened, headBefore, gitOut(repo, "rev-parse", "HEAD"), cnt, finished, untrailered)
	if res != nil { // the box run's result-event tally (nil for stages that had no stream-json result)
		rec.CostUSD, rec.InTok, rec.OutTok = res.CostUSD, res.InTok, res.OutTok
	}
	if err := appendStageRecord(repo, run, rec); err != nil {
		ui.Warn("telemetry: could not record the %s stage: %v", stage, err)
	}
}

// stageCost is a cost/token tally — for one task or a whole run.
type stageCost struct {
	usd    float64
	inTok  int
	outTok int
}

// runCost is a run's cost broken out per task (a work stage's cost, keyed by the task it finished)
// plus the run total (EVERY stage's cost, so review overhead shows in the total even before it's
// attributed per task — that per-model/per-peer attribution is the follow-on slice).
type runCost struct {
	byTask map[string]stageCost
	total  stageCost
}

// readStageRecords reads a run's telemetry rows back from .agent/runs/<run>.jsonl. Best-effort: a
// missing/unreadable file or a malformed line yields what parsed, never an error — the closing
// summary that reads this must never break the run's end.
func readStageRecords(repo, run string) []stageRecord {
	data, err := os.ReadFile(filepath.Join(repo, ".agent", "runs", run+".jsonl"))
	if err != nil {
		return nil
	}
	var recs []stageRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r stageRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs
}

// costFromRecords aggregates telemetry into a runCost: every stage's cost/tokens sum into the total,
// and a stage that carries cost is attributed to the task(s) it finished (split evenly on the rare
// multi-finish; cost with no finished task lands in the total only). Pure — unit-tested.
func costFromRecords(recs []stageRecord) runCost {
	rc := runCost{byTask: map[string]stageCost{}}
	for _, r := range recs {
		rc.total.usd += r.CostUSD
		rc.total.inTok += r.InTok
		rc.total.outTok += r.OutTok
		n := len(r.Finished)
		if n == 0 || (r.CostUSD == 0 && r.InTok == 0 && r.OutTok == 0) {
			continue
		}
		for _, id := range r.Finished {
			c := rc.byTask[id]
			c.usd += r.CostUSD / float64(n)
			c.inTok += r.InTok / n
			c.outTok += r.OutTok / n
			rc.byTask[id] = c
		}
	}
	return rc
}
