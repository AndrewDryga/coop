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
	// session marks a tool only an interactive session (Authority.Session) is offered: a loop box
	// works the task its iteration assigned, so it never claims or releases one.
	session bool
	// loop marks a tool only a loop iteration is offered: it acts on the task the iteration holds.
	loop bool
}

// toolTable is THE tool set, in the order tools/list presents it. Adding a tool here is the whole
// change; doctor asserts a loop box sees exactly the non-session names and nothing shell-, exec-,
// or file-shaped.
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
		name:        "tasks_claim",
		description: "Claim a task before you work on it: a todo task moves into 10_in_progress/ and this session becomes its owner, so a coop loop or another session leaves it alone until you complete, block or release it, or this session ends. Claiming a task already in progress makes it yours when nobody else holds it. Refused when another live process holds or claims the task; a blocked task waits for a human's answer, and a done task stays archived.",
		schema:      object(map[string]any{"id": idProp()}, "id"),
		run:         (*Server).claim,
		session:     true,
	},
	{
		name:        "tasks_update_state",
		description: "Patch state.md: send id and one or more fields to change. Omitted fields keep their saved values. Refresh before each commit and before pausing. A complete status is refused while Done so far or Traps still holds the template's <…> text.",
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
		description: "Move the task into 99_done/ after required verification passed. List every required check that did not run after your final edits in unrun_checks, or send [] when all of them ran; a nonempty list refuses completion, so run those checks, or block on the missing authority with tasks_block. Normal implementation needs its task-bound commit. If the task is already satisfied or an investigation reasonably could not reproduce the issue, send outcome plus concrete reason and evidence to close without a fake commit. A task the review reopened, whose existing commit already meets the finding, re-closes with a normal completion, without outcome. Human decisions use tasks_block. Before calling, promote any promised logs from tmp/ to artifacts/ and verify their durable paths, not links back into tmp/. Keep evidence in artifacts/, not compiled programs or rebuildable bulk: put those in tmp/ and record a binary's commit, sha256 and build command in log.md. Requires a nonempty, fully checked checklist. Refused while state.md's Done so far or Traps still holds the template's <…> text, or while log.md has no entry saying what changed and why: Coop's own notes do not count, and a no-change outcome's reason and evidence become its entry. Refused for a blocked task, which waits for a human's answer, and when another live process holds the task.",
		schema: object(map[string]any{
			"id":       idProp(),
			"outcome":  enumProp("Omit for normal implementation completion. No-change choices require reason and evidence.", "already_satisfied", "could_not_reproduce", "wont_fix"),
			"reason":   textProp("Why this truthful no-change outcome satisfies the task; required with outcome.", true, blockLimit),
			"evidence": textProp("Existing implementation and verification evidence, or exact reproduction attempts and retained logs; required with outcome.", true, blockLimit),
			"unrun_checks": map[string]any{
				"type":        "array",
				"description": "Every required check that did not run after your final edits, one per item. Send [] only when all of them ran. A nonempty list refuses completion: run those checks, or block on the missing authority with tasks_block.",
				"maxItems":    maxItems,
				"items":       textProp("One required check that did not run, such as its exact command.", false, lineLimit),
			},
		}, "id", "unrun_checks"),
		run: (*Server).complete,
	},
	{
		name:        "tasks_block",
		description: "Park the task in 50_blocked/ on a one-way-door decision a human must make, writing its decision.md (question, why it is blocked, options, your recommendation). Refused when another live process holds the task.",
		schema: object(map[string]any{
			"id":             idProp(),
			"question":       textProp("The question for the human, in plain words, on one line.", false, lineLimit),
			"why":            textProp("What happened, and why the task cannot go on without an answer.", true, blockLimit),
			"options":        array(textProp("One option: A — <name>: <consequence>.", false, lineLimit), "1 to 64 options."),
			"recommendation": textProp("The option best for the product in the long term, and why.", true, blockLimit),
		}, "id", "question", "why", "options", "recommendation"),
		run: (*Server).block,
	},
	{
		name:        "tasks_wait",
		description: "Return your assigned task to 00_todo/ until another task is done, when this one must not start or land before it. The loop skips it while that task is unfinished and works it again once it is in 99_done/; a person decides only when the task it waits for is blocked. Call it before committing anything for this task, which completes later with its one commit: save unfinished changes as a patch in its tmp/ folder, put the checkout back as you found it, and say in state.md how to resume. Refused for a task that is not in the queue, already done, or waiting for this one in turn. A human choice uses tasks_block instead.",
		schema: object(map[string]any{
			"id":     idProp(),
			"on":     textProp("The exact id of the task this one must wait for, as tasks_list reports it.", false, lineLimit),
			"reason": textProp("Why this task cannot go on before that one is done, for its log.md.", true, blockLimit),
		}, "id", "on", "reason"),
		run:  (*Server).wait,
		loop: true,
	},
	{
		name:        "tasks_release",
		description: "Hand back a task you claimed and will not finish in this session: it returns to 00_todo/ with its notes, so a coop loop or another session can take it. Refused when another live process holds or claims the task.",
		schema:      object(map[string]any{"id": idProp()}, "id"),
		run:         (*Server).release,
		session:     true,
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

// ToolNames lists a loop box's tool set in presentation order — the fixed contract doctor asserts.
func ToolNames() []string { return names(offeredTools(false)) }

// offeredTools is the tool set a server offers, in presentation order: the loop's nine, or for an
// interactive session the shared ones plus the claim and release pair it works its own tasks with.
func offeredTools(session bool) []tool {
	out := make([]tool, 0, len(toolTable))
	for _, t := range toolTable {
		if t.session && !session || t.loop && session {
			continue
		}
		out = append(out, t)
	}
	return out
}

func (s *Server) tools() []tool { return offeredTools(s.authority.Session != nil) }

func names(tools []tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.name
	}
	return out
}

