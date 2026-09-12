package taskmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// Text bounds a box can hand a tool, matching the fork-proposal limits the host already enforces.
const (
	lineLimit  = 4096
	blockLimit = 64 << 10
	maxItems   = 64
)

type tool struct {
	name, description string
	schema            map[string]any
	run               func(s *Server, ctx context.Context, args json.RawMessage) *toolResult
}

// toolTable is THE tool set, in the order tools/list presents it. Adding a tool here is the whole
// change; doctor asserts the box sees exactly these names and nothing shell-, exec-, or file-shaped.
var toolTable = []tool{
	{
		name:        "tasks_list",
		description: "List the task queue(s) by state, as `coop tasks ls` shows them: id, title, state, subtask counts, queue root, and which task is assigned to this run.",
		schema: object(map[string]any{
			"state": prop("string", "Only tasks in this state: todo, in_progress, blocked, or done. Omit for every lifecycle state."),
		}),
		run: (*Server).list,
	},
	{
		name:        "tasks_get",
		description: "Read one task: its task.md, state.md, log.md, and decision.md (when present), plus its folder path for tmp/ and artifacts/.",
		schema:      object(map[string]any{"id": prop("string", "The task id (its folder name).")}, "id"),
		run:         (*Server).get,
	},
	{
		name:        "tasks_update_state",
		description: "Overwrite the task's state.md resume snapshot with the four lifecycle fields. Refresh it before each commit and before pausing.",
		schema: object(map[string]any{
			"id":          prop("string", "The task id."),
			"status":      prop("string", "One line: where the task stands (e.g. in progress — step 3 of 5)."),
			"done_so_far": prop("string", "What is finished and verified; may span lines."),
			"next_action": prop("string", "One line: the very next concrete step, or none."),
			"traps":       prop("string", "Gotchas the next agent must know, or —; may span lines."),
		}, "id", "status", "done_so_far", "next_action", "traps"),
		run: (*Server).updateState,
	},
	{
		name:        "tasks_append_log",
		description: "Append an entry to the task's log.md journal: what you did and why (decisions, dead ends, surprises). Entries are appended, never rewritten.",
		schema: object(map[string]any{
			"id":    prop("string", "The task id."),
			"entry": prop("string", "The entry, as markdown; may span lines (a `## <date> — <what>` heading plus bullets is the house style)."),
		}, "id", "entry"),
		run: (*Server).appendLog,
	},
	{
		name:        "tasks_complete",
		description: "Move the task into 99_done/ — the final action after its commit landed — normalizing state.md's Status to complete and Next action to none. The loop checks the assigned commit before moving: fix any refusal and retry this tool in the same turn. Refused when another live process holds the task.",
		schema:      object(map[string]any{"id": prop("string", "The task id.")}, "id"),
		run:         (*Server).complete,
	},
	{
		name:        "tasks_block",
		description: "Park the task in 50_blocked/ on a one-way-door decision a human must make, writing its decision.md (question, options, your recommendation). Refused when another live process holds the task.",
		schema: object(map[string]any{
			"id":             prop("string", "The task id."),
			"decision":       prop("string", "What must be chosen, and why it cannot be undone cheaply."),
			"options":        array("string", "Each option as one line: `A — <name>: <consequence>`."),
			"recommendation": prop("string", "Your pick and one line why."),
		}, "id", "decision", "options", "recommendation"),
		run: (*Server).block,
	},
	{
		name:        "tasks_set_subtasks",
		description: "Rewrite the task's whole `## Subtasks` checklist — add, refine, reorder, or check items off. Send the complete list every time; it replaces the section.",
		schema: object(map[string]any{
			"id": prop("string", "The task id."),
			"subtasks": map[string]any{
				"type":        "array",
				"description": "The complete checklist, in order.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": prop("string", "The subtask, one line."),
						"done": prop("boolean", "Whether it is checked off."),
					},
					"required":             []string{"text"},
					"additionalProperties": false,
				},
			},
		}, "id", "subtasks"),
		run: (*Server).setSubtasks,
	},
	{
		name:        "tasks_propose",
		description: "File separate work you spotted, without folding it into this task: a ready task goes to 00_todo/ (kind task); only the genuinely large — work no single iteration could finish, or an idea a human must scope — goes to xx_backlog/ (kind backlog). When the call is close, file a task.",
		schema: object(map[string]any{
			"kind":       prop("string", "task (ready work) or backlog (genuinely large, needs human scoping)."),
			"title":      prop("string", "One line."),
			"context":    prop("string", "The problem, why it matters, and where in the code it lives."),
			"acceptance": prop("string", "What proves it is done, including a green gate."),
			"approach":   prop("string", "The boring plan."),
			"subtasks":   array("string", "Small, end-to-end, testable steps (1 to 64)."),
			"queue":      prop("string", "Optional queue root (as tasks_list reports it) in a monorepo; defaults to the assigned task's queue."),
		}, "kind", "title", "context", "acceptance", "approach", "subtasks"),
		run: (*Server).propose,
	},
}

