package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Controller jobs could edit projects but had no database for their tests. The host-parent
// grant must start exactly the frozen stack, even when the working project file is broken.
func TestControllerWorkStartsItsGrantedServicesBeforeTheCommand(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent/project.yaml"), []byte("box: [broken]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "test-services.yml"), []byte("services:\n  db:\n    image: postgres:18\n"), 0644); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	grant, _ := json.Marshal(map[string]any{"version": 1, "job_digest": digest, "repository": repo, "runtime": map[string]any{"version": 1, "dockerfile": "", "compose": "test-services.yml", "environment": map[string]string{}}})
	t.Setenv("COOP_CONTROLLER_JOB", digest)
	t.Setenv("COOP_CONTROLLER_RUNTIME", string(grant))
	recorder := filepath.Join(t.TempDir(), "calls")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", CPUs: "1", Memory: "1073741824", Pids: "256"}
	if code, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{Image: "test-box", Repo: repo, Workdir: "/workspace", Cmd: []string{"true"}, ControllerJob: true, Batch: true, Quiet: true}); err != nil || code != 0 {
		t.Fatalf("run = %d, %v", code, err)
	}
	calls, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	up, command := strings.Index(string(calls), "up -d --wait"), strings.Index(string(calls), "run ")
	if up < 0 || command < up {
		t.Fatalf("required services must start before the job command:\n%s", calls)
	}
	if !strings.Contains(string(calls), "--network "+ComposeProject(repo)+"_default") {
		t.Fatalf("job must join its private services network:\n%s", calls)
	}
	if strings.Contains(string(calls), "COOP_CONTROLLER_RUNTIME=") {
		t.Fatal("host runtime grant reached the agent box")
	}
}

func TestControllerWorkDoesNotRunWithoutItsRequiredServices(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "test-services.yml"), []byte("services:\n  db:\n    image: postgres:18\n"), 0644); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	grant, _ := json.Marshal(map[string]any{"version": 1, "job_digest": digest, "repository": repo, "runtime": map[string]any{"version": 1, "dockerfile": "", "compose": "test-services.yml", "environment": map[string]string{}}})
	t.Setenv("COOP_CONTROLLER_JOB", digest)
	t.Setenv("COOP_CONTROLLER_RUNTIME", string(grant))
	recorder := filepath.Join(t.TempDir(), "calls")
	script := filepath.Join(t.TempDir(), "rt")
	body := "#!/bin/sh\necho \"$*\" >> '" + recorder + "'\ncase \"$*\" in *'config --services'*) echo db ;; *'up -d --wait'*) echo 'database unhealthy' >&2; exit 1 ;; esac\n"
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", CPUs: "1", Memory: "1073741824", Pids: "256"}
	code, err := Run(cfg, runtime.Runtime{Name: script}, RunSpec{Image: "test-box", Repo: repo, Cmd: []string{"true"}, ControllerJob: true, Batch: true, Quiet: true})
	if err == nil || code == 0 {
		t.Fatalf("missing required database must refuse job, got %d, %v", code, err)
	}
	// An unhealthy service may be healthy on the next attempt, so this is not a refusal to stop at.
	if !errors.As(err, new(*ServicesNotStarted)) || errors.As(err, new(*LaunchRefused)) {
		t.Fatalf("Run error = %v, want services that did not start, not a launch refusal", err)
	}
	calls, _ := os.ReadFile(recorder)
	if strings.Contains(string(calls), "run ") {
		t.Fatalf("job ran without its required database:\n%s", calls)
	}
}

