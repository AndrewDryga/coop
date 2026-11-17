package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestNativeOpenOwnerClosesPrivateClientWhenInspectionFails(t *testing.T) {
	pinHome(t)
	recorder := filepath.Join(t.TempDir(), "commands")
	rt := serviceReviewRuntime(t, recorder)
	docker, err := runtime.BindDocker(context.Background(), rt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	root, err := config.PrivateRunfiles(nil)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := filepath.Glob(filepath.Join(root, "coop-docker-client-*"))
	if err != nil || len(clients) != 1 {
		t.Fatalf("private clients: %v %v", clients, err)
	}
	sentinel := filepath.Join(root, "unrelated")
	if err = os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	owner := &nativeOpenOwner{docker: docker, ref: runtime.DockerRef{Name: "coop-native-fixture", Labels: map[string]string{LabelKey: LabelBroker}}}
	t.Setenv("COOP_TEST_DAEMON", "replacement-daemon")
	if err := owner.close(); err == nil {
		t.Fatal("changed daemon inspection succeeded")
	}
	if _, err := os.Stat(clients[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private client survived failed inspection: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatal("unrelated runtime artifact changed", err)
	}
	if data, err := os.ReadFile(recorder); err != nil || len(data) != 0 {
		t.Fatalf("unproven runtime was mutated: %s %v", data, err)
	}
}

// The owner's start is where a taken published port or a bad --add-host fails, so each readiness
// failure carries what Docker printed rather than a bare fixed sentence.
func TestNativeOpenReadinessFailureCarriesDockersWords(t *testing.T) {
	said := "Error response from daemon: Bind for 0.0.0.0:8080 failed: port is already allocated"
	for _, tc := range []struct {
		name    string
		ready   func(chan bool)
		running bool
		want    string
	}{
		{"no ready line", func(ready chan bool) { ready <- false }, true, "did not report readiness: " + said},
		{"stopped after ready", func(ready chan bool) { ready <- true }, false, "namespace is not running: " + said},
		{"timed out", func(chan bool) {}, true, "readiness timed out: " + said},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := &tailBuffer{max: 8 << 10}
			_, _ = stderr.Write([]byte(said + "\n"))
			ready := make(chan bool, 1)
			tc.ready(ready)
			err := awaitNativeOpenReady(ready, 10*time.Millisecond, stderr, func() bool { return tc.running })
			if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
				t.Fatalf("readiness error = %v, want it to end %q", err, tc.want)
			}
		})
	}
	ready := make(chan bool, 1)
	ready <- true
	if err := awaitNativeOpenReady(ready, time.Second, &tailBuffer{max: 64}, func() bool { return true }); err != nil {
		t.Fatalf("a ready, running owner failed: %v", err)
	}
}