func prop(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func array(kind, description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": map[string]any{"type": kind}}
}

func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// ToolNames lists the tool set in presentation order — the fixed contract doctor asserts.
func ToolNames() []string {
	names := make([]string, len(toolTable))
	for i, t := range toolTable {
		names[i] = t.name
	}
	return names
}

func toolNames() string { return strings.Join(ToolNames(), ", ") }

func toolDescriptors() []map[string]any {
	out := make([]map[string]any, 0, len(toolTable))
	for _, t := range toolTable {
		out = append(out, map[string]any{"name": t.name, "description": t.description, "inputSchema": t.schema})
	}
	return out
}

func toolByName(name string) (tool, bool) {
	for _, t := range toolTable {
		if t.name == name {
			return t, true
		}
	}
	return tool{}, false
}

// decodeArgs decodes a tool's arguments strictly: an unknown field is a refusal, not a silent
// drop, so a misspelled argument never becomes a mutation with a missing value.
func decodeArgs(raw json.RawMessage, v any) *toolResult {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refusal("invalid arguments: " + err.Error())
	}
	return nil
}

// located is a task and the queue root it was found under.
type located struct {
	root string
	item tasks.Item
}

// locate finds a task by exact id across the queue roots. A slug fragment is not enough here: a
// machine caller must never mutate a task it only partially named.
func (s *Server) locate(id string) (located, *toolResult) {
	if id == "" {
		return located{}, refusal("id is required")
	}
	var hits []located
	for _, root := range s.authority.QueueRoots {
		items, err := tasks.ReadTaskTree(root)
		if err != nil {
			return located{}, refusal(fmt.Sprintf("read queue %s: %v", root, err))
		}
		for _, item := range items {
			if item.ID == id {
				hits = append(hits, located{root: root, item: item})
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return located{}, refusal(fmt.Sprintf("no task with id %q in the queue — tasks_list shows every id", id))
	default:
		return located{}, refusal(fmt.Sprintf("task id %q exists in %d queues — the queues must be repaired before it can be touched", id, len(hits)))
	}
}

// underLease runs mutate on a task that is not the assigned one: it leases the task for the call
// and releases it after. A task another live process holds is refused before anything is touched;
// the assigned task runs under the launching iteration's own lease and never leases twice.
func (s *Server) underLease(loc located, verb string, mutate func() error) *toolResult {
	if loc.item.ID == s.authority.Assigned {
		if err := mutate(); err != nil {
			return refusal(fmt.Sprintf("%s %s: %v", verb, loc.item.ID, err))
		}
		return nil
	}
	lease, observed, err := tasks.TryTaskLease(loc.root, loc.item, s.authority.Owner)
	if err != nil {
		return refusal(fmt.Sprintf("%s %s: %v", verb, loc.item.ID, err))
	}
	if lease == nil {
		return heldRefusal(loc.item.ID, verb, observed.String())
	}
	err = mutate()
	if releaseErr := lease.Release(); releaseErr != nil {
		err = errors.Join(err, releaseErr)
	}
	if err != nil {
		return refusal(fmt.Sprintf("%s %s: %v", verb, loc.item.ID, err))
	}
	return nil
}

func heldRefusal(id, verb, holder string) *toolResult {
	return refusal(fmt.Sprintf("task %s is held by another live process (%s) — %s refused; leave that task alone and keep to your own", id, holder, verb))
}

type listedTask struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	State    string `json:"state"`
	Queue    string `json:"queue"`
	Subtasks struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	} `json:"subtasks"`
	Assigned    bool `json:"assigned,omitempty"`
	HasDecision bool `json:"has_decision,omitempty"`
}

