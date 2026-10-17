package tasks

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
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
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/safetext"
)

const (
	// A version 1 record also pinned its subject's commit IDs. Version 2 finds the subject's commit by
	// its Coop-Task trailer instead, so a signing pass leaves the record valid.
	pendingReviewVersion       = 2
	pendingReviewLegacyVersion = 1
	pendingReviewPlanVersion   = 1
	pendingReviewFileSuffix    = ".pending-review.json"
	pendingReviewFileLimit     = 256 << 10
	pendingReviewScanLimit     = 4096
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
// BaseHead is the host-recorded start of the commit range the review searches for its subjects.
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

// PendingReviewRecord names its subject by queue, task id and archive generation, never by commit
// ID: the review finds the subject's commit by its Coop-Task trailer after Plan.BaseHead, or after
// Base for a task another session completed with a commit from before the run.
//
// Reviewed is what its latest signoff round started on. A verdict covers only that content, so a
// subject whose commits changed since goes back to signoff instead of being cleared.
type PendingReviewRecord struct {
	Version      int                    `json:"version"`
	Plan         PendingReviewPlan      `json:"plan"`
	Task         TaskInstance           `json:"task"`
	Fingerprint  CompletionFingerprint  `json:"fingerprint"`
	Base         string                 `json:"base,omitempty"`
	Reviewed     *PendingReviewContent  `json:"reviewed,omitempty"`
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
	Phase        PendingReviewPhase    `json:"phase"`
	Round        int                   `json:"round"`
	RoundStarted bool                  `json:"round_started,omitempty"`
}

type PendingReviewCohort struct {
	Plan     PendingReviewPlan
	Subjects []PendingReviewRecord
}

// PendingReviewContent fingerprints what a signoff round reviewed: every commit in the subject's
// review range (after its Base, or the run's) up to HEAD when the round started. Audit rework adds
// its repair as a commit with no trailer, and a reviewer of the run sees the commits around the
// subject's own too. A commit counts by its change, author and message, which signing keeps;
// commits added later are not covered.
type PendingReviewContent struct {
	Commits int    `json:"commits"`
	Digest  string `json:"digest"`
}

// pendingReviewRecordV1 reads a version 1 record. Its bindings named commit IDs that a signing pass
// replaces, so reading drops them: the subject's commit is found by trailer like a version 2 one.
type pendingReviewRecordV1 struct {
	PendingReviewRecord
	Binding  json.RawMessage          `json:"binding"`
	Previous *pendingReviewPreviousV1 `json:"previous,omitempty"`
}

type pendingReviewPreviousV1 struct {
	PendingReviewPrevious
	Binding json.RawMessage `json:"binding"`
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
	branch, err := gitBranchErr(repo)
	if err != nil {
		return PendingReviewPlan{}, fmt.Errorf("read pending-review checked-out branch: %w", err)
	}
	if !strings.HasPrefix(branch, "refs/heads/") {
		return PendingReviewPlan{}, errors.New("pending-review workspace has no checked-out branch")
	}
	plan := PendingReviewPlan{
		Version: pendingReviewPlanVersion, CohortID: hex.EncodeToString(cohortRaw), Workspace: workspace,
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

func pendingReviewRecordPrefix(root string) (string, error) {
	key, err := LeaseAuthorityKey(root, "pending-review-queue")
	return key + "-", err
}

// PendingReviewCommitsError is a final-review task without exactly one commit carrying its
// Coop-Task trailer where the review looks for it. It keeps the debt and starts no reviewer; the CLI
// draws Cause and Remedy.
type PendingReviewCommitsError struct {
	TaskID string
	// Base is the commit the search started after: where the run started, or, with OwnBase, the
	// parent of the commit another session completed the task with. Empty means the whole branch.
	Base    string
	OwnBase bool
	Found   int
	// Commits names the commits carrying the trailer, "<short id> by <author>", when more than one
	// does: a planted one can sit anywhere, and only the person can tell which to drop.
	Commits []string
}

func (e *PendingReviewCommitsError) needs() string {
	where := "in this branch"
	if e.Base != "" {
		short := e.Base[:min(12, len(e.Base))]
		where = "after " + short + ", where this run started"
		if e.OwnBase {
			where = "after " + short + ", just before the commit it was completed with"
		}
	}
	found := fmt.Sprintf("found %d", e.Found)
	if len(e.Commits) > 0 {
		found += ": " + strings.Join(e.Commits, ", ")
	}
	return fmt.Sprintf("needs exactly one commit with %q %s; %s", CoopTaskTrailer+": "+e.TaskID, where, found)
}

// describeTaskCommits names commits as "<short id> by <author>" when there are several, the first
// five and how many more: a box can plant any number. A box sets its own author, so anything a
// terminal would act on or a reader could not see, such as an escape sequence, a direction override
// or a line separator, becomes '?' (markHidden).
func describeTaskCommits(repo string, shas []string) []string {
	if len(shas) < 2 {
		return nil
	}
	const shown = 5
	more := 0
	if len(shas) > shown {
		shas, more = shas[:shown], len(shas)-shown
	}
	described := make([]string, len(shas), len(shas)+1)
	for i, sha := range shas {
		line := gitOut(repo, "show", "-s", "--format=%h by %an <%ae>", sha)
		if line == "" {
			line = sha[:min(12, len(sha))]
		}
		described[i] = markHidden(line)
	}
	if more > 0 {
		described = append(described, fmt.Sprintf("and %d more", more))
	}
	return described
}

// markHidden writes '?' for invalid UTF-8 and for each character safetext.Hidden names, so the
// person still sees that something was there.
func markHidden(line string) string {
	previous := rune(-1)
	return strings.Map(func(r rune) rune {
		hidden := r == utf8.RuneError || safetext.Hidden(previous, r)
		previous = r
		if hidden {
			return '?'
		}
		return r
	}, line)
}

func (e *PendingReviewCommitsError) Error() string { return "task " + e.TaskID + " " + e.needs() }

// Cause is the refusal as a sentence.
func (e *PendingReviewCommitsError) Cause() string { return "Task " + e.TaskID + " " + e.needs() + "." }

// Remedy is how to repair the history before trying again. A box writes commit messages, so an
// extra commit may be forged: dropping it is as likely right as squashing.
func (e *PendingReviewCommitsError) Remedy() string {
	switch {
	case e.Found == 0:
		return "Restore one commit with that trailer"
	case e.Found == 2:
		return "Drop the extra commit or squash them"
	default:
		return "Drop the extra commits or squash them"
	}
}

// pendingReviewSubjectBase is where a subject's review range starts.
func pendingReviewSubjectBase(subject PendingReviewRecord) string {
	if subject.Base != "" {
		return subject.Base
	}
	return subject.Plan.BaseHead
}

// pendingReviewSubjectCommits finds each subject's one commit: the commit in its review range whose
// sole Coop-Task trailer names it. A box writes commit messages, so the search never looks at
// history the host-recorded base already had. The range is read the way git reads it, as the
// commits reachable from HEAD and not from the base, so a start commit the run amended (the recovery
// recipe for a commit that lost its folder move) or re-signed still bounds it. Signing rewrites
// commit IDs but keeps every trailer, so a signed run still finds its commits. A dropped or
// duplicated commit is a *PendingReviewCommitsError.
func pendingReviewSubjectCommits(repo string, subjects []PendingReviewRecord) (map[string]string, error) {
	after, err := rawReachableAuditCommits(repo, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read the history final review covers: %w", err)
	}
	earlier := map[string]map[string]bool{}
	found := make(map[string]string, len(subjects))
	for _, subject := range subjects {
		id, base := subject.Task.Ref.ID, pendingReviewSubjectBase(subject)
		before, ok := earlier[base]
		if !ok {
			commits, err := rawReachableAuditCommits(repo, base)
			if err != nil {
				return nil, fmt.Errorf("read commit %s, where the final review of %s starts: %w", base, id, err)
			}
			before = make(map[string]bool, len(commits))
			for _, commit := range commits {
				before[commit.sha] = true
			}
			earlier[base] = before
		}
		var shas []string
		for _, commit := range after {
			if !before[commit.sha] && !commit.taskBindingInvalid && len(commit.taskValues) == 1 && commit.taskValues[0] == id {
				shas = append(shas, commit.sha)
			}
		}
		if len(shas) != 1 {
			return nil, &PendingReviewCommitsError{TaskID: id, Base: base, OwnBase: subject.Base != "", Found: len(shas),
				Commits: describeTaskCommits(repo, shas)}
		}
		found[id] = shas[0]
	}
	return found, nil
}

// pendingReviewNewBase is the Base a subject entering review needs. Its trailer must name exactly
// one commit in the whole branch, as a box's own completion check already requires: a box can plant
// a commit naming a task it does not hold (tolerated with a warning), and a host completion must not
// bind that commit while the human's own commit goes unreviewed. That commit after the run's base
// needs no Base. One from before the run, such as a task another session committed before the loop
// started and completes during it, is anchored the way --review-task anchors an import: after its
// parent. A recompletion keeps the Base it has.
func pendingReviewNewBase(repo string, subject PendingReviewRecord) (string, error) {
	_, err := pendingReviewSubjectCommits(repo, []PendingReviewRecord{subject})
	var missing *PendingReviewCommitsError
	if subject.Base != "" || (err != nil && (!errors.As(err, &missing) || missing.Found != 0)) {
		return subject.Base, err
	}
	head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	bindings, ok := rawTaskBindings(repo, head)
	if !ok {
		return "", errors.New("read reachable task bindings for final review")
	}
	if shas := bindings[subject.Task.Ref.ID]; len(shas) != 1 {
		return "", &PendingReviewCommitsError{TaskID: subject.Task.Ref.ID, Found: len(shas), Commits: describeTaskCommits(repo, shas)}
	}
	if err == nil {
		return "", nil
	}
	anchor, _, err := pendingReviewAnchor(repo, head, bindings, subject.Task.Ref.ID)
	return anchor, err
}

// PendingReviewEarlierCommits returns the commit of each subject anchored before the run's base, a
// task another session committed before the loop started: the run's own range does not show it, and
// a reviewer must see what its verdict accepts.
func PendingReviewEarlierCommits(repo string, subjects []PendingReviewRecord) (map[string]string, error) {
	var anchored []PendingReviewRecord
	for _, subject := range subjects {
		if subject.Base != "" {
			anchored = append(anchored, subject)
		}
	}
	if len(anchored) == 0 {
		return nil, nil
	}
	return pendingReviewSubjectCommits(repo, anchored)
}

// pendingReviewAnchor returns the parent of id's one reachable commit, where a review range that
// must include that commit starts, and how many commits lead from it to head.
func pendingReviewAnchor(repo, head string, bindings map[string][]string, id string) (string, int, error) {
	if shas := bindings[id]; len(shas) != 1 {
		return "", 0, &PendingReviewCommitsError{TaskID: id, Found: len(shas), Commits: describeTaskCommits(repo, shas)}
	}
	subject := bindings[id][0]
	history, err := rawAuditHistoryUntil(repo, head, auditReopenHistoryLimit+1,
		func(commit rawAuditCommit) bool { return commit.sha == subject })
	if err != nil {
		return "", 0, fmt.Errorf("read review subject %s history: %w", id, err)
	}
	if history[0].parent == "" {
		return "", 0, fmt.Errorf("review subject %s has no parent commit to anchor final verification", id)
	}
	return history[0].parent, len(history), nil
}

// pendingReviewHistory is a review window: the linear history in base..HEAD as git reads it, with
// each commit's change and the tree the first change applies to. A review of the run saw all of it,
// not just the subject's own commit: an attempt that exits before any range check can rewrite an
// untrailered commit on either side, or drop the commit the window starts from.
type pendingReviewHistory struct {
	start   string
	commits []rawAuditCommit
	changes []string
}

// readPendingReviewHistory reads the window after base. It must be linear, as audit authority
// already requires.
func readPendingReviewHistory(repo, base string) (pendingReviewHistory, error) {
	var history pendingReviewHistory
	before, err := rawReachableAuditCommits(repo, base)
	if err != nil {
		return history, fmt.Errorf("read commit %s, where final review starts: %w", base, err)
	}
	earlier := make(map[string]bool, len(before))
	for _, commit := range before {
		earlier[commit.sha] = true
	}
	head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	if !validAuditReopenHead(head) {
		return history, errors.New("resolve the HEAD final review covers")
	}
	if earlier[head] {
		return history, nil
	}
	raw, err := rawAuditHistoryUntil(repo, head, auditReopenHistoryLimit+1,
		func(commit rawAuditCommit) bool { return commit.parent == "" || earlier[commit.parent] })
	if err != nil {
		return history, fmt.Errorf("read the commits final review covers: %w", err)
	}
	changes, err := semanticRawHistoryChangeTrees(repo, raw)
	if err != nil {
		return history, fmt.Errorf("read what the final-review commits change: %w", err)
	}
	// Each change is a diff against its parent, so the window also records the tree the first one
	// applies to: dropping the commit the window starts from changes HEAD's tree but no diff.
	start := auditObjectID("tree", nil, len(raw[0].tree))
	if raw[0].parent != "" {
		if start, err = auditCommitTree(repo, raw[0].parent); err != nil {
			return history, fmt.Errorf("read the tree final review starts from: %w", err)
		}
	}
	history.start, history.commits, history.changes = start, raw, changes
	return history, nil
}

// readPendingReviewWindows reads the window of each distinct base among subjects.
func readPendingReviewWindows(repo string, subjects []PendingReviewRecord) (map[string]pendingReviewHistory, error) {
	windows := map[string]pendingReviewHistory{}
	for _, subject := range subjects {
		base := pendingReviewSubjectBase(subject)
		if _, ok := windows[base]; ok {
			continue
		}
		window, err := readPendingReviewHistory(repo, base)
		if err != nil {
			return nil, err
		}
		windows[base] = window
	}
	return windows, nil
}

// content is what a signoff round starting now reviews.
func (h pendingReviewHistory) content() PendingReviewContent {
	return PendingReviewContent{Commits: len(h.commits), Digest: h.digest(len(h.commits))}
}

// covers reports whether every commit a round reviewed is still there, unchanged and in order. A
// commit added after the round does not count.
func (h pendingReviewHistory) covers(reviewed *PendingReviewContent) bool {
	return reviewed != nil && reviewed.Commits >= 1 && reviewed.Commits <= len(h.commits) &&
		h.digest(reviewed.Commits) == reviewed.Digest
}

// digest fingerprints the tree the window starts from and its first n commits by change, author and
// message, none of which signing changes. Every field carries its length, so no bytes inside one can
// pass for a boundary.
func (h pendingReviewHistory) digest(n int) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%d:%s", len(h.start), h.start)
	for i := range n {
		commit := h.commits[i]
		for _, field := range []string{h.changes[i], commit.authorName, commit.authorEmail, commit.authorDate, commit.commitMessage} {
			fmt.Fprintf(sum, "%d:%s", len(field), field)
		}
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// backToSignoff makes a subject wait for signoff again, with no verdict covering it.
func backToSignoff(record *PendingReviewRecord) {
	record.Phase = PendingReviewSignoff
	record.RoundStarted = false
	record.Reviewed = nil
}

func decodePendingReviewJSON(data []byte, value any) error {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), pendingReviewFileLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("pending-review record contains multiple JSON values")
		}
		return err
	}
	return nil
}

// decodePendingReviewRecord reads a record, converting a version 1 one (legacy) to version 2.
func decodePendingReviewRecord(data []byte) (record PendingReviewRecord, legacy bool, err error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return PendingReviewRecord{}, false, err
	}
	if header.Version == pendingReviewLegacyVersion {
		var old pendingReviewRecordV1
		if err := decodePendingReviewJSON(data, &old); err != nil {
			return PendingReviewRecord{}, false, err
		}
		record = old.PendingReviewRecord
		record.Version = pendingReviewVersion
		if old.Previous != nil {
			previous := old.Previous.PendingReviewPrevious
			record.Previous = &previous
		}
		return record, true, nil
	}
	if err := decodePendingReviewJSON(data, &record); err != nil {
		return PendingReviewRecord{}, false, err
	}
	return record, false, nil
}

