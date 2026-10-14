package procharness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

type binaryKey struct {
	root, pkg, tags string
}

// BinaryCache belongs to one test process with frozen source/build inputs. It caches only
// immutable compiled bytes, never case homes, workspaces or authority. TestMain owns its
// private directory and removal; this is not a persistent or cross-version cache.
type BinaryCache struct {
	dir, goPath string
	env         []string
	mu          sync.Mutex
	files       map[binaryKey]string
}

// NewBinaryCache captures the toolchain/environment before tests change PATH, HOME or TMPDIR.
// Call it in TestMain beneath a process-owned temporary root, not a first case's TempDir.
func NewBinaryCache(dir string) (*BinaryCache, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		return nil, err
	}
	goPath, err = filepath.Abs(goPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &BinaryCache{dir: dir, goPath: goPath, env: os.Environ(), files: map[binaryKey]string{}}, nil
}

// Copy builds each root/package/tag identity once and creates an independent executable.
// Never return the cache path or hardlink it into a writable case layout.
func (c *BinaryCache) Copy(ctx context.Context, root, destination, pkg string, tags ...string) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	var normalized []string
	for _, tag := range tags {
		normalized = append(normalized, strings.Fields(strings.ReplaceAll(tag, ",", " "))...)
	}
	slices.Sort(normalized)
	key := binaryKey{root: root, pkg: pkg, tags: strings.Join(slices.Compact(normalized), ",")}
	source, err := c.binary(ctx, key)
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if err := out.Chmod(0o700); err != nil {
		return errors.Join(err, out.Close(), os.Remove(destination))
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(destination))
	}
	return nil
}

func (c *BinaryCache) binary(ctx context.Context, key binaryKey) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if file := c.files[key]; file != "" {
		return file, nil
	}
	file, err := os.CreateTemp(c.dir, "binary-")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(name)
		}
	}()
	result := Run(ctx, Command{
		Path: c.goPath, Dir: key.root, Env: c.env, MaxOutput: 1 << 20,
		Args: []string{"build", "-trimpath", "-tags", key.tags, "-o", name, key.pkg},
	})
	if result.Err != nil || result.ExitCode != 0 {
		return "", errors.Join(result.Err, fmt.Errorf("build %s: exit %d\n%s%s", key.pkg, result.ExitCode, result.Stdout, result.Stderr))
	}
	info, err := os.Stat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", errors.New("fixture build did not produce a nonempty regular executable")
	}
	if err := os.Chmod(name, 0o500); err != nil {
		return "", err
	}
	c.files[key], complete = name, true
	return name, nil
}
