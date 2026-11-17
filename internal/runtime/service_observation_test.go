package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A context switch while services are checked must not judge them on another daemon: both the
// listing and the inspect go to the frozen daemon, which must still answer there at the end.
func TestFrozenServiceObservationAsksOneDaemon(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	endpoint := composeFixtureDaemon(t)
	t.Setenv("DOCKER_HOST", endpoint)
	recorder := filepath.Join(t.TempDir(), "commands")
	path := filepath.Join(t.TempDir(), "docker")
	id := strings.Repeat("b", 64)
	record := `{"ID":"` + id + `","Labels":{"coop.service":"db"},"Status":"running","ExitCode":0,"Running":true,` +
		`"Paused":false,"Healthcheck":false,"Health":"","Ports":{},"Networks":{"frozen-net":{}}}`
	// Only the frozen daemon's socket knows the service; the ambient context has none.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *'--host " + endpoint + " '*'ps -q -a'*) echo " + id + " ;;\n" +
		"  *'--host " + endpoint + " '*'inspect --format'*) printf '%s\\n' '" + record + "' ;;\n" +
		"  *'ps -q -a'*) ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := (Runtime{Name: path}).FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "unix:///other.sock")
	labels := map[string]string{"coop.service": "db"}
	observed, found, err := rt.ObserveServiceContainer(t.Context(), labels)
	if err != nil || !found || observed.ID != id || !observed.Running || !observed.Networks["frozen-net"] {
		t.Fatalf("frozen observation = %+v, %v, %v; want the frozen daemon's running service", observed, found, err)
	}
	commands, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(commands)), "\n") {
		if !strings.Contains(line, "--host "+endpoint+" ") {
			t.Fatalf("a frozen observation followed the current context: %s", line)
		}
	}
	t.Setenv("COOP_TEST_DAEMON", "daemon-two")
	if _, found, err := rt.ObserveServiceContainer(t.Context(), labels); err == nil || found {
		t.Fatalf("an observation of a replaced daemon = %v, %v; want an error", found, err)
	}
}
