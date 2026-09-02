package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const controllerTaskRecordFile = ".coop-controller-task.json"

var controllerTaskReferenceRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// ControllerTaskDraft is the exact host-approved engineering checklist that a remote workspace
// owns. The durable task queue remains the lifecycle authority; this document only makes creation
// deterministic and rejects a changed replay under the same offer identity.
type ControllerTaskDraft struct {
	OfferRef        string   `json:"offer_ref"`
	Title           string   `json:"title"`
	Prompt          string   `json:"prompt"`
	SuccessChecks   []string `json:"success_checks"`
	AuthorityLimits []string `json:"authority_limits"`
	InstructionRef  string   `json:"instruction_ref,omitempty"`
	SourceRefs      []string `json:"source_refs"`
}

type controllerTaskRecord struct {
	Version     int    `json:"version"`
	OfferRef    string `json:"offer_ref"`
	DraftSHA256 string `json:"draft_sha256"`
}

// ControllerTaskBinding is the durable controller identity recovered from an existing task
// projection. It deliberately omits inode generation: a restored projection is a new local
// filesystem instance of the same queue and task identities.
type ControllerTaskBinding struct {
	QueueID     string
	TaskID      string
	ID          string
	OfferRef    string
	DraftSHA256 string
}

// ReadControllerTaskBinding verifies that a restored task projection still contains the exact
// approved draft and durable queue/task identities before a session row is allowed to bind it.
func ReadControllerTaskBinding(workspace, id string) (ControllerTaskBinding, error) {
	queue := filepath.Join(workspace, TasksRoot)
	item, ok, err := CurrentTask(queue, id)
	if err != nil {
		return ControllerTaskBinding{}, err
	}
	if !ok {
		return ControllerTaskBinding{}, errors.New("restored controller task is missing")
	}
	instance, err := ReadTaskInstance(queue, item)
	if err != nil {
		return ControllerTaskBinding{}, err
	}
	root, err := OpenTaskMetadataRoot(item.Dir)
	if err != nil {
		return ControllerTaskBinding{}, err
	}
	defer root.Close()
	recordBody, err := ReadTaskMetadataFile(root, controllerTaskRecordFile)
	if err != nil {
		return ControllerTaskBinding{}, fmt.Errorf("read restored controller task identity: %w", err)
	}
	var record controllerTaskRecord
	if err := decodeControllerTaskJSON(recordBody, &record); err != nil || record.Version != 1 {
		return ControllerTaskBinding{}, errors.New("restored controller task identity is invalid")
	}
	draftBody, err := ReadTaskMetadataFile(root, ".coop-controller-draft.json")
	if err != nil {
		return ControllerTaskBinding{}, fmt.Errorf("read restored controller task draft: %w", err)
	}
	var draft ControllerTaskDraft
	if err := decodeControllerTaskJSON(draftBody, &draft); err != nil {
		return ControllerTaskBinding{}, errors.New("restored controller task draft is invalid")
	}
	_, digest, err := validateControllerTaskDraft(draft)
	if err != nil || record.OfferRef != draft.OfferRef || record.DraftSHA256 != digest {
		return ControllerTaskBinding{}, errors.New("restored controller task conflicts with its approved draft")
	}
	return ControllerTaskBinding{
		QueueID: instance.Ref.QueueID, TaskID: instance.Ref.TaskID, ID: instance.Ref.ID,
		OfferRef: record.OfferRef, DraftSHA256: record.DraftSHA256,
	}, nil
}

func decodeControllerTaskJSON(body []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("controller task document contains multiple JSON values")
		}
		return err
	}
	return nil
}

// ControllerTaskDraftSHA256 returns the digest used by both the task folder and session binding.
// It validates the complete draft first, so callers cannot persist an identity for an unusable
// projection.
func ControllerTaskDraftSHA256(draft ControllerTaskDraft) (string, error) {
	_, digest, err := validateControllerTaskDraft(draft)
	return digest, err
}

// EnsureControllerTask creates or reconciles exactly one task in a session workspace. It never
// follows a repository-controlled symlink while establishing the queue and never overwrites a
// task that already answers the deterministic identity with different approved bytes.
func EnsureControllerTask(workspace string, draft ControllerTaskDraft) (TaskInstance, error) {
	encoded, digest, err := validateControllerTaskDraft(draft)
	if err != nil {
		return TaskInstance{}, err
	}
	root, err := ensureControllerTaskRoot(workspace)
	if err != nil {
		return TaskInstance{}, err
	}
	id := controllerTaskID(draft.OfferRef)
	if item, ok, err := CurrentTask(root, id); err != nil {
		return TaskInstance{}, err
	} else if ok {
		if err := requireControllerTaskRecord(item.Dir, draft.OfferRef, digest); err != nil {
			return TaskInstance{}, err
		}
		return EnsureTaskInstance(root, item)
	}

	todo := filepath.Join(root, StateTodo)
	tmp, err := os.MkdirTemp(todo, ".controller-task-")
	if err != nil {
		return TaskInstance{}, fmt.Errorf("create controller task staging folder: %w", err)
	}
	defer os.RemoveAll(tmp)
	record, _ := json.Marshal(controllerTaskRecord{Version: 1, OfferRef: draft.OfferRef, DraftSHA256: digest})
	files := map[string][]byte{
		"task.md":                []byte(controllerTaskMarkdown(id, draft)),
		"log.md":                 []byte("# Log — " + draft.Title + "\n"),
		"state.md":               []byte(controllerTaskState(draft.Title)),
		controllerTaskRecordFile: append(record, '\n'),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), body, 0o644); err != nil {
			return TaskInstance{}, fmt.Errorf("write controller task %s: %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, ".coop-controller-draft.json"), append(encoded, '\n'), 0o644); err != nil {
		return TaskInstance{}, fmt.Errorf("write controller task draft: %w", err)
	}
	destination := filepath.Join(todo, id)
	if err := os.Rename(tmp, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return TaskInstance{}, fmt.Errorf("publish controller task: %w", err)
		}
		item, ok, readErr := CurrentTask(root, id)
		if readErr != nil {
			return TaskInstance{}, readErr
		}
		if !ok {
			return TaskInstance{}, errors.New("controller task publish raced without a durable task")
		}
		if err := requireControllerTaskRecord(item.Dir, draft.OfferRef, digest); err != nil {
			return TaskInstance{}, err
		}
		return EnsureTaskInstance(root, item)
	}
	item, ok, err := CurrentTask(root, id)
	if err != nil {
		return TaskInstance{}, err
	}
	if !ok {
		return TaskInstance{}, errors.New("published controller task is unreadable")
	}
	return EnsureTaskInstance(root, item)
}

