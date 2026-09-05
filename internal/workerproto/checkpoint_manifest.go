package workerproto

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
)

const (
	MaxWorkspaceCheckpointFiles      = 4096
	MaxWorkspaceCheckpointTaskFiles  = 128
	MaxWorkspaceCheckpointPathBytes  = 1024
	MaxWorkspaceCheckpointManifest   = 1 << 20
	workspaceCheckpointManifestEntry = "manifest.json"
)

// WorkspaceCheckpointBundleManifest names every byte in the portable tar artifact. Tar entry
// names are host-generated and ordinal; original Git path bytes are base64 encoded so a checkpoint
// neither loses non-UTF-8 names nor lets an archive path select a restore destination.
type WorkspaceCheckpointBundleManifest struct {
	Version             int                               `json:"version"`
	CheckpointRef       string                            `json:"checkpoint_ref"`
	RepositoryRef       string                            `json:"repository_ref"`
	BaseRevision        string                            `json:"base_revision"`
	BranchRef           string                            `json:"branch_ref"`
	CommittedRevision   string                            `json:"committed_revision"`
	CandidateTreeSHA256 string                            `json:"candidate_tree_sha256"`
	TrackedPatch        WorkspaceCheckpointBundleEntry    `json:"tracked_patch"`
	UntrackedFiles      []WorkspaceCheckpointFileEntry    `json:"untracked_files"`
	TaskProjection      WorkspaceCheckpointTaskProjection `json:"task_projection"`
	GateReceipt         *WorkspaceCheckpointBundleEntry   `json:"gate_receipt"`
}

type WorkspaceCheckpointBundleEntry struct {
	Entry    string `json:"entry"`
	SHA256   string `json:"sha256"`
	ByteSize int64  `json:"byte_size"`
}

type WorkspaceCheckpointFileEntry struct {
	PathB64   string `json:"path_b64"`
	Entry     string `json:"entry"`
	Mode      int64  `json:"mode"`
	SHA256    string `json:"sha256"`
	ByteSize  int64  `json:"byte_size"`
	PathBytes []byte `json:"-"`
}

type WorkspaceCheckpointTaskReference struct {
	QueueID string
	TaskID  string
	ID      string
}

type WorkspaceCheckpointTaskProjection struct {
	QueueID     string                           `json:"queue_id"`
	TaskID      string                           `json:"task_id"`
	ID          string                           `json:"id"`
	State       string                           `json:"state"`
	StateSHA256 string                           `json:"state_sha256"`
	Files       []WorkspaceCheckpointFileEntry   `json:"files"`
	Ref         WorkspaceCheckpointTaskReference `json:"-"`
}

func DecodeWorkspaceCheckpointBundleManifest(document []byte) (WorkspaceCheckpointBundleManifest, error) {
	if len(document) == 0 || len(document) > MaxWorkspaceCheckpointManifest {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint bundle manifest exceeds its bound")
	}
	var manifest WorkspaceCheckpointBundleManifest
	if err := decodeStrict(document, &manifest); err != nil {
		return WorkspaceCheckpointBundleManifest{}, fmt.Errorf("invalid workspace checkpoint bundle manifest: %w", err)
	}
	if err := manifest.validate(); err != nil {
		return WorkspaceCheckpointBundleManifest{}, err
	}
	return manifest, nil
}

// ValidateForCapture applies the same strict contract used by decoding before a producer writes
// the manifest into a bundle.
func (m *WorkspaceCheckpointBundleManifest) ValidateForCapture() error { return m.validate() }

