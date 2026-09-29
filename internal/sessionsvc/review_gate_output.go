package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/session"
)

// A review keeps everything its gate printed, stdout and stderr as they came,
// beside the review for the controller that asked for it. The controller reads
// it page by page; no response holds more than one page. A red result that
// said only "gate failed" left the fixing agent guessing which check failed.
//
// The log has a size limit only so that a gate printing without end cannot
// fill the worker's disk; a log cut at the limit says so on every read.
var (
	maxReviewGateOutputBytes  int64 = 64 << 20
	reviewGateOutputPageBytes       = 1 << 20
)

// ReviewGateOutput is what the review says about its gate's output.
type ReviewGateOutput struct {
	Command    []string `json:"command,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Bytes      int64    `json:"bytes"`
	Complete   bool     `json:"complete"`
	Incomplete string   `json:"incomplete,omitempty"`
	Lost       string   `json:"lost,omitempty"`
}

// ReviewGateOutputPage is one page of a review gate's kept output. NextCursor is
// empty on the last page. Lost says why nothing was kept, and then Output is
// empty; Incomplete says why the kept output is not all the gate printed.
type ReviewGateOutputPage struct {
	Output     string
	NextCursor string
	Bytes      int64
	Complete   bool
	Incomplete string
	Lost       string
}

func (s *Service) reviewGateOutputPath(operationID string) (string, error) {
	if s.stateRoot == "" || !validSessionPathComponent(operationID) {
		return "", errors.New("invalid review output identity")
	}
	return filepath.Join(s.stateRoot, "review-artifacts", operationID+".gate.log"), nil
}

// reviewGateLog is the gate's stdout and stderr. A write never fails the gate:
// the gate's own result stands whatever happens to the copy of its output.
type reviewGateLog struct {
	mu      sync.Mutex
	file    *os.File
	written int64
	cut     bool
	err     error
}

func (l *reviewGateLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil || l.err != nil || l.cut {
		return len(p), nil
	}
	chunk := p
	if room := maxReviewGateOutputBytes - l.written; int64(len(chunk)) > room {
		chunk, l.cut = chunk[:room], true
	}
	n, err := l.file.Write(chunk)
	l.written += int64(n)
	if err != nil {
		l.err = err
	}
	return len(p), nil
}

// openReviewGateOutput starts a fresh log for the operation. A review re-run
// after a restart starts it again, as it runs its gate again.
func (s *Service) openReviewGateOutput(operationID string) (*reviewGateLog, error) {
	path, err := s.reviewGateOutputPath(operationID)
	if err != nil {
		return &reviewGateLog{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return &reviewGateLog{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return &reviewGateLog{}, err
	}
	return &reviewGateLog{file: file}, nil
}

// finish closes the log and says what it holds. The log is synced before the
// review completes, so a completed review never names output it lost.
func (l *reviewGateLog) finish(result ReviewGateResult, openErr error) *ReviewGateOutput {
	l.mu.Lock()
	defer l.mu.Unlock()
	output := &ReviewGateOutput{Command: append([]string(nil), result.Command...), Bytes: l.written}
	if result.ExitCode != nil {
		code := *result.ExitCode
		output.ExitCode = &code
	}
	if l.file != nil {
		l.err = errors.Join(l.err, l.file.Sync(), l.file.Close())
		l.file = nil
	}
	switch {
	case openErr != nil:
		output.Bytes = 0
		output.Lost = "Coop could not keep the check's output: " + SanitizeReviewText(openErr.Error(), MaxReviewErrorBytes)
	case l.err != nil && l.written == 0:
		output.Lost = "Coop could not keep the check's output: " + SanitizeReviewText(l.err.Error(), MaxReviewErrorBytes)
	case l.err != nil:
		output.Incomplete = fmt.Sprintf("Coop kept the first %d bytes of the check's output, then could not write more: %s",
			l.written, SanitizeReviewText(l.err.Error(), MaxReviewErrorBytes))
	case l.cut:
		output.Incomplete = fmt.Sprintf("The check printed more than %s; Coop kept the first %s.",
			reviewByteSize(maxReviewGateOutputBytes), reviewByteSize(maxReviewGateOutputBytes))
	default:
		output.Complete = true
	}
	return output
}

func reviewByteSize(bytes int64) string {
	switch {
	case bytes >= 1<<20 && bytes%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", bytes>>20)
	case bytes >= 1<<10 && bytes%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", bytes>>10)
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

// ReadReviewGateOutput reads one page of a completed review's gate output, from
// cursor ("" for the first page). Only the review's own session reads it.
func (s *Service) ReadReviewGateOutput(ctx context.Context, sessionID, operationID, cursor string) (ReviewGateOutputPage, error) {
	_, dossier, err := s.GetReview(ctx, sessionID, operationID)
	if err != nil {
		return ReviewGateOutputPage{}, err
	}
	output := dossier.GateOutput
	switch {
	case output == nil && dossier.Gate == ReviewGateNone:
		return ReviewGateOutputPage{Lost: "This review ran no check: the repository has none configured."}, nil
	case output == nil && dossier.Gate == ReviewGateNotRun:
		return ReviewGateOutputPage{Lost: "This review ran no check."}, nil
	case output == nil:
		return ReviewGateOutputPage{Lost: "Coop kept no output for this review: it ran before Coop kept check output."}, nil
	case output.Lost != "":
		return ReviewGateOutputPage{Lost: output.Lost}, nil
	}
	offset := int64(0)
	if cursor != "" {
		offset, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || offset < 0 || offset > output.Bytes || strconv.FormatInt(offset, 10) != cursor {
			return ReviewGateOutputPage{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "invalid review output cursor"}
		}
	}
	path, err := s.reviewGateOutputPath(operationID)
	if err != nil {
		return ReviewGateOutputPage{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ReviewGateOutputPage{Lost: "The check's output is no longer kept: its session was discarded or cleaned up."}, nil
	}
	if err != nil {
		return ReviewGateOutputPage{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ReviewGateOutputPage{}, err
	}
	if !info.Mode().IsRegular() || info.Size() != output.Bytes {
		return ReviewGateOutputPage{Lost: "The check's kept output changed after its review, so Coop no longer serves it."}, nil
	}
	page := make([]byte, min(int64(reviewGateOutputPageBytes), output.Bytes-offset))
	if _, err := file.ReadAt(page, offset); err != nil && !errors.Is(err, io.EOF) {
		return ReviewGateOutputPage{}, err
	}
	end := offset + int64(len(page))
	if end < output.Bytes {
		page = page[:reviewGateOutputRuneCut(page)]
		end = offset + int64(len(page))
	}
	result := ReviewGateOutputPage{
		Output: string(page), Bytes: output.Bytes, Complete: output.Complete, Incomplete: output.Incomplete,
	}
	if end < output.Bytes {
		result.NextCursor = strconv.FormatInt(end, 10)
	}
	return result, nil
}

// reviewGateOutputRuneCut ends a page before a character the page would split.
// A page is never cut to nothing: bytes that are not a character pass as they are.
func reviewGateOutputRuneCut(page []byte) int {
	for back := 1; back < utf8.UTFMax && back <= len(page); back++ {
		start := len(page) - back
		if !utf8.RuneStart(page[start]) {
			continue
		}
		if !utf8.FullRune(page[start:]) && start > 0 {
			return start
		}
		break
	}
	return len(page)
}
