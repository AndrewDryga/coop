package acpctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/config"
)

// ThreadBindings is the on-disk acpproxy.BindingStore: one small JSON file per editor thread under
// dir, named by a hash of the editor id (an adapter-minted id never becomes a host path), written
// atomically at 0600 and pruned by age when the store opens. The dir sits in coop's private config
// tree, which no box mounts, so only the host ever writes a binding.
type ThreadBindings struct{ dir string }

// threadBindingMaxAge outlives every adapter's own transcript retention (Claude Code prunes its
// sessions after 30 days by default), so a binding is never dropped while its transcript could
// still load.
const threadBindingMaxAge = 90 * 24 * time.Hour

const maxThreadBindingBytes = 4096

type threadBinding struct {
	EditorID  string    `json:"editor_id"`
	Provider  string    `json:"provider"`
	AdapterID string    `json:"adapter_id"`
	Updated   time.Time `json:"updated"`
}

// ThreadBindingsDir is where a config tree keeps its thread bindings.
func ThreadBindingsDir(cfg *config.Config) string { return filepath.Join(cfg.ConfigDir, "acp-threads") }

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
	data, err := json.Marshal(threadBinding{EditorID: editorID, Provider: binding.Provider, AdapterID: binding.AdapterID, Updated: time.Now().UTC()})
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
	_ = os.Remove(s.path(editorID))
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
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
