package agent

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

const largeSessionHistoryEntries = 256

const (
	largeHistoryHitID       = "11111111-2222-4333-8444-555555555555"
	largeHistoryPersonalID  = "22222222-2222-4333-8444-555555555555"
	largeHistoryWrongCwdID  = "33333333-2222-4333-8444-555555555555"
	largeHistoryMalformedID = "44444444-2222-4333-8444-555555555555"
	largeHistoryMissingID   = "99999999-2222-4333-8444-555555555555"
)

func TestSessionLookupLargeHistory(t *testing.T) {
	for _, provider := range Names() {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			ws := "/work/large-history/repo"
			seedLargeSessionHistory(t, cfg, provider, config.DefaultProfile, ws, largeHistoryHitID, largeSessionHistoryEntries)
			seedLargeSessionHistory(t, cfg, provider, "personal", ws, largeHistoryPersonalID, 0)
			ag, _ := Get(provider)

			assertLargeHistoryResume(t, ag, cfg, ws, largeHistoryHitID, true)
			assertLargeHistoryResume(t, ag, cfg, ws, largeHistoryMissingID, false)
			assertLargeHistoryResume(t, ag, cfg, ws, largeHistoryWrongCwdID, false)
			assertLargeHistoryResume(t, ag, cfg, ws, largeHistoryMalformedID, false)
			assertLargeHistoryResume(t, ag, cfg, ws+"-wrong", largeHistoryHitID, false)
			if cmd := ag.StartSession(cfg, largeHistoryMissingID); len(cmd) == 0 || (ag.PresetSessionID() && !slices.Contains(cmd, largeHistoryMissingID)) {
				t.Fatalf("fresh session command = %v, want a runnable command scoped to the new id", cmd)
			}

			cfg.SetActiveProfile(provider, "personal")
			assertLargeHistoryResume(t, ag, cfg, ws, largeHistoryHitID, false)
			assertLargeHistoryResume(t, ag, cfg, ws, largeHistoryPersonalID, true)
			cfg.SetActiveProfile(provider, config.DefaultProfile)
			assertLargeHistoryResume(t, ag, cfg, ws, largeHistoryPersonalID, false)

			if discoverer, ok := ag.(SessionDiscoverer); ok {
				if got := discoverer.SessionIDs(cfg, ws); !slices.Equal(got, []string{largeHistoryHitID}) {
					t.Fatalf("SessionIDs = %v, want only exact CLI/cwd/account session", got)
				}
				if got := discoverer.LatestSessionID(cfg, ws); got != largeHistoryHitID {
					t.Fatalf("LatestSessionID = %q, want %q", got, largeHistoryHitID)
				}
			}

			fdBefore := openSessionFDCount()
			for i := range 32 {
				id := largeHistoryHitID
				if i%2 == 1 {
					id = largeHistoryMissingID
				}
				_, _ = ag.Resume(cfg, ws, id)
			}
			if fdAfter := openSessionFDCount(); fdBefore >= 0 && fdAfter > fdBefore {
				t.Fatalf("repeated lookups leaked descriptors: before=%d after=%d", fdBefore, fdAfter)
			}
		})
	}
}

func TestGeminiLargeLegacySession(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ws := "/work/large-legacy/repo"
	id := "77777777-2222-4333-8444-555555555555"
	root := cfg.AgentProfileDir("gemini", config.DefaultProfile)
	bucket := filepath.Join(root, "tmp", "legacy")
	mustWriteSessionHistory(t, filepath.Join(bucket, ".project_root"), ws+"\n")
	project := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
	body := `{"messages":[{"type":"user","content":"` + strings.Repeat("x", (1<<20)+1) +
		fmt.Sprintf(`"}],"sessionId":%q,"projectHash":%q}`, id, project)
	mustWriteSessionHistory(t, filepath.Join(bucket, "chats", "session.json"), body)
	ag, _ := Get("gemini")
	assertLargeHistoryResume(t, ag, cfg, ws, id, true)
}