func validatePendingReviewPlan(plan PendingReviewPlan) error {
	if plan.Version != pendingReviewPlanVersion || !durableIdentityRE.MatchString(plan.CohortID) ||
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
	if err := validatePendingReviewSubject(record.Task, record.Fingerprint); err != nil {
		return err
	}
	if (record.Base != "" && !validAuditReopenHead(record.Base)) ||
		(record.Reviewed != nil && (record.Reviewed.Commits < 1 || !validAuditReopenHead(record.Reviewed.Digest))) {
		return errors.New("invalid pending-review commit context")
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
		if err := validatePendingReviewSubject(previous.Task, previous.Fingerprint); err != nil {
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

func validatePendingReviewSubject(task TaskInstance, fingerprint CompletionFingerprint) error {
	if task.Ref.ID == "" || !durableIdentityRE.MatchString(task.Ref.QueueID) ||
		!durableIdentityRE.MatchString(task.Ref.TaskID) || task.Generation.Device == 0 ||
		task.Generation.Inode == 0 || fingerprint.Device != task.Generation.Device ||
		fingerprint.Inode != task.Generation.Inode || !durableIdentityRE.MatchString(fingerprint.Receipt) ||
		fingerprint.ReceiptBusy || !validAuditReopenHead(fingerprint.Tree) {
		return errors.New("invalid pending-review subject")
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
	record, _, err := decodePendingReviewRecord(data)
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

func pendingReviewRecordMatchesExpected(current, expected PendingReviewRecord) bool {
	return current.Version == expected.Version && pendingReviewPlanEqual(current.Plan, expected.Plan) &&
		sameTaskInstance(current.Task, expected.Task) && current.Fingerprint.Matches(expected.Fingerprint) &&
		current.Base == expected.Base && (current.Reviewed == nil) == (expected.Reviewed == nil) &&
		(current.Reviewed == nil || *current.Reviewed == *expected.Reviewed) &&
		current.Phase == expected.Phase && current.Round == expected.Round &&
		current.RoundStarted == expected.RoundStarted && current.Prepared == expected.Prepared
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
	subject := PendingReviewRecord{Plan: plan, Task: instance}
	if hadPrior {
		subject.Plan, subject.Base = prior.Plan, prior.Base
	}
	base, err := pendingReviewNewBase(repo, subject)
	if err != nil {
		return err
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
		Version: pendingReviewVersion, Plan: subject.Plan, Task: instance, Fingerprint: fingerprint, Base: base,
		Phase: PendingReviewSignoff, Prepared: true, UpdatedAt: time.Now().UTC(),
	}
	if hadPrior {
		record.Round = prior.Round
		record.Previous = &PendingReviewPrevious{
			Task: prior.Task, Fingerprint: prior.Fingerprint,
			Phase: prior.Phase, Round: prior.Round, RoundStarted: prior.RoundStarted,
		}
	}
	if err := writePendingReviewRecord(l.root, record); err != nil {
		return err
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
	return writePendingReviewRecord(root, previousPendingReview(record))
}

// previousPendingReview is the debt a prepared recompletion displaced. It keeps the subject's Base,
// which the same task's commit still needs.
func previousPendingReview(record PendingReviewRecord) PendingReviewRecord {
	previous := record.Previous
	return PendingReviewRecord{
		Version: record.Version, Plan: record.Plan, Task: previous.Task, Fingerprint: previous.Fingerprint, Base: record.Base,
		Phase: previous.Phase, Round: previous.Round, RoundStarted: previous.RoundStarted, UpdatedAt: time.Now().UTC(),
	}
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
	branch, err := gitBranchErr(repo)
	if err != nil {
		return fmt.Errorf("read pending final review branch: %w", err)
	}
	if branch != plan.Branch {
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
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return PendingReviewCohort{}, err
	}
	wantedRoots := map[string]bool{}
	var wantedPrefixes []string
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
		wantedPrefixes = append(wantedPrefixes, prefix)
	}
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
			// The registry is host-wide: only the selected queues' records count toward the bound, so
			// other queues' records cannot fail this load, however many there are.
			name := entry.Name()
			if !strings.HasSuffix(name, pendingReviewFileSuffix) ||
				!slices.ContainsFunc(wantedPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
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
	var cohort PendingReviewCohort
	var reworked []AuditReopenRecord
	roots := map[string]string{}
	for _, entry := range entries {
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
		record, legacy, decodeErr := decodePendingReviewRecord(data)
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
		if err := pendingReviewPlanMatches(repo, hosts, record.Plan); err != nil {
			return PendingReviewCohort{}, err
		}
		if legacy {
			// Version 1 pinned the commit wherever it was, so one from before the run base gets the
			// anchor a new enrollment would give it, once.
			base, err := pendingReviewNewBase(repo, record)
			if err != nil {
				return PendingReviewCohort{}, err
			}
			if base != record.Base {
				record.Base = base
				if err := writePendingReviewRecord(root, record); err != nil {
					return PendingReviewCohort{}, err
				}
			}
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
			if err == nil && current.State == StateDone && record.Fingerprint.Matches(currentFingerprint) {
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
				record = previousPendingReview(record)
			}
		}
		instance, instanceErr := ReadTaskInstance(root, current)
		if instanceErr != nil || !sameTaskInstance(instance, record.Task) {
			return PendingReviewCohort{}, errors.Join(instanceErr, fmt.Errorf("pending-review task %s changed generation", current.ID))
		}
		if current.State != StateDone {
			// A reopened subject keeps its debt only under host review authority. Whether that
			// authority still matches HEAD is the lease's question: a stale one parks the task with
			// recovery guidance instead of stopping every later loop here.
			audit, active, auditErr := ReadAuditReopenRecord(root, current.ID)
			if auditErr != nil {
				return PendingReviewCohort{}, auditErr
			}
			if !active || !auditReopenRecordActive(audit) {
				return PendingReviewCohort{}, fmt.Errorf("pending-review task %s left the archive without host review authority", current.ID)
			}
			replaced, err := recoverInterruptedRework(repo, root, current.ID)
			if err != nil {
				return PendingReviewCohort{}, fmt.Errorf("recover task %s's interrupted rework: %w", current.ID, err)
			}
			if replaced != nil {
				reworked = append(reworked, *replaced)
			}
			record.Phase = PendingReviewReopened
			record.Prepared = false
			record.Reviewed = nil
			record.UpdatedAt = time.Now().UTC()
			if err := writePendingReviewRecord(root, record); err != nil {
				return PendingReviewCohort{}, err
			}
			cohort.Plan = record.Plan
			cohort.Subjects = append(cohort.Subjects, record)
			roots[record.Task.Ref.ID] = root
			continue
		}
		fingerprint, err := CompletionFingerprintFor(root, current)
		if err != nil || !record.Fingerprint.Matches(fingerprint) {
			return PendingReviewCohort{}, errors.Join(err, fmt.Errorf("pending-review task %s completion receipt or archive changed", current.ID))
		}
		cohort.Plan = record.Plan
		cohort.Subjects = append(cohort.Subjects, record)
		roots[record.Task.Ref.ID] = root
	}
	slices.SortFunc(cohort.Subjects, func(a, b PendingReviewRecord) int {
		return strings.Compare(a.Task.Ref.ID, b.Task.Ref.ID)
	})
	if len(cohort.Subjects) > 0 {
		ids := make([]string, len(cohort.Subjects))
		for i, subject := range cohort.Subjects {
			ids[i] = subject.Task.Ref.ID
		}
		// Carry the other reopened tasks across a recovered rework now: once its authority moved, a
		// later load no longer sees the rework, and those tasks would be parked as stale.
		for _, record := range reworked {
			if err := CarryReopenedReviews(repo, hosts, ids, record.BaselineHead, &record); err != nil {
				return PendingReviewCohort{}, fmt.Errorf("keep final-review authority across task %s's interrupted rework: %w", record.TaskID, err)
			}
		}
		if _, err := pendingReviewSubjectCommits(repo, cohort.Subjects); err != nil {
			return PendingReviewCohort{}, err
		}
		if err := returnChangedToSignoff(repo, roots, cohort.Subjects); err != nil {
			return PendingReviewCohort{}, err
		}
	}
	return cohort, nil
}

// returnChangedToSignoff sends back to signoff each subject awaiting verification whose reviewed
// commits changed or disappeared: a later attempt amended one of them, such as its commit or the
// repair after it, or someone rewrote them while the loop was stopped. Verification then cannot
// clear content no reviewer saw.
func returnChangedToSignoff(repo string, roots map[string]string, subjects []PendingReviewRecord) error {
	var accepted []PendingReviewRecord
	for _, subject := range subjects {
		if subject.Phase == PendingReviewVerify {
			accepted = append(accepted, subject)
		}
	}
	windows, err := readPendingReviewWindows(repo, accepted)
	if err != nil {
		return err
	}
	for i, subject := range subjects {
		id := subject.Task.Ref.ID
		if subject.Phase != PendingReviewVerify || windows[pendingReviewSubjectBase(subject)].covers(subject.Reviewed) {
			continue
		}
		backToSignoff(&subject)
		subject.UpdatedAt = time.Now().UTC()
		if err := writePendingReviewRecord(roots[id], subject); err != nil {
			return err
		}
		subjects[i] = subject
	}
	return nil
}

// recoverInterruptedRework accepts the rework a stopped controller left uncompleted: when the task's
// commit was rewritten after its audit authority was recorded, the rewrite must pass the same check
// completion applies, and the authority moves to it without spending its generation. It returns the
// authority it replaced, because that rework also replayed commits other reopened tasks record.
func recoverInterruptedRework(repo, root, id string) (replaced *AuditReopenRecord, err error) {
	authority, err := lockLeaseAuthority(root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, nil // no authority file, or a controller holds the task and checks it before working it
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlockLeaseFile(authority)) }()
	record, ok, err := ReadAuditReopenRecord(root, id)
	if err != nil || !ok || !auditReopenRecordActive(record) {
		return nil, err
	}
	head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	if AuditReopenCurrentValid(repo, head, id, record) {
		return nil, nil
	}
	rebased, err := rebasedAuditReopenRecord(repo, record.BaselineHead, head, id, record)
	if err != nil {
		return nil, nil // not a rework this authority accepts; the lease parks the task
	}
	if err := replaceAuditReopenRecordIfMatches(root, record, rebased); err != nil {
		return nil, err
	}
	return &record, nil
}

// CarryReopenedReviews keeps the audit authority of the tasks in ids that a final review reopened
// valid after the host itself rewrote the history that authority records: a signing pass (reworked
// is nil), or reworked's accepted audit rework, which replays every commit after its own. Each
// record must have held at oldHead. Its task's commits are found again by trailer at HEAD and must
// still mean exactly what was reviewed, except that the reworked task's commit becomes its
// replacement. Anything else leaves the record as it was, and the loop parks that task with
// recovery guidance when it next leases it.
func CarryReopenedReviews(repo string, hosts, ids []string, oldHead string, reworked *AuditReopenRecord) error {
	if reworked != nil {
		ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == reworked.TaskID })
	}
	if len(ids) == 0 {
		return nil
	}
	var rework map[AuditReopenCommit]AuditReopenCommit
	if reworked != nil {
		head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
		rebased, err := rebasedAuditReopenRecord(repo, oldHead, head, reworked.TaskID, *reworked)
		if err != nil {
			return err
		}
		rework = map[AuditReopenCommit]AuditReopenCommit{reworked.Subject: rebased.Subject}
	}
	for _, id := range ids {
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return err
		}
		if err := carryAuditReopen(repo, task.Root, id, oldHead, rework); err != nil {
			return fmt.Errorf("carry task %s's review authority: %w", id, err)
		}
	}
	return nil
}

// CarryReopenedReviewsAfterSigning is CarryReopenedReviews for every subject of this workspace's
// pending final review; one without audit authority is skipped. Every host signing pass calls it,
// the loop's and `coop sign`'s alike.
func CarryReopenedReviewsAfterSigning(repo, oldHead string) error {
	workspace, err := canonicalPendingReviewWorkspace(repo)
	if err != nil {
		return err
	}
	records, err := workspacePendingReviews(workspace)
	if err != nil {
		return err
	}
	var errs []error
	for _, record := range records {
		for _, queue := range record.Plan.Queues {
			if queue.ID == record.Task.Ref.QueueID {
				errs = append(errs, carryAuditReopen(repo, queue.Root, record.Task.Ref.ID, oldHead, nil))
			}
		}
	}
	return errors.Join(errs...)
}

func carryAuditReopen(repo, root, id, oldHead string, rework map[AuditReopenCommit]AuditReopenCommit) (err error) {
	authority, err := lockLeaseAuthority(root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil // no authority to carry, or a controller holds the task and checks it before working it
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlockLeaseFile(authority)) }()
	record, ok, err := ReadAuditReopenRecord(root, id)
	if err != nil || !ok || !auditReopenRecordActive(record) {
		return err
	}
	if AuditReopenCurrentValid(repo, "HEAD", id, record) || !AuditReopenCurrentValid(repo, oldHead, id, record) {
		return nil
	}
	current, err := CaptureAuditReopen(repo, id)
	if err != nil || current.Subject != record.Subject || len(current.History) < len(record.History) {
		return nil
	}
	for i, commit := range record.History {
		if replacement, ok := rework[commit]; ok {
			commit = replacement
		}
		if current.History[i] != commit {
			return nil
		}
	}
	current.Generation = record.Generation
	return replaceAuditReopenRecordIfMatches(root, record, current)
}

// workspacePendingReviews reads every pending final-review record that names workspace. The
// registry is host-wide, so a record for another workspace, or one that does not read, is skipped.
func workspacePendingReviews(workspace string) ([]PendingReviewRecord, error) {
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return nil, err
	}
	defer registry.Close()
	directory, err := registry.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	var records []PendingReviewRecord
	for {
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), pendingReviewFileSuffix) {
				continue
			}
			data, err := ReadTaskMetadataFile(registry, entry.Name())
			if err != nil {
				continue
			}
			record, _, err := decodePendingReviewRecord(data)
			if err == nil && record.Plan.Workspace == workspace && validatePendingReviewRecord(record) == nil {
				records = append(records, record)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return records, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
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

// PendingReviewImportBase returns the parent of the oldest explicitly imported review subject's
// commit, found by its Coop-Task trailer. Anchoring the review range before it keeps final
// verification meaningful even when the import itself creates no new commits.
func PendingReviewImportBase(repo string, hosts, ids []string) (string, error) {
	head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	bindings, ok := rawTaskBindings(repo, head)
	if !ok {
		return "", errors.New("read reachable task bindings for the review import")
	}
	base, depth := "", -1
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return "", err
		}
		if task.Item.State != StateDone {
			return "", fmt.Errorf("task %s is not archived and cannot enter pending final review", id)
		}
		anchor, length, err := pendingReviewAnchor(repo, head, bindings, id)
		if err != nil {
			return "", err
		}
		if length > depth {
			base, depth = anchor, length
		}
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
		if prior.Task.Ref.ID == task.Item.ID && prior.Fingerprint.Matches(fingerprint) {
			return nil
		}
		return fmt.Errorf("task %s already has pending review for a different generation", task.Item.ID)
	}
	record := PendingReviewRecord{
		Version: pendingReviewVersion, Plan: clonePendingReviewPlan(plan), Task: instance, Fingerprint: fingerprint,
		Phase: PendingReviewSignoff, UpdatedAt: time.Now().UTC(),
	}
	base, err := pendingReviewNewBase(repo, record)
	if err != nil {
		return err
	}
	record.Base = base
	return writePendingReviewRecord(task.Root, record)
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

