package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
