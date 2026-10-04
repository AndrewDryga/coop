package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// blitz-core's first download took 45 minutes, and the create that needed it was staged again,
// and logged, at every poll (2026-10-03). A command waiting on a first download is tried after
// 5 s, then 10, 20, 40, 80 and 120 s; other commands keep the ordinary pace.
func TestCommandWaitingOnAFirstDownloadBacksOff(t *testing.T) {
	var waits firstDownloadWaits
	start := time.Date(2026, 10, 3, 21, 38, 0, 0, time.UTC)
	downloading := fmt.Errorf("%w: fetch job source: %w", errArtifactTransfer, errJobSourceDownloading)

	now := start
	var tries []time.Duration
	for now.Sub(start) < 10*time.Minute {
		if !waits.held("create", now) {
			tries = append(tries, now.Sub(start))
			waits.settled("create", downloading, now)
		}
		now = now.Add(time.Second) // the controller delivers it again every poll
	}
	want := []time.Duration{0, 5 * time.Second, 15 * time.Second, 35 * time.Second, 75 * time.Second,
		155 * time.Second, 275 * time.Second, 395 * time.Second, 515 * time.Second}
	if fmt.Sprint(tries) != fmt.Sprint(want) {
		t.Fatalf("tried at %v, want %v", tries, want)
	}

	// The download is published: the next try succeeds and the command keeps no wait.
	now = now.Add(2 * time.Minute)
	waits.settled("create", nil, now)
	if waits.held("create", now) || len(waits.waits) != 0 {
		t.Fatalf("a settled command still waits: %+v", waits.waits)
	}

	// Another failure is not a download, and is tried again at the ordinary pace.
	waits.settled("get", errors.New("transport closed"), now)
	if waits.held("get", now) {
		t.Fatal("an ordinary transfer failure was held back")
	}
}

// A wait for a command the controller stopped delivering is dropped on a later settlement.
func TestWaitForAnUndeliveredCommandIsDropped(t *testing.T) {
	var waits firstDownloadWaits
	now := time.Date(2026, 10, 3, 21, 38, 0, 0, time.UTC)
	waits.settled("superseded", errJobSourceDownloading, now)
	waits.settled("other", nil, now.Add(10*time.Minute))
	if _, kept := waits.waits["superseded"]; kept {
		t.Fatal("a wait outlived its command's deliveries")
	}
}

// The whole loop, for 80 seconds of a first download: the controller delivers the create, with a
// fresh lease, at every five-second poll. Run keeps polling, stages the create only after each
// back-off (five times in seventeen polls), and reports the wait once instead of on every try.
// Every poll fsyncs the journal, so the run stays short.
func TestRunStagesACreateWaitingOnAFirstDownloadCalmly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stager := &stillDownloadingStager{}
		executor, err := NewExecutor(ExecutorConfig{API: unreachableAPI{}, JobSourceStager: stager,
			JournalDir: t.TempDir(), Now: time.Now, WorkerID: "worker-a"})
		if err != nil {
			t.Fatal(err)
		}
		command := createWithJobSource(t)
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second+time.Millisecond)
		defer cancel()
		polls := 0
		transport := pollTransportFunc(func(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
			polls++
			if len(poll.CommandResults) != 0 { // a create still waiting on its download has no result
				t.Errorf("poll %d carried results %+v", polls, poll.CommandResults)
				cancel()
			}
			command.LeaseExpiresAt = time.Now().Add(time.Minute)
			return workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: time.Now(),
				Commands: []workerproto.Command{command}}, nil
		})
		connector, err := NewConnector(ConnectorConfig{Executor: executor, Now: time.Now, Transport: transport,
			Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) }})
		if err != nil {
			t.Fatal(err)
		}

		var reports []error
		if err := connector.Run(ctx, 5*time.Second, func(err error) { reports = append(reports, err) }); !errors.Is(err, context.DeadlineExceeded) && !t.Failed() {
			t.Fatalf("run: %v", err)
		}
		if stager.calls != 5 {
			t.Errorf("staged the waiting create %d times in 80 s, want 5 (after 0, 5, 15, 35 and 75 s)", stager.calls)
		}
		if len(reports) != 1 || !errors.Is(reports[0], errJobSourceDownloading) {
			t.Errorf("reported %d times, want the download wait once: %v", len(reports), reports)
		}
		if polls != 17 {
			t.Errorf("polled %d times in 80 s, want 17; the controller needs every poll to renew the lease", polls)
		}
	})
}

type stillDownloadingStager struct{ calls int }

func (s *stillDownloadingStager) Stage(context.Context, string, workerproto.JobSource) error {
	s.calls++
	return fmt.Errorf("%w: fetch job source: %w", errArtifactTransfer, errJobSourceDownloading)
}

// unreachableAPI is the session API a create never reaches while its source downloads.
type unreachableAPI struct{}

func (unreachableAPI) Do(context.Context, Request) (json.RawMessage, error) {
	return nil, errors.New("a create waiting on its download reached the API")
}

func (unreachableAPI) Forward(context.Context, Request, io.Reader) (*http.Response, error) {
	return nil, errors.New("a create waiting on its download reached the API")
}

func createWithJobSource(t *testing.T) workerproto.Command {
	t.Helper()
	job := workerproto.JobSpec{
		Version: 2, JobRef: "job:blitz", Source: ptrJobSource(testJobSource()),
		Companions: []workerproto.JobCompanion{}, Targets: []string{"codex"}, Mode: "readonly",
		RepositoryReadOnly: true, Egress: workerproto.JobEgress{Mode: "none", Rules: []workerproto.JobRule{}},
		Limits: workerproto.JobLimits{MaxTurns: 1, MaxQueuedTurns: 1, MaxQueuedBytes: 4096,
			TurnTimeoutMS: 60_000, MaxPatchBytes: 1024},
		Environment: map[string]string{}, Check: workerproto.JobCheck{Argv: []string{}, Environment: map[string]string{}},
		Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256},
	}
	body, err := json.Marshal(map[string]any{"task": job.JobRef, "job": job})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"method": "POST", "path": "/v1/sessions", "body": json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload = payload
	return command
}
