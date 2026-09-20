package eval

import (
	"fmt"
	"os"
	"path/filepath"
)

// Scaffold writes a working custom suite into dir: a runnable agent example plus a documented loop
// example, and the directories the manifest references. It is create-only — the CLI proves the
// manifest does not already exist — so it never overwrites a user's work. The result loads and
// plans as-is (a placeholder verifier), so a user edits a working shape rather than a blank file.
func Scaffold(dir string) error {
	files := map[string]string{
		"suite.yaml":                   scaffoldManifest,
		"verifiers/greeting/README.md": scaffoldVerifierNote,
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

const scaffoldVerifierNote = `# Independent verifier

This directory is the case's INDEPENDENT final grader. It is never mounted into the
evaluated model's workspace — it must not appear in the candidate's files, Git history,
images or tools. v1 runs it in a clean sandbox after the candidate has stopped.

Milestone 1 ships the suite schema and planning only; wire your grader once trial
execution and grading land.
`
