package cli

import (
	"io"
	"os"
	"os/exec"
	"strings"
)

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// queueHasTodo reports whether a TASKS.md file still has an unclaimed "[ ]" item.
func queueHasTodo(queue string) bool {
	data, err := os.ReadFile(queue)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "[ ]")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func gitClone(src, dst string) error {
	return exec.Command("git", "clone", "--quiet", src, dst).Run()
}

func gitCheckoutNewBranch(repo, branch string) error {
	return exec.Command("git", "-C", repo, "checkout", "--quiet", "-b", branch).Run()
}
