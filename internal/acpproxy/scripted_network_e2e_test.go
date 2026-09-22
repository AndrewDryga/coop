package acpproxy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the real outer CLI's admission and inner re-exec, not just a preconfigured spawn.
func TestScriptedACPOneOffOfflineOverridesAmbientOpen(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin, fixtureBin := filepath.Join(tmp, "coop"), filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	entry, guardedRuntime := filepath.Join(tmp, "offline-coop"), filepath.Join(tmp, "offline-runtime")
	if err := os.WriteFile(entry, []byte(fmt.Sprintf("#!/bin/sh\nexec %q \"$@\" --egress none\n", coopBin)), 0o700); err != nil {
		t.Fatal(err)
	}
	guard := fmt.Sprintf(`#!/bin/sh
if [ "$1" = run ]; then
  previous= offline=0
  for argument do
    if [ "$previous" = --network ] && [ "$argument" = none ]; then offline=1; fi
    previous=$argument
  done
  if [ "$offline" != 1 ]; then
    echo 'admitted offline ACP reached a runtime without --network none' >&2
    exit 97
  fi
fi
exec %q "$@"
`, fixtureBin)
	if err := os.WriteFile(guardedRuntime, []byte(guard), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"claude": {matrixGeneration("claude", "offline", repo, "offline fixture answer")},
	}})
	proc := startScriptedACPEnv(t, entry, guardedRuntime, repo, tmp, plan, "claude",
		map[string]string{"COOP_EGRESS": "open", "COOP_NETWORK": "1"}, "claude")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
	}); err != nil {
		t.Fatalf("offline initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
}
