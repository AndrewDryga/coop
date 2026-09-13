//go:build providerlivee2e

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const providerLoopLiveArchiveCount = 300
const providerLoopLiveSearchID = "archive-299"
const providerLoopLiveSearchQuery = "frostlight"

func providerLoopLiveArchiveFiles() map[string]string {
	files := make(map[string]string, providerLoopLiveArchiveCount)
	for i := 0; i < providerLoopLiveArchiveCount; i++ {
		id := fmt.Sprintf("archive-%03d", i)
		title := "Completed fixture " + id
		if id == providerLoopLiveSearchID {
			title = "Repair frostlight retry"
		}
		files[filepath.Join(tasksRoot, stateDone, id, "task.md")] = "# " + title + "\n\n## Subtasks\n- [x] Verified fixture work\n"
	}
	return files
}

func prepareProviderLoopLiveArchive(repo string) error {
	for path, body := range providerLoopLiveArchiveFiles() {
		path = filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		// Pin the fixture's owned directory mode independently of the host umask.
		if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func verifyProviderLoopLiveArchive(layout procharness.Layout) ([]string, error) {
	var entries []string
	for path, body := range providerLoopLiveArchiveFiles() {
		full := filepath.Join(layout.Repo, path)
		got, err := readProviderLoopLiveFile(layout, full)
		info, statErr := os.Lstat(full)
		dirInfo, dirErr := os.Lstat(filepath.Dir(full))
		if err != nil || statErr != nil || dirErr != nil || string(got) != body || info.Mode() != 0o600 || dirInfo.Mode() != os.ModeDir|0o755 {
			return nil, fmt.Errorf("live loop changed the search archive")
		}
		entries = append(entries, filepath.Dir(path)+string(filepath.Separator), path)
	}
	return entries, nil
}

func TestProviderLoopLiveContractArchiveDirectoryMode(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareProviderLoopLiveArchive(layout.Repo); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyProviderLoopLiveArchive(layout); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.Repo, tasksRoot, stateDone, providerLoopLiveSearchID)
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyProviderLoopLiveArchive(layout); err == nil {
		t.Fatal("changed archive directory permissions passed verification")
	}
}

type providerLoopLiveSearchCounts struct {
	Calls       int `json:"calls"`
	Accepted    int `json:"accepted"`
	Matched     int `json:"matched"`
	Returned    int `json:"returned"`
	ResultBytes int `json:"result_bytes"`
}

func (s *providerLoopLiveTaskServer) observeSearchRequest(raw json.RawMessage) bool {
	s.statsMu.Lock()
	s.stats.Search.Calls++
	s.statsMu.Unlock()
	var args struct {
		Query string
		State string
	}
	return json.Unmarshal(raw, &args) == nil && strings.EqualFold(strings.TrimSpace(args.Query), providerLoopLiveSearchQuery) && (args.State == "" || args.State == "done")
}

func (s *providerLoopLiveTaskServer) observeSearchReply(text string, resultBytes int, refused bool) {
	if refused || resultBytes > 64<<10 {
		return
	}
	var result struct {
		Tasks             []struct{ ID, Title, State, Queue string }
		Matched, Returned int
		Truncated         bool
	}
	if json.Unmarshal([]byte(text), &result) != nil || result.Matched != 1 || result.Returned != 1 || result.Truncated || len(result.Tasks) != 1 {
		return
	}
	item := result.Tasks[0]
	if item.ID != providerLoopLiveSearchID || item.Title != "Repair frostlight retry" || item.State != "done" || item.Queue != s.root {
		return
	}
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats.Search.Accepted++
	s.stats.Search.Matched, s.stats.Search.Returned = result.Matched, result.Returned
	s.stats.Search.ResultBytes = max(s.stats.Search.ResultBytes, resultBytes)
}

func (s providerLoopLiveSearchCounts) valid(calls int) bool {
	return s.Calls >= 0 && s.Calls <= calls && s.Accepted >= 0 && s.Accepted <= s.Calls &&
		(s.Accepted == 0 && s.Matched == 0 && s.Returned == 0 && s.ResultBytes == 0 ||
			s.Accepted > 0 && s.Matched == 1 && s.Returned == 1 && s.ResultBytes > 0 && s.ResultBytes <= 64<<10)
}

func TestProviderLoopLiveContractSearchObservation(t *testing.T) {
	for _, tc := range []struct {
		name, query, id string
		refused         bool
		bytes           int
		want            bool
	}{
		{"narrow", "frostlight", providerLoopLiveSearchID, false, 500, true},
		{"unfiltered", "", providerLoopLiveSearchID, false, 500, false},
		{"other query", "archive", providerLoopLiveSearchID, false, 500, false},
		{"wrong id", "frostlight", "archive-001", false, 500, false},
		{"refused", "frostlight", providerLoopLiveSearchID, true, 500, false},
		{"oversized", "frostlight", providerLoopLiveSearchID, false, 65537, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newProviderLoopLiveTaskServer(t.TempDir(), "claude")
			if err != nil {
				t.Fatal(err)
			}
			_ = s.observeCall("tasks_list")
			args, _ := json.Marshal(map[string]string{"query": tc.query})
			if s.observeSearchRequest(args) {
				result, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"id": tc.id, "title": "Repair frostlight retry", "state": "done", "queue": s.root}}, "matched": 1, "returned": 1, "truncated": false})
				s.observeSearchReply(string(result), tc.bytes, tc.refused)
			}
			o := s.observation()
			if !o.valid() || (o.Search.Accepted == 1) != tc.want {
				t.Fatalf("search observation = %+v", o)
			}
		})
	}
}
