package tasks

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	pendingReviewVersion      = 1
	pendingReviewFileSuffix   = ".pending-review.json"
	pendingReviewDoneSuffix   = ".pending-review-reviewed.json"
	pendingReviewMarkerID     = "pending-review-workspace"
	pendingReviewMarkerFile   = ".pending-review-workspace"
	pendingReviewSigningID    = "pending-review-signing"
	pendingReviewSigningFile  = ".pending-review-signing.json"
	pendingReviewFileLimit    = 256 << 10
	pendingReviewScanLimit    = 4096
	pendingReviewSigningLimit = 1024
)

type PendingReviewPhase string

const (
	PendingReviewSignoff  PendingReviewPhase = "signoff_pending"
	PendingReviewVerify   PendingReviewPhase = "verify_pending"
	PendingReviewReopened PendingReviewPhase = "reopened"
)

// PendingReviewStage pins the resolved targets and policy of one review stage. Targets use the
// ordinary provider[:model][/effort][@account] spelling so the loop package can reconstruct the
// exact ladder without importing its orchestration types into task authority.
type PendingReviewStage struct {
	Targets []string `json:"targets"`
	Prompt  string   `json:"prompt,omitempty"`
	Writes  string   `json:"writes,omitempty"`
}

type PendingReviewQueue struct {
	Root string `json:"root"`
	ID   string `json:"id"`
}

