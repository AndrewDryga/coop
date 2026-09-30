package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// External profile trees get their own rooted opener; ordinary suite paths still cannot escape
// the suite root. Canonical names also match the host's explicit build-approval authority.
func openProfileDirectory(path string) (string, *openedSuiteTree, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", nil, err
	}
	volume := filepath.VolumeName(canonical) + string(filepath.Separator)
	base, err := os.OpenRoot(volume)
	if err != nil {
		return "", nil, err
	}
	defer base.Close()
	tree, err := openSuiteTree(base, strings.TrimPrefix(canonical, volume))
	return canonical, tree, err
}

func stageRuntimeProfiles(ctx context.Context, stateRoot string, source, staged *Suite, remaining *int64, entries *int) error {
	// Do not open new authority or change ordinary suites when no case requests a profile.
	requested := false
	for _, c := range source.Cases {
		requested = requested || c.Runtime != nil
	}
	if !requested {
		return nil
	}
	_, suiteTree, err := openProfileDirectory(source.Dir)
	if err != nil {
		return err
	}
	defer suiteTree.root.Close()
	_, stateTree, err := openProfileDirectory(stateRoot)
	if err != nil {
		return err
	}
	defer stateTree.root.Close()
	profiles := map[string]*CaseRuntime{}
	var trees []*openedSuiteTree
	defer func() {
		for _, tree := range trees {
			_ = tree.root.Close()
		}
	}()
	for i, c := range source.Cases {
		if c.Runtime == nil {
			continue
		}
		copy := *c.Runtime
		r := &copy
		staged.Cases[i].Runtime = r
		canonical, tree, err := openProfileDirectory(r.Profile)
		if err != nil {
			return fmt.Errorf("case %q runtime profile: %w", c.ID, err)
		}
		trees = append(trees, tree)
		if suiteTreesOverlap(tree, suiteTree) || suiteTreesOverlap(tree, stateTree) {
			return fmt.Errorf("case %q runtime profile overlaps suite or eval state", c.ID)
		}
		r.Profile = canonical
		if previous := profiles[canonical]; previous != nil {
			*r = *previous
			// Per-case phase budgets are not properties of the shared build inputs.
			r.AgentTimeout, r.VerifierTimeout, r.Workdir = c.Runtime.AgentTimeout, c.Runtime.VerifierTimeout, c.Runtime.Workdir
			continue
		}
		for _, previous := range trees[:len(trees)-1] {
			if suiteTreesOverlap(tree, previous) {
				return fmt.Errorf("case %q runtime profile overlaps another profile", c.ID)
			}
		}
		r.RetainedProfile = filepath.Join("profiles", fmt.Sprintf("profile-%04d", len(profiles)+1))
		dest := filepath.Join(staged.Dir, r.RetainedProfile)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		r.ProfileDigest, err = stageOpenedTreeWithPolicy(ctx, tree.root, dest, remaining, entries, true)
		if err != nil {
			return fmt.Errorf("case %q runtime profile: %w", c.ID, err)
		}
		r.sourceIdentity = tree.chain[len(tree.chain)-1]
		profiles[canonical] = r
	}
	for _, r := range profiles {
		if err := r.VerifyProfile(ctx); err != nil {
			return err
		}
	}
	return nil
}

// VerifyProfile revalidates original host authority, not the retained copy. A retained export
// cannot authorize a build or hide a changed/deleted original profile.
func (r *CaseRuntime) VerifyProfile(ctx context.Context) error {
	if r == nil || r.sourceIdentity == nil || r.ProfileDigest == "" {
		return fmt.Errorf("runtime profile has not been staged")
	}
	canonical, tree, err := openProfileDirectory(r.Profile)
	if err != nil {
		return fmt.Errorf("runtime profile is unavailable: %w", err)
	}
	defer tree.root.Close()
	if canonical != r.Profile || !os.SameFile(r.sourceIdentity, tree.chain[len(tree.chain)-1]) {
		return fmt.Errorf("runtime profile changed since staging")
	}
	digest, err := openedTreeDigestWithPolicy(ctx, tree.root, true)
	if err != nil {
		return err
	}
	_, named, err := openProfileDirectory(r.Profile)
	if err != nil {
		return err
	}
	defer named.root.Close()
	if digest != r.ProfileDigest || !os.SameFile(r.sourceIdentity, named.chain[len(named.chain)-1]) {
		return fmt.Errorf("runtime profile changed since staging")
	}
	return nil
}
