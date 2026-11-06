package box

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestServiceBindSourcesMissingRefuseBeforeComposeStart(t *testing.T) {
	cases := []struct {
		name   string
		source string
		volume string
	}{
		{name: "short read-only", source: "./missing-readonly", volume: "./missing-readonly:/data:ro"},
		{name: "short writable", source: "./missing-writable", volume: "./missing-writable:/data:rw"},
		{name: "long type bind bare source", source: "missing-long", volume: "type: bind\nsource: missing-long\ntarget: /data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			composeDir := filepath.Join(repo, "compose")
			if err := os.MkdirAll(composeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			compose := filepath.Join(composeDir, "compose.yml")
			volume := tc.volume
			if tc.name == "long type bind bare source" {
				volume = "- " + strings.ReplaceAll(volume, "\n", "\n        ")
			} else {
				volume = "- " + volume
			}
			body := "services:\n  app:\n    image: alpine\n    volumes:\n      " + volume + "\n"
			if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}

			recorder := filepath.Join(t.TempDir(), "runtime.log")
			_, err := startAutomaticTestServices(recorderRuntime(t, recorder), repo, compose, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.source) {
				t.Fatalf("start with missing bind source %q = %v, want a source-naming refusal", tc.source, err)
			}
			if data, readErr := os.ReadFile(recorder); readErr == nil {
				if strings.Contains(string(data), " create ") || strings.Contains(string(data), " up ") {
					t.Errorf("Compose created or started services before refusing the missing source:\n%s", data)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			missing := filepath.Join(composeDir, tc.source)
			if _, statErr := os.Lstat(missing); !os.IsNotExist(statErr) {
				t.Errorf("missing bind source %q appeared on disk: %v", missing, statErr)
			}
		})
	}
}

func TestServiceBindSourcesExistingFileAndDirectoryStart(t *testing.T) {
	cases := []struct {
		name   string
		source string
		volume string
		setup  func(string) error
	}{
		{
			name:   "regular file",
			source: "./existing.pem",
			volume: "./existing.pem:/secret:ro",
			setup: func(path string) error {
				return os.WriteFile(path, []byte("certificate\n"), 0o600)
			},
		},
		{
			name:   "directory",
			source: "./existing-data",
			volume: "./existing-data:/data:rw",
			setup: func(path string) error {
				return os.Mkdir(path, 0o755)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			composeDir := filepath.Join(repo, "compose")
			if err := os.MkdirAll(composeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := tc.setup(filepath.Join(composeDir, tc.source)); err != nil {
				t.Fatal(err)
			}
			compose := filepath.Join(composeDir, "compose.yml")
			body := "services:\n  app:\n    image: alpine\n    volumes:\n      - " + tc.volume + "\n"
			if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			if _, err := startAutomaticTestServices(recorderRuntime(t, recorder), repo, compose, io.Discard, io.Discard); err != nil {
				t.Fatalf("start with existing %s bind source: %v", tc.name, err)
			}
			data, err := os.ReadFile(recorder)
			if err != nil || !strings.Contains(string(data), " up ") {
				t.Fatalf("Compose did not start existing %s bind source: %q, %v", tc.name, data, err)
			}
		})
	}
}

func TestServiceBindSourcesMissingSourceDoesNotBlockDown(t *testing.T) {
	repo := t.TempDir()
	composeDir := filepath.Join(repo, "compose")
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(composeDir, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  app:\n    image: alpine\n    volumes:\n      - ./missing.pem:/secret:ro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	if err := DownServicesFile(recorderRuntime(t, recorder), repo, compose, false, io.Discard, io.Discard); err != nil {
		t.Fatalf("down with missing bind source: %v", err)
	}
	data, err := os.ReadFile(recorder)
	if err != nil || !strings.Contains(string(data), " down ") {
		t.Fatalf("Compose down did not run with missing bind source: %q, %v", data, err)
	}
}

func TestServiceBindSourcesRequiredDirectoryRefusedBeforeStart(t *testing.T) {
	repo := t.TempDir()
	composeDir := filepath.Join(repo, ".agent")
	if err := os.MkdirAll(filepath.Join(repo, "certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(composeDir, "project.yaml"), []byte("services:\n  require_real_files:\n    - certs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(composeDir, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  app:\n    image: alpine\n    volumes:\n      - ../certs:/certs:ro\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reviewLog := filepath.Join(t.TempDir(), "review-runtime.log")
	if _, err := ReviewServiceStart(repo, compose, serviceReviewRuntime(t, reviewLog), true); err == nil || !strings.Contains(err.Error(), "../certs") {
		t.Fatalf("review with a required directory bind = %v, want a path-naming refusal", err)
	}
	if calls, err := os.ReadFile(reviewLog); err != nil || len(calls) != 0 {
		t.Fatalf("review contacted the runtime before refusing the required directory: %q, %v", calls, err)
	}

	startLog := filepath.Join(t.TempDir(), "start-runtime.log")
	if _, err := startAutomaticTestServices(recorderRuntime(t, startLog), repo, compose, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "../certs") {
		t.Fatalf("automatic start with a required directory bind = %v, want a path-naming refusal", err)
	}
	// The existing writer inspection is read-only; no Compose command may create a container.
	if calls, err := os.ReadFile(startLog); err != nil || string(calls) != "ps -q --filter label=com.docker.compose.project\n" {
		t.Fatalf("automatic start did more than inspect existing service writers: %q, %v", calls, err)
	}
}

func TestServiceBindSourcesProfiles(t *testing.T) {
	repo := t.TempDir()
	file := filepath.Join(repo, "compose.yml")
	data := []byte("services:\n  app:\n    image: alpine\n  tools:\n    image: alpine\n    profiles: [tools]\n    volumes: ['./missing:/data:ro']\n")
	for _, selected := range [][]string{nil, {"tools"}} {
		args, cleanup, _, err := snapshotComposeArgsForStartPinned(t.Context(), recorderRuntime(t, filepath.Join(t.TempDir(), "runtime.log")), repo, file, "", data, selected, true)
		if cleanup != nil {
			cleanup()
		}
		if len(selected) == 0 && (err != nil || len(args) == 0) {
			t.Fatalf("inactive profile refused default start: %v", err)
		}
		if len(selected) > 0 && (err == nil || !strings.Contains(err.Error(), "./missing")) {
			t.Fatalf("explicit profile selection did not refuse its missing source: %v", err)
		}
	}
}

func TestServiceBindSourcesRequiredDirectoryThroughAlias(t *testing.T) {
	repo := t.TempDir()
	writeForkServiceTestFile(t, repo, ".agent/project.yaml", []byte("services:\n  require_real_files: [certs]\n"), 0o644)
	if err := os.Mkdir(filepath.Join(repo, "certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	data := []byte(fmt.Sprintf("services:\n  app:\n    image: alpine\n    volumes:\n      - type: bind\n        source: %q\n        target: /certs\n        read_only: true\n", filepath.Join(alias, "certs")))
	_, cleanup, _, err := snapshotComposeArgsForStartPinned(t.Context(), recorderRuntime(t, filepath.Join(t.TempDir(), "runtime.log")), repo, filepath.Join(repo, "compose.yml"), "", data, nil, true)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "certs") {
		t.Fatalf("required directory accepted through an alias: %v", err)
	}
}

func TestServiceBindSourcesSelectedStartChecksOnlyServicesAndDependencies(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	cases := []struct {
		name       string
		body       string
		wantErrSrc string
	}{
		{
			name: "unrelated missing bind is ignored",
			body: "services:\n  app:\n    image: alpine\n  unrelated:\n    image: alpine\n    volumes:\n      - ./missing-unrelated.key:/key:ro\n",
		},
		{
			name:       "missing dependency bind is refused",
			body:       "services:\n  app:\n    image: alpine\n    depends_on: [database]\n  database:\n    image: postgres\n    volumes:\n      - ./missing-database.key:/key:ro\n  unrelated:\n    image: alpine\n    volumes:\n      - ./missing-unrelated.key:/key:ro\n",
			wantErrSrc: "./missing-database.key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.body)
			if err := os.WriteFile(compose, data, 0o644); err != nil {
				t.Fatal(err)
			}
			runtimeLog := filepath.Join(t.TempDir(), "runtime.log")
			args, cleanup, _, err := snapshotComposeArgsForStartPinned(
				t.Context(), recorderRuntime(t, runtimeLog), repo, compose, "", data, []string{"app"}, true,
			)
			if tc.wantErrSrc != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSrc) {
					t.Fatalf("selected start error = %v, want refusal naming %q", err, tc.wantErrSrc)
				}
				if cleanup != nil {
					cleanup()
				}
			} else {
				if err != nil {
					t.Fatalf("selected start refused an unrelated missing bind: %v", err)
				}
				defer cleanup()
				if len(args) == 0 {
					t.Fatal("selected start returned no private Compose arguments")
				}
			}
			if calls, readErr := os.ReadFile(runtimeLog); !os.IsNotExist(readErr) && (readErr != nil || len(calls) != 0) {
				t.Fatalf("snapshot boundary invoked the runtime before bind selection: %q, %v", calls, readErr)
			}
		})
	}
}

func TestServiceBindSourcesPrivateSnapshotDisablesBindCreation(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	if err := os.WriteFile(filepath.Join(repo, "ro.data"), []byte("certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "rw.data"), []byte("writable data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const privateMarker = "test-only-key-material-must-stay-hidden"
	if err := os.WriteFile(filepath.Join(repo, "tls.key"), []byte(privateMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	data := []byte(`services:
  app:
    image: alpine
    command: ["sh", "-c", "printf '%s' '$$COOP_CONTAINER_VALUE'"]
    environment:
      LITERAL: "$$COOP_CONTAINER_VALUE"
    volumes:
      - ./ro.data:/read-only:ro,cached
      - ./rw.data:/writable:rw,delegated
      - type: bind
        source: ./tls.key
        target: "/run/key:escaped"
        read_only: true
        consistency: consistent
      - cache:/named-short
      - /anonymous
      - type: volume
        source: cache
        target: /named-long
volumes:
  cache: {}
`)
	args, cleanup, hidden, err := snapshotComposeArgsForStartData(repo, compose, "", data, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !slices.Equal(hidden, []string{"tls.key"}) {
		t.Fatalf("hidden secret paths = %v, want the key kept behind a decoy", hidden)
	}

	var snapshotPath, shadowPath string
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-f" {
			continue
		}
		if strings.HasSuffix(args[i+1], "coop-compose-override-shadow.yml") {
			shadowPath = args[i+1]
		} else {
			snapshotPath = args[i+1]
		}
	}
	if snapshotPath == "" || shadowPath == "" {
		t.Fatalf("private Compose arguments do not include the main snapshot and secret shadow: %v", args)
	}
	snapshotBytes, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	shadowBytes, err := os.ReadFile(shadowPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshotBytes), privateMarker) || strings.Contains(string(shadowBytes), privateMarker) {
		t.Fatal("private Compose files exposed test key material")
	}

	type service struct {
		Volumes     []any             `yaml:"volumes"`
		Command     []string          `yaml:"command"`
		Environment map[string]string `yaml:"environment"`
	}
	type document struct {
		Services map[string]service `yaml:"services"`
	}
	var original, snapshot document
	if err := yaml.Unmarshal(data, &original); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(snapshotBytes, &snapshot); err != nil {
		t.Fatal(err)
	}
	got := snapshot.Services["app"]
	wantEscaped := "printf '%s' '$$COOP_CONTAINER_VALUE'"
	if !slices.Equal(got.Command, []string{"sh", "-c", wantEscaped}) || got.Environment["LITERAL"] != "$$COOP_CONTAINER_VALUE" {
		t.Fatalf("Compose-escaped container values changed: command=%q environment=%q", got.Command, got.Environment)
	}
	if len(got.Volumes) != 6 {
		t.Fatalf("private snapshot volumes = %d, want 6", len(got.Volumes))
	}
	wantBinds := map[string]struct {
		source      string
		target      string
		writable    bool
		consistency string
	}{
		"/read-only":       {source: "./ro.data", target: "/read-only", consistency: "cached"},
		"/writable":        {source: "./rw.data", target: "/writable", writable: true, consistency: "delegated"},
		"/run/key:escaped": {source: "./tls.key", target: "/run/key:escaped", consistency: "consistent"},
	}
	for _, volume := range got.Volumes[:3] {
		source, target := bindSourceTarget(volume)
		want, ok := wantBinds[target]
		if !ok || source != want.source || target != want.target || !serviceVolumeIsBind(volume, source) {
			t.Fatalf("private bind source/target = %q:%q, want one of %v", source, target, wantBinds)
		}
		converted, ok := volume.(map[string]any)
		if !ok {
			t.Fatalf("private bind %q did not use long form: %T", target, volume)
		}
		bind, ok := converted["bind"].(map[string]any)
		if !ok || bind["create_host_path"] != false {
			t.Fatalf("private bind %q can create a host path: %v", target, converted["bind"])
		}
		actualTarget, writable := serviceVolumeTargetAccess(volume)
		if actualTarget != want.target || writable != want.writable || converted["consistency"] != want.consistency {
			t.Fatalf("private bind %q access/consistency = target %q writable=%v consistency=%v", target, actualTarget, writable, converted["consistency"])
		}
		delete(wantBinds, target)
	}
	if len(wantBinds) != 0 {
		t.Fatalf("private snapshot lost bind mounts: %v", wantBinds)
	}
	if !reflect.DeepEqual(original.Services["app"].Volumes[3:], got.Volumes[3:]) {
		t.Fatalf("named and anonymous volumes changed: before=%#v after=%#v", original.Services["app"].Volumes[3:], got.Volumes[3:])
	}

	var shadow struct {
		Services map[string]struct {
			Volumes []struct {
				Source   string `yaml:"source"`
				Target   string `yaml:"target"`
				ReadOnly bool   `yaml:"read_only"`
				Bind     struct {
					CreateHostPath *bool `yaml:"create_host_path"`
				} `yaml:"bind"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(shadowBytes, &shadow); err != nil {
		t.Fatal(err)
	}
	decoys := shadow.Services["app"].Volumes
	if len(decoys) != 1 || decoys[0].Target != "/run/key:escaped" || !decoys[0].ReadOnly || decoys[0].Source == filepath.Join(repo, "tls.key") {
		t.Fatalf("secret shadow override does not replace the key bind with a read-only decoy: %+v", decoys)
	}
	if create := decoys[0].Bind.CreateHostPath; create == nil || *create {
		t.Fatal("secret shadow override permits host directory creation")
	}
	decoyBytes, err := os.ReadFile(decoys[0].Source)
	if err != nil || len(decoyBytes) != 0 {
		t.Fatalf("secret shadow source is not an empty decoy: size=%d, err=%v", len(decoyBytes), err)
	}
}