// PendingReviewPlan is immutable cohort context. A resumed review uses this plan rather than
// silently applying a later loop.yaml to work accepted under a different reviewer contract.
type PendingReviewPlan struct {
	Version       int                  `json:"version"`
	CohortID      string               `json:"cohort_id"`
	Workspace     string               `json:"workspace"`
	Branch        string               `json:"branch"`
	Queues        []PendingReviewQueue `json:"queues"`
	BaseHead      string               `json:"base_head"`
	ConfigDigest  string               `json:"config_digest,omitempty"`
	Continue      string               `json:"continue"`
	Signoff       PendingReviewStage   `json:"signoff"`
	SignoffRounds int                  `json:"signoff_rounds"`
	VerifyEnabled bool                 `json:"verify_enabled,omitempty"`
	Verify        PendingReviewStage   `json:"verify,omitempty"`
	MCPDisabled   bool                 `json:"mcp_disabled,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
}

// PendingReviewBinding retains exact raw and signature-independent history evidence. Binding is
// the same bounded semantic shape used by audit-reopen authority; a signing rewrite may replace
// Head only when Subject and every descendant remain identical.
type PendingReviewBinding struct {
	Head    string              `json:"head"`
	Raw     []string            `json:"raw"`
	Subject AuditReopenCommit   `json:"subject"`
	History []AuditReopenCommit `json:"history"`
}

type PendingReviewRecord struct {
	Version      int                    `json:"version"`
	Plan         PendingReviewPlan      `json:"plan"`
	Task         TaskInstance           `json:"task"`
	Fingerprint  CompletionFingerprint  `json:"fingerprint"`
	Binding      PendingReviewBinding   `json:"binding"`
	Phase        PendingReviewPhase     `json:"phase"`
	Round        int                    `json:"round"`
	RoundStarted bool                   `json:"round_started,omitempty"`
	Prepared     bool                   `json:"prepared,omitempty"`
	Previous     *PendingReviewPrevious `json:"previous,omitempty"`
	UpdatedAt    time.Time              `json:"updated_at"`
}

// PendingReviewPrevious is the active debt displaced by a prepared recompletion. If publication
// dies before the matching completion receipt, recovery restores this exact state rather than
// losing the reopen obligation or treating the unaccepted generation as reviewed.
type PendingReviewPrevious struct {
	Task         TaskInstance          `json:"task"`
	Fingerprint  CompletionFingerprint `json:"fingerprint"`
	Binding      PendingReviewBinding  `json:"binding"`
	Phase        PendingReviewPhase    `json:"phase"`
	Round        int                   `json:"round"`
	RoundStarted bool                  `json:"round_started,omitempty"`
}

type PendingReviewCohort struct {
	Plan     PendingReviewPlan
	Subjects []PendingReviewRecord
}

type pendingReviewReviewed struct {
	Version     int                   `json:"version"`
	CohortID    string                `json:"cohort_id"`
	Task        TaskInstance          `json:"task"`
	Fingerprint CompletionFingerprint `json:"fingerprint"`
	Binding     PendingReviewBinding  `json:"binding"`
	ReviewedAt  time.Time             `json:"reviewed_at"`
}

type pendingReviewSigningStep struct {
	Branch     string    `json:"branch"`
	OldHead    string    `json:"old_head"`
	NewHead    string    `json:"new_head"`
	OldCommits []string  `json:"old_commits"`
	NewCommits []string  `json:"new_commits"`
	CreatedAt  time.Time `json:"created_at"`
}

type pendingReviewSigningJournal struct {
	Version   int                        `json:"version"`
	Workspace string                     `json:"workspace"`
	Steps     []pendingReviewSigningStep `json:"steps"`
	UpdatedAt time.Time                  `json:"updated_at"`
}

type pendingReviewWorkspaceMarker struct {
	Version   int       `json:"version"`
	Workspace string    `json:"workspace"`
	UpdatedAt time.Time `json:"updated_at"`
}

func canonicalPendingReviewWorkspace(repo string) (string, error) {
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("pending-review workspace %q is not a real directory", repo)
	}
	return filepath.Clean(abs), nil
}

// NewPendingReviewPlan captures one run's immutable review context before its first completion.
func NewPendingReviewPlan(repo string, hosts []string, baseHead, configDigest, continueCommand string, signoff PendingReviewStage, signoffRounds int, verifyEnabled bool, verify PendingReviewStage, mcpDisabled bool) (PendingReviewPlan, error) {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return PendingReviewPlan{}, err
	}
	if !validAuditReopenHead(baseHead) || signoffRounds < 1 || len(signoff.Targets) == 0 {
		return PendingReviewPlan{}, errors.New("invalid pending-review run context")
	}
	if verifyEnabled && len(verify.Targets) == 0 {
		return PendingReviewPlan{}, errors.New("enabled pending verification has no target")
	}
	cohortRaw := make([]byte, 16)
	if _, err := rand.Read(cohortRaw); err != nil {
		return PendingReviewPlan{}, err
	}
	branch := gitOut(repo, "symbolic-ref", "--quiet", "HEAD")
	if !strings.HasPrefix(branch, "refs/heads/") {
		return PendingReviewPlan{}, errors.New("pending-review workspace has no checked-out branch")
	}
	plan := PendingReviewPlan{
		Version: pendingReviewVersion, CohortID: hex.EncodeToString(cohortRaw), Workspace: workspace,
		Branch: branch, BaseHead: baseHead, ConfigDigest: configDigest, Continue: continueCommand,
		Signoff: clonePendingReviewStage(signoff), SignoffRounds: signoffRounds,
		VerifyEnabled: verifyEnabled, Verify: clonePendingReviewStage(verify), MCPDisabled: mcpDisabled,
		CreatedAt: time.Now().UTC(),
	}
	seen := map[string]bool{}
	for _, host := range hosts {
		canonical, err := canonicalTaskRoot(host)
		if err != nil {
			return PendingReviewPlan{}, err
		}
		if seen[canonical] {
			return PendingReviewPlan{}, fmt.Errorf("pending-review queue %q is repeated", canonical)
		}
		seen[canonical] = true
		queueID, err := EnsureQueueIdentity(canonical)
		if err != nil {
			return PendingReviewPlan{}, fmt.Errorf("pending-review queue identity: %w", err)
		}
		plan.Queues = append(plan.Queues, PendingReviewQueue{Root: canonical, ID: queueID})
	}
	if len(plan.Queues) == 0 {
		return PendingReviewPlan{}, errors.New("pending-review plan has no task queue")
	}
	return plan, nil
}

func clonePendingReviewStage(stage PendingReviewStage) PendingReviewStage {
	stage.Targets = slices.Clone(stage.Targets)
	return stage
}

func clonePendingReviewPlan(plan PendingReviewPlan) PendingReviewPlan {
	plan.Queues = slices.Clone(plan.Queues)
	plan.Signoff = clonePendingReviewStage(plan.Signoff)
	plan.Verify = clonePendingReviewStage(plan.Verify)
	return plan
}

func pendingReviewRecordName(root, id string) (string, error) {
	prefix, err := pendingReviewRecordPrefix(root)
	if err != nil {
		return "", err
	}
	key, err := LeaseAuthorityKey(root, id)
	return prefix + key + pendingReviewFileSuffix, err
}

func pendingReviewReviewedName(root, id string) (string, error) {
	prefix, err := pendingReviewRecordPrefix(root)
	if err != nil {
		return "", err
	}
	key, err := LeaseAuthorityKey(root, id)
	return prefix + key + pendingReviewDoneSuffix, err
}

func pendingReviewRecordPrefix(root string) (string, error) {
	key, err := LeaseAuthorityKey(root, "pending-review-queue")
	return key + "-", err
}

func pendingReviewWorkspaceFile(repo, id, suffix string) (string, error) {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return "", err
	}
	key, err := LeaseAuthorityKey(workspace, id)
	if err != nil {
		return "", err
	}
	return key + suffix, nil
}

func markPendingReviewWorkspace(repo string) error {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return err
	}
	name, err := pendingReviewWorkspaceFile(workspace, pendingReviewMarkerID, pendingReviewMarkerFile)
	if err != nil {
		return err
	}
	marker := pendingReviewWorkspaceMarker{Version: pendingReviewVersion, Workspace: workspace, UpdatedAt: time.Now().UTC()}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	return AtomicWriteTaskFile(registry, name, append(data, '\n'))
}

func pendingReviewWorkspaceMarked(repo string) (bool, error) {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return false, err
	}
	name, err := pendingReviewWorkspaceFile(workspace, pendingReviewMarkerID, pendingReviewMarkerFile)
	if err != nil {
		return false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return false, err
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(data) > pendingReviewFileLimit {
		return false, fmt.Errorf("pending-review workspace marker exceeds %d bytes", pendingReviewFileLimit)
	}
	var marker pendingReviewWorkspaceMarker
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&marker); err != nil {
		return false, fmt.Errorf("decode pending-review workspace marker: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("pending-review workspace marker contains multiple JSON values")
		}
		return false, err
	}
	if marker.Version != pendingReviewVersion || marker.Workspace != workspace || marker.UpdatedAt.IsZero() {
		return false, errors.New("invalid pending-review workspace marker")
	}
	return true, nil
}

func pendingReviewSigningJournalName(repo string) (string, error) {
	return pendingReviewWorkspaceFile(repo, pendingReviewSigningID, pendingReviewSigningFile)
}

func readPendingReviewSigningJournal(repo string) (pendingReviewSigningJournal, bool, error) {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return pendingReviewSigningJournal{}, false, err
	}
	name, err := pendingReviewSigningJournalName(workspace)
	if err != nil {
		return pendingReviewSigningJournal{}, false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return pendingReviewSigningJournal{}, false, err
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, name)
	if errors.Is(err, os.ErrNotExist) {
		return pendingReviewSigningJournal{}, false, nil
	}
	if err != nil {
		return pendingReviewSigningJournal{}, false, err
	}
	if len(data) > pendingReviewFileLimit {
		return pendingReviewSigningJournal{}, false, fmt.Errorf("pending-review signing journal exceeds %d bytes", pendingReviewFileLimit)
	}
	var journal pendingReviewSigningJournal
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&journal); err != nil {
		return pendingReviewSigningJournal{}, false, fmt.Errorf("decode pending-review signing journal: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return pendingReviewSigningJournal{}, false, errors.New("pending-review signing journal contains multiple JSON values")
		}
		return pendingReviewSigningJournal{}, false, err
	}
	if journal.Version != pendingReviewVersion || journal.Workspace != workspace || journal.UpdatedAt.IsZero() || len(journal.Steps) > pendingReviewSigningLimit {
		return pendingReviewSigningJournal{}, false, errors.New("invalid pending-review signing journal")
	}
	for _, step := range journal.Steps {
		if !strings.HasPrefix(step.Branch, "refs/heads/") || !validAuditReopenHead(step.OldHead) ||
			!validAuditReopenHead(step.NewHead) || step.OldHead == step.NewHead || step.CreatedAt.IsZero() ||
			len(step.OldCommits) == 0 || len(step.OldCommits) != len(step.NewCommits) ||
			step.OldCommits[len(step.OldCommits)-1] != step.OldHead || step.NewCommits[len(step.NewCommits)-1] != step.NewHead {
			return pendingReviewSigningJournal{}, false, errors.New("invalid pending-review signing journal step")
		}
		for i := range step.OldCommits {
			if !validAuditReopenHead(step.OldCommits[i]) || !validAuditReopenHead(step.NewCommits[i]) {
				return pendingReviewSigningJournal{}, false, errors.New("invalid pending-review signing commit mapping")
			}
		}
	}
	return journal, true, nil
}

func writePendingReviewSigningJournal(repo string, journal pendingReviewSigningJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(data)+1 > pendingReviewFileLimit {
		return fmt.Errorf("pending-review signing journal exceeds %d bytes", pendingReviewFileLimit)
	}
	name, err := pendingReviewSigningJournalName(repo)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	return AtomicWriteTaskFile(registry, name, append(data, '\n'))
}

func removePendingReviewWorkspaceFiles(repo string) error {
	marker, err := pendingReviewWorkspaceFile(repo, pendingReviewMarkerID, pendingReviewMarkerFile)
	if err != nil {
		return err
	}
	journal, err := pendingReviewSigningJournalName(repo)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	var result error
	for _, name := range []string{journal, marker} {
		if removeErr := registry.Remove(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, removeErr)
		}
	}
	return result
}

// RecordPendingReviewSigning writes the exact old-to-new commit map before Coop advances the
// branch. It is a no-op unless this workspace has pending review debt. A restart can therefore
// distinguish Coop's own signing rewrite from an arbitrary history edit without trusting tree
// equality alone.
func RecordPendingReviewSigning(repo, branch, oldHead, newHead string, oldCommits, newCommits []string) error {
	marked, err := pendingReviewWorkspaceMarked(repo)
	if err != nil || !marked {
		return err
	}
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(branch, "refs/heads/") || gitOut(repo, "symbolic-ref", "--quiet", "HEAD") != branch ||
		!validAuditReopenHead(oldHead) || !validAuditReopenHead(newHead) || oldHead == newHead ||
		gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}") != oldHead || len(oldCommits) == 0 ||
		len(oldCommits) != len(newCommits) || oldCommits[len(oldCommits)-1] != oldHead ||
		newCommits[len(newCommits)-1] != newHead {
		return errors.New("invalid pending-review signing transition")
	}
	for i := range oldCommits {
		if !validAuditReopenHead(oldCommits[i]) || !validAuditReopenHead(newCommits[i]) {
			return errors.New("invalid pending-review signing commit mapping")
		}
		oldTree := gitOut(repo, "show", "-s", "--format=%T", oldCommits[i])
		newTree := gitOut(repo, "show", "-s", "--format=%T", newCommits[i])
		if oldTree == "" || oldTree != newTree {
			return fmt.Errorf("pending-review signing changed commit %d's tree", i+1)
		}
	}
	journal, ok, err := readPendingReviewSigningJournal(workspace)
	if err != nil {
		return err
	}
	if !ok {
		journal = pendingReviewSigningJournal{Version: pendingReviewVersion, Workspace: workspace}
	}
	if len(journal.Steps) >= pendingReviewSigningLimit {
		return fmt.Errorf("pending-review signing journal reached %d transitions; finish final review before signing more history", pendingReviewSigningLimit)
	}
	journal.Steps = append(journal.Steps, pendingReviewSigningStep{
		Branch: branch, OldHead: oldHead, NewHead: newHead,
		OldCommits: slices.Clone(oldCommits), NewCommits: slices.Clone(newCommits), CreatedAt: time.Now().UTC(),
	})
	journal.UpdatedAt = time.Now().UTC()
	return writePendingReviewSigningJournal(workspace, journal)
}

func pendingReviewIsAncestor(repo, ancestor, head string) bool {
	return ancestor == head || gitOut(repo, "merge-base", ancestor, head) == ancestor
}

func rebindPendingReviewFromSigningJournal(repo string, record PendingReviewRecord) (PendingReviewRecord, bool, error) {
	journal, ok, err := readPendingReviewSigningJournal(repo)
	if err != nil || !ok {
		return record, false, err
	}
	currentHead := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	binding := record.Binding
	binding.Raw = slices.Clone(binding.Raw)
	rebound := false
	for _, step := range journal.Steps {
		if step.Branch != record.Plan.Branch || !pendingReviewIsAncestor(repo, step.NewHead, currentHead) {
			continue
		}
		headIndex := slices.Index(step.OldCommits, binding.Head)
		if headIndex < 0 {
			continue
		}
		mapped := make([]string, len(binding.Raw))
		for i, raw := range binding.Raw {
			index := slices.Index(step.OldCommits, raw)
			if index < 0 {
				return record, false, fmt.Errorf("pending-review signing journal does not map the full binding for task %s", record.Task.Ref.ID)
			}
			mapped[i] = step.NewCommits[index]
		}
		binding.Head = step.NewCommits[headIndex]
		binding.Raw = mapped
		rebound = true
	}
	if !rebound {
		return record, false, nil
	}
	if !pendingReviewRawBindingValid(repo, binding) ||
		!AuditReopenCurrentValid(repo, currentHead, record.Task.Ref.ID, bindingAsAuditRecord(record.Task.Ref.ID, binding)) {
		return record, false, fmt.Errorf("pending-review signing journal does not validate current history for task %s", record.Task.Ref.ID)
	}
	record.Binding = binding
	record.UpdatedAt = time.Now().UTC()
	return record, true, nil
}

func pendingReviewWorkspaceHasRecords(repo string, queueRoots []string) (bool, error) {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return false, err
	}
	defer registry.Close()
	directory, err := registry.Open(".")
	if err != nil {
		return false, err
	}
	defer directory.Close()
	prefixes := make([]string, 0, len(queueRoots))
	for _, root := range queueRoots {
		prefix, prefixErr := pendingReviewRecordPrefix(root)
		if prefixErr != nil {
			return false, prefixErr
		}
		prefixes = append(prefixes, prefix)
	}
	for {
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), pendingReviewFileSuffix) {
				continue
			}
			data, fileErr := ReadTaskMetadataFile(registry, entry.Name())
			if fileErr != nil {
				if slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(entry.Name(), prefix) }) {
					return true, nil // retain the marker until selected-queue evidence is repaired
				}
				continue
			}
			record, decodeErr := decodePendingReviewRecord(data)
			if decodeErr != nil {
				if slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(entry.Name(), prefix) }) {
					return true, nil
				}
				continue
			}
			if record.Plan.Workspace == workspace {
				return true, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
	}
}

func clearPendingReviewWorkspaceIfEmpty(repo string, queueRoots []string) error {
	hasRecords, err := pendingReviewWorkspaceHasRecords(repo, queueRoots)
	if err != nil || hasRecords {
		return err
	}
	return removePendingReviewWorkspaceFiles(repo)
}

func decodePendingReviewRecord(data []byte) (PendingReviewRecord, error) {
	var record PendingReviewRecord
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), pendingReviewFileLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		return PendingReviewRecord{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return PendingReviewRecord{}, errors.New("pending-review record contains multiple JSON values")
		}
		return PendingReviewRecord{}, err
	}
	return record, nil
}

func validatePendingReviewPlan(plan PendingReviewPlan) error {
	if plan.Version != pendingReviewVersion || !durableIdentityRE.MatchString(plan.CohortID) ||
		plan.Workspace == "" || !filepath.IsAbs(plan.Workspace) || filepath.Clean(plan.Workspace) != plan.Workspace ||
		!strings.HasPrefix(plan.Branch, "refs/heads/") || len(plan.Queues) == 0 || strings.TrimSpace(plan.Continue) == "" ||
		!validAuditReopenHead(plan.BaseHead) || plan.SignoffRounds < 1 || len(plan.Signoff.Targets) == 0 ||
		plan.CreatedAt.IsZero() {
		return errors.New("invalid pending-review plan")
	}
	if plan.VerifyEnabled && len(plan.Verify.Targets) == 0 {
		return errors.New("enabled pending-review verification has no targets")
	}
	for _, writes := range []string{plan.Signoff.Writes, plan.Verify.Writes} {
		if writes != "" && writes != "tasks" && writes != "repo" {
			return fmt.Errorf("invalid pending-review write policy %q", writes)
		}
	}
	seenRoots, seenIDs := map[string]bool{}, map[string]bool{}
	for _, queue := range plan.Queues {
		if queue.Root == "" || !filepath.IsAbs(queue.Root) || filepath.Clean(queue.Root) != queue.Root ||
			!durableIdentityRE.MatchString(queue.ID) || seenRoots[queue.Root] || seenIDs[queue.ID] {
			return errors.New("invalid pending-review queue identity")
		}
		seenRoots[queue.Root], seenIDs[queue.ID] = true, true
	}
	return nil
}

func validatePendingReviewRecord(record PendingReviewRecord) error {
	if record.Version != pendingReviewVersion || record.Round < 0 || record.UpdatedAt.IsZero() {
		return errors.New("invalid pending-review record")
	}
	if err := validatePendingReviewPlan(record.Plan); err != nil {
		return err
	}
	if err := validatePendingReviewSubject(record.Task, record.Fingerprint, record.Binding); err != nil {
		return err
	}
	switch record.Phase {
	case PendingReviewSignoff, PendingReviewVerify, PendingReviewReopened:
	default:
		return fmt.Errorf("invalid pending-review phase %q", record.Phase)
	}
	if record.Prepared && record.Phase != PendingReviewSignoff {
		return errors.New("prepared pending-review state is not awaiting signoff")
	}
	if record.Phase == PendingReviewVerify && !record.Plan.VerifyEnabled {
		return errors.New("pending-review verification is not enabled by its plan")
	}
	if record.RoundStarted && (record.Phase != PendingReviewSignoff || record.Round < 1) {
		return errors.New("invalid pending-review round state")
	}
	if record.Previous != nil {
		previous := record.Previous
		if !record.Prepared || previous.Task.Ref != record.Task.Ref || previous.Round < 0 {
			return errors.New("invalid previous pending-review state")
		}
		if err := validatePendingReviewSubject(previous.Task, previous.Fingerprint, previous.Binding); err != nil {
			return fmt.Errorf("invalid previous pending-review state: %w", err)
		}
		switch previous.Phase {
		case PendingReviewSignoff, PendingReviewVerify, PendingReviewReopened:
		default:
			return errors.New("invalid previous pending-review phase")
		}
		if previous.Phase == PendingReviewVerify && !record.Plan.VerifyEnabled {
			return errors.New("previous pending-review verification is not enabled by its plan")
		}
		if previous.RoundStarted && (previous.Phase != PendingReviewSignoff || previous.Round < 1) {
			return errors.New("invalid previous pending-review round state")
		}
	}
	return nil
}

func validatePendingReviewSubject(task TaskInstance, fingerprint CompletionFingerprint, binding PendingReviewBinding) error {
	if task.Ref.ID == "" || !durableIdentityRE.MatchString(task.Ref.QueueID) ||
		!durableIdentityRE.MatchString(task.Ref.TaskID) || task.Generation.Device == 0 ||
		task.Generation.Inode == 0 || fingerprint.Device != task.Generation.Device ||
		fingerprint.Inode != task.Generation.Inode || !durableIdentityRE.MatchString(fingerprint.Receipt) ||
		fingerprint.ReceiptBusy || !validAuditReopenHead(fingerprint.Tree) ||
		!validAuditReopenHead(binding.Head) || len(binding.Raw) != len(binding.History)+1 ||
		len(binding.Raw) == 0 || binding.Raw[len(binding.Raw)-1] != binding.Head {
		return errors.New("invalid pending-review subject")
	}
	for _, commit := range binding.Raw {
		if !validAuditReopenHead(commit) {
			return errors.New("invalid raw pending-review binding")
		}
	}
	if err := validateAuditReopenRecord(bindingAsAuditRecord(task.Ref.ID, binding), task.Ref.ID); err != nil {
		return fmt.Errorf("invalid semantic pending-review binding: %w", err)
	}
	return nil
}

func readPendingReviewRecord(root, id string) (PendingReviewRecord, bool, error) {
	name, err := pendingReviewRecordName(root, id)
	if err != nil {
		return PendingReviewRecord{}, false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return PendingReviewRecord{}, false, err
	}
	defer registry.Close()
	before, err := registry.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return PendingReviewRecord{}, false, nil
	}
	if err != nil {
		return PendingReviewRecord{}, false, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || !ok || stat.Nlink != 1 || before.Size() > pendingReviewFileLimit {
		return PendingReviewRecord{}, false, fmt.Errorf("pending-review record %q is not a bounded single-link regular file", name)
	}
	file, err := registry.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return PendingReviewRecord{}, false, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		if err != nil {
			return PendingReviewRecord{}, false, err
		}
		return PendingReviewRecord{}, false, fmt.Errorf("pending-review record %q changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, pendingReviewFileLimit+1))
	if err != nil {
		return PendingReviewRecord{}, false, err
	}
	if len(data) > pendingReviewFileLimit {
		return PendingReviewRecord{}, false, fmt.Errorf("pending-review record %q exceeds %d bytes", name, pendingReviewFileLimit)
	}
	record, err := decodePendingReviewRecord(data)
	if err != nil {
		return PendingReviewRecord{}, false, fmt.Errorf("decode pending-review record %q: %w", name, err)
	}
	if err := validatePendingReviewRecord(record); err != nil {
		return PendingReviewRecord{}, false, fmt.Errorf("validate pending-review record %q: %w", name, err)
	}
	return record, true, nil
}

func writePendingReviewRecord(root string, record PendingReviewRecord) error {
	if err := validatePendingReviewRecord(record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data)+1 > pendingReviewFileLimit {
		return fmt.Errorf("pending-review record exceeds %d bytes", pendingReviewFileLimit)
	}
	name, err := pendingReviewRecordName(root, record.Task.Ref.ID)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	return AtomicWriteTaskFile(registry, name, append(data, '\n'))
}

func removePendingReviewRecord(root, id string) error {
	name, err := pendingReviewRecordName(root, id)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	err = registry.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func readPendingReviewReviewed(root, id string) (pendingReviewReviewed, bool, error) {
	name, err := pendingReviewReviewedName(root, id)
	if err != nil {
		return pendingReviewReviewed{}, false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return pendingReviewReviewed{}, false, err
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, name)
	if errors.Is(err, os.ErrNotExist) {
		return pendingReviewReviewed{}, false, nil
	}
	if err != nil {
		return pendingReviewReviewed{}, false, err
	}
	if len(data) > pendingReviewFileLimit {
		return pendingReviewReviewed{}, false, fmt.Errorf("pending-review reviewed receipt exceeds %d bytes", pendingReviewFileLimit)
	}
	var reviewed pendingReviewReviewed
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), pendingReviewFileLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reviewed); err != nil {
		return pendingReviewReviewed{}, false, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return pendingReviewReviewed{}, false, errors.New("pending-review reviewed receipt contains multiple JSON values")
		}
		return pendingReviewReviewed{}, false, err
	}
	if reviewed.Version != pendingReviewVersion || !durableIdentityRE.MatchString(reviewed.CohortID) ||
		reviewed.Task.Ref.ID != id || reviewed.ReviewedAt.IsZero() {
		return pendingReviewReviewed{}, false, errors.New("invalid pending-review reviewed receipt")
	}
	if err := validatePendingReviewSubject(reviewed.Task, reviewed.Fingerprint, reviewed.Binding); err != nil {
		return pendingReviewReviewed{}, false, fmt.Errorf("invalid pending-review reviewed receipt: %w", err)
	}
	return reviewed, true, nil
}

func writePendingReviewReviewed(root string, record PendingReviewRecord) error {
	reviewed := pendingReviewReviewed{
		Version: pendingReviewVersion, CohortID: record.Plan.CohortID, Task: record.Task,
		Fingerprint: record.Fingerprint, Binding: record.Binding, ReviewedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(reviewed)
	if err != nil {
		return err
	}
	name, err := pendingReviewReviewedName(root, record.Task.Ref.ID)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	return AtomicWriteTaskFile(registry, name, append(data, '\n'))
}

func removePendingReviewReviewed(root, id string) error {
	name, err := pendingReviewReviewedName(root, id)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	err = registry.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func capturePendingReviewBinding(repo, id string) (PendingReviewBinding, error) {
	record, err := CaptureAuditReopen(repo, id)
	if err != nil {
		return PendingReviewBinding{}, err
	}
	raw, err := rawAuditHistoryCount(repo, record.BaselineHead, len(record.History)+1)
	if err != nil {
		return PendingReviewBinding{}, err
	}
	commits := make([]string, len(raw))
	for i := range raw {
		commits[i] = raw[i].sha
	}
	return PendingReviewBinding{Head: record.BaselineHead, Raw: commits, Subject: record.Subject, History: slices.Clone(record.History)}, nil
}

func bindingAsAuditRecord(id string, binding PendingReviewBinding) AuditReopenRecord {
	return AuditReopenRecord{
		Version: auditReopenVersion, Generation: strings.Repeat("0", 32), TaskID: id,
		BaselineHead: binding.Head, Subject: binding.Subject, History: slices.Clone(binding.History),
	}
}

func pendingReviewBindingExtendsSemantics(prior, current PendingReviewBinding) bool {
	return prior.Subject == current.Subject && len(current.History) >= len(prior.History) &&
		slices.Equal(prior.History, current.History[:len(prior.History)])
}

func pendingReviewBindingEqual(a, b PendingReviewBinding) bool {
	return a.Head == b.Head && a.Subject == b.Subject && slices.Equal(a.Raw, b.Raw) && slices.Equal(a.History, b.History)
}

func pendingReviewRecordMatchesExpected(current, expected PendingReviewRecord) bool {
	return current.Version == expected.Version && pendingReviewPlanEqual(current.Plan, expected.Plan) &&
		sameTaskInstance(current.Task, expected.Task) && current.Fingerprint == expected.Fingerprint &&
		pendingReviewBindingEqual(current.Binding, expected.Binding) && current.Phase == expected.Phase &&
		current.Round == expected.Round && current.RoundStarted == expected.RoundStarted &&
		current.Prepared == expected.Prepared
}

// reconcileReviewedPendingReview completes the crash-idempotent half of clear: the reviewed
// generation receipt is published first, so a process death before active-record removal must not
// launch the same reviewer again. Exact generation/fingerprint/binding equality is required under
// the task lock; an older tombstone beside a later authorized generation is left alone.
func reconcileReviewedPendingReview(root, id string) (bool, error) {
	authority, err := lockLeaseAuthority(root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return false, err
	}
	finish := func(reconciled bool, operationErr error) (bool, error) {
		return reconciled, errors.Join(operationErr, unlockLeaseFile(authority))
	}
	record, active, err := readPendingReviewRecord(root, id)
	if err != nil || !active {
		return finish(false, err)
	}
	reviewed, ok, err := readPendingReviewReviewed(root, id)
	if err != nil || !ok {
		return finish(false, err)
	}
	if reviewed.CohortID != record.Plan.CohortID || !sameTaskInstance(reviewed.Task, record.Task) || reviewed.Fingerprint != record.Fingerprint ||
		!pendingReviewBindingEqual(reviewed.Binding, record.Binding) {
		return finish(false, nil)
	}
	if err := removePendingReviewRecord(root, id); err != nil {
		return finish(false, err)
	}
	return finish(true, nil)
}

func pendingReviewRawBindingValid(repo string, binding PendingReviewBinding) bool {
	raw, err := rawAuditHistoryCount(repo, binding.Head, len(binding.Raw))
	if err != nil || len(raw) != len(binding.Raw) {
		return false
	}
	for i := range raw {
		if raw[i].sha != binding.Raw[i] {
			return false
		}
	}
	return true
}

// MarkCompletedForReview publishes pending debt and the ordinary completion receipt under the
// same task authority. The prepared record comes first: a crash before the receipt is recoverable
// as unaccepted work; a crash after it can promote the exact nonce on startup.
func (l *TaskLease) MarkCompletedForReview(repo string, task Item, plan PendingReviewPlan) error {
	if task.ID != l.id || task.State != StateDone || filepath.Dir(filepath.Dir(task.Dir)) != l.root {
		return errors.New("pending-review completion does not match its task lease")
	}
	if err := validatePendingReviewPlan(plan); err != nil {
		return err
	}
	// Legacy loop tasks may not have entered a fork or interactive-claim path yet. Establish their
	// durable identity here while this completion still owns the task authority lock; the record
	// must never fall back to the readable folder slug as identity.
	instance, err := EnsureTaskInstance(l.root, task)
	if err != nil {
		return fmt.Errorf("read pending-review task identity: %w", err)
	}
	prior, hadPrior, err := readPendingReviewRecord(l.root, task.ID)
	if err != nil {
		return err
	} else if hadPrior && prior.Phase != PendingReviewReopened && !prior.Prepared {
		return fmt.Errorf("task %s already has pending final review", task.ID)
	} else if hadPrior && !sameTaskInstance(prior.Task, instance) {
		return fmt.Errorf("task %s pending-review identity changed", task.ID)
	}
	binding, err := capturePendingReviewBinding(repo, task.ID)
	if err != nil {
		return fmt.Errorf("capture pending-review task binding: %w", err)
	}
	receipt, err := completionReceiptFor(task.Dir)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	receipt.Nonce = hex.EncodeToString(nonce)
	if l.Reopen != nil {
		receipt.AuditReopenGeneration = l.Reopen.Generation
	}
	fingerprint, err := completionFingerprintWithReceipt(task, receipt.Nonce, false)
	if err != nil {
		return err
	}
	record := PendingReviewRecord{
		Version: pendingReviewVersion, Plan: plan, Task: instance, Fingerprint: fingerprint,
		Binding: binding, Phase: PendingReviewSignoff, Prepared: true, UpdatedAt: time.Now().UTC(),
	}
	if hadPrior {
		record.Plan = prior.Plan
		record.Round = prior.Round
		record.Previous = &PendingReviewPrevious{
			Task: prior.Task, Fingerprint: prior.Fingerprint, Binding: prior.Binding,
			Phase: prior.Phase, Round: prior.Round, RoundStarted: prior.RoundStarted,
		}
	}
	if err := writePendingReviewRecord(l.root, record); err != nil {
		return err
	}
	if err := markPendingReviewWorkspace(repo); err != nil {
		return errors.Join(err, rollbackPreparedPendingReview(l.root, record))
	}
	if err := writeLeaseCompletionReceiptValue(l.authority, receipt); err != nil {
		return errors.Join(err, rollbackPreparedPendingReview(l.root, record))
	}
	if err := activatePreparedPendingReview(l.root, record, writePendingReviewRecord); err != nil {
		return errors.Join(err, clearLeaseCompletionReceipt(l.authority))
	}
	return nil
}

func activatePreparedPendingReview(root string, prepared PendingReviewRecord, write func(string, PendingReviewRecord) error) error {
	active := prepared
	active.Prepared = false
	active.Previous = nil
	active.UpdatedAt = time.Now().UTC()
	if err := write(root, active); err != nil {
		return errors.Join(err, rollbackPreparedPendingReview(root, prepared))
	}
	return nil
}

func rollbackPreparedPendingReview(root string, record PendingReviewRecord) error {
	if record.Previous == nil {
		return removePendingReviewRecord(root, record.Task.Ref.ID)
	}
	previous := record.Previous
	restored := PendingReviewRecord{
		Version: record.Version, Plan: record.Plan, Task: previous.Task, Fingerprint: previous.Fingerprint,
		Binding: previous.Binding, Phase: previous.Phase, Round: previous.Round,
		RoundStarted: previous.RoundStarted, UpdatedAt: time.Now().UTC(),
	}
	return writePendingReviewRecord(root, restored)
}

func pendingReviewPlanEqual(a, b PendingReviewPlan) bool {
	return a.Version == b.Version && a.CohortID == b.CohortID && a.Workspace == b.Workspace && a.Branch == b.Branch &&
		a.BaseHead == b.BaseHead && a.ConfigDigest == b.ConfigDigest && a.Continue == b.Continue &&
		a.SignoffRounds == b.SignoffRounds && a.VerifyEnabled == b.VerifyEnabled &&
		a.Signoff.Prompt == b.Signoff.Prompt && a.Signoff.Writes == b.Signoff.Writes &&
		a.Verify.Prompt == b.Verify.Prompt && a.Verify.Writes == b.Verify.Writes && a.MCPDisabled == b.MCPDisabled &&
		a.CreatedAt.Equal(b.CreatedAt) && slices.Equal(a.Queues, b.Queues) &&
		slices.Equal(a.Signoff.Targets, b.Signoff.Targets) && slices.Equal(a.Verify.Targets, b.Verify.Targets)
}

// pendingReviewPlanForConcurrentCompletion returns the one review plan whose durable completion
// window currently observes id as a non-subject. The caller holds id's task authority lock, so it
// can publish matching review debt before publishing the completion receipt.
func pendingReviewPlanForConcurrentCompletion(root, id string) (PendingReviewPlan, bool, error) {
	index, err := ReadCompletionWindowIndex(root)
	if err != nil {
		return PendingReviewPlan{}, false, err
	}
	var selected PendingReviewPlan
	found := false
	for _, window := range index.Windows {
		if !window.ReviewWindow || window.PendingReview == nil || slices.Contains(window.ReviewSubjects, id) {
			continue
		}
		if err := validatePendingReviewPlan(*window.PendingReview); err != nil {
			return PendingReviewPlan{}, false, fmt.Errorf("invalid pending-review plan in completion window: %w", err)
		}
		if found && !pendingReviewPlanEqual(selected, *window.PendingReview) {
			return PendingReviewPlan{}, false, fmt.Errorf("task %s is observed by completion windows with different pending-review plans", id)
		}
		selected = clonePendingReviewPlan(*window.PendingReview)
		found = true
	}
	return selected, found, nil
}

func pendingReviewPlanMatches(repo string, hosts []string, plan PendingReviewPlan) error {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return err
	}
	if workspace != plan.Workspace || len(hosts) != len(plan.Queues) {
		return errors.New("pending final review belongs to a different workspace or queue selection")
	}
	if branch := gitOut(repo, "symbolic-ref", "--quiet", "HEAD"); branch != plan.Branch {
		return errors.New("pending final review belongs to a different branch")
	}
	for i, host := range hosts {
		root, err := canonicalTaskRoot(host)
		if err != nil {
			return err
		}
		identity, err := readQueueIdentity(root)
		if err != nil || root != plan.Queues[i].Root || identity.ID != plan.Queues[i].ID {
			return fmt.Errorf("pending final review queue %d changed identity", i+1)
		}
	}
	return nil
}

// LoadPendingReviews discovers host-private records, validates their exact accepted generations,
// and returns at most one coherent cohort. Multiple review contracts never get silently mixed.
func LoadPendingReviews(repo string, hosts []string) (PendingReviewCohort, error) {
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return PendingReviewCohort{}, err
	}
	directory, err := registry.Open(".")
	if err != nil {
		registry.Close()
		return PendingReviewCohort{}, err
	}
	var entries []os.DirEntry
	for {
		batch, readErr := directory.ReadDir(256)
		for _, entry := range batch {
			if !strings.HasSuffix(entry.Name(), pendingReviewFileSuffix) {
				continue
			}
			entries = append(entries, entry)
			if len(entries) > pendingReviewScanLimit {
				directory.Close()
				registry.Close()
				return PendingReviewCohort{}, fmt.Errorf("pending-review authority registry exceeds %d records", pendingReviewScanLimit)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			directory.Close()
			registry.Close()
			return PendingReviewCohort{}, readErr
		}
	}
	directory.Close()
	registry.Close()
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return PendingReviewCohort{}, err
	}
	wantedRoots := map[string]bool{}
	wantedPrefixes := map[string]bool{}
	for _, host := range hosts {
		root, err := canonicalTaskRoot(host)
		if err != nil {
			return PendingReviewCohort{}, err
		}
		wantedRoots[root] = true
		prefix, err := pendingReviewRecordPrefix(root)
		if err != nil {
			return PendingReviewCohort{}, err
		}
		wantedPrefixes[prefix] = true
	}
	var cohort PendingReviewCohort
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), pendingReviewFileSuffix) {
			continue
		}
		belongs := false
		for prefix := range wantedPrefixes {
			if strings.HasPrefix(entry.Name(), prefix) {
				belongs = true
				break
			}
		}
		if !belongs {
			continue
		}
		// The root and task id are inside the strict record. Records for other workspaces are
		// ignored; records claiming this workspace must validate against this invocation.
		registry, openErr := OpenLeaseAuthorityRoot()
		if openErr != nil {
			return PendingReviewCohort{}, openErr
		}
		data, readErr := ReadTaskMetadataFile(registry, entry.Name())
		registry.Close()
		if readErr != nil {
			return PendingReviewCohort{}, readErr
		}
		record, decodeErr := decodePendingReviewRecord(data)
		if decodeErr != nil {
			return PendingReviewCohort{}, fmt.Errorf("decode pending-review record %q: %w", entry.Name(), decodeErr)
		}
		if record.Plan.Workspace != workspace {
			continue
		}
		if err := validatePendingReviewRecord(record); err != nil {
			return PendingReviewCohort{}, fmt.Errorf("validate pending-review record %q: %w", entry.Name(), err)
		}
		var root string
		for _, queue := range record.Plan.Queues {
			if queue.ID == record.Task.Ref.QueueID {
				root = queue.Root
				break
			}
		}
		if root == "" || !wantedRoots[root] {
			return PendingReviewCohort{}, fmt.Errorf("pending final review for %s requires its original queue selection", record.Task.Ref.ID)
		}
		wantName, err := pendingReviewRecordName(root, record.Task.Ref.ID)
		if err != nil || wantName != entry.Name() {
			return PendingReviewCohort{}, fmt.Errorf("pending-review record %q has the wrong authority key", entry.Name())
		}
		if reconciled, err := reconcileReviewedPendingReview(root, record.Task.Ref.ID); err != nil {
			return PendingReviewCohort{}, fmt.Errorf("reconcile reviewed pending task %s: %w", record.Task.Ref.ID, err)
		} else if reconciled {
			continue
		}
		if err := pendingReviewPlanMatches(repo, hosts, record.Plan); err != nil {
			return PendingReviewCohort{}, err
		}
		if len(cohort.Subjects) > 0 && !pendingReviewPlanEqual(cohort.Plan, record.Plan) {
			return PendingReviewCohort{}, errors.New("pending final reviews have different stored review plans; finish the oldest cohort explicitly")
		}
		current, ok, err := CurrentTask(root, record.Task.Ref.ID)
		if err != nil || !ok {
			return PendingReviewCohort{}, errors.Join(err, fmt.Errorf("pending-review task %s is missing", record.Task.Ref.ID))
		}
		if record.Prepared {
			var currentFingerprint CompletionFingerprint
			if current.State == StateDone {
				currentFingerprint, err = CompletionFingerprintFor(root, current)
			}
			if err == nil && current.State == StateDone && currentFingerprint == record.Fingerprint {
				record.Prepared = false
				record.Previous = nil
				record.UpdatedAt = time.Now().UTC()
				if err := writePendingReviewRecord(root, record); err != nil {
					return PendingReviewCohort{}, err
				}
			} else {
				if current.State == StateDone {
					return PendingReviewCohort{}, errors.Join(err, fmt.Errorf("prepared pending-review task %s has no matching completion receipt", current.ID))
				}
				if rollbackErr := rollbackPreparedPendingReview(root, record); rollbackErr != nil {
					return PendingReviewCohort{}, errors.Join(err, rollbackErr)
				}
				if record.Previous == nil {
					continue
				}
				previous := record.Previous
				record = PendingReviewRecord{
					Version: record.Version, Plan: record.Plan, Task: previous.Task, Fingerprint: previous.Fingerprint,
					Binding: previous.Binding, Phase: previous.Phase, Round: previous.Round,
					RoundStarted: previous.RoundStarted, UpdatedAt: time.Now().UTC(),
				}
			}
		}
		if current.State != StateDone {
			instance, instanceErr := ReadTaskInstance(root, current)
			if instanceErr != nil || !sameTaskInstance(instance, record.Task) {
				return PendingReviewCohort{}, errors.Join(instanceErr, fmt.Errorf("pending-review task %s changed generation", current.ID))
			}
			audit, active, auditErr := ReadAuditReopenRecord(root, current.ID)
			if auditErr != nil {
				return PendingReviewCohort{}, auditErr
			}
			if !active || !auditReopenRecordActive(audit) {
				return PendingReviewCohort{}, errors.Join(auditErr, fmt.Errorf("pending-review task %s left the archive without host review authority", current.ID))
			}
			record.Phase = PendingReviewReopened
			record.Prepared = false
			record.UpdatedAt = time.Now().UTC()
			if err := writePendingReviewRecord(root, record); err != nil {
				return PendingReviewCohort{}, err
			}
			cohort.Plan = record.Plan
			cohort.Subjects = append(cohort.Subjects, record)
			continue
		}
		instance, err := ReadTaskInstance(root, current)
		if err != nil || !sameTaskInstance(instance, record.Task) {
			return PendingReviewCohort{}, errors.Join(err, fmt.Errorf("pending-review task %s changed generation", current.ID))
		}
		fingerprint, err := CompletionFingerprintFor(root, current)
		if err != nil || fingerprint != record.Fingerprint {
			return PendingReviewCohort{}, errors.Join(err, fmt.Errorf("pending-review task %s completion receipt or archive changed", current.ID))
		}
		head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
		bindingRecord := bindingAsAuditRecord(current.ID, record.Binding)
		if !pendingReviewRawBindingValid(repo, record.Binding) || !AuditReopenCurrentValid(repo, head, current.ID, bindingRecord) {
			rebound, changed, rebindErr := rebindPendingReviewFromSigningJournal(repo, record)
			if rebindErr != nil {
				return PendingReviewCohort{}, rebindErr
			}
			if !changed {
				return PendingReviewCohort{}, fmt.Errorf("pending-review task %s Git history changed outside an acknowledged host signing rewrite", current.ID)
			}
			record = rebound
			if err := writePendingReviewRecord(root, record); err != nil {
				return PendingReviewCohort{}, err
			}
		}
		cohort.Plan = record.Plan
		cohort.Subjects = append(cohort.Subjects, record)
	}
	slices.SortFunc(cohort.Subjects, func(a, b PendingReviewRecord) int {
		return strings.Compare(a.Task.Ref.ID, b.Task.Ref.ID)
	})
	if len(cohort.Subjects) > 0 {
		if err := markPendingReviewWorkspace(repo); err != nil {
			return PendingReviewCohort{}, err
		}
	} else if err := clearPendingReviewWorkspaceIfEmpty(repo, hosts); err != nil {
		return PendingReviewCohort{}, err
	}
	return cohort, nil
}

// RebindPendingReviewAfterSigning is the explicit post-signing authority transition. The old raw
// binding must still validate at oldHead, the signed result must be the current newHead, and the
// complete semantic prefix must be preserved. Ordinary startup never infers this transition from
// tree equality, so an unrelated rewrite cannot silently acquire review authority.
func RebindPendingReviewAfterSigning(repo, root, id, oldHead, newHead string) error {
	return rebindPendingReviewAfterAuthorizedRewrite(repo, root, id, oldHead, newHead)
}

// RebindPendingReviewAfterAuditRewrite preserves another completed task's review debt when a
// host-authorized audit repair rewrites an older commit and faithfully replays its descendants.
func RebindPendingReviewAfterAuditRewrite(repo, root, id, oldHead, newHead, auditID string, authority AuditReopenRecord) error {
	if _, err := rebasedAuditReopenRecord(repo, oldHead, newHead, auditID, authority); err != nil {
		return err
	}
	return rebindPendingReviewAfterAuthorizedRewrite(repo, root, id, oldHead, newHead)
}

func rebindPendingReviewAfterAuthorizedRewrite(repo, root, id, oldHead, newHead string) error {
	record, ok, err := readPendingReviewRecord(root, id)
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("pending-review record for %s is missing after history rewrite", id))
	}
	if !validAuditReopenHead(oldHead) || !validAuditReopenHead(newHead) || oldHead == newHead ||
		gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}") != newHead ||
		!pendingReviewRawBindingValid(repo, record.Binding) ||
		!AuditReopenCurrentValid(repo, oldHead, id, bindingAsAuditRecord(id, record.Binding)) {
		return fmt.Errorf("history transition for task %s does not start at its recorded raw binding", id)
	}
	current, err := capturePendingReviewBinding(repo, id)
	if err != nil {
		return err
	}
	if !pendingReviewBindingExtendsSemantics(record.Binding, current) {
		return fmt.Errorf("history rewrite changed pending-review semantics for task %s", id)
	}
	record.Binding = current
	record.UpdatedAt = time.Now().UTC()
	return writePendingReviewRecord(root, record)
}

// EnrollExistingPendingReviews binds receipt-valid host completions discovered by
// completion-window recovery (and by the explicit legacy import path) to a review plan without
// manufacturing a new completion receipt. It is idempotent for the exact generation.
func EnrollExistingPendingReviews(repo string, hosts, ids []string, plan PendingReviewPlan) error {
	if err := validatePendingReviewPlan(plan); err != nil {
		return err
	}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return err
		}
		if task.Item.State != StateDone {
			return fmt.Errorf("task %s is not archived and cannot enter pending final review", id)
		}
		// A legacy archive may never have created a task-authority inode. Creating the empty lock
		// here grants no completion authority; it only lets the import inspect and clearly reject a
		// missing receipt under the same serialized boundary.
		authority, err := lockLeaseAuthority(task.Root, id, true, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			return err
		}
		finish := func(operationErr error) error {
			return errors.Join(operationErr, unlockLeaseFile(authority))
		}
		if err := finish(enrollExistingPendingReviewLocked(repo, task, authority, plan)); err != nil {
			return err
		}
	}
	return nil
}

// PendingReviewImportBase returns the parent of the oldest explicitly imported review subject.
// CaptureAuditReopen already proves a bounded, linear subject-to-HEAD history; the longest such
// history is therefore the earliest subject in the requested set. Anchoring before it keeps final
// verification meaningful even when the import itself creates no new commits.
func PendingReviewImportBase(repo string, hosts, ids []string) (string, error) {
	base, depth := "", -1
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return "", err
		}
		if task.Item.State != StateDone {
			return "", fmt.Errorf("task %s is not archived and cannot enter pending final review", id)
		}
		binding, err := capturePendingReviewBinding(repo, id)
		if err != nil {
			return "", err
		}
		if len(binding.Raw) <= depth {
			continue
		}
		parent, err := gitOutErr(repo, "rev-parse", "--verify", binding.Raw[0]+"^")
		if err != nil {
			return "", fmt.Errorf("review subject %s has no parent commit to anchor final verification: %w", id, err)
		}
		base, depth = parent, len(binding.Raw)
	}
	if base == "" {
		return "", errors.New("no archived tasks were selected for final review")
	}
	return base, nil
}

func enrollExistingPendingReviewLocked(repo string, task QueuedTask, authority *os.File, plan PendingReviewPlan) error {
	instance, err := EnsureTaskInstance(task.Root, task.Item)
	if err != nil {
		return err
	}
	fingerprint, err := completionFingerprintLocked(task.Item, crashCompletionLock{authority: authority})
	if err != nil || fingerprint.Receipt == "" {
		return errors.Join(err, fmt.Errorf("task %s has no host-accepted completion receipt", task.Item.ID))
	}
	if prior, ok, readErr := readPendingReviewRecord(task.Root, task.Item.ID); readErr != nil {
		return readErr
	} else if ok {
		if prior.Task.Ref.ID == task.Item.ID && prior.Fingerprint == fingerprint {
			return nil
		}
		return fmt.Errorf("task %s already has pending review for a different generation", task.Item.ID)
	}
	if reviewed, ok, readErr := readPendingReviewReviewed(task.Root, task.Item.ID); readErr != nil {
		return readErr
	} else if ok && sameTaskInstance(reviewed.Task, instance) {
		if reviewed.Fingerprint == fingerprint {
			return fmt.Errorf("task %s already passed final review for this accepted generation", task.Item.ID)
		}
		receipt, accepted := readLeaseCompletionReceipt(authority, task.Item.Dir)
		if !accepted || receipt.AuditReopenGeneration == "" {
			return fmt.Errorf("task %s archive changed after its recorded final review", task.Item.ID)
		}
		// A later host-authorized audit reopen is a new accepted review generation even though
		// lifecycle moves preserve the task directory inode. Its receipt generation, not changed
		// provider-writable archive bytes alone, is the authority that permits reenrollment.
	}
	binding, err := capturePendingReviewBinding(repo, task.Item.ID)
	if err != nil {
		return err
	}
	record := PendingReviewRecord{
		Version: pendingReviewVersion, Plan: clonePendingReviewPlan(plan), Task: instance, Fingerprint: fingerprint,
		Binding: binding, Phase: PendingReviewSignoff, UpdatedAt: time.Now().UTC(),
	}
	if err := writePendingReviewRecord(task.Root, record); err != nil {
		return err
	}
	if err := markPendingReviewWorkspace(repo); err != nil {
		return errors.Join(err, removePendingReviewRecord(task.Root, task.Item.ID))
	}
	return nil
}

func updatePendingReviewRecords(hosts, ids []string, update func(*PendingReviewRecord)) error {
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return err
		}
		authority, err := lockLeaseAuthority(task.Root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			return fmt.Errorf("lock pending-review task %s: %w", id, err)
		}
		record, ok, readErr := readPendingReviewRecord(task.Root, id)
		if readErr == nil && !ok {
			readErr = fmt.Errorf("pending-review record for %s is missing", id)
		}
		if readErr == nil {
			update(&record)
			record.UpdatedAt = time.Now().UTC()
			readErr = writePendingReviewRecord(task.Root, record)
		}
		if err := errors.Join(readErr, unlockLeaseFile(authority)); err != nil {
			return err
		}
	}
	return nil
}

func updateExpectedPendingReviewRecords(hosts []string, expected []PendingReviewRecord, update func(*PendingReviewRecord)) error {
	wanted := slices.Clone(expected)
	slices.SortFunc(wanted, func(a, b PendingReviewRecord) int {
		return strings.Compare(a.Task.Ref.ID, b.Task.Ref.ID)
	})
	for _, want := range wanted {
		id := want.Task.Ref.ID
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return err
		}
		authority, err := lockLeaseAuthority(task.Root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			return fmt.Errorf("lock pending-review task %s: %w", id, err)
		}
		record, ok, readErr := readPendingReviewRecord(task.Root, id)
		if readErr == nil && !ok {
			readErr = fmt.Errorf("pending-review record for %s is missing", id)
		}
		if readErr == nil && !pendingReviewRecordMatchesExpected(record, want) {
			readErr = fmt.Errorf("pending-review task %s changed generation or phase after the review started", id)
		}
		if readErr == nil {
			update(&record)
			record.UpdatedAt = time.Now().UTC()
			readErr = writePendingReviewRecord(task.Root, record)
		}
		if err := errors.Join(readErr, unlockLeaseFile(authority)); err != nil {
			return err
		}
	}
	return nil
}

func uniqueTaskAcrossRoots(hosts []string, id string) (QueuedTask, error) {
	var found []QueuedTask
	for _, root := range hosts {
		item, ok, err := CurrentTask(root, id)
		if err != nil {
			return QueuedTask{}, err
		}
		if ok {
			found = append(found, QueuedTask{Root: root, Item: item})
		}
	}
	if len(found) != 1 {
		return QueuedTask{}, fmt.Errorf("pending-review task %s resolves to %d queues", id, len(found))
	}
	return found[0], nil
}

func BeginPendingReviewRound(hosts, ids []string, round int) error {
	return updatePendingReviewRecords(hosts, ids, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewSignoff
		if round > record.Round {
			record.Round = round
		}
		record.RoundStarted = true
	})
}

func MarkPendingReviewReopened(hosts, ids []string) error {
	return updatePendingReviewRecords(hosts, ids, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewReopened
		record.RoundStarted = false
	})
}

func MarkExpectedPendingReviewsReopened(hosts []string, expected []PendingReviewRecord) error {
	return updateExpectedPendingReviewRecords(hosts, expected, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewReopened
		record.RoundStarted = false
	})
}

func MarkPendingReviewVerify(hosts, ids []string) error {
	return updatePendingReviewRecords(hosts, ids, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewVerify
		record.RoundStarted = false
	})
}

func MarkExpectedPendingReviewsVerify(hosts []string, expected []PendingReviewRecord) error {
	return updateExpectedPendingReviewRecords(hosts, expected, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewVerify
		record.RoundStarted = false
	})
}

func ClearPendingReviews(hosts []string, expected []PendingReviewRecord) error {
	workspace := ""
	wanted := slices.Clone(expected)
	slices.SortFunc(wanted, func(a, b PendingReviewRecord) int {
		return strings.Compare(a.Task.Ref.ID, b.Task.Ref.ID)
	})
	for _, want := range wanted {
		id := want.Task.Ref.ID
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return err
		}
		authority, err := lockLeaseAuthority(task.Root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			return err
		}
		record, ok, readErr := readPendingReviewRecord(task.Root, id)
		if readErr == nil && !ok {
			readErr = fmt.Errorf("pending-review record for %s is missing", id)
		}
		if readErr == nil && !pendingReviewRecordMatchesExpected(record, want) {
			readErr = fmt.Errorf("pending-review task %s changed generation or phase after the review verdict", id)
		}
		if readErr == nil {
			if task.Item.State != StateDone {
				readErr = fmt.Errorf("pending-review task %s left the archive after the review verdict", id)
			} else if instance, instanceErr := ReadTaskInstance(task.Root, task.Item); instanceErr != nil {
				readErr = instanceErr
			} else if !sameTaskInstance(instance, record.Task) {
				readErr = fmt.Errorf("pending-review task %s changed generation after the review verdict", id)
			} else if fingerprint, fingerprintErr := completionFingerprintLocked(task.Item, crashCompletionLock{authority: authority}); fingerprintErr != nil {
				readErr = fingerprintErr
			} else if fingerprint != record.Fingerprint {
				readErr = fmt.Errorf("pending-review task %s completion receipt or archive changed after the review verdict", id)
			} else if head := gitOut(record.Plan.Workspace, "rev-parse", "--verify", "HEAD^{commit}"); !pendingReviewRawBindingValid(record.Plan.Workspace, record.Binding) ||
				!AuditReopenCurrentValid(record.Plan.Workspace, head, id, bindingAsAuditRecord(id, record.Binding)) {
				readErr = fmt.Errorf("pending-review task %s Git history changed after the review verdict", id)
			}
		}
		if readErr == nil {
			if workspace == "" {
				workspace = record.Plan.Workspace
			} else if workspace != record.Plan.Workspace {
				readErr = errors.New("pending-review subjects belong to different workspaces")
			}
		}
		if readErr == nil {
			readErr = writePendingReviewReviewed(task.Root, record)
		}
		if readErr == nil {
			readErr = removePendingReviewRecord(task.Root, id)
		}
		err = errors.Join(readErr, unlockLeaseFile(authority))
		if err != nil {
			return err
		}
	}
	if workspace != "" {
		return clearPendingReviewWorkspaceIfEmpty(workspace, hosts)
	}
	return nil
}
