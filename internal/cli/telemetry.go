package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
	GateFiles   []string `json:"gate_files,omitempty"`  // host-detected gate-defining paths touched by the stage
	QueueTodo   int      `json:"queue_todo"`
	QueueDoing  int      `json:"queue_doing"`
	QueueDone   int      `json:"queue_done"`
}

// buildStageRecord assembles a record from a stage's EFFECTIVE target (the post-rotation Target, so
// the row shows what ran, not what was configured) and its outcome. Pure — unit-tested.
func buildStageRecord(run, stage, coopVer string, tgt agents.Target, start, end time.Time, exit, retries, reopened int, headBefore, headAfter string, q taskCounts, finished, untrailered, gateFiles []string) stageRecord {
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
		GateFiles:   gateFiles,
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
func (a *app) recordStage(repo, run, stage string, tgt agents.Target, start time.Time, exit, retries, reopened int, headBefore string, hosts, finished, untrailered, gateFiles []string, res *iterResult) {
	cnt, _ := queueProgress(hosts)
	rec := buildStageRecord(run, stage, resolveVersion(), tgt, start, time.Now(), exit, retries, reopened, headBefore, gitOut(repo, "rev-parse", "HEAD"), cnt, finished, untrailered, gateFiles)
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

// runCost is a run's cost broken out per task (a work stage's cost, keyed by the task it finished),
// per model (every costed stage's model, plus peer models once captured), and the run total (EVERY
// stage's cost, so review overhead shows in the total even before it's attributed per task).
type runCost struct {
	byTask  map[string]stageCost
	byModel []modelSpend
	total   stageCost
}

// modelSpend is one model's cost roll-up for the by-model line in the closing digest.
type modelSpend struct {
	model string // provider[:model]
	cost  stageCost
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

// peerRecord is one in-turn consult/delegate call's usage, appended by the wrapper (best-effort) to
// .agent/runs/<run>.peers.jsonl. Providers report tokens; cost isn't in every provider's stream
// (codex gives none), so peers contribute tokens to the by-model roll-up, not dollars.
type peerRecord struct {
	Run      string `json:"run"`
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	In       int    `json:"in"`
	Out      int    `json:"out"`
}

// readPeerRecords reads a run's peer-usage rows from .agent/runs/<run>.peers.jsonl. Best-effort, same
// contract as readStageRecords: a missing/unreadable file or a bad line yields what parsed.
func readPeerRecords(repo, run string) []peerRecord {
	data, err := os.ReadFile(filepath.Join(repo, ".agent", "runs", run+".peers.jsonl"))
	if err != nil {
		return nil
	}
	var recs []peerRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r peerRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs
}

// costForRepo aggregates EVERY run's telemetry under repo (.agent/runs/*.jsonl + the matching
// *.peers.jsonl) into one runCost — a fork/clone's total spend across all its loop runs, for the
// fleet board and fork review/merge. Best-effort: no runs dir → a zero runCost.
func costForRepo(repo string) runCost {
	entries, err := os.ReadDir(filepath.Join(repo, ".agent", "runs"))
	if err != nil {
		return runCost{byTask: map[string]stageCost{}}
	}
	var stages []stageRecord
	var peers []peerRecord
	for _, e := range entries {
		switch n := e.Name(); {
		case strings.HasSuffix(n, ".peers.jsonl"):
			peers = append(peers, readPeerRecords(repo, strings.TrimSuffix(n, ".peers.jsonl"))...)
		case strings.HasSuffix(n, ".jsonl"):
			stages = append(stages, readStageRecords(repo, strings.TrimSuffix(n, ".jsonl"))...)
		}
	}
	return costFromRecords(stages, peers)
}

// costFromRecords aggregates telemetry into a runCost: every stage's cost/tokens sum into the total,
// and a stage that carries cost is attributed to the task(s) it finished (split evenly on the rare
// multi-finish; cost with no finished task lands in the total only). In-turn consult/delegate peers
// add their tokens to the matching model (and the total) — tokens only, since a peer's stream carries
// no cost. Pure — unit-tested.
func costFromRecords(recs []stageRecord, peers []peerRecord) runCost {
	rc := runCost{byTask: map[string]stageCost{}}
	models := map[string]stageCost{}
	for _, r := range recs {
		rc.total.usd += r.CostUSD
		rc.total.inTok += r.InTok
		rc.total.outTok += r.OutTok
		if r.CostUSD == 0 && r.InTok == 0 && r.OutTok == 0 {
			continue
		}
		k := modelKey(r.Provider, r.Model)
		mc := models[k]
		mc.usd += r.CostUSD
		mc.inTok += r.InTok
		mc.outTok += r.OutTok
		models[k] = mc
		n := len(r.Finished)
		if n == 0 {
			continue // costed but finished no task (e.g. interrupted) — in the total + by-model only
		}
		for _, id := range r.Finished {
			c := rc.byTask[id]
			c.usd += r.CostUSD / float64(n)
			c.inTok += r.InTok / n
			c.outTok += r.OutTok / n
			rc.byTask[id] = c
		}
	}
	// In-turn peers add their tokens to the matching model and the run total — tokens only, since a
	// peer's stream carries no cost (codex). A peer-only model shows in the by-model line with cost "—".
	for _, p := range peers {
		k := modelKey(p.Provider, p.Model)
		mc := models[k]
		mc.inTok += p.In
		mc.outTok += p.Out
		models[k] = mc
		rc.total.inTok += p.In
		rc.total.outTok += p.Out
	}
	rc.byModel = sortedSpend(models)
	return rc
}

// modelKey is the by-model bucket key: "provider:model", or just "provider" when the model is blank.
func modelKey(provider, model string) string {
	if model == "" {
		return provider
	}
	return provider + ":" + model
}

// sortedSpend flattens a model→cost map into a slice sorted by cost desc (ties broken by name), so
// the by-model line is stable and leads with the priciest model.
func sortedSpend(m map[string]stageCost) []modelSpend {
	out := make([]modelSpend, 0, len(m))
	for k, c := range m {
		out = append(out, modelSpend{k, c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].cost.usd != out[j].cost.usd {
			return out[i].cost.usd > out[j].cost.usd
		}
		return out[i].model < out[j].model
	})
	return out
}
