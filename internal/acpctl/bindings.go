package acpctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
)

// ThreadBindings is the on-disk acpproxy.BindingStore: one small JSON file per editor thread under
// dir, named by a hash of the editor id (an adapter-minted id never becomes a host path), written
// atomically at 0600 and pruned by age when the store opens. The dir sits in coop's private config
// tree, which no box mounts, so only the host ever writes a binding.
type ThreadBindings struct{ dir string }

// Custody is outside the binding records and never mounted. Its permanent lock serializes
// Save, Forget and pruning.
func (s *ThreadBindings) custody() string { return s.dir + ".custody" }

func (s *ThreadBindings) lock(ctx context.Context) (*os.File, error) {
	if err := config.EnsurePrivateDir(s.custody()); err != nil {
		return nil, err
	}
	path := filepath.Join(s.custody(), "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := config.TightenReadable(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		_ = f.Close()
		return nil, errors.New("unsafe thread binding lock")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return f, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *ThreadBindings) forgetFile(name string) error {
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncThreadDirectory(s.dir)
}

func syncThreadDirectory(path string) error {
	root, err := safefile.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Sync()
}

// threadBindingMaxAge outlives every adapter's own transcript retention (Claude Code prunes its
// sessions after 30 days by default), so a binding is never dropped while its transcript could
// still load.
const threadBindingMaxAge = 90 * 24 * time.Hour

// Editor IDs are adapter-owned and can be several kilobytes. Keep the record bounded, but leave
// room for JSON escaping and timestamp formatting around the largest ID we support in practice.
const maxThreadBindingBytes = 8 << 10

type threadBinding struct {
	EditorID  string    `json:"editor_id"`
	Provider  string    `json:"provider"`
	AdapterID string    `json:"adapter_id"`
	Updated   time.Time `json:"updated"`
}

// OpenThreadBindings prepares dir and prunes bindings older than threadBindingMaxAge. A dir that
// cannot be created still yields a usable store: every write then fails quietly and every lookup
// misses, which is exactly the in-memory-only behaviour coop had before bindings existed.
func OpenThreadBindings(dir string) *ThreadBindings {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		acpproxy.Trace("thread bindings dir %s unavailable (%v) — reopened threads resolve by editor id only", dir, err)
	}
	pruneThreadBindings(dir, time.Now())
	return &ThreadBindings{dir: dir}
}

func (s *ThreadBindings) path(editorID string) string {
	sum := sha256.Sum256([]byte(editorID))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".json")
}

// Lookup reports the binding saved for editorID, or a miss for an absent, oversized, malformed or
// mismatched entry — never an error the editor sees.
func (s *ThreadBindings) Lookup(editorID string) (acpproxy.SessionBinding, bool) {
	if s == nil || editorID == "" {
		return acpproxy.SessionBinding{}, false
	}
	f, err := os.Open(s.path(editorID))
	if err != nil {
		return acpproxy.SessionBinding{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxThreadBindingBytes+1))
	if err != nil || len(data) > maxThreadBindingBytes {
		return acpproxy.SessionBinding{}, false
	}
	var b threadBinding
	if json.Unmarshal(data, &b) != nil || b.EditorID != editorID || !bindingToken(b.Provider, 64) || !bindingToken(b.AdapterID, 256) {
		return acpproxy.SessionBinding{}, false
	}
	return acpproxy.SessionBinding{Provider: b.Provider, AdapterID: b.AdapterID}, true
}

// Save records the binding; a write failure is traced, not surfaced — the live session is unaffected.
func (s *ThreadBindings) Save(editorID string, binding acpproxy.SessionBinding) {
	if s == nil || editorID == "" || !bindingToken(binding.Provider, 64) || !bindingToken(binding.AdapterID, 256) {
		return
	}
	lock, err := s.lock(context.Background())
	if err != nil {
		acpproxy.Trace("thread binding lock unavailable: %v", err)
		return
	}
	defer lock.Close()
	data, err := json.Marshal(threadBinding{EditorID: editorID, Provider: binding.Provider, AdapterID: binding.AdapterID, Updated: time.Now().UTC()})
	if err == nil && len(data) > maxThreadBindingBytes {
		acpproxy.Trace("thread binding could not be saved: serialized record exceeds %d bytes", maxThreadBindingBytes)
		return
	}
	if err == nil {
		err = config.WriteFileAtomic(s.path(editorID), data)
	}
	if err != nil {
		acpproxy.Trace("thread binding for %s could not be saved: %v", editorID, err)
	}
}

// Forget removes the binding; a thread the editor deleted must not resurrect a native session.
func (s *ThreadBindings) Forget(editorID string) {
	if s == nil || editorID == "" {
		return
	}
	lock, err := s.lock(context.Background())
	if err != nil {
		return
	}
	defer lock.Close()
	if err := s.forgetFile(filepath.Base(s.path(editorID))); err != nil {
		acpproxy.Trace("thread deletion could not be retained: %v", err)
	}
}

// bindingToken accepts the printable single-line identifiers providers and adapters mint; anything
// else came from a corrupt or foreign file and is treated as a miss.
func bindingToken(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f }) < 0
}

// pruneThreadBindings drops binding files not written since now-threadBindingMaxAge. It runs once
// per coop acp process, so the dir stays bounded by the threads a user actually reopens.
func pruneThreadBindings(dir string, now time.Time) {
	s := &ThreadBindings{dir: dir}
	lock, err := s.lock(context.Background())
	if err != nil {
		return
	}
	defer lock.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-threadBindingMaxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			_ = s.forgetFile(entry.Name())
		}
	}
}
