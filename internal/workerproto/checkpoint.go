package workerproto

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	WorkspaceCheckpointVersion         = 1
	WorkspaceCheckpointBundleMediaType = "application/vnd.coop.workspace-checkpoint.v1+tar"
	MaxWorkspaceCheckpointBundleBytes  = 64 << 20
	MaxWorkspaceCheckpointSubtasks     = 64
)

var (
	workspaceCheckpointIdentityPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	workspaceCheckpointRevisionPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	workspaceCheckpointTaskStates      = []string{"todo", "in_progress", "blocked", "done"}
	workspaceCheckpointGateStates      = []string{"not_run", "passed", "failed", "startup_error"}
)

// WorkspaceCheckpoint is the portable, control-plane-visible identity of one
// encrypted checkpoint artifact. The artifact contains the tracked binary patch,
// selected untracked files, and exact task projection. It never contains a
// worker-local path, inode, provider transcript, credential, or home directory.
type WorkspaceCheckpoint struct {
	Version             int                       `json:"version"`
	CheckpointRef       string                    `json:"checkpoint_ref"`
	SessionRef          string                    `json:"session_ref"`
	PlacementGeneration int                       `json:"placement_generation"`
	RepositoryRef       string                    `json:"repository_ref"`
	BaseRevision        string                    `json:"base_revision"`
	BranchRef           string                    `json:"branch_ref"`
	CommittedRevision   string                    `json:"committed_revision"`
	CandidateTreeSHA256 string                    `json:"candidate_tree_sha256"`
	Task                WorkspaceCheckpointTask   `json:"task"`
	Gate                WorkspaceCheckpointGate   `json:"gate"`
	Bundle              WorkspaceCheckpointBundle `json:"bundle"`
	CreatedAt           time.Time                 `json:"created_at"`
}

type WorkspaceCheckpointTask struct {
	QueueID     string `json:"queue_id"`
	TaskID      string `json:"task_id"`
	ID          string `json:"id"`
	State       string `json:"state"`
	Subtasks    []bool `json:"subtasks"`
	StateSHA256 string `json:"state_sha256"`
}

type WorkspaceCheckpointGate struct {
	Status     string `json:"status"`
	Revision   string `json:"revision,omitempty"`
	ReceiptRef string `json:"receipt_ref,omitempty"`
}

type WorkspaceCheckpointBundle struct {
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	ByteSize  int64  `json:"byte_size"`
}

func DecodeWorkspaceCheckpoint(document []byte) (WorkspaceCheckpoint, error) {
	var checkpoint WorkspaceCheckpoint
	if err := decodeStrict(document, &checkpoint); err != nil {
		return WorkspaceCheckpoint{}, fmt.Errorf("invalid workspace checkpoint: %w", err)
	}
	if err := checkpoint.Validate(); err != nil {
		return WorkspaceCheckpoint{}, err
	}
	return checkpoint, nil
}

func (c WorkspaceCheckpoint) Validate() error {
	if c.Version != WorkspaceCheckpointVersion {
		return fmt.Errorf("unsupported workspace checkpoint version %d", c.Version)
	}
	for field, value := range map[string]string{
		"checkpoint_ref": c.CheckpointRef,
		"session_ref":    c.SessionRef,
		"repository_ref": c.RepositoryRef,
	} {
		if err := reference(value, 256, field); err != nil {
			return err
		}
	}
	if err := validateWorkspaceCheckpointBranch(c.BranchRef); err != nil {
		return err
	}
	if c.PlacementGeneration < 1 {
		return errors.New("placement_generation must be positive")
	}
	if !workspaceCheckpointRevisionPattern.MatchString(c.BaseRevision) ||
		!workspaceCheckpointRevisionPattern.MatchString(c.CommittedRevision) {
		return errors.New("checkpoint revisions must be exact Git object identities")
	}
	if !digestPattern.MatchString(c.CandidateTreeSHA256) {
		return errors.New("candidate_tree_sha256 must be a lowercase SHA-256 digest")
	}
	if c.CreatedAt.IsZero() {
		return errors.New("created_at is required")
	}
	_, offset := c.CreatedAt.Zone()
	if offset != 0 {
		return errors.New("created_at must be UTC")
	}
	if err := c.Task.validate(); err != nil {
		return err
	}
	if err := c.Gate.validate(); err != nil {
		return err
	}
	return c.Bundle.validate()
}

func validateWorkspaceCheckpointBranch(value string) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || value == "@" ||
		strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return errors.New("branch_ref is invalid")
	}
	for _, item := range value {
		if item < 0x20 || item == 0x7f || strings.ContainsRune(" ~^:?*[\\", item) {
			return errors.New("branch_ref is invalid")
		}
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return errors.New("branch_ref is invalid")
		}
	}
	return nil
}

func (t WorkspaceCheckpointTask) validate() error {
	if !workspaceCheckpointIdentityPattern.MatchString(t.QueueID) ||
		!workspaceCheckpointIdentityPattern.MatchString(t.TaskID) {
		return errors.New("checkpoint task requires portable queue and task identities")
	}
	if err := reference(t.ID, 256, "task id"); err != nil {
		return err
	}
	if !slices.Contains(workspaceCheckpointTaskStates, t.State) {
		return errors.New("invalid checkpoint task state")
	}
	if len(t.Subtasks) < 1 || len(t.Subtasks) > MaxWorkspaceCheckpointSubtasks {
		return fmt.Errorf("checkpoint task must contain 1 to %d subtasks", MaxWorkspaceCheckpointSubtasks)
	}
	if !digestPattern.MatchString(t.StateSHA256) {
		return errors.New("task state_sha256 must be a lowercase SHA-256 digest")
	}
	return nil
}

func (g WorkspaceCheckpointGate) validate() error {
	if !slices.Contains(workspaceCheckpointGateStates, g.Status) {
		return errors.New("invalid checkpoint gate status")
	}
	if g.Status == "not_run" {
		if g.Revision != "" || g.ReceiptRef != "" {
			return errors.New("not-run checkpoint gate cannot carry a revision or receipt")
		}
		return nil
	}
	if !workspaceCheckpointRevisionPattern.MatchString(g.Revision) {
		return errors.New("completed checkpoint gate requires its exact revision")
	}
	return reference(g.ReceiptRef, 256, "gate receipt_ref")
}

func (b WorkspaceCheckpointBundle) validate() error {
	if b.MediaType != WorkspaceCheckpointBundleMediaType {
		return errors.New("invalid checkpoint bundle media type")
	}
	if !digestPattern.MatchString(b.SHA256) {
		return errors.New("checkpoint bundle sha256 must be a lowercase SHA-256 digest")
	}
	if b.ByteSize < 1 || b.ByteSize > MaxWorkspaceCheckpointBundleBytes {
		return fmt.Errorf("checkpoint bundle must contain 1 to %d bytes", MaxWorkspaceCheckpointBundleBytes)
	}
	return nil
}
