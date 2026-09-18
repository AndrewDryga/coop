//go:build networkruntimee2e

package box

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// killedSupervisorEnv hands TestKilledFilteredSupervisor its launch; only the test that kills it sets it.
const killedSupervisorEnv = "COOP_TEST_KILLED_SUPERVISOR"

type killedSupervisorLaunch struct {
	Candidate   networkstate.CandidateSpec `json:"candidate"`
	Repo        string                     `json:"repo"`
	ConfigDir   string                     `json:"config_dir"`
	Fingerprint string                     `json:"fingerprint"`
	RunIDFile   string                     `json:"run_id_file"`
}

// A filtered run's gateway containers and volumes belong to the coop process that launched it, and
// when that process is SIGKILLed only network recovery can remove them: they carry the gateway's
// coop.network.* labels, which the box sweep never reads. This kills a real supervisor while its
// workload runs and again partway through teardown, inspects both label families for what each
// left, and settles them with the call a filtered launch makes before it starts its own gateway.
func TestKilledFilteredRunIsSettledByTheNextLaunch(t *testing.T) {
	trial := os.Getenv("COOP_NETWORK_TRIAL_STATE")
	if trial == "" {
		t.Skip("requires COOP_NETWORK_TRIAL_STATE and a local Docker daemon")
	}
	// Recovery reads the store where NetworkStatePath resolves, so this trial store lives there.
	t.Setenv("XDG_STATE_HOME", filepath.Join(trial, "killed-supervisor"))
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	build, cancelBuild := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancelBuild()
	docker, err := runtime.BindDocker(build, runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := BuildNetworkCandidate(build, docker, nil, nil)
	_ = docker.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name        string
		midTeardown bool
	}{{"while-running", false}, {"mid-teardown", true}} {
		t.Run(c.name, func(t *testing.T) {
			runID := killFilteredSupervisor(t, store, candidate, c.midTeardown)
			boxes, containers, volumes := runLeftovers(t, runID)
			t.Logf("killed %s left boxes %v, gateway containers %v, gateway volumes %v", c.name, boxes, containers, volumes)
			if record, err := store.Execution(runID); err != nil || record.Receipt != nil {
				t.Fatalf("killed run %s = (receipt %+v, %v), want it interrupted with its receipt unsealed", runID, record.Receipt, err)
			}
			if !c.midTeardown && (len(containers) == 0 || len(volumes) == 0) {
				t.Fatalf("a supervisor killed mid-run left containers %v and volumes %v, want both", containers, volumes)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			results, err := RecoverNetworkRuns(ctx, runtime.Runtime{Name: "docker"}, "")
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(results, func(r NetworkRecovery) bool { return r.RunID == runID })
			if i < 0 {
				t.Fatalf("recovery did not find killed run %s: %+v", runID, results)
			}
			if r := results[i]; r.Skipped != "" || len(r.Pending) > 0 || len(r.Failures) > 0 || !r.Sealed {
				t.Fatalf("recovery of %s = %+v, want it settled and sealed", runID, r)
			}
			if boxes, containers, volumes := runLeftovers(t, runID); len(boxes)+len(containers)+len(volumes) > 0 {
				t.Fatalf("after recovery run %s still holds boxes %v, containers %v, volumes %v", runID, boxes, containers, volumes)
			}
			record, err := store.Execution(runID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Receipt == nil || record.Receipt.Finality != "final" || record.Receipt.Workload != "supervisor_lost" {
				t.Fatalf("recovered receipt = %+v, want final with workload supervisor_lost", record.Receipt)
			}
			for _, resource := range record.Resources {
				if resource.State != "gone" {
					t.Errorf("recovered run still records %s as %s", resource.Role, resource.State)
				}
			}
			if record.Artifact.State != "gone" {
				t.Errorf("recovered run still records its launch artifact as %s", record.Artifact.State)
			}
		})
	}
}

// killFilteredSupervisor runs TestKilledFilteredSupervisor as a real process, waits for its workload
// to start, and SIGKILLs it — at once, or after a SIGINT once teardown has removed the agent, when
// the gateway is still standing. It returns the run the killed process owned.
func killFilteredSupervisor(t *testing.T, store *networkstate.Store, candidate networkstate.CandidateSpec, midTeardown bool) string {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	policy, err := store.Admit(repo, networkstate.Admission{InvocationMode: &mode, Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator"},
		Rules: []egress.Rule{{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(t.TempDir(), "run-id")
	launch, err := json.Marshal(killedSupervisorLaunch{Candidate: candidate, Repo: repo, ConfigDir: t.TempDir(),
		Fingerprint: policy.Fingerprint, RunIDFile: idFile})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledFilteredSupervisor$")
	cmd.Env = append(os.Environ(), killedSupervisorEnv+"="+string(launch))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			<-exited
		}
	}()
	await := func(what string, ready func() bool) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Minute); !ready(); time.Sleep(10 * time.Millisecond) {
			select {
			case err := <-exited:
				reaped = true
				t.Fatalf("the supervisor exited (%v) before %s", err, what)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting until %s", what)
			}
		}
	}
	var runID string
	await("its run was registered", func() bool {
		data, err := os.ReadFile(idFile)
		runID = strings.TrimSpace(string(data))
		return err == nil && len(runID) == 32 // a whole id: the helper's write is not atomic
	})
	// Whatever the test proves, the run must not outlive it: its record is in the trial store, which
	// no `coop net recover` on this host reads. Settling an already-settled run changes nothing.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := RecoverNetworkRuns(ctx, runtime.Runtime{Name: "docker"}, runID); err != nil {
			t.Logf("cleanup could not settle run %s: %v", runID, err)
		}
	})
	record := func() networkstate.Execution {
		value, err := store.Execution(runID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	await("its workload started", func() bool { return record().WorkloadStarted })
	if midTeardown {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		// A race, not a hook: after the agent the teardown still probes and stops the guard, so the
		// kill lands inside it. Should the whole teardown ever beat this 10 ms poll, the supervisor
		// exits on its own and the test fails saying so — a flake, never a false pass.
		await("teardown removed the agent", func() bool {
			resources := record().Resources
			i := slices.IndexFunc(resources, func(r networkstate.Resource) bool { return r.Role == "agent" })
			return i >= 0 && resources[i].State == "gone"
		})
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = <-exited
	reaped = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the supervisor ended with %v before the kill landed", err)
	}
	return runID
}

// runLeftovers lists what a run still holds in both label families: containers the box sweep would
// see (coop=box), and the gateway's containers and volumes (coop.network.run), which it never does.
func runLeftovers(t *testing.T, runID string) (boxes, containers, volumes []string) {
	t.Helper()
	run := "label=coop.network.run=" + runID
	return dockerFields(t, "ps", "-aq", "--filter", "label=coop=box", "--filter", run),
		dockerFields(t, "ps", "-a", "--filter", run, "--format", `{{.Label "coop.network.role"}}`),
		dockerFields(t, "volume", "ls", "-q", "--filter", run)
}

func dockerFields(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return strings.Fields(string(out))
}

// TestKilledFilteredSupervisor is the process TestKilledFilteredRunIsSettledByTheNextLaunch kills: one
// filtered run of a long sleep that records its run id and tears down on SIGINT.
func TestKilledFilteredSupervisor(t *testing.T) {
	raw := os.Getenv(killedSupervisorEnv)
	if raw == "" {
		t.Skip("run only by TestKilledFilteredRunIsSettledByTheNextLaunch")
	}
	var launch killedSupervisorLaunch
	if err := json.Unmarshal([]byte(raw), &launch); err != nil {
		t.Fatal(err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	smoke, err := store.BeginQualification(launch.Candidate, qualifiedClients(t, launch.Candidate))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer stop()
	cfg := &config.Config{ConfigDir: launch.ConfigDir, HomeInBox: "/home/node", Egress: "filtered", Memory: "256m", Pids: "128", CPUs: "1", NoNewPrivileges: true}
	permit := &networkSmokeLaunch{authority: smoke, registered: func(r networkstate.Execution) {
		if err := os.WriteFile(launch.RunIDFile, []byte(r.ID), 0o600); err != nil {
			t.Error(err)
		}
	}}
	_, err = runWithNetworkSmoke(cfg, runtime.Runtime{Name: "docker"}, RunSpec{
		Repo: launch.Repo, Workdir: "/workspace", Batch: true, Quiet: true, Ctx: ctx, Stdout: io.Discard, Stderr: io.Discard,
		CapturedEgress: &CapturedEgress{Store: store, Project: launch.Repo, Fingerprint: launch.Fingerprint}, Cmd: []string{"sleep", "300"},
	}, defaultCompositionArtifactOps(), permit)
	t.Fatalf("the run ended (%v) before its supervisor was killed", err)
}
