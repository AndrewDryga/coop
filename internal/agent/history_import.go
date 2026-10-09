package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/safefile"
)

// NativeHistoryFile binds the ownership decision to the exact bytes the importer may copy.
type NativeHistoryFile struct {
	Path, CWD, SessionID string
	SHA256               string
	Size                 int64
}

type NativeHistoryPlan struct {
	Files        []NativeHistoryFile
	Dependencies map[string][]NativeHistoryFile
	// Nil selects the whole file; a nonnil empty span list selects no rows.
	Filtered map[string][]NativeHistorySpan
	// Indexes are append-merged across legacy sources. Empty keys deduplicate
	// exact rows; a named key identifies a native session-index entry.
	Indexes map[string]string
	Skipped []string
}

type NativeHistorySpan struct {
	Offset, Size int64
	Key          string
}

var errNativeHistoryForeign = errors.New("history belongs to another repository")

type nativeHistorySource struct {
	root      *os.File
	ownsCWD   func(string) bool
	plan      NativeHistoryPlan
	witnesses map[string]NativeHistoryFile
	added     map[string]bool
	proofs    map[string][]string
}

func openNativeHistory(source string, ownsCWD func(string) bool) (*nativeHistorySource, error) {
	if ownsCWD == nil {
		return nil, errors.New("native history needs repository ownership")
	}
	s := &nativeHistorySource{ownsCWD: ownsCWD, witnesses: map[string]NativeHistoryFile{}, added: map[string]bool{}, proofs: map[string][]string{},
		plan: NativeHistoryPlan{Filtered: map[string][]NativeHistorySpan{}, Indexes: map[string]string{}, Dependencies: map[string][]NativeHistoryFile{}}}
	root, err := safefile.OpenRoot(source)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.root = root
	return s, nil
}

func (s *nativeHistorySource) close() {
	if s.root != nil {
		_ = s.root.Close()
	}
}

func (s *nativeHistorySource) owns(cwd string) bool {
	return filepath.IsAbs(cwd) && filepath.Clean(cwd) == cwd && s.ownsCWD(cwd)
}

func (s *nativeHistorySource) skip(path string, err error) {
	s.plan.Skipped = append(s.plan.Skipped, path+": "+err.Error())
}

// Metadata and the digest come from the same strict descriptor. Transcript bytes
// stream through the hash; metadata bounds do not cap cumulative transcript size.
func (s *nativeHistorySource) inspect(path string, inspect func(io.Reader) error) bool {
	if s.root == nil {
		return false
	}
	file, err := safefile.OpenRegular(s.root, path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		s.skip(path, err)
		return false
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		s.skip(path, err)
		return false
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		s.skip(path, errors.New("history file has ambiguous link ownership"))
		return false
	}
	hash := sha256.New()
	reader := io.TeeReader(file, hash)
	if inspect != nil {
		err = inspect(reader)
	}
	if errors.Is(err, errNativeHistoryForeign) {
		return false
	}
	if err == nil {
		_, err = io.Copy(io.Discard, reader)
	}
	if err != nil {
		s.skip(path, err)
		return false
	}
	after, err := file.Stat()
	if err != nil {
		s.skip(path, err)
		return false
	}
	named, err := safefile.OpenRegular(s.root, path)
	if err != nil {
		s.skip(path, err)
		return false
	}
	current, statErr := named.Stat()
	_ = named.Close()
	if statErr != nil || !os.SameFile(before, after) || !os.SameFile(before, current) ||
		before.Size() != after.Size() || before.Size() != current.Size() ||
		!before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(current.ModTime()) {
		s.skip(path, errors.New("history changed while planning; retry after its writer exits"))
		return false
	}
	witness := NativeHistoryFile{Path: path, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: before.Size()}
	if prior, exists := s.witnesses[path]; exists && prior != witness {
		s.skip(path, errors.New("history changed between ownership and companion reads"))
		return false
	}
	s.witnesses[path] = witness
	return true
}

