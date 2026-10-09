package box

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestNativeIndexImportRecoveryPreservesConcurrentEdits(t *testing.T) {
	for _, state := range []string{"before", "after", "changed"} {
		t.Run(state, func(t *testing.T) {
			home, source, plan := nativeImportFixture(t)
			path := "history.jsonl"
			before, after := []byte("{\"text\":\"native\"}\n"), []byte("{\"text\":\"native\"}\n{\"text\":\"retained\"}\n")
			imports := filepath.Join(filepath.Dir(home), "imports")
			if err := os.MkdirAll(imports, 0700); err != nil {
				t.Fatal(err)
			}
			name := nativeIndexPrefix(path) + "recovery"
			digest := func(data []byte) nativeHistoryReceipt {
				return nativeHistoryReceipt{Size: int64(len(data)), Digest: fmt.Sprintf("%x", sha256.Sum256(data))}
			}
			receipt := nativeIndexReceipt{Version: 1, Path: path, Before: digest(before), After: digest(after)}
			data, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(imports, name+".json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if state != "after" {
				if err := os.WriteFile(filepath.Join(imports, name+".data"), after, 0600); err != nil {
					t.Fatal(err)
				}
			}
			current := before
			if state == "after" {
				current = after
			}
			if state == "changed" {
				current = []byte("{\"text\":\"edited\"}\n")
			}
			if err := os.WriteFile(filepath.Join(home, path), current, 0600); err != nil {
				t.Fatal(err)
			}
			plan.Files = []agents.NativeHistoryFile{{Path: path}}
			plan.Indexes = map[string]string{path: ""}
			err = importNativeHistory(t.Context(), home, source, plan)
			if state == "changed" {
				if err == nil || string(mustReadFile(t, filepath.Join(home, path))) != string(current) {
					t.Fatal("concurrent native edit overwritten", err)
				}
				if string(mustReadFile(t, filepath.Join(imports, name+".data"))) != string(after) {
					t.Fatal("recovery stage lost")
				}
				return
			}
			if err != nil || string(mustReadFile(t, filepath.Join(home, path))) != string(after) {
				t.Fatal("recovery lost index", err)
			}
			if _, err := os.Stat(filepath.Join(imports, name+".data")); !os.IsNotExist(err) {
				t.Fatal("completed stage retained")
			}
		})
	}
}

