package box

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// TestAutoUpServices: box.Run auto-starts sibling services only when enabled (COOP_AUTO_UP),
// the box is on the services network, it's online, and the runtime has compose — Apple
// `container` does not.
func TestAutoUpServices(t *testing.T) {
	cases := []struct {
		name    string
		autoUp  bool
		network bool
		egress  string
		rtName  string
		want    bool
	}{
		{"defaults: on, networked, online, docker", true, true, "open", "docker", true},
		{"podman too", true, true, "open", "podman", true},
		{"COOP_AUTO_UP=0 opts out", false, true, "open", "docker", false},
		{"no services network (COOP_NETWORK=0)", true, false, "open", "docker", false},
		{"offline box (COOP_EGRESS=none)", true, true, "none", "docker", false},
		{"Apple container has no compose", true, true, "open", "container", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{AutoUp: c.autoUp, Egress: c.egress}
			spec := RunSpec{Network: c.network}
			if got := autoUpServices(cfg, spec, c.rtName); got != c.want {
				t.Errorf("autoUpServices = %v, want %v", got, c.want)
			}
		})
	}
}

// recorderRuntime returns a runtime whose binary is a shim that appends its args to a recorder
// file and exits 0 — so a test can assert whether `compose up` was actually invoked.
func recorderRuntime(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "rt")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *\"config --services\"*) printf '%s\\n' db ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

// EnsureServices validates the compose file before running it: a valid file reaches `compose up`,
// an unsafe one is refused with a naming error and NO compose command is ever run.
func TestEnsureServicesValidates(t *testing.T) {
	t.Run("valid file runs compose up", func(t *testing.T) {
		repo := t.TempDir()
		os.MkdirAll(filepath.Join(repo, ".agent"), 0o755)
		os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
			[]byte("services:\n  db:\n    image: postgres:18\n"), 0o644)
		rec := filepath.Join(t.TempDir(), "rec")
		if _, err := EnsureServices(recorderRuntime(t, rec), repo, repo, io.Discard, io.Discard); err != nil {
			t.Fatalf("valid file should run: %v", err)
		}
		out, _ := os.ReadFile(rec)
		if !strings.Contains(string(out), "compose") || !strings.Contains(string(out), "up -d --wait --remove-orphans") {
			t.Errorf("expected `compose ... up` to run, recorder has: %q", out)
		}
	})

	t.Run("unsafe file is refused, compose never runs", func(t *testing.T) {
		repo := t.TempDir()
		os.MkdirAll(filepath.Join(repo, ".agent"), 0o755)
		os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
			[]byte("services:\n  x:\n    image: a\n    privileged: true\n"), 0o644)
		rec := filepath.Join(t.TempDir(), "rec")
		_, err := EnsureServices(recorderRuntime(t, rec), repo, repo, io.Discard, io.Discard)
		if err == nil {
			t.Fatal("an unsafe compose file must be refused")
		}
		if !strings.Contains(err.Error(), "refusing to run compose.yml") {
			t.Errorf("error should name the refused file, got: %v", err)
		}
		if _, statErr := os.Stat(rec); statErr == nil {
			out, _ := os.ReadFile(rec)
			t.Errorf("compose must NOT run for a refused file, but recorder has: %q", out)
		}
	})

	t.Run("no compose file is a no-op", func(t *testing.T) {
		rec := filepath.Join(t.TempDir(), "rec")
		dir := t.TempDir()
		services, err := EnsureServices(recorderRuntime(t, rec), dir, dir, io.Discard, io.Discard)
		if err != nil {
			t.Fatalf("no compose file should be a nil no-op: %v", err)
		}
		if len(services) != 0 {
			t.Fatalf("no compose file services = %v, want none", services)
		}
		if _, statErr := os.Stat(rec); statErr == nil {
			t.Error("compose must not run when there's no file")
		}
	})
}

func composeLabelRuntime(t *testing.T, recorder string, ids []string, labels map[string]string) runtime.Runtime {
	return composeLabelRuntimeWithServices(t, recorder, ids, labels, []string{"db"}, 0)
}

