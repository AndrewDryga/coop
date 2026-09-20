package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/eval"
)

// Grading is the one part of an eval the candidate must never touch, so it is deliberately built as
// the candidate's opposite:
//
//   - it runs AFTER the candidate's processes are gone, against an immutable SNAPSHOT of the
//     workspace, so nothing the candidate is still doing can change the verdict;
//   - it runs in a FRESH container from the trusted Coop image — never the candidate's image, shell
//     configuration or interpreter — with NO model credentials and NO network, so the grader cannot
//     be steered, and a verifier cannot quietly phone a model for its answer either;
//   - the verifier is mounted read-only at a fixed path that was never in any candidate mount, so
//     the hidden material stays hidden even from a candidate that explored its whole filesystem.
//
// The verdict contract is three-valued on purpose, because "the grader broke" is not "the model
// failed" (the spec is explicit that a verifier crash, timeout or missing dependency is a HARNESS
// error, never a quality zero):
//
//	exit 0        -> passed
//	exit 1        -> failed (the candidate's work did not meet the case)
//	anything else -> harness error (verifier crashed, dependency missing, killed)
//	launch/timeout-> harness error
const (
	// gradeWorkspaceDir is where the snapshot is mounted for the verifier.
	gradeWorkspaceDir = "/workspace"
	// gradeVerifierDir is where the hidden verifier is mounted. It is intentionally NOT under the
	// workspace: nothing the candidate could enumerate ever pointed here.
	gradeVerifierDir = "/coop-verifier"
	// gradeExitFailed is the one non-zero exit that means an honest "did not pass".
	gradeExitFailed = 1
)

// verifierEntries are the accepted entry points inside a verifier directory, in order. `verify.sh`
// is run with the TRUSTED image's own shell; `verify` must be executable and is run directly.
var verifierEntries = []struct {
	name    string
	command []string
}{
	// /bin/sh by ABSOLUTE path: an unqualified "sh" resolves through PATH, and PATH is one of the
	// things a candidate can try to influence — a forged /workspace/bin/sh that exits 0 would be a
	// silent pass for every case.
	{"verify.sh", []string{"/bin/sh", gradeVerifierDir + "/verify.sh"}},
	{"verify", []string{gradeVerifierDir + "/verify"}},
}

// gradeRequest is everything grading needs; it keeps the decision logic independent of how the box
// is launched so the verdict mapping is unit-testable without a runtime.
type gradeRequest struct {
	Image     string // the TRUSTED image, resolved by the caller — never anything the candidate built
	Workspace string // host path of the immutable snapshot
	Verifier  string // host path of the verifier directory (hidden material)
	CaseID    string
}

// boxRunner launches one box and reports its exit code; nil in production, injected by tests (the
// same seam the loop uses for its own box launches).
type boxRunner func(box.RunSpec) (int, error)