func (s *Server) toolNames() string { return strings.Join(names(s.tools()), ", ") }

func (s *Server) toolDescriptors() []map[string]any {
	tools := s.tools()
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{"name": t.name, "description": t.description, "inputSchema": t.schema})
	}
	return out
}

func (s *Server) toolByName(name string) (tool, bool) {
	for _, t := range s.tools() {
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return refusal("invalid arguments: expected an object of named arguments")
	}
	target := reflect.ValueOf(v).Elem()
	typ := target.Type()
	for name, value := range fields {
		index := -1
		var expected []string
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key := field.Tag.Get("json")
			expected = append(expected, key)
			if key == name {
				index = i
			}
		}
		if index < 0 {
			return refusal(fmt.Sprintf("invalid argument %q: expected fields %s", name, strings.Join(expected, ", ")))
		}
		field := target.Field(index)
		shape := argumentShape(field.Type())
		if field.Kind() == reflect.Slice {
			var err error
			value, err = arrayArgument(value)
			if err != nil {
				return refusal(fmt.Sprintf("invalid argument %s: expected %s: %v", name, shape, err))
			}
		}
		dec := json.NewDecoder(bytes.NewReader(value))
		dec.DisallowUnknownFields()
		if err := dec.Decode(field.Addr().Interface()); err != nil {
			return refusal(fmt.Sprintf("invalid argument %s: expected %s: %v", name, shape, err))
		}
	}
	return nil
}

// arrayArgument accepts the array itself or one JSON encoding of it. Decode elements
// separately to keep null from silently becoming a string or a zero-valued item.
func arrayArgument(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > 0 && raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, err
		}
		raw = json.RawMessage(encoded)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	if items == nil {
		return nil, errors.New("null is not an array")
	}
	for _, item := range items {
		if bytes.Equal(bytes.TrimSpace(item), []byte("null")) {
			return nil, errors.New("array items must not be null")
		}
	}
	return raw, nil
}

