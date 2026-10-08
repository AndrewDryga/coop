package forkctl

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type isolatedParent struct {
	Repo     string   `json:"repo"`
	Metadata []string `json:"metadata"`
	Target   string   `json:"target"`
	Head     string   `json:"head"`
	Tree     string   `json:"tree"`
	Index    string   `json:"index"`
	Content  string   `json:"content"`
}

type isolatedTreeEntry struct{ mode, object, path string }

func observed(repo string, args ...string) (string, error) {
	output, err := forkspace.ObserveGit(context.Background(), repo, args...)
	return strings.TrimSuffix(string(output), "\n"), err
}

func isolatedTree(repo, commit string) ([]isolatedTreeEntry, error) {
	data, err := forkspace.ObserveGit(context.Background(), repo, "ls-tree", "-r", "--full-tree", "-z", commit)
	if err != nil {
		return nil, err
	}
	var entries []isolatedTreeEntry
	paths := map[string]string{}
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, path, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || !validObjectID(fields[2]) || !safeIsolatedTreePath(path) {
			return nil, errors.New("isolated publication has an invalid or reserved Git tree path")
		}
		if fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" && fields[0] != "160000" {
			return nil, fmt.Errorf("isolated publication refuses tree mode %s at %q", fields[0], path)
		}
		entries = append(entries, isolatedTreeEntry{fields[0], fields[2], path})
		folded := isolatedPathKey(path)
		if old, exists := paths[folded]; exists && old != path {
			return nil, fmt.Errorf("isolated tree has case or normalization aliases %q and %q", old, path)
		}
		paths[folded] = path
	}
	for folded, path := range paths {
		parts := strings.Split(folded, "/")
		for i := 1; i < len(parts); i++ {
			if other, exists := paths[strings.Join(parts[:i], "/")]; exists {
				return nil, fmt.Errorf("isolated tree has file/ancestor aliases %q and %q", other, path)
			}
		}
	}
	return entries, nil
}

func safeIsolatedTreePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") || strings.Contains(path, ":") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		normal := isolatedPathKey(segment)
		if normal == "" || normal == ".git" || strings.HasPrefix(normal, "git~") || strings.HasPrefix(normal, ".git~") {
			return false
		}
	}
	return true
}

// Publication must not acquire automatic authority through filesystem aliases.
// Darwin folds names and ignores format characters; Windows also trims suffixes.
func isolatedPathKey(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		part = strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, part)
		parts[i] = cases.Fold().String(norm.NFC.String(strings.TrimRight(part, ". ")))
	}
	return strings.Join(parts, "/")
}

func isolatedHostSurfacePath(path string) string {
	path = isolatedPathKey(path)
	base := filepath.Base(path)
	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") {
		path = strings.TrimSuffix(path, base) + "Dockerfile" + strings.TrimPrefix(base, "dockerfile")
	}
	return path
}

// Compare logical index entries and every tracked byte, not Git's stat cache or
// hidden skip/assume flags. No persistent allocator tuple becomes replay authority.
func captureIsolatedParent(repo string) (isolatedParent, error) {
	return captureIsolatedCheckout(repo, false, 0)
}

// Review previews committed history without demanding a clean shared checkout.
// Publication still captures and rechecks the complete semantic precondition.
func captureIsolatedReviewBase(repo string) (isolatedParent, error) {
	if err := forkspace.QualifyDefaultLFSStorage(context.Background(), repo); err != nil {
		return isolatedParent{}, err
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return isolatedParent{}, err
	}
	metadata, err := forkspace.GitMetadataDirectories(canonical)
	if err != nil {
		return isolatedParent{}, err
	}
	head, err := observed(canonical, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !validObjectID(head) {
		return isolatedParent{}, errors.Join(errors.New("isolated review needs a committed parent HEAD"), err)
	}
	tree, err := observed(canonical, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return isolatedParent{}, err
	}
	target, targetErr := observed(canonical, "symbolic-ref", "--quiet", "HEAD")
	if targetErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(targetErr, &exitErr) || exitErr.ExitCode() != 1 {
			return isolatedParent{}, targetErr
		}
	}
	return isolatedParent{Repo: canonical, Metadata: metadata, Target: target, Head: head, Tree: tree}, nil
}

func captureIsolatedCheckout(repo string, detachedAllowed bool, depth int) (isolatedParent, error) {
	return captureIsolatedCheckoutTree(repo, detachedAllowed, depth, "")
}

func captureIsolatedCheckoutTree(repo string, detachedAllowed bool, depth int, expectedCommit string) (isolatedParent, error) {
	remaining := 1024
	return captureIsolatedCheckoutBounded(repo, detachedAllowed, depth, expectedCommit, &remaining)
}

