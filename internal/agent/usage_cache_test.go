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
			history := ReadUsageHistory(context.Background(), profile, (claudeAgent{}).Usage(), nil)
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
		h := ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
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
			h := ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
			if len(h.Events) != 1 || h.Partial {
				t.Fatalf("initial read: %+v", h)
			}
			cache := NewUsageCache(dir).open(profile, spec.HistoryCacheID)
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
			h = ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
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
			if h := ReadUsageHistory(ctx, profile, spec, NewUsageCache(dir)); !h.Partial {
				t.Fatal("interrupted read looked complete")
			}
			if h := ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir)); h.Partial || calls != 2 {
				t.Fatalf("interrupted result reused: %+v calls=%d", h, calls)
			}
			ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
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
	if h := ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir)); len(h.Events) != 1 || h.Partial {
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
	if h := ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir)); len(h.Events) != 0 || !h.Partial {
		t.Fatalf("source link or stale entry accepted: %+v", h)
	}
}

func usageCacheEntryPath(t *testing.T, dir, profile, name string) string {
	t.Helper()
	key, ok := usageCacheKey(profile, (claudeAgent{}).Usage().HistoryCacheID)
	if !ok {
		t.Fatal("no cache key")
	}
	return filepath.Join(dir, key, usageCacheFile(filepath.Join("projects", "p", name)))
}

