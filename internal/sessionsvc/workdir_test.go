package sessionsvc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
)

func TestSessionWorkdirMarkerKeepsNewAndLegacyHistoriesDistinct(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := "/host/forks/one"
	if got, err := SessionWorkdir(root, host); err != nil || got != host {
		t.Fatalf("legacy workdir = %q, %v", got, err)
	}
	if got, err := prepareSessionWorkdir(root, host, ""); err != nil || got != box.BareWorkdir {
		t.Fatalf("new workdir = %q, %v", got, err)
	}
	if got, err := prepareSessionWorkdir(root, host, "native-1"); err != nil || got != box.BareWorkdir {
		t.Fatalf("resumed new workdir = %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(root, stableWorkdirMarker)); err != nil {
		t.Fatal(err)
	}
	if got, err := prepareSessionWorkdir(root, host, "native-1"); err != nil || got != host {
		t.Fatalf("resumed legacy workdir = %q, %v", got, err)
	}
}

func TestSessionWorkdirRejectsForgedMarker(t *testing.T) {
	for name, makeMarker := range map[string]func(string) error{
		"symlink":  func(path string) error { return os.Symlink("/workspace", path) },
		"nonempty": func(path string) error { return os.WriteFile(path, []byte("wrong"), 0o600) },
		"readable": func(path string) error { return os.WriteFile(path, nil, 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := makeMarker(filepath.Join(root, stableWorkdirMarker)); err != nil {
				t.Fatal(err)
			}
			if got, err := SessionWorkdir(root, "/host/fork"); err == nil || got != "" {
				t.Fatalf("unsafe marker = %q, %v", got, err)
			}
		})
	}
}
