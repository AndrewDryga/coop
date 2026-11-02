package sessionsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/safefile"
	"github.com/AndrewDryga/coop/internal/session"
)

const companionDiscardRoot = "companion-discards"
const companionDiscardReceiptName = "receipt.json"
const companionDiscardTree = "workspace"
const companionDiscardStaged = "staged"
const companionReclaimTimeout = 30 * time.Second

// A closed primary may contain unpublished work for months. Its read-only
// companions are reproducible from frozen sources and need no such retention.
// The receipt precedes an atomic rename: interrupted deletion never leaves a
// half-deleted .git occupying the original companion's name.
type companionDiscardReceipt struct {
	Version   int                         `json:"version"`
	StoreID   string                      `json:"store_id"`
	SessionID string                      `json:"session_id"`
	JobDigest string                      `json:"job_digest"`
	Plan      sessionCompanionDiscardPlan `json:"plan"`
}

func (s *Service) reclaimClosedCompanions(ctx context.Context, sessions []session.Session, result *StorageReclaim) {
	ctx, cancel := context.WithTimeout(ctx, companionReclaimTimeout)
	defer cancel()
	budget := s.storageLimitsSnapshot().MaxReclaimPerPass
	// Failed old stages must not consume every opportunity to reclaim new copies.
	stageBudget := budget / 2
	if budget == 1 && s.storage.companionStageFirst {
		stageBudget = 1
	}
	s.storage.companionStageFirst = !s.storage.companionStageFirst
	allowance := stageBudget
	s.purgeCompanionDiscards(ctx, result, &stageBudget)
	budget -= allowance - stageBudget
	type candidate struct {
		sessionID string
		companion session.CompanionRepository
	}
	var candidates []candidate
	for _, sess := range sessions {
		if ctx.Err() != nil {
			return
		}
		if sess.State == session.SessionClosed && sess.ActiveTurnID == "" && sess.QueuedTurnCount == 0 {
			for _, companion := range sess.Companions {
				if _, err := os.Lstat(companion.Workspace); !errors.Is(err, os.ErrNotExist) {
					candidates = append(candidates, candidate{sess.ID, companion})
				}
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].sessionID+"/"+candidates[i].companion.Name < candidates[j].sessionID+"/"+candidates[j].companion.Name
	})
	if len(candidates) == 0 {
		return
	}
	start := s.storage.companionCursor % len(candidates)
	for offset := 0; offset < len(candidates) && budget > 0 && ctx.Err() == nil; offset++ {
		index := (start + offset) % len(candidates)
		s.storage.companionCursor = index + 1
		item := candidates[index]
		unlock, ok := s.tryLockSessionRuntime(item.sessionID)
		if !ok {
			continue
		}
		budget--
		err := s.reclaimClosedCompanion(ctx, item.sessionID, item.companion)
		unlock()
		if err != nil {
			result.Problems = append(result.Problems, fmt.Sprintf("reclaim closed companion %s/%s: %v", item.sessionID, item.companion.Name, err))
		} else {
			result.CompanionsReclaimed++
		}
	}
}

func (s *Service) reclaimClosedCompanion(ctx context.Context, id string, binding session.CompanionRepository) error {
	current, err := s.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if current.State != session.SessionClosed || current.Activity != session.ActivityParked ||
		current.ActiveTurnID != "" || current.QueuedTurnCount != 0 ||
		s.sessionQuarantined(id) || s.runtimeCleanupPending(id) {
		return errors.New("session is not proven closed and idle")
	}
	if err := s.validateSessionForkAuthority(ctx, current); err != nil {
		return err
	}
	if !sessionOwnsCompanion(current, binding) {
		return errors.New("companion binding changed")
	}
	expected, err := sessionCompanionWorkspace(s.stateRoot, id, binding.Name)
	if err != nil || expected != binding.Workspace {
		return errors.New("companion path is not owned by this session")
	}
	// Historical linked worktrees retain their existing explicit discard path.
	metadata, err := os.Lstat(filepath.Join(binding.Workspace, ".git"))
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 {
		return errors.New("companion does not own self-contained Git metadata")
	}
	plan, err := planSessionCompanionDiscardContext(ctx, binding)
	if err != nil {
		return err
	}
	stage, err := s.stageCompanionDiscard(ctx, current, plan)
	if err != nil {
		return err
	}
	return s.purgeCompanionDiscard(ctx, stage)
}

func sessionOwnsCompanion(owner session.Session, binding session.CompanionRepository) bool {
	for _, companion := range owner.Companions {
		if companion == binding {
			return true
		}
	}
	return false
}