func composeLabelRuntimeWithServices(t *testing.T, recorder string, ids []string, labels map[string]string, services []string, serviceExit int) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "rt")
	var script strings.Builder
	script.WriteString("#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n")
	script.WriteString("if [ \"$1\" = ps ]; then\n  :\n")
	for _, id := range ids {
		script.WriteString("  printf '%s\\n' " + strconv.Quote(id) + "\n")
	}
	script.WriteString("fi\nif [ \"$1\" = inspect ]; then\n  case \"$4\" in\n")
	for id, value := range labels {
		script.WriteString("    " + id + ") printf '%s\\n' " + strconv.Quote(value) + " ;;\n")
	}
	script.WriteString("  esac\nfi\n")
	script.WriteString("case \"$*\" in\n  *\"config --services\"*)\n")
	for _, service := range services {
		script.WriteString("    printf '%s\\n' " + strconv.Quote(service) + "\n")
	}
	if serviceExit != 0 {
		script.WriteString("    exit " + strconv.Itoa(serviceExit) + "\n")
	}
	script.WriteString("    ;;\nesac\n")
	if err := os.WriteFile(shim, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

func composeOverrideRuntime(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "rt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *\"config --format json\"*) printf '%s\\n' " + strconv.Quote(`{"services":{"keycloak":{"expose":["8443"]}}}`) + " ;;\n" +
		"  *\"config --services\"*) printf '%s\\n' keycloak ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

func testComposeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
		[]byte("services:\n  db:\n    image: postgres:18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func testComposeRepoWithServices(t *testing.T, services ...string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	var compose strings.Builder
	compose.WriteString("services:\n")
	for _, service := range services {
		compose.WriteString("  " + service + ":\n    image: example/" + service + "\n")
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(compose.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestEnsureServicesReturnsResolvedServiceNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		services []string
	}{
		{name: "db and keycloak", services: []string{"db", "keycloak"}},
		{name: "different names", services: []string{"api", "worker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testComposeRepoWithServices(t, tc.services...)
			rec := filepath.Join(t.TempDir(), "rec")
			rt := composeLabelRuntimeWithServices(t, rec, nil, nil, tc.services, 0)

			got, err := EnsureServices(rt, repo, repo, io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.services) {
				t.Fatalf("services = %v, want Compose order %v", got, tc.services)
			}
			data, err := os.ReadFile(rec)
			if err != nil {
				t.Fatal(err)
			}
			calls := string(data)
			config := "compose -p " + ComposeProject(repo) + " -f " + filepath.Join(repo, ".agent", "compose.yml") + " config --services"
			up := "compose -p " + ComposeProject(repo) + " -f " + filepath.Join(repo, ".agent", "compose.yml") + " up -d --wait --remove-orphans"
			if i, j := strings.Index(calls, config), strings.Index(calls, up); i < 0 || j < 0 || i >= j {
				t.Fatalf("service discovery must use the current project/file before up:\n%s", calls)
			}
		})
	}
}

func TestEnsureServicesUsesOneGeneratedOverrideForDiscoveryAndStartup(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte("services:\n  keycloak:\n    image: example/keycloak\n    expose: [8443]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(t.TempDir(), "rec")

	services, err := EnsureServices(composeOverrideRuntime(t, rec), repo, repo, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(services, []string{"keycloak"}) {
		t.Fatalf("services = %v, want [keycloak]", services)
	}

	data, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	var discoveryOverride, startupOverride string
	for _, call := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(call)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] != "-f" || !strings.Contains(filepath.Base(fields[i+1]), "coop-compose-override-") {
				continue
			}
			switch {
			case strings.HasSuffix(call, "config --services"):
				discoveryOverride = fields[i+1]
			case strings.HasSuffix(call, "up -d --wait --remove-orphans"):
				startupOverride = fields[i+1]
			}
		}
	}
	if discoveryOverride == "" || startupOverride == "" {
		t.Fatalf("generated override missing from discovery/startup calls:\n%s", data)
	}
	if discoveryOverride != startupOverride {
		t.Fatalf("discovery override %q differs from startup override %q", discoveryOverride, startupOverride)
	}
}

func TestEnsureServicesStopsWhenServiceDiscoveryFails(t *testing.T) {
	repo := testComposeRepoWithServices(t, "db", "keycloak")
	rec := filepath.Join(t.TempDir(), "rec")
	rt := composeLabelRuntimeWithServices(t, rec, nil, nil, nil, 23)

	services, err := EnsureServices(rt, repo, repo, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "compose config --services exited with code 23") {
		t.Fatalf("discovery error = %v, want named compose failure", err)
	}
	if len(services) != 0 {
		t.Fatalf("failed discovery returned services %v", services)
	}
	data, readErr := os.ReadFile(rec)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(data), " up -d ") {
		t.Fatalf("compose up must not run after discovery fails:\n%s", data)
	}
}

func legacyLabels(repo, project string) string {
	return `{"com.docker.compose.project":` + strconv.Quote(project) +
		`,"com.docker.compose.project.working_dir":` + strconv.Quote(filepath.Join(repo, ".agent")) + `}`
}

func TestEnsureServicesReconcilesOwnedLegacyBeforeCurrentUp(t *testing.T) {
	repo := testComposeRepo(t)
	rec := filepath.Join(t.TempDir(), "rec")
	legacy := ServicesProject(repo)
	rt := composeLabelRuntime(t, rec, []string{"old-db"}, map[string]string{
		"old-db": legacyLabels(repo, legacy),
	})
	if _, err := EnsureServices(rt, repo, repo, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	legacyDown := "compose -p " + legacy + " -f " + filepath.Join(repo, ".agent", "compose.yml") + " down --remove-orphans"
	currentUp := "compose -p " + ComposeProject(repo) + " -f " + filepath.Join(repo, ".agent", "compose.yml") + " up -d --wait --remove-orphans"
	if i, j := strings.Index(got, legacyDown), strings.Index(got, currentUp); i < 0 || j < 0 || i >= j {
		t.Fatalf("legacy cleanup must precede current up:\n%s", got)
	}
}

func TestLegacyComposeOwnershipFailsClosed(t *testing.T) {
	repo := testComposeRepo(t)
	legacy := ServicesProject(repo)
	tests := []struct {
		name   string
		ids    []string
		labels map[string]string
		reason string
	}{
		{name: "no containers"},
		{
			name: "missing working dir", ids: []string{"old"},
			labels: map[string]string{"old": `{"com.docker.compose.project":` + strconv.Quote(legacy) + `}`},
			reason: "no Compose working-directory label",
		},
		{
			name: "outside workspace", ids: []string{"old"},
			labels: map[string]string{"old": legacyLabels(t.TempDir(), legacy)},
			reason: "outside this workspace",
		},
		{
			name: "mixed ownership", ids: []string{"ours", "theirs"},
			labels: map[string]string{
				"ours":   legacyLabels(repo, legacy),
				"theirs": legacyLabels(t.TempDir(), legacy),
			},
			reason: "outside this workspace",
		},
		{
			name: "wrong repeated project label", ids: []string{"old"},
			labels: map[string]string{"old": legacyLabels(repo, "other")},
			reason: "different project label",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "rec")
			owned, reason, err := legacyComposeOwnership(composeLabelRuntime(t, rec, tc.ids, tc.labels), repo)
			if err != nil {
				t.Fatal(err)
			}
			if owned {
				t.Fatal("ambiguous or empty legacy project must not be owned")
			}
			if tc.reason == "" {
				if reason != "" {
					t.Fatalf("empty project reason = %q, want empty", reason)
				}
			} else if !strings.Contains(reason, tc.reason) {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
			data, _ := os.ReadFile(rec)
			if strings.Contains(string(data), "compose ") {
				t.Fatalf("ownership check ran Compose cleanup:\n%s", data)
			}
		})
	}
}

func TestDownServicesKeepsLegacyVolumes(t *testing.T) {
	repo := testComposeRepo(t)
	rec := filepath.Join(t.TempDir(), "rec")
	legacy := ServicesProject(repo)
	rt := composeLabelRuntime(t, rec, []string{"old-db"}, map[string]string{
		"old-db": legacyLabels(repo, legacy),
	})
	if err := DownServices(rt, repo, repo, true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	var composeCalls []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "compose ") {
			composeCalls = append(composeCalls, line)
		}
	}
	if len(composeCalls) != 2 {
		t.Fatalf("compose calls = %v, want current and legacy down", composeCalls)
	}
	if !strings.Contains(composeCalls[0], ComposeProject(repo)) ||
		!strings.Contains(composeCalls[0], "down --remove-orphans --volumes") {
		t.Errorf("current down did not remove current volumes: %q", composeCalls[0])
	}
	if !strings.Contains(composeCalls[1], legacy) ||
		!strings.HasSuffix(composeCalls[1], "down --remove-orphans") ||
		strings.Contains(composeCalls[1], "--volumes") {
		t.Errorf("legacy down must preserve volumes: %q", composeCalls[1])
	}
}
