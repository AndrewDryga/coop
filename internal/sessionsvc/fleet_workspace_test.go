package sessionsvc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestFleetWorkspaceCapture(t *testing.T) {
	source, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(source, "tracked"), "base\n")
	git("add", "tracked")
	git("commit", "-qm", "base")
	base := gitOut(source, "rev-parse", "HEAD")
	sessionWorkspaceWrite(t, filepath.Join(source, "committed"), "unpublished\n")
	git("add", "committed")
	git("commit", "-qm", "local change")
	sessionWorkspaceWrite(t, filepath.Join(source, "tracked"), "staged edit\n")
	git("add", "tracked")
	sessionWorkspaceWrite(t, filepath.Join(source, "tracked"), "worktree edit\n")
	sessionWorkspaceWrite(t, filepath.Join(source, "extra"), "\x00selected\n")
	sessionWorkspaceWrite(t, filepath.Join(source, "unselected"), "do not upload\n")
	index, err := os.ReadFile(filepath.Join(source, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	ref, err := captureFleetWorkspace(t.Context(), source, "job", "project", base, []string{"extra"}, &wire, fixtureCheckpointReserve)
	if err != nil {
		t.Fatal(err)
	}
	var objects, selected bytes.Buffer
	m, err := workerproto.ReadFleetWorkspaceInput(ref, bytes.NewReader(wire.Bytes()), func(h *tar.Header, r io.Reader) error {
		dst := &selected
		if h.Name == "repository.tar" {
			dst = &objects
		}
		_, err := io.Copy(dst, r)
		return err
	})
	if err != nil || m.JobRef != "job" || m.CommittedRevision != gitOut(source, "rev-parse", "HEAD") || len(m.UntrackedFiles) != 1 || string(m.UntrackedFiles[0].PathBytes) != "extra" || selected.String() != "\x00selected\n" {
		t.Fatalf("input: %+v, %v", m, err)
	}
	if !bytes.Contains(objects.Bytes(), []byte("worktree edit")) || !bytes.Contains(objects.Bytes(), []byte("unpublished")) || bytes.Contains(wire.Bytes(), []byte("do not upload")) {
		t.Fatal("wrong source content")
	}
	after, err := os.ReadFile(filepath.Join(source, ".git", "index"))
	if err != nil || !bytes.Equal(index, after) {
		t.Fatal("capture modified the operator index")
	}
	corrupt := bytes.Clone(wire.Bytes())
	corrupt[len(corrupt)-1] = 1
	if _, err := workerproto.ReadFleetWorkspaceInput(ref, bytes.NewReader(corrupt), nil); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	sessionWorkspaceWrite(t, filepath.Join(source, ".env"), "private\n")
	if err := os.Symlink(t.TempDir(), filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"tracked", ".env", ".git/config", "../sibling", "link", "missing"} {
		wire.Reset()
		if _, err := captureFleetWorkspace(t.Context(), source, "job", "project", base, []string{path}, &wire, fixtureCheckpointReserve); err == nil || wire.Len() != 0 {
			t.Fatalf("unsafe selection %q: %v, %d bytes", path, err, wire.Len())
		}
	}
	git("add", "--force", ".env")
	git("commit", "-qm", "hidden intermediate")
	git("rm", "-q", ".env")
	git("commit", "-qm", "remove hidden file")
	if _, err := captureFleetWorkspace(t.Context(), source, "job", "project", base, nil, &wire, fixtureCheckpointReserve); err == nil || wire.Len() != 0 {
		t.Fatalf("transferred hidden historical file: %v", err)
	}
}

func TestFleetWorkspaceInitializesOnlyTheSubmittedJob(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "no-system"))
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "tracked"), "base")
	git("add", "tracked")
	git("commit", "-qm", "approved base")
	stateRoot := filepath.Join(t.TempDir(), "state")
	service := newTestSessionService(t, stateRoot, repo, nil)
	t.Cleanup(func() { _ = service.Stop() })
	base := service.Job.Source.Binding.BaseCommit
	const offer = "operator-fleet-job:ea221256-b948-4971-967c-905dcf2be750"
	sessionWorkspaceWrite(t, filepath.Join(repo, "tracked"), "unpublished")
	git("commit", "-qam", "local commit")
	head := gitOut(repo, "rev-parse", "HEAD")
	sessionWorkspaceWrite(t, filepath.Join(repo, "tracked"), "worktree")
	sessionWorkspaceWrite(t, filepath.Join(repo, "extra"), "binary\x00")
	var wire bytes.Buffer
	ref, err := captureFleetWorkspace(t.Context(), repo, offer, service.Job.Source.RepositoryRef, base, []string{"extra"}, &wire, fixtureCheckpointReserve)
	if err != nil {
		t.Fatal(err)
	}
	service.Job.WorkspaceInput = &ref
	downloads := 0
	var corrupt bool
	service.workspaceInputFetcher = func(ctx context.Context, job string, expected workerproto.BodyReference, dst io.Writer) error {
		downloads++
		if job != "ryker-work:test:session:1" || expected != ref {
			return errors.New("unexpected input grant")
		}
		if corrupt {
			_, err := dst.Write(wire.Bytes()[:wire.Len()/2])
			return err
		}
		_, err := dst.Write(wire.Bytes())
		return err
	}
	create := service.request(t, "ryker-work:test:session:1")
	create.Task = offer
	sess, err := service.CreateRemoteSession(t.Context(), "input-create", create)
	if err != nil {
		t.Fatal(err)
	}
	turn := session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "run"}
	if _, err := service.Store().SubmitTurn(t.Context(), "input-turn", turn); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("turn before initialization: %v", err)
	}
	if _, err := service.sessionExecution(t.Context(), sess); err == nil {
		t.Fatal("runtime permitted before initialization")
	}
	request := EnsureWorkspaceTaskRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Task: tasks.ControllerTaskDraft{OfferRef: offer, Title: "Operate on submitted workspace", Prompt: "Use the selected local changes.", SuccessChecks: []string{"input restored"}}}
	wrong := request
	wrong.Task.OfferRef = "different"
	if _, err := service.EnsureWorkspaceTask(t.Context(), "input-wrong", wrong); err == nil || downloads != 0 {
		t.Fatalf("wrong offer: downloads=%d, err=%v", downloads, err)
	}
	corrupt = true
	if _, err := service.EnsureWorkspaceTask(t.Context(), "input-short", request); err == nil {
		t.Fatal("truncated input accepted")
	}
	if got := gitOut(sess.Workspace, "rev-parse", "HEAD"); got != base {
		t.Fatalf("failed input changed HEAD to %s", got)
	}
	corrupt = false
	// Lose the durable binding after Git and the task directory have been written.
	db, err := sql.Open("sqlite", filepath.Join(service.Store().Root(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER interrupt_input BEFORE UPDATE OF workspace_task ON sessions BEGIN SELECT RAISE(ABORT,'interrupted initial input'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureWorkspaceTask(t.Context(), "input-interrupted", request); err == nil {
		t.Fatal("simulated binding loss was not observed")
	}
	if gitOut(sess.Workspace, "rev-parse", "HEAD") != head {
		t.Fatal("fault did not occur after materialization")
	}
	if _, err := service.Store().SubmitTurn(t.Context(), "interrupted-turn", turn); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("turn on incomplete state: %v", err)
	}
	if _, err := db.Exec("DROP TRIGGER interrupt_input"); err != nil {
		t.Fatal(err)
	}
	fetcher := service.workspaceInputFetcher
	if err := service.Stop(); err != nil {
		t.Fatal(err)
	}
	service = newTestSessionService(t, stateRoot, "", nil)
	service.workspaceInputFetcher = fetcher
	bound, err := service.EnsureWorkspaceTask(t.Context(), "input-bind", request)
	if err != nil {
		t.Fatal(err)
	}
	if bound.WorkspaceTask == nil || bound.WorkspaceTask.OfferRef != offer || gitOut(bound.Workspace, "rev-parse", "HEAD") != head {
		t.Fatal("submitted HEAD or offer missing")
	}
	for path, want := range map[string]string{"tracked": "worktree", "extra": "binary\x00"} {
		got, err := os.ReadFile(filepath.Join(bound.Workspace, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v", path, got, err)
		}
	}
	if got := gitOut(bound.Workspace, "ls-files", "--others", "--exclude-standard", "--", "extra"); got != "extra" {
		t.Fatalf("selected file is no longer untracked: %q", got)
	}
	manifest, err := workerproto.ReadFleetWorkspaceInput(ref, bytes.NewReader(wire.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	untracked := manifest.UntrackedFiles[0]
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s %d %s\n", head, manifest.TrackedTree, manifest.TrackedTree, untracked.PathB64, untracked.Mode, untracked.SHA256))))
	service.sourceRefresher = func(context.Context, string, workerproto.JobSource, string) (string, error) { return base, nil }
	compare := func(equal bool) {
		t.Helper()
		changes, err := service.getWorkspaceChanges(t.Context(), bound.ID, true)
		if err != nil || changes.WorkingSourceDigest == "" || (changes.WorkingSourceDigest == want) != equal {
			t.Fatalf("verification digest %q, expected match %v: %v", changes.WorkingSourceDigest, equal, err)
		}
	}
	compare(true)
	sessionWorkspaceWrite(t, filepath.Join(bound.Workspace, "extra"), "different")
	compare(false)
	sessionWorkspaceWrite(t, filepath.Join(bound.Workspace, "extra"), "binary\x00")
	compare(true)
	sessionWorkspaceWrite(t, filepath.Join(bound.Workspace, "tracked"), "later worker edit")
	compare(false)
	if _, err := service.EnsureWorkspaceTask(t.Context(), "input-bind", request); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(bound.Workspace, "tracked"))
	if downloads != 3 || err != nil || string(got) != "later worker edit" {
		t.Fatalf("retry overwrote later work: %d fetches, %q, %v", downloads, got, err)
	}
	if _, err := service.sessionExecution(t.Context(), bound); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(bound.Workspace, "tracked"), "worktree")
	_, staged, _ := stageCandidateOnSession(t, service, "source-attestation", bound)
	service.runner = &verificationCleanupRunner{cleanup: func() error { return errors.New("simulated cleanup failure") }}
	if proof, err := service.getVerificationChanges(t.Context(), bound.ID, staged.ID, staged.CandidateSHA256); err == nil || proof.WorkingSourceDigest != "" {
		t.Fatal("failed candidate cleanup returned source evidence")
	}
	service.runner = &verificationCleanupRunner{cleanup: func() error {
		return os.WriteFile(filepath.Join(bound.Workspace, "tracked"), []byte("cleanup writer"), 0644)
	}}
	proof, err := service.getVerificationChanges(t.Context(), bound.ID, staged.ID, staged.CandidateSHA256)
	if err != nil || proof.WorkingSourceDigest == "" || proof.WorkingSourceDigest == want {
		t.Fatalf("late cleanup writer omitted from verification: %q, %v", proof.WorkingSourceDigest, err)
	}
	if _, err := service.getVerificationChanges(t.Context(), bound.ID, staged.ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("source evidence returned for stale candidate")
	}
}

type verificationCleanupRunner struct {
	RunnerFunc
	cleanup func() error
}

func (r *verificationCleanupRunner) ReapInterruptedTurn(context.Context, session.Session, session.Turn) error {
	return r.cleanup()
}

func TestFleetWorkspaceRejectsRevertedSetupChanges(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "approved")
	base := gitOut(repo, "rev-parse", "HEAD")
	for _, path := range []string{".agent/project.yaml", ".tool-versions", "compose.yaml", "Dockerfile"} {
		t.Run(path, func(t *testing.T) {
			git("reset", "--hard", base)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0755); err != nil {
				t.Fatal(err)
			}
			sessionWorkspaceWrite(t, filepath.Join(repo, path), "local, unapproved setup")
			git("add", "--force", path)
			git("commit", "-qm", "local setup")
			git("rm", "-q", path)
			git("commit", "-qm", "revert setup")
			m := workerproto.FleetWorkspaceInput{BaseRevision: base, CommittedRevision: gitOut(repo, "rev-parse", "HEAD"), TrackedTree: gitOut(repo, "rev-parse", "HEAD^{tree}")}
			job := workerproto.JobSpec{Runtime: &workerproto.JobRuntime{Compose: "compose.yaml", Dockerfile: "Dockerfile"}}
			err := validateFleetInputPaths(t.Context(), job, repo, repo, m)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("setup history for %q: %v", path, err)
			}
		})
	}
}

// These small test archives have their own budget, independent of the developer's host disk capacity.
func fixtureCheckpointReserve(int64) int64 { return 1 << 20 }
