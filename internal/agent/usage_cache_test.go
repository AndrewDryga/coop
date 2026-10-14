package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func usageCacheFixture(id string, output int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-01T10:00:00Z","requestId":%q,"message":{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn","content":"transcript must not enter cache","usage":{"input_tokens":100,"output_tokens":%d}}}`+"\n", id, output)
}

func countedUsageSpec(calls *int) UsageSpec {
	spec := (claudeAgent{}).Usage()
	parse := spec.ParseHistory
	spec.ParseHistory = func(r io.Reader) (UsageHistory, error) {
		(*calls)++
		return parse(r)
	}
	return spec
}

func TestUsageHistoryBeyondFormerByteLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files int
		size  int64
	}{
		{"file", 1, 129 << 20},
		{"root", 3, 100 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			for i := 0; i < tc.files; i++ {
				path := filepath.Join(profile, "projects", "p", fmt.Sprintf("%d.jsonl", i))
				mustWriteSessionHistory(t, path, "")
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				// A sparse oversized record tests recovery without filling the host disk.
				if _, err := file.Seek(tc.size, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(file, "\n"+usageCacheFixture(fmt.Sprint(i), 20)); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			history := ReadUsageHistory(context.Background(), profile, (claudeAgent{}).Usage(), "")
			if len(history.Events) != tc.files || !history.Available || !history.Partial {
				t.Fatalf("later requests lost: events=%d partial=%v available=%v", len(history.Events), history.Partial, history.Available)
			}
		})
	}
}

func TestUsageHistoryCacheReuseAndInvalidation(t *testing.T) {
	profile, dir := t.TempDir(), t.TempDir()
	path := filepath.Join(profile, "projects", "p", "s.jsonl")
	data := usageCacheFixture("r", 10)
	mustWriteSessionHistory(t, path, data)
	calls := 0
	spec := countedUsageSpec(&calls)
	read := func(expectedCalls, expectedEvents, expectedOutput int) UsageHistory {
		t.Helper()
		h := ReadUsageHistory(context.Background(), profile, spec, dir)
		var output int64
		for _, e := range h.Events {
			output += e.Output
		}
		if calls != expectedCalls || len(h.Events) != expectedEvents || output != int64(expectedOutput) || h.Partial {
			t.Fatalf("calls=%d events=%d output=%d partial=%v", calls, len(h.Events), output, h.Partial)
		}
		return h
	}
	read(1, 1, 10)
	h := read(1, 1, 10)
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	if v := ValueUsageHistory(h, now.Add(31*24*time.Hour), spec.Price); v.Priced != 0 || v.USD != 0 {
		t.Fatalf("cached events did not age out: %+v", v)
	}
	if v := ValueUsageHistory(h, now, func(UsageEvent) (float64, bool) { return 7, true }); v.USD != 7 {
		t.Fatalf("cache persisted an old tariff: %+v", v)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, usageCacheFixture("s", 20)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	read(2, 2, 30)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	read(3, 0, 0)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	read(4, 1, 10)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(usageCacheFixture("r", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	read(5, 1, 20) // Same size and mtime, different ctime.
	replacement := filepath.Join(profile, "replacement")
	if err := os.WriteFile(replacement, []byte(usageCacheFixture("r", 30)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	read(6, 1, 30)
	read(6, 1, 30)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	read(6, 0, 0)
}

func TestUsageHistoryCacheFailureAndPrivacy(t *testing.T) {
	for _, failure := range []string{"corrupt", "version", "invalid event", "entry link", "entry pipe", "root link", "unwritable"} {
		t.Run(failure, func(t *testing.T) {
			profile, dir := t.TempDir(), t.TempDir()
			path := filepath.Join(profile, "projects", "p", "s.jsonl")
			mustWriteSessionHistory(t, path, usageCacheFixture("r", 10))
			calls := 0
			spec := countedUsageSpec(&calls)
			h := ReadUsageHistory(context.Background(), profile, spec, dir)
			if len(h.Events) != 1 || h.Partial {
				t.Fatalf("initial read: %+v", h)
			}
			cache := openUsageCache(dir, profile, spec.HistoryCacheID)
			if cache == nil {
				t.Fatal("cache was not created")
			}
			name := filepath.Join(cache.dir, usageCacheFile(filepath.Join("projects", "p", "s.jsonl")))
			cache.root.Close()
			info, err := os.Stat(cache.dir)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("private directory: %v %v", info, err)
			}
			info, err = os.Stat(name)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("private file: %v %v", info, err)
			}
			data, err := os.ReadFile(name)
			if err != nil || strings.Contains(string(data), "transcript must not enter cache") {
				t.Fatalf("transcript persisted or missing cache: %v", err)
			}
			switch failure {
			case "corrupt":
				if err := os.WriteFile(name, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "version", "invalid event":
				var entry usageCacheEntry
				if err := json.Unmarshal(data, &entry); err != nil {
					t.Fatal(err)
				}
				if failure == "version" {
					entry.Version++
				} else {
					entry.History.Events[0].Input = -1
				}
				data, err = json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "entry link", "entry pipe":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if failure == "entry link" {
					outside := filepath.Join(t.TempDir(), "cache.json")
					if err := os.WriteFile(outside, data, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, name); err != nil {
						t.Fatal(err)
					}
				} else if err := syscall.Mkfifo(name, 0o600); err != nil {
					t.Fatal(err)
				}
			case "root link":
				linked := filepath.Join(t.TempDir(), "cache-link")
				if err := os.Symlink(dir, linked); err != nil {
					t.Fatal(err)
				}
				dir = linked
			case "unwritable":
				dir = filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(dir, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h = ReadUsageHistory(context.Background(), profile, spec, dir)
			if calls != 2 || len(h.Events) != 1 || h.Partial {
				t.Fatalf("cache failure lost native usage: calls=%d history=%+v", calls, h)
			}
		})
	}
}

func TestUsageHistoryInterruptedReadsAreNotCached(t *testing.T) {
	for _, cause := range []string{"cancel", "append", "replace"} {
		t.Run(cause, func(t *testing.T) {
			profile, dir := t.TempDir(), t.TempDir()
			path := filepath.Join(profile, "projects", "p", "s.jsonl")
			mustWriteSessionHistory(t, path, usageCacheFixture("r", 10))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			spec := countedUsageSpec(&calls)
			parse := spec.ParseHistory
			spec.ParseHistory = func(r io.Reader) (UsageHistory, error) {
				h, err := parse(r)
				if calls == 1 {
					switch cause {
					case "cancel":
						cancel()
					case "append":
						f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := io.WriteString(f, usageCacheFixture("s", 20)); err != nil {
							t.Fatal(err)
						}
						if err := f.Close(); err != nil {
							t.Fatal(err)
						}
					case "replace":
						replacement := filepath.Join(profile, "new")
						if err := os.WriteFile(replacement, []byte(usageCacheFixture("s", 20)), 0o600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(replacement, path); err != nil {
							t.Fatal(err)
						}
					}
				}
				return h, err
			}
			if h := ReadUsageHistory(ctx, profile, spec, dir); !h.Partial {
				t.Fatal("interrupted read looked complete")
			}
			if h := ReadUsageHistory(context.Background(), profile, spec, dir); h.Partial || calls != 2 {
				t.Fatalf("interrupted result reused: %+v calls=%d", h, calls)
			}
			ReadUsageHistory(context.Background(), profile, spec, dir)
			if calls != 2 {
				t.Fatalf("stable result not cached: calls=%d", calls)
			}
		})
	}
}

func TestUsageHistoryDeepPathsAndSourceLink(t *testing.T) {
	profile, dir := t.TempDir(), t.TempDir()
	path := filepath.Join(profile, "projects", strings.Repeat("nested/", 10), "s.jsonl")
	mustWriteSessionHistory(t, path, usageCacheFixture("r", 10))
	spec := (claudeAgent{}).Usage()
	if h := ReadUsageHistory(context.Background(), profile, spec, dir); len(h.Events) != 1 || h.Partial {
		t.Fatalf("deep native request lost: %+v", h)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "s.jsonl")
	mustWriteSessionHistory(t, outside, usageCacheFixture("outside", 20))
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if h := ReadUsageHistory(context.Background(), profile, spec, dir); len(h.Events) != 0 || !h.Partial {
		t.Fatalf("source link or stale entry accepted: %+v", h)
	}
}

type usageReadError struct{ reader io.Reader }

func (r usageReadError) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if n > 0 {
		err = errors.New("transient disk read failure")
	}
	return n, err
}

func TestUsageHistoryReadErrorIsNotCached(t *testing.T) {
	profile, dir := t.TempDir(), t.TempDir()
	mustWriteSessionHistory(t, filepath.Join(profile, "projects", "p", "s.jsonl"), usageCacheFixture("r", 10))
	calls := 0
	spec := countedUsageSpec(&calls)
	parse := spec.ParseHistory
	spec.ParseHistory = func(r io.Reader) (UsageHistory, error) {
		if calls == 0 {
			reader := r.(*usageContextReader)
			reader.reader = usageReadError{reader.reader}
			r = reader
		}
		return parse(r)
	}
	if h := ReadUsageHistory(context.Background(), profile, spec, dir); !h.Partial {
		t.Fatal("read error looked complete")
	}
	if h := ReadUsageHistory(context.Background(), profile, spec, dir); h.Partial || calls != 2 {
		t.Fatalf("disk error cached indefinitely: partial=%v calls=%d", h.Partial, calls)
	}
	ReadUsageHistory(context.Background(), profile, spec, dir)
	if calls != 2 {
		t.Fatalf("recovered read not cached: %d", calls)
	}
}
