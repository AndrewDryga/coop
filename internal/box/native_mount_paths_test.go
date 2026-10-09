package box

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func legacyMountRuntime(t *testing.T, source, status string) runtime.Runtime {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "legacy-fixture", OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}))
	id := strings.Repeat("a", 64)
	record := map[string]any{"ID": id, "Status": status, "Mounts": []map[string]any{{"Type": "bind", "Source": source, "RW": false}}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\ncase \"$*\" in\n*\"ps -q\"*) printf '%s\\n' '" + id + "' ;;\n" +
		"*\"inspect --type container --format\"*) printf '%s\\n' " + strconv.Quote(string(raw)) + " ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}

func TestLegacyMountPathsResolveOnlySafeStoppedMissingTails(t *testing.T) {
	home := t.TempDir()
	unrelated := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(filepath.Join(home, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	nonDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(nonDirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, source, status string
		allowed              bool
	}{
		{"unrelated existing live bind", unrelated, "running", true},
		{"unrelated removed leaf", filepath.Join(unrelated, "removed"), "exited", true},
		{"unrelated removed subtree", filepath.Join(unrelated, "removed", "nested"), "created", true},
		{"unresolved running bind", filepath.Join(unrelated, "removed"), "running", false},
		{"unresolved paused bind", filepath.Join(unrelated, "removed"), "paused", false},
		{"unresolved restarting bind", filepath.Join(unrelated, "removed"), "restarting", false},
		{"unresolved dead bind", filepath.Join(unrelated, "removed"), "dead", false},
		{"exact home stopped readonly", home, "exited", false},
		{"home ancestor", filepath.Dir(home), "created", false},
		{"removed home descendant", filepath.Join(home, "removed"), "exited", false},
		{"home alias", alias, "exited", false},
		{"removed aliased descendant", filepath.Join(alias, "removed"), "exited", false},
		{"dangling alias", dangling, "exited", false},
		{"nested dangling alias", filepath.Join(dangling, "nested"), "exited", false},
		{"symlink loop", loop, "exited", false},
		{"relative source", "relative/missing", "exited", false},
		{"non directory ancestor", filepath.Join(nonDirectory, "child"), "exited", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := legacyMountRuntime(t, test.source, test.status)
			err := checkLegacyMountPaths(t.Context(), rt, false, home)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, want%v: %v", err == nil, test.allowed, err)
			}
		})
	}
}

func TestLegacyMountPathsRefuseInaccessibleAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can traverse mode000 directories")
	}
	denied := filepath.Join(t.TempDir(), "denied")
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o700) })
	rt := legacyMountRuntime(t, filepath.Join(denied, "missing"), "exited")
	if err := checkLegacyMountPaths(t.Context(), rt, false, t.TempDir()); err == nil {
		t.Fatal("unavailable ancestor treated as harmless absence")
	}
}

func TestAuthorityPathMissingTailDoesNotHideDanglingAlias(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "removed"), alias); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{alias, filepath.Join(alias, "nested")} {
		if _, err := resolveAuthorityPath(source); err == nil {
			t.Fatal("dangling alias reconstructed as an unrelated missing tail")
		}
	}
}
