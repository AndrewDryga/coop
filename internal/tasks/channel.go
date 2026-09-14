package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The host-side face of the in-box task channel (internal/taskmcp). Every mutation a box can
// request through its eight task tools lands here, on the same validation, file discipline, and
// collision rules the CLI uses, so a call from the box can never write a queue the host would not
// have written itself.

// Task text/list limits are shared by proposal validation and the task-channel descriptors.
const (
	TaskTitleLimit = 256
	TaskLineLimit  = 4096
	TaskBlockLimit = 64 << 10
	TaskListLimit  = 64
	// TaskProposalFileLimit includes JSON escaping and metadata, not just input text.
	TaskProposalFileLimit = 256 << 10
)

// TaskTextRequirement describes ValidTaskText without confusing UTF-8 bytes with characters.
func TaskTextRequirement(multiline bool, limit int) string {
	shape := "one safe line"
	controls := "no control characters"
	if multiline {
		shape = "text"
		controls = "only newline and tab control characters allowed"
	}
	return fmt.Sprintf("non-blank UTF-8 %s of at most %d bytes; %s", shape, limit, controls)
}

// ValidTaskText reports whether value is safe, non-empty text of at most limit bytes: valid UTF-8
// with no control characters other than (when multiline) newline and tab — the fork-proposal
// rule, applied to every string a box hands the channel.
func ValidTaskText(value string, multiline bool, limit int) bool {
	return validProposalText(value, multiline, limit)
}

// TaskDraft is the body of a task the channel creates or proposes: the sections and checklist
// `coop tasks add --context/--acceptance/--approach/--subtask` fills.
type TaskDraft struct {
	Kind       ForkProposalKind
	Title      string
	Context    string
	Acceptance string
	Approach   string
	Subtasks   []string
}

func (d TaskDraft) proposal() (ForkTaskProposal, error) {
	idbuf := make([]byte, 16)
	if _, err := rand.Read(idbuf); err != nil {
		return ForkTaskProposal{}, err
	}
	proposal := ForkTaskProposal{
		Version: forkProposalVersion, ID: hex.EncodeToString(idbuf), Kind: d.Kind, Title: d.Title,
		Context: d.Context, Acceptance: d.Acceptance, Approach: d.Approach, Subtasks: d.Subtasks,
	}
	if err := validateForkTaskProposal(proposal); err != nil {
		return ForkTaskProposal{}, err
	}
	return proposal, nil
}

