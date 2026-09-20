package eval

import (
	"fmt"
	"os"
	"path/filepath"
)

// Scaffold writes a working custom suite into dir: a runnable agent example plus a documented loop
// example, and the directories the manifest references. It is create-only — the CLI proves the
// manifest does not already exist — so it never overwrites a user's work. The result loads and
// RUNS as-is — the scaffolded verifier is a real, working grader for the scaffolded case, not a
// placeholder — so `coop eval init` followed by `coop eval run` produces an actual verdict, and a
// user edits something they have already seen work.
func Scaffold(dir string) error {
	files := map[string]string{
		"suite.yaml":                   scaffoldManifest,
		"verifiers/greeting/README.md": scaffoldVerifierNote,
		"verifiers/greeting/verify.sh": scaffoldVerifier,
		"files/greeting/.keep":         "",
	}
	for rel := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(rel), err)
		}
	}
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		// Create-only per file: eval init never truncates an existing file, even one the user kept
		// after deleting suite.yaml. O_EXCL fails if it already exists.
		f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		_, werr := f.WriteString(body)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("write %s: %w", rel, werr)
		}
	}
	return nil
}

const scaffoldManifest = `# A custom Coop eval suite. Run it with:
#   coop eval run ./suite.yaml <target|preset>... --timeout 30m
#
# Each positional after the suite is one configuration to compare — a target
# (provider[:model][/effort][@account]) or a preset. Run the same suite before
# and after a change, then compare the two run ids.

version: 1
name: my-suite
runner: agent            # "agent" (one headless attempt) or "loop" (the whole Coop loop)

cases:
  - id: greeting
    instruction: "Create greeting.txt containing exactly: hello"
    files: ./files/greeting        # optional initial workspace files
    verifier: ./verifiers/greeting # an INDEPENDENT grader, never mounted into the candidate
    timeout: 10m

# A loop suite instead looks like this (uncomment and set runner: loop):
#
# runner: loop
# loop_config: ./loop.yaml
# cases:
#   - id: repository-evolution
#     fixture: ./fixtures/app             # the initial repository the scenario starts from
#     tasks: ./queues/repository-evolution # an ordinary Coop task queue template
#     verifier: ./verifiers/repository-evolution
#     timeout: 50m
`

// scaffoldVerifier grades the scaffolded case for real. It is deliberately tiny and uses only the
// three exit codes, so it doubles as the worked example the README describes.
const scaffoldVerifier = `#!/bin/sh
# Grades the "greeting" case. Runs in a clean container with the candidate's finished
# workspace at /workspace — no model credentials, no network.
#
#   exit 0 = passed    exit 1 = did not pass    anything else = the GRADER broke

test -f /workspace/greeting.txt || {
	echo "greeting.txt was not created"
	exit 1
}
if [ "$(cat /workspace/greeting.txt)" != "hello" ]; then
	echo "greeting.txt does not contain exactly: hello"
	exit 1
fi
echo "greeting.txt is correct"
exit 0
`

const scaffoldVerifierNote = `# Independent verifier

This directory is the case's INDEPENDENT final grader. It is never mounted into the
evaluated model's workspace — it must not appear in the candidate's files, Git history,
images or tools. Coop runs it in a clean sandbox after the candidate has stopped.

## Write one

Add ` + "`verify.sh`" + ` here (or an executable ` + "`verify`" + `). It runs with the graded
workspace at /workspace and this directory at /coop-verifier, in a container with no
model credentials and no network.

    #!/bin/sh
    # exit 0 = passed, exit 1 = did not pass, anything else = the grader itself broke
    test -f /workspace/answer.txt || exit 1
    grep -q "hello" /workspace/answer.txt || exit 1

## The three exit codes matter

  0  the candidate met the case
  1  the candidate did not meet the case
  *  the GRADER broke (crash, missing dependency, killed) — recorded as a grading
     error, never as a model failure, so a broken verifier can never look like a
     regression

That last row is why a verifier should end in a check that exits 0 or 1, not in a
build tool with its own exit codes: ` + "`make test`" + ` exits 2 on a missing target and
` + "`cargo test`" + ` exits 101, both of which read as "the grader broke". Write
` + "`make test || exit 1`" + ` when you mean "this failing is a fail".

## What the grader sees

The workspace is an immutable SNAPSHOT taken after the candidate exited, including
its ` + "`.git`" + ` — so you can check whether it committed, and what. Writing into
/workspace is fine: it is a copy, and nothing the verifier does changes what was
recorded. Files the candidate left as symlinks pointing outside the workspace are
not carried into the snapshot.
`
