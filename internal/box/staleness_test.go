package box

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func writeRepoFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInputsHash(t *testing.T) {
	repo := t.TempDir()
	if _, ok := inputsHash(repo); ok {
		t.Fatal("no Dockerfile.agent: want ok=false (shared base)")
	}

	writeRepoFile(t, filepath.Join(repo, "Dockerfile.agent"), "FROM x\n")
	h1, ok := inputsHash(repo)
	if !ok || h1 == "" {
		t.Fatalf("with Dockerfile.agent: want ok=true and a hash, got ok=%v hash=%q", ok, h1)
	}
	if h2, _ := inputsHash(repo); h2 != h1 {
		t.Fatalf("hash not stable: %s != %s", h2, h1)
	}

	writeRepoFile(t, filepath.Join(repo, "Dockerfile.agent"), "FROM y\n")
	h3, _ := inputsHash(repo)
	if h3 == h1 {
		t.Fatal("a Dockerfile.agent change must change the hash")
	}

	writeRepoFile(t, filepath.Join(repo, ".tool-versions"), "golang 1.26.4\n")
	h4, _ := inputsHash(repo)
	if h4 == h3 {
		t.Fatal("adding .tool-versions must change the hash")
	}
	writeRepoFile(t, filepath.Join(repo, ".tool-versions"), "golang 1.27.0\n")
	if h5, _ := inputsHash(repo); h5 == h4 {
		t.Fatal("a .tool-versions change must change the hash")
	}
}

func TestStaleImageInputs(t *testing.T) {
	repo := t.TempDir()
	cfg := &config.Config{BoxHome: t.TempDir()}
	const img = "coop-test"

	if StaleImageInputs(cfg, repo, img) {
		t.Fatal("shared base (no Dockerfile.agent) must never be reported stale")
	}

	writeRepoFile(t, filepath.Join(repo, "Dockerfile.agent"), "FROM x\n")
	if StaleImageInputs(cfg, repo, img) {
		t.Fatal("an unstamped image must not be reported stale (no guessing)")
	}

	StampImageInputs(cfg, repo, img)
	if StaleImageInputs(cfg, repo, img) {
		t.Fatal("a freshly stamped image must not be stale")
	}

	writeRepoFile(t, filepath.Join(repo, ".tool-versions"), "golang 1.26.4\n")
	if !StaleImageInputs(cfg, repo, img) {
		t.Fatal("changed inputs must be reported stale")
	}

	StampImageInputs(cfg, repo, img)
	if StaleImageInputs(cfg, repo, img) {
		t.Fatal("re-stamping must clear staleness")
	}
}
