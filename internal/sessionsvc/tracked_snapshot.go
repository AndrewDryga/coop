package sessionsvc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

// Capture tracked working content without staging hydrated LFS payloads as false
// changes. All index/object writes belong to a disposable private repository;
// neither the model's index nor its executable filters participate in capture.
func streamSessionTrackedPatch(ctx context.Context, workspace, base string, output io.Writer) error {
	return withSessionTrackedTree(ctx, workspace, func(tree string, env []string) error {
		diff := exec.CommandContext(ctx, "git", gitArgs(workspace, []string{
			"diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", "--submodule=short", "--binary", base, tree, "--",
		})...)
		diff.Env, diff.Stdout = env, output
		return errors.Join(diff.Run(), ctx.Err())
	})
}

func withSessionTrackedTree(ctx context.Context, workspace string, action func(string, []string) error) error {
	head, err := sessionWorkspaceCommitContext(ctx, workspace, "HEAD")
	if err != nil {
		return err
	}
	return withSessionPrivateIndex(ctx, session.CompanionRepository{Workspace: workspace, BaseCommit: head}, func(env []string) error {
		env = append(env, "GIT_LITERAL_PATHSPECS=1")
		command := func(args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, "git", gitArgs(workspace, args)...)
			cmd.Env = env
			return cmd
		}
		// Export entries rather than copying index bytes: split/sparse indexes
		// must resolve their backing data, and agent skip-worktree bits are not
		// authority to omit changed content. Staged additions/deletions survive.
		if err := command("read-tree", "--empty").Run(); err != nil {
			return err
		}
		listing, err := forkspace.GitCommandWithEnv(ctx, workspace, sessionCompanionCheckoutGitEnv(), "ls-files", "--stage", "-z")
		if err != nil {
			return err
		}
		pipe, err := listing.StdoutPipe()
		if err != nil {
			return err
		}
		defer pipe.Close()
		if err := listing.Start(); err != nil {
			return err
		}
		importIndex := command("update-index", "-z", "--index-info")
		importIndex.Stdin = pipe
		importErr := importIndex.Run()
		if importErr != nil {
			_ = listing.Process.Kill()
		}
		if err := errors.Join(importErr, listing.Wait()); err != nil {
			return err
		}
		status := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceGitOutputLimit}
		err = forkspace.RunLFSStatus(ctx, workspace, command("status", "--porcelain=v2", "--untracked-files=no", "--no-renames", "--ignore-submodules=all", "-z"), status,
			func(limit int, args ...string) ([]byte, error) {
				data, truncated, err := runSessionPrivateGitContext(ctx, workspace, limit, env, args...)
				if truncated {
					return nil, errors.New("snapshot Git read exceeds its bound")
				}
				return data, err
			})
		if err != nil {
			return err
		}
		if status.truncated {
			return errors.New("snapshot workspace status exceeds its bound")
		}
		changes, err := parseSessionWorkspaceStatus(status.buf.Bytes())
		if err != nil {
			return err
		}
		var paths bytes.Buffer
		for _, change := range append(changes.Unstaged, changes.Conflicts...) {
			paths.Write(change.PathBytes)
			paths.WriteByte(0)
		}
		if paths.Len() != 0 {
			stage := command("add", "-u", "--pathspec-from-file=-", "--pathspec-file-nul")
			stage.Stdin = &paths
			if err := stage.Run(); err != nil {
				return err
			}
		}
		tree, truncated, err := runSessionPrivateGitContext(ctx, workspace, 128, env, "write-tree")
		if err != nil || truncated {
			return errors.New("cannot resolve tracked snapshot tree")
		}
		return action(strings.TrimSpace(string(tree)), env)
	})
}
