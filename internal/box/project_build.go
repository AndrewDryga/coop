package box

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

// A filtered launch builds the project's box Dockerfile on the locked client image every time it
// starts. When nothing the build reads has changed, the image it made last time is the image it
// would make now, so the launch runs that exact image id instead of staging the tree and building
// again. The image is still proven like a fresh build (derived_image.go); only the staging and the
// build are skipped.

// projectBuildInputs is the digest of everything a filtered project build reads: the context it
// stages (contextDigest) and each setting that shapes the build around it — the daemon, the locked
// client image it builds on and the client set that image must keep, the base tag and output tag it
// is given, the Dockerfile's path, and the environment the docker client builds under (buildx turns
// SOURCE_DATE_EPOCH into the image's timestamps, and BUILDX_BUILDER picks the builder).
func projectBuildInputs(tree string, candidate networkstate.CandidateSpec, closure agents.ClientClosure, base, tag, dfRel string) string {
	sum := sha256.New()
	for _, field := range []string{"project-build-v1", candidate.Runtime.DaemonID, candidate.ClientImage, closure.Digest, base, tag, dfRel,
		os.Getenv("DOCKER_DEFAULT_PLATFORM"), os.Getenv("DOCKER_BUILDKIT"), os.Getenv("BUILDX_BUILDER"), os.Getenv("SOURCE_DATE_EPOCH"), tree} {
		sum.Write([]byte(field + "\x00"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// maxReusableDockerfileBytes bounds what reusableProjectBuild reads; a box Dockerfile is a page or
// two, and a larger one simply builds every launch.
const maxReusableDockerfileBytes = 1 << 20

// reusableProjectBuild reports whether a build of this staged context may be reused for the same
// inputs, which holds only when its result is a function of the context and the base alone. The
// Dockerfile and the ignore files Docker reads beside it must be regular files the digest covered —
// docker follows a link straight out of the context — and the Dockerfile must name nothing Docker
// could resolve differently another day (dockerfileReusable).
func reusableProjectBuild(dir string, entries []contextEntry, dfRel string) bool {
	modes := make(map[string]fs.FileMode, len(entries))
	for _, entry := range entries {
		modes[entry.rel] = entry.mode
	}
	dockerfile := filepath.FromSlash(dfRel)
	for _, rel := range []string{dockerfile, dockerfile + ".dockerignore", ".dockerignore"} {
		if mode, ok := modes[rel]; ok && !mode.IsRegular() || !ok && rel == dockerfile {
			return false
		}
	}
	file, err := os.OpenFile(filepath.Join(dir, dockerfile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxReusableDockerfileBytes+1))
	return err == nil && len(data) <= maxReusableDockerfileBytes && dockerfileReusable(data)
}

// dockerContinuation is BuildKit's line-continuation rule for the default escape character: a
// backslash that is not itself escaped, followed only by spaces or tabs.
var dockerContinuation = regexp.MustCompile(`([^\\])\\[ \t]*$|^\\[ \t]*$`)

// dockerfileReusable reports whether a Dockerfile's only inputs are its build context and the
// COOP_BASE_IMAGE it is given. It refuses what it does not recognise rather than parsing it: a
// parser directive (a `# syntax=` frontend is an image fetched by tag, `# escape=` changes how lines
// join), a heredoc, a FROM of anything but the base or an earlier stage, a COPY --from another
// image, any RUN flag (a mount reads state outside the context), and ADD, ONBUILD or an unknown
// instruction. A refused Dockerfile builds every launch, exactly as before.
//
// The one unsafe mistake is to read an instruction differently from Docker, so lines join as
// BuildKit joins them (dockerInstructions), and a COPY whose flags Docker would unquote, unescape or
// split at a non-ASCII byte is refused rather than read.
func dockerfileReusable(dockerfile []byte) bool {
	text := strings.TrimPrefix(string(dockerfile), "\ufeff")
	// Docker's lexer reads a stray byte of invalid UTF-8 as a space where Go does not, and Docker 20.10
	// trimmed one carriage return where later releases trim them all.
	if !utf8.ValidString(text) || strings.Contains(text, "<<") || strings.Contains(text, "\r\r") {
		return false
	}
	instructions, ok := dockerInstructions(text)
	if !ok {
		return false
	}
	stages := map[string]bool{}
	count := 0
	for _, instruction := range instructions {
		fields := strings.Fields(instruction)
		if len(fields) == 0 || strings.IndexFunc(fields[0], func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
			return false
		}
		args := fields[1:]
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			if len(args) == 0 || args[0] != "${COOP_BASE_IMAGE}" && args[0] != "$COOP_BASE_IMAGE" && !stages[strings.ToLower(args[0])] {
				return false
			}
			switch {
			case len(args) == 3 && strings.EqualFold(args[1], "AS"):
				stages[strings.ToLower(args[2])] = true
			case len(args) != 1:
				return false
			}
			count++
		case "COPY":
			if strings.IndexFunc(instruction, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
				return false // Docker's flag lexer splits words at single bytes a UTF-8 reading keeps whole
			}
			for _, arg := range args {
				if !strings.HasPrefix(arg, "--") {
					break
				}
				from, ok := strings.CutPrefix(arg, "--from=")
				if strings.ContainsAny(arg, `"'\`) || arg == "--from" || ok && !stages[strings.ToLower(from)] && !earlierStage(from, count) {
					return false
				}
			}
		case "RUN":
			if len(args) > 0 && strings.HasPrefix(args[0], "--") {
				return false
			}
		case "ARG", "ENV", "LABEL", "USER", "WORKDIR", "EXPOSE", "VOLUME", "ENTRYPOINT", "CMD", "SHELL", "STOPSIGNAL", "HEALTHCHECK", "MAINTAINER":
		default:
			return false
		}
	}
	return count > 0
}

// dockerInstructions splits a Dockerfile into instructions the way BuildKit's parser does: a line
// continues on dockerContinuation, a continuation line is appended as it is (only its trailing line
// ending removed), and comment and blank lines inside an instruction are skipped. ok is false for a
// comment before the first instruction that could be a parser directive.
func dockerInstructions(text string) (instructions []string, ok bool) {
	isComment := func(line string) bool { return strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "#") }
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		raw := strings.TrimRight(lines[i], "\r\n")
		if isComment(raw) {
			if len(instructions) == 0 && strings.Contains(raw, "=") {
				return nil, false // a parser directive, or close enough to one
			}
			continue
		}
		line, continued := continueDockerLine(strings.TrimLeftFunc(raw, unicode.IsSpace))
		if !continued && line == "" {
			continue
		}
		for continued && i+1 < len(lines) {
			i++
			next := strings.TrimRight(lines[i], "\r\n")
			if isComment(next) || strings.TrimLeftFunc(next, unicode.IsSpace) == "" {
				continue
			}
			var part string
			part, continued = continueDockerLine(next)
			line += part
		}
		instructions = append(instructions, line)
	}
	return instructions, true
}

// continueDockerLine returns line without a continuation backslash, and whether the next line
// continues it.
func continueDockerLine(line string) (string, bool) {
	if dockerContinuation.MatchString(line) {
		return dockerContinuation.ReplaceAllString(line, "$1"), true
	}
	return line, false
}

// earlierStage reports whether from is the index of a stage already declared.
func earlierStage(from string, declared int) bool {
	index, err := strconv.Atoi(from)
	return err == nil && index >= 0 && index < declared && strconv.Itoa(index) == from
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
