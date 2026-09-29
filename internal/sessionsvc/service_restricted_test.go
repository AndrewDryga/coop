package sessionsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// modeRecordingRunner remembers the session every turn ran under, and completes the turn so
// nothing is left for a restart to reap.
type modeRecordingRunner struct {
	store *session.Store
	mu    sync.Mutex
	modes []string
}

func (r *modeRecordingRunner) Run(ctx context.Context, sess session.Session, turn session.Turn) (session.Turn, error) {
	r.mu.Lock()
	r.modes = append(r.modes, sess.Mode)
	r.mu.Unlock()
	return r.store.CompleteTurn(ctx, session.CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, Message: "OK"})
}

func (r *modeRecordingRunner) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.modes...)
}

// A bare create runs no Git and makes no workspace: the session row carries the mode and no
// repository binding, its posture is the policy's own, the public projection says "bare", and a
// restarted daemon runs its turns under the same mode — read off the row, never off the policy.
func TestABarePolicyCreatesAWorkspacelessSessionThatSurvivesARestart(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	runner := &modeRecordingRunner{}
	service := newTestSessionService(t, stateRoot, "", func(store *session.Store) Runner {
		runner.store = store
		return runner
	})
	created, err := service.CreateRemoteSession(context.Background(), "bare-create", service.request(t, "route"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Mode != "bare" || created.Repository != "" || created.Workspace != "" || created.ForkName != "" ||
		created.ForkGeneration != "" || created.BaseCommit != "" || len(created.RepositoryFreshness) != 0 ||
		created.NetworkMode != string(egress.Open) || created.ProjectEnv || created.ProjectMCP {
		t.Fatalf("bare session = %+v", created)
	}
	if entries, err := os.ReadDir(stateRoot); err != nil {
		t.Fatal(err)
	} else {
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "fork") {
				t.Fatalf("bare create left repository-shaped state: %v", entries)
			}
		}
	}
	replayed, err := service.CreateRemoteSession(context.Background(), "bare-create", service.request(t, "route"))
	if err != nil || replayed.ID != created.ID || replayed.Mode != "bare" {
		t.Fatalf("bare replay = %+v, %v", replayed, err)
	}
	wire, err := json.Marshal(publicSession(created))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(`"mode":"bare"`)) || !bytes.Contains(wire, []byte(`"fork_name":""`)) {
		t.Fatalf("public bare session = %s", wire)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SubmitTurn(context.Background(), "bare-turn-1", session.SubmitTurnRequest{
		SessionID: created.ID, ExpectedRevision: created.Revision, Prompt: "first",
	}); err != nil {
		t.Fatal(err)
	}
	waitForSessionTest(t, func() bool { return len(runner.seen()) == 1 })
	if err := service.Stop(); err != nil {
		t.Fatal(err)
	}

	// Restart with no registry; execution still comes from the admitted job.
	restarted := &modeRecordingRunner{}
	service = newTestSessionService(t, stateRoot, "", func(store *session.Store) Runner {
		restarted.store = store
		return restarted
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := service.GetSession(context.Background(), created.ID)
	if err != nil || current.Mode != "bare" || current.Workspace != "" {
		t.Fatalf("restarted bare session = %+v, %v", current, err)
	}
	if _, err := service.SubmitTurn(context.Background(), "bare-turn-2", session.SubmitTurnRequest{
		SessionID: created.ID, ExpectedRevision: current.Revision, Prompt: "second",
	}); err != nil {
		t.Fatal(err)
	}
	waitForSessionTest(t, func() bool { return len(restarted.seen()) == 1 })
	if got := append(runner.seen(), restarted.seen()...); got[0] != "bare" || got[1] != "bare" {
		t.Fatalf("turns ran under modes %v, want bare before and after the restart", got)
	}
}

// Every repository-specific operation refuses a bare session with one sentence and a state
// conflict, over the wire; the lifecycle operations that need no workspace keep working, and
// the discard of a workspace-less session retires it without touching anything on disk.
func TestABareSessionRefusesRepositoryOperationsAndStillClosesAndDiscards(t *testing.T) {
	service := newSessionFixture(t, Config{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		RunnerFactory: func(store *session.Store) Runner {
			return RunnerFunc(func(ctx context.Context, sess session.Session, turn session.Turn) (session.Turn, error) {
				return store.CompleteTurn(ctx, session.CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, Message: "OK"})
			})
		},
		ReviewGate: ReviewGateFunc(func(context.Context, ReviewGateRequest) (ReviewGateResult, error) {
			return ReviewGateResult{Configured: true, Passed: true}, nil
		}),
	}, "")
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	handler := NewHTTPHandler(service.Service)
	post := func(path, body, key string) *httptest.ResponseRecorder {
		return sessionHTTPTestRequest(t, handler, http.MethodPost, path, body, key, "application/json")
	}
	get := func(path string) *httptest.ResponseRecorder {
		return sessionHTTPTestRequest(t, handler, http.MethodGet, path, "", "", "")
	}
	createdResponse := post("/v1/sessions", service.body(t, "route"), "bare-route-create")
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Session.Mode != "bare" || !bytes.Contains(createdResponse.Body.Bytes(), []byte(`"mode":"bare"`)) {
		t.Fatalf("created bare session over the wire = %s", createdResponse.Body.String())
	}
	sessionID := created.Session.ID
	const refusal = "session has no workspace: its execution mode is bare"
	refused := func(name string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), refusal) ||
			!strings.Contains(response.Body.String(), `"invalid_session_state"`) {
			t.Fatalf("%s: status = %d body=%s; want 409 with %q", name, response.Code, response.Body.String(), refusal)
		}
	}
	refused("changes", get("/v1/sessions/"+sessionID+"/changes"))
	refused("changes page", get("/v1/sessions/"+sessionID+"/changes?patch_offset=0&patch_limit=100"))
	refused("workspace task", post("/v1/sessions/"+sessionID+"/workspace",
		`{"expected_revision":1,"task":{"offer_ref":"record:task_offer:routes","title":"Route task","prompt":"Exercise every route.","success_checks":["route passes"],"authority_limits":[],"source_refs":[]}}`,
		"bare-workspace"))
	refused("checkpoint", post("/v1/sessions/"+sessionID+"/checkpoint",
		`{"session_ref":"ref","expected_revision":1,"placement_generation":1,"repository_ref":"repo"}`, "bare-checkpoint"))
	// Review refuses on its own bound-fork check, which a workspace-less session cannot pass.
	if response := post("/v1/sessions/"+sessionID+"/review", `{"expected_revision":1}`, "bare-review"); response.Code < 400 ||
		!strings.Contains(response.Body.String(), "not its exact bound fork") {
		t.Fatalf("review: status = %d body=%s", response.Code, response.Body.String())
	}
	// A restore is refused on the session before any workspace could be rewritten.
	if err := service.Service.validateRestoreWorkspaceSession(context.Background(), created.Session.toRecord(t, service.Service), RestoreWorkspaceCheckpointRequest{}, false); err == nil ||
		!strings.Contains(err.Error(), refusal) {
		t.Fatalf("restore: %v", err)
	}
	// The bindings a bare session cannot carry are refused at admission, by name.
	binding := `{"endpoint":"https://responder.example/v1/state-tools/mcp","token":"` + strings.Repeat("b", 48) + `"}`
	boundRequest := service.request(t, "bound")
	boundRequest.ControllerTools = &session.ControllerTools{Endpoint: "https://responder.example/v1/state-tools/mcp", Token: strings.Repeat("b", 48)}
	boundBody, err := json.Marshal(boundRequest)
	if err != nil {
		t.Fatal(err)
	}
	if response := post("/v1/sessions", string(boundBody), "bare-binding"); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "bare session cannot bind an MCP endpoint") {
		t.Fatalf("bare responder binding: status = %d body=%s", response.Code, response.Body.String())
	}
	if response := post("/v1/sessions/"+sessionID+"/turns", `{"expected_revision":1,"prompt":"x","controller_tools":`+binding+`}`, "bare-turn-binding"); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "binds no controller MCP endpoint") {
		t.Fatalf("bare turn binding: status = %d body=%s", response.Code, response.Body.String())
	}
	schema := `{"type":"object"}`
	schemaSum := sha256.Sum256([]byte(schema))
	digest := hex.EncodeToString(schemaSum[:])
	if response := post("/v1/sessions/"+sessionID+"/turns", `{"expected_revision":1,"prompt":"x","output_contract":{"json_schema":`+schema+`,"sha256":"`+digest+`","require_semantic_validation":true}}`, "bare-semantic"); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "require_semantic_validation is not available") {
		t.Fatalf("bare semantic validation: status = %d body=%s", response.Code, response.Body.String())
	}

	// What a bare session can do: run a turn, be closed, be discarded.
	turnResponse := post("/v1/sessions/"+sessionID+"/turns", `{"expected_revision":1,"prompt":"route"}`, "bare-turn")
	if turnResponse.Code != http.StatusOK {
		t.Fatalf("turn status = %d body=%s", turnResponse.Code, turnResponse.Body.String())
	}
	if response := get("/v1/sessions/" + sessionID + "/network"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"mode":"open"`) {
		t.Fatalf("network status = %d body=%s", response.Code, response.Body.String())
	}
	waitForSessionTest(t, func() bool {
		current, err := service.GetSession(context.Background(), sessionID)
		return err == nil && current.Activity == session.ActivityParked && current.TurnsUsed == 1
	})
	current, err := service.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	closeResponse := post("/v1/sessions/"+sessionID+"/close", fmt.Sprintf(`{"expected_revision":%d}`, current.Revision), "bare-close")
	if closeResponse.Code != http.StatusOK {
		t.Fatalf("close status = %d body=%s", closeResponse.Code, closeResponse.Body.String())
	}
	var closed sessionMutationSessionResponse
	if err := json.Unmarshal(closeResponse.Body.Bytes(), &closed); err != nil {
		t.Fatal(err)
	}
	planResponse := post("/v1/sessions/"+sessionID+"/discard-plan", fmt.Sprintf(`{"expected_revision":%d}`, closed.Session.Revision), "bare-plan")
	if planResponse.Code != http.StatusOK {
		t.Fatalf("discard plan status = %d body=%s", planResponse.Code, planResponse.Body.String())
	}
	var planned sessionMutationPlanResponse
	if err := json.Unmarshal(planResponse.Body.Bytes(), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Plan.OperationID == "" || planned.Plan.Plan.Workspace.Branch != "" {
		t.Fatalf("bare discard plan = %+v", planned)
	}
	discardResponse := post("/v1/sessions/"+sessionID+"/discard", fmt.Sprintf(`{"plan_operation_id":%q}`, planned.Plan.OperationID), "bare-discard")
	if discardResponse.Code != http.StatusOK || !strings.Contains(discardResponse.Body.String(), `"state":"discarded"`) {
		t.Fatalf("discard status = %d body=%s", discardResponse.Code, discardResponse.Body.String())
	}
}

// toRecord fetches the durable record behind a public session, for a service-level check.
func (dto SessionDTO) toRecord(t *testing.T, service *Service) session.Session {
	t.Helper()
	sess, err := service.GetSession(context.Background(), dto.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// A readonly policy pins and forks exactly as a normal one — the fork is what the readonly box
// mounts — and its session carries both the mode and the implied read-only bit.
func TestAReadOnlyPolicyCreatesAForkedSessionUnderItsMode(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	service.Job.Mode, service.Job.RepositoryReadOnly, service.Job.Targets = "readonly", true, []string{"claude@work"}
	defer service.Stop()
	created, err := service.CreateRemoteSession(context.Background(), "readonly-create", service.request(t, "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Mode != "readonly" || !created.RepositoryReadOnly || created.Workspace == "" || created.BaseCommit == "" ||
		created.NetworkMode != string(egress.Open) {
		t.Fatalf("readonly session = %+v", created)
	}
	wire, err := json.Marshal(publicSession(created))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(`"mode":"readonly"`)) || !bytes.Contains(wire, []byte(`"repository_read_only":true`)) {
		t.Fatalf("public readonly session = %s", wire)
	}
	if _, err := service.GetChanges(context.Background(), created.ID); err != nil {
		t.Fatalf("a readonly session has a workspace to diff: %v", err)
	}
}

// A readonly policy that wrote no posture inherits the project's remembered one. When that
// resolves to filtered at create time — an approval landed after the policy loaded — the create
// is refused with its reason, because the restricted profile is not qualified under a gateway.
func TestARestrictedSessionRefusesAFilteredPostureAtCreate(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	service.Job.Mode, service.Job.RepositoryReadOnly, service.Job.Targets = "readonly", true, []string{"claude@work"}
	defer service.Stop()
	service.testAdmitNetwork = func(string, string, executionConfig, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{Mode: egress.Filtered, Fingerprint: strings.Repeat("f", 64)}, nil
	}
	_, err := service.CreateRemoteSession(context.Background(), "readonly-filtered", service.request(t, "inspect"))
	if session.CodeOf(err) != session.CodeNetworkUnavailable || !strings.Contains(err.Error(), "restricted sessions do not support filtered networking") {
		t.Fatalf("filtered readonly create = %v", err)
	}
	sessions, err := service.ListSessions(context.Background(), 10)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("a refused create left a session: %+v, %v", sessions, err)
	}
}
