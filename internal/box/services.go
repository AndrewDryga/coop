package box

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// EnsureServices brings the repo's sibling services up (compose up -d --wait) so a box can
// reach them by name. It is idempotent — already-running services are a fast no-op — and a
// no-op (nil) when the repo has no compose file. Progress is written to stdout/stderr; the
// caller decides where to point them and gates on a compose-capable runtime (Apple
// `container` has no compose). Shared by `coop up` and box.Run's auto-start.
func EnsureServices(rt runtime.Runtime, repo string, stdout, stderr io.Writer) error {
	file := ComposeFile(repo)
	if file == "" {
		return nil
	}
	// coop runs this file on the HOST daemon, so validate it first: an in-box agent may author it
	// (the compose path is no longer shadowed), but the host refuses anything that reaches outside a
	// repo-scoped, loopback-only container. The specific violation rides out to `coop up` / the
	// auto-up warning, so a refused file names exactly why.
	if err := ValidateComposeFile(file, repo); err != nil {
		return fmt.Errorf("refusing to run %s: %w", filepath.Base(file), err)
	}
	proj := ServicesProject(repo)
	code, err := rt.Run(nil, stdout, stderr, "compose", "-p", proj, "-f", file, "up", "-d", "--wait")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("compose up exited with code %d", code)
	}
	return nil
}

// autoUpServices reports whether box.Run should auto-start sibling services before launching a
// box: the COOP_AUTO_UP toggle is on (default), the box joins the services network (so it could
// reach them), it isn't offline (COOP_EGRESS=none, where there's nothing to reach), and the
// runtime supports compose — Apple `container` does not. Whether a compose file actually exists
// is checked separately, by EnsureServices.
func autoUpServices(cfg *config.Config, spec RunSpec, rtName string) bool {
	return cfg.AutoUp && spec.Network && cfg.Egress == "open" && rtName != "container"
}
