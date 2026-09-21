package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// CheckoutFingerprint binds a no-change completion to the index and actual checkout bytes.
// Status labels and diffs miss same-status edits and files hidden by index flags. Git supplies
// metadata only; direct rooted reads avoid filters, textconv and recursive submodule drivers.
func CheckoutFingerprint(repo string) (string, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return "", err
	}
	defer root.Close()
	return checkoutFingerprint(repo, root, nil)
}

func checkoutFingerprint(repo string, root *os.Root, ancestors []os.FileInfo) (string, error) {
	identity, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	for _, ancestor := range ancestors {
		if os.SameFile(identity, ancestor) {
			return "", fmt.Errorf("recursive checkout at %s", repo)
		}
	}
	ancestors = append(ancestors, identity)
	checkIdentity := func() error {
		current, err := os.Stat(repo)
		if err != nil {
			return err
		}
		if !os.SameFile(identity, current) {
			return fmt.Errorf("checkout changed while inspecting %s", repo)
		}
		return nil
	}
	if err := checkIdentity(); err != nil {
		return "", err
	}
	head, err := gitRawOutErr(repo, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		// An initialized, unborn nested repository is normal. Other Git failures are not
		// evidence of an empty checkout.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return "", err
		}
		ref, refErr := gitRawOutErr(repo, "symbolic-ref", "--quiet", "HEAD")
		if refErr != nil {
			return "", errors.Join(err, refErr)
		}
		head = "unborn:" + ref
	}
	status, err := gitRawOutErr(repo, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=all")
	if err != nil {
		return "", err
	}
	index, err := gitRawOutErr(repo, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		return "", err
	}
	untracked, err := gitRawOutErr(repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	if err := checkIdentity(); err != nil {
		return "", err
	}
	sum := sha256.New()
	// Porcelain also captures intent-to-add, which is not exposed by ls-files --stage -v.
	fingerprintFields(sum, head, status, index, untracked)
	seen := make(map[string]bool)
	for _, record := range strings.Split(index, "\x00") {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 4 {
			return "", fmt.Errorf("invalid checkout index entry %q", record)
		}
		if seen[path] {
			continue // All stages are bound above; read the worktree path only once.
		}
		seen[path] = true
		digest, err := checkoutPathFingerprint(repo, root, path, true, fields[1] == "160000", ancestors)
		if err != nil {
			return "", fmt.Errorf("inspect tracked %q: %w", path, err)
		}
		fingerprintFields(sum, path, digest)
	}
	for _, path := range strings.Split(untracked, "\x00") {
		if path == "" {
			continue
		}
		// Git reports an opaque nested repository with a trailing slash.
		nested := strings.HasSuffix(path, "/")
		digest, err := checkoutPathFingerprint(repo, root, strings.TrimSuffix(path, "/"), false, nested, ancestors)
		if err != nil {
			return "", fmt.Errorf("inspect untracked %q: %w", path, err)
		}
		fingerprintFields(sum, path, digest)
	}
	if err := checkIdentity(); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func fingerprintFields(dst io.Writer, fields ...string) {
	for _, field := range fields {
		fmt.Fprintf(dst, "%d:", len(field))
		io.WriteString(dst, field)
	}
}

// Open each real ancestor separately: an inherited directory-to-symlink replacement is valid
// dirty work, but its literal target must be bound instead of following it outside the checkout.
func checkoutPathFingerprint(repo string, root *os.Root, rel string, tracked, nested bool, ancestors []os.FileInfo) (string, error) {
	rel = filepath.FromSlash(rel)
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel || rel == "." {
		return "", fmt.Errorf("invalid checkout path %q", rel)
	}
	parent, err := root.OpenRoot(".")
	if err != nil {
		return "", err
	}
	defer func() { parent.Close() }()
	parts := strings.Split(rel, string(filepath.Separator))
	for i, name := range parts {
		info, err := parent.Lstat(name)
		if err != nil {
			if tracked && errors.Is(err, os.ErrNotExist) {
				return "missing", nil
			}
			return "", err
		}
		if i < len(parts)-1 && info.IsDir() {
			next, err := openRealSubroot(parent, name)
			if err != nil {
				return "", err
			}
			parent.Close()
			parent = next
			continue
		}
		if i == len(parts)-1 && info.IsDir() && nested {
			child, err := openRealSubroot(parent, name)
			if err != nil {
				return "", err
			}
			defer child.Close()
			metadata, err := child.Lstat(".git")
			if errors.Is(err, os.ErrNotExist) && tracked {
				// A deinitialized submodule has an empty mountpoint. A populated directory
				// without metadata is not an examined child and cannot authorize completion.
				dir, err := child.Open(".")
				if err != nil {
					return "", err
				}
				_, readErr := dir.ReadDir(1)
				closeErr := dir.Close()
				if errors.Is(readErr, io.EOF) && closeErr == nil {
					return "uninitialized submodule", nil
				}
				return "", errors.Join(errors.New("submodule has content but no Git metadata"), readErr, closeErr)
			}
			if err != nil {
				return "", err
			}
			if !metadata.IsDir() && !metadata.Mode().IsRegular() {
				return "", errors.New("nested checkout Git metadata is not a file or directory")
			}
			digest, err := checkoutFingerprint(filepath.Join(repo, rel), child, ancestors)
			if err != nil {
				return "", err
			}
			// Git uses a pathname rather than our held descriptor. Revalidate real ancestry
			// as well as the child's own identity after inspecting it.
			current, err := openRealSubroot(root, rel)
			if err != nil {
				return "", err
			}
			after, statErr := current.Stat(".")
			closeErr := current.Close()
			if statErr != nil || closeErr != nil {
				return "", errors.Join(statErr, closeErr)
			}
			if !os.SameFile(info, after) {
				return "", errors.New("nested checkout changed while inspecting it")
			}
			return "repository:" + digest, nil
		}
		digest, err := checkoutEntryFingerprint(parent, name, info)
		return fmt.Sprintf("entry:%d:%s", i, digest), err
	}
	return "", errors.New("empty checkout path")
}

func checkoutEntryFingerprint(root *os.Root, name string, before os.FileInfo) (string, error) {
	sum := sha256.New()
	fingerprintFields(sum, fmt.Sprint(before.Mode()))
	switch {
	case before.Mode()&os.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return "", err
		}
		fingerprintFields(sum, target)
	case before.Mode().IsRegular():
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", err
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
			file.Close()
			return "", errors.Join(errors.New("checkout file changed while opening it"), err)
		}
		n, readErr := io.Copy(sum, io.LimitReader(file, opened.Size()+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		if err := errors.Join(readErr, statErr, closeErr); err != nil {
			return "", err
		}
		if n != opened.Size() || after.Size() != opened.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(opened.ModTime()) {
			return "", errors.New("checkout file changed while reading it")
		}
	}
	// Directories, sockets and devices bind their type/mode, never an open that can block.
	return hex.EncodeToString(sum.Sum(nil)), nil
}
