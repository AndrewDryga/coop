package forkctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/gitdiff"
	"github.com/AndrewDryga/coop/internal/hostsurface"
	"github.com/AndrewDryga/coop/internal/ui"
)

type isolatedCandidate struct {
	dir        string
	identity   forkspace.Identity
	parent     isolatedParent
	sourceHead string
	sourceTree string
	upstream   string
	head       string
	tree       string
}

func isolatedForkIdentity(repo, name string) (forkspace.Identity, bool, error) {
	identity, exists, err := forkspace.ReadGeneration(repo, name)
	if err != nil || !exists {
		return identity, false, err
	}
	isIsolated, err := forkspace.IsolatedGeneration(repo, identity)
	return identity, isIsolated, err
}

func (candidate isolatedCandidate) cleanup() {
	if candidate.dir != "" {
		_ = os.RemoveAll(candidate.dir)
	}
}

// Custom tools need published files without changing the base comparison's
// HEAD...ref contract. Both copies are retained because GUI tools can return early.
func (candidate isolatedCandidate) committedPreview(repo string) (_ string, err error) {
	preview, err := mkdirStateTemp(repo, forkspace.IsolatedRecordPrefix("preview", candidate.identity)+"-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(preview)
		}
	}()
	ctx := context.Background()
	if err = forkspace.GitClonePinnedContext(ctx, candidate.dir, preview, candidate.head); err != nil {
		return "", err
	}
	if err = forkspace.CheckoutIndependent(ctx, preview, candidate.head, ""); err != nil {
		return "", err
	}
	if err = forkspace.MaterializeIndependentTree(ctx, repo, preview, candidate.parent.Head, candidate.head, candidate.dir); err != nil {
		return "", err
	}
	return preview, nil
}

// Lifecycle admission excludes registered model writers. Native outside writers
// remain outside the boundary, so exact source identity is checked again afterward.
func prepareIsolatedCandidate(repo, name string, identity forkspace.Identity, sign bool) (_ isolatedCandidate, err error) {
	return captureIsolatedCandidate(repo, name, identity, sign, false)
}