func (s *Service) stageCompanionDiscard(ctx context.Context, owner session.Session, plan sessionCompanionDiscardPlan) (string, error) {
	stateRoot, rootErr := filepath.EvalSymlinks(s.stateRoot)
	parent := filepath.Join(stateRoot, companionDiscardRoot)
	if rootErr != nil {
		return "", rootErr
	}
	if err := ensureNoSymlinkPath(parent); err != nil {
		return "", err
	}
	if err := ensurePrivateDirectory(parent); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, "companion-")
	if err != nil {
		return "", err
	}
	receipt := companionDiscardReceipt{Version: 1, StoreID: s.store.ID(), SessionID: owner.ID, JobDigest: owner.JobDigest, Plan: plan}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	if err := config.WriteFileAtomic(filepath.Join(stage, companionDiscardReceiptName), encoded); err != nil {
		_ = os.Remove(stage)
		return "", err
	}
	// purgeCompanionDiscard also resumes a receipt written just before a crash
	// interrupted this rename. It rechecks identity and cleanliness first.
	if err := s.moveCompanionDiscard(ctx, stage, receipt); err != nil {
		if _, treeErr := os.Lstat(filepath.Join(stage, companionDiscardTree)); errors.Is(treeErr, os.ErrNotExist) {
			_ = os.Remove(filepath.Join(stage, companionDiscardReceiptName))
			_ = os.Remove(stage)
		}
		return "", err
	}
	root, err := os.OpenRoot(stage)
	if err != nil {
		return "", err
	}
	err = markCompanionDiscardStaged(root)
	_ = root.Close()
	if err != nil {
		return "", err
	}
	return stage, nil
}

func (s *Service) moveCompanionDiscard(ctx context.Context, stage string, receipt companionDiscardReceipt) error {
	plan := receipt.Plan
	info, err := os.Lstat(plan.Workspace)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("companion discard source was replaced")
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil || !identity.sameDirectory(plan.WorkspaceIdentity) {
		return errors.New("companion discard identity changed")
	}
	binding := session.CompanionRepository{Name: plan.Name, Repository: plan.Repo, Workspace: plan.Workspace, BaseCommit: plan.Head}
	if err := verifySessionCompanionContext(ctx, binding); err != nil {
		return err
	}
	stateRoot, err := filepath.EvalSymlinks(s.stateRoot)
	if err != nil {
		return err
	}
	if err := ensureNoSymlinkPath(plan.Workspace); err != nil {
		return err
	}
	root, err := os.OpenRoot(stateRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	source, err := filepath.Rel(stateRoot, plan.Workspace)
	if err != nil {
		return err
	}
	destination, err := filepath.Rel(stateRoot, filepath.Join(stage, companionDiscardTree))
	if err != nil {
		return err
	}
	return root.Rename(source, destination)
}

func (s *Service) purgeCompanionDiscards(ctx context.Context, result *StorageReclaim, budget *int) {
	stateRoot, rootErr := filepath.EvalSymlinks(s.stateRoot)
	parent := filepath.Join(stateRoot, companionDiscardRoot)
	if rootErr != nil {
		result.Problems = append(result.Problems, rootErr.Error())
		return
	}
	if err := ensureNoSymlinkPath(parent); err != nil {
		result.Problems = append(result.Problems, err.Error())
		return
	}
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || len(entries) > storageForkRootEntries {
		result.Problems = append(result.Problems, "companion discard inventory is unreadable or exceeds its bound")
		return
	}
	if len(entries) == 0 {
		return
	}
	start := s.storage.companionStageCursor % len(entries)
	for offset := 0; offset < len(entries) && *budget > 0 && ctx.Err() == nil; offset++ {
		index := (start + offset) % len(entries)
		s.storage.companionStageCursor = index + 1
		entry := entries[index]
		if !strings.HasPrefix(entry.Name(), "companion-") || !entry.IsDir() {
			continue
		}
		*budget--
		if err := s.purgeCompanionDiscard(ctx, filepath.Join(parent, entry.Name())); err != nil {
			result.Problems = append(result.Problems, fmt.Sprintf("finish companion discard: %v", err))
		} else {
			result.CompanionStagesPurged++
		}
	}
}

// Once staging is durable, a missing tree means deletion finished. It must
// never become fresh authority over whatever now occupies the original name.
func markCompanionDiscardStaged(root *os.Root) error {
	marker, err := root.OpenFile(companionDiscardStaged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := root.Lstat(companionDiscardStaged)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			return errors.New("companion discard stage marker is invalid")
		}
		return nil
	}
	if err != nil {
		return err
	}
	syncErr := marker.Sync()
	return errors.Join(syncErr, marker.Close())
}