func ValidateWorkspaceCheckpointPair(
	checkpoint WorkspaceCheckpoint,
	manifest WorkspaceCheckpointBundleManifest,
) error {
	if checkpoint.Version != manifest.Version || checkpoint.CheckpointRef != manifest.CheckpointRef ||
		checkpoint.RepositoryRef != manifest.RepositoryRef || checkpoint.BaseRevision != manifest.BaseRevision ||
		checkpoint.BranchRef != manifest.BranchRef || checkpoint.CommittedRevision != manifest.CommittedRevision ||
		checkpoint.CandidateTreeSHA256 != manifest.CandidateTreeSHA256 {
		return errors.New("workspace checkpoint descriptor and manifest identity do not match")
	}
	if checkpoint.Task.QueueID != manifest.TaskProjection.QueueID ||
		checkpoint.Task.TaskID != manifest.TaskProjection.TaskID || checkpoint.Task.ID != manifest.TaskProjection.ID ||
		checkpoint.Task.State != manifest.TaskProjection.State ||
		checkpoint.Task.StateSHA256 != manifest.TaskProjection.StateSHA256 {
		return errors.New("workspace checkpoint descriptor and manifest task do not match")
	}
	if checkpoint.Gate.Status == "not_run" {
		if manifest.GateReceipt != nil {
			return errors.New("not-run workspace checkpoint unexpectedly contains a gate receipt")
		}
		return nil
	}
	if checkpoint.Gate.Revision != checkpoint.CommittedRevision || manifest.GateReceipt == nil {
		return errors.New("completed workspace checkpoint gate is not bound to its receipt")
	}
	return nil
}

func (m *WorkspaceCheckpointBundleManifest) validate() error {
	if m.Version != WorkspaceCheckpointVersion {
		return fmt.Errorf("unsupported workspace checkpoint bundle version %d", m.Version)
	}
	for field, value := range map[string]string{
		"checkpoint_ref": m.CheckpointRef,
		"repository_ref": m.RepositoryRef,
	} {
		if err := reference(value, 256, field); err != nil {
			return err
		}
	}
	if err := validateWorkspaceCheckpointBranch(m.BranchRef); err != nil {
		return err
	}
	if !workspaceCheckpointRevisionPattern.MatchString(m.BaseRevision) ||
		!workspaceCheckpointRevisionPattern.MatchString(m.CommittedRevision) ||
		!digestPattern.MatchString(m.CandidateTreeSHA256) {
		return errors.New("checkpoint bundle workspace identity is invalid")
	}
	if err := validateWorkspaceCheckpointBundleEntry(m.TrackedPatch, "workspace.patch", true); err != nil {
		return fmt.Errorf("tracked patch: %w", err)
	}
	if len(m.UntrackedFiles) > MaxWorkspaceCheckpointFiles {
		return fmt.Errorf("checkpoint contains more than %d untracked files", MaxWorkspaceCheckpointFiles)
	}
	entries := map[string]bool{workspaceCheckpointManifestEntry: true, m.TrackedPatch.Entry: true}
	total := m.TrackedPatch.ByteSize
	if err := validateWorkspaceCheckpointFiles(m.UntrackedFiles, "untracked", MaxWorkspaceCheckpointFiles, entries, &total); err != nil {
		return err
	}
	if err := m.TaskProjection.validate(entries, &total); err != nil {
		return err
	}
	if m.GateReceipt != nil {
		if err := validateWorkspaceCheckpointBundleEntry(*m.GateReceipt, "gate/receipt.json", false); err != nil {
			return fmt.Errorf("gate receipt: %w", err)
		}
		if entries[m.GateReceipt.Entry] {
			return errors.New("checkpoint bundle entry is duplicated")
		}
		entries[m.GateReceipt.Entry] = true
		total += m.GateReceipt.ByteSize
	}
	if total > MaxWorkspaceCheckpointBundleBytes {
		return errors.New("checkpoint bundle content exceeds its bound")
	}
	return nil
}