func (s *nativeHistorySource) add(path, cwd, id string) {
	if s.added[path] {
		return
	}
	witness, ok := s.witnesses[path]
	if !ok {
		if !s.inspect(path, nil) {
			return
		}
		witness = s.witnesses[path]
	}
	witness.CWD, witness.SessionID = cwd, id
	s.plan.Files = append(s.plan.Files, witness)
	s.added[path] = true
}

func (s *nativeHistorySource) small(path string, limit int64) ([]byte, bool) {
	var data []byte
	ok := s.inspect(path, func(reader io.Reader) error {
		var err error
		data, err = io.ReadAll(io.LimitReader(reader, limit+1))
		if err == nil && int64(len(data)) > limit {
			err = errors.New("native ownership metadata exceeds its bound")
		}
		return err
	})
	return data, ok
}

func (s *nativeHistorySource) walk(path string, visit func(string)) { s.walkAt(path, 0, visit) }

func (s *nativeHistorySource) walkAt(path string, depth int, visit func(string)) {
	if s.root == nil {
		return
	}
	if depth > 16 {
		s.skip(path, errors.New("native history nesting exceeds its bound"))
		return
	}
	dir, err := safefile.OpenDir(s.root, path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.skip(path, err)
		return
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			child := filepath.Join(path, entry.Name())
			mode, err := safefile.LstatMode(dir, entry.Name())
			switch {
			case err != nil:
				s.skip(child, err)
			case mode.IsDir():
				s.walkAt(child, depth+1, visit)
			case mode.IsRegular():
				visit(child)
			default:
				s.skip(child, errors.New("native history is not a regular file or directory"))
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				s.skip(path, readErr)
			}
			return
		}
	}
}

func (s *nativeHistorySource) filter(path string, keys []string, indexKey string, keep func(map[string]string) bool) {
	spans := []NativeHistorySpan{}
	var proofs []string
	if s.inspect(path, func(reader io.Reader) error {
		return nativeHistoryKeyedRows(reader, keys, indexKey, func(fields map[string]string, offset, size int64, key string) error {
			if keep(fields) {
				spans = append(spans, NativeHistorySpan{Offset: offset, Size: size, Key: key})
				for _, value := range fields {
					proofs = append(proofs, s.proofs[value]...)
				}
			}
			return nil
		})
	}) {
		s.plan.Filtered[path] = spans
		s.plan.Indexes[path] = indexKey
		s.add(path, "", "")
		s.depend(path, proofs...)
	}
}

func nativeHistorySessionName(path string) string {
	name := filepath.Base(path)
	if len(name) < 36 || !ValidSessionID(name[:36]) {
		return ""
	}
	if len(name) == 36 || name[36] == '.' || name[36] == '-' {
		return name[:36]
	}
	return ""
}

func (s *nativeHistorySource) companions(roots []string, sessions map[string]string) {
	for _, root := range roots {
		s.walk(root, func(path string) {
			rel, _ := filepath.Rel(root, path)
			first := strings.SplitN(rel, string(filepath.Separator), 2)[0]
			id := nativeHistorySessionName(first)
			if cwd := sessions[id]; cwd != "" {
				s.add(path, cwd, id)
				s.depend(path, s.proofs[id]...)
			}
		})
	}
}

func (s *nativeHistorySource) depend(path string, proofs ...string) {
	seen := map[string]bool{}
	for _, prior := range s.plan.Dependencies[path] {
		seen[prior.Path] = true
	}
	for _, proof := range proofs {
		if proof == path || seen[proof] {
			continue
		}
		witness, ok := s.witnesses[proof]
		if !ok {
			s.skip(path, errors.New("native ownership witness unavailable"))
			s.plan.Files = slices.DeleteFunc(s.plan.Files, func(file NativeHistoryFile) bool { return file.Path == path })
			return
		}
		s.plan.Dependencies[path] = append(s.plan.Dependencies[path], witness)
		seen[proof] = true
	}
	slices.SortFunc(s.plan.Dependencies[path], func(a, b NativeHistoryFile) int { return strings.Compare(a.Path, b.Path) })
}

func (s *nativeHistorySource) finish() NativeHistoryPlan {
	slices.SortFunc(s.plan.Files, func(a, b NativeHistoryFile) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(s.plan.Skipped)
	return s.plan
}
