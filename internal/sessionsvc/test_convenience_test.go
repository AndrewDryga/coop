package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

func ensureSessionCompanion(
	stateRoot, sessionID string,
	binding session.CompanionRepository,
) (session.CompanionRepository, error) {
	return ensureSessionCompanionContext(context.Background(), stateRoot, sessionID, binding)
}

func sessionCompanionStatus(
	binding session.CompanionRepository,
) (status []byte, truncated bool, returnErr error) {
	return sessionCompanionStatusContext(context.Background(), binding)
}

func createSessionWorkspace(repo, generatedName string) (sessionWorkspace, error) {
	if repo == "" {
		return sessionWorkspace{}, errors.New("repository is required")
	}
	if !forkspace.ValidName(generatedName) {
		return sessionWorkspace{}, fmt.Errorf("invalid session workspace name %q", generatedName)
	}
	base, err := sessionWorkspaceCommit(repo, "HEAD")
	if err != nil {
		return sessionWorkspace{}, fmt.Errorf("capture parent HEAD: %w", err)
	}
	return ensureSessionWorkspace(repo, generatedName, base)
}

func ensureSessionWorkspace(repo, generatedName, base string) (sessionWorkspace, error) {
	return ensureSessionWorkspaceContext(context.Background(), nil, repo, generatedName, base, testSessionStoreID, generatedName)
}

func inspectSessionChanges(repo, workspace, base string, maxPatchBytes int) (WorkspaceChanges, error) {
	parentHead, err := sessionWorkspaceCommit(repo, "HEAD")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve current parent HEAD: %w", err)
	}
	return inspectSessionChangesPageAtParent(repo, workspace, base, parentHead, 0, maxPatchBytes)
}

func inspectSessionChangesPage(
	repo string,
	workspace string,
	base string,
	patchOffset int64,
	patchLimit int,
) (WorkspaceChanges, error) {
	parentHead, err := sessionWorkspaceCommit(repo, "HEAD")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve current parent HEAD: %w", err)
	}
	return inspectSessionChangesPageAtParent(repo, workspace, base, parentHead, patchOffset, patchLimit)
}

func prepareSessionOutputDir(workspace, turnID string) (*sessionOutputDirectory, string, error) {
	if !filepath.IsAbs(workspace) || !validSessionHTTPPathID(turnID) {
		return nil, "", errors.New("invalid turn output identity")
	}
	return prepareSessionOutputDirAtRoot(filepath.Join(workspace, sessionOutputRoot), turnID)
}
