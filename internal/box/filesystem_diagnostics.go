package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

const (
	filesystemBuildLogWindow  = 1 << 20
	filesystemBuildLines      = 20
	filesystemBuildFirstError = 5
	filesystemBuildLineBytes  = 512
)

var (
	buildKitFailedStep = regexp.MustCompile(`^#(\d+) ERROR\b`)
	buildKitStepOutput = regexp.MustCompile(`^#(\d+) \d+\.\d+ (.*)$`)
	buildOutputError   = regexp.MustCompile(`(?i)(^|: )(fatal )?error\b`)
)

// Capture useful evidence before failed-admission cleanup removes its private
// log. The original error remains discoverable with errors.Is/As.
func filesystemBuildError(cause error, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("read filesystem build diagnostic: %w", err))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return errors.Join(cause, err)
	}
	data := make([]byte, min(info.Size(), filesystemBuildLogWindow))
	n, err := file.ReadAt(data, max(0, info.Size()-filesystemBuildLogWindow))
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.Join(cause, err)
	}
	text := string(data[:n])
	if _, whole, cut := strings.Cut(text, "\n"); cut && info.Size() > filesystemBuildLogWindow {
		text = whole
	}
	detail := strings.Join(filesystemBuildDetail(text), "\n")
	if detail == "" {
		return cause
	}
	return fmt.Errorf("%w\nFilesystem helper build output:\n%s", cause, detail)
}

// filesystemBuildDetail picks what a person needs from a plain BuildKit log. The end of the log is
// no guide: its footer repeats the failed command and excerpts the Dockerfile, and the ten step
// lines it repeats can already have lost the first error. So this takes the failed step's own
// output, the compiler's or package manager's words, and adds the log's last line for the exit
// status. A failure with no step output, such as a base image that cannot be pulled, or a log in
// any other shape reads from the end.
func filesystemBuildDetail(log string) []string {
	var lines []string
	failed := ""
	for line := range strings.Lines(log) {
		if line = cleanLaunchText(line); line == "" {
			continue
		}
		lines = append(lines, line)
		if match := buildKitFailedStep.FindStringSubmatch(line); match != nil && failed == "" {
			// The first failure cancels the other steps, so a later ERROR is not the cause.
			failed = match[1]
		}
	}
	var output []string
	for _, line := range lines {
		if match := buildKitStepOutput.FindStringSubmatch(line); match != nil && match[1] == failed {
			output = append(output, match[2])
		}
	}
	selected := lines[max(0, len(lines)-filesystemBuildLines):]
	if len(output) > 0 {
		selected = append(filesystemBuildCause(output), lines[len(lines)-1])
	}
	for i, line := range selected {
		if len(line) > filesystemBuildLineBytes {
			// Both ends matter: a compiler names the file first, a command's exit status comes last.
			selected[i] = strings.ToValidUTF8(line[:filesystemBuildLineBytes/2]+" ... "+line[len(line)-filesystemBuildLineBytes/2:], "")
		}
	}
	return selected
}

// filesystemBuildCause bounds a failed step's output. Its end says how the step stopped, and its
// first error is the one the rest may only follow from.
func filesystemBuildCause(output []string) []string {
	if len(output) <= filesystemBuildLines {
		return output
	}
	tail := output[len(output)-filesystemBuildLines:]
	first := slices.IndexFunc(output, buildOutputError.MatchString)
	if first < 0 || first >= len(output)-filesystemBuildLines {
		return tail
	}
	omitted := fmt.Sprintf("[... %d lines omitted ...]", len(output)-filesystemBuildLines)
	return slices.Concat(output[first:first+filesystemBuildFirstError], []string{omitted}, tail[filesystemBuildFirstError:])
}
