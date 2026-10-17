// Command releasenotes is the release workflow's preflight. It fails closed before publication and
// prints the release notes only after every local check succeeds.
//
//	go run ./tools/releasenotes notes <tag>
//	go run ./tools/releasenotes verify-remote <tag>
//
// notes checks that the tag names HEAD and that CHANGELOG.md's first release section is the tag's,
// then prints the tag's section of .github/release-notes.md: the short summary the release commit
// carries, which is the whole release body. verify-remote checks that origin's tag is still the
// one checked out, just before publication.
//
// Both files use the release changelog format, not general Markdown. Release sections use ATX H2
// headings, with closed comments and fenced code blocks. Raw HTML blocks are unsupported; put HTML
// examples in fenced code.
package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	semverTag = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)` +
		`(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?` +
		`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	objectID        = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	heading         = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*)|[ \t]*)$`)
	closingSequence = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
	fence           = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	container       = regexp.MustCompile(`^(?:>[ \t]*|[-+*](?:[ \t]+|$)|[0-9]+[.)](?:[ \t]+|$))`)
	taskBox         = regexp.MustCompile(`^\[[ xX]\](?:[ \t]+|$)`)
	separator       = regexp.MustCompile(`^ {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	underline       = regexp.MustCompile(`^ {0,3}(?:-+|=+)[ \t]*$`)
	setextH2        = regexp.MustCompile(`^ {0,3}-+[ \t]*$`)
	linkDefinition  = regexp.MustCompile(`^\[[^\]]+\]:`)
	// Raw HTML starts with a tag, a processing instruction, a declaration or a comment. RE2 has no
	// lookahead, so a tag name consumes the character that ends it; the same lines match.
	htmlStart   = regexp.MustCompile(`^ {0,3}<(?:/?[A-Za-z][A-Za-z0-9-]*(?:[ \t/>]|$)|[?!])`)
	htmlComment = regexp.MustCompile(`^ {0,3}<!--`)
)

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "notes" && os.Args[1] != "verify-remote") {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/releasenotes notes|verify-remote <tag>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "release preflight: %v; refusing to publish\n", err)
		os.Exit(1)
	}
}

func run(command, tag string) error {
	identity, err := localTag(tag)
	if err != nil {
		return err
	}
	if command == "verify-remote" {
		return verifyRemote(identity)
	}
	if _, err := releaseFile("CHANGELOG.md", tag[1:]); err != nil {
		return err
	}
	notes, err := releaseFile(summaryFile, tag[1:])
	if err != nil {
		return err
	}
	_, err = os.Stdout.WriteString(notes)
	return err
}

// summaryFile is the release body: the release skill writes it in the finalize commit, so the
// published page never needs editing by hand afterwards.
const summaryFile = ".github/release-notes.md"

// releaseFile returns the release section for version from the file at path.
func releaseFile(path, version string) (string, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(text) {
		return "", errors.New(path + " is not valid UTF-8")
	}
	notes, err := releaseNotes(string(text), version, path == summaryFile)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return notes, nil
}

// tagIdentity is what the event tag resolved to at checkout: its object, which is the commit itself
// for a lightweight tag, and the commit that object peels to.
type tagIdentity struct{ ref, object, commit string }

func localTag(tag string) (tagIdentity, error) {
	match := semverTag.FindStringSubmatch(tag)
	if match == nil || leadingZero(match[1]) {
		return tagIdentity{}, errors.New("tag must be strict vMAJOR.MINOR.PATCH SemVer")
	}
	ref := "refs/tags/" + tag
	head, err := git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return tagIdentity{}, err
	}
	commit, err := git("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return tagIdentity{}, err
	}
	object, err := git("rev-parse", "--verify", ref)
	if err != nil {
		return tagIdentity{}, err
	}
	if !objectID.MatchString(head) || !objectID.MatchString(object) || head != commit {
		return tagIdentity{}, errors.New("event tag does not resolve to the checked-out commit")
	}
	return tagIdentity{ref, object, commit}, nil
}

