package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

// Publication has always been one commit. Freeze that commit before the gate,
// not at approval time: the reviewed SHA is the SHA that will be pushed.
func freezeReviewCommit(ctx context.Context, candidate reviewScratch, op session.Operation) (string, string, error) {
	head, tree, err := ReviewGitIdentity(candidate.dir, candidate.name)
	if err != nil {
		return "", "", err
	}
	date := op.CreatedAt.UTC().Format(time.RFC3339)
	env := append(sessionCompanionGitEnv(), "GIT_AUTHOR_NAME=Coop", "GIT_AUTHOR_EMAIL=coop@localhost",
		"GIT_COMMITTER_NAME=Coop", "GIT_COMMITTER_EMAIL=coop@localhost", "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	output, truncated, err := runSessionWorkspaceGitWithEnvContext(ctx, candidate.dir, 1024, env,
		"commit-tree", tree, "-p", candidate.base, "-m", "Reviewed changes\n\nCoop-Review: "+op.ID)
	commit := strings.TrimSpace(string(output))
	if err != nil || truncated || !validSessionReviewObject(commit) {
		return "", "", errors.Join(err, errors.New("cannot freeze reviewed commit"))
	}
	if err := forkspace.GitRefCommand(ctx, candidate.dir, "update-ref", "refs/heads/"+candidate.name, commit, head).Run(); err != nil {
		return "", "", err
	}
	return commit, tree, nil
}

func (s *Service) reviewCandidatePath(operationID string) (string, error) {
	if s.stateRoot == "" || !validSessionPathComponent(operationID) {
		return "", errors.New("invalid retained review identity")
	}
	return filepath.Join(s.stateRoot, "review-candidates", operationID), nil
}

func (s *Service) retainReviewCandidate(ctx context.Context, source string, dossier ReviewDossier) error {
	destination, err := s.reviewCandidatePath(dossier.OperationID)
	if err != nil {
		return err
	}
	root := filepath.Dir(destination)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	release, err := s.admitNewFork(root, "")
	if err != nil {
		return err
	}
	defer release()
	staging := filepath.Join(root, ".staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(staging, dossier.OperationID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	// Fetch only this commit's closure. Cloning every scratch ref would retain
	// intermediate model commits that are deliberately absent from the PR.
	format := "sha1"
	if len(dossier.CandidateHead) == 64 {
		format = "sha256"
	}
	args := append(append([]string{}, forkspace.GitHardening...), "init", "--quiet", "--template=", "--object-format="+format, stage)
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = sessionCompanionGitEnv()
	if err := command.Run(); err != nil {
		return err
	}
	if err := forkspace.GitFetchPinnedContext(ctx, source, stage, dossier.CandidateHead); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"update-ref", "refs/coop/reviews/" + dossier.OperationID, dossier.CandidateHead},
		{"update-ref", "--no-deref", "HEAD", dossier.CandidateHead},
	} {
		command := forkspace.GitRefCommand(ctx, stage, args...)
		command.Env = sessionCompanionGitEnv()
		if err := command.Run(); err != nil {
			return err
		}
	}
	if _, _, err := runSessionCompanionGitContext(ctx, stage, 4096, "read-tree", dossier.CandidateHead); err != nil {
		return err
	}
	if err := forkspace.CopyLFSObjects(ctx, stage, dossier.CandidateHead, source); err != nil {
		return err
	}
	data, err := json.Marshal(dossier)
	if err != nil || len(data) > session.MaxOperationResultBytes {
		return errors.Join(err, errors.New("retained review receipt exceeds its bound"))
	}
	if err := os.WriteFile(filepath.Join(stage, "review.json"), data, 0600); err != nil {
		return err
	}
	if err := syncReviewCandidate(stage); err != nil {
		return err
	}
	// A replay must validate the existing immutable result, never replace it.
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("retained review candidate already exists")
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return errors.Join(syncReviewPath(root), syncReviewPath(s.stateRoot))
}

