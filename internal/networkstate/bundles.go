package networkstate

import "github.com/AndrewDryga/coop/internal/egress"

// checkBundles validates the release-owned provider bundles a policy selects, their size included,
// before anything uses them. A bundle's content under its version is fixed by the release itself
// (agent.TestNetworkBundlesChangeOnlyWithANewVersion), so this host keeps no copy of it.
func (s *Store) checkBundles(bundles []egress.Bundle) error {
	if err := s.authorityAvailable(); err != nil {
		return err
	}
	_, err := egress.SelectedBundles(bundles)
	return err
}
