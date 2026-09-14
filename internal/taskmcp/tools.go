package taskmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// Text bounds a box can hand a tool, matching the fork-proposal limits the host already enforces.
const (
	lineLimit         = tasks.TaskLineLimit
	blockLimit        = tasks.TaskBlockLimit
	maxItems          = tasks.TaskListLimit
	searchQueryLimit  = 256
	searchResultLimit = 20
	searchByteLimit   = 64 << 10
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
		description: "List task summaries by state. To find related work without reading the whole archive, use query for a literal ID/title search. Searches return at most 20 complete summaries within 64 KiB, with total match and returned counts; narrow the query when truncated. Omit query to list all tasks as before.",
		schema: withExample(object(map[string]any{
			"state": enumProp("Only tasks in this state. Omit or use an empty string for every lifecycle state.", "", "todo", "in_progress", "blocked", "done"),
			"query": textProp("Optional case-insensitive literal substring of task ID or title, not a regex. Surrounding spaces are trimmed; omit rather than sending an empty query.", false, searchQueryLimit),
		}), map[string]any{"query": "retry", "state": "todo"}),
		run: (*Server).list,
	},
	{
		name:        "tasks_get",
		description: "Read one task: its task.md, state.md, log.md, and decision.md (when present), plus its folder path for tmp/ and artifacts/.",
		schema:      object(map[string]any{"id": idProp()}, "id"),
		run:         (*Server).get,
	},
	{
		name:        "tasks_update_state",
		description: "Patch state.md: send id and one or more fields to change. Omitted fields keep their saved values. Refresh before each commit and before pausing.",
		schema: withExample(withMinProperties(object(map[string]any{
			"id":          idProp(),
			"status":      textProp("Where the task stands, e.g. in progress — tests next.", false, blockLimit),
			"done_so_far": textProp("What is finished and verified. Use the literal string \"—\" if nothing is finished.", true, blockLimit),
			"next_action": textProp("The next concrete step. If changing it to no next action, send the literal string \"none\"; never send null/empty.", false, blockLimit),
			"traps":       textProp("Gotchas for the next agent. If changing it when there are none, send the literal string \"—\"; never send null/empty.", true, blockLimit),
		}, "id"), 2), map[string]any{
			"id": "<exact task id>", "next_action": "Run the focused regression",
		}),
		run: (*Server).updateState,
	},
	{
		name:        "tasks_append_log",
		description: "Append an entry to the task's log.md journal: what you did and why (decisions, dead ends, surprises). Entries are appended, never rewritten.",
		schema: object(map[string]any{
			"id":    idProp(),
			"entry": textProp("The entry as markdown: a `## <date> — <what>` heading plus bullets.", true, blockLimit),
		}, "id", "entry"),
		run: (*Server).appendLog,
	},
	{
		name:        "tasks_complete",
		description: "Move the task into 99_done/ — the final action after its commit landed and required verification passed — normalizing state.md's Status to complete and Next action to none. Before calling, promote any promised logs from tmp/ to artifacts/ and verify their durable paths, not links back into tmp/. Requires a nonempty, fully checked checklist. Failed, unavailable, and never-attempted required checks stay open. The loop checks the assigned commit before moving: fix any refusal and retry this tool in the same turn. Refused when another live process holds the task.",
		schema:      object(map[string]any{"id": idProp()}, "id"),
		run:         (*Server).complete,
	},
	{
		name:        "tasks_block",
		description: "Park the task in 50_blocked/ on a one-way-door decision a human must make, writing its decision.md (question, options, your recommendation). Refused when another live process holds the task.",
		schema: object(map[string]any{
			"id":             idProp(),
			"decision":       textProp("What must be chosen, and why it cannot be undone cheaply.", true, blockLimit),
			"options":        array(textProp("One option: A — <name>: <consequence>.", false, lineLimit), "1 to 64 options."),
			"recommendation": textProp("Your pick and why.", true, blockLimit),
		}, "id", "decision", "options", "recommendation"),
		run: (*Server).block,
	},
	{
		name:        "tasks_set_subtasks",
		description: "Rewrite the task's whole `## Subtasks` checklist — add, refine, reorder, or check items off. Send the complete list every time; it replaces the section. Check off only finished work: recording a required check as pending does not complete it. Do not drop required verification to allow completion.",
		schema: object(map[string]any{
			"id": idProp(),
			"subtasks": map[string]any{
				"type":        "array",
				"description": "The complete checklist, in order (1 to 64 items).",
				"minItems":    1,
				"maxItems":    maxItems,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": textProp("The subtask.", false, lineLimit),
						"done": prop("boolean", "Whether it is checked off; omitted means false."),
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
		description: fmt.Sprintf("File separate work you spotted, without folding it into this task: ready work is kind task; only genuinely large work needing human scoping is kind backlog. When the call is close, file a task. Fork proposals must also fit %d bytes of serialized JSON, including escaping and metadata.", tasks.TaskProposalFileLimit),
		schema: withExample(object(map[string]any{
			"kind":       enumProp("task (ready work) or backlog (genuinely large, needs human scoping).", "task", "backlog"),
			"title":      textProp("Title containing a letter or digit for its task id.", false, tasks.TaskTitleLimit),
			"context":    textProp("Problem, source location and impact; for observed failures, distinguish suspected cause from proven evidence and state remaining investigation. A passing retry does not prove the cause.", true, blockLimit),
			"acceptance": textProp("What proves it is done, preserving the failing behavior and denial assertions unless the human authorizes a contract change, including required green gates. Recording required verification as pending is not an alternative to passing it.", true, blockLimit),
			"approach":   textProp("The boring plan.", true, blockLimit),
			"subtasks":   array(textProp("One small, end-to-end, testable step.", false, lineLimit), "1 to 64 steps."),
			"queue":      prop("string", "Plain loops: optional queue root from tasks_list; defaults to the assigned task's queue. Forks always use the host-bound canonical queue; this field cannot redirect it."),
		}, "kind", "title", "context", "acceptance", "approach", "subtasks"), map[string]any{
			"kind": "task", "title": "Investigate the crash-test timeout", "context": "test/application_test.exs timed out under full load; three isolated reruns passed; cause unknown.",
			"acceptance": "Preserve the abnormal-crash reason and supervisor-survival assertions. A missing or wrong crash reason still fails; required gates pass.",
			"approach":   "Capture evidence under load before choosing a fix; record any remaining uncertainty.", "subtasks": []string{"Investigate the timeout, preserve the contract and verify success and denial paths"},
		}),
		run: (*Server).propose,
	},
}

func prop(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func idProp() map[string]any {
	p := prop("string", "The exact non-empty task id (its folder name), as tasks_list reports it.")
	p["minLength"] = 1
	return p
}

func textProp(description string, multiline bool, limit int) map[string]any {
	p := prop("string", description+" Must be "+tasks.TaskTextRequirement(multiline, limit)+".")
	p["minLength"] = 1
	return p
}

func enumProp(description string, values ...string) map[string]any {
	p := prop("string", description)
	p["enum"] = values
	return p
}

func array(items map[string]any, description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items, "minItems": 1, "maxItems": maxItems}
}

func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func withExample(schema, example map[string]any) map[string]any {
	schema["examples"] = []map[string]any{example}
	return schema
}

func withMinProperties(schema map[string]any, count int) map[string]any {
	schema["minProperties"] = count
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

// Use the advertised order, not a second required-field registry. Present but invalid values
// still reach the strict decoder and field validators; this only makes omissions cheap to repair.
func missingRequiredArguments(t tool, raw json.RawMessage) *toolResult {
	if !utf8.Valid(raw) {
		return nil // Let the strict decoder reject malformed bytes without normalizing them.
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil
	}
	required, _ := t.schema["required"].([]string)
	var missing []string
	for _, name := range required {
		if _, present := fields[name]; !present {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	message := "missing required fields: " + strings.Join(missing, ", ") + "; resend the complete arguments object."
	properties, _ := t.schema["properties"].(map[string]any)
	for _, name := range missing {
		property, _ := properties[name].(map[string]any)
		if description, _ := property["description"].(string); description != "" {
			message += "\n" + name + ": " + description
		}
	}
	return refusal(message)
}

// decodeArgs decodes a tool's arguments strictly: an unknown field is a refusal, not a silent
// drop, so a misspelled argument never becomes a mutation with a missing value.
func decodeArgs(raw json.RawMessage, v any) *toolResult {
	// encoding/json replaces malformed UTF-8 before the string validator can see it.
	if !utf8.Valid(raw) {
		return refusal("invalid arguments: send valid UTF-8 JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refusal("invalid arguments: " + err.Error())
	}
	return nil
}

// Omitted optional values keep their defaults; explicit null is not a boolean or string.
// encoding/json otherwise silently converts it to the zero value, allowing invalid mutations.
type nonNullString string

func (s *nonNullString) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return &json.UnmarshalTypeError{Value: "null", Type: reflect.TypeFor[string]()}
	}
	return json.Unmarshal(raw, (*string)(s))
}

type optionalString struct {
	present bool
	value   string
}

func (s *optionalString) UnmarshalJSON(raw []byte) error {
	var value nonNullString
	if err := value.UnmarshalJSON(raw); err != nil {
		return err
	}
	s.present = true
	s.value = string(value)
	return nil
}

type nonNullBool bool

func (b *nonNullBool) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return &json.UnmarshalTypeError{Value: "null", Type: reflect.TypeFor[bool]()}
	}
	return json.Unmarshal(raw, (*bool)(b))
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
		State nonNullString   `json:"state"`
		Query json.RawMessage `json:"query"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	query := ""
	if in.Query != nil {
		var value nonNullString
		if r := decodeArgs(in.Query, &value); r != nil {
			return r
		}
		if r := checkText("query", string(value), false, searchQueryLimit); r != nil {
			return r
		}
		query = strings.ToLower(strings.TrimSpace(string(value)))
	}
	if in.State != "" {
		known := false
		for _, st := range tasks.TaskStates {
			known = known || tasks.StateLabel(st) == string(in.State)
		}
		if !known {
			return refusal(fmt.Sprintf("unknown state %q — one of todo, in_progress, blocked, done", in.State))
		}
	}
	out := []listedTask{}
	matched := 0
	for _, root := range s.authority.QueueRoots {
		items, err := tasks.ReadTaskTree(root)
		if err != nil {
			return refusal(fmt.Sprintf("read queue %s: %v", root, err))
		}
		for _, item := range items {
			if in.State != "" && tasks.StateLabel(item.State) != string(in.State) {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(item.ID), query) && !strings.Contains(strings.ToLower(item.Title), query) {
				continue
			}
			matched++
			summary := listed(root, item, s.authority.Assigned)
			if query != "" {
				// Even a match beyond the returned prefix needs a useful refusal
				// if no narrower query could ever return its whole summary.
				if r := searchedTasks([]listedTask{summary}, 1); r.IsError {
					return r
				}
			}
			if query == "" || len(out) < searchResultLimit {
				out = append(out, summary)
			}
		}
	}
	if query != "" {
		return searchedTasks(out, matched)
	}
	return jsonResult(map[string]any{"tasks": out})
}

// Fit a complete deterministic prefix; count omissions even after the result cap.
// Measure the actual MCP result because escaping titles and text can expand it.
func searchedTasks(out []listedTask, matched int) *toolResult {
	for {
		result := map[string]any{"tasks": out, "matched": matched, "returned": len(out), "truncated": len(out) < matched}
		if len(out) < matched {
			result["hint"] = "Narrow query or select a state to see omitted matches."
		}
		reply := jsonResult(result)
		data, err := json.Marshal(reply)
		if err != nil {
			return refusal("encode search result: " + err.Error())
		}
		if len(data) <= searchByteLimit {
			return reply
		}
		if len(out) == 1 {
			return refusal(fmt.Sprintf("task %q summary exceeds the 64 KiB search limit; use tasks_get with that id, or narrow query to other tasks", out[0].ID))
		}
		out = out[:len(out)-1]
	}
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
		ID         string         `json:"id"`
		Status     optionalString `json:"status"`
		DoneSoFar  optionalString `json:"done_so_far"`
		NextAction optionalString `json:"next_action"`
		Traps      optionalString `json:"traps"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if !in.Status.present && !in.DoneSoFar.present && !in.NextAction.present && !in.Traps.present {
		return refusal("no state fields supplied: send at least one of status, done_so_far, next_action, or traps")
	}
	for _, f := range []struct {
		name      string
		value     optionalString
		multiline bool
	}{{"status", in.Status, false}, {"done_so_far", in.DoneSoFar, true}, {"next_action", in.NextAction, false}, {"traps", in.Traps, true}} {
		if !f.value.present {
			continue
		}
		if r := checkText(f.name, f.value.value, f.multiline, blockLimit); r != nil {
			return r
		}
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if r := s.underLease(loc, "update state of", func() error {
		fields, err := tasks.ReadTaskState(loc.item.Dir)
		if err != nil {
			return err
		}
		if in.Status.present {
			fields.Status = in.Status.value
		}
		if in.DoneSoFar.present {
			fields.DoneSoFar = in.DoneSoFar.value
		}
		if in.NextAction.present {
			fields.NextAction = in.NextAction.value
		}
		if in.Traps.present {
			fields.Traps = in.Traps.value
		}
		return tasks.WriteTaskState(loc.item.Dir, loc.item.Title, fields)
	}); r != nil {
		return r
	}
	return textResult(fmt.Sprintf("state.md of %s updated", loc.item.ID))
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

const completionEvidenceHandoff = " — write nothing more inside that folder. Host finalization removes tmp/; never cite tmp/ as retained evidence. Cite only durable artifacts you verified before completion, or say the full log was not retained. Stored output is not independent proof a check ran."

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
		if err := tasks.RequireCompletedChecklist(loc.item); err != nil {
			return refusal(err.Error())
		}
		return textResult(fmt.Sprintf("%s is already done", loc.item.ID) + completionEvidenceHandoff)
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
		return textResult(fmt.Sprintf("%s moved to %s/", loc.item.ID, tasks.StateDone) + completionEvidenceHandoff)
	}
	// The assigned task moves under the launching iteration's lease; the host finalizes it after
	// the box exits (commit binding, receipt, tmp/ removal). Normalizing here keeps state.md's
	// lifecycle fields truthful the moment the folder lands in done.
	if check := s.authority.ValidateAssignedCompletion; check != nil {
		if err := check(); err != nil {
			return refusal(fmt.Sprintf("complete %s: %v; the task has not moved — repair the problem and retry tasks_complete in this turn", loc.item.ID, err))
		}
	}
	// Binding validation may take time. Re-read the checklist immediately
	// before moving; the host finalizer checks it again after provider exit.
	loc, r = s.locate(in.ID)
	if r != nil {
		return r
	}
	if err := tasks.RequireCompletedChecklist(loc.item); err != nil {
		return refusal(err.Error())
	}
	if err := tasks.MoveTaskDir(loc.root, loc.item, tasks.StateDone); err != nil {
		return refusal(fmt.Sprintf("complete %s: %v", loc.item.ID, err))
	}
	dir := filepath.Join(loc.root, tasks.StateDone, loc.item.ID)
	if err := tasks.NormalizeTaskState(loc.item.ID, dir, "complete", "none", "—", "—"); err != nil {
		return refusal(fmt.Sprintf("%s moved to %s/ but its state.md could not be normalized: %v", loc.item.ID, tasks.StateDone, err))
	}
	return textResult(fmt.Sprintf("%s moved to %s/", loc.item.ID, tasks.StateDone) + completionEvidenceHandoff)
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
			Text string      `json:"text"`
			Done nonNullBool `json:"done"`
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
		items = append(items, tasks.Subtask{Text: st.Text, Done: bool(st.Done)})
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
		Kind       string        `json:"kind"`
		Title      string        `json:"title"`
		Context    string        `json:"context"`
		Acceptance string        `json:"acceptance"`
		Approach   string        `json:"approach"`
		Subtasks   []string      `json:"subtasks"`
		Queue      nonNullString `json:"queue"`
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
	root, r := s.proposalRoot(string(in.Queue))
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
	return refusal(field + " must be " + tasks.TaskTextRequirement(multiline, limit))
}