func captureIsolatedCandidate(repo, name string, identity forkspace.Identity, sign, review bool) (_ isolatedCandidate, err error) {
	ws := forkspace.Workspace(repo, name)
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return isolatedCandidate{}, err
	}
	if err := forkspace.ValidateIndependentGit(ws); err != nil {
		return isolatedCandidate{}, err
	}
	var parent isolatedParent
	if review {
		parent, err = captureIsolatedReviewBase(repo)
	} else {
		parent, err = captureIsolatedParent(repo)
	}
	if err != nil {
		if block := parentRefusal(err, false); block != nil {
			return isolatedCandidate{}, block
		}
		return isolatedCandidate{}, err
	}
	sourceHead, err := observed(ws, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	sourceTree, err := observed(ws, "rev-parse", "--verify", sourceHead+"^{tree}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	upstream, published, err := isolatedSourceBoundary(repo, identity, parent.Head)
	if err != nil {
		return isolatedCandidate{}, err
	}
	if _, err := observed(ws, "merge-base", "--is-ancestor", upstream, sourceHead); err != nil {
		return isolatedCandidate{}, fmt.Errorf("isolated fork no longer descends from its published source boundary: %w", err)
	}
	dir, err := mkdirStateTemp(repo, forkspace.IsolatedRecordPrefix("candidate", identity)+"-")
	if err != nil {
		return isolatedCandidate{}, err
	}
	candidate := isolatedCandidate{dir: dir, identity: identity, parent: parent,
		sourceHead: sourceHead, sourceTree: sourceTree, upstream: upstream}
	defer func() {
		if err != nil {
			candidate.cleanup()
		}
	}()
	ctx := context.Background()
	if err = forkspace.GitClonePinnedContext(ctx, repo, dir, parent.Head); err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.PropagateGitIdentity(repo, dir); err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.GitFetchPinnedContext(ctx, ws, dir, sourceHead); err != nil {
		return isolatedCandidate{}, err
	}
	if err = validateIsolatedHistory(dir, repo, upstream, sourceHead); err != nil {
		return isolatedCandidate{}, err
	}
	// Rebase skips a source commit the parent already has, such as one a person cherry-picked, only
	// when it compares that commit with the parent's own commits since the last publication, as an
	// ordinary fork's rebase does from its merge base. An isolated source shares no merge base with
	// the parent's copies of it, so two throwaway merges stand in: the upstream joins the published
	// boundary and the parent HEAD, so published source is never replayed, and the rebased branch
	// joins the source and the parent commit that published it, so those copies are never compared
	// and a source commit that redoes a change the parent reverted is kept. Rebase replays no merge.
	base, err := isolatedJoin(ctx, dir, parent.Tree, upstream, parent.Head)
	if err != nil {
		return isolatedCandidate{}, err
	}
	branch, err := isolatedJoin(ctx, dir, sourceTree, sourceHead, published)
	if err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.CheckoutIndependent(ctx, dir, branch, "candidate"); err != nil {
		return isolatedCandidate{}, err
	}
	args := []string{"rebase", "--onto", parent.Head, base, "candidate"}
	if sign {
		// -f: a pick whose parent is already HEAD is otherwise kept as it was, unsigned.
		args = append(forkspace.TrustedSignArgs(), "rebase", "-f", "--gpg-sign", "--onto", parent.Head, base, "candidate")
	}
	command, commandErr := forkspace.GitCommandWithEnv(ctx, dir, forkspace.IndependentGitEnv(), args...)
	if commandErr != nil {
		return isolatedCandidate{}, commandErr
	}
	if output, rebaseErr := command.CombinedOutput(); rebaseErr != nil {
		if files := isolatedConflictFiles(ctx, dir); len(files) > 0 {
			return isolatedCandidate{}, &isolatedConflictError{name: name, branch: gitBranch(repo), files: files}
		}
		return isolatedCandidate{}, fmt.Errorf("isolated private rebase failed; source and parent remain unchanged: %w\n"+
			"Retained source: %s\nPublished source boundary: %s\n"+
			"Review and rework only unpublished source commits after that boundary; preserve its ancestry. "+
			"Check trusted signing settings if signing failed. Retry `coop fork review %s` before merging.\n"+
			"Diagnostics below are from a discarded private clone; do not apply its recovery instructions to the retained source:\n%s",
			rebaseErr, ws, upstream, name, output)
	}
	candidate.head, err = observed(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	candidate.tree, err = observed(dir, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	if err = validateIsolatedHistory(dir, repo, parent.Head, candidate.head); err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.MaterializeIndependentTree(ctx, repo, dir, parent.Head, candidate.head, ws); err != nil {
		return isolatedCandidate{}, err
	}
	if err = candidate.validateSource(repo); err != nil {
		return isolatedCandidate{}, err
	}
	return candidate, nil
}

// isolatedJoin is a throwaway merge commit of tree with parents first and second, or first when
// they are one commit: a commit with that parent twice would be an empty commit the rebase keeps.
func isolatedJoin(ctx context.Context, dir, tree, first, second string) (string, error) {
	if first == second {
		return first, nil
	}
	join, err := forkspace.GitCommandWithEnv(ctx, dir, forkspace.IndependentGitEnv(),
		"commit-tree", "-p", first, "-p", second, "-m", "Coop rebase join", tree)
	if err != nil {
		return "", err
	}
	out, err := join.Output()
	commit := strings.TrimSpace(string(out))
	if err != nil || !forkspace.ValidObjectID(commit) {
		return "", errors.Join(errors.New("prepare the isolated private rebase"), err)
	}
	return commit, nil
}

func (candidate isolatedCandidate) validateSource(repo string) error {
	if err := forkspace.ValidateGenerationWorkspace(repo, candidate.identity); err != nil {
		return err
	}
	ws := forkspace.Workspace(repo, candidate.identity.Name)
	if err := forkspace.ValidateIndependentGit(ws); err != nil {
		return err
	}
	head, err := observed(ws, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head != candidate.sourceHead {
		return errors.Join(errors.New("isolated source no longer matches the captured review; preserve any publication journal/custody"), err)
	}
	return nil
}

// isolatedFileLimit is the largest file an isolated publication reads and judges.
const isolatedFileLimit = 5 << 20

const changeInYourCheckout = "Remove it from the fork's commits and make the change in your own checkout."

// isolatedRefusal answers a commit of an isolated fork that Coop will not merge, in the block the
// other merge refusals use: what it refuses and why, that --force cannot change it, and what to do.
// isolatedConflictFiles is the paths a stopped private rebase in dir left unmerged, none when it
// stopped for another reason, such as signing.
func isolatedConflictFiles(ctx context.Context, dir string) []string {
	command, err := forkspace.GitCommandWithEnv(ctx, dir, forkspace.IndependentGitEnv(), "ls-files", "--unmerged", "-z")
	if err != nil {
		return nil
	}
	out, err := command.Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if _, path, ok := strings.Cut(entry, "\t"); ok && !seen[path] {
			seen[path] = true
			files = append(files, path)
		}
	}
	return files
}

// isolatedConflictError is the refusal of an isolated fork whose reviewed commits conflict with the
// parent branch. Nothing of the parent may enter an isolated box, so no agent can rebase them; the
// two ways out are a person's.
type isolatedConflictError struct {
	name, branch string
	files        []string
}

// Unwrap is the refusal itself, so it prints as every other refusal does.
func (e *isolatedConflictError) Unwrap() error { return e.refusal() }

func (e *isolatedConflictError) Error() string { return e.refusal().Error() }

func (e *isolatedConflictError) refusal() error {
	files := slices.Clone(e.files)
	more := ""
	if len(files) > 5 {
		files, more = files[:5], " and more"
	}
	for i := range files {
		files[i] = ui.SafeInline(files[i])
	}
	branch := parentBranchWords(e.branch)
	return ui.CommandFailed("Fork "+e.name+"'s reviewed commits conflict with "+branch,
		"They conflict in "+strings.Join(files, ", ")+more+", so nothing merged. It is an isolated fork: it holds the repository's branches only as they were when it was created, and nothing of "+
			branch+" may enter its box later, so no agent can rebase it.",
		[2]string{"Change " + branch + " until the fork's commits apply, then merge again:", "coop fork merge " + e.name},
		[2]string{"Or discard its work and start a new isolated fork from " + branch + ":", "coop fork rm " + e.name + " --force"})
}

func isolatedRefusal(headline, why, action string) *ui.UsageError {
	return ui.CommandFailed(headline, why+"\n--force cannot bypass this check.", [2]string{"", action})
}

func nestedSourcesRefusal(path string) *ui.UsageError {
	return isolatedRefusal(fmt.Sprintf("The fork changes %q, which Coop will not merge", path),
		"Coop does not merge changes to submodules or nested Git repositories, because they decide what git submodule update fetches.", changeInYourCheckout)
}

// Scan every reachable incoming commit, including add-then-remove attacks. A
// terminal diff cannot grant permission to import unsafe intermediate history.
func validateIsolatedHistory(repo, authority, upstream, head string) error {
	shadowed := box.NewShadowDecider(authority)
	surfaces := configuredSurfaces(authority)
	// One rev-list names every incoming commit with its parents, and each commit's tree is listed once
	// with its sizes: a commit whose first parent is the commit before it compares against the tree
	// already listed. File contents come through one cat-file, started at the first file read.
	history, err := observed(repo, "rev-list", "--reverse", "--parents", upstream+".."+head)
	if err != nil {
		return err
	}
	var blobs *forkspace.BlobReader
	defer func() {
		if blobs != nil {
			_ = blobs.Close()
		}
	}()
	// read is the content of a blob of at most isolatedFileLimit bytes, less one final newline: the
	// one-shot read this replaces trimmed it, and the scan judges the same text it did.
	read := func(object string) (string, error) {
		if blobs == nil {
			var err error
			if blobs, err = forkspace.ObserveBlobs(context.Background(), repo); err != nil {
				return "", err
			}
		}
		data, err := blobs.Read(object, isolatedFileLimit)
		return strings.TrimSuffix(string(data), "\n"), err
	}
	var previousCommit string
	var previousTree []isolatedTreeEntry
	for _, line := range strings.Split(history, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		commit := fields[0]
		after, err := isolatedSizedTree(repo, commit)
		if err != nil {
			return err
		}
		if len(fields) < 2 {
			return errors.New("isolated incoming history has no qualified parent")
		}
		before := previousTree
		if fields[1] != previousCommit {
			if before, err = isolatedSizedTree(repo, fields[1]); err != nil {
				return err
			}
		}
		previousCommit, previousTree = commit, after
		old := map[string]isolatedTreeEntry{}
		for _, entry := range before {
			old[entry.path] = entry
		}
		for _, entry := range after {
			previous, present := old[entry.path]
			delete(old, entry.path)
			if present && previous == entry {
				continue
			}
			if entry.mode == "160000" || previous.mode == "160000" || filepath.Base(hostsurface.Key(entry.path)) == ".gitmodules" {
				return nestedSourcesRefusal(entry.path)
			}
			if reason, automatic := surfaces.Classify("M", entry.path); automatic {
				return isolatedRefusal(fmt.Sprintf("The fork changes %q, which Coop will not merge", entry.path), reason, changeInYourCheckout)
			}
			if err := isolatedPolicyPath(authority, entry.path); err != nil {
				return err
			}
			hidden, err := shadowed(hostsurface.Key(entry.path))
			if err != nil {
				return isolatedRefusal(fmt.Sprintf("Coop cannot check the fork's change to %q", entry.path), err.Error(),
					"Remove it if you did not create it, or make it a readable regular file of at most 1 MiB, then merge again.")
			}
			if hidden {
				return isolatedRefusal(fmt.Sprintf("The fork adds %q, a file Coop hides from boxes", entry.path),
					"Its name matches a secret or a .coopignore rule, and Coop will not merge a secret from an isolated fork.",
					"Remove it from the fork's commits.")
			}
			if entry.size < 0 {
				return fmt.Errorf("read the size of %q from the fork", entry.path)
			}
			if entry.size > isolatedFileLimit {
				return isolatedRefusal(fmt.Sprintf("The fork adds %q, a file over 5 MiB", entry.path),
					"Coop reads every file it merges from an isolated fork, and 5 MiB is the most it reads.",
					"Keep it out of the fork's commits and add it in your own checkout.")
			}
			content, err := read(entry.object)
			if err != nil {
				return err
			}
			if filepath.Base(hostsurface.Key(entry.path)) == "package.json" {
				if entry.mode == "120000" || !json.Valid([]byte(content)) {
					return isolatedRefusal(fmt.Sprintf("Coop cannot read the scripts in %q", entry.path),
						"It is a link or is not valid JSON, so Coop cannot tell whether npm would run new commands on install.",
						"Make it a regular, valid package.json, or leave it out of the fork's commits.")
				}
				oldContent := ""
				if present && previous.mode != "120000" {
					if previous.size < 0 || previous.size > isolatedFileLimit {
						return errors.New("previous package lifecycle scripts are uninspectable")
					}
					var err error
					if oldContent, err = read(previous.object); err != nil {
						return err
					}
				}
				if script := changedLifecycleScript(oldContent, content); script != "" {
					return isolatedRefusal(fmt.Sprintf("The fork adds or changes the %s script in %q", script, entry.path),
						"npm runs it by itself when someone runs npm install.", changeInYourCheckout)
				}
			}
			if strings.IndexByte(content, 0) < 0 {
				if adds, err := commitAddsCredential(repo, previous, present, entry, content); err != nil {
					return err
				} else if adds {
					return isolatedRefusal(fmt.Sprintf("The fork adds what looks like a credential to %q", entry.path),
						"Coop will not merge a possible secret from an isolated fork. Deleting it in a later commit does not help, because Coop reads every commit.",
						"Rewrite the fork's commits so none adds it, then merge again.")
				}
			}
		}
		for _, entry := range old {
			if entry.mode == "160000" || filepath.Base(hostsurface.Key(entry.path)) == ".gitmodules" {
				return nestedSourcesRefusal(entry.path)
			}
			if box.IsCoopignoreName(filepath.Base(hostsurface.Key(entry.path))) {
				return coopignoreRefusal(entry.path)
			}
		}
	}
	return nil
}

// isolatedPolicyPath refuses the paths an ordinary merge holds for --force (PolicyScan), which an
// isolated merge never forces: a .coopignore, which decides what every later box is shown, and a
// task in the host's queue, which a later loop would run as ready work.
func isolatedPolicyPath(authority, path string) error {
	if box.IsCoopignoreName(filepath.Base(hostsurface.Key(path))) {
		return coopignoreRefusal(path)
	}
	if queue := forkTaskQueuePath(authority, path); queue != "" {
		return isolatedRefusal(fmt.Sprintf("The fork adds or changes the task %q", path),
			"Tasks come from your "+queue+" queue, never from a fork: a later loop would run this one as ready work.",
			"Remove it from the fork's commits and add the task in your own checkout.")
	}
	return nil
}

func coopignoreRefusal(path string) error {
	return isolatedRefusal(fmt.Sprintf("The fork changes %q, which Coop will not merge", path),
		"A .coopignore decides what every later box is shown.", changeInYourCheckout)
}

// commitAddsCredential reports whether a finding in content, the commit's version of entry, sits on
// a line the commit adds: any line of a file it adds, else a line its diff from previous adds. A
// credential the previous version held is the parent's, or an earlier incoming commit added it and
// was judged for it.
func commitAddsCredential(repo string, previous isolatedTreeEntry, present bool, entry isolatedTreeEntry, content string) (bool, error) {
	findings := box.ScanSecrets(content)
	if len(findings) == 0 {
		return false, nil
	}
	added := gitdiff.LineRanges{{1, math.MaxInt}}
	if present {
		patch, err := observed(repo, "diff", "-U0", "--no-color", previous.object, entry.object)
		if err == nil {
			added, err = gitdiff.AddedLines(patch)
		}
		if err != nil {
			return false, fmt.Errorf("isolated publication cannot read the changes to %q: %w", entry.path, err)
		}
	}
	for _, finding := range findings {
		if added.Contains(finding.Line) {
			return true, nil
		}
	}
	return false, nil
}

func (c *Control) gateIsolatedCandidate(repo, img string, candidate isolatedCandidate, review bool) error {
	if img == "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "coop-isolated-gate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	if err := forkspace.GitClonePinnedContext(ctx, candidate.dir, dir, candidate.head); err != nil {
		return err
	}
	if err := forkspace.CheckoutIndependent(ctx, dir, candidate.head, "candidate"); err != nil {
		return err
	}
	if err := forkspace.MaterializeIndependentTree(ctx, repo, dir, candidate.parent.Head, candidate.head, candidate.dir); err != nil {
		return err
	}
	base := forkspace.GitRefCommand(ctx, dir, "update-ref", "refs/coop/session-parent", candidate.parent.Head)
	base.Env = forkspace.IndependentGitEnv()
	if err := base.Run(); err != nil {
		return err
	}
	var green bool
	if c.gateOK != nil {
		green = c.gateOK(repo, dir, img)
	} else {
		green, err = c.runGateBoundary(repo, dir, img, review, repo)
	}
	if err != nil || !green {
		return errors.Join(errors.New("isolated candidate gate failed; parent and model source unchanged"), err)
	}
	state, err := captureIsolatedParent(dir)
	if err != nil || state.Head != candidate.head || state.Tree != candidate.tree {
		return errors.Join(errors.New("isolated gate changed the checked candidate; approval refused"), err)
	}
	return candidate.validateSource(repo)
}

// mkdirStateTemp makes a new owner-only folder in the fork records through that folder's own
// handle, never by name, and returns its path.
func mkdirStateTemp(repo, prefix string) (string, error) {
	state, err := forkspace.OpenStateRoot(repo, "", true)
	if err != nil {
		return "", err
	}
	defer state.Close()
	name, err := forkspace.MkdirTempIn(state, prefix)
	if err != nil {
		return "", err
	}
	return filepath.Join(forkspace.StateDir(repo), name), nil
}