func TestGeminiRejectsOversizedOrTrailingLegacySession(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ws := "/work/legacy-bounds/repo"
	root := cfg.AgentProfileDir("gemini", config.DefaultProfile)
	bucket := filepath.Join(root, "tmp", "legacy")
	mustWriteSessionHistory(t, filepath.Join(bucket, ".project_root"), ws+"\n")
	project := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
	cases := []struct {
		name string
		id   string
		body string
	}{
		{
			name: "oversized", id: "88888888-2222-4333-8444-555555555555",
			body: `{"messages":"` + strings.Repeat("x", geminiLegacyMetadataLimit) +
				fmt.Sprintf(`","sessionId":%q,"projectHash":%q}`, "88888888-2222-4333-8444-555555555555", project),
		},
		{
			name: "trailing value", id: "99999999-2222-4333-8444-555555555555",
			body: fmt.Sprintf(`{"sessionId":%q,"projectHash":%q}`, "99999999-2222-4333-8444-555555555555", project) + "\n{}",
		},
	}
	ag, _ := Get("gemini")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustWriteSessionHistory(t, filepath.Join(bucket, "chats", tc.name+".json"), tc.body)
			assertLargeHistoryResume(t, ag, cfg, ws, tc.id, false)
		})
	}
}

func TestGeminiRejectsOversizedCurrentMetadata(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ws := "/work/current-metadata-bound/repo"
	id := "aaaaaaaa-2222-4333-8444-555555555555"
	root := cfg.AgentProfileDir("gemini", config.DefaultProfile)
	bucket := filepath.Join(root, "tmp", "current")
	mustWriteSessionHistory(t, filepath.Join(bucket, ".project_root"), ws+"\n")
	project := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
	body := fmt.Sprintf(`{"sessionId":%q,"projectHash":%q,"padding":"`, id, project) +
		strings.Repeat("x", geminiMetadataLimit) + `"}`
	mustWriteSessionHistory(t, filepath.Join(bucket, "chats", "session.jsonl"), body+"\n")
	ag, _ := Get("gemini")
	assertLargeHistoryResume(t, ag, cfg, ws, id, false)
}

func BenchmarkGeminiLargeLegacySession(b *testing.B) {
	cfg := &config.Config{ConfigDir: b.TempDir()}
	ws := "/work/large-legacy/repo"
	id := "77777777-2222-4333-8444-555555555555"
	root := cfg.AgentProfileDir("gemini", config.DefaultProfile)
	bucket := filepath.Join(root, "tmp", "legacy")
	mustWriteSessionHistory(b, filepath.Join(bucket, ".project_root"), ws+"\n")
	project := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
	body := `{"messages":[{"type":"user","content":"` + strings.Repeat("x", (1<<20)+1) +
		fmt.Sprintf(`"}],"sessionId":%q,"projectHash":%q}`, id, project)
	mustWriteSessionHistory(b, filepath.Join(bucket, "chats", "session.json"), body)
	ag, _ := Get("gemini")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = ag.Resume(cfg, ws, id)
	}
}

func TestGeminiBucketCWD(t *testing.T) {
	dir := t.TempDir()
	for name, marker := range map[string]string{
		"exact":    "/work/repo\n",
		"relative": "work/repo\n",
		"oversize": strings.Repeat("x", geminiProjectRootLimit+1),
	} {
		mustWriteSessionHistory(t, filepath.Join(dir, name, ".project_root"), marker)
	}
	outside := filepath.Join(t.TempDir(), "marker")
	mustWriteSessionHistory(t, outside, "/work/outside\n")
	if err := os.MkdirAll(filepath.Join(dir, "linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked", ".project_root")); err != nil {
		t.Fatal(err)
	}
	root, err := openSessionRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if got := geminiBucketCWD(root, "exact"); got != "/work/repo" {
		t.Fatalf("exact marker = %q", got)
	}
	for _, name := range []string{"missing", "relative", "oversize", "linked"} {
		if got := geminiBucketCWD(root, name); got != "" {
			t.Errorf("%s marker = %q, want legacy fallback", name, got)
		}
	}
}

