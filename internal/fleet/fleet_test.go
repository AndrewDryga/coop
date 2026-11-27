package fleet_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fleet"
	"github.com/AndrewDryga/coop/internal/testutil/workertls"
	"github.com/AndrewDryga/coop/internal/workerconnector"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestControllerConnector(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	c, err := fleet.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ca := workertls.NewCA(t)
	server := c.Server("")
	srv := httptest.NewUnstartedServer(server.Handler)
	srv.TLS = server.TLSConfig
	srv.TLS.Certificates = []tls.Certificate{ca.ServerCertificate(t)}
	srv.StartTLS()
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err = os.WriteFile(caFile, ca.PEM, 0600); err != nil {
		t.Fatal(err)
	}
	enroll := func(id, token string, renew time.Duration) *workerconnector.HTTPTransport {
		t.Helper()
		d := t.TempDir()
		p := filepath.Join(d, "token")
		if err := os.WriteFile(p, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		tr, err := workerconnector.NewHTTPTransport(workerconnector.HTTPTransportConfig{BaseURL: srv.URL, CAFile: caFile, EnrollmentTokenFile: p, IdentityFile: filepath.Join(d, "worker.json"), RenewBefore: renew})
		if err != nil {
			t.Fatal(err)
		}
		if id != "" {
			got, e := tr.Identity(context.Background())
			if e != nil || got.ID != id || got.WorkspaceRef != "test" {
				t.Fatalf("enroll %s: %v %v", id, got, e)
			}
		}
		return tr
	}
	token, err := c.Invite("first", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	first := enroll("first", token, 24*time.Hour)
	if _, err = enroll("", token, time.Hour).Identity(context.Background()); err == nil {
		t.Fatal("replayed enrollment")
	}
	token, err = c.Invite("second", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	second := enroll("second", token, time.Hour)
	job := workerproto.JobSpec{Version: 2, JobRef: "job-1", Targets: []string{"claude@work"}, Companions: []workerproto.JobCompanion{}, Mode: "normal", RepositoryReadOnly: true, Egress: workerproto.JobEgress{Mode: "open", Rules: []workerproto.JobRule{}}, Environment: map[string]string{}, Check: workerproto.JobCheck{Argv: []string{}, Environment: map[string]string{}}, Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256}, Limits: workerproto.JobLimits{MaxTurns: 2, MaxQueuedTurns: 1, MaxQueuedBytes: 1 << 20, TurnTimeoutMS: 60000, MaxPatchBytes: 1 << 20}}
	var vectors struct {
		Vectors []struct{ Job workerproto.JobSpec }
	}
	fixture, e := os.ReadFile("../../testdata/protocol/job-digest-v2.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(fixture, &vectors); e != nil {
		t.Fatal(e)
	}
	job.Source = vectors.Vectors[0].Job.Source
	for _, id := range []string{"job-1", "job-2"} {
		job.JobRef = id
		b, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Submit("test", b, "write a greeting"); err != nil {
			t.Fatal(err)
		}
	}
	poll := workerproto.Poll{Version: 2, PollRef: "first-1", Worker: workerproto.WorkerHello{ID: "first", WorkspaceRef: "test", ProtocolVersion: "2", BuildVersion: "test", ClockAt: time.Now(), State: "eligible", SandboxDigest: strings.Repeat("a", 64), Capabilities: []workerproto.Capability{{Name: "job-setup", Version: "2"}}, Capacity: workerproto.Capacity{State: "eligible", TurnSlotsFree: 1, TurnSlotsTotal: 1}}}
	poll.Worker.AccountUsage = &workerproto.AccountUsage{Version: 1, Accounts: []workerproto.AccountUsageAccount{{Account: "claude@work", SignIn: workerproto.AccountSignedIn, Limits: []workerproto.AccountUsageLimit{}}}}
	send := func(tr *workerconnector.HTTPTransport) workerproto.Command {
		t.Helper()
		out, err := tr.Poll(context.Background(), poll)
		if err != nil || len(out.Commands) != 1 {
			t.Fatalf("poll: %v %v", out, err)
		}
		return out.Commands[0]
	}
	cmd := send(first)
	grant, e := first.FetchJobSourceGrant(context.Background(), "job-1", job.Source.RepositoryIdentity())
	if e != nil || !grant.Public || grant.Token != "" {
		t.Fatal("public grant", e)
	}
	if _, e = second.FetchJobSourceGrant(context.Background(), "job-1", job.Source.RepositoryIdentity()); e == nil {
		t.Fatal("source grant issued to wrong worker")
	}
	wrong := job.Source.RepositoryIdentity()
	wrong.GitHubRepositoryID++
	if _, e = first.FetchJobSourceGrant(context.Background(), "job-1", wrong); e == nil {
		t.Fatal("source grant widened")
	}
	if cmd.SessionRef != "job-1" {
		t.Fatal(cmd)
	}
	if resend := send(first); resend.CommandID != cmd.CommandID {
		t.Fatal("lost command on retry")
	}
	poll.Worker.ID = "second"
	if _, err = first.Poll(context.Background(), poll); err == nil {
		t.Fatal("impersonation allowed")
	}
	if out, e := second.Poll(t.Context(), poll); e != nil || len(out.Commands) != 0 {
		t.Fatalf("concurrent same-account admission: %v %v", out, e)
	}
	if err = c.SetAccountLimit("claude@work", 2); err != nil {
		t.Fatal(err)
	}
	if other := send(second); other.SessionRef != "job-2" {
		t.Fatal("duplicate placement")
	}
	poll.Worker.ID = "first"
	bodies := []string{`{"operation":{"id":"op_1","state":"running"}}`, `{"id":"op_1","state":"succeeded","resource_type":"session","resource_id":"remote_1"}`, `{"id":"remote_1","revision":1}`, `{"turn":{"id":"turn_1","state":"starting"}}`, `{"id":"turn_1","state":"completed","assistant_message":"hello` + strings.Repeat("a", 257<<10) + `"}`}
	for i, body := range bodies {
		response := workerproto.APIResponse{Status: 200, Body: json.RawMessage(body)}
		if i == 0 {
			response.Status = 202
		}
		if i == len(bodies)-1 {
			digest := sha256.Sum256([]byte(body))
			ref := workerproto.BodyReference{SHA256: hex.EncodeToString(digest[:]), ByteSize: int64(len(body))}
			if e = second.UploadResponseBody(t.Context(), cmd.CommandID, ref, strings.NewReader(body)); e == nil {
				t.Fatal("wrong worker uploaded response")
			}
			for range 2 {
				if e = first.UploadResponseBody(t.Context(), cmd.CommandID, ref, strings.NewReader(body)); e != nil {
					t.Fatal(e)
				}
			}
			response.Body = nil
			response.BodyRef = &ref
		}
		res, _ := json.Marshal(response)
		poll.CommandResults = []workerproto.CommandResult{{CommandID: cmd.CommandID, State: "succeeded", OperationKey: cmd.IdempotencyKey, Resource: res}}
		out, err := first.Poll(context.Background(), poll)
		if err != nil || len(out.AcknowledgedResultCommandIDs) != 1 {
			t.Fatalf("receipt %d: %v %v", i, out, err)
		}
		if i < len(bodies)-1 {
			if len(out.Commands) == 0 {
				poll.CommandResults = nil
				deadline := time.Now().Add(5 * time.Second)
				for len(out.Commands) == 0 && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
					out, err = first.Poll(t.Context(), poll)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(out.Commands) != 1 {
				t.Fatal(out)
			}
			cmd = out.Commands[0]
		}
	}
	reopened, err := fleet.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	st, err := reopened.Status("job-1")
	if err != nil || st.Stage != "completed" || st.Worker != "first" || string(st.Output) != bodies[len(bodies)-1] {
		t.Fatal(st, err)
	}
	if err = reopened.Revoke("second"); err != nil {
		t.Fatal(err)
	}
	poll.Worker.ID = "second"
	poll.CommandResults = nil
	if _, err = second.Poll(context.Background(), poll); err == nil {
		t.Fatal("revoked worker admitted")
	}
}
