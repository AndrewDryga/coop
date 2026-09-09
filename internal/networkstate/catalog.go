package networkstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/egress"
)

const MaxCatalogMembers = 128
const maxCatalogBytes = 4 << 20

// LaunchCatalog is private owner-authenticated replay state. The box validates
// the opaque prototype; lifecycle owners separately bind the catalog reference
// to an exact session intent or worker reservation. A catalog ID is not an API
// permission and must never be accepted as request-supplied authority.
type LaunchCatalog struct {
	Version         int                  `json:"version"`
	ID              string               `json:"id"`
	Project         string               `json:"project"`
	Fingerprint     string               `json:"fingerprint"`
	QualificationID string               `json:"qualification_id,omitempty"`
	CandidateID     string               `json:"candidate_id,omitempty"`
	Members         []CatalogMember      `json:"members"`
	Session         *SessionCatalogOwner `json:"session,omitempty"`
	ReferenceDigest string               `json:"reference_digest,omitempty"`
}

type CatalogMember struct {
	Key           string              `json:"key"`
	InputsID      string              `json:"inputs_id,omitempty"`
	Dependencies  []egress.Dependency `json:"dependencies"`
	MCPProjection string              `json:"mcp_projection"`
	Prototype     json.RawMessage     `json:"prototype"`
}

func (s *Store) catalogID(catalog LaunchCatalog) string {
	catalog.ID = ""
	data, _ := json.Marshal(catalog)
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("network-launch-catalog-v1\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) validateCatalog(catalog LaunchCatalog) error {
	if err := validateCatalogEnvelope(catalog); err != nil {
		return err
	}
	policy, err := s.LoadSnapshot(catalog.Project, catalog.Fingerprint)
	if err != nil {
		return err
	}
	var qualification Qualification
	if policy.Mode == egress.Filtered {
		qualification, err = s.Qualification(catalog.QualificationID)
		if err != nil {
			return err
		}
		if qualification.CandidateID != catalog.CandidateID {
			return errors.New("network catalog qualification belongs to another candidate")
		}
		if err := qualification.RequirePolicy(policy); err != nil {
			return err
		}
	} else if catalog.QualificationID != "" || catalog.CandidateID != "" {
		return errors.New("ordinary network catalog cannot carry filtered qualification")
	}
	for _, member := range catalog.Members {
		if policy.Mode == egress.Filtered {
			if _, err := s.Inputs(member.InputsID); err != nil {
				return err
			}
			if err := qualification.RequireSelection(policy, member.Dependencies, member.MCPProjection); err != nil {
				return err
			}
		} else {
			if len(member.Dependencies) != 0 || member.MCPProjection != "none" || catalog.Session == nil && member.InputsID != "" {
				return errors.New("ordinary network catalog cannot carry filtered provider dependencies")
			}
			if catalog.Session != nil {
				if _, err := s.Inputs(member.InputsID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateCatalogEnvelope(catalog LaunchCatalog) error {
	if catalog.Session == nil && catalog.ReferenceDigest != "" || catalog.Session != nil && !lowerHex(catalog.ReferenceDigest, 64) {
		return errors.New("network session catalog requires an exact launch reference")
	}
	if catalog.Session != nil {
		if err := validateSessionCatalogOwner(*catalog.Session); err != nil {
			return err
		}
	}
	if catalog.Version != 1 || !utf8.ValidString(catalog.Project) || len(catalog.Members) == 0 || len(catalog.Members) > MaxCatalogMembers {
		return errors.New("network catalog exceeds its member limit or has an unsupported version")
	}
	seen := map[string]bool{}
	for _, member := range catalog.Members {
		if member.Key == "" || len(member.Key) > 256 || !utf8.ValidString(member.Key) || strings.ContainsAny(member.Key, "\x00\r\n") || seen[member.Key] ||
			len(member.Prototype) == 0 || len(member.Prototype) > 1<<20 || !utf8.Valid(member.Prototype) || !json.Valid(member.Prototype) {
			return errors.New("invalid network catalog member")
		}
		seen[member.Key] = true
	}
	return nil
}

func (s *Store) RecordCatalog(catalog LaunchCatalog) (string, error) {
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	if catalog.ID != "" {
		return "", errors.New("new network catalog cannot supply its own identity")
	}
	data, err := json.Marshal(catalog)
	if err != nil || len(data)+64 > maxCatalogBytes {
		return "", errors.New("network catalog exceeds its byte limit")
	}
	if err := s.validateCatalog(catalog); err != nil {
		return "", err
	}
	catalog.ID = s.catalogID(catalog)
	data, _ = json.Marshal(catalog)
	name := "catalog-" + catalog.ID + ".json"
	if err := s.publishBounded(name, data, false, 0o600, maxCatalogBytes); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		previous, err := s.read(name, maxCatalogBytes)
		if err != nil || !bytes.Equal(previous, data) {
			return "", errors.New("invalid stored network catalog")
		}
	}
	if err := s.confirmPublication(); err != nil {
		return "", err
	}
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	return catalog.ID, nil
}

func (s *Store) Catalog(id string) (LaunchCatalog, error) {
	catalog, err := s.CatalogEnvelope(id)
	if err != nil {
		return LaunchCatalog{}, err
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

// CatalogEnvelope authenticates launch metadata without opening captured native
// inputs. Reexec owners must check the full current exposure union before Catalog.
func (s *Store) CatalogEnvelope(id string) (LaunchCatalog, error) {
	if err := s.intactAuthority(); err != nil {
		return LaunchCatalog{}, err
	}
	catalog, err := s.readCatalogEnvelope(id)
	if err != nil {
		return LaunchCatalog{}, err
	}
	if !hmac.Equal([]byte(s.catalogID(catalog)), []byte(id)) {
		return LaunchCatalog{}, errors.New("network catalog failed owner-bound integrity validation")
	}
	if err := s.intactAuthority(); err != nil {
		return LaunchCatalog{}, err
	}
	return catalog, nil
}

// CatalogForCleanup reads private lifecycle metadata without the launch key or
// captured inputs. It is not an authenticated launch capability. Callers must
// bind it to their separately retained session reference before cleanup.
func (e *Evidence) CatalogForCleanup(id string) (LaunchCatalog, error) {
	return e.files.readCatalogEnvelope(id)
}

func (s *Store) readCatalogEnvelope(id string) (LaunchCatalog, error) {
	if !lowerHex(id, 64) {
		return LaunchCatalog{}, errors.New("invalid network catalog reference")
	}
	data, err := s.read("catalog-"+id+".json", maxCatalogBytes)
	if err != nil {
		return LaunchCatalog{}, err
	}
	var catalog LaunchCatalog
	if err := strictJSON(data, &catalog); err != nil {
		return LaunchCatalog{}, err
	}
	canonical, _ := json.Marshal(catalog)
	if catalog.ID != id || !bytes.Equal(data, canonical) {
		return LaunchCatalog{}, errors.New("network catalog failed owner-bound integrity validation")
	}
	if err := validateCatalogEnvelope(catalog); err != nil {
		return LaunchCatalog{}, err
	}
	return catalog, nil
}