func BenchmarkSessionLookupLargeHistory(b *testing.B) {
	for _, provider := range Names() {
		b.Run(provider, func(b *testing.B) {
			cfg := &config.Config{ConfigDir: b.TempDir()}
			ws := "/work/large-history/repo"
			seedLargeSessionHistory(b, cfg, provider, config.DefaultProfile, ws, largeHistoryHitID, largeSessionHistoryEntries)
			ag, _ := Get(provider)
			for _, tc := range []struct {
				name string
				id   string
			}{{"hit", largeHistoryHitID}, {"full-miss", largeHistoryMissingID}} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						sessionBenchmarkCommand, sessionBenchmarkResumed = ag.Resume(cfg, ws, tc.id)
					}
				})
			}
		})
	}
}

var (
	sessionBenchmarkCommand []string
	sessionBenchmarkResumed bool
)

func assertLargeHistoryResume(t *testing.T, ag Agent, cfg *config.Config, ws, id string, want bool) {
	t.Helper()
	cmd, got := ag.Resume(cfg, ws, id)
	if got != want {
		t.Fatalf("Resume(%q, %q) = (%v, %v), want resumed=%v", ws, id, cmd, got, want)
	}
	if got && !slices.Contains(cmd, id) {
		t.Fatalf("resumed command %v does not pin exact id %q", cmd, id)
	}
}

func openSessionFDCount() int {
	for _, root := range []string{"/dev/fd", "/proc/self/fd"} {
		if entries, err := os.ReadDir(root); err == nil {
			return len(entries)
		}
	}
	return -1
}

func largeHistoryID(i int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
}

func mustWriteSessionHistory(tb testing.TB, path, body string) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		tb.Fatal(err)
	}
}

