package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func nativeImportFixture(t *testing.T) (string, string, agents.NativeHistoryPlan) {
	t.Helper()
	ifHome, err := prepareNativeHome(context.Background(), &config.Config{ConfigDir: t.TempDir()}, "codex", "default", t.TempDir(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	path, content := "sessions/old.jsonl", "own native transcript\n"
	if err := os.MkdirAll(filepath.Join(source, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return ifHome, source, agents.NativeHistoryPlan{Files: []agents.NativeHistoryFile{
		{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))},
	}}
}

func TestNativeHistoryImportPreservesOriginalAndNativeDeletion(t *testing.T) {
	home, source, plan := nativeImportFixture(t)
	if err := importNativeHistory(context.Background(), home, source, plan); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(home, plan.Files[0].Path)
	got, err := os.ReadFile(name)
	if err != nil || string(got) != "own native transcript\n" {
		t.Fatalf("history=%q, err=%v", got, err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := importNativeHistory(context.Background(), home, source, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("import resurrected a native deletion")
	}
	if _, err := os.Stat(filepath.Join(source, plan.Files[0].Path)); err != nil {
		t.Fatal("source was removed", err)
	}
}

func TestNativeHistoryImportRefusesChangedLinkedAndConflictingFiles(t *testing.T) {
	for _, kind := range []string{"changed-source", "source-link", "destination-link", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			home, source, plan := nativeImportFixture(t)
			original := filepath.Join(source, plan.Files[0].Path)
			dest := filepath.Join(home, plan.Files[0].Path)
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "changed-source":
				err = os.WriteFile(original, []byte("changed source"), 0o600)
			case "source-link":
				err = os.Link(original, filepath.Join(source, "alias"))
			case "destination-link":
				err = os.Symlink(original, dest)
			case "conflict":
				err = os.WriteFile(dest, []byte("native edit"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := importNativeHistory(context.Background(), home, source, plan); err == nil {
				t.Fatal("ambiguous import succeeded")
			}
			if kind == "conflict" {
				data, err := os.ReadFile(dest)
				if err != nil || string(data) != "native edit" {
					t.Fatal("native edit overwritten")
				}
			}
		})
	}
}

func TestNativeHistoryImportFiltersIndexes(t *testing.T) {
	home, source, plan := nativeImportFixture(t)
	content := "foreign\nown\nforeign\n"
	file := &plan.Files[0]
	if err := os.WriteFile(filepath.Join(source, file.Path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	file.Size, file.SHA256 = int64(len(content)), hex.EncodeToString(sum[:])
	plan.Filtered = map[string][]agents.NativeHistorySpan{file.Path: {{Offset: 8, Size: 4}}}
	if err := importNativeHistory(context.Background(), home, source, plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, file.Path))
	if err != nil || string(data) != "own\n" {
		t.Fatalf("filtered=%q, err=%v", data, err)
	}
}

func TestNativeHistoryImportRecoversPendingPublication(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-link", true: "after-link"}[published], func(t *testing.T) {
			home, source, plan := nativeImportFixture(t)
			file := plan.Files[0]
			key := sha256.Sum256([]byte(source + "\x00" + file.Path))
			name := hex.EncodeToString(key[:])
			imports := filepath.Join(filepath.Dir(home), "imports")
			if err := os.Mkdir(imports, 0o700); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(source, file.Path))
			stage := filepath.Join(imports, name+".data")
			if err := os.WriteFile(stage, data, 0o600); err != nil {
				t.Fatal(err)
			}
			receipt, _ := json.Marshal(nativeHistoryReceipt{Version: 1, Source: source, Path: file.Path, Digest: file.SHA256, Size: file.Size,
				SourceDigest: file.SHA256, SourceSize: file.Size, DependenciesDigest: fmt.Sprintf("%x", sha256.Sum256(nil))})
			if err := os.WriteFile(filepath.Join(imports, name+".json"), receipt, 0o600); err != nil {
				t.Fatal(err)
			}
			if published {
				dest := filepath.Join(home, file.Path)
				if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(stage, dest); err != nil {
					t.Fatal(err)
				}
			}
			if err := importNativeHistory(context.Background(), home, source, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("published stage still linked")
			}
			got, _ := os.ReadFile(filepath.Join(imports, name+".json"))
			if !strings.Contains(string(got), `"done":true`) {
				t.Fatal("publication not completed")
			}
		})
	}
}
