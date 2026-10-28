package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func recordLoopServiceCleanup(t *testing.T, repo string) (runtime.Runtime, func(string)) {
	t.Helper()
	writeTaskFile(t, filepath.Join(repo, ".agent", "compose.yml"), "services:\n  db:\n    image: postgres:18\n")
	log := filepath.Join(t.TempDir(), "runtime.log")
	t.Setenv("COOP_TEST_SERVICE_CLEANUP_LOG", log)
	shim := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COOP_TEST_SERVICE_CLEANUP_LOG\"\nif [ \"$1 $2\" = \"volume ls\" ]; then echo fixture_volume; fi\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}, func(owner string) {
		t.Helper()
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal("loop did not clean its private services:", err)
		}
		output := string(data)
		if owner == "" || !strings.Contains(output, "label=com.docker.compose.project="+box.ComposeProjectFor(repo, owner)) || strings.Count(output, "volume rm fixture_volume\n") != 1 {
			t.Fatalf("private owner cleanup missing or repeated for %q:\n%s", owner, output)
		}
		if strings.Contains(output, "label=com.docker.compose.project="+box.ComposeProject(repo)+" ") {
			t.Fatalf("loop cleanup selected development services:\n%s", output)
		}
	}
}