func argumentShape(typ reflect.Type) string {
	if typ == reflect.TypeFor[optionalString]() {
		return "a string"
	}
	switch typ.Kind() {
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.String {
			return "an array of strings (or a string containing that JSON array)"
		}
		return "an array of objects; each item takes text (string) and optional done (boolean)"
	case reflect.Bool:
		return "a boolean"
	default:
		return "a string"
	}
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

// requiredStrings is a list that must be sent even when empty: encoding/json reads an omitted
// field and an explicit null alike as an empty list, and for unrun_checks an empty list claims that
// every required check ran.
type requiredStrings struct {
	present bool
	values  []string
}

func (s *requiredStrings) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return &json.UnmarshalTypeError{Value: "null", Type: reflect.TypeFor[[]string]()}
	}
	s.present = true
	return json.Unmarshal(raw, &s.values)
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
// machine caller must never mutate a task it only partially named. Every tool that names a task
// starts here, so each refuses a queue the box has pointed elsewhere by swapping a folder for a
// link: the queue is checked from the workspace before it is read (tasks.ReadTaskTreeBelow).
func (s *Server) locate(id string) (located, *toolResult) {
	if id == "" {
		return located{}, refusal("id is required")
	}
	var hits []located
	for _, root := range s.authority.QueueRoots {
		items, err := tasks.ReadTaskTreeBelow(s.authority.Workspace, root)
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
// the assigned task runs under the launching iteration's own lease and never leases twice. A done
// task is final: a lease taken on it would clear the receipt that marks its completion accepted.
func (s *Server) underLease(loc located, verb string, mutate func() error) *toolResult {
	if loc.item.ID == s.authority.Assigned {
		if err := mutate(); err != nil {
			return refusal(fmt.Sprintf("%s %s: %v", verb, loc.item.ID, err))
		}
		return nil
	}
	if loc.item.State == tasks.StateDone {
		return refusal(fmt.Sprintf("%s is done and its folder is final — %s refused", loc.item.ID, verb))
	}
	if r := s.boxWriteRefusal(loc, verb); r != nil {
		return r
	}
	lease, observed, err := tasks.TryTaskLease(loc.root, loc.item, s.authority.Owner)
	if err != nil {
		return refusal(fmt.Sprintf("%s %s: %v", verb, loc.item.ID, err))
	}
	if lease == nil {
		return heldRefusal(loc.item.ID, verb, observed.String())
	}
	// A box session claims under the lease authority this lease now holds, so a claim that landed
	// after the first check is visible here, and no new one can land until the write is done.
	if r := s.boxWriteRefusal(loc, verb); r != nil {
		if releaseErr := lease.Release(); releaseErr != nil {
			return refusal(fmt.Sprintf("%s %s: %v", verb, loc.item.ID, releaseErr))
		}
		return r
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

// boxActor is whose claims count as this box's own: the interactive session's, or nobody's in a
// loop box, which works its assignment under the iteration's lease and owns no claim.
func (s *Server) boxActor() tasks.ClaimActor {
	if s.authority.Session != nil {
		return *s.authority.Session
	}
	return tasks.ClaimActor{}
}

// boxWriteRefusal keeps the box off a task someone else holds — a live claim, a person's claim or a
// fork's assignment (tasks.BoxWriteRefusal) — with a reason it can relay to the person.
func (s *Server) boxWriteRefusal(loc located, verb string) *toolResult {
	if err := tasks.BoxWriteRefusal(loc.root, loc.item.ID, s.boxActor()); err != nil {
		return refusal(fmt.Sprintf("%s %s refused: %v — leave it to its owner, who can release it", verb, loc.item.ID, err))
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
	Assigned    bool   `json:"assigned,omitempty"`
	HasDecision bool   `json:"has_decision,omitempty"`
	WaitsOn     string `json:"waits_on,omitempty"`
	// Claim, in an interactive session, says who holds a live task: "yours" for the session's own
	// claim, else its owner, so the agent finds its work again after a provider switch.
	Claim string `json:"claim,omitempty"`
}

func (s *Server) listed(root string, item tasks.Item) listedTask {
	out := listedTask{ID: item.ID, Title: item.Title, State: tasks.StateLabel(item.State), Queue: root,
		Assigned: item.ID == s.authority.Assigned, HasDecision: item.HasDecision, WaitsOn: item.WaitsOn}
	out.Subtasks.Total = len(item.Subtasks)
	for _, done := range item.Subtasks {
		if done {
			out.Subtasks.Done++
		}
	}
	if s.authority.Session != nil && (item.State == tasks.StateTodo || item.State == tasks.StateInProgress) {
		claim, err := tasks.SessionClaimLabel(root, item.ID, *s.authority.Session)
		if err != nil {
			claim = "unknown: its claim record cannot be read"
		}
		out.Claim = claim
	}
	return out
}

func (s *Server) list(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		State nonNullString  `json:"state"`
		Query optionalString `json:"query"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	query := ""
	if in.Query.present {
		if r := checkText("query", in.Query.value, false, searchQueryLimit); r != nil {
			return r
		}
		query = strings.ToLower(strings.TrimSpace(in.Query.value))
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
		items, err := tasks.ReadTaskTreeBelow(s.authority.Workspace, root)
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
			summary := s.listed(root, item)
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
	files, err := tasks.ReadTaskFiles(s.authority.Workspace, loc.item.Dir)
	if err != nil {
		return refusal(fmt.Sprintf("read task %s: %v", loc.item.ID, err))
	}
	summary := s.listed(loc.root, loc.item)
	result := map[string]any{
		"id": summary.ID, "title": summary.Title, "state": summary.State, "queue": summary.Queue,
		"dir": loc.item.Dir, "subtasks": summary.Subtasks, "assigned": summary.Assigned, "files": files,
	}
	if summary.Claim != "" {
		result["claim"] = summary.Claim
	}
	return jsonResult(result)
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
		if gaps := stateTemplateGaps(fields); len(gaps) > 0 && statusSaysComplete(fields.Status) {
			return errors.New("a complete status keeps no template text: " + strings.Join(gaps, "; "))
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

// bulkNote follows a completion with the bulk warning `coop tasks done` prints, or nothing when the
// folder holds no bulk. Its advice is not the host's (record the build in log.md instead): the agent
// has just been told to write nothing more inside that folder, so it leaves the decision to a person.
// The folder is measured from the workspace, so a folder swapped for a link since the task was
// located measures nothing.
func (s *Server) bulkNote(dir string) string {
	headline, facts, ok := tasks.TaskBulkWarningBelow(s.authority.Workspace, dir)
	if !ok {
		return ""
	}
	return "\n\n" + headline + ".\n" + strings.Join(facts, "\n") +
		"\nLeave the folder as it is and say what these files are in your final reply, so a person can decide what to keep."
}

func (s *Server) complete(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID          string          `json:"id"`
		Outcome     optionalString  `json:"outcome"`
		Reason      optionalString  `json:"reason"`
		Evidence    optionalString  `json:"evidence"`
		UnrunChecks requiredStrings `json:"unrun_checks"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if r := checkUnrunChecks(in.UnrunChecks); r != nil {
		return r
	}
	claim := CompletionClaim{Outcome: "implemented"}
	if in.Outcome.present {
		claim.Outcome, claim.Reason, claim.Evidence = in.Outcome.value, in.Reason.value, in.Evidence.value
		switch claim.Outcome {
		case "already_satisfied", "could_not_reproduce", "wont_fix":
		default:
			return refusal("invalid outcome: use already_satisfied, could_not_reproduce, or wont_fix")
		}
		if !in.Reason.present || !in.Evidence.present {
			return refusal("no-change completion requires both reason and evidence")
		}
		if r := checkText("reason", claim.Reason, true, blockLimit); r != nil {
			return r
		}
		if r := checkText("evidence", claim.Evidence, true, blockLimit); r != nil {
			return r
		}
	} else if in.Reason.present || in.Evidence.present {
		return refusal("reason and evidence require a no-change outcome")
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if loc.item.State == tasks.StateDone {
		// A receipt means it was accepted, even with open items: a person can force-close a task. A
		// done folder without one is an unowned move, which still has to show a finished checklist.
		if !tasks.HasCompletionReceipt(loc.root, loc.item) {
			if err := tasks.RequireCompletedChecklist(loc.item); err != nil {
				return refusal(err.Error())
			}
		}
		return textResult(fmt.Sprintf("%s is already done", loc.item.ID) + completionEvidenceHandoff + s.bulkNote(loc.item.Dir))
	}
	// The same rule as `coop tasks claim`: a blocked task's decision belongs to a human, and only the
	// host's unblock returns it to work, so the box can finish neither its own blocked task nor another.
	if loc.item.State == tasks.StateBlocked {
		return refusal(fmt.Sprintf("%s is blocked on a decision for a human, who returns it to todo with `coop tasks unblock %s` — leave it alone", loc.item.ID, loc.item.ID))
	}
	if loc.item.ID != s.authority.Assigned {
		if claim.NoChange() && s.authority.Session != nil {
			// Host completion checks no commit, so a session closes a no-change task plainly.
			return refusal("outcome closes a loop's assigned task; in this session, record the reason and evidence with tasks_append_log, then call tasks_complete without outcome")
		}
		if claim.NoChange() {
			return refusal("no-change completion is available only for the assigned task")
		}
		if r := unrunChecksRefusal(loc.item.ID, in.UnrunChecks.values); r != nil {
			return r
		}
		if r := s.boxWriteRefusal(loc, "complete"); r != nil {
			return r
		}
		if r := completionRecordRefusal(loc.item, false); r != nil {
			return r
		}
		// Another task: the host's own trusted completion — its lease, receipt, and normalization —
		// so the loop's completion audit sees a controller-owned move, not an unowned one. It judges
		// the claim again under that lease, where no new claim can land.
		if err := tasks.CompleteBoxTask(s.authority.Workspace, loc.root, loc.item, s.boxActor()); err != nil {
			if errors.Is(err, tasks.ErrTaskLeased) {
				return heldRefusal(loc.item.ID, "complete", "leased")
			}
			return refusal(fmt.Sprintf("complete %s: %v", loc.item.ID, err))
		}
		dir := filepath.Join(loc.root, tasks.StateDone, loc.item.ID)
		return textResult(fmt.Sprintf("%s moved to %s/", loc.item.ID, tasks.StateDone) + completionEvidenceHandoff + s.bulkNote(dir))
	}
	// The assigned task moves under the launching iteration's lease; the host finalizes it after
	// the box exits (commit binding, receipt, tmp/ removal). Normalizing here keeps state.md's
	// lifecycle fields truthful the moment the folder lands in done.
	if err := tasks.RequireCompletedChecklist(loc.item); err != nil {
		return refusal(err.Error())
	}
	if r := unrunChecksRefusal(loc.item.ID, in.UnrunChecks.values); r != nil {
		return r
	}
	if r := completionRecordRefusal(loc.item, claim.NoChange()); r != nil {
		return r
	}
	if check := s.authority.ValidateAssignedCompletion; check != nil {
		if err := check(claim); err != nil {
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
	dir := filepath.Join(loc.root, tasks.StateDone, loc.item.ID)
	if claim.NoChange() {
		if err := tasks.CompleteTaskWithClosure(loc.root, loc.item, tasks.TaskClosure(claim)); err != nil {
			return refusal(fmt.Sprintf("complete %s: %v", loc.item.ID, err))
		}
	} else {
		if err := tasks.MoveTaskDir(loc.root, loc.item, tasks.StateDone); err != nil {
			return refusal(fmt.Sprintf("complete %s: %v", loc.item.ID, err))
		}
		if err := tasks.NormalizeTaskState(loc.item.ID, dir, "complete", "none", "—", "—"); err != nil {
			return refusal(fmt.Sprintf("%s moved to %s/ but its state.md could not be normalized: %v", loc.item.ID, tasks.StateDone, err))
		}
	}
	result := fmt.Sprintf("%s moved to %s/", loc.item.ID, tasks.StateDone)
	if claim.NoChange() {
		result = fmt.Sprintf("%s closed as %s", loc.item.ID, strings.ReplaceAll(claim.Outcome, "_", " "))
	}
	return textResult(result + completionEvidenceHandoff + s.bulkNote(dir))
}

func (s *Server) block(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID             string   `json:"id"`
		Question       string   `json:"question"`
		Why            string   `json:"why"`
		Options        []string `json:"options"`
		Recommendation string   `json:"recommendation"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if r := checkText("question", in.Question, false, lineLimit); r != nil {
		return r
	}
	if r := checkText("why", in.Why, true, blockLimit); r != nil {
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
	decision := tasks.Decision{Question: in.Question, Why: in.Why, Options: in.Options, Recommendation: in.Recommendation}
	// Before anything moves: text that would forge the human's answer is refused, not parked.
	if err := tasks.CheckDecision(decision); err != nil {
		return refusal(err.Error())
	}
	if loc.item.ID != s.authority.Assigned {
		if r := s.boxWriteRefusal(loc, "block"); r != nil {
			return r
		}
		if err := tasks.BlockBoxTask(loc.root, loc.item, s.boxActor()); err != nil {
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
	earlier, err := tasks.ReplaceDecision(dir, loc.item.ID, loc.item.Title, decision)
	if err != nil {
		return refusal(fmt.Sprintf("%s moved to %s/ but its decision.md could not be written: %v", loc.item.ID, tasks.StateBlocked, err))
	}
	result := fmt.Sprintf("%s moved to %s/ with its decision.md — a human resolves it; stop working it", loc.item.ID, tasks.StateBlocked)
	if earlier != "" {
		// The human had answered its last question; that answer is not lost, but it is the reason to
		// be sure this is a NEW question before the human is asked again.
		result += fmt.Sprintf(". It had been answered (%q); that decision is kept in its log.md", earlier)
	}
	return textResult(result)
}

// wait hands the task this iteration holds back to the queue until another task is done
// (tasks.WaitForTask). A fork's projection holds one task and cannot see the queue the other is in,
// so a fork task finishes or blocks instead.
func (s *Server) wait(_ context.Context, args json.RawMessage) *toolResult {
	var in struct {
		ID     string `json:"id"`
		On     string `json:"on"`
		Reason string `json:"reason"`
	}
	if r := decodeArgs(args, &in); r != nil {
		return r
	}
	if r := checkText("on", in.On, false, lineLimit); r != nil {
		return r
	}
	if r := checkText("reason", in.Reason, true, blockLimit); r != nil {
		return r
	}
	if s.authority.ProposalOutbox != "" {
		return refusal("a fork works one task and cannot wait for another: finish it, or park it with tasks_block if it must not land yet")
	}
	loc, r := s.locate(in.ID)
	if r != nil {
		return r
	}
	if loc.item.ID != s.authority.Assigned {
		return refusal(fmt.Sprintf("only the task assigned to this run can wait, and %s is not it", loc.item.ID))
	}
	// The base is read first and the binding check runs from it, so a commit landing meanwhile is one
	// the loop's post-exit audit sees as made after the wait.
	base := ""
	if s.authority.WaitBase != nil {
		head, err := s.authority.WaitBase()
		if err != nil {
			return refusal(fmt.Sprintf("wait %s: Coop could not read the history to check for its commit: %v", loc.item.ID, err))
		}
		base = head
	}
	if shas, err := tasks.TaskCommits(s.authority.Workspace, base, loc.item.ID); err != nil {
		return refusal(fmt.Sprintf("wait %s: Coop could not read the history to check for its commit: %v", loc.item.ID, err))
	} else if len(shas) > 0 {
		return refusal(fmt.Sprintf("%s already has a commit with its Coop-Task trailer (%s), and a task completes with one commit, so it cannot wait now: finish it, or park it with tasks_block if it must not land yet", loc.item.ID, strings.Join(shas, ", ")))
	}
	if err := tasks.WaitForTask(s.authority.QueueRoots, tasks.QueuedTask{Root: loc.root, Item: loc.item}, in.On, in.Reason); err != nil {
		return refusal(fmt.Sprintf("wait %s: %v", loc.item.ID, err))
	}
	if s.waitBase == "" {
		s.waitBase = base
	}
	return textResult(fmt.Sprintf("%s moved to %s/ and waits for %s — stop working it; the loop works it again once %s is done", loc.item.ID, tasks.StateTodo, in.On, in.On))
}

// claim and release are an interactive session's own lifecycle: it has no task assigned, so it
// claims the one it works and hands back one it will not finish. Both run on the host's task
// authority (tasks.ClaimSessionTask, tasks.ReleaseSessionTask), bound to the session's process.
func (s *Server) claim(_ context.Context, args json.RawMessage) *toolResult {
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
	replaced, err := tasks.ClaimSessionTask(loc.root, loc.item, *s.authority.Session)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskLeased) {
			return heldRefusal(loc.item.ID, "claim", "leased")
		}
		return refusal(fmt.Sprintf("claim %s: %v", loc.item.ID, err))
	}
	result := fmt.Sprintf("%s is claimed by this session, in %s/ — keep its files current as you work, then finish with tasks_complete, tasks_block or tasks_release", loc.item.ID, tasks.StateInProgress)
	if replaced != "" {
		result += fmt.Sprintf(". Its previous owner had stopped (%s)", replaced)
	}
	return textResult(result)
}

func (s *Server) release(_ context.Context, args json.RawMessage) *toolResult {
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
	switch loc.item.State {
	case tasks.StateTodo:
		// Only an unclaimed todo task is already handed back; a claim on one is still a claim to give up.
		if _, owned, err := tasks.ReadTaskOwnerRecord(loc.root, loc.item.ID); err != nil {
			return refusal(fmt.Sprintf("release %s: %v", loc.item.ID, err))
		} else if !owned {
			return textResult(fmt.Sprintf("%s is already in %s/ with no claim", loc.item.ID, tasks.StateTodo))
		}
	case tasks.StateInProgress:
	case tasks.StateBlocked:
		return refusal(fmt.Sprintf("%s is blocked on a decision only a human can answer; nothing to release", loc.item.ID))
	default:
		return refusal(fmt.Sprintf("%s is done; completed work stays in the archive", loc.item.ID))
	}
	if err := tasks.ReleaseSessionTask(loc.root, loc.item, *s.authority.Session); err != nil {
		if errors.Is(err, tasks.ErrTaskLeased) {
			return heldRefusal(loc.item.ID, "release", "leased")
		}
		return refusal(fmt.Sprintf("release %s: %v", loc.item.ID, err))
	}
	return textResult(fmt.Sprintf("%s returned to %s/ with its notes; this session no longer owns it", loc.item.ID, tasks.StateTodo))
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
		path, err := tasks.WriteForkProposal(s.authority.Workspace, s.authority.ProposalOutbox, draft)
		if err != nil {
			return refusal("propose: " + err.Error())
		}
		return textResult(fmt.Sprintf("proposal filed as %s — the host imports it into the canonical queue when this fork lands", filepath.Base(path)))
	}
	root, r := s.proposalRoot(string(in.Queue))
	if r != nil {
		return r
	}
	item, err := tasks.CreateDraftTask(s.authority.Workspace, root, draft)
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

// checkUnrunChecks refuses an unrun_checks list that was never sent, or whose entries a task file
// could not carry. The tool dispatcher reports an omitted field first; this keeps the handler from
// ever reading an omission as "every check ran".
func checkUnrunChecks(list requiredStrings) *toolResult {
	if !list.present {
		return refusal("unrun_checks is required: list every required check that did not run after your final edits, or send [] when all of them ran")
	}
	if len(list.values) > maxItems {
		return refusal(fmt.Sprintf("unrun_checks takes at most %d entries", maxItems))
	}
	for _, check := range list.values {
		if r := checkText("unrun_checks", check, false, lineLimit); r != nil {
			return r
		}
	}
	return nil
}

// unrunChecksRefusal turns away a completion whose worker reports a required check it did not run.
// Workers ticked such a check with the excuse appended to its checklist text, and only the final
// review caught it, after the loop had moved on.
func unrunChecksRefusal(id string, unrun []string) *toolResult {
	if len(unrun) == 0 {
		return nil
	}
	return refusal(fmt.Sprintf("%s has not moved: required checks did not run after the final edits: %s. Run them and call tasks_complete again with unrun_checks [], or, if this run lacks the authority to run them, call tasks_block on that missing authority", id, strings.Join(unrun, "; ")))
}

// completionRecordRefusal keeps Coop's scaffold out of a completed task: one night's final review
// reopened 7 of 12 tasks only because log.md held nothing but the template header, and completed
// state.md files still read the template's <…> Traps text. A no-change outcome writes its reason and
// evidence to log.md itself, so it needs no entry first. Completion repairs a malformed state.md, so
// only a readable one is checked.
func completionRecordRefusal(item tasks.Item, noChange bool) *toolResult {
	var gaps []string
	if fields, err := tasks.ReadTaskState(item.Dir); err == nil {
		gaps = stateTemplateGaps(fields)
	}
	if !noChange {
		hasEntry, err := tasks.TaskLogHasEntry(item.Dir)
		if err != nil {
			return refusal(fmt.Sprintf("complete %s: read log.md: %v", item.ID, err))
		}
		if !hasEntry {
			gaps = append(gaps, "log.md has no entry yet: append what changed and why with tasks_append_log (Coop's own notes do not count)")
		}
	}
	if len(gaps) == 0 {
		return nil
	}
	return refusal(fmt.Sprintf("%s has not moved: %s. Then call tasks_complete again", item.ID, strings.Join(gaps, "; ")))
}

// stateTemplateGaps names each state.md field a completed task keeps that still holds template
// text, with what tasks_update_state should send instead.
func stateTemplateGaps(fields tasks.TaskStateFields) []string {
	repairs := map[string]string{
		"Done so far": "send done_so_far: what is finished and verified",
		"Traps":       "send traps: the gotchas for the next agent, or \"—\" when there are none",
	}
	var gaps []string
	for _, label := range tasks.StateTemplateFields(fields) {
		gaps = append(gaps, fmt.Sprintf("state.md's %s still holds the template's <…> text (%s)", label, repairs[label]))
	}
	return gaps
}

// statusSaysComplete reads a status the way completion writes one: "complete", or "complete — …".
func statusSaysComplete(status string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(status)), "complete")
}

// checkText refuses text a task file must never carry: empty, over its limit, invalid UTF-8, or
// control characters (newlines and tabs allowed only where a block is expected).
func checkText(field, value string, multiline bool, limit int) *toolResult {
	if tasks.ValidTaskText(value, multiline, limit) {
		return nil
	}
	return refusal(field + " must be " + tasks.TaskTextRequirement(multiline, limit))
}