func TestUsageCachePruneAfterCompleteScan(t *testing.T) {
	profile, retired, unlisted, dir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	write := func(root, name, data string) {
		mustWriteSessionHistory(t, filepath.Join(root, "projects", "p", name), data)
	}
	write(profile, "kept.jsonl", usageCacheFixture("kept", 10))
	write(profile, "gone.jsonl", usageCacheFixture("gone", 20))
	// A record the decoder rejects makes the history partial, not the walk.
	write(profile, "rejected.jsonl", "{\n"+usageCacheFixture("rejected", 30))
	write(retired, "s.jsonl", usageCacheFixture("retired", 40))
	write(unlisted, "s.jsonl", usageCacheFixture("unlisted", 50))
	calls := 0
	spec := countedUsageSpec(&calls)
	first := NewUsageCache(dir)
	for _, root := range []string{profile, retired, unlisted} {
		ReadUsageHistory(context.Background(), root, spec, first)
	}
	first.Prune()
	kept, gone := usageCacheEntryPath(t, dir, profile, "kept.jsonl"), usageCacheEntryPath(t, dir, profile, "gone.jsonl")
	rejected := usageCacheEntryPath(t, dir, profile, "rejected.jsonl")
	retiredEntry, unlistedEntry := usageCacheEntryPath(t, dir, retired, "s.jsonl"), usageCacheEntryPath(t, dir, unlisted, "s.jsonl")
	for _, entry := range []string{kept, gone, rejected, retiredEntry, unlistedEntry} {
		if _, err := os.Stat(entry); err != nil {
			t.Fatalf("a scan that used every entry pruned one: %v", err)
		}
	}
	if err := os.Remove(filepath.Join(profile, "projects", "p", "gone.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(retired); err != nil {
		t.Fatal(err)
	}
	// Only private regular files named like Coop's entries are Coop's to delete.
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := usageCacheEntryPath(t, dir, profile, "linked.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	shared := usageCacheEntryPath(t, dir, profile, "shared.jsonl")
	if err := os.WriteFile(shared, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o644); err != nil { // Independent of the umask.
		t.Fatal(err)
	}
	note := filepath.Join(filepath.Dir(unlistedEntry), "notes.json")
	if err := os.WriteFile(note, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// os.Root follows a relative link to a sibling root; Prune must not prune through it.
	rootLink := filepath.Join(dir, strings.Repeat("a", 64))
	if err := os.Symlink(filepath.Base(filepath.Dir(kept)), rootLink); err != nil {
		t.Fatal(err)
	}

	second := NewUsageCache(dir)
	h := ReadUsageHistory(context.Background(), profile, spec, second)
	ReadUsageHistory(context.Background(), retired, spec, second)
	second.Prune()
	if calls != 5 || len(h.Events) != 2 || !h.Partial {
		t.Fatalf("live entries were not reused: calls=%d history=%+v", calls, h)
	}
	for _, removed := range []string{gone, filepath.Dir(retiredEntry), unlistedEntry} {
		if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unused cache entry %s survived a complete scan: %v", removed, err)
		}
	}
	for _, survivor := range []string{kept, rejected, link, outside, shared, note, rootLink} {
		if _, err := os.Lstat(survivor); err != nil {
			t.Fatalf("prune removed %s: %v", survivor, err)
		}
	}
	ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
	if calls != 5 {
		t.Fatalf("prune invalidated a live entry: calls=%d", calls)
	}
}

func TestUsageCachePartialScanDeletesNothing(t *testing.T) {
	for _, cause := range []string{"interrupted", "linked directory", "unreadable directory", "filter error", "unreadable root"} {
		t.Run(cause, func(t *testing.T) {
			profile, dir := t.TempDir(), t.TempDir()
			spec := (claudeAgent{}).Usage()
			mustWriteSessionHistory(t, filepath.Join(profile, "projects", "p", "kept.jsonl"), usageCacheFixture("kept", 10))
			path := filepath.Join(profile, "projects", "p", "gone.jsonl")
			mustWriteSessionHistory(t, path, usageCacheFixture("gone", 20))
			ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
			gone := usageCacheEntryPath(t, dir, profile, "gone.jsonl")
			if _, err := os.Stat(gone); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch cause {
			case "interrupted":
				mustWriteSessionHistory(t, filepath.Join(profile, "projects", "p", "new.jsonl"), usageCacheFixture("new", 30))
				parse := spec.ParseHistory
				spec.ParseHistory = func(r io.Reader) (UsageHistory, error) {
					cancel()
					return parse(r)
				}
			case "linked directory":
				if err := os.Symlink(t.TempDir(), filepath.Join(profile, "projects", "linked")); err != nil {
					t.Fatal(err)
				}
			case "unreadable directory":
				if os.Geteuid() == 0 {
					t.Skip("root reads a directory regardless of its mode")
				}
				locked := filepath.Join(profile, "projects", "p2")
				mustWriteSessionHistory(t, filepath.Join(locked, "s.jsonl"), usageCacheFixture("locked", 30))
				if err := os.Chmod(locked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			case "filter error":
				spec.HistoryFilter = func(*os.Root, string) (bool, error) {
					return false, errors.New("parent session unreadable")
				}
			case "unreadable root":
				moved := profile + ".moved"
				if err := os.Rename(profile, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, profile); err != nil {
					t.Fatal(err)
				}
			}
			cache := NewUsageCache(dir)
			if h := ReadUsageHistory(ctx, profile, spec, cache); !h.Partial {
				t.Fatalf("partial scan looked complete: %+v", h)
			}
			cache.Prune()
			if _, err := os.Stat(gone); err != nil {
				t.Fatalf("partial scan deleted an entry: %v", err)
			}
		})
	}
}

// A cache folder the walk could not use keeps its entries even when Prune can open it again,
// as after a transient failure; the walk itself then reads history uncached and complete.
func TestUsageCacheUnusableFolderKeepsEntries(t *testing.T) {
	profile, dir := t.TempDir(), t.TempDir()
	spec := (claudeAgent{}).Usage()
	path := filepath.Join(profile, "projects", "p", "gone.jsonl")
	mustWriteSessionHistory(t, path, usageCacheFixture("gone", 20))
	ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
	gone := usageCacheEntryPath(t, dir, profile, "gone.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Dir(gone)
	if err := os.Chmod(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := NewUsageCache(dir)
	if h := ReadUsageHistory(context.Background(), profile, spec, cache); h.Partial {
		t.Fatalf("an unusable cache made history partial: %+v", h)
	}
	if err := os.Chmod(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	cache.Prune()
	if _, err := os.Stat(gone); err != nil {
		t.Fatalf("an unusable cache folder lost its entries: %v", err)
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
	if h := ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir)); !h.Partial {
		t.Fatal("read error looked complete")
	}
	if h := ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir)); h.Partial || calls != 2 {
		t.Fatalf("disk error cached indefinitely: partial=%v calls=%d", h.Partial, calls)
	}
	ReadUsageHistory(context.Background(), profile, spec, NewUsageCache(dir))
	if calls != 2 {
		t.Fatalf("recovered read not cached: %d", calls)
	}
}
