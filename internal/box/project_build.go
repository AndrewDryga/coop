package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
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

// refuseCopiedProjectFile stops a build whose Dockerfile asks for project.File, which no build context
// holds (buildContextSelection). Docker alone would only say that a file the person can see was not
// found, or, when a pattern also matches another file, build an image without it and say nothing.
// This only explains: the file stays out of the image whatever this reads, so a form it misses still
// cannot bake it in. A plain search would refuse working builds, since a comment, a RUN, a COPY's
// destination and another stage's file can all name it without needing it.
func refuseCopiedProjectFile(dockerfile []byte, dfRel string) error {
	var instruction strings.Builder
	for _, line := range strings.Split(string(dockerfile), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue // Docker drops both, even inside a continued instruction
		}
		text, continued := strings.CutSuffix(line, `\`)
		instruction.WriteString(text + " ")
		if continued {
			continue
		}
		if copiesProjectFile(instruction.String()) {
			return fmt.Errorf("%s copies %s, and Coop leaves that file out of every project build so that changing a network rule needs no new image. Copy another file instead", dfRel, project.File)
		}
		instruction.Reset()
	}
	return nil
}

// copiesProjectFile reads one instruction: a COPY or ADD from the build context with a source that
// names project.File or is a pattern matching it. A folder holding the file does not count: `COPY . .`
// asks for whatever the context has, and gets it.
func copiesProjectFile(instruction string) bool {
	words := strings.Fields(instruction)
	if len(words) == 0 || !strings.EqualFold(words[0], "COPY") && !strings.EqualFold(words[0], "ADD") {
		return false
	}
	args := words[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		if strings.HasPrefix(args[0], "--from=") {
			return false // another stage's file, which the context has no say in
		}
		args = args[1:]
	}
	var sources []string
	if json.Unmarshal([]byte(strings.Join(args, " ")), &sources) != nil {
		sources = args // as Docker reads it: plain words when the rest is not a JSON list
	}
	for i := 0; i < len(sources)-1; i++ { // the last one is the destination
		if matched, _ := path.Match(path.Clean("/" + sources[i])[1:], project.File); matched {
			return true
		}
	}
	return false
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
