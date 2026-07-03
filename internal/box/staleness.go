package box

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// The shared base gets a second stamp: which coop version built it and a hash of the box
// definition (BaseDockerfile) that binary would generate. A later run compares the stamped
// definition against its own — a newer binary whose entry script/package list/node pin
// changed over an old image is exactly the kubectl-style skew worth one warning line. The
// stamp file's mtime doubles as the build time, so image age needs no runtime-specific
// `image inspect` flags either.

// imageAgeNudge is how old a box image gets before launches nudge a refresh: a round month
// is several agent-CLI releases behind (they churn weekly), without nagging fresh setups.
const imageAgeNudge = 30 * 24 * time.Hour

// baseDefHash hashes the box definition THIS binary would build the shared base from.
func baseDefHash() string {
	sum := sha256.Sum256([]byte(BaseDockerfile()))
	return hex.EncodeToString(sum[:])
}

func imageMetaPath(cfg *config.Config, img string) string {
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(img)
	return filepath.Join(cfg.BoxHome, "image-meta", safe)
}

// StampImageMeta records which coop version built the shared base image and the hash of
// the definition it built from. Called on a successful base build; best-effort.
func StampImageMeta(cfg *config.Config, img, version string) {
	p := imageMetaPath(cfg, img)
	if os.MkdirAll(filepath.Dir(p), 0o755) == nil {
		_ = os.WriteFile(p, []byte("coop "+version+"\ndef "+baseDefHash()+"\n"), 0o644)
	}
}

// BaseImageSkew reports whether the base image was built from a different box definition
// than this binary carries (e.g. `coop update --self-only` without the rebuild), and by
// which coop version. Best-effort: no stamp (image built by an older coop, or elsewhere)
// reads as no skew — never nag on a guess.
func BaseImageSkew(cfg *config.Config, img string) (builtBy string, skewed bool) {
	data, err := os.ReadFile(imageMetaPath(cfg, img))
	if err != nil {
		return "", false
	}
	var def string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "coop":
			builtBy = f[1]
		case "def":
			def = f[1]
		}
	}
	if def == "" {
		return "", false // corrupt/foreign stamp — a guess, so stay quiet
	}
	return builtBy, def != baseDefHash()
}

// ImageBuildAge returns when img was last built by this coop install, from the mtime of
// whichever stamp the build wrote (base meta, or a per-project inputs hash). ok is false
// when no stamp exists — the image wasn't built here, so its age is a guess.
func ImageBuildAge(cfg *config.Config, img string) (time.Time, bool) {
	for _, p := range []string{imageMetaPath(cfg, img), inputsHashPath(cfg, img)} {
		if fi, err := os.Stat(p); err == nil {
			return fi.ModTime(), true
		}
	}
	return time.Time{}, false
}

// StalenessNudges collects the launch-time staleness warnings for repo's image: per-project
// input drift, base binary/image skew, and plain old age. Each is one line, best-effort, and
// never blocks a run; the caller decides where to print them (box.Run for interactive runs,
// the loop's startup for batch iterations).
func StalenessNudges(cfg *config.Config, repo, img string) []string {
	var out []string
	if StaleImageInputs(cfg, repo, img) {
		out = append(out, "box image is stale — Dockerfile.agent/.tool-versions changed since it was built; run 'coop build'")
	}
	if builtBy, skewed := BaseImageSkew(cfg, img); skewed {
		out = append(out, fmt.Sprintf("box image was built by coop %s and this coop expects a different box — run 'coop build' to realign them", builtBy))
	}
	if at, ok := ImageBuildAge(cfg, img); ok {
		if age := time.Since(at); age >= imageAgeNudge {
			out = append(out, fmt.Sprintf("box image is %d days old — 'coop update' refreshes the agent CLIs baked into it", int(age.Hours()/24)))
		}
	}
	return out
}