func validateControllerTaskDraft(draft ControllerTaskDraft) ([]byte, string, error) {
	if !controllerReference(draft.OfferRef) || !controllerText(draft.Title, 120) ||
		strings.ContainsAny(draft.Title, "\r\n") || !controllerText(draft.Prompt, 12_000) ||
		!controllerTexts(draft.SuccessChecks, 1, 20, 1_000) ||
		!controllerTexts(draft.AuthorityLimits, 0, 20, 500) ||
		(draft.InstructionRef != "" && !controllerReference(draft.InstructionRef)) ||
		!controllerReferences(draft.SourceRefs, 20) {
		return nil, "", errors.New("controller task draft is invalid")
	}
	encoded, err := json.Marshal(draft)
	if err != nil || len(encoded) > 64<<10 {
		return nil, "", errors.New("controller task draft is outside its byte bound")
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func controllerText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) &&
		strings.IndexByte(value, 0) < 0 && strings.TrimSpace(value) != ""
}

func controllerTexts(values []string, minimum, maximum, itemMaximum int) bool {
	if len(values) < minimum || len(values) > maximum {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !controllerText(value, itemMaximum) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func controllerReference(value string) bool {
	return controllerText(value, 256) && controllerTaskReferenceRE.MatchString(value)
}

func controllerReferences(values []string, maximum int) bool {
	if len(values) > maximum {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !controllerReference(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func controllerTaskID(offerRef string) string {
	sum := sha256.Sum256([]byte(offerRef))
	return "responder-" + hex.EncodeToString(sum[:12])
}

func ensureControllerTaskRoot(workspace string) (string, error) {
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve controller workspace: %w", err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil || !filepath.IsAbs(workspace) || filepath.Clean(resolved) != abs {
		return "", errors.New("controller workspace must name an absolute real directory")
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("controller workspace is not a real directory")
	}
	paths := []string{filepath.Join(abs, ".agent"), filepath.Join(abs, TasksRoot)}
	for _, state := range TaskStates {
		paths = append(paths, filepath.Join(abs, TasksRoot, state))
	}
	for _, path := range paths {
		if err := ensureRealControllerDirectory(path); err != nil {
			return "", err
		}
	}
	return filepath.Join(abs, TasksRoot), nil
}

func ensureRealControllerDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("controller task path %q is not a real directory", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("controller task path %q is not a real directory", path)
	}
	return nil
}

func requireControllerTaskRecord(dir, offerRef, digest string) error {
	root, err := OpenTaskMetadataRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	body, err := ReadTaskMetadataFile(root, controllerTaskRecordFile)
	if err != nil {
		return fmt.Errorf("read controller task identity: %w", err)
	}
	var record controllerTaskRecord
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || record.Version != 1 ||
		record.OfferRef != offerRef || record.DraftSHA256 != digest {
		return errors.New("controller task identity conflicts with the approved draft")
	}
	return nil
}

func controllerTaskMarkdown(id string, draft ControllerTaskDraft) string {
	var body strings.Builder
	fmt.Fprintf(&body, "---\nid: %s\ntitle: %s\nlabels: [responder]\n---\n\n# %s\n\n", id, draft.Title, draft.Title)
	fmt.Fprintf(&body, "**Context:** %s\n\n", draft.Prompt)
	fmt.Fprintf(&body, "**Acceptance:** %s\n\n", strings.Join(draft.SuccessChecks, "; "))
	body.WriteString("**Approach:** Work only inside the confirmed repository authority and keep this checklist current.\n\n")
	if len(draft.AuthorityLimits) > 0 {
		body.WriteString("## Authority limits\n")
		for _, limit := range draft.AuthorityLimits {
			fmt.Fprintf(&body, "- %s\n", limit)
		}
		body.WriteByte('\n')
	}
	if draft.InstructionRef != "" {
		fmt.Fprintf(&body, "**Trusted instruction:** `%s`\n\n", draft.InstructionRef)
	}
	if len(draft.SourceRefs) > 0 {
		body.WriteString("**Source refs:** `" + strings.Join(draft.SourceRefs, "`, `") + "`\n\n")
	}
	body.WriteString("## Subtasks\n")
	for _, check := range draft.SuccessChecks {
		fmt.Fprintf(&body, "- [ ] %s\n", check)
	}
	return body.String()
}

func controllerTaskState(title string) string {
	return "# State — " + title + "\n\n**Status:** not started\n**Done so far:** —\n" +
		"**Next action:** Read task.md and execute the first unchecked subtask.\n**Traps:** —\n"
}
