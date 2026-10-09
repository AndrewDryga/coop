package box

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestNativeHistoryDefersPausedLegacyWriterAndImportsFinalTail(t *testing.T) {
	for _, acp := range []bool{false, true} {
		t.Run(fmt.Sprint(acp), func(t *testing.T) {
			ctx := context.Background()
			cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
			repo := t.TempDir()
			source := cfg.AgentProfileDir("codex", "default")
			if acp {
				source = acpSharedDir(cfg, "codex")
			}
			path := filepath.Join(source, "sessions", "2026", "10", "09", "rollout-owned.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			initial := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"11111111-2222-4333-8444-555555555555\",\"cwd\":%q,\"source\":\"cli\"}}\n", repo)
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			busy := filepath.Join(t.TempDir(), "busy")
			if err := os.WriteFile(busy, nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_TEST_HISTORY_BUSY", busy)
			t.Setenv("COOP_TEST_HISTORY_SOURCE", source)
			shim := filepath.Join(t.TempDir(), "runtime-fixture")
			script := `#!/bin/sh
case "$1" in
ps) if [ -f "$COOP_TEST_HISTORY_BUSY" ]; then echo legacy; fi ;;
inspect) printf '[{"Type":"bind","RW":true,"Source":"%s"}]\n' "$COOP_TEST_HISTORY_SOURCE" ;;
*) exit 19 ;;
esac
`
			if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			rt := runtime.Runtime{Name: shim}
			if _, err := PrepareNativeHome(ctx, cfg, rt, "codex", "default", repo, acp); err == nil {
				t.Fatal("paused writer admitted")
			}
			home, err := NativeHomePath(cfg, "codex", "default", repo, acp)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(home), "owner.json")); !os.IsNotExist(err) {
				t.Fatal("busy source published native ownership")
			}
			final := initial + "{\"type\":\"response_item\",\"payload\":{\"text\":\"final retained turn\"}}\n"
			if err := os.WriteFile(path, []byte(final), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(busy); err != nil {
				t.Fatal(err)
			}
			home, err = PrepareNativeHome(ctx, cfg, rt, "codex", "default", repo, acp)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(home, "sessions", "2026", "10", "09", "rollout-owned.jsonl"))
			if err != nil || string(data) != final {
				t.Fatal("writer's final turn lost", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("original history removed", err)
			}
			// A completed import no longer depends on an obsolete runtime.
			if _, err := PrepareNativeHome(ctx, cfg, runtime.Runtime{Name: "must-not-execute"}, "codex", "default", repo, acp); err != nil {
				t.Fatal("completed import re-fenced old writer", err)
			}
			other, err := PrepareNativeHome(ctx, cfg, runtime.Runtime{Name: "must-not-execute"}, "codex", "default", t.TempDir(), acp)
			if err != nil {
				t.Fatal("foreign-only source required runtime", err)
			}
			if strings.Contains(other, "/profiles/") {
				t.Fatal("legacy profile selected as native home")
			}
		})
	}
}
