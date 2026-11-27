package sessionsvc

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fleet"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/workertls"
	"github.com/AndrewDryga/coop/internal/workerconnector"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The model is scripted. Everything between the operator submission and the
// returned answer uses the shipped controller, connector, and session API.
func TestFleetRunsScriptedTurn(t *testing.T) {
	dir := t.TempDir()
	c, err := fleet.Open(filepath.Join(dir, "controller"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ca := workertls.NewCA(t)
	server := c.Server("")
	web := httptest.NewUnstartedServer(server.Handler)
	web.TLS = server.TLSConfig
	web.TLS.Certificates = []tls.Certificate{ca.ServerCertificate(t)}
	web.StartTLS()
	defer web.Close()
	fixture := newSessionFixture(t, Config{StateRoot: filepath.Join(dir, "state"), SourceConfig: &config.Config{}, RunnerFactory: func(store *session.Store) Runner {
		return RunnerFunc(func(ctx context.Context, s session.Session, turn session.Turn) (session.Turn, error) {
			if _, err := store.MarkTurnSendIntent(ctx, s.ID, turn.ID); err != nil {
				return turn, err
			}
			if _, err := store.MarkTurnSent(ctx, s.ID, turn.ID); err != nil {
				return turn, err
			}
			return store.CompleteTurn(ctx, session.CompleteTurnRequest{SessionID: s.ID, TurnID: turn.ID, Message: "scripted reply " + turn.Prompt})
		})
	}}, "")
	defer fixture.Stop()
	fixture.testUsageAccounts = func() []usageAccount {
		return []usageAccount{{provider: "codex", name: "work", signIn: agents.SignedIn}}
	}
	fixture.testAccountQuota = func(context.Context, *config.Config, string, string) (agents.UsageQuota, error) {
		return agents.UsageQuota{}, nil
	}
	if err = fixture.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	root := shortSessionSocketRoot(t)
	sock := filepath.Join(root, "coop.sock")
	l, cleanup, err := ListenSocket(root, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	srv := &http.Server{Handler: NewHTTPHandler(fixture.Service)}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	api, err := workerconnector.NewUnixAPI(sock, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.Invite("demo", "linux", true)
	if err != nil {
		t.Fatal(err)
	}
	cert, seed := filepath.Join(dir, "ca"), filepath.Join(dir, "seed")
	if err = os.WriteFile(cert, ca.PEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(seed, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	tr, err := workerconnector.NewHTTPTransport(workerconnector.HTTPTransportConfig{BaseURL: web.URL, CAFile: cert, EnrollmentTokenFile: seed, IdentityFile: filepath.Join(dir, "identity")})
	if err != nil {
		t.Fatal(err)
	}
	stager, err := workerconnector.NewJobSourceStager(tr, fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer stager.Close()
	executor, err := workerconnector.NewExecutor(workerconnector.ExecutorConfig{API: api, BodyTransport: tr, JobSourceStager: stager, JournalDir: filepath.Join(dir, "journal"), WorkerID: "demo", Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := workerconnector.NewConnector(workerconnector.ConnectorConfig{Executor: executor, Now: time.Now, Transport: tr, Hello: func(ctx context.Context, at time.Time) workerproto.WorkerHello {
		return workerproto.WorkerHello{ID: "demo", WorkspaceRef: "linux", State: "eligible", ProtocolVersion: "2", BuildVersion: "test", SandboxDigest: strings.Repeat("a", 64), ClockAt: at, Capacity: workerconnector.LiveCapacity(ctx, api), Capabilities: workerconnector.LiveCapabilities(ctx, api), AccountUsage: workerconnector.LiveAccountUsage(ctx, api)}
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := fixture.request(t, "scripted")
	if _, err = c.Submit("linux", req.Job, "hello"); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(25 * time.Second)
	for time.Now().Before(end) {
		if err = conn.PollOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		s, err := c.Status("scripted")
		if err != nil {
			t.Fatal(err)
		}
		switch s.Stage {
		case "failed", "uncertain":
			t.Fatalf("turn: %+v", s)
		case "completed":
			if s.Session == "" || s.Turn == "" || !strings.Contains(string(s.Result), "scripted reply hello") {
				t.Fatalf("missing result: %+v", s)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("fleet did not complete scripted turn")
}