func listed(root string, item tasks.Item, assigned string) listedTask {
	out := listedTask{ID: item.ID, Title: item.Title, State: tasks.StateLabel(item.State), Queue: root,
		Assigned: item.ID == assigned, HasDecision: item.HasDecision}
	out.Subtasks.Total = len(item.Subtasks)
	for _, done := range item.Subtasks {
		if done {
			out.Subtasks.Done++
		}
	}
	return out
}

func (s *Server) list(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		State string `json:"state"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if in.State != "" {
		known := false
		for _, st := range tasks.TaskStates {
			known = known || tasks.StateLabel(st) == in.State
		}
		if !known {
			return refusal(fmt.Sprintf("unknown state %q — one of todo, in_progress, blocked, done", in.State))
		}
	}
	out := []listedTask{}
	for _, root := range s.authority.QueueRoots {
		items, err := tasks.ReadTaskTree(root)
		if err != nil {
			return refusal(fmt.Sprintf("read queue %s: %v", root, err))
		}
		for _, item := range items {
			if in.State != "" && tasks.StateLabel(item.State) != in.State {
				continue
			}
			out = append(out, listed(root, item, s.authority.Assigned))
		}
	}
	return jsonResult(map[string]any{"tasks": out})
}

func (s *Server) get(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID string `json:"id"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	files, err := tasks.ReadTaskFiles(loc.item.Dir)
	if err != nil {
		return refusal(fmt.Sprintf("read task %s: %v", loc.item.ID, err))
	}
	summary := listed(loc.root, loc.item, s.authority.Assigned)
	return jsonResult(map[string]any{
		"id": summary.ID, "title": summary.Title, "state": summary.State, "queue": summary.Queue,
		"dir": loc.item.Dir, "subtasks": summary.Subtasks, "assigned": summary.Assigned, "files": files,
	})
}