func TestControllerReviewRequiresOnlyTheSelectedStack(t *testing.T) {
	for _, test := range []struct {
		name, review              string
		present, allowed, offline bool
	}{
		{"review replaces removed Work stack", "review.yml", true, true, false},
		{"missing review stack is refused", "review.yml", false, false, false},
		{"missing Work stack is refused", "", false, false, false},
		{"offline review cannot omit its required stack", "review.yml", true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0755); err != nil {
				t.Fatal(err)
			}
			if test.review != "" {
				if err := os.WriteFile(filepath.Join(repo, ".agent/project.yaml"), []byte("review:\n  compose: "+test.review+"\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if test.present {
				if err := os.WriteFile(filepath.Join(repo, test.review), []byte("services:\n  db:\n    image: postgres:18\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			digest := strings.Repeat("c", 64)
			data, _ := json.Marshal(map[string]any{"version": 1, "job_digest": digest, "repository": repo, "runtime": map[string]any{"version": 1, "dockerfile": "", "compose": "removed-work.yml", "environment": map[string]string{}}})
			t.Setenv("COOP_CONTROLLER_JOB", digest)
			t.Setenv("COOP_CONTROLLER_RUNTIME", string(data))
			cfg := &config.Config{BaseImage: "test-box", ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", CPUs: "1", Memory: "1073741824", Pids: "256"}
			if test.offline {
				cfg.Egress = "none"
			}
			recorder := filepath.Join(t.TempDir(), "calls")
			code, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{Repo: repo, Image: "test-box", ControllerJob: true, Review: true, Batch: true, Quiet: true, Cmd: []string{"true"}})
			if test.allowed && (err != nil || code != 0) {
				t.Fatalf("selected review stack refused: %d %v", code, err)
			}
			if !test.allowed && err == nil {
				t.Fatal("job ran without its required stack")
			}
		})
	}
}

func TestControllerWorkRefusesSharedServiceVolumesBeforeTheCommand(t *testing.T) {
	for _, declaration := range []string{"external: true", "name: shared-database"} {
		t.Run(declaration, func(t *testing.T) {
			repo := t.TempDir()
			body := "services:\n  db:\n    image: postgres:18\n    volumes: [data:/data]\nvolumes:\n  data:\n    " + declaration + "\n"
			if err := os.WriteFile(filepath.Join(repo, "services.yml"), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
			digest := strings.Repeat("d", 64)
			data, _ := json.Marshal(map[string]any{"version": 1, "job_digest": digest, "repository": repo, "runtime": map[string]any{"version": 1, "dockerfile": "", "compose": "services.yml", "environment": map[string]string{}}})
			t.Setenv("COOP_CONTROLLER_JOB", digest)
			t.Setenv("COOP_CONTROLLER_RUNTIME", string(data))
			recorder := filepath.Join(t.TempDir(), "calls")
			cfg := &config.Config{BaseImage: "test-box", ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", CPUs: "1", Memory: "1073741824", Pids: "256"}
			_, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{Repo: repo, Image: "test-box", ControllerJob: true, Batch: true, Quiet: true, Cmd: []string{"true"}})
			if err == nil || !strings.Contains(err.Error(), "project-local") {
				t.Fatalf("shared volume must be refused: %v", err)
			}
			calls, _ := os.ReadFile(recorder)
			if strings.Contains(string(calls), "up -d") || strings.Contains(string(calls), "run ") {
				t.Fatalf("shared volume reached execution: %s", calls)
			}
		})
	}
}

func TestFilteredControllerToolchainIsRefusedEvenIfItsImageIsCached(t *testing.T) {
	repo := t.TempDir()
	cfg := &config.Config{BaseImage: "base", Egress: "filtered"}
	grant := &ControllerRuntime{Version: 1, JobDigest: strings.Repeat("e", 64), Repository: repo, Runtime: &workerproto.JobRuntime{Version: 1, Dockerfile: "Dockerfile", Compose: "", Environment: map[string]string{}}}
	_, err := prepareControllerImage(cfg, recorderRuntime(t, filepath.Join(t.TempDir(), "calls")), grant, nil)
	if err == nil || !strings.Contains(err.Error(), "filtered controller jobs") {
		t.Fatalf("filtered runtime must not silently replace a granted project image: %v", err)
	}
}

// A canceled job must not create another dependency stack while its worker releases the lease.
func TestCanceledControllerSetupNeverStartsServicesOrTheCommand(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "services.yml"), []byte("services:\n  db:\n    image: postgres:18\n"), 0644); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("f", 64)
	grant, _ := json.Marshal(map[string]any{"version": 1, "job_digest": digest, "repository": repo, "runtime": map[string]any{"version": 1, "dockerfile": "", "compose": "services.yml", "environment": map[string]string{}}})
	t.Setenv("COOP_CONTROLLER_JOB", digest)
	t.Setenv("COOP_CONTROLLER_RUNTIME", string(grant))
	directory := t.TempDir()
	discovered := filepath.Join(directory, "discovered")
	release := filepath.Join(directory, "release")
	calls := filepath.Join(directory, "calls")
	shim := filepath.Join(directory, "rt")
	script := "#!/bin/sh\necho \"$*\" >> " + strconv.Quote(calls) + "\ncase \"$*\" in *'config --services'*) touch " + strconv.Quote(discovered) + "; while [ ! -f " + strconv.Quote(release) + " ]; do sleep 0.01; done; echo db ;; esac\n"
	if err := os.WriteFile(shim, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &config.Config{BaseImage: "test-box", ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", CPUs: "1", Memory: "1073741824", Pids: "256"}
	var runErr error
	done := make(chan struct{})
	go func() {
		_, runErr = Run(cfg, runtime.Runtime{Name: shim}, RunSpec{Repo: repo, Image: "test-box", ControllerJob: true, Batch: true, Quiet: true, Ctx: ctx, Cmd: []string{"true"}})
		close(done)
	}()
	wait.ForFile(t, discovered)
	cancel()
	if err := os.WriteFile(release, []byte("release\n"), 0600); err != nil {
		t.Fatal(err)
	}
	<-done
	if runErr == nil {
		t.Fatal("canceled setup was reported as successful")
	}
	recorded, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(recorded), "up -d") || strings.Contains(string(recorded), "run ") {
		t.Fatalf("canceled controller setup executed a service or command:\n%s", recorded)
	}
}

// A review's separate database must override project defaults, while explicit controller
// values keep their authority. Putting defaults in the controller environment broke this order.
func TestReviewOverridesProjectDefaultsAndKeepsControllerValues(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0755); err != nil {
		t.Fatal(err)
	}
	project := "box:\n  env:\n    PROJECT_ONLY: tampered\nreview:\n  env:\n    PGHOST: review-db\n    CI: candidate\n"
	if err := os.WriteFile(filepath.Join(repo, ".agent/project.yaml"), []byte(project), 0644); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	grant, _ := json.Marshal(map[string]any{"version": 1, "job_digest": digest, "repository": repo, "runtime": map[string]any{"version": 1, "dockerfile": "", "compose": "", "environment": map[string]string{"PGHOST": "work-db", "CI": "project", "PROJECT_ONLY": "frozen"}}})
	t.Setenv("COOP_CONTROLLER_JOB", digest)
	t.Setenv("COOP_CONTROLLER_RUNTIME", string(grant))
	calls := filepath.Join(t.TempDir(), "calls")
	envLog := filepath.Join(t.TempDir(), "environment")
	shim := filepath.Join(t.TempDir(), "rt")
	script := "#!/bin/sh\necho \"$*\" >> " + strconv.Quote(calls) + "\nlast=\nfor arg do\n if [ \"$last\" = --env-file ]; then cat \"$arg\" >> " + strconv.Quote(envLog) + "; fi\n last=$arg\ndone\n"
	if err := os.WriteFile(shim, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{BaseImage: "test-box", ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open"}
	code, err := Run(cfg, runtime.Runtime{Name: shim}, RunSpec{Repo: repo, Image: "test-box", ControllerJob: true, Review: true, Batch: true, Quiet: true, Cmd: []string{"true"}, ExtraArgs: []string{"-e", "CI=controller"}})
	if err != nil || code != 0 {
		t.Fatalf("review setup failed: %d, %v", code, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	defaults, err := os.ReadFile(envLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(defaults), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			actual[key] = value
		}
	}

	for _, line := range strings.Split(string(data), "\n") {
		args := strings.Fields(line)
		if len(args) == 0 || args[0] != "run" {
			continue
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-e" {
				key, value, ok := strings.Cut(args[i+1], "=")
				if ok {
					actual[key] = value
				}
			}
		}
	}
	for key, value := range map[string]string{"PGHOST": "review-db", "CI": "controller", "PROJECT_ONLY": "frozen"} {
		if actual[key] != value {
			t.Errorf("effective %s=%q, want %q; calls:\n%s", key, actual[key], value, data)
		}
	}
}

func TestAcceptedProjectDefaultsFitTheHostEnvelope(t *testing.T) {
	values := map[string]string{}
	for _, key := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		values[key] = "x" + strings.Repeat("<>&\u2028", 340) + "x"
	}
	digest := strings.Repeat("a", 64)
	setup := &workerproto.JobRuntime{Version: 1, Dockerfile: strings.Repeat("<&>/", 255) + "x", Compose: strings.Repeat("<&>/", 255) + "y", Environment: values}
	encoded, err := EncodeControllerRuntime(digest, "/"+strings.Repeat("&/", 2047)+"r", setup)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+len(ControllerJobEnv)+1 >= 128<<10 {
		t.Fatalf("accepted runtime exceeds a host envelope: %d bytes", len(encoded))
	}
	t.Setenv(ControllerJobEnv, digest)
	t.Setenv(ControllerRuntimeEnv, encoded)
	grant, err := ControllerRuntimeFromEnvironment()
	if err != nil || grant == nil || len(grant.Runtime.Environment) != len(values) {
		t.Fatalf("valid defaults could not cross the host boundary: %v", err)
	}
}

// A custom-only stack used to start successfully, then fail because the box joins default.
// Refuse that topology before starting services instead of leaving a half-started test setup.
func TestControllerServicesRequireAReachableDefaultNetwork(t *testing.T) {
	for _, networks := range []string{"    networks: [backend]\n", "    networks:\n      backend: {}\n"} {
		data := []byte("services:\n  db:\n    image: postgres:18\n" + networks + "networks:\n  backend: {}\n")
		if err := validateControllerServices(data); err == nil || !strings.Contains(err.Error(), "default network") {
			t.Fatalf("custom-only stack must be refused before startup: %v", err)
		}
	}
	for _, networks := range []string{"", "    networks: []\n", "    networks: {}\n", "    networks: [default, backend]\n", "    networks:\n      default:\n        aliases: [project-db]\n      backend: {}\n"} {
		data := []byte("services:\n  db:\n    image: postgres:18\n" + networks + "networks:\n  default: {}\n  backend: {}\n")
		if err := validateControllerServices(data); err != nil {
			t.Fatalf("default-connected stack refused: %v", err)
		}
	}
}
