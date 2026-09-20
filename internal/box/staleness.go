package box

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
)

// A per-project image (built from a repo's .agent/Dockerfile) bakes that repo's toolchain
// at build time, so it can drift from the files that define it. We record a hash of those
// inputs when `coop build` builds the image, and compare on a later run to nudge a rebuild.
// (The shared base has no per-repo inputs — `coop update` keeps it fresh — so it's exempt.)
//
// The hash lives in a coop-config side file rather than a docker image label: it needs no
// runtime-specific build/inspect flags (works the same on docker and Apple container)
// and is pure to test. Worst case if it drifts (image deleted, built elsewhere) is a
// missed or spurious *warning* — never a blocked run.

// inputsHash hashes the files that define a repo's per-project image. ok is false when the
// repo has no box Dockerfile (it runs on the shared base, which has no per-repo inputs).
func inputsHash(repo string) (hash string, ok bool) {
	dfRel := project.DockerfilePath(repo)
	if !fileExists(filepath.Join(repo, dfRel)) {
		return "", false
	}
	h := sha256.New()
	for _, name := range []string{dfRel, ".tool-versions"} {
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

// StaleImageInputs reports whether repo's per-project image was built from a different box
// Dockerfile/.tool-versions than are on disk now. Best-effort: no box Dockerfile, or no recorded
// stamp (never built by this coop), returns false — never nag on a guess.
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
// definition (baseImageDefinition) that binary would generate. A later run compares the stamped
// definition against its own — a newer binary whose entry script/package list/node pin
// changed over an old image is exactly the kubectl-style skew worth one warning line. The
// stamp file's mtime doubles as the build time, so image age needs no runtime-specific
// `image inspect` flags either.

// ImageAgeNudge is how old a box image gets before launches nudge a refresh: a round month keeps the
// OS packages and Node underneath the clients current, without nagging fresh setups. The clients
// themselves move only with Coop, which the skew nudge reports.
const ImageAgeNudge = 30 * 24 * time.Hour

// baseDefHash hashes the box definition THIS binary would build the shared base from — for both
// platforms it builds, so a launch never asks the runtime which one this host's image is. Its
// inputs are embedded, so it is computed once; "" when they are inconsistent, which never stamps
// or reports a skew on a guess.
var baseDefHash = sync.OnceValue(func() string {
	sum := sha256.New()
	for _, arch := range []string{"amd64", "arm64"} {
		files, err := baseImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: arch, Libc: "glibc"})
		if err != nil {
			return ""
		}
		for _, name := range slices.Sorted(maps.Keys(files)) {
			fmt.Fprintf(sum, "%s\x00%d\x00", name, len(files[name]))
			sum.Write(files[name])
		}
	}
	return hex.EncodeToString(sum.Sum(nil))
})

func imageMetaPath(cfg *config.Config, img string) string {
	return filepath.Join(cfg.BoxHome, "image-meta", safeImageFileName(img))
}

// safeImageFileName is one image reference as a file name, for the records Coop keeps per image.
func safeImageFileName(img string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(img)
}

// StampImageMeta records which coop version built the shared base image and the hash of
// the definition it built from. Called on a successful base build; best-effort.
func StampImageMeta(cfg *config.Config, img, version string) {
	p := imageMetaPath(cfg, img)
	if os.MkdirAll(filepath.Dir(p), 0o755) == nil {
		_ = os.WriteFile(p, []byte("coop "+version+"\ndef "+baseDefHash()+"\n"), 0o644)
	}
}

// readImageMeta parses one StampImageMeta file: the Coop version that built the image and the
// definition it built from, each "" when absent.
func readImageMeta(path string) (builtBy, def string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
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
	return builtBy, def, nil
}

// EarlierManagedBase reports whether a Coop on this host built a managed base before — an untagged
// coop-box from before the tag named its definition, or one for another definition — and which
// Coop built the latest of them. A missing base for this definition on such a host is an upgrade
// to repair, not a first build to leave to the operator.
func EarlierManagedBase(cfg *config.Config) (builtBy string, ok bool) {
	dir := filepath.Dir(imageMetaPath(cfg, ManagedBaseRepository))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var latest time.Time
	for _, entry := range entries {
		name := entry.Name()
		if name != ManagedBaseRepository && !IsManagedBase(strings.Replace(name, "_", ":", 1)) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || ok && !info.ModTime().After(latest) {
			continue
		}
		if version, _, err := readImageMeta(filepath.Join(dir, name)); err == nil {
			builtBy, latest, ok = version, info.ModTime(), true
		}
	}
	return builtBy, ok
}

// BaseImageSkew reports whether the base image was built from a different box definition
// than this binary carries (e.g. `coop update --self-only` without the rebuild), and by
// which coop version. Best-effort: no stamp (image built by an older coop, or elsewhere)
// reads as no skew — never nag on a guess. A managed base names its definition in its tag, so
// only an operator's COOP_BASE_IMAGE can drift this way.
func BaseImageSkew(cfg *config.Config, img string) (builtBy string, skewed bool) {
	if IsManagedBase(img) {
		return "", false
	}
	builtBy, def, err := readImageMeta(imageMetaPath(cfg, img))
	if err != nil {
		return "", false
	}
	current := baseDefHash()
	if def == "" || current == "" {
		return "", false // corrupt/foreign stamp, or no definition to compare — a guess, so stay quiet
	}
	return builtBy, def != current
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
		out = append(out, "box image is stale — the box Dockerfile/.tool-versions changed since it was built; run 'coop build'")
	}
	if builtBy, skewed := BaseImageSkew(cfg, img); skewed {
		out = append(out, fmt.Sprintf("box image was built by coop %s and this coop expects a different box — run 'coop build' to realign them", builtBy))
	}
	if at, ok := ImageBuildAge(cfg, img); ok {
		if age := time.Since(at); age >= ImageAgeNudge {
			out = append(out, fmt.Sprintf("box image is %d days old — 'coop update' rebuilds it on the newest OS packages and Node", int(age.Hours()/24)))
		}
	}
	return out
}