func (s *Service) purgeCompanionDiscard(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureNoSymlinkPath(stage); err != nil {
		return err
	}
	pinned, err := safefile.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer pinned.Close()
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer root.Close()
	original, err := pinned.Stat()
	if err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(original, opened) {
		return errors.New("companion discard directory changed while opening")
	}
	encoded, err := safefile.ReadRegular(pinned, companionDiscardReceiptName, 64<<10)
	if err != nil {
		return err
	}
	var receipt companionDiscardReceipt
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(&struct{}{}) != io.EOF || receipt.Version != 1 || receipt.StoreID != s.store.ID() {
		return errors.New("companion discard receipt is unproven")
	}
	expected, err := sessionCompanionWorkspace(s.stateRoot, receipt.SessionID, receipt.Plan.Name)
	if err != nil || receipt.Plan.Workspace != expected || !filepath.IsAbs(receipt.Plan.Repo) || !forkspace.ValidObjectID(receipt.Plan.Head) || receipt.Plan.WorkspaceIdentity.Inode == 0 {
		return errors.New("companion discard receipt binding is invalid")
	}
	marker, markerErr := root.Lstat(companionDiscardStaged)
	staged := markerErr == nil
	if staged && (!marker.Mode().IsRegular() || marker.Size() != 0) {
		return errors.New("companion discard stage marker is invalid")
	}
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	info, err := root.Lstat(companionDiscardTree)
	if errors.Is(err, os.ErrNotExist) && !staged {
		if _, sourceErr := os.Lstat(receipt.Plan.Workspace); sourceErr == nil {
			unlock, ok := s.tryLockSessionRuntime(receipt.SessionID)
			if !ok {
				return errors.New("companion discard owner is busy")
			}
			defer unlock()
			owner, ownerErr := s.store.GetSession(ctx, receipt.SessionID)
			binding := session.CompanionRepository{Name: receipt.Plan.Name, Repository: receipt.Plan.Repo, Workspace: receipt.Plan.Workspace, BaseCommit: receipt.Plan.Head}
			if ownerErr != nil || owner.State != session.SessionClosed || owner.ActiveTurnID != "" || owner.QueuedTurnCount != 0 || owner.JobDigest != receipt.JobDigest || !sessionOwnsCompanion(owner, binding) || s.sessionQuarantined(owner.ID) || s.runtimeCleanupPending(owner.ID) {
				return errors.New("companion discard owner or binding is not proven closed")
			}
			if err := s.validateSessionForkAuthority(ctx, owner); err != nil {
				return err
			}
		}
		if err := s.moveCompanionDiscard(ctx, stage, receipt); err != nil {
			return err
		}
		info, err = root.Lstat(companionDiscardTree)
	}
	if s.testBeforeCompanionPurge != nil {
		s.testBeforeCompanionPurge(stage)
	}
	current, currentErr := os.Lstat(stage)
	if currentErr != nil || !os.SameFile(original, current) {
		return errors.New("companion discard directory was replaced")
	}
	if err == nil {
		tree, openErr := safefile.OpenDir(pinned, companionDiscardTree)
		if openErr != nil {
			return openErr
		}
		treeInfo, statErr := tree.Stat()
		identity, identityErr := sessionWorkspaceIdentityFor(info)
		if statErr != nil || !os.SameFile(info, treeInfo) || identityErr != nil || !identity.sameDirectory(receipt.Plan.WorkspaceIdentity) {
			_ = tree.Close()
			return errors.New("staged companion directory was replaced")
		}
		if err := markCompanionDiscardStaged(root); err != nil {
			_ = tree.Close()
			return err
		}
		if err := pinned.Sync(); err != nil {
			_ = tree.Close()
			return err
		}
		// Keep the exact tree handle open until unlink finishes.
		live, liveErr := root.Lstat(companionDiscardTree)
		if liveErr != nil || !os.SameFile(treeInfo, live) {
			_ = tree.Close()
			return errors.New("staged companion changed before removal")
		}
		if err := ctx.Err(); err != nil {
			_ = tree.Close()
			return err
		}
		removeErr := root.RemoveAll(companionDiscardTree)
		_ = tree.Close()
		if removeErr != nil {
			return removeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := root.Lstat(companionDiscardTree); !errors.Is(err, os.ErrNotExist) {
		return errors.New("staged companion bytes remain")
	}
	entries, err := pinned.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != companionDiscardReceiptName && entry.Name() != companionDiscardStaged {
			return errors.New("companion discard directory contains unknown data")
		}
	}
	// Preserve the staged marker until receipt removal: a crash then leaves
	// only bounded metadata, never fresh deletion authority over the old path.
	if err := root.Remove(companionDiscardReceiptName); err != nil {
		return err
	}
	if _, err := root.Lstat(companionDiscardStaged); err == nil {
		if err := root.Remove(companionDiscardStaged); err != nil {
			return err
		}
	}
	current, currentErr = os.Lstat(stage)
	if currentErr != nil || !os.SameFile(original, current) {
		return errors.New("companion discard directory changed before retirement")
	}
	return os.Remove(stage)
}
