package sessionsvc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A synthetic value the credential scanner matches. It is never a real secret, and no error a test
// checks may repeat it.
var upstreamCheckpointFixture = "ghp_" + strings.Repeat("aB7C", 10) + "\n"

type upstreamCheckpointCase struct {
	upstream string // the job source's remote, which the fixture refresher fetches from
	base     string
	service  *sessionFixture
	source   session.Session
	refresh  *int
}

// newUpstreamCheckpointCase admits a session at base, then advances the authorized upstream with a
// commit that holds a scanner match, as in the 2026-11-19 incident.
func newUpstreamCheckpointCase(t *testing.T, offer string) upstreamCheckpointCase {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	upstream, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(upstream, ".gitignore"), ".agent/tasks/\n")
	sessionWorkspaceWrite(t, filepath.Join(upstream, "tracked.txt"), "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	c := upstreamCheckpointCase{upstream: upstream, base: gitOut(upstream, "rev-parse", "HEAD"), refresh: new(int)}
	c.service = newTestSessionService(t, filepath.Join(t.TempDir(), "state"), upstream, nil)
	t.Cleanup(func() { _ = c.service.Stop() })
	inner := c.service.sourceRefresher
	c.service.sourceRefresher = func(ctx context.Context, jobRef string, source workerproto.JobSource, staged string) (string, error) {
		*c.refresh++
		return inner(ctx, jobRef, source, staged)
	}
	ctx := context.Background()
	created, err := c.service.CreateRemoteSession(ctx, offer+"-source", c.service.request(t, offer))
	if err != nil {
		t.Fatal(err)
	}
	c.source, err = c.service.EnsureWorkspaceTask(ctx, offer+"-task", EnsureWorkspaceTaskRequest{
		SessionID: created.ID, ExpectedRevision: created.Revision,
		Task: tasks.ControllerTaskDraft{OfferRef: offer, Title: "Rebased work", Prompt: "Keep the work after a rebase.", SuccessChecks: []string{"exact restore"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (c upstreamCheckpointCase) advanceUpstream(t *testing.T, path, content, message string) string {
	t.Helper()
	sessionWorkspaceWrite(t, filepath.Join(c.upstream, path), content)
	sessionWorkspaceGit(t, c.upstream, "add", path)
	sessionWorkspaceGit(t, c.upstream, "commit", "-qm", message)
	return gitOut(c.upstream, "rev-parse", "HEAD")
}

func (c upstreamCheckpointCase) capture(t *testing.T, key string) (CheckpointWorkspaceResult, error) {
	t.Helper()
	return c.service.CheckpointWorkspace(context.Background(), key, CheckpointWorkspaceRequest{
		SessionID: c.source.ID, SessionRef: "source", ExpectedRevision: c.source.Revision, PlacementGeneration: 1, RepositoryRef: "repo",
	})
}

// freshWorker is a second worker with its own store, running the source session's exact job.
func (c upstreamCheckpointCase) freshWorker(t *testing.T) *sessionFixture {
	t.Helper()
	worker := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", nil)
	t.Cleanup(func() { _ = worker.Stop() })
	worker.Job = c.service.Job
	stageTestJobBinding(t, worker.stateRoot, c.upstream, *c.service.Job.Source)
	worker.sourceRefresher = c.service.sourceRefresher
	return worker
}

// checkpointPayload reads the bundle's manifest and the Git object names its repository carries.
func checkpointPayload(t *testing.T, bundle []byte) (workerproto.WorkspaceCheckpointBundleManifest, map[string]bool) {
	t.Helper()
	var manifest workerproto.WorkspaceCheckpointBundleManifest
	objects := map[string]bool{}
	outer := tar.NewReader(bytes.NewReader(bundle))
	for {
		header, err := outer.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(outer)
		if err != nil {
			t.Fatal(err)
		}
		switch header.Name {
		case "manifest.json":
			if manifest, err = workerproto.DecodeWorkspaceCheckpointBundleManifest(data); err != nil {
				t.Fatal(err)
			}
		case "repository.tar":
			inner := tar.NewReader(bytes.NewReader(data))
			for {
				member, err := inner.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				objects[member.Name] = true
			}
		}
	}
	return manifest, objects
}

// manifestNamesUpstream says whether the bundle's manifest bytes carry the field at all, which a
// v15.0.0 worker would refuse.
func manifestNamesUpstream(t *testing.T, bundle []byte) bool {
	t.Helper()
	outer := tar.NewReader(bytes.NewReader(bundle))
	if header, err := outer.Next(); err != nil || header.Name != "manifest.json" {
		t.Fatalf("bundle does not start with its manifest: %v", err)
	}
	data, err := io.ReadAll(outer)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(data, []byte(`"upstream_revision"`))
}

// rebaseOnUpstream is the incident's shape: the task commits, the upstream gains a commit holding a
// scanner match, and the work moves onto it by a rebase or a merge.
func (c upstreamCheckpointCase) rebaseOnUpstream(t *testing.T, merge bool) (upstream, flagged string) {
	t.Helper()
	workspace := c.source.Workspace
	sessionWorkspaceWrite(t, filepath.Join(workspace, "task.txt"), "task work\n")
	sessionWorkspaceGit(t, workspace, "add", "task.txt")
	sessionWorkspaceGit(t, workspace, "commit", "-qm", "task work")
	upstream = c.advanceUpstream(t, "inherited-fixture.txt", upstreamCheckpointFixture, "upstream fixture")
	sessionWorkspaceGit(t, workspace, "fetch", "-q", c.upstream, "main")
	if merge {
		sessionWorkspaceGit(t, workspace, "merge", "-q", "--no-edit", upstream)
	} else {
		sessionWorkspaceGit(t, workspace, "rebase", "-q", upstream)
	}
	return upstream, gitOut(c.upstream, "rev-parse", upstream+":inherited-fixture.txt")
}

func (c upstreamCheckpointCase) captureBundle(t *testing.T, key string) (CheckpointWorkspaceResult, []byte) {
	t.Helper()
	captured, err := c.capture(t, key)
	if err != nil {
		t.Fatalf("checkpoint refused: %v", err)
	}
	bundle, err := readCheckpointBundle(t, c.service.Service, captured.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return captured, bundle
}

func restoreCheckpoint(worker *sessionFixture, key string, target session.Session, captured CheckpointWorkspaceResult, bundle []byte) (session.Session, error) {
	return worker.RestoreWorkspaceCheckpoint(context.Background(), key, RestoreWorkspaceCheckpointRequest{
		SessionID: target.ID, ExpectedRevision: target.Revision, Checkpoint: captured.Checkpoint, Stream: bytes.NewReader(bundle),
	})
}

func (c upstreamCheckpointCase) target(t *testing.T, worker *sessionFixture, key, offer string) session.Session {
	t.Helper()
	target, err := worker.CreateRemoteSession(context.Background(), key, worker.request(t, offer))
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestCheckpointAfterRebaseOntoAuthorizedUpstreamRestoresOnAFreshWorker(t *testing.T) {
	for _, merge := range []bool{false, true} {
		name := "rebase"
		if merge {
			name = "merge"
		}
		t.Run(name, func(t *testing.T) {
			offer := "offer:" + name
			c := newUpstreamCheckpointCase(t, offer)
			workspace := c.source.Workspace
			upstream, flagged := c.rebaseOnUpstream(t, merge)
			// The default moves on after the rebase: the prerequisite is the commit the work holds.
			c.advanceUpstream(t, "later.txt", "later\n", "later upstream work")
			sessionWorkspaceWrite(t, filepath.Join(workspace, "tracked.txt"), "dirty\n")
			sessionWorkspaceWrite(t, filepath.Join(workspace, "notes.txt"), "untracked\n")

			captured, bundle := c.captureBundle(t, name+"-capture")
			manifest, objects := checkpointPayload(t, bundle)
			if manifest.UpstreamRevision != upstream || manifest.BaseRevision != c.base {
				t.Fatalf("manifest upstream=%q base=%q, want %s on %s", manifest.UpstreamRevision, manifest.BaseRevision, upstream, c.base)
			}
			if objects["objects/blob/"+flagged] || objects["objects/commit/"+upstream] {
				t.Fatal("checkpoint carried objects the authorized upstream already holds")
			}
			if !objects["objects/commit/"+captured.Checkpoint.CommittedRevision] {
				t.Fatal("checkpoint lost the task's commit")
			}

			worker := c.freshWorker(t)
			restored, err := restoreCheckpoint(worker, name+"-restore", c.target(t, worker, name+"-target", offer), captured, bundle)
			if err != nil {
				t.Fatalf("fresh worker did not restore the checkpoint: %v", err)
			}
			if got := gitOut(restored.Workspace, "rev-parse", "HEAD"); got != captured.Checkpoint.CommittedRevision {
				t.Fatalf("restored HEAD %s, want %s", got, captured.Checkpoint.CommittedRevision)
			}
			// Fails the test unless the restored work still holds the upstream it was moved onto.
			sessionWorkspaceGit(t, restored.Workspace, "merge-base", "--is-ancestor", upstream, "HEAD")
			for path, want := range map[string]string{
				"task.txt": "task work\n", "tracked.txt": "dirty\n", "notes.txt": "untracked\n",
				"inherited-fixture.txt": upstreamCheckpointFixture,
			} {
				if got, err := os.ReadFile(filepath.Join(restored.Workspace, path)); err != nil || string(got) != want {
					t.Fatalf("restored %s = %q, %v", path, got, err)
				}
			}
			if pathExists(filepath.Join(restored.Workspace, "later.txt")) {
				t.Fatal("restore moved the work onto a default it was never rebased onto")
			}
		})
	}
}

// A checkpoint that needs nothing past the base keeps the v15.0.0 shape, and restores on a fresh
// worker exactly as such a checkpoint did.
func TestCheckpointWithoutAnUpstreamKeepsTheV15ShapeAndRestores(t *testing.T) {
	c := newUpstreamCheckpointCase(t, "offer:v15")
	sessionWorkspaceWrite(t, filepath.Join(c.source.Workspace, "task.txt"), "task work\n")
	sessionWorkspaceGit(t, c.source.Workspace, "add", "task.txt")
	sessionWorkspaceGit(t, c.source.Workspace, "commit", "-qm", "task work")
	captured, bundle := c.captureBundle(t, "v15-capture")
	if manifestNamesUpstream(t, bundle) || *c.refresh != 0 {
		t.Fatalf("plain capture named an upstream or asked the source (%d refreshes)", *c.refresh)
	}
	worker := c.freshWorker(t)
	worker.sourceRefresher = func(context.Context, string, workerproto.JobSource, string) (string, error) {
		return "", errors.New("a v15-shaped restore must not need the source")
	}
	restored, err := restoreCheckpoint(worker, "v15-restore", c.target(t, worker, "v15-target", "offer:v15"), captured, bundle)
	if err != nil || gitOut(restored.Workspace, "rev-parse", "HEAD") != captured.Checkpoint.CommittedRevision {
		t.Fatalf("v15-shaped restore = %v", err)
	}
}

func TestRebasedCheckpointRestoreRefusesAnUnprovenUpstream(t *testing.T) {
	for _, tc := range []string{"refresh-fails", "grant-refused", "missing", "rewritten"} {
		t.Run(tc, func(t *testing.T) {
			offer := "offer:" + tc
			c := newUpstreamCheckpointCase(t, offer)
			upstream, _ := c.rebaseOnUpstream(t, false)
			captured, bundle := c.captureBundle(t, tc+"-capture")
			rewrite := func() {
				sessionWorkspaceGit(t, c.upstream, "reset", "-q", "--hard", c.base)
				c.advanceUpstream(t, "other.txt", "other\n", "rewritten default")
			}
			if tc == "missing" {
				rewrite() // the fresh worker stages a source that never had the upstream commit
			}
			worker := c.freshWorker(t)
			code := session.CodeRepositoryUnavailable
			if tc == "missing" || tc == "rewritten" {
				// The refresh worked and the default provably lacks the commit: a retry would only
				// spend another controller refresh, so the answer is final.
				code = session.CodeInvalidSessionState
			}
			switch tc {
			case "refresh-fails":
				worker.sourceRefresher = func(context.Context, string, workerproto.JobSource, string) (string, error) {
					return "", errors.New("controller unreachable")
				}
			case "grant-refused":
				code = session.CodeInvalidSessionState
				worker.sourceRefresher = func(context.Context, string, workerproto.JobSource, string) (string, error) {
					return "", workerproto.ErrSourceGrantRefused
				}
			case "rewritten":
				rewrite() // the staged copy still holds the commit, but the default no longer does
			}
			target := c.target(t, worker, tc+"-target", offer)
			refreshes := *c.refresh
			_, err := restoreCheckpoint(worker, tc+"-restore", target, captured, bundle)
			if session.CodeOf(err) != code || !strings.Contains(err.Error(), upstream) {
				t.Fatalf("unproven upstream restore = %v, want %s", err, code)
			}
			if (tc == "missing" || tc == "rewritten") && (!strings.Contains(err.Error(), "no longer holds") || *c.refresh != refreshes+1) {
				t.Fatalf("a commit the default lacks answered %v after %d refreshes, want a final answer after one", err, *c.refresh-refreshes)
			}
			if strings.Contains(err.Error(), "ghp_") {
				t.Fatal("restore refusal repeated credential-shaped content")
			}
			if got := gitOut(target.Workspace, "rev-parse", "HEAD"); got != c.base {
				t.Fatalf("refused restore changed the workspace to %s", got)
			}
			if pathExists(filepath.Join(target.Workspace, "task.txt")) {
				t.Fatal("refused restore wrote task files")
			}
			if tc == "refresh-fails" {
				// Once the source answers again, a new key restores into the same unused session.
				worker.sourceRefresher = c.service.sourceRefresher
				restored, err := restoreCheckpoint(worker, tc+"-restore-again", target, captured, bundle)
				if err != nil || restored.ID != target.ID || gitOut(target.Workspace, "rev-parse", "HEAD") != captured.Checkpoint.CommittedRevision {
					t.Fatalf("retry after a 503 = %v", err)
				}
			}
		})
	}
}

// A session without a usable job authority cannot have its default checked. Capture and restore
// both answer 409 there and name the cause, since asking again cannot give the session a job.
func TestRebasedCheckpointWithoutJobAuthorityAnswers409(t *testing.T) {
	t.Run("capture", func(t *testing.T) {
		c := newUpstreamCheckpointCase(t, "offer:no-job-capture")
		c.rebaseOnUpstream(t, false)
		clearHistoricalJob(t, c.service.Store(), c.source.ID)
		_, err := c.capture(t, "no-job-capture")
		if session.CodeOf(err) != session.CodeInvalidSessionState || !strings.Contains(err.Error(), "cannot be checked for it: session has no durable job authority") ||
			strings.Contains(err.Error(), "ghp_") {
			t.Fatalf("capture without a job authority = %v, want 409", err)
		}
	})
	t.Run("restore", func(t *testing.T) {
		c := newUpstreamCheckpointCase(t, "offer:no-job-restore")
		c.rebaseOnUpstream(t, false)
		captured, bundle := c.captureBundle(t, "no-job-restore-capture")
		worker := c.freshWorker(t)
		target := c.target(t, worker, "no-job-restore-target", "offer:no-job-restore")
		clearHistoricalJob(t, worker.Store(), target.ID)
		_, err := restoreCheckpoint(worker, "no-job-restore", target, captured, bundle)
		if session.CodeOf(err) != session.CodeInvalidSessionState || !strings.Contains(err.Error(), "but the session has no job authority") {
			t.Fatalf("restore without a job authority = %v, want 409", err)
		}
	})
}

// The final answer for a commit the default lost leaves the replacement session unused: once the
// branch holds the commit again (an accidental force-push undone), a new idempotency key restores
// into the same session, while the refused key keeps its answer.
func TestRestoreAfterTheDefaultRegainsTheUpstreamTakesANewKey(t *testing.T) {
	c := newUpstreamCheckpointCase(t, "offer:regained")
	upstream, _ := c.rebaseOnUpstream(t, false)
	captured, bundle := c.captureBundle(t, "regained-capture")
	worker := c.freshWorker(t)
	target := c.target(t, worker, "regained-target", "offer:regained")
	sessionWorkspaceGit(t, c.upstream, "reset", "-q", "--hard", c.base)
	c.advanceUpstream(t, "other.txt", "other\n", "accidental force-push")
	if _, err := restoreCheckpoint(worker, "regained-restore", target, captured, bundle); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("restore after the force-push = %v, want 409", err)
	}
	unused, err := worker.Store().GetSession(context.Background(), target.ID)
	if err != nil || unused.Revision != target.Revision || gitOut(target.Workspace, "rev-parse", "HEAD") != c.base {
		t.Fatalf("the refused restore changed the session: revision %d (was %d), %v", unused.Revision, target.Revision, err)
	}
	sessionWorkspaceGit(t, c.upstream, "reset", "-q", "--hard", upstream)
	if _, err := restoreCheckpoint(worker, "regained-restore", target, captured, bundle); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("the refused key answered %v, want its saved 409", err)
	}
	restored, err := restoreCheckpoint(worker, "regained-restore-again", target, captured, bundle)
	if err != nil || restored.ID != target.ID || gitOut(target.Workspace, "rev-parse", "HEAD") != captured.Checkpoint.CommittedRevision {
		t.Fatalf("a new key after the default regained the commit = %v", err)
	}
}

// Only git's clean "no such object" answer is final. A lookup git could not answer, here a git
// that fails with a message, stays the retryable 503.
func TestRestoreUpstreamLookupFailureStaysRetryable(t *testing.T) {
	c := newUpstreamCheckpointCase(t, "offer:lookup")
	c.rebaseOnUpstream(t, false)
	captured, bundle := c.captureBundle(t, "lookup-capture")
	sessionWorkspaceGit(t, c.upstream, "reset", "-q", "--hard", c.base)
	c.advanceUpstream(t, "other.txt", "other\n", "rewritten default")
	worker := c.freshWorker(t) // stages a source that never had the upstream commit
	target := c.target(t, worker, "lookup-target", "offer:lookup")
	missing := strings.Repeat("1", 40)
	if !checkpointCommitMissing(context.Background(), c.upstream, missing) {
		t.Fatal("git's own answer for a missing commit did not read as missing")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Appendf(nil, "#!/bin/sh\nfor arg in \"$@\"; do [ \"$arg\" = cat-file ] && { echo 'fatal: packfile is corrupt' >&2; exit 128; }; done\nexec %q \"$@\"\n", realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = restoreCheckpoint(worker, "lookup-restore", target, captured, bundle)
	if session.CodeOf(err) != session.CodeRepositoryUnavailable || !strings.Contains(err.Error(), "whose copy could not be read") {
		t.Fatalf("a failed lookup answered %v, want 503", err)
	}
	if checkpointCommitMissing(context.Background(), c.upstream, missing) {
		t.Fatal("a failing git read as a missing commit")
	}
}

// A restore that stopped after it started changing files resumes from its journaled body. It proved
// the upstream before that point, so a default force-pushed in between must not fence it forever.
func TestResumedRebasedRestoreDoesNotNeedTheDefaultAgain(t *testing.T) {
	c := newUpstreamCheckpointCase(t, "offer:resume")
	c.rebaseOnUpstream(t, false)
	captured, bundle := c.captureBundle(t, "resume-capture")
	worker := c.freshWorker(t)
	target := c.target(t, worker, "resume-target", "offer:resume")
	db, err := sql.Open("sqlite", filepath.Join(worker.Store().Root(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER interrupt_restore_completion BEFORE UPDATE ON operations
		WHEN NEW.method = 'RestoreWorkspaceCheckpoint' AND NEW.state = 'succeeded'
		BEGIN SELECT RAISE(ABORT, 'injected completion loss'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreCheckpoint(worker, "resume-restore", target, captured, bundle); err == nil || !strings.Contains(err.Error(), "injected completion loss") {
		t.Fatalf("missing injected completion failure: %v", err)
	}
	if op, err := worker.Store().GetOperation(context.Background(), "resume-restore"); err != nil || op.State != session.OperationRunning {
		t.Fatalf("restore did not stop while running: %+v, %v", op, err)
	}
	if _, err := db.Exec("DROP TRIGGER interrupt_restore_completion"); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, c.upstream, "reset", "-q", "--hard", c.base)
	c.advanceUpstream(t, "other.txt", "other\n", "rewritten default")
	restored, err := restoreCheckpoint(worker, "resume-restore", target, captured, bundle)
	if err != nil || gitOut(restored.Workspace, "rev-parse", "HEAD") != captured.Checkpoint.CommittedRevision {
		t.Fatalf("resumed restore = %v", err)
	}
}

func TestRebasedCheckpointStillRefusesCredentialsTheTaskAdded(t *testing.T) {
	for _, kind := range []string{"committed", "added-then-removed"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c := newUpstreamCheckpointCase(t, "offer:"+kind)
			workspace := c.source.Workspace
			upstream := c.advanceUpstream(t, "inherited-fixture.txt", upstreamCheckpointFixture, "upstream fixture")
			sessionWorkspaceGit(t, workspace, "fetch", "-q", c.upstream, "main")
			sessionWorkspaceGit(t, workspace, "rebase", "-q", upstream)
			// A different value than the upstream's, so its blob is the task's own.
			sessionWorkspaceWrite(t, filepath.Join(workspace, "task-secret.txt"), "token="+upstreamCheckpointFixture)
			sessionWorkspaceGit(t, workspace, "add", "task-secret.txt")
			sessionWorkspaceGit(t, workspace, "commit", "-qm", "task adds a credential")
			added := gitOut(workspace, "rev-parse", "HEAD")
			if kind == "added-then-removed" {
				sessionWorkspaceGit(t, workspace, "rm", "-q", "task-secret.txt")
				sessionWorkspaceGit(t, workspace, "commit", "-qm", "task removes it again")
			}
			head := gitOut(workspace, "rev-parse", "HEAD")
			_, err := c.capture(t, kind+"-capture")
			if session.CodeOf(err) != session.CodeInvalidSessionState || err == nil ||
				!strings.Contains(err.Error(), "neither the session base nor the job's default branch holds") {
				t.Fatalf("task credential was not refused as the task's own: %v", err)
			}
			if strings.Contains(err.Error(), "ghp_") {
				t.Fatal("refusal repeated credential-shaped content")
			}
			// The retry really found the upstream, and what still matched is the task's blob: leaving
			// out the upstream refuses, and leaving out the task's adding commit as well passes.
			defaultHead, err := c.service.pinCurrentSessionParent(ctx, c.source)
			if err != nil {
				t.Fatal(err)
			}
			for _, alsoTask := range []bool{false, true} {
				err := withSessionTrackedTree(ctx, workspace, func(tree string, env []string) error {
					found, err := checkpointUpstream(ctx, workspace, c.source.Repository, c.base, head, defaultHead, env)
					if err != nil || found != upstream {
						t.Fatalf("capture's retry chose upstream %q, %v; want %s", found, err, upstream)
					}
					if alsoTask {
						found = added // a test-only cut: everything up to the task's adding commit stays home
					}
					return writeCheckpointRepository(ctx, workspace, c.base, found, head, tree, env, io.Discard)
				})
				if refused := checkpointCredentialRefusal(err); refused == alsoTask {
					t.Fatalf("export with the task's commit also left out (%t) refused=%t: %v", alsoTask, refused, err)
				}
			}
		})
	}
}

// A refusal the default branch might lift, but that capture could not check, says so: retryable
// while the source is unreachable, final when the controller refuses access.
func TestRebasedCheckpointSaysWhenTheDefaultCouldNotBeChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		code   session.ErrorCode
		detail string
	}{
		"unreachable":   {errors.New("controller unreachable"), session.CodeRepositoryUnavailable, "could not be refreshed"},
		"grant-refused": {workerproto.ErrSourceGrantRefused, session.CodeInvalidSessionState, "asking again will not change that"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newUpstreamCheckpointCase(t, "offer:"+name)
			c.rebaseOnUpstream(t, false)
			c.service.sourceRefresher = func(context.Context, string, workerproto.JobSource, string) (string, error) { return "", tc.err }
			_, err := c.capture(t, name+"-capture")
			if session.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.detail) || strings.Contains(err.Error(), "ghp_") {
				t.Fatalf("unchecked default answered %v, want %s", err, tc.code)
			}
		})
	}
}

// Refs, remotes and config live in the workspace, where the agent writes. Pointing them at a commit
// must not make it an authorized upstream: neither one the agent made, nor a real commit of the
// source that is not on the job's default branch.
func TestWorkspaceRefsDoNotAuthorizeACheckpointUpstream(t *testing.T) {
	for _, origin := range []string{"agent-made", "source-branch"} {
		t.Run(origin, func(t *testing.T) {
			c := newUpstreamCheckpointCase(t, "offer:"+origin)
			workspace := c.source.Workspace
			var chosen string
			if origin == "agent-made" {
				sessionWorkspaceWrite(t, filepath.Join(workspace, "forged.txt"), upstreamCheckpointFixture)
				sessionWorkspaceGit(t, workspace, "add", "forged.txt")
				sessionWorkspaceGit(t, workspace, "commit", "-qm", "forged upstream")
				chosen = gitOut(workspace, "rev-parse", "HEAD")
			} else {
				// A real commit of the source on another branch, which the worker's own copy holds.
				sessionWorkspaceGit(t, c.upstream, "checkout", "-q", "-b", "feature")
				chosen = c.advanceUpstream(t, "feature.txt", upstreamCheckpointFixture, "feature branch")
				sessionWorkspaceGit(t, c.upstream, "checkout", "-q", "main")
				c.advanceUpstream(t, "unrelated.txt", "unrelated\n", "default moves on")
				sessionWorkspaceGit(t, c.source.Repository, "fetch", "-q", c.upstream, "+refs/heads/feature:refs/remotes/origin/feature")
				sessionWorkspaceGit(t, workspace, "fetch", "-q", c.upstream, "feature")
				sessionWorkspaceGit(t, workspace, "reset", "-q", "--hard", chosen)
			}
			for _, ref := range []string{"refs/remotes/origin/main", "refs/heads/main", "refs/coop/current-default", "refs/coop/session-parent"} {
				sessionWorkspaceGit(t, workspace, "update-ref", ref, chosen)
			}
			sessionWorkspaceGit(t, workspace, "config", "branch.main.merge", "refs/heads/main")
			sessionWorkspaceWrite(t, filepath.Join(workspace, "task.txt"), "task work\n")
			sessionWorkspaceGit(t, workspace, "add", "task.txt")
			sessionWorkspaceGit(t, workspace, "commit", "-qm", "task work on the chosen upstream")
			_, err := c.capture(t, origin+"-capture")
			if err == nil || !strings.Contains(err.Error(), "neither the session base nor the job's default branch holds") {
				t.Fatalf("a workspace ref authorized %s: %v", origin, err)
			}
			if *c.refresh == 0 {
				t.Fatal("capture never consulted the job's own source")
			}
		})
	}
}

// A bundle whose bytes match its descriptor but whose manifest breaks the contract is the
// controller's fault: no retry of the same bytes helps, so it answers 400, the same on replay,
// with a detail that quotes nothing from the bundle.
func TestRestoreOfAMalformedManifestAnswersInvalidRequest(t *testing.T) {
	c := newUpstreamCheckpointCase(t, "offer:malformed")
	upstream, _ := c.rebaseOnUpstream(t, false)
	captured, bundle := c.captureBundle(t, "malformed-capture")
	// Same length, so the tar header stays exact: an upstream equal to the base is invalid.
	field := []byte(`"upstream_revision":"` + upstream + `"`)
	if bytes.Count(bundle, field) != 1 {
		t.Fatalf("bundle manifest does not name upstream %s once", upstream)
	}
	malformed := bytes.Replace(bundle, field, []byte(`"upstream_revision":"`+c.base+`"`), 1)
	checkpoint := captured.Checkpoint
	digest := sha256.Sum256(malformed)
	checkpoint.Bundle.SHA256 = hex.EncodeToString(digest[:])
	malformedCapture := captured
	malformedCapture.Checkpoint = checkpoint
	worker := c.freshWorker(t)
	target := c.target(t, worker, "malformed-target", "offer:malformed")
	for _, attempt := range []string{"first", "replay"} {
		_, err := restoreCheckpoint(worker, "malformed-restore", target, malformedCapture, malformed)
		var answer *session.Error
		if !errors.As(err, &answer) || answer.Code != session.CodeInvalidRequest || answer.Detail != "checkpoint bundle is invalid" {
			t.Fatalf("%s malformed manifest restore = %v, want 400 invalid_request", attempt, err)
		}
	}
	if got := gitOut(target.Workspace, "rev-parse", "HEAD"); got != c.base {
		t.Fatalf("refused restore changed the workspace to %s", got)
	}
}
