package sessionsvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestControllerBookkeepingExclusionsNeverHideUserWork(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer service.Stop()
	// A session's external task reference and its approved offer are distinct API identities.
	// Preflight must preserve the established binding contract while keeping user files visible.
	sess, err := service.CreateRemoteSession(t.Context(), "create-task-visibility", service.request(t, "visibility-session"))
	if err != nil {
		t.Fatal(err)
	}
	draft := tasks.ControllerTaskDraft{OfferRef: "record:task_offer:visibility", Title: "Keep user work visible",
		Prompt: "Change only the intended product file.", SuccessChecks: []string{"focused tests pass"}}
	request := EnsureWorkspaceTaskRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Task: draft}
	bound, err := service.EnsureWorkspaceTask(t.Context(), "ensure-task-visibility", request)
	if err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(bound.Workspace, tasks.TasksRoot)
	for _, state := range tasks.TaskStates {
		item, _ := mustCurrentTask(t, queue, bound.WorkspaceTask.ID)
		if item.State != state {
			if err := tasks.MoveTaskDir(queue, item, state); err != nil {
				t.Fatal(err)
			}
		}
		if status := gitOut(bound.Workspace, "status", "--porcelain", "--untracked-files=all"); status != "" {
			t.Fatalf("generated task in %s is dirty:\n%s", state, status)
		}
	}
	item, _ := mustCurrentTask(t, queue, bound.WorkspaceTask.ID)
	// Git's ordinary filename patterns also match directories. Metadata basenames must not
	// conceal unexpected descendants if an agent replaces a generated file with a folder.
	for _, path := range []string{filepath.Join(item.Dir, "log.md"), filepath.Join(queue, ".coop-queue.json")} {
		original := path + ".original"
		if err := os.Rename(path, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "unexpected.txt"), []byte("user file\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(bound.Workspace, filepath.Join(path, "unexpected.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if status := gitOut(bound.Workspace, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(status, filepath.ToSlash(relative)) {
			t.Fatalf("metadata filename pattern concealed a directory descendant:\n%s", status)
		}
		if status, truncated, err := sessionWorkspaceStatusContext(t.Context(), bound.Workspace); err != nil || truncated || !strings.Contains(string(status), filepath.ToSlash(relative)) {
			t.Fatalf("host status concealed a metadata directory descendant: %q, truncated=%t err=%v", status, truncated, err)
		}
		if err := os.Remove(filepath.Join(path, "unexpected.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(original, path); err != nil {
			t.Fatal(err)
		}
	}
	visible := []string{
		".agent/user-notes.md", ".agent/tasks/00_todo/unrelated/task.md",
		filepath.ToSlash(filepath.Join(tasks.TasksRoot, item.State, item.ID, "unexpected.txt")),
	}
	for _, name := range visible {
		path := filepath.Join(bound.Workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("user work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	status := gitOut(bound.Workspace, "status", "--porcelain", "--untracked-files=all")
	for _, name := range visible {
		if !strings.Contains(status, name) {
			t.Fatalf("local exclusions hid user path %s:\n%s", name, status)
		}
	}
	hostStatus, truncated, err := sessionWorkspaceStatusContext(t.Context(), bound.Workspace)
	if err != nil || truncated {
		t.Fatalf("host status = %q, truncated=%t, err=%v", hostStatus, truncated, err)
	}
	for _, name := range visible {
		if !strings.Contains(string(hostStatus), name) {
			t.Fatalf("host status hid user path %s: %q", name, hostStatus)
		}
	}
	tracked := filepath.ToSlash(filepath.Join(tasks.TasksRoot, item.State, item.ID, "log.md"))
	git("-C", bound.Workspace, "add", "--force", "--", tracked)
	git("-C", bound.Workspace, "commit", "-qm", "track explicit user change")
	file, err := os.OpenFile(filepath.Join(bound.Workspace, tracked), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("user edit\n")
	if err := file.Close(); err != nil || writeErr != nil {
		t.Fatalf("write tracked user edit: %v, %v", writeErr, err)
	}
	if status := gitOut(bound.Workspace, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(status, tracked) {
		t.Fatalf("local exclusions hid a tracked modification:\n%s", status)
	}
	before, err := os.ReadFile(filepath.Join(bound.Workspace, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureWorkspaceTask(t.Context(), "ensure-task-visibility", request); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(bound.Workspace, ".git", "info", "exclude"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("operation replay rewrote exclusions: %v", err)
	}
}

func TestRedirectedLocalExclusionsCannotPublishATaskBinding(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(t.Context(), "create-task-exclude-redirect", service.request(t, "record:task_offer:redirect"))
	if err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(sess.Workspace, ".git", "info", "exclude")
	if err := os.Rename(exclude, exclude+".original"); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canary, []byte("unchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, exclude); err != nil {
		t.Fatal(err)
	}
	_, err = service.EnsureWorkspaceTask(t.Context(), "ensure-task-exclude-redirect", EnsureWorkspaceTaskRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision,
		Task: tasks.ControllerTaskDraft{OfferRef: "record:task_offer:redirect", Title: "Reject a redirected exclude",
			Prompt: "Never write through a symlink.", SuccessChecks: []string{"focused tests pass"}},
	})
	if session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("redirected exclusions accepted: %v", err)
	}
	current := mustSession(t, service, sess.ID)
	if current.Revision != sess.Revision || current.WorkspaceTask != nil {
		t.Fatalf("failed exclusions published a binding: %+v", current)
	}
	if body, err := os.ReadFile(canary); err != nil || string(body) != "unchanged\n" {
		t.Fatalf("redirect changed its target: %q, %v", body, err)
	}
}

func TestRefusedTaskBindingCannotCreateOrHideAnotherProjection(t *testing.T) {
	for _, refusal := range []string{"stale revision", "different binding", "busy runtime", "queued turn", "active turn", "read only"} {
		t.Run(refusal, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
			repo, git := gitrepo.New(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
			defer service.Stop()
			if refusal == "read only" {
				service.Job.RepositoryReadOnly = true
			}
			sess, err := service.CreateRemoteSession(t.Context(), "create-task-refusal", service.request(t, "record:task_offer:refusal"))
			if err != nil {
				t.Fatal(err)
			}
			draft := tasks.ControllerTaskDraft{OfferRef: "record:task_offer:refusal", Title: "Keep rejected work visible",
				Prompt: "Do not create task custody for an unbound request.", SuccessChecks: []string{"focused tests pass"}}
			request := EnsureWorkspaceTaskRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Task: draft}
			if refusal == "different binding" {
				sess, err = service.EnsureWorkspaceTask(t.Context(), "ensure-original-binding", request)
				if err != nil {
					t.Fatal(err)
				}
				request.ExpectedRevision = sess.Revision
				request.Task.OfferRef = "record:task_offer:foreign"
			} else if refusal == "stale revision" {
				request.ExpectedRevision++
			} else if refusal == "busy runtime" {
				service.parkedRuntimeWait = 10 * time.Millisecond
				release := service.lockSessionRuntime(sess.ID)
				defer release()
			} else if refusal == "queued turn" || refusal == "active turn" {
				if _, err := service.Store().SubmitTurn(t.Context(), "queue-before-task", session.SubmitTurnRequest{
					SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "Read only",
				}); err != nil {
					t.Fatal(err)
				}
				if refusal == "active turn" {
					if _, ok, err := service.Store().LeaseNextTurn(t.Context(), sess.ID); err != nil || !ok {
						t.Fatalf("fixture turn lease: ok=%t err=%v", ok, err)
					}
				}
				sess = mustSession(t, service, sess.ID)
				request.ExpectedRevision = sess.Revision
			}
			exclude := filepath.Join(sess.Workspace, ".git", "info", "exclude")
			before, err := os.ReadFile(exclude)
			if err != nil {
				t.Fatal(err)
			}
			queue := filepath.Join(sess.Workspace, tasks.TasksRoot, tasks.StateTodo)
			beforeTasks, err := os.ReadDir(queue)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if _, err := service.EnsureWorkspaceTask(t.Context(), "ensure-refused-binding", request); err == nil {
				t.Fatal("refused task binding was accepted")
			}
			afterTasks, err := os.ReadDir(queue)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(afterTasks) != len(beforeTasks) {
				t.Fatalf("refused request created a second projection: before=%d after=%d", len(beforeTasks), len(afterTasks))
			}
			if after, err := os.ReadFile(exclude); err != nil || string(after) != string(before) {
				t.Fatalf("refused request hid unbound task files: %v", err)
			}
		})
	}
}

func TestExactBoundTaskRecoveryPublishesOnlyItsDurableReceipt(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(t.Context(), "create-bound-recovery", service.request(t, "record:task_offer:bound-recovery"))
	if err != nil {
		t.Fatal(err)
	}
	request := EnsureWorkspaceTaskRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision,
		Task: tasks.ControllerTaskDraft{OfferRef: "record:task_offer:bound-recovery", Title: "Recover exact binding",
			Prompt: "Keep the immutable task identity.", SuccessChecks: []string{"focused tests pass"}}}
	bound, err := service.EnsureWorkspaceTask(t.Context(), "ensure-bound-first", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store().SubmitTurn(t.Context(), "use-before-binding-recovery", session.SubmitTurnRequest{
		SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "Read only",
	}); err != nil {
		t.Fatal(err)
	}
	used := mustSession(t, service, bound.ID)
	exclude := filepath.Join(bound.Workspace, ".git", "info", "exclude")
	if err := os.Rename(exclude, exclude+".original"); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canary, []byte("unchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, exclude); err != nil {
		t.Fatal(err)
	}
	// A row binding may commit before its outer operation receipt. The stale revision,
	// used session and unusable Git metadata must not make that exact identity unrecoverable.
	for _, key := range []string{"ensure-bound-fresh", "ensure-bound-running"} {
		if key == "ensure-bound-running" {
			op, _, err := service.Store().ReserveOperation(t.Context(), "EnsureWorkspaceTask", key, request)
			if err != nil {
				t.Fatal(err)
			}
			intent, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Store().MarkOperationRunning(t.Context(), op.ID, intent); err != nil {
				t.Fatal(err)
			}
		}
		recovered, err := service.EnsureWorkspaceTask(t.Context(), key, request)
		if err != nil || recovered.Revision != used.Revision || recovered.WorkspaceTask == nil || *recovered.WorkspaceTask != *bound.WorkspaceTask {
			t.Fatalf("exact bound recovery lost custody: recovered=%+v err=%v", recovered, err)
		}
		if body, err := os.ReadFile(canary); err != nil || string(body) != "unchanged\n" {
			t.Fatalf("receipt recovery changed filesystem: %q err=%v", body, err)
		}
	}
}
