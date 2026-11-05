package agent

import (
	"fmt"
	"os"
	"path/filepath"
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

func TestCodexExactResumeIncludesArchivedAndExecWithoutDiscovery(t *testing.T) {
	home := t.TempDir()
	const cwd = "/work/owned"
	archived := "01900000-0000-7000-8000-000000000001"
	execID := "01900000-0000-7000-8000-000000000002"
	cliID := "01900000-0000-7000-8000-000000000003"
	foreign := "01900000-0000-7000-8000-000000000004"
	for _, item := range []struct{ path, id, cwd, source string }{
		{"archived_sessions/old.jsonl", archived, cwd, "cli"},
		{"sessions/exec.jsonl", execID, cwd, "exec"},
		{"sessions/cli.jsonl", cliID, cwd, "cli"},
		{"archived_sessions/foreign.jsonl", foreign, "/work/foreign", "cli"},
	} {
		writeNativeHistoryFixture(t, home, item.path, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"source":%q}}`+"\n", item.id, item.cwd, item.source))
	}
	for _, id := range []string{archived, execID, cliID} {
		if found := findCodexSession(home, cwd, id); found != id {
			t.Fatalf("exact native ID lost: %s", id)
		}
	}
	if found := findCodexSession(home, cwd, foreign); found != "" {
		t.Fatal("exact lookup crossed repository boundary")
	}
	ids := codexSessionIDs(home, cwd)
	if len(ids) != 1 || ids[0] != cliID {
		t.Fatalf("interactive discovery broadened: %v", ids)
	}
}
