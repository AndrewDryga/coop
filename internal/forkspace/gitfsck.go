package forkspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/safefile"
)

// RunGitFsck checks objects and references without reflogs. Unlike the operational
// view, its refs directory is a real snapshot: modern Git's reference verifier
// refuses a symlink at that root. It never reconciles HEAD or changes cached views.
func RunGitFsck(ctx context.Context, dir string, env []string, stdout, stderr io.Writer, args ...string) error {
	view, err := fsckGitView(ctx, dir)
	if err != nil {
		return err
	}
	defer os.RemoveAll(view.dir)
	full := append(append(append([]string{"-C", dir}, GitHardening...), gitViewHardening...), "fsck", "--no-reflogs")
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Env = view.envFrom(env)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

func fsckGitView(ctx context.Context, workTree string) (_ *gitView, err error) {
	if err := validateIsolatedGitAccess(workTree); err != nil {
		return nil, err
	}
	gitDir, commonDir, err := gitDirsOf(workTree)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "coop-gitfsck-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	common, err := safefile.OpenRoot(commonDir)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	work, err := safefile.OpenRoot(gitDir)
	if err != nil {
		return nil, err
	}
	defer work.Close()
	if err := copyGitMetadataFile(ctx, work, "HEAD", filepath.Join(dir, "HEAD")); err != nil {
		return nil, err
	}
	refs, err := safefile.OpenDir(common, "refs")
	if err != nil {
		return nil, err
	}
	defer refs.Close()
	if err := copyGitRefs(ctx, refs, filepath.Join(dir, "refs")); err != nil {
		return nil, err
	}
	for _, name := range []string{"packed-refs", "shallow", "config"} {
		if err := copyGitMetadataFile(ctx, common, name, filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	config, err := trustedGitConfig(ctx, filepath.Join(dir, "config"), dir)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), config, 0o600); err != nil {
		return nil, err
	}
	if err := os.Symlink(filepath.Join(commonDir, "objects"), filepath.Join(dir, "objects")); err != nil {
		return nil, err
	}
	return &gitView{workTree: workTree, gitDir: gitDir, commonDir: commonDir, dir: dir}, nil
}

func copyGitRefs(ctx context.Context, source *os.File, dest string) error {
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := source.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(dest, entry.Name())
			if entry.IsDir() {
				child, err := safefile.OpenDir(source, entry.Name())
				if err != nil {
					return err
				}
				err = copyGitRefs(ctx, child, path)
				closeErr := child.Close()
				if err := errors.Join(err, closeErr); err != nil {
					return err
				}
			} else if err := copyGitMetadataFile(ctx, source, entry.Name(), path); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func copyGitMetadataFile(ctx context.Context, root *os.File, name, dest string) error {
	source, err := safefile.OpenRegular(root, name)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(target, &gitMetadataReader{ctx: ctx, reader: source})
	return errors.Join(err, target.Close())
}

type gitMetadataReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *gitMetadataReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
