package box

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestAutomaticProjectBuildRequiresOpenNetworking(t *testing.T) {
	for _, mode := range []string{"open", "filtered", "none"} {
		t.Run(mode, func(t *testing.T) {
			repo, _ := gitProject(t, "FROM debian\n")
			marker := filepath.Join(t.TempDir(), "build")
			script := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nif [ \"$1\" = build ]; then touch '"+marker+"'; fi\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Egress: mode, BoxHome: t.TempDir(), BaseImage: "coop-box"}
			err := BuildWith(runtime.Runtime{Name: script}, cfg, repo, false, "test", strings.NewReader(""), io.Discard)
			_, statErr := os.Stat(marker)
			if mode == "open" {
				if err != nil || statErr != nil {
					t.Fatalf("open automatic build did not run: %v / %v", err, statErr)
				}
			} else if err == nil || !strings.Contains(err.Error(), "coop build --egress "+mode) || !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("restricted automatic build: %v / builder marker %v", err, statErr)
			}
		})
	}
}