// leadingZero reports whether a prerelease has a numeric identifier with a leading zero, which
// SemVer forbids.
func leadingZero(prerelease string) bool {
	for _, part := range strings.Split(prerelease, ".") {
		if len(part) > 1 && part[0] == '0' && strings.Trim(part, "0123456789") == "" {
			return true
		}
	}
	return false
}

func git(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	// A remote helper can hold git's stderr open after the timeout kills git; stop waiting for it.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("timed out after %v", time.Minute)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func verifyRemote(tag tagIdentity) error {
	// Compare the tag object too: replacing an annotation at the same commit is still tag
	// mutation. Never fetch here and silently accept the replacement.
	expected := map[string]string{tag.ref: tag.object}
	if tag.object != tag.commit {
		expected[tag.ref+"^{}"] = tag.commit
	}
	out, err := git("ls-remote", "--exit-code", "origin", tag.ref, tag.ref+"^{}")
	if err != nil {
		return err
	}
	actual := map[string]string{}
	for line := range strings.Lines(out) {
		fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(fields) != 2 || !objectID.MatchString(fields[0]) || actual[fields[1]] != "" {
			return errors.New("remote tag lookup returned malformed or duplicate refs")
		}
		actual[fields[1]] = fields[0]
	}
	if !maps.Equal(actual, expected) {
		return errors.New("remote event tag is missing or changed since checkout")
	}
	return nil
}

// releaseNotes returns the first release section of text, which must be headed ## version, with
// the comments outside code removed and the blank lines around it trimmed. A changelog's next H2 is
// the previous release; the summary is one section, so only refuses another H2 there, where it
// would cut the published notes short.
func releaseNotes(text, version string, only bool) (string, error) {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	var notes []string
	var selected, substantive, comment, paragraph bool
	fenceRun := ""             // the open code fence's opening run, empty outside a fence
	codeLine, codeEnd := -1, 0 // where an open code span closes; codeLine is -1 outside one

	withoutComments := func(line string, index int) string {
		var out strings.Builder
		for pos := 0; pos < len(line); {
			switch {
			case comment:
				end := strings.Index(line[pos:], "-->")
				if end < 0 {
					return out.String()
				}
				comment, pos = false, pos+end+3
			case codeLine >= 0:
				if index < codeLine {
					out.WriteString(line[pos:])
					return out.String()
				}
				out.WriteString(line[pos:codeEnd])
				pos, codeLine = codeEnd, -1
			case strings.HasPrefix(line[pos:], "<!--"):
				comment, pos = true, pos+4
			case line[pos] == '\\' && pos+1 < len(line):
				out.WriteString(line[pos : pos+2])
				pos += 2
			case line[pos] == '`':
				run := line[pos:]
				run = run[:len(run)-len(strings.TrimLeft(run, "`"))]
				// Code spans may cross paragraph lines, but never swallow the next release
				// heading, code block or blank-line boundary.
				codeLine, codeEnd = codeSpanEnd(lines, index, pos+len(run), run)
				out.WriteString(run)
				pos += len(run)
			default:
				out.WriteByte(line[pos])
				pos++
			}
		}
		return out.String()
	}

	for index, raw := range lines {
		// Code fences own their literal contents, including comment-looking bytes.
		if fenceRun != "" {
			closer := closesFence(raw, fenceRun)
			if selected {
				notes = append(notes, raw)
				substantive = substantive || (!closer && trimSpace(raw) != "")
			}
			if closer {
				fenceRun = ""
			}
			paragraph = false
			continue
		}

		if !comment && htmlStart.MatchString(raw) && !htmlComment.MatchString(raw) {
			return "", errors.New("raw HTML blocks are unsupported; use fenced HTML examples")
		}
		var opener []string
		rawHeading := false
		if !comment {
			opener, rawHeading = fence.FindStringSubmatch(raw), heading.MatchString(raw)
		}
		if opener != nil {
			run, info := opener[1], opener[2]
			if run[0] == '`' && strings.Contains(info, "`") {
				return "", errors.New("backtick fence info may not contain backticks")
			}
			fenceRun = run
			if selected {
				notes = append(notes, raw)
			}
			paragraph = false
			continue
		}
		line := withoutComments(raw, index)

		if paragraph && setextH2.MatchString(line) {
			return "", errors.New("setext H2 is unsupported; use ## version release headings")
		}
		// Removing a comment must never manufacture a structural heading.
		var match []string
		if rawHeading {
			match = heading.FindStringSubmatch(line)
		}
		if match != nil && len(match[1]) == 2 {
			if selected {
				if only {
					return "", errors.New("holds a second H2; use ### for headings inside the release section")
				}
				break
			}
			title := strings.Trim(closingSequence.ReplaceAllString(match[2], ""), " \t")
			if title != version {
				return "", errors.New("first H2 must be finalized as ## " + version)
			}
			selected, paragraph = true, false
			continue
		}
		if selected {
			notes = append(notes, line)
			substantive = substantive || substantiveLine(line)
		}
		paragraph = strings.Trim(line, " \t") != "" && !blockBoundary(raw)
	}

	if comment || fenceRun != "" {
		return "", errors.New("has an unclosed comment or code fence")
	}
	if !selected || !substantive {
		return "", errors.New("needs a finalized first release section with substantive notes")
	}
	for len(notes) > 0 && trimSpace(notes[0]) == "" {
		notes = notes[1:]
	}
	for len(notes) > 0 && trimSpace(notes[len(notes)-1]) == "" {
		notes = notes[:len(notes)-1]
	}
	return strings.Join(notes, "\n") + "\n", nil
}

