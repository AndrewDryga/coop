package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

type usageCache struct {
	dir  string
	root *os.Root
}

func openUsageCache(dir, profile, kind string) *usageCache {
	if dir == "" || kind == "" {
		return nil
	}
	profile, err := filepath.Abs(profile)
	if err != nil || config.EnsurePrivateDir(dir) != nil {
		return nil
	}
	base, err := openSessionRoot(dir)
	if err != nil {
		return nil
	}
	defer base.Close()
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(kind+"\x00"+profile)))
	if err := base.Mkdir(key, 0o700); err != nil && !os.IsExist(err) {
		return nil
	}
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
	return &usageCache{dir: filepath.Join(dir, key), root: root}
}

func usageCacheFile(path string) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(path)))
}

func (cache *usageCache) read(path string, source os.FileInfo) (UsageHistory, bool) {
	if cache == nil {
		return UsageHistory{}, false
	}
	name := usageCacheFile(path)
	info, err := cache.root.Lstat(name)
	if err != nil || info.Mode().Perm() != 0o600 {
		return UsageHistory{}, false
	}
	data, err := readRootUsageArtifact(cache.root, name, 64<<20)
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

func (cache *usageCache) write(path string, source os.FileInfo, history UsageHistory) {
	if cache == nil {
		return
	}
	data, err := json.Marshal(usageCacheEntry{Version: usageCacheVersion, Source: usageFingerprint(source), History: history})
	if err == nil && len(data) <= 64<<20 {
		// Cache persistence is optional: failure must not discard usable native usage.
		_ = config.WriteFileAtomic(filepath.Join(cache.dir, usageCacheFile(path)), data)
	}
}
