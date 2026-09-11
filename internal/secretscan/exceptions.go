package secretscan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// .coopsecretsignore — the reviewed false positives for ONE check, named by the exact finding
// they excuse. It is a file a person edits, not a command that mutates state: a scan prints the
// entries, the human pastes the ones they have actually reviewed and writes why. Nothing here
// generates or approves a baseline, and removing a line restores its check.
//
// The file holds no credential material: an entry is a checksum plus a sentence. That is what
// makes it safe to commit, and it is also the limit — a fingerprint is not encryption, and an
// entry is not permission to move a secret anywhere.

// ExceptionsFile is the project-root file name. One file, one project; there is no global
// registry and no per-host key, so the same file works in every checkout of the project.
const ExceptionsFile = ".coopsecretsignore"

// maxExceptionsBytes caps what the parser will read. The file is a hand-edited list of one-line
// entries; anything this large is a mistake or a denial-of-service, not a review.
const maxExceptionsBytes = 64 << 10

// maxExceptionEntries caps how many findings one file may excuse. A project with thousands of
// reviewed false positives has a detector problem, not an exception-file problem.
const maxExceptionEntries = 2000

// placeholderReason is what a scan prints in place of the sentence the human owes. Pasting the
// block without editing it is not a review, so the parser refuses it by name.
const placeholderReason = "<reason>"

// Exceptions is a parsed .coopsecretsignore: the fingerprints whose findings this check skips.
type Exceptions struct {
	reasons map[string]string
}

// Len is how many entries the file holds — including entries that match nothing today, which
// are inert and are never rewritten: an old entry is a record of a decision, not a lie.
func (e *Exceptions) Len() int {
	if e == nil {
		return 0
	}
	return len(e.reasons)
}

// Excuses reports whether this exact finding was reviewed and excused.
func (e *Exceptions) Excuses(fingerprint string) bool {
	if e == nil || fingerprint == "" {
		return false
	}
	_, ok := e.reasons[fingerprint]
	return ok
}

// ExceptionsError is a file the parser refused, with the line that has to change. Every rejection
// names its actual line, so the fix is an edit, not a hunt.
type ExceptionsError struct {
	Line    int // 0 when the problem is the file itself rather than one entry
	Problem string
}

func (e *ExceptionsError) Error() string {
	if e.Line == 0 {
		return e.Problem
	}
	return fmt.Sprintf("Line %d %s", e.Line, e.Problem)
}

// fingerprintRe is the only entry shape: the version tag this build understands followed by its
// hex digest. A glob, a bare path, or a rule name is refused — an exception names ONE reviewed
// finding, and a pattern would quietly excuse findings nobody has seen.
var fingerprintRe = regexp.MustCompile(`^` + FingerprintVersion + `:[0-9a-f]{64}$`)

// versionedRe matches anything shaped like a versioned id, so a file written for a future format
// is told its version is unsupported instead of being called malformed.
var versionedRe = regexp.MustCompile(`^(fp-v[0-9]+):`)

// LoadExceptions reads path. A missing file is the normal case and yields no exceptions and no
// error. Anything else — unreadable, not a regular file, too large, or holding an entry the
// parser will not accept — is an error: a check that silently ignored a broken exception file
// would report a result nobody asked for.
func LoadExceptions(path string) (*Exceptions, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &ExceptionsError{Problem: sentenceOf(err)}
	}
	if !info.Mode().IsRegular() {
		return nil, &ExceptionsError{Problem: "The exception file is not a regular file."}
	}
	if info.Size() > maxExceptionsBytes {
		return nil, &ExceptionsError{Problem: fmt.Sprintf("The exception file is larger than %d KB.", maxExceptionsBytes>>10)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ExceptionsError{Problem: sentenceOf(err)}
	}
	return ParseExceptions(string(data))
}

// ParseExceptions is LoadExceptions' pure half.
func ParseExceptions(content string) (*Exceptions, error) {
	out := &Exceptions{reasons: map[string]string{}}
	for i, raw := range strings.Split(content, "\n") {
		n := i + 1
		line := strings.TrimSpace(raw)
		// A blank line separates entries and a full-line comment labels them — a scan prints
		// both, so the block a person pastes is accepted exactly as printed.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, reason, ok := strings.Cut(line, "#")
		id = strings.TrimSpace(id)
		reason = strings.TrimSpace(reason)
		if version := versionedRe.FindStringSubmatch(id); version != nil && version[1] != FingerprintVersion {
			return nil, &ExceptionsError{Line: n, Problem: fmt.Sprintf("uses finding ID version %q, which this Coop does not support.", version[1])}
		}
		if !fingerprintRe.MatchString(id) {
			return nil, &ExceptionsError{Line: n, Problem: "needs a finding ID followed by # and your reason."}
		}
		if !ok || reason == "" {
			return nil, &ExceptionsError{Line: n, Problem: "needs a reason after #."}
		}
		if reason == placeholderReason {
			return nil, &ExceptionsError{Line: n, Problem: "still has the placeholder reason " + placeholderReason + "."}
		}
		// A repeat of the same decision is harmless; two different reasons for one finding mean
		// the file disagrees with itself, and coop must not pick a winner on someone's behalf.
		if prior, dup := out.reasons[id]; dup && prior != reason {
			return nil, &ExceptionsError{Line: n, Problem: "repeats a finding ID with a different reason."}
		}
		out.reasons[id] = reason
		if len(out.reasons) > maxExceptionEntries {
			return nil, &ExceptionsError{Line: n, Problem: fmt.Sprintf("is past the limit of %d entries.", maxExceptionEntries)}
		}
	}
	return out, nil
}

// sentenceOf renders an OS error as the one sentence a reason slot holds: the cause, not the
// syscall and path Go prepends (the reader already knows which file failed — it is named in the
// headline above).
func sentenceOf(err error) string {
	msg := err.Error()
	if pathErr, ok := err.(*fs.PathError); ok {
		msg = pathErr.Err.Error()
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	r := []rune(msg)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	msg = string(r)
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}
