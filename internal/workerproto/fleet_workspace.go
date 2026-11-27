package workerproto

import (
	"archive/tar"
	"errors"
	"io"
)

// FleetWorkspaceInput is the operator's initial source over an independently
// authorized Git base. Only worker-produced checkpoints carry task or gate evidence.
type FleetWorkspaceInput struct {
	Version           int                            `json:"version"`
	JobRef            string                         `json:"job_ref"`
	RepositoryRef     string                         `json:"repository_ref"`
	BaseRevision      string                         `json:"base_revision"`
	CommittedRevision string                         `json:"committed_revision"`
	TrackedTree       string                         `json:"tracked_tree"`
	Repository        WorkspaceCheckpointBundleEntry `json:"repository"`
	UntrackedFiles    []WorkspaceCheckpointFileEntry `json:"untracked_files"`
}

func (m *FleetWorkspaceInput) Validate() error {
	if m.Version != 1 || reference(m.JobRef, 256, "job ref") != nil || reference(m.RepositoryRef, 256, "repository ref") != nil ||
		!workspaceCheckpointRevisionPattern.MatchString(m.BaseRevision) ||
		!workspaceCheckpointRevisionPattern.MatchString(m.CommittedRevision) || len(m.CommittedRevision) != len(m.BaseRevision) ||
		!workspaceCheckpointRevisionPattern.MatchString(m.TrackedTree) || len(m.TrackedTree) != len(m.BaseRevision) || m.UntrackedFiles == nil {
		return errors.New("invalid fleet workspace source identity")
	}
	if err := validateWorkspaceCheckpointBundleEntry(m.Repository, "repository.tar", false); err != nil {
		return err
	}
	total := m.Repository.ByteSize
	return validateWorkspaceCheckpointFiles(m.UntrackedFiles, "untracked", MaxWorkspaceCheckpointFiles, map[string]bool{}, &total)
}

func ReadFleetWorkspaceInput(ref BodyReference, input io.Reader, visit func(*tar.Header, io.Reader) error) (FleetWorkspaceInput, error) {
	var m FleetWorkspaceInput
	err := readWorkspaceArchive(ref, WorkspaceCheckpointVersion, input, func(document []byte) ([]archiveMember, error) {
		if err := decodeStrict(document, &m); err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		members := []archiveMember{{m.Repository, 0644}}
		for _, f := range m.UntrackedFiles {
			members = append(members, archiveMember{WorkspaceCheckpointBundleEntry{Entry: f.Entry, ByteSize: f.ByteSize, SHA256: f.SHA256}, f.Mode})
		}
		return members, nil
	}, visit)
	return m, err
}
