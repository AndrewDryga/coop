package box

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

// projectBuildInputs binds an explicit host build to its staged context, runtime, locked client
// set and build settings. A launch consumes that immutable result; it never repeats instructions
// that could fetch different external inputs. Version 1 digests belonged to automatic builds.
func projectBuildInputs(tree string, candidate networkstate.CandidateSpec, closure agents.ClientClosure, base, tag, dfRel string) string {
	sum := sha256.New()
	for _, field := range []string{"project-build-v2", candidate.Runtime.DaemonID, candidate.ClientImage, closure.Digest, base, tag, dfRel,
		os.Getenv("DOCKER_DEFAULT_PLATFORM"), os.Getenv("DOCKER_BUILDKIT"), os.Getenv("BUILDX_BUILDER"), os.Getenv("SOURCE_DATE_EPOCH"), tree} {
		sum.Write([]byte(field + "\x00"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Docker follows linked control files outside its context. Require regular staged files covered
// by the digest before executing anything, not merely before remembering the resulting image.
func validateProjectBuildFiles(dir string, entries []contextEntry, dfRel string) error {
	modes := make(map[string]fs.FileMode, len(entries))
	for _, entry := range entries {
		modes[entry.rel] = entry.mode
	}
	dockerfile := filepath.FromSlash(dfRel)
	for _, rel := range []string{dockerfile, dockerfile + ".dockerignore", ".dockerignore"} {
		mode, covered := modes[rel]
		if covered && !mode.IsRegular() || !covered && rel == dockerfile {
			return fmt.Errorf("%s must be a regular file inside the build context", rel)
		}
		info, err := os.Lstat(filepath.Join(dir, rel))
		if errors.Is(err, fs.ErrNotExist) && rel != dockerfile {
			continue
		}
		if err != nil || !covered || !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular staged build file", rel)
		}
	}
	return nil
}

// readImageID reads the image id a build wrote with --iidfile.
func readImageID(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 128))
	id := strings.TrimSpace(string(data))
	digest, ok := strings.CutPrefix(id, "sha256:")
	if err != nil || !ok || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", errors.New("the build did not report the image it made")
	}
	return id, nil
}
