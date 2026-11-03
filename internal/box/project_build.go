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

// projectBuildInputs binds a project build to its staged context, runtime, locked client set and
// build settings. A launch consumes that immutable result while all of them still match. Version 1
// digests belonged to automatic builds.
func projectBuildInputs(tree string, candidate networkstate.CandidateSpec, closure agents.ClientClosure, base, tag, dfRel string) string {
	return projectBuildDigest(append(append([]string{"project-build-v2", candidate.Runtime.DaemonID, candidate.ClientImage, closure.Digest, base, tag, dfRel},
		projectBuildEnvironment()...), tree))
}

// projectBuildReview names what a person approves with an explicit host build: the staged files, which
// Dockerfile, the daemon and the build settings. It leaves out only what Coop changes by itself: the
// client image, its closure and the tags derived from them. A match proves a rebuild runs nothing a
// person has not approved, even on a client image that did not exist then. An agent can reach none of
// this without changing the digest, so it cannot obtain a build's ordinary networking this way.
func projectBuildReview(tree, daemonID, dfRel string) string {
	id := boxAgentIdentity() // the build args a Dockerfile can branch on
	return projectBuildDigest(append(append([]string{"project-build-review-v1", daemonID, dfRel, fmt.Sprint(id.uid), fmt.Sprint(id.gid)}, projectBuildEnvironment()...), tree))
}

func projectBuildEnvironment() []string {
	return []string{os.Getenv("DOCKER_DEFAULT_PLATFORM"), os.Getenv("DOCKER_BUILDKIT"), os.Getenv("BUILDX_BUILDER"), os.Getenv("SOURCE_DATE_EPOCH")}
}

func projectBuildDigest(fields []string) string {
	sum := sha256.New()
	for _, field := range fields {
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
