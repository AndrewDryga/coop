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
	"regexp"
	"slices"
	"strings"
	"unicode"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
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
// destination, another stage's file and a heredoc's body can all name it without needing it.
func refuseCopiedProjectFile(dockerfile []byte, dfRel string) error {
	copies := false
	dockerfileInstructions(dockerfile, func(instruction string) bool {
		copies = copiesProjectFile(instruction)
		return !copies
	})
	if copies {
		return fmt.Errorf("%s copies %s, and Coop leaves that file out of every project build so that changing a network rule needs no new image. Copy another file instead", dfRel, project.File)
	}
	return nil
}

// dockerfileInstructions calls each with every instruction of dockerfile, its continued lines joined,
// until each returns false. Comments, blank lines and heredoc bodies are not instructions.
func dockerfileInstructions(dockerfile []byte, each func(instruction string) bool) {
	var instruction strings.Builder
	var bodies []heredoc
	for _, raw := range strings.Split(string(dockerfile), "\n") {
		if len(bodies) > 0 { // its lines are text, and no comment or continuation applies to them
			if bodies[0].endsAt(raw) {
				bodies = bodies[1:]
			}
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue // Docker drops both, even inside a continued instruction
		}
		text, continued := strings.CutSuffix(line, `\`)
		instruction.WriteString(text + " ")
		if continued {
			continue
		}
		if !each(instruction.String()) {
			return
		}
		bodies = heredocsOf(instruction.String())
		instruction.Reset()
	}
}

// copiesProjectFile reads one instruction: a COPY or ADD from the build context with a source that
// names project.File or is a pattern matching it. A folder holding the file does not count: `COPY . .`
// asks for whatever the context has, and gets it.
func copiesProjectFile(instruction string) bool {
	for _, source := range contextCopySources(instruction) {
		if matched, _ := path.Match(path.Clean("/" + source)[1:], project.File); matched {
			return true
		}
	}
	return false
}

// contextCopySources is what one COPY or ADD instruction reads from the build context: its sources,
// as written. Another stage's files and any other instruction read none.
func contextCopySources(instruction string) []string {
	words := strings.Fields(instruction)
	if len(words) == 0 || !strings.EqualFold(words[0], "COPY") && !strings.EqualFold(words[0], "ADD") {
		return nil
	}
	args := words[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		if strings.HasPrefix(args[0], "--from=") {
			return nil // another stage's file, which the context has no say in
		}
		args = args[1:]
	}
	var sources []string
	if json.Unmarshal([]byte(strings.Join(args, " ")), &sources) != nil {
		sources = args // as Docker reads it: plain words when the rest is not a JSON list
	}
	if len(sources) < 2 {
		return nil
	}
	return sources[:len(sources)-1] // the last one is the destination
}

// broadContextFiles is how many staged files a Dockerfile can leave uncopied before a build says so.
const broadContextFiles = 100

// uncopiedContextFiles counts the staged files that no COPY or ADD of the Dockerfile names, as a path,
// a folder holding it or a pattern. Each of them still voids the approval when it changes, because the
// approval covers the whole staged context. A source Coop cannot read, such as one a build argument
// spells, counts as copying everything, so the count errs low, never high.
func uncopiedContextFiles(dockerfile []byte, entries []contextEntry, dfRel string) int {
	var sources []string
	everything := false
	dockerfileInstructions(dockerfile, func(instruction string) bool {
		for _, source := range contextCopySources(instruction) {
			switch clean := path.Clean("/" + source)[1:]; {
			case strings.Contains(source, "://"): // ADD from a URL reads no context file
			case clean == "" || strings.Contains(source, "$"):
				everything = true
				return false
			default:
				sources = append(sources, clean)
			}
		}
		return true
	})
	if everything {
		return 0
	}
	uncopied := 0
	for _, entry := range entries {
		rel := filepath.ToSlash(entry.rel)
		if !entry.mode.IsRegular() || rel == dfRel || path.Base(rel) == ".dockerignore" || rel == dfRel+".dockerignore" {
			continue
		}
		copied := false
		for p := rel; p != "." && !copied; p = path.Dir(p) {
			copied = slices.ContainsFunc(sources, func(source string) bool {
				matched, _ := path.Match(source, p)
				return matched
			})
		}
		if !copied {
			uncopied++
		}
	}
	return uncopied
}

// projectBuildChange says what voided a project build approval, from the files the approved build
// covered, so a refusal sends the person to those files. The approval covers every staged file, and
// emisar's box built from its whole repository root, so any edit anywhere voided it, and a refusal that
// named nothing sent its supervisor to suspect a Coop upgrade (2026-11-05). approved is a match of the
// project's files and settings, which leaves only Coop's own inputs. "" when the reviewed files are not
// on record.
func projectBuildChange(store *networkstate.Store, project string, files map[string]string, approved bool) string {
	if approved {
		return "this project's files and settings are still the approved ones; only Coop's side changed, such as its client image"
	}
	reviewed, ok := store.ProjectBuildReviewedFiles(project)
	if !ok {
		return ""
	}
	var changed []string
	for rel, digest := range files {
		if reviewed[rel] != digest {
			changed = append(changed, rel)
		}
	}
	for rel := range reviewed {
		if _, ok := files[rel]; !ok {
			changed = append(changed, rel)
		}
	}
	if len(changed) == 0 {
		return "the files are the approved ones, so the Docker daemon, the Dockerfile in use or a build setting changed"
	}
	slices.Sort(changed)
	// A folder that came or went is named by the files in it, which sort right after it.
	var named []string
	for i, rel := range changed {
		if i+1 == len(changed) || !strings.HasPrefix(changed[i+1], rel+"/") {
			named = append(named, rel)
		}
	}
	changed = named
	shown := changed[:min(len(changed), 5)]
	names := make([]string, len(shown))
	for i, rel := range shown {
		names[i] = ui.SafeInline(rel)
	}
	text := "files that changed: " + strings.Join(names, ", ")
	if more := len(changed) - len(shown); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

// withProjectBuildChange is reason followed by what changed, when that is known.
func withProjectBuildChange(reason string, store *networkstate.Store, project string, files map[string]string, approved bool) string {
	if change := projectBuildChange(store, project, files, approved); change != "" {
		return reason + "; " + change
	}
	return reason
}

// heredoc is a body that follows an instruction, up to the line holding only its name. A `<<-` body may
// indent that line with tabs.
type heredoc struct {
	name  string
	chomp bool
}

func (h heredoc) endsAt(line string) bool {
	line = strings.TrimRight(line, "\r")
	if h.chomp {
		line = strings.TrimLeft(line, "\t")
	}
	return line == h.name
}

var heredocWord = regexp.MustCompile(`^[0-9]*<<(-?)([^<]*)$`)

// heredocsOf reads the bodies one instruction opens, the way Docker's parser does: only RUN, COPY and
// ADD can have one, directly or under ONBUILD, and the whole shell word must be the opener, so neither
// `cat<<EOF` nor a quoted "<<EOF" opens anything.
func heredocsOf(instruction string) []heredoc {
	words := shellWords(instruction)
	if len(words) > 1 && strings.EqualFold(words[0].raw, "ONBUILD") {
		words = words[1:]
	}
	if len(words) == 0 || !slices.ContainsFunc([]string{"RUN", "COPY", "ADD"}, func(name string) bool { return strings.EqualFold(words[0].raw, name) }) {
		return nil
	}
	var bodies []heredoc
	for _, word := range words[1:] {
		match := heredocWord.FindStringSubmatch(word.raw)
		if match == nil {
			continue
		}
		if name := word.text[len(word.raw)-len(match[2]):]; name != "" { // the opener itself has no quotes
			bodies = append(bodies, heredoc{name: name, chomp: match[1] == "-"})
		}
	}
	return bodies
}

// shellWord is one word of an instruction: as written, and with its quotes and escapes taken off.
type shellWord struct{ raw, text string }

func shellWords(s string) []shellWord {
	var (
		words     []shellWord
		raw, text strings.Builder
		quote     rune
		escaped   bool
	)
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
			text.WriteRune(r)
		case r == '\\' && quote != '\'':
			escaped = true
		case r == quote:
			quote = 0
		case quote != 0:
			text.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case unicode.IsSpace(r):
			if raw.Len() > 0 {
				words = append(words, shellWord{raw.String(), text.String()})
				raw.Reset()
				text.Reset()
			}
			continue
		default:
			text.WriteRune(r)
		}
		raw.WriteRune(r)
	}
	if raw.Len() > 0 {
		words = append(words, shellWord{raw.String(), text.String()})
	}
	return words
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