func (s *Server) updateState(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		DoneSoFar  string `json:"done_so_far"`
		NextAction string `json:"next_action"`
		Traps      string `json:"traps"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	for _, f := range []struct {
		name, value string
		multiline   bool
	}{{"status", in.Status, false}, {"done_so_far", in.DoneSoFar, true}, {"next_action", in.NextAction, false}, {"traps", in.Traps, true}} {
		if r := checkText(f.name, f.value, f.multiline, blockLimit); r != nil {
			return r
		}
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if r := s.underLease(loc, "update state of", func() error {
		return tasks.WriteTaskState(loc.item.Dir, loc.item.Title, tasks.TaskStateFields{
			Status: in.Status, DoneSoFar: in.DoneSoFar, NextAction: in.NextAction, Traps: in.Traps,
		})
	}); r != nil {
		return r
	}
	return textResult(fmt.Sprintf("state.md of %s rewritten", loc.item.ID))
}

func (s *Server) appendLog(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID    string `json:"id"`
		Entry string `json:"entry"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if r := checkText("entry", in.Entry, true, blockLimit); r != nil {
		return r
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if r := s.underLease(loc, "append to log of", func() error {
		return tasks.AppendTaskLogEntry(loc.item.Dir, in.Entry)
	}); r != nil {
		return r
	}
	return textResult(fmt.Sprintf("entry appended to log.md of %s", loc.item.ID))
}

func (s *Server) complete(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID string `json:"id"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if loc.item.State == tasks.StateDone {
		return textResult(fmt.Sprintf("%s is already done", loc.item.ID))
	}
	if loc.item.ID != s.authority.Assigned {
		// Another task: the host's own trusted completion — its lease, receipt, and normalization —
		// so the loop's completion audit sees a controller-owned move, not an unowned one.
		if err := tasks.CompleteTrustedTask(loc.root, loc.item); err != nil {
			if errors.Is(err, tasks.ErrTaskLeased) {
				return heldRefusal(loc.item.ID, "complete", "leased")
			}
			return refusal(fmt.Sprintf("complete %s: %v", loc.item.ID, err))
		}
		return textResult(fmt.Sprintf("%s moved to %s/", loc.item.ID, tasks.StateDone))
	}
	// The assigned task moves under the launching iteration's lease; the host finalizes it after
	// the box exits (commit binding, receipt, tmp/ removal). Normalizing here keeps state.md's
	// lifecycle fields truthful the moment the folder lands in done.
	if check := s.authority.ValidateAssignedCompletion; check != nil {
		if err := check(); err != nil {
			return refusal(fmt.Sprintf("complete %s: %v; the task has not moved — repair the problem and retry tasks_complete in this turn", loc.item.ID, err))
		}
	}
	if err := tasks.MoveTaskDir(loc.root, loc.item, tasks.StateDone); err != nil {
		return refusal(fmt.Sprintf("complete %s: %v", loc.item.ID, err))
	}
	dir := filepath.Join(loc.root, tasks.StateDone, loc.item.ID)
	if err := tasks.NormalizeTaskState(loc.item.ID, dir, "complete", "none", "—", "—"); err != nil {
		return refusal(fmt.Sprintf("%s moved to %s/ but its state.md could not be normalized: %v", loc.item.ID, tasks.StateDone, err))
	}
	return textResult(fmt.Sprintf("%s moved to %s/ — write nothing more inside that folder", loc.item.ID, tasks.StateDone))
}

func (s *Server) block(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID             string   `json:"id"`
		Decision       string   `json:"decision"`
		Options        []string `json:"options"`
		Recommendation string   `json:"recommendation"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if r := checkText("decision", in.Decision, true, blockLimit); r != nil {
		return r
	}
	if r := checkText("recommendation", in.Recommendation, true, blockLimit); r != nil {
		return r
	}
	if len(in.Options) == 0 || len(in.Options) > maxItems {
		return refusal(fmt.Sprintf("options needs 1 to %d entries", maxItems))
	}
	for _, option := range in.Options {
		if r := checkText("options", option, false, lineLimit); r != nil {
			return r
		}
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if loc.item.State == tasks.StateDone {
		return refusal(fmt.Sprintf("%s is already done — a shipped task is not blocked from the box; a human reopens it with `coop tasks block`", loc.item.ID))
	}
	decision := tasks.Decision{Question: in.Decision, Options: in.Options, Recommendation: in.Recommendation}
	if loc.item.ID != s.authority.Assigned {
		if err := tasks.BlockTrustedTask(loc.root, loc.item, tasks.ClaimActor{}); err != nil {
			if errors.Is(err, tasks.ErrTaskLeased) {
				return heldRefusal(loc.item.ID, "block", "leased")
			}
			return refusal(fmt.Sprintf("block %s: %v", loc.item.ID, err))
		}
	} else if loc.item.State != tasks.StateBlocked {
		if err := tasks.MoveTaskDir(loc.root, loc.item, tasks.StateBlocked); err != nil {
			return refusal(fmt.Sprintf("block %s: %v", loc.item.ID, err))
		}
	}
	dir := filepath.Join(loc.root, tasks.StateBlocked, loc.item.ID)
	if err := tasks.WriteDecision(dir, loc.item.ID, loc.item.Title, decision); err != nil {
		return refusal(fmt.Sprintf("%s moved to %s/ but its decision.md could not be written: %v", loc.item.ID, tasks.StateBlocked, err))
	}
	return textResult(fmt.Sprintf("%s moved to %s/ with its decision.md — a human resolves it; stop working it", loc.item.ID, tasks.StateBlocked))
}

func (s *Server) setSubtasks(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID       string `json:"id"`
		Subtasks []struct {
			Text string `json:"text"`
			Done bool   `json:"done"`
		} `json:"subtasks"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if len(in.Subtasks) == 0 || len(in.Subtasks) > maxItems {
		return refusal(fmt.Sprintf("subtasks needs 1 to %d entries — send the complete list", maxItems))
	}
	items := make([]tasks.Subtask, 0, len(in.Subtasks))
	for _, st := range in.Subtasks {
		if r := checkText("subtasks[].text", st.Text, false, lineLimit); r != nil {
			return r
		}
		items = append(items, tasks.Subtask{Text: st.Text, Done: st.Done})
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if r := s.underLease(loc, "rewrite subtasks of", func() error {
		return tasks.RewriteSubtasks(loc.item.Dir, items)
	}); r != nil {
		return r
	}
	done := 0
	for _, st := range items {
		if st.Done {
			done++
		}
	}
	return textResult(fmt.Sprintf("subtasks of %s rewritten: %d/%d done", loc.item.ID, done, len(items)))
}

func (s *Server) propose(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		Kind       string   `json:"kind"`
		Title      string   `json:"title"`
		Context    string   `json:"context"`
		Acceptance string   `json:"acceptance"`
		Approach   string   `json:"approach"`
		Subtasks   []string `json:"subtasks"`
		Queue      string   `json:"queue"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	kind := tasks.ForkProposalKind(in.Kind)
	if kind != tasks.ForkProposalTask && kind != tasks.ForkProposalBacklog {
		return refusal(`kind must be "task" or "backlog"`)
	}
	draft := tasks.TaskDraft{Kind: kind, Title: in.Title, Context: in.Context, Acceptance: in.Acceptance, Approach: in.Approach, Subtasks: in.Subtasks}
	if s.authority.ProposalOutbox != "" {
		// A fork's projection has no canonical queue: the host imports the outbox when it lands.
		path, err := tasks.WriteForkProposal(s.authority.ProposalOutbox, draft)
		if err != nil {
			return refusal("propose: " + err.Error())
		}
		return textResult(fmt.Sprintf("proposal filed as %s — the host imports it into the canonical queue when this fork lands", filepath.Base(path)))
	}
	root, r := s.proposalRoot(in.Queue)
	if r != nil {
		return r
	}
	item, err := tasks.CreateDraftTask(root, draft)
	if err != nil {
		return refusal("propose: " + err.Error())
	}
	where := tasks.StateTodo
	if kind == tasks.ForkProposalBacklog {
		where = tasks.StateBacklog
	}
	return textResult(fmt.Sprintf("filed %s under %s/ of %s — a later iteration works it; stay on your own task", item.ID, where, root))
}

// proposalRoot picks the queue a proposal lands in: the one named, else the assigned task's, else
// the first configured root.
func (s *Server) proposalRoot(queue string) (string, *toolResult) {
	roots := s.authority.QueueRoots
	if queue != "" {
		if !slices.Contains(roots, queue) {
			return "", refusal(fmt.Sprintf("unknown queue %q — one of: %s", queue, strings.Join(roots, ", ")))
		}
		return queue, nil
	}
	if s.authority.Assigned != "" {
		if loc, r := s.locate(s.authority.Assigned); r == nil {
			return loc.root, nil
		}
	}
	return roots[0], nil
}

// checkText refuses text a task file must never carry: empty, over its limit, invalid UTF-8, or
// control characters (newlines and tabs allowed only where a block is expected).
func checkText(field, value string, multiline bool, limit int) *toolResult {
	if tasks.ValidTaskText(value, multiline, limit) {
		return nil
	}
	shape := "one line"
	if multiline {
		shape = "text"
	}
	return refusal(fmt.Sprintf("%s must be non-empty %s of at most %d bytes with no control characters", field, shape, limit))
}
