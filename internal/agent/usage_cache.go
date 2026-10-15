package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/AndrewDryga/coop/internal/config"
)

// Increment this when normalized history decoding changes. Tariffs need no invalidation.
const usageCacheVersion = 1

type usageFileFingerprint struct {
	Device, Inode           uint64
	Size, Modified, Changed int64
}

func usageFingerprint(info os.FileInfo) usageFileFingerprint {
	stat := info.Sys().(*syscall.Stat_t)
	return usageFileFingerprint{Device: uint64(stat.Dev), Inode: uint64(stat.Ino),
		Size: info.Size(), Modified: info.ModTime().UnixNano(), Changed: usageChangeTime(stat)}
}

type usageCacheEntry struct {
	Version int
	Source  usageFileFingerprint
	History UsageHistory
}

// UsageCache is one usage inspection's view of the private cache directory: one subdirectory per
// provider history root, one entry per transcript. It records what the inspection used, so Prune
// can drop the entries of transcripts and roots that are gone.
type UsageCache struct {
	dir        string
	mu         sync.Mutex
	used       map[string]map[string]bool // root key -> entries read or written
	incomplete map[string]bool            // roots whose walk may have missed a transcript
}

// NewUsageCache returns nil, which reads history uncached, for an empty dir.
func NewUsageCache(dir string) *UsageCache {
	if dir == "" {
		return nil
	}
	return &UsageCache{dir: dir, used: map[string]map[string]bool{}, incomplete: map[string]bool{}}
}

// usageCacheRoot is one walk's handle on the entries of one history root.
type usageCacheRoot struct {
	cache *UsageCache
	key   string
	dir   string
	root  *os.Root
	used  map[string]bool
}

func usageCacheKey(profile, kind string) (string, bool) {
	profile, err := filepath.Abs(profile)
	if err != nil || kind == "" {
		return "", false
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(kind+"\x00"+profile))), true
}

func (cache *UsageCache) open(profile, kind string) *usageCacheRoot {
	if cache == nil {
		return nil
	}
	key, ok := usageCacheKey(profile, kind)
	if !ok {
		return nil
	}
	root := cache.openRoot(key)
	if root == nil {
		cache.record(key, nil, false) // Whatever an unusable root holds stays.
		return nil
	}
	return &usageCacheRoot{cache: cache, key: key, dir: filepath.Join(cache.dir, key), root: root, used: map[string]bool{}}
}

func (cache *UsageCache) openRoot(key string) *os.Root {
	if config.EnsurePrivateDir(cache.dir) != nil {
		return nil
	}
	base, err := openSessionRoot(cache.dir)
	if err != nil {
		return nil
	}
	defer base.Close()
	if err := base.Mkdir(key, 0o700); err != nil && !os.IsExist(err) {
		return nil
	}
	return openUsageCacheDir(base, key)
}

// openUsageCacheDir opens a root's entries only while the directory is the private one Coop made.
func openUsageCacheDir(base *os.Root, key string) *os.Root {
	info, err := base.Lstat(key)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil
	}
	root, err := base.OpenRoot(key)
	if err != nil {
		return nil
	}
	current, err := root.Stat(".")
	if err != nil || !os.SameFile(info, current) {
		root.Close()
		return nil
	}
	return root
}

func usageCacheFile(path string) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(path)))
}

func (entries *usageCacheRoot) read(path string, source os.FileInfo) (UsageHistory, bool) {
	if entries == nil {
		return UsageHistory{}, false
	}
	name := usageCacheFile(path)
	entries.used[name] = true
	info, err := entries.root.Lstat(name)
	if err != nil || info.Mode().Perm() != 0o600 {
		return UsageHistory{}, false
	}
	data, err := readRootUsageArtifact(entries.root, name, 64<<20)
	if err != nil {
		return UsageHistory{}, false
	}
	var entry usageCacheEntry
	if json.Unmarshal(data, &entry) != nil || entry.Version != usageCacheVersion || entry.Source != usageFingerprint(source) {
		return UsageHistory{}, false
	}
	for _, event := range entry.History.Events {
		if !event.valid() {
			return UsageHistory{}, false
		}
	}
	return entry.History, true
}

func (entries *usageCacheRoot) write(path string, source os.FileInfo, history UsageHistory) {
	if entries == nil {
		return
	}
	name := usageCacheFile(path)
	entries.used[name] = true
	data, err := json.Marshal(usageCacheEntry{Version: usageCacheVersion, Source: usageFingerprint(source), History: history})
	if err == nil && len(data) <= 64<<20 {
		// Cache persistence is optional: failure must not discard usable native usage.
		_ = config.WriteFileAtomic(filepath.Join(entries.dir, name), data)
	}
}

// finish hands the walk's entries to the inspection and closes the root.
func (entries *usageCacheRoot) finish(complete bool) {
	if entries == nil {
		return
	}
	entries.root.Close()
	entries.cache.record(entries.key, entries.used, complete)
}

// skip keeps every entry of a history root the inspection could not open.
func (cache *UsageCache) skip(profile, kind string) {
	if key, ok := usageCacheKey(profile, kind); ok && cache != nil {
		cache.record(key, nil, false)
	}
}

func (cache *UsageCache) record(key string, used map[string]bool, complete bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.used[key] == nil {
		cache.used[key] = map[string]bool{}
	}
	maps.Copy(cache.used[key], used)
	if !complete {
		cache.incomplete[key] = true
	}
}

// Prune deletes the entries this inspection did not use: those of transcripts that are gone, and
// every entry of a root it did not read, whose emptied directory goes too. Call it only after an
// uninterrupted inspection that read every provider's roots; to a narrower one, live entries look
// unused. A root whose walk may have missed a transcript keeps every entry. Only private regular
// files named like Coop's entries are removed, through os.Root, so links and other files stay.
func (cache *UsageCache) Prune() {
	if cache == nil {
		return
	}
	base, err := openSessionRoot(cache.dir)
	if err != nil {
		return
	}
	defer base.Close()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, key := range usageCacheNames(base, "") {
		if cache.incomplete[key] {
			continue
		}
		root := openUsageCacheDir(base, key)
		if root == nil {
			continue
		}
		used, read := cache.used[key]
		for _, name := range usageCacheNames(root, ".json") {
			if used[name] {
				continue
			}
			if info, err := root.Lstat(name); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 {
				_ = root.Remove(name)
			}
		}
		root.Close()
		if !read {
			_ = base.Remove(key) // Fails, keeping the directory, while anything else is inside.
		}
	}
}

// usageCacheNames lists the names Coop gives roots (no suffix) and entries (".json"): a lowercase
// SHA-256 digest and the suffix. A read error only shortens the list.
func usageCacheNames(root *os.Root, suffix string) []string {
	dir, err := root.Open(".")
	if err != nil {
		return nil
	}
	defer dir.Close()
	names, _ := dir.Readdirnames(-1)
	return slices.DeleteFunc(names, func(name string) bool {
		digest, ok := strings.CutSuffix(name, suffix)
		return !ok || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != ""
	})
}