func captureIsolatedCheckoutBounded(repo string, detachedAllowed bool, depth int, expectedCommit string, remaining *int) (isolatedParent, error) {
	if depth > 16 {
		return isolatedParent{}, errors.New("isolated parent gitlinks exceed the inspection depth")
	}
	if err := forkspace.QualifyDefaultLFSStorage(context.Background(), repo); err != nil {
		return isolatedParent{}, err
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return isolatedParent{}, err
	}
	metadata, err := forkspace.GitMetadataDirectories(canonical)
	if err != nil {
		return isolatedParent{}, err
	}
	if _, err := os.Lstat(filepath.Join(metadata[0], "index.lock")); !errors.Is(err, os.ErrNotExist) {
		return isolatedParent{}, errors.Join(errors.New("isolated land refuses an uncertain index.lock; preserve it and reconcile the checkout"), err)
	}
	target, err := observed(canonical, "symbolic-ref", "--quiet", "HEAD")
	if target == "" && detachedAllowed {
		err = nil
	}
	if err != nil || target != "" && !strings.HasPrefix(target, "refs/heads/") {
		return isolatedParent{}, errors.Join(errors.New("isolated land needs an attached target branch"), err)
	}
	head, err := observed(canonical, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !validObjectID(head) {
		return isolatedParent{}, errors.Join(errors.New("isolated parent has no exact commit"), err)
	}
	if expectedCommit == "" {
		expectedCommit = head
	}
	tree, err := observed(canonical, "rev-parse", "--verify", expectedCommit+"^{tree}")
	if err != nil {
		return isolatedParent{}, err
	}
	flags, err := forkspace.ObserveGit(context.Background(), canonical, "ls-files", "-v", "-z")
	if err != nil {
		return isolatedParent{}, err
	}
	for _, record := range bytes.Split(flags, []byte{0}) {
		if len(record) != 0 && (len(record) < 3 || record[0] != 'H' || record[1] != ' ') {
			return isolatedParent{}, errors.New("isolated land refuses hidden index flags or unmerged entries")
		}
	}
	index, err := forkspace.ObserveGit(context.Background(), canonical, "ls-files", "--stage", "-z")
	if err != nil {
		return isolatedParent{}, err
	}
	entries, err := isolatedTree(canonical, expectedCommit)
	if err != nil {
		return isolatedParent{}, err
	}
	var expected bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&expected, "%s %s 0\t%s\x00", entry.mode, entry.object, entry.path)
	}
	if !bytes.Equal(index, expected.Bytes()) {
		return isolatedParent{}, errors.New("isolated land refuses staged changes or an unmerged index")
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return isolatedParent{}, err
	}
	defer root.Close()
	content := sha256.New()
	for _, entry := range entries {
		fmt.Fprintf(content, "%d:%s:%s:", len(entry.path), entry.path, entry.mode)
		if err := verifyIsolatedTracked(canonical, root, entry, depth, remaining, content); err != nil {
			return isolatedParent{}, fmt.Errorf("isolated parent tracked file %q: %w", entry.path, err)
		}
	}
	// Each observation is independent; detect a branch or index change during collection.
	endHead, err := observed(canonical, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || endHead != head {
		return isolatedParent{}, errors.Join(errors.New("parent changed while capturing isolated land"), err)
	}
	endTarget, err := observed(canonical, "symbolic-ref", "--quiet", "HEAD")
	if target == "" && endTarget == "" && detachedAllowed {
		err = nil
	}
	if err != nil || endTarget != target {
		return isolatedParent{}, errors.Join(errors.New("parent target changed while capturing isolated land"), err)
	}
	endIndex, err := forkspace.ObserveGit(context.Background(), canonical, "ls-files", "--stage", "-z")
	if err != nil || !bytes.Equal(index, endIndex) {
		return isolatedParent{}, errors.Join(errors.New("parent index changed while capturing isolated land"), err)
	}
	sum := sha256.Sum256(index)
	return isolatedParent{canonical, metadata, target, head, tree, fmt.Sprintf("%x", sum), fmt.Sprintf("%x", content.Sum(nil))}, nil
}

