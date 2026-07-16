package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	Run        string   `json:"run"`
	Stage      string   `json:"stage"`    // preflight | work | between | signoff | verify
	Outcome    string   `json:"outcome"`  // success | authentication | rate_limit | output_limit | process_failure | malformed_stream | interrupted
	Provider   string   `json:"provider"` // the EFFECTIVE target, after any rate-limit rotation
	Model      string   `json:"model,omitempty"`
	Effort     string   `json:"effort,omitempty"`
	Account    string   `json:"account,omitempty"`
	Coop       string   `json:"coop"`
	Start      string   `json:"start"`
	End        string   `json:"end"`
	Exit       int      `json:"exit"`
	Retries    int      `json:"retries,omitempty"`
	CostUSD    float64  `json:"cost_usd,omitempty"` // the stage's result-event cost (lead + its native subagents)
	InTok      int      `json:"in_tok,omitempty"`   // input tokens (fresh + cache write + cache read)
	OutTok     int      `json:"out_tok,omitempty"`  // output tokens
	HeadBefore string   `json:"head_before,omitempty"`
	HeadAfter  string   `json:"head_after,omitempty"`
	Reopened   int      `json:"reopened,omitempty"`   // review stages: task folders moved back to in_progress
	Finished   []string `json:"finished,omitempty"`   // work stage: task ids this iteration moved to done
	GateFiles  []string `json:"gate_files,omitempty"` // host-detected gate-defining paths touched by the stage
	QueueTodo  int      `json:"queue_todo"`
	QueueDoing int      `json:"queue_doing"`
	QueueDone  int      `json:"queue_done"`
}

// buildStageRecord assembles a record from a stage's EFFECTIVE target (the post-rotation Target, so
// the row shows what ran, not what was configured) and its outcome. Pure — unit-tested.
func buildStageRecord(run, stage, outcome, coopVer string, tgt agents.Target, start, end time.Time, exit, retries, reopened int, headBefore, headAfter string, q taskCounts, finished, gateFiles []string) stageRecord {
	return stageRecord{
		Run:        run,
		Stage:      stage,
		Outcome:    outcome,
		Provider:   tgt.Provider,
		Model:      tgt.Model,
		Effort:     tgt.Effort,
		Account:    tgt.Account(),
		Coop:       coopVer,
		Start:      start.UTC().Format(time.RFC3339),
		End:        end.UTC().Format(time.RFC3339),
		Exit:       exit,
		Retries:    retries,
		HeadBefore: headBefore,
		HeadAfter:  headAfter,
		Reopened:   reopened,
		Finished:   finished,
		GateFiles:  gateFiles,
		QueueTodo:  q.Todo,
		QueueDoing: q.Doing,
		QueueDone:  q.Done,
	}
}

// appendStageRecord appends one record to .agent/runs/<run>.jsonl under repo. Best-effort: the
// error is returned only so the caller can warn once — it must never fail a loop iteration.
func appendStageRecord(repo, run string, rec stageRecord) error {
	if !safeRunID(run) {
		return fmt.Errorf("invalid run id")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	runsRoot, err := openRunsRoot(repo, true)
	if err != nil {
		return err
	}
	defer runsRoot.Close()
	f, err := runsRoot.OpenFile(run+".jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("stage telemetry target is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fmt.Errorf("stage telemetry target must have exactly one hardlink")
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func safeRunID(run string) bool {
	return run != "" && !strings.ContainsAny(run, "/\\\x00")
}

// openRunsRoot opens the host-owned telemetry directory without following repository-provided
// links. Every host writer uses this boundary before publishing run state.
func openRunsRoot(repo string, create bool) (*os.Root, error) {
	repoRoot, err := os.OpenRoot(repo)
	if err != nil {
		return nil, err
	}
	defer repoRoot.Close()
	agentInfo, err := repoRoot.Lstat(".agent")
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, err
		}
		if err := repoRoot.MkdirAll(".agent", 0o755); err != nil {
			return nil, err
		}
		agentInfo, err = repoRoot.Lstat(".agent")
	}
	if err != nil || !agentInfo.IsDir() || agentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf(".agent is not a regular directory")
	}
	agentRoot, err := repoRoot.OpenRoot(".agent")
	if err != nil {
		return nil, err
	}
	defer agentRoot.Close()
	openedAgent, err := agentRoot.Stat(".")
	if err != nil || !os.SameFile(agentInfo, openedAgent) {
		return nil, fmt.Errorf(".agent changed while opening")
	}
	runsInfo, err := agentRoot.Lstat("runs")
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, err
		}
		if err := agentRoot.MkdirAll("runs", 0o755); err != nil {
			return nil, err
		}
		runsInfo, err = agentRoot.Lstat("runs")
	}
	if err != nil || !runsInfo.IsDir() || runsInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf(".agent/runs is not a regular directory")
	}
	runsRoot, err := agentRoot.OpenRoot("runs")
	if err != nil {
		return nil, err
	}
	openedRuns, err := runsRoot.Stat(".")
	if err != nil || !os.SameFile(runsInfo, openedRuns) {
		runsRoot.Close()
		return nil, fmt.Errorf(".agent/runs changed while opening")
	}
	return runsRoot, nil
}