// gradeSnapshot runs the case's verifier against a workspace snapshot and maps the result onto a
// trial status. It never returns an error: every failure mode is a TrialResult, because a run must
// record why a trial has no verdict rather than abort the whole sweep.
func (a *app) gradeSnapshot(ctx context.Context, req gradeRequest, run boxRunner) eval.TrialResult {
	command, err := verifierCommand(req.Verifier)
	if err != nil {
		return eval.TrialResult{Status: eval.TrialError, Detail: err.Error()}
	}
	// The grader's box policy must come from nowhere the candidate can write. A box reads
	// .agent/project.yaml from its policy repo — which defaults to the mounted repo, i.e. the
	// SNAPSHOT — so a candidate could ship a project.yaml that re-opens the grader's network or
	// rewrites its PATH. Point the policy at an empty directory of our own instead: no policy, no
	// override, nothing to forge.
	policyDir, err := os.MkdirTemp("", "coop-eval-nopolicy-")
	if err != nil {
		return eval.TrialResult{Status: eval.TrialError, Detail: "could not prepare the grading sandbox: " + err.Error()}
	}
	defer os.RemoveAll(policyDir)

	if run == nil {
		// A CLONE of the config with egress cut. SetEgress (not a bare field write) also marks the
		// mode explicit, which is what stops a project policy from deciding it a second time; and a
		// clone, not `*a.cfg`, because Config's per-run maps are shared by a shallow copy.
		offline := a.cfg.Clone()
		offline.SetEgress("none")
		run = func(spec box.RunSpec) (int, error) { return box.Run(offline, a.rt, spec) }
	}

	var out, errOut bytes.Buffer
	spec := box.RunSpec{
		Ctx:        ctx,
		Image:      req.Image,
		Repo:       req.Workspace,
		PolicyRepo: policyDir, // never the snapshot: see above
		// The grader sees the tree as it is. Secret shadowing would replace a file the candidate was
		// asked to produce — a .env, a key, a certificate — with an empty decoy, and the case would
		// fail for a reason that has nothing to do with the model.
		GradeSnapshot: true,
		Workdir:       gradeWorkspaceDir,
		Cmd:           command,
		// Batch: no tty and no stdin — a verifier that waits for input times out rather than hangs
		// the sweep. Quiet: grading narration is the run's, not the box's.
		Batch: true,
		Quiet: true,
		// Homes is false: the grading container gets NO agent credentials. This is the line that
		// keeps a verifier from being able to spend the user's quota or read their tokens.
		Homes: false,
		// No shared cache volume: it is writable, it persists between trials, and the grader has no
		// business reading anything a previous candidate left in it.
		Cache:     false,
		Stdout:    &out,
		Stderr:    &errOut,
		ExtraArgs: []string{"-v", req.Verifier + ":" + gradeVerifierDir + ":ro"},
	}
	code, runErr := run(spec)
	detail := gradeDetail(out.String(), errOut.String())
	switch {
	case runErr != nil:
		// Could not launch, or was killed at the deadline: a harness error, never a quality zero.
		// An expired budget is a timeout wherever it is noticed — the same rule the trial steps use.
		return fail(ctx, joinDetail("grading could not run: "+runErr.Error(), detail))
	case code == 0:
		return eval.TrialResult{Status: eval.TrialPassed, Detail: detail}
	case code == gradeExitFailed:
		return eval.TrialResult{Status: eval.TrialFailed, Detail: detail}
	default:
		// The verifier neither passed nor failed the candidate — it broke. Keeping this distinct is
		// what stops a broken grader from silently reading as a model regression.
		return eval.TrialResult{Status: eval.TrialError, Detail: joinDetail(
			fmt.Sprintf("verifier exited %d, which is neither pass (0) nor fail (1) — treating it as a grading error, not a model failure", code), detail)}
	}
}

// verifierCommand picks the entry point inside a verifier directory and returns the command to run
// it inside the grading container. A verifier with no recognized entry is a harness error stated in
// the terms the author needs to fix it.
func verifierCommand(dir string) ([]string, error) {
	for _, entry := range verifierEntries {
		info, err := os.Stat(filepath.Join(dir, entry.name))
		if err != nil || info.IsDir() {
			continue
		}
		if entry.name == "verify" && info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("verifier %q is not executable — chmod +x it, or provide verify.sh instead", filepath.Join(dir, entry.name))
		}
		return entry.command, nil
	}
	return nil, fmt.Errorf("verifier directory %q has no verify.sh or executable verify — the grader needs an entry point", dir)
}

// gradeDetail keeps a bounded, readable tail of what the verifier said. The text is candidate- and
// verifier-influenced, so it is truncated here and again when the record is written.
func gradeDetail(stdout, stderr string) string {
	const maxDetail = 2 << 10
	parts := make([]string, 0, 2)
	if s := strings.TrimSpace(stdout); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(stderr); s != "" {
		parts = append(parts, s)
	}
	joined := strings.Join(parts, "\n")
	if len(joined) > maxDetail {
		return "…" + joined[len(joined)-maxDetail:] // the tail is where a verifier says why
	}
	return joined
}

func joinDetail(lead, rest string) string {
	if strings.TrimSpace(rest) == "" {
		return lead
	}
	return lead + ": " + rest
}