func (t *WorkspaceCheckpointTaskProjection) validate(entries map[string]bool, total *int64) error {
	if !workspaceCheckpointIdentityPattern.MatchString(t.QueueID) ||
		!workspaceCheckpointIdentityPattern.MatchString(t.TaskID) {
		return errors.New("checkpoint task projection requires portable queue and task identities")
	}
	if err := reference(t.ID, 256, "task id"); err != nil {
		return err
	}
	if !containsString(workspaceCheckpointTaskStates, t.State) || !digestPattern.MatchString(t.StateSHA256) {
		return errors.New("checkpoint task projection state is invalid")
	}
	if len(t.Files) == 0 || len(t.Files) > MaxWorkspaceCheckpointTaskFiles {
		return fmt.Errorf("checkpoint task projection must contain 1 to %d files", MaxWorkspaceCheckpointTaskFiles)
	}
	if err := validateWorkspaceCheckpointFiles(t.Files, "task", MaxWorkspaceCheckpointTaskFiles, entries, total); err != nil {
		return err
	}
	t.Ref = WorkspaceCheckpointTaskReference{QueueID: t.QueueID, TaskID: t.TaskID, ID: t.ID}
	return nil
}

func validateWorkspaceCheckpointFiles(
	files []WorkspaceCheckpointFileEntry,
	prefix string,
	maximum int,
	entries map[string]bool,
	total *int64,
) error {
	if len(files) > maximum {
		return errors.New("checkpoint file count exceeds its bound")
	}
	paths := make(map[string]bool, len(files))
	for index := range files {
		file := &files[index]
		expectedEntry := fmt.Sprintf("%s/%06d", prefix, index)
		if file.Entry != expectedEntry || entries[file.Entry] {
			return errors.New("checkpoint bundle entries must be unique host ordinals")
		}
		entries[file.Entry] = true
		if file.Mode != 0o644 && file.Mode != 0o755 {
			return errors.New("checkpoint file mode is invalid")
		}
		if !digestPattern.MatchString(file.SHA256) || file.ByteSize < 0 || file.ByteSize > MaxWorkspaceCheckpointBundleBytes {
			return errors.New("checkpoint file metadata is invalid")
		}
		decoded, err := decodeWorkspaceCheckpointPath(file.PathB64)
		if err != nil {
			return err
		}
		if paths[string(decoded)] {
			return errors.New("checkpoint file path is duplicated")
		}
		paths[string(decoded)] = true
		file.PathBytes = decoded
		*total += file.ByteSize
		if *total > MaxWorkspaceCheckpointBundleBytes {
			return errors.New("checkpoint bundle content exceeds its bound")
		}
	}
	return nil
}

func validateWorkspaceCheckpointBundleEntry(entry WorkspaceCheckpointBundleEntry, expected string, emptyAllowed bool) error {
	if entry.Entry != expected || !digestPattern.MatchString(entry.SHA256) || entry.ByteSize < 0 ||
		(!emptyAllowed && entry.ByteSize == 0) || entry.ByteSize > MaxWorkspaceCheckpointBundleBytes {
		return errors.New("checkpoint bundle entry metadata is invalid")
	}
	return nil
}

func decodeWorkspaceCheckpointPath(encoded string) ([]byte, error) {
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(MaxWorkspaceCheckpointPathBytes) {
		return nil, errors.New("checkpoint path is invalid")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded || len(decoded) == 0 ||
		len(decoded) > MaxWorkspaceCheckpointPathBytes || decoded[0] == '/' || string(decoded) == "." ||
		string(decoded) == ".." || path.Clean(string(decoded)) != string(decoded) {
		return nil, errors.New("checkpoint path is invalid")
	}
	for _, part := range splitPathBytes(decoded) {
		// Git never reports its own directory as untracked, so a `.git` component (any case, as
		// Git's own dotfile check treats it) can only be a forged bundle aiming a hook or config
		// at the restored workspace.
		if len(part) == 0 || string(part) == "." || string(part) == ".." || strings.EqualFold(string(part), ".git") {
			return nil, errors.New("checkpoint path is invalid")
		}
	}
	for _, value := range decoded {
		if value == 0 {
			return nil, errors.New("checkpoint path is invalid")
		}
	}
	return decoded, nil
}

func splitPathBytes(value []byte) [][]byte {
	var parts [][]byte
	start := 0
	for index, item := range value {
		if item == '/' {
			parts = append(parts, value[start:index])
			start = index + 1
		}
	}
	return append(parts, value[start:])
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
