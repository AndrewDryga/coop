package sessionsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

type scriptedModelAccess struct{ grants atomic.Int32 }

func TestControllerModelJobRequiresWorkerModelTransport(t *testing.T) {
	f := newSessionFixture(t, Config{StateRoot: filepath.Join(t.TempDir(), "state"),
		Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) { return turn, nil })}, "")
	defer f.Stop()
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.Job.ModelCredentials = "controller"
	_, err := f.CreateRemoteSessionAsync(t.Context(), "missing-relay", f.request(t, "central-job"))
	var refused *session.Error
	if !errors.As(err, &refused) || refused.Code != session.CodeInvalidRequest {
		t.Fatalf("job without controller model transport was admitted: %v", err)
	}
}

func (s *scriptedModelAccess) ModelGrant(_ context.Context, _ string, b workerproto.ModelGrantRequest) (workerproto.ModelGrant, error) {
	s.grants.Add(1)
	return workerproto.ModelGrant{ModelGrantRequest: b, Token: strings.Repeat("proxy-", 8), Epoch: 1, Selection: "apikey", ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (s *scriptedModelAccess) ForwardModel(_ context.Context, _, _, _, _ string, _ *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: response\n\n"))}, nil
}

func TestModelRelayRequiresFrozenAccountAndRuntimeCustody(t *testing.T) {
	access := &scriptedModelAccess{}
	f := newSessionFixture(t, Config{StateRoot: filepath.Join(t.TempDir(), "state"), SourceConfig: &config.Config{ConfigDir: t.TempDir()},
		ModelAccess: access, Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) { return turn, nil })}, "")
	defer f.Stop()
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.Job.ModelCredentials = "controller"
	f.Job.Targets = []string{"claude:sonnet@work"}
	bound, err := f.CreateRemoteSession(t.Context(), "create", f.request(t, "central-job"))
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(NewHTTPHandler(f.Service))
	defer web.Close()
	post := func(account, digest string) int {
		t.Helper()
		body, _ := json.Marshal(workerproto.ModelGrantRequest{JobDigest: digest, RunID: "run", Provider: "claude", Account: account})
		resp, err := web.Client().Post(web.URL+"/v1/model-access/"+bound.ID+"/grant", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("work", bound.JobDigest); got != 403 {
		t.Fatalf("unreserved runtime received grant: %d", got)
	}
	if ok, _ := f.reserveRuntimeResources(bound.ID, f.Job.Resources); !ok {
		t.Fatal("could not reserve runtime")
	}
	if got := post("other", bound.JobDigest); got != 403 {
		t.Fatalf("wrong account: %d", got)
	}
	if got := post("work", strings.Repeat("f", 64)); got != 403 {
		t.Fatalf("wrong job digest: %d", got)
	}
	if access.grants.Load() != 0 {
		t.Fatal("refused request reached controller")
	}
	if got := post("work", bound.JobDigest); got != 200 {
		t.Fatalf("selected account refused: %d", got)
	}
	if access.grants.Load() != 1 {
		t.Fatal("grant was not acquired")
	}
	f.runtimeCleanupFailed(bound.ID)
	if got := post("work", bound.JobDigest); got != 403 {
		t.Fatalf("uncertain runtime received grant: %d", got)
	}
}
