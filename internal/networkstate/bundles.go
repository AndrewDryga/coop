package networkstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"

	"github.com/AndrewDryga/coop/internal/egress"
)

// CheckBundles pins canonical content under a provider/client/backend/auth/version identity.
// A new trusted version may serve new captures. Changed content under an existing
// version is integrity drift, not an update or a request the operator can approve.
func (s *Store) CheckBundles(bundles []egress.Bundle) error {
	if err := s.authorityAvailable(); err != nil {
		return err
	}
	selected, err := egress.SelectedBundles(bundles)
	if err != nil {
		return err
	}
	for _, bundle := range selected {
		data, err := json.Marshal(bundle)
		if err != nil || len(data) > egress.MaxDocumentBytes {
			return errors.New("provider bundle exceeds byte limit")
		}
		identity, _ := json.Marshal(bundle.Dependency())
		mac := hmac.New(sha256.New, s.key)
		_, _ = mac.Write(identity)
		name := "bundle-" + hex.EncodeToString(mac.Sum(nil)) + ".json"
		if err := s.publish(name, data, false); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			existing, err := s.read(name, egress.MaxDocumentBytes)
			if err != nil || !bytes.Equal(existing, data) {
				return errors.New("provider bundle integrity drift: content changed without a new version")
			}
			if err := s.confirmPublication(); err != nil {
				return err
			}
		}
	}
	return nil
}