// codeSpanEnd finds the run of exactly as many backticks as run that closes a code span opened on
// lines[index], searching from byte start. It returns the line and the byte the closing run ends
// at, or -1 when no such run comes before the next block boundary.
func codeSpanEnd(lines []string, index, start int, run string) (int, int) {
	for number := index; number < len(lines); number++ {
		line := lines[number]
		if number != index && blockBoundary(line) {
			break
		}
		from := 0
		if number == index {
			from = start
		}
		for p := from; p+len(run) <= len(line); p++ {
			end := p + len(run)
			if line[p:end] == run && (p == 0 || line[p-1] != '`') && (end == len(line) || line[end] != '`') {
				return number, end
			}
		}
	}
	return -1, 0
}

// closesFence reports whether a line closes the fence opened by run: up to three spaces, at least
// as many of the same character, then only spaces and tabs.
func closesFence(line, run string) bool {
	rest := line
	for i := 0; i < 3 && strings.HasPrefix(rest, " "); i++ {
		rest = rest[1:]
	}
	after := strings.TrimLeft(rest, run[:1])
	return len(rest)-len(after) >= len(run) && strings.Trim(after, " \t") == ""
}

// substantiveLine reports whether a notes line says anything once its quote, list and task markers
// are gone: a heading, thematic break or link reference definition alone does not.
func substantiveLine(line string) bool {
	content := trimSpace(line)
	for {
		marker := container.FindStringIndex(content)
		if marker == nil {
			break
		}
		content = strings.TrimLeft(content[marker[1]:], " \t")
	}
	content = taskBox.ReplaceAllString(content, "")
	return content != "" && !heading.MatchString(content) && !separator.MatchString(content) &&
		!linkDefinition.MatchString(content)
}

// blockBoundary reports whether a line is blank or starts a block, which a code span never crosses.
func blockBoundary(line string) bool {
	return strings.Trim(line, " \t") == "" || heading.MatchString(line) || fence.MatchString(line) ||
		separator.MatchString(line) || container.MatchString(strings.TrimLeft(line, " \t")) ||
		underline.MatchString(line) || htmlStart.MatchString(line)
}

// trimSpace trims Unicode white space and also the ASCII separators U+001C to U+001F, which the
// release format has always counted as space (it was first written against Python's str.strip).
func trimSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || '\x1c' <= r && r <= '\x1f'
	})
}
