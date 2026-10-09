package box

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestNativeAccessSnapshotDoesNotRecoverOrCreateSourceState(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	seedCanonicalFixture(t, cfg, "claude", "work")
	dir := filepath.Join(cfg.ConfigDir, "claude", "credentials", "work")
	lock := filepath.Join(dir, ".authority.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if snapshot, found, err := SnapshotNativeAccess(t.Context(), cfg, "claude", "work"); err != nil || !found || !snapshot.Ready {
		t.Fatal("access snapshot failed", err)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatal("snapshot created source lock", err)
	}
	pending := filepath.Join(dir, "renewal.json")
	raw := []byte(`{"version":1,"id":"pending","provider":"claude","account":"work","epoch":1,"revision":1}`)
	if err := os.WriteFile(pending, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := SnapshotNativeAccess(t.Context(), cfg, "claude", "work"); err == nil || !found {
		t.Fatal("pending renewal was usable")
	}
	if data, err := os.ReadFile(pending); err != nil || !bytes.Equal(data, raw) {
		t.Fatal("snapshot recovered source custody", err)
	}
}

func TestNativeAccessSnapshotRefreshOnlyNeedsHostRenewal(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	files := canonicalFixtureFiles(t, "claude", "work")
	files[".credentials.json"] = []byte(`{"claudeAiOauth":{"refreshToken":"REFRESH_CANARY","expiresAt":1,"scopes":["user:inference"]}}`)
	importCanonicalFixture(t, cfg, "claude", "work", files)
	snapshot, found, err := SnapshotNativeAccess(t.Context(), cfg, "claude", "work")
	if err != nil || !found || !snapshot.Configured || snapshot.Ready || len(snapshot.Files) != 0 {
		t.Fatalf("refresh-only snapshot = %+v, %v, %v", snapshot, found, err)
	}
}

func TestNativeAccessSnapshotCanonicalDenials(t *testing.T) {
	for _, mode := range []string{"missing", "readable", "symlink", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			seedCanonicalFixture(t, cfg, "claude", "work")
			path := filepath.Join(cfg.ConfigDir, "claude", "credentials", "work", "authority.json")
			var err error
			switch mode {
			case "missing":
				err = os.Remove(path)
			case "readable":
				err = os.Chmod(path, 0644)
			case "symlink":
				if err = os.Rename(path, path+".real"); err == nil {
					err = os.Symlink(path+".real", path)
				}
			case "revoked":
				err = removeNativeAuthority(t.Context(), cfg, "claude", "work")
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshot, found, err := SnapshotNativeAccess(t.Context(), cfg, "claude", "work")
			if !found || snapshot.Configured || snapshot.Ready || len(snapshot.Files) != 0 {
				t.Fatal("unusable canonical authority exported access or permitted legacy fallback")
			}
			if (err == nil) != (mode == "revoked") {
				t.Fatalf("unexpected %s result: %v", mode, err)
			}
		})
	}
}