func verifyIsolatedTracked(repo string, root *os.Root, entry isolatedTreeEntry, depth int, remaining *int, contentState hash.Hash) error {
	parts := strings.Split(entry.path, "/")
	for i := 1; i < len(parts); i++ {
		info, err := root.Lstat(strings.Join(parts[:i], "/"))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(errors.New("tracked path has a redirected or missing ancestor"), err)
		}
	}
	info, err := root.Lstat(entry.path)
	if err != nil {
		return err
	}
	if entry.mode == "160000" {
		if *remaining == 0 {
			return errors.New("isolated parent gitlinks exceed the inspection count")
		}
		*remaining--
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("gitlink is not a real directory")
		}
		child := filepath.Join(repo, filepath.FromSlash(entry.path))
		state, err := captureIsolatedCheckoutBounded(child, true, depth+1, "", remaining)
		if err != nil || state.Head != entry.object {
			return errors.Join(errors.New("gitlink checkout is changed or unqualified"), err)
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		_, _ = contentState.Write(sum[:])
		return nil
	}
	var reader io.Reader
	var size int64
	if entry.mode == "120000" {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("tracked symlink changed mode")
		}
		target, err := root.Readlink(entry.path)
		if err != nil {
			return err
		}
		reader, size = strings.NewReader(target), int64(len(target))
	} else {
		if !info.Mode().IsRegular() || (info.Mode().Perm()&0111 != 0) != (entry.mode == "100755") {
			return errors.New("tracked file changed mode")
		}
		file, err := root.OpenFile(entry.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		after, err := file.Stat()
		if err != nil || !os.SameFile(info, after) || !after.Mode().IsRegular() {
			return errors.Join(errors.New("tracked file changed while opening"), err)
		}
		reader, size = file, info.Size()
	}
	var digest hash.Hash = sha1.New()
	if len(entry.object) == 64 {
		digest = sha256.New()
	}
	fmt.Fprintf(digest, "blob %d\x00", size)
	content := sha256.New()
	n, err := io.Copy(io.MultiWriter(digest, content), reader)
	if err != nil || n != size {
		return errors.Join(errors.New("tracked content changed while reading"), err)
	}
	_, _ = contentState.Write(content.Sum(nil))
	if fmt.Sprintf("%x", digest.Sum(nil)) == entry.object {
		return nil
	}
	// A native LFS checkout contains verified payload bytes, not its committed pointer.
	if entry.mode != "120000" {
		expected := sha256.New()
		if err := forkspace.ObserveCheckoutBlob(context.Background(), repo, entry.object, entry.path, expected); err != nil {
			return fmt.Errorf("qualify native checkout conversion: %w", err)
		}
		if bytes.Equal(content.Sum(nil), expected.Sum(nil)) {
			return nil
		}
		blobSize, err := observed(repo, "cat-file", "-s", entry.object)
		count, sizeErr := strconv.ParseInt(blobSize, 10, 64)
		if err != nil || sizeErr != nil || count > 1024 {
			return errors.New("tracked content differs from the committed tree")
		}
		pointer, err := forkspace.ObserveGit(context.Background(), repo, "cat-file", "blob", entry.object)
		if err == nil && len(pointer) <= 1024 {
			oid, length, ok := forkspace.ParseLFSPointer(pointer)
			attr, attrErr := forkspace.ObserveGit(context.Background(), repo, "check-attr", "--cached", "-z", "filter", "--", entry.path)
			if ok && attrErr == nil && string(attr) == entry.path+"\x00filter\x00lfs\x00" && size == length {
				if fmt.Sprintf("%x", content.Sum(nil)) == oid {
					return nil
				}
			}
		}
	}
	return errors.New("tracked content differs from the committed tree")
}

func sameIsolatedParent(a, b isolatedParent) bool { return reflect.DeepEqual(a, b) }

func isolatedSourceUntracked(repo string, depth int, remaining *int) error {
	if depth > 16 || *remaining == 0 {
		return errors.New("isolated source exceeds recursive cleanup inspection bounds")
	}
	*remaining--
	if err := forkspace.ValidateIndependentGit(repo); err != nil {
		return err
	}
	untracked, err := forkspace.ObserveGit(context.Background(), repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || len(untracked) > 0 {
		return errors.Join(errors.New("isolated source still has uncommitted work; keep the fork"), err)
	}
	head, err := observed(repo, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	links, err := forkspace.Gitlinks(context.Background(), repo, head)
	if err != nil {
		return err
	}
	for path := range links {
		if err := isolatedSourceUntracked(filepath.Join(repo, filepath.FromSlash(path)), depth+1, remaining); err != nil {
			return fmt.Errorf("isolated submodule %q: %w", path, err)
		}
	}
	return nil
}

func checkIsolatedIntroductions(repo, candidate, base, publication string) error {
	before, err := isolatedTree(candidate, base)
	if err != nil {
		return err
	}
	after, err := isolatedTree(candidate, publication)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, entry := range before {
		existing[entry.path] = true
	}
	for _, entry := range after {
		if existing[entry.path] {
			continue
		}
		parts := strings.Split(entry.path, "/")
		for i := 1; i <= len(parts); i++ {
			path := strings.Join(parts[:i], "/")
			info, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(path)))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			if existing[path] {
				break // a committed file-to-directory transition is checked by Git
			}
			if i == len(parts) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("isolated land refuses untracked/ignored path collision at %q", path)
			}
		}
	}
	return nil
}