// BeginPendingReviewRound records that a signoff round starts for ids, with what each subject's
// commits do now: the content that round's verdict covers.
func BeginPendingReviewRound(hosts, ids []string, round int) error {
	var subjects []PendingReviewRecord
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return err
		}
		record, ok, err := readPendingReviewRecord(task.Root, id)
		if err == nil && !ok {
			err = fmt.Errorf("pending-review record for %s is missing", id)
		}
		if err != nil {
			return err
		}
		subjects = append(subjects, record)
	}
	if len(subjects) == 0 {
		return nil
	}
	repo := subjects[0].Plan.Workspace
	if _, err := pendingReviewSubjectCommits(repo, subjects); err != nil {
		return err
	}
	windows, err := readPendingReviewWindows(repo, subjects)
	if err != nil {
		return err
	}
	return updatePendingReviewRecords(hosts, ids, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewSignoff
		if round > record.Round {
			record.Round = round
		}
		record.RoundStarted = true
		content := windows[pendingReviewSubjectBase(*record)].content()
		record.Reviewed = &content
	})
}

func MarkPendingReviewReopened(hosts, ids []string) error {
	return updatePendingReviewRecords(hosts, ids, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewReopened
		record.RoundStarted = false
		record.Reviewed = nil
	})
}

func MarkExpectedPendingReviewsReopened(hosts []string, expected []PendingReviewRecord) error {
	return updateExpectedPendingReviewRecords(hosts, expected, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewReopened
		record.RoundStarted = false
		record.Reviewed = nil
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

// ClearPendingReviews retires the debt of subjects a receipt-consistent review accepted. Each
// subject must still be the archived generation with exactly one commit in its review range; a
// crash before the last removal only means those subjects are reviewed again. A subject whose
// commits no longer do what its signoff reviewed goes back to signoff instead: the verdict covered
// other content.
func ClearPendingReviews(hosts []string, expected []PendingReviewRecord) error {
	if len(expected) == 0 {
		return nil
	}
	wanted := slices.Clone(expected)
	slices.SortFunc(wanted, func(a, b PendingReviewRecord) int {
		return strings.Compare(a.Task.Ref.ID, b.Task.Ref.ID)
	})
	plan := wanted[0].Plan
	for _, want := range wanted {
		if !pendingReviewPlanEqual(want.Plan, plan) {
			return errors.New("pending-review subjects belong to different review cohorts")
		}
	}
	if _, err := pendingReviewSubjectCommits(plan.Workspace, wanted); err != nil {
		return fmt.Errorf("history changed after the review verdict: %w", err)
	}
	windows, err := readPendingReviewWindows(plan.Workspace, wanted)
	if err != nil {
		return err
	}
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
			} else if !record.Fingerprint.Matches(fingerprint) {
				readErr = fmt.Errorf("pending-review task %s completion receipt or archive changed after the review verdict", id)
			}
		}
		if readErr == nil && windows[pendingReviewSubjectBase(record)].covers(record.Reviewed) {
			readErr = removePendingReviewRecord(task.Root, id)
		} else if readErr == nil {
			backToSignoff(&record)
			record.UpdatedAt = time.Now().UTC()
			readErr = writePendingReviewRecord(task.Root, record)
		}
		if err := errors.Join(readErr, unlockLeaseFile(authority)); err != nil {
			return err
		}
	}
	return nil
}
