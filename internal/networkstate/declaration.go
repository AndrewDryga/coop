package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/egress"
)

// DeclaredNetworkFingerprint binds the operator declaration without reading
// credential-derived bundles. It is not the later effective Snapshot fingerprint.
// The caller must prove complete prospective exposure before opening the Store.
func (s *Store) DeclaredNetworkFingerprint(project string, mode egress.Mode, rules []egress.Rule, exportDestinations bool) (string, error) {
	if err := s.authorityAvailable(); err != nil {
		return "", err
	}
	if _, err := egress.ParseMode(string(mode)); err != nil {
		return "", err
	}
	for _, rule := range rules {
		values := []string{rule.To.Domain, rule.To.IP, rule.To.CIDR, rule.To.Provider, rule.Protocol}
		values = append(values, rule.To.Features...)
		values = append(values, rule.Types...)
		for _, value := range values {
			if !utf8.ValidString(value) {
				return "", errors.New("declared network contains invalid UTF-8")
			}
		}
	}
	normalized, err := egress.NormalizeRules(rules)
	if err != nil {
		return "", err
	}
	if mode != egress.Filtered && len(normalized) != 0 {
		return "", errors.New("egress rules require filtered mode")
	}
	projectID, err := s.projectID(project)
	if err != nil {
		return "", err
	}
	envelope := struct {
		Version            int           `json:"version"`
		ProjectID          string        `json:"project_id"`
		Mode               egress.Mode   `json:"mode"`
		Rules              []egress.Rule `json:"rules"`
		ExportDestinations bool          `json:"export_destinations"`
	}{1, projectID, mode, normalized, exportDestinations}
	data, err := json.Marshal(envelope)
	if err != nil || len(data) > egress.MaxDocumentBytes {
		return "", errors.New("declared network envelope exceeds its byte limit")
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("declared-network-v1\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), s.intactAuthority()
}