func (s *Service) reclaimReviewStages(ctx context.Context, limits StorageLimits, result *StorageReclaim) {
	s.reclaimOperationStages(ctx, "review-candidates", limits, result)
	s.reclaimOperationStages(ctx, "workspace-checkpoints", limits, result)
}

func (s *Service) reclaimOperationStages(ctx context.Context, kind string, limits StorageLimits, result *StorageReclaim) {
	path := filepath.Join(s.stateRoot, kind, ".staging")
	root, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		result.Problems = append(result.Problems, "cannot inspect "+kind+" staging: "+err.Error())
		return
	}
	defer root.Close()
	entries, err := root.ReadDir(limits.MaxReclaimPerPass)
	if err != nil && !errors.Is(err, io.EOF) {
		result.Problems = append(result.Problems, "cannot list "+kind+" staging: "+err.Error())
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		separator := strings.LastIndexByte(entry.Name(), '-')
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || separator < 1 {
			continue
		}
		op, err := s.store.GetOperationByID(ctx, entry.Name()[:separator])
		if err != nil {
			continue
		}
		if kind == "review-candidates" && (op.Method != "RunReview" || op.State == session.OperationReserved || op.State == session.OperationRunning) {
			continue
		}
		if kind == "workspace-checkpoints" && op.Method != "CheckpointWorkspace" && op.Method != "RestoreWorkspaceCheckpoint" {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < limits.GraceWindow {
			continue
		}
		unlock, claimed := s.tryLockOperation(op.IdempotencyKey)
		if !claimed {
			continue
		}
		err = os.RemoveAll(filepath.Join(path, entry.Name()))
		unlock()
		if err != nil {
			result.Problems = append(result.Problems, "remove interrupted "+kind+" stage: "+err.Error())
		} else {
			result.StagedPurged++
		}
	}
}

func (s *Service) retainedReviewCandidate(ctx context.Context, operationID string) (string, ReviewDossier, error) {
	directory, err := s.reviewCandidatePath(operationID)
	if err != nil {
		return "", ReviewDossier{}, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", ReviewDossier{}, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 {
		return "", ReviewDossier{}, errors.New("unsafe retained review directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", ReviewDossier{}, err
	}
	defer root.Close()
	file, err := root.Open("review.json")
	if err != nil {
		// Only an absent candidate directory permits rebuilding a review. Missing
		// files inside published custody are corruption, not a fresh attempt.
		return "", ReviewDossier{}, fmt.Errorf("retained review receipt unavailable: %v", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, session.MaxOperationResultBytes+1))
	err = errors.Join(readErr, file.Close())
	if err != nil || len(data) > session.MaxOperationResultBytes {
		return "", ReviewDossier{}, errors.Join(err, errors.New("invalid retained review receipt"))
	}
	dossier, err := decodeSessionReviewDossier(data)
	if err != nil || dossier.OperationID != operationID || !dossier.CandidateRetained {
		return "", ReviewDossier{}, errors.New("retained review identity changed")
	}
	for _, path := range []string{".git/objects/info/alternates", ".git/commondir"} {
		if _, err := root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return "", ReviewDossier{}, errors.New("retained review depends on another object store")
		}
	}
	head, tree, err := ReviewGitIdentity(directory, "refs/coop/reviews/"+operationID)
	if err != nil || head != dossier.CandidateHead || tree != dossier.CandidateTree {
		return "", ReviewDossier{}, errors.New("retained reviewed commit is unavailable")
	}
	parents, _, err := runSessionCompanionGitContext(ctx, directory, 1024, "rev-list", "--parents", "-n", "1", head)
	if err != nil || strings.TrimSpace(string(parents)) != head+" "+dossier.ParentHead {
		return "", ReviewDossier{}, errors.New("retained review parent changed")
	}
	if err := forkspace.CopyLFSObjects(ctx, directory, head); err != nil {
		return "", ReviewDossier{}, err
	}
	return directory, dossier, nil
}

func syncReviewCandidate(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("retained review contains a symlink: %s", path)
		}
		return syncReviewPath(path)
	})
}

func syncReviewPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