// recordStage builds and appends a stage record, stamping end-time, HEAD-after, and the queue
// counts at emit time. Best-effort — a write failure is warned once and swallowed, so telemetry can
// never break the run it observes.
func (a *app) recordStage(repo, run, stage, outcome string, tgt agents.Target, start time.Time, exit, retries, reopened int, headBefore string, hosts, finished, gateFiles []string, res *iterResult) {
	cnt, _ := queueProgress(hosts)
	rec := buildStageRecord(run, stage, outcome, resolveVersion(), tgt, start, time.Now(), exit, retries, reopened, headBefore, gitOut(repo, "rev-parse", "HEAD"), cnt, finished, gateFiles)
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
	data, err := readRunFile(repo, run+".jsonl")
	if err != nil {
		return nil
	}
	var recs []stageRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, maxRunTelemetryLineBytes)
	for len(recs) < maxRunTelemetryRows && scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r stageRecord
		if json.Unmarshal(line, &r) == nil {
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

// preparePeerRecordFile creates the one append target a consult wrapper may use for this run.
// The host owns publication: the wrapper never creates or follows a repository-provided path.
func preparePeerRecordFile(repo, run string) (path string, err error) {
	if !safeRunID(run) {
		return "", fmt.Errorf("invalid run id")
	}
	runsRoot, err := openRunsRoot(repo, true)
	if err != nil {
		return "", err
	}
	defer runsRoot.Close()
	name := run + ".peers.jsonl"
	f, err := runsRoot.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	remove := true
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if remove {
			_ = runsRoot.Remove(name)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return "", err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("peer usage target is not a private regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return "", fmt.Errorf("peer usage target must have exactly one hardlink")
	}
	remove = false
	return filepath.Join(repo, ".agent", "runs", name), nil
}

func removeEmptyPeerRecordFile(path string) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 0 {
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok && stat.Nlink == 1 {
		_ = os.Remove(path)
	}
}

// readPeerRecords reads a run's peer-usage rows from .agent/runs/<run>.peers.jsonl. Best-effort, same
// contract as readStageRecords: a missing/unreadable file or a bad line yields what parsed.
func readPeerRecords(repo, run string) []peerRecord {
	data, err := readRunFile(repo, run+".peers.jsonl")
	if err != nil {
		return nil
	}
	var recs []peerRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, maxRunTelemetryLineBytes)
	for len(recs) < maxRunTelemetryRows && scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r peerRecord
		if json.Unmarshal(line, &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs
}

// costForRepo aggregates EVERY run's telemetry under repo (.agent/runs/*.jsonl + the matching
// *.peers.jsonl) into one runCost — a fork/clone's total spend across all its loop runs, for the
// fleet board and fork review/merge. Best-effort: no runs dir → a zero runCost.
func costForRepo(repo string) runCost {
	runsRoot, err := openRunsRoot(repo, false)
	if err != nil {
		return runCost{byTask: map[string]stageCost{}}
	}
	defer runsRoot.Close()
	dir, err := runsRoot.Open(".")
	if err != nil {
		return runCost{byTask: map[string]stageCost{}}
	}
	entries, err := dir.ReadDir(maxRunTelemetryFiles + 1)
	_ = dir.Close()
	if err != nil || len(entries) > maxRunTelemetryFiles {
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

const (
	maxRunTelemetryBytes     = 8 << 20
	maxRunTelemetryLineBytes = 1 << 20
	maxRunTelemetryRows      = 4096
	maxRunTelemetryFiles     = 4096
)

func readRunFile(repo, name string) ([]byte, error) {
	if !strings.HasSuffix(name, ".jsonl") || strings.ContainsAny(name, "/\\\x00") {
		return nil, fmt.Errorf("invalid run telemetry name")
	}
	runsRoot, err := openRunsRoot(repo, false)
	if err != nil {
		return nil, err
	}
	defer runsRoot.Close()
	f, err := runsRoot.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxRunTelemetryBytes {
		return nil, fmt.Errorf("run telemetry target is not a bounded regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return nil, fmt.Errorf("run telemetry target must have exactly one hardlink")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRunTelemetryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read bounded run telemetry: %w", err)
	}
	if len(data) > maxRunTelemetryBytes {
		return nil, fmt.Errorf("run telemetry exceeded the size limit")
	}
	return data, nil
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
