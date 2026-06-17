package box

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
)

// A per-project image (built from a repo's Dockerfile.agent) bakes that repo's toolchain
// at build time, so it can drift from the files that define it. We record a hash of those
// inputs when `coop build` builds the image, and compare on a later run to nudge a rebuild.
// (The shared base has no per-repo inputs — `coop update` keeps it fresh — so it's exempt.)
//
// The hash lives in a coop-config side file rather than a docker image label: it needs no
// runtime-specific build/inspect flags (works the same on docker/podman/Apple container)
// and is pure to test. Worst case if it drifts (image deleted, built elsewhere) is a
// missed or spurious *warning* — never a blocked run.

// inputsHash hashes the files that define a repo's per-project image. ok is false when the
// repo has no Dockerfile.agent (it runs on the shared base, which has no per-repo inputs).
func inputsHash(repo string) (hash string, ok bool) {
	if !fileExists(filepath.Join(repo, "Dockerfile.agent")) {
		return "", false
	}
	h := sha256.New()
	for _, name := range []string{"Dockerfile.agent", ".tool-versions"} {
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			continue // a missing .tool-versions is fine; its absence is part of the hash via the name
		}
		h.Write([]byte(name + "\x00"))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func inputsHashPath(cfg *config.Config, img string) string {
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(img)
	return filepath.Join(cfg.BoxHome, "image-inputs", safe)
}

// StampImageInputs records the inputs hash for a freshly built per-project image, so a
// later run can detect drift. A no-op for the shared base (no per-repo inputs).
func StampImageInputs(cfg *config.Config, repo, img string) {
	hash, ok := inputsHash(repo)
	if !ok {
		return
	}
	p := inputsHashPath(cfg, img)
	if os.MkdirAll(filepath.Dir(p), 0o755) == nil {
		_ = os.WriteFile(p, []byte(hash), 0o644)
	}
}

// StaleImageInputs reports whether repo's per-project image was built from a different
// Dockerfile.agent/.tool-versions than are on disk now. Best-effort: no Dockerfile.agent,
// or no recorded stamp (never built by this coop), returns false — never nag on a guess.
func StaleImageInputs(cfg *config.Config, repo, img string) bool {
	hash, ok := inputsHash(repo)
	if !ok {
		return false
	}
	stored, err := os.ReadFile(inputsHashPath(cfg, img))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(stored)) != hash
}