func TestPrivateACPImportsInactiveAccountHistoryWithoutForeignRows(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	repo := t.TempDir()
	for i, account := range []string{"work", "personal"} {
		id := fmt.Sprintf("%08d-2222-4333-8444-555555555555", i+1)
		profile := cfg.AgentProfileDir("codex", account)
		if err := os.MkdirAll(profile, 0700); err != nil {
			t.Fatal(err)
		}
		writeRepoFile(t, filepath.Join(profile, "sessions", "rollout-"+id+".jsonl"), fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/workspace"}}`+"\n", id))
		writeRepoFile(t, filepath.Join(profile, "history.jsonl"), fmt.Sprintf(`{"session_id":%q,"text":%q}`+"\n", id, account))
		writeRepoFile(t, filepath.Join(profile, "sessions", "rollout-foreign.jsonl"), `{"type":"session_meta","payload":{"id":"99999999-2222-4333-8444-555555555555","cwd":"/foreign"}}`+"\n")
	}
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1\" in ps) exit 0;; *) exit 19;; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	home, err := PreparePrivateACPHome(t.Context(), cfg, runtime.Runtime{Name: shim}, "codex", "work", repo, func(cwd string) bool { return cwd == "/workspace" })
	if err != nil {
		t.Fatal(err)
	}
	data := string(mustReadFile(t, filepath.Join(home, "history.jsonl")))
	if !strings.Contains(data, "work") || !strings.Contains(data, "personal") {
		t.Fatal("inactive account history lost")
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", "rollout-foreign.jsonl")); !os.IsNotExist(err) {
		t.Fatal("foreign history imported")
	}
	again, err := PreparePrivateACPHome(t.Context(), cfg, runtime.Runtime{Name: "must-not-run"}, "codex", "personal", repo, func(cwd string) bool { return cwd == "/workspace" })
	if err != nil || again != home {
		t.Fatal("account switch changed ACP home or re-fenced complete import", err)
	}
}

func TestNativeIndexImportsLatestRetainedTitle(t *testing.T) {
	home, source, plan := nativeImportFixture(t)
	path := "session_index.jsonl"
	data := "{\"id\":\"one\",\"thread_name\":\"old\"}\n{\"id\":\"two\",\"thread_name\":\"other\"}\n{\"id\":\"one\",\"thread_name\":\"latest\"}\n"
	writeRepoFile(t, filepath.Join(source, path), data)
	var spans []agents.NativeHistorySpan
	if err := agents.NativeHistoryIndexRows(strings.NewReader(data), "id", func(key string, offset, size int64) error {
		spans = append(spans, agents.NativeHistorySpan{Key: key, Offset: offset, Size: size})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan.Files = []agents.NativeHistoryFile{{Path: path, Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}}
	plan.Indexes = map[string]string{path: "id"}
	plan.Filtered = map[string][]agents.NativeHistorySpan{path: spans}
	if err := importNativeHistory(t.Context(), home, source, plan); err != nil {
		t.Fatal(err)
	}
	got := string(mustReadFile(t, filepath.Join(home, path)))
	if strings.Contains(got, "old") || !strings.Contains(got, "latest") || !strings.Contains(got, "other") {
		t.Fatal("append-only native title update lost", got)
	}
}

func TestNativeHistoryIndexMergesAccountsAndRemembersDeletedRows(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get("codex")
	const cwd = "/owned/repository"
	id1, id2 := "11111111-2222-4333-8444-555555555555", "22222222-2222-4333-8444-555555555555"
	makeSource := func(id, text string) string {
		source := t.TempDir()
		writeRepoFile(t, filepath.Join(source, "sessions", "rollout-"+id+".jsonl"), fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`+"\n", id, cwd))
		writeRepoFile(t, filepath.Join(source, "history.jsonl"), fmt.Sprintf(`{"session_id":%q,"text":%q}`+"\n", id, text))
		writeRepoFile(t, filepath.Join(source, "session_index.jsonl"), fmt.Sprintf(`{"id":%q,"thread_name":%q}`+"\n", id, text))
		return source
	}
	importSource := func(source string) {
		plan, err := ag.NativeHistory(source, func(got string) bool { return got == cwd })
		if err != nil {
			t.Fatal(err)
		}
		if err := importNativeHistory(t.Context(), home, source, plan); err != nil {
			t.Fatal(err)
		}
		if complete, err := nativeHistoryImported(home, source, plan); err != nil || !complete {
			t.Fatal("index receipt not complete", err)
		}
	}
	first, second := makeSource(id1, "first"), makeSource(id2, "second")
	importSource(first)
	importSource(second)
	for _, name := range []string{"history.jsonl", "session_index.jsonl"} {
		data := string(mustReadFile(t, filepath.Join(home, name)))
		if !strings.Contains(data, id1) || !strings.Contains(data, id2) {
			t.Fatal("account history missing", name)
		}
		if err := os.WriteFile(filepath.Join(home, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Identical source rows under another account must not resurrect native deletion.
	importSource(makeSource(id1, "first"))
	for _, name := range []string{"history.jsonl", "session_index.jsonl"} {
		if data := mustReadFile(t, filepath.Join(home, name)); len(data) != 0 {
			t.Fatal("deleted native index row resurrected")
		}
	}
	// New eligible rows from the same old index are still imported.
	writeRepoFile(t, filepath.Join(second, "history.jsonl"), fmt.Sprintf(`{"session_id":%q,"text":"second"}`+"\n"+`{"session_id":%q,"text":"later"}`+"\n", id2, id2))
	importSource(second)
	data := string(mustReadFile(t, filepath.Join(home, "history.jsonl")))
	if !strings.Contains(data, "later") || strings.Contains(data, "second") {
		t.Fatal("new row lost or deleted old row resurrected")
	}
}

func TestNativeHistoryIndexPreservesCurrentSessionTitle(t *testing.T) {
	home, source, plan := nativeImportFixture(t)
	const id = "11111111-2222-4333-8444-555555555555"
	path := "session_index.jsonl"
	data := fmt.Sprintf(`{"id":%q,"thread_name":"retained"}`+"\n", id)
	writeRepoFile(t, filepath.Join(source, path), data)
	current := fmt.Sprintf(`{"id":%q,"thread_name":"current native title"}`+"\n", id)
	writeRepoFile(t, filepath.Join(home, path), current)
	var spans []agents.NativeHistorySpan
	if err := agents.NativeHistoryIndexRows(strings.NewReader(data), "id", func(key string, offset, size int64) error {
		spans = append(spans, agents.NativeHistorySpan{Offset: offset, Size: size, Key: key})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	witness := agents.NativeHistoryFile{Path: path, Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}
	plan.Files = []agents.NativeHistoryFile{witness}
	plan.Indexes = map[string]string{path: "id"}
	plan.Filtered = map[string][]agents.NativeHistorySpan{path: spans}
	if err := importNativeHistory(t.Context(), home, source, plan); err != nil {
		t.Fatal(err)
	}
	if string(mustReadFile(t, filepath.Join(home, path))) != current {
		t.Fatal("retained title overwrote native title")
	}
}
