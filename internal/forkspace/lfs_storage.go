package forkspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// QualifyDefaultLFSStorage inspects only trusted native sources. Isolated clones
// use their own default cache; selecting arbitrary storage is not that contract.
// Native config is read as data, never through an alias, filter, helper or pager.
func QualifyDefaultLFSStorage(ctx context.Context, repository string) error {
	canonical, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return err
	}
	roots, err := GitMetadataDirectories(canonical)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--no-pager", "--git-dir="+roots[0], "--work-tree="+canonical,
		"config", "--includes", "--null", "--get", "lfs.storage")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "GIT_CONFIG_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	output, diagnostic := &lfsConfigOutput{}, &lfsConfigOutput{}
	cmd.Stdout, cmd.Stderr = output, diagnostic
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && output.Len() == 0 && diagnostic.Len() == 0 && ctx.Err() == nil {
		return nil
	}
	if err != nil || diagnostic.Len() != 0 || ctx.Err() != nil {
		return errors.Join(errors.New("cannot qualify native LFS storage configuration"), err, ctx.Err())
	}
	if data := output.Bytes(); len(data) == 0 || data[len(data)-1] != 0 || bytes.Count(data, []byte{0}) != 1 {
		return errors.New("native LFS storage configuration is malformed")
	}
	if output.Len() == 1 {
		return nil // An explicitly empty value selects Git LFS's default storage.
	}
	return fmt.Errorf("isolated forks require the default common-directory LFS cache; %s selects lfs.storage — existing work is unchanged", canonical)
}

type lfsConfigOutput struct{ bytes.Buffer }

func (output *lfsConfigOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > 4096 {
		return 0, errors.New("native LFS configuration output exceeds its bound")
	}
	return output.Buffer.Write(data)
}