func seedLargeSessionHistory(tb testing.TB, cfg *config.Config, provider, account, ws, hitID string, entries int) {
	tb.Helper()
	root := cfg.AgentProfileDir(provider, account)
	switch provider {
	case "claude":
		for i := range entries {
			mustWriteSessionHistory(tb, filepath.Join(root, "projects", fmt.Sprintf("foreign-%04d", i), largeHistoryID(i)+".jsonl"), "{}\n")
		}
		project := filepath.Join(root, "projects", ClaudeProjectKey(ws))
		mustWriteSessionHistory(tb, filepath.Join(project, hitID+".jsonl"), "{}\n")
		if err := os.MkdirAll(filepath.Join(project, largeHistoryMalformedID+".jsonl"), 0o700); err != nil {
			tb.Fatal(err)
		}
		mustWriteSessionHistory(tb, filepath.Join(root, "projects", ClaudeProjectKey(ws+"-wrong"), largeHistoryWrongCwdID+".jsonl"), "{}\n")
	case "codex":
		sessions := filepath.Join(root, "sessions", "2026", "07", "16")
		for i := range entries {
			line := codexHistoryLine(largeHistoryID(i), fmt.Sprintf("/work/foreign/%04d", i), "session_meta", "cli")
			mustWriteSessionHistory(tb, filepath.Join(sessions, fmt.Sprintf("rollout-%04d.jsonl", i)), line)
		}
		mustWriteSessionHistory(tb, filepath.Join(sessions, "rollout-hit.jsonl"), codexHistoryLine(hitID, ws, "session_meta", "cli"))
		mustWriteSessionHistory(tb, filepath.Join(sessions, "rollout-wrong-cwd.jsonl"), codexHistoryLine(largeHistoryWrongCwdID, ws+"-wrong", "session_meta", "cli"))
		mustWriteSessionHistory(tb, filepath.Join(sessions, "rollout-malformed.jsonl"), codexHistoryLine(largeHistoryMalformedID, ws, "response_item", "cli"))
		mustWriteSessionHistory(tb, filepath.Join(sessions, "rollout-exec.jsonl"), codexHistoryLine(largeHistoryMalformedID, ws, "session_meta", "exec"))
	case "gemini":
		for i := range entries {
			bucketName := fmt.Sprintf("foreign-%04d", i/16)
			cwd := "/work/" + bucketName
			bucket := filepath.Join(root, "tmp", bucketName)
			mustWriteSessionHistory(tb, filepath.Join(bucket, ".project_root"), cwd+"\n")
			project := fmt.Sprintf("%x", sha256.Sum256([]byte(cwd)))
			metadata := fmt.Sprintf(`{"sessionId":%q,"projectHash":%q}`, largeHistoryID(i), project)
			mustWriteSessionHistory(tb, filepath.Join(bucket, "chats", fmt.Sprintf("session-%04d.jsonl", i)), metadata+"\n{\"id\":\"message\"}\n")
		}
		target := filepath.Join(root, "tmp", "target")
		mustWriteSessionHistory(tb, filepath.Join(target, ".project_root"), ws+"\n")
		project := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
		mustWriteSessionHistory(tb, filepath.Join(target, "chats", "session-hit.jsonl"), fmt.Sprintf(`{"sessionId":%q,"projectHash":%q}`, hitID, project)+"\n")
		mustWriteSessionHistory(tb, filepath.Join(target, "chats", "session-malformed.json"), fmt.Sprintf(`{"sessionId":%q,"projectHash":17}`, largeHistoryMalformedID))
		wrong := filepath.Join(root, "tmp", "wrong")
		mustWriteSessionHistory(tb, filepath.Join(wrong, ".project_root"), ws+"-wrong\n")
		wrongProject := fmt.Sprintf("%x", sha256.Sum256([]byte(ws+"-wrong")))
		mustWriteSessionHistory(tb, filepath.Join(wrong, "chats", "session-wrong.jsonl"), fmt.Sprintf(`{"sessionId":%q,"projectHash":%q}`, largeHistoryWrongCwdID, wrongProject)+"\n")
		for i := range 16 {
			legacyCWD := fmt.Sprintf("/work/legacy/%02d", i)
			legacyProject := fmt.Sprintf("%x", sha256.Sum256([]byte(legacyCWD)))
			body := `{"messages":"` + strings.Repeat("x", 4<<10) + fmt.Sprintf(`","sessionId":%q,"projectHash":%q}`, largeHistoryID(entries+i), legacyProject)
			mustWriteSessionHistory(tb, filepath.Join(root, "tmp", "legacy", "chats", fmt.Sprintf("session-%02d.json", i)), body)
		}
	case "grok":
		for i := range entries {
			bucket := filepath.Join(root, "sessions", fmt.Sprintf("foreign-%04d", i))
			mustWriteSessionHistory(tb, filepath.Join(bucket, ".cwd"), fmt.Sprintf("/work/foreign/%04d\n", i))
			mustWriteSessionHistory(tb, filepath.Join(bucket, largeHistoryID(i), "summary.json"), "{}\n")
		}
		target := filepath.Join(root, "sessions", "target")
		mustWriteSessionHistory(tb, filepath.Join(target, ".cwd"), ws+"\n")
		mustWriteSessionHistory(tb, filepath.Join(target, hitID, "summary.json"), "{}\n")
		if err := os.MkdirAll(filepath.Join(target, largeHistoryMalformedID), 0o700); err != nil {
			tb.Fatal(err)
		}
		wrong := filepath.Join(root, "sessions", "wrong")
		mustWriteSessionHistory(tb, filepath.Join(wrong, ".cwd"), ws+"-wrong\n")
		mustWriteSessionHistory(tb, filepath.Join(wrong, largeHistoryWrongCwdID, "summary.json"), "{}\n")
	default:
		tb.Fatalf("unhandled provider %q", provider)
	}
}

func codexHistoryLine(id, cwd, recordType, source string) string {
	return fmt.Sprintf(`{"type":%q,"payload":{"id":%q,"cwd":%q,"source":%q}}`+"\n", recordType, id, cwd, source)
}