// CreateDraftTask validates draft and creates its folder under root — 00_todo/ for kind task,
// xx_backlog/ for kind backlog — exactly as `coop tasks add` / `coop backlog add` would.
func CreateDraftTask(root string, draft TaskDraft) (Item, error) {
	proposal, err := draft.proposal()
	if err != nil {
		return Item{}, err
	}
	state := StateTodo
	if proposal.Kind == ForkProposalBacklog {
		state = StateBacklog
	}
	values := map[string]string{
		"Context": proposal.Context, "Acceptance criteria": proposal.Acceptance, "Approach": proposal.Approach,
	}
	id, err := createTaskFolder(root, state, slugify(proposal.Title), proposal.Title, values, proposal.Subtasks)
	if err != nil {
		return Item{}, err
	}
	var items []Item
	if state == StateBacklog {
		items, err = ReadBacklog(root)
	} else {
		items, err = ReadTaskTree(root)
	}
	if err != nil {
		return Item{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return Item{}, fmt.Errorf("task %s was created but cannot be read back", id)
}

// WriteForkProposal validates draft and writes it into outbox as <id>.json — the exact file
// ImportForkProposals reads when the fork lands — returning the file's path. The outbox's entry
// limit is enforced here so the import can never be poisoned by one proposal too many.
func WriteForkProposal(outbox string, draft TaskDraft) (string, error) {
	proposal, err := draft.proposal()
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(outbox)
	if err != nil {
		return "", fmt.Errorf("open proposal outbox: %w", err)
	}
	defer root.Close()
	entries, err := readDirBounded(root, forkProposalCountLimit)
	if err != nil {
		return "", fmt.Errorf("read proposal outbox: %w", err)
	}
	if len(entries) >= forkProposalCountLimit {
		return "", fmt.Errorf("the proposal outbox already holds %d proposals, its limit — this run cannot file more", forkProposalCountLimit)
	}
	data, err := json.MarshalIndent(proposal, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if len(data) > TaskProposalFileLimit {
		return "", fmt.Errorf("proposal exceeds %d bytes of serialized JSON — shorten its text or checklist before retrying", TaskProposalFileLimit)
	}
	name := proposal.ID + ".json"
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return "", fmt.Errorf("write proposal %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return filepath.Join(outbox, name), nil
}

// TaskStateFields are the four labeled lines of a state.md resume snapshot.
type TaskStateFields struct {
	Status, DoneSoFar, NextAction, Traps string
}

// ReadTaskState reads the canonical snapshot written by WriteTaskState. It preserves multiline
// Done so far and Traps values so a partial tool update cannot erase an omitted field.
func ReadTaskState(taskDir string) (TaskStateFields, error) {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return TaskStateFields{}, err
	}
	defer root.Close()
	body, err := ReadTaskMetadataFile(root, "state.md")
	if err != nil {
		return TaskStateFields{}, fmt.Errorf("read state.md: %w", err)
	}
	lines := strings.Split(string(body), "\n")
	labels := []string{taskStateStatus, taskStateDone, taskStateNext, taskStateTraps}
	indexes := make([]int, len(labels))
	for i, label := range labels {
		found := labeledLineIndexes(lines, label)
		if len(found) != 1 || i > 0 && found[0] <= indexes[i-1] {
			return TaskStateFields{}, fmt.Errorf("state.md must contain one ordered %s field", label)
		}
		indexes[i] = found[0]
	}
	value := func(i int) string {
		end := len(lines)
		if i+1 < len(indexes) {
			end = indexes[i+1]
		}
		parts := append([]string{strings.TrimPrefix(strings.TrimPrefix(lines[indexes[i]], labels[i]), " ")}, lines[indexes[i]+1:end]...)
		return strings.TrimSuffix(strings.Join(parts, "\n"), "\n")
	}
	return TaskStateFields{Status: value(0), DoneSoFar: value(1), NextAction: value(2), Traps: value(3)}, nil
}

// WriteTaskState overwrites state.md with the canonical snapshot shape `coop tasks add` seeds, so
// the file stays parseable by completion normalization (one line per label) whatever the agent
// wrote before.
func WriteTaskState(taskDir, title string, fields TaskStateFields) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	body := fmt.Sprintf("# State — %s\n\n%s %s\n%s %s\n%s %s\n%s %s\n",
		title, taskStateStatus, fields.Status, taskStateDone, fields.DoneSoFar,
		taskStateNext, fields.NextAction, taskStateTraps, fields.Traps)
	return AtomicWriteTaskFile(root, "state.md", []byte(body))
}

// Subtask is one checklist item of a task's `## Subtasks` section.
type Subtask struct {
	Text string
	Done bool
}

// RewriteSubtasks replaces task.md's `## Subtasks` checklist — from its heading to the next `## `
// heading or the end of the file — with the given items, appending the section when the task has
// none, so the host's [n/m] counter and lint keep parsing it by construction.
func RewriteSubtasks(taskDir string, subtasks []Subtask) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	body, err := ReadTaskMetadataFile(root, "task.md")
	if err != nil {
		return fmt.Errorf("read task.md: %w", err)
	}
	var section strings.Builder
	section.WriteString("## Subtasks\n")
	for _, st := range subtasks {
		marker := " "
		if st.Done {
			marker = "x"
		}
		fmt.Fprintf(&section, "- [%s] %s\n", marker, st.Text)
	}
	lines := strings.Split(string(body), "\n")
	start, end := -1, len(lines)
	inFence := false
	for i, line := range lines {
		if fenceMarker(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if start < 0 {
			if strings.TrimSpace(line) == "## Subtasks" {
				start = i
			}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			end = i
			break
		}
	}
	var out string
	if start < 0 {
		out = strings.TrimRight(string(body), "\n") + "\n\n" + section.String()
	} else {
		before := strings.Join(lines[:start], "\n")
		after := strings.TrimLeft(strings.Join(lines[end:], "\n"), "\n")
		out = before + "\n" + section.String()
		if after != "" {
			out += "\n" + after
		}
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
	}
	return AtomicWriteTaskFile(root, "task.md", []byte(out))
}

// Decision is the one-way-door question a box parks a task on.
type Decision struct {
	Question       string
	Options        []string
	Recommendation string
}

// RenderDecision is the decision.md body for a filled-in request — the shape `coop tasks block`
// seeds, with the Resolution line left for the human. Separate from the write so a caller can ask
// "is the file already exactly this request?" without writing anything (see saveRequestedDecision).
func RenderDecision(id, title string, d Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- Explain the choice and your recommendation. A human supplies the answer.\n"+
		"     To save the answer and return this task to todo:\n"+
		"     coop tasks unblock %s \"<answer>\" -->\n\n", id)
	fmt.Fprintf(&b, "# Decision: %s?\n\n**Blocks:** this task (%s).\n\n**The decision:** %s\n\n**Options:**\n\n", title, id, d.Question)
	for _, option := range d.Options {
		fmt.Fprintf(&b, "- %s\n", option)
	}
	fmt.Fprintf(&b, "\n**Recommendation:** %s\n\n---\n\n%s", d.Recommendation, decisionResolutionLine)
	return b.String()
}

// WriteDecision writes a task's decision.md in the shape `coop tasks block` seeds, filled in, with
// the Resolution line left for the human. An existing decision.md is replaced: a blocked task
// carries exactly one open question, and `coop tasks unblock` reads the first Resolution it finds.
func WriteDecision(taskDir, id, title string, d Decision) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return AtomicWriteTaskFile(root, "decision.md", []byte(RenderDecision(id, title, d)))
}

// TaskFileNames are the task-folder files the channel reads back for an agent, in order.
var TaskFileNames = []string{"task.md", "state.md", "log.md", "decision.md"}

// ReadTaskFiles returns the task folder's metadata files by name, omitting absent ones, each read
// through the bounded single-link boundary every host reader uses.
func ReadTaskFiles(taskDir string) (map[string]string, error) {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files := map[string]string{}
	for _, name := range TaskFileNames {
		data, err := ReadTaskMetadataFile(root, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		files[name] = string(data)
	}
	return files, nil
}
