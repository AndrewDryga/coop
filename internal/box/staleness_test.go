package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestBaseImageSkew(t *testing.T) {
	cfg := &config.Config{BoxHome: t.TempDir()}
	const img = "coop-box"

	// No stamp (image built by an older coop, or elsewhere) → never a skew guess.
	if by, skewed := BaseImageSkew(cfg, img); skewed {
		t.Fatalf("unstamped image must not report skew (builtBy=%q)", by)
	}

	// Stamped by THIS binary → same definition, no skew, builder version readable.
	StampImageMeta(cfg, img, "v2.9.0")
	by, skewed := BaseImageSkew(cfg, img)
	if skewed {
		t.Fatal("an image stamped by this binary must not be skewed")
	}
	if by != "v2.9.0" {
		t.Fatalf("builtBy = %q, want v2.9.0", by)
	}

	// A stamp from a binary with a different box definition → skew, naming the builder.
	writeRepoFile(t, imageMetaPath(cfg, img), "coop v2.0.0\ndef 0123abc\n")
	if by, skewed := BaseImageSkew(cfg, img); !skewed || by != "v2.0.0" {
		t.Fatalf("changed definition: want skew by v2.0.0, got skewed=%v by=%q", skewed, by)
	}

	// A corrupt stamp (no def line) is a guess → quiet.
	writeRepoFile(t, imageMetaPath(cfg, img), "garbage\n")
	if _, skewed := BaseImageSkew(cfg, img); skewed {
		t.Fatal("a corrupt stamp must not report skew")
	}
}

func TestImageBuildAgeAndNudges(t *testing.T) {
	repo := t.TempDir()
	cfg := &config.Config{BoxHome: t.TempDir()}
	const img = "coop-box"

	// No stamp at all → age unknown, and a bare repo yields no nudges.
	if _, ok := ImageBuildAge(cfg, img); ok {
		t.Fatal("no stamp: age must be unknown")
	}
	if n := StalenessNudges(cfg, repo, img); len(n) != 0 {
		t.Fatalf("nothing stamped, nothing stale: want no nudges, got %v", n)
	}

	// A fresh base stamp → age known, still no nudges (new image, same definition).
	StampImageMeta(cfg, img, "v3.0.0")
	if _, ok := ImageBuildAge(cfg, img); !ok {
		t.Fatal("stamped: age must be known")
	}
	if n := StalenessNudges(cfg, repo, img); len(n) != 0 {
		t.Fatalf("fresh stamp: want no nudges, got %v", n)
	}

	// Backdate the stamp past the threshold → exactly the age nudge.
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(imageMetaPath(cfg, img), old, old); err != nil {
		t.Fatal(err)
	}
	n := StalenessNudges(cfg, repo, img)
	if len(n) != 1 || !strings.Contains(n[0], "days old") {
		t.Fatalf("40-day-old image: want the age nudge, got %v", n)
	}

	// A definition change on top → the skew nudge joins it, naming the builder.
	writeRepoFile(t, imageMetaPath(cfg, img), "coop v2.0.0\ndef stale\n")
	if err := os.Chtimes(imageMetaPath(cfg, img), old, old); err != nil {
		t.Fatal(err)
	}
	n = StalenessNudges(cfg, repo, img)
	if len(n) != 2 || !strings.Contains(n[0], "built by coop v2.0.0") {
		t.Fatalf("skewed + old: want skew then age, got %v", n)
	}

	// Per-project images date from their inputs stamp (no meta), so age works there too.
	cfg2 := &config.Config{BoxHome: t.TempDir()}
	writeRepoFile(t, filepath.Join(repo, "Dockerfile.agent"), "FROM x\n")
	StampImageInputs(cfg2, repo, "proj-img")
	if _, ok := ImageBuildAge(cfg2, "proj-img"); !ok {
		t.Fatal("per-project inputs stamp must date the image")
	}
}
