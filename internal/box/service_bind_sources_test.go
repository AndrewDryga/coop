package box

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A service may share a Unix socket through a bind, as Compose validation allows, and may bind a
// folder; a path the project declares as needing a real file must be one, and a device or named pipe
// is never a bind source.
func TestServiceBindSourcesAllowSocketsUnlessTheProjectNeedsARealFile(t *testing.T) {
	// A Unix socket path must stay short, so the project lives under /tmp, not t.TempDir().
	workspace, err := os.MkdirTemp("/tmp", "cbs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(workspace) })
	agent := filepath.Join(workspace, ".agent")
	for _, dir := range []string{agent, filepath.Join(workspace, "run"), filepath.Join(workspace, "data")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("unix", filepath.Join(workspace, "run", "app.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := syscall.Mkfifo(filepath.Join(workspace, "run", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "config.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(agent, "compose.yml")
	project := func(required string) {
		t.Helper()
		body := "box:\n  egress: filtered\n"
		if required != "" {
			body += "services:\n  require_real_files: [" + required + "]\n"
		}
		if err := os.WriteFile(filepath.Join(agent, "project.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	compose := func(source string) []byte {
		return []byte("services:\n  app:\n    image: busybox\n    volumes:\n      - " + source + ":/mnt/x\n")
	}
	for _, tc := range []struct {
		name, source, required, refusal string
	}{
		{name: "socket", source: "../run/app.sock"},
		{name: "folder", source: "../data"},
		{name: "regular file", source: "../config.txt", required: "config.txt"},
		{name: "socket the project needs as a real file", source: "../run/app.sock", required: "run/app.sock", refusal: "must be a regular file"},
		{name: "folder the project needs as a real file", source: "../data", required: "data", refusal: "must be a regular file"},
		{name: "named pipe", source: "../run/pipe", refusal: "must be a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project(tc.required)
			err := checkServiceBindSources(workspace, file, compose(tc.source), nil)
			switch {
			case tc.refusal == "" && err != nil:
				t.Fatalf("bind of %s refused: %v", tc.source, err)
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
				t.Fatalf("bind of %s = %v, want %q", tc.source, err, tc.refusal)
			}
		})
	}
}
