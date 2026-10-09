package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNativeHistoryFixture(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeHistoryImportsOwnedRecordsAndKeepsForeignOnHost(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	const foreign = "22222222-2222-4333-8444-555555555555"
	const cwd = "/work/owned"
	for _, provider := range Names() {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			var ownedPath, foreignPath string
			switch provider {
			case "claude":
				ownedPath = filepath.Join("projects", ClaudeProjectKey(cwd), id+".jsonl")
				foreignPath = filepath.Join("projects", ClaudeProjectKey(cwd), foreign+".jsonl")
				writeNativeHistoryFixture(t, root, ownedPath, fmt.Sprintf(`{"cwd":%q,"sessionId":%q}`+"\n", cwd, id))
				writeNativeHistoryFixture(t, root, foreignPath, fmt.Sprintf(`{"cwd":"/work/foreign","sessionId":%q}`+"\n", foreign))
			case "codex":
				ownedPath = filepath.Join("archived_sessions", "2026", "10", "08", "rollout-"+id+".jsonl")
				foreignPath = filepath.Join("sessions", "2026", "10", "08", "rollout-"+foreign+".jsonl")
				writeNativeHistoryFixture(t, root, ownedPath, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"source":"exec"}}`+"\n", id, cwd))
				writeNativeHistoryFixture(t, root, foreignPath, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/work/foreign","source":"cli"}}`+"\n", foreign))
				writeNativeHistoryFixture(t, root, "history.jsonl", fmt.Sprintf(`{"session_id":%q,"text":"OWN"}`+"\n"+`{"session_id":%q,"text":"FOREIGN"}`+"\n", id, foreign))
				writeNativeHistoryFixture(t, root, "session_index.jsonl", fmt.Sprintf(`{"id":%q,"thread_name":"OWN"}`+"\n"+`{"id":%q,"thread_name":"FOREIGN"}`+"\n", id, foreign))
				writeNativeHistoryFixture(t, root, filepath.Join("shell_snapshots", id+".123.sh"), "OWN\n")
			case "gemini":
				ownedPath = filepath.Join("tmp", "owned", "chats", id+".jsonl")
				foreignPath = filepath.Join("tmp", "foreign", "chats", foreign+".jsonl")
				writeNativeHistoryFixture(t, root, "tmp/owned/.project_root", cwd+"\n")
				writeNativeHistoryFixture(t, root, "tmp/foreign/.project_root", "/work/foreign\n")
				writeNativeHistoryFixture(t, root, ownedPath, fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x"}`+"\n", id, sha256.Sum256([]byte(cwd))))
				writeNativeHistoryFixture(t, root, foreignPath, fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x"}`+"\n", foreign, sha256.Sum256([]byte("/work/foreign"))))
			case "grok":
				ownedPath = filepath.Join("sessions", "owned", id, "summary.json")
				foreignPath = filepath.Join("sessions", "foreign", foreign, "summary.json")
				writeNativeHistoryFixture(t, root, "sessions/owned/.cwd", cwd+"\n")
				writeNativeHistoryFixture(t, root, "sessions/foreign/.cwd", "/work/foreign\n")
				writeNativeHistoryFixture(t, root, ownedPath, "{}\n")
				writeNativeHistoryFixture(t, root, foreignPath, "{}\n")
			default:
				t.Fatalf("missing native migration fixture for %s", provider)
			}
			ag, _ := Get(provider)
			plan, err := ag.NativeHistory(root, func(value string) bool { return value == cwd })
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range plan.Files {
				if file.Path == foreignPath {
					t.Fatal("foreign history imported")
				}
				data, err := os.ReadFile(filepath.Join(root, file.Path))
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(data)
				if file.Size != int64(len(data)) || file.SHA256 != hex.EncodeToString(sum[:]) {
					t.Fatalf("wrong source witness for %s", file.Path)
				}
				if file.Path == ownedPath {
					found = file.CWD == cwd && file.SessionID == id
				}
			}
			if !found {
				t.Fatalf("owned native record absent: %+v", plan)
			}
			for path, spans := range plan.Filtered {
				data, err := os.ReadFile(filepath.Join(root, path))
				if err != nil {
					t.Fatal(err)
				}
				for _, span := range spans {
					if strings.Contains(string(data[span.Offset:span.Offset+span.Size]), "FOREIGN") {
						t.Fatalf("foreign filtered row in %s", path)
					}
				}
				if len(spans) != 1 {
					t.Fatalf("selected %d native index rows", len(spans))
				}
			}
			if _, err := os.Stat(filepath.Join(root, foreignPath)); err != nil {
				t.Fatal("foreign original was removed")
			}
		})
	}
}

func TestNativeHistoryWitnessRejectsChangesAndLinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "record")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := openNativeHistory(root, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.inspect("record", func(reader io.Reader) error {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("changed-size"), 0o600)
	}) {
		t.Fatal("changed source acquired a witness")
	}
	if err := os.Symlink(path, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if s.inspect("linked", nil) {
		t.Fatal("linked source acquired a witness")
	}
	if err := os.Link(path, filepath.Join(root, "hardlinked")); err != nil {
		t.Fatal(err)
	}
	if s.inspect("hardlinked", nil) {
		t.Fatal("hardlinked source acquired a witness")
	}
	if len(s.plan.Skipped) < 3 {
		t.Fatal("unsafe originals were not reported for recovery")
	}
}
