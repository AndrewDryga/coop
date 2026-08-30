package cli

import (
	"path/filepath"
	"testing"
)

func TestWorkerConnectAcceptsOnlyOneExplicitConfiguration(t *testing.T) {
	path, err := parseWorkerConnectFlags([]string{"--config", "worker.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "worker.json" {
		t.Fatalf("path = %q", path)
	}
	for _, args := range [][]string{nil, {"--config"}, {"--token", "secret"}, {"--config", "a", "--config", "b"}} {
		if _, err := parseWorkerConnectFlags(args); err == nil {
			t.Fatalf("args %v were accepted", args)
		}
	}
}
