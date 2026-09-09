package networkstate

import (
	"errors"
	"path/filepath"
	"unicode"
	"unicode/utf8"
)

// SessionCatalogOwner is durable lifecycle custody, not the daemon process
// that happens to serve it. Only trusted session creation supplies these values.
type SessionCatalogOwner struct {
	SessionID, OperationID, ForkName, ForkGeneration, AuthorityDigest string
}

func validateSessionCatalogOwner(owner SessionCatalogOwner) error {
	if err := validateSessionNetworkIdentity(owner.SessionID, owner.OperationID, owner.ForkName, owner.AuthorityDigest); err != nil {
		return err
	}
	if !lowerHex(owner.ForkGeneration, 32) {
		return errors.New("invalid session network catalog generation")
	}
	return nil
}

func validateSessionNetworkIdentity(sessionID, operationID, forkName, authorityDigest string) error {
	for _, value := range []string{sessionID, operationID, forkName} {
		if value == "" || len(value) > 256 || !utf8.ValidString(value) || filepath.Base(value) != value || value == "." || value == ".." {
			return errors.New("invalid session network catalog owner")
		}
		for _, char := range value {
			if unicode.IsControl(char) || unicode.IsSpace(char) {
				return errors.New("invalid session network catalog owner")
			}
		}
	}
	if !lowerHex(authorityDigest, 64) {
		return errors.New("invalid session network catalog authority")
	}
	return nil
}

// SessionCatalog checks lifecycle identity before opening policy or provider
// inputs. The caller must first validate current workspace custody and exposure.
func (s *Store) SessionCatalog(id string, expected SessionCatalogOwner) (LaunchCatalog, error) {
	if err := validateSessionCatalogOwner(expected); err != nil {
		return LaunchCatalog{}, err
	}
	catalog, err := s.CatalogEnvelope(id)
	if err != nil {
		return LaunchCatalog{}, err
	}
	if catalog.Session == nil || *catalog.Session != expected {
		return LaunchCatalog{}, errors.New("network catalog belongs to another session owner")
	}
	if err := s.validateCatalog(catalog); err != nil {
		return LaunchCatalog{}, err
	}
	if err := s.confirmPublication(); err != nil {
		return LaunchCatalog{}, err
	}
	if err := s.intactAuthority(); err != nil {
		return LaunchCatalog{}, err
	}
	return catalog, nil
}
