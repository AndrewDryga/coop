package sessionsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
)

// A worker keeps every session it ever ran, and an eval worker discards hundreds a day. Startup
// resolved the job of each one, discarded or not, with a few git calls per row: the Mac eval
// worker's 992 sessions (961 discarded) kept it past `sessions connect`'s 30 s readiness window
// after a reboot on 2026-09-30, and it could not start at all. A discarded session never runs
// again, so only the others are resolved.
func TestRestartResolvesOnlySessionsThatCanRunAgain(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	open := func() *Service {
		t.Helper()
		service, err := newSessionServiceWithTestStorage(t, Config{
			StateRoot: root, SourceConfig: &config.Config{ConfigDir: t.TempDir()},
			Runner: &periodicCleanupRunner{}, CleanupInterval: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	job := bareWorkerJob()
	document, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := job.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	service := open()
	var live []string
	for i := range 4 {
		req := CreateRemoteSessionRequest{Task: fmt.Sprintf("workspace:restart-%d", i), Job: document, ExpectedJobDigest: digest}
		created, err := service.CreateRemoteSession(ctx, fmt.Sprintf("create-%d", i), req)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			live = append(live, created.ID)
			continue
		}
		closed, err := service.store.CloseSession(ctx, "close-"+created.ID, session.CloseSessionRequest{
			SessionID: created.ID, ExpectedRevision: created.Revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.store.MarkSessionDiscarded(ctx, closed.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Stop(); err != nil {
		t.Fatal(err)
	}
	service = open()
	defer service.Stop()
	var resolved []string
	service.testStartupExecution = func(id string) { resolved = append(resolved, id) }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	slices.Sort(live)
	slices.Sort(resolved)
	if !slices.Equal(resolved, live) {
		t.Fatalf("startup resolved %v; want only the sessions that can run again %v", resolved, live)
	}
}
