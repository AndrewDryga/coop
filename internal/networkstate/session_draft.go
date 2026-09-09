package networkstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/egress"
)

const maxSessionNetworkDraftBytes = 4 << 20

// SessionNetworkDraft is owner-private pre-pin custody. Each Plan is the box's
// closed primitive DTO; immutable InputsID references keep secret payloads out
// of the small SQLite operation intent. A draft never grants launch authority.
type SessionNetworkDraft struct {
	Version            int                         `json:"version"`
	ID                 string                      `json:"id"`
	Project            string                      `json:"project"`
	PolicyFingerprint  string                      `json:"policy_fingerprint"`
	ReferenceDigest    string                      `json:"reference_digest"`
	Mode               egress.Mode                 `json:"mode"`
	ExportDestinations bool                        `json:"export_destinations"`
	SessionID          string                      `json:"session_id"`
	OperationID        string                      `json:"operation_id"`
	ForkName           string                      `json:"fork_name"`
	AuthorityDigest    string                      `json:"authority_digest"`
	Exposed            []string                    `json:"exposed"`
	Members            []SessionNetworkDraftMember `json:"members"`
}

type SessionNetworkDraftMember struct {
	Key      string          `json:"key"`
	InputsID string          `json:"inputs_id"`
	Plan     json.RawMessage `json:"plan"`
}

func validateSessionNetworkDraftEnvelope(draft SessionNetworkDraft) error {
	// The generation does not exist until the pinned workspace is materialized.
	if validateSessionNetworkIdentity(draft.SessionID, draft.OperationID, draft.ForkName, draft.AuthorityDigest) != nil ||
		draft.Version != 1 || draft.ID != "" && !lowerHex(draft.ID, 64) ||
		!lowerHex(draft.PolicyFingerprint, 64) || !lowerHex(draft.ReferenceDigest, 64) || len(draft.Members) == 0 || len(draft.Members) > MaxCatalogMembers ||
		len(draft.Exposed) == 0 || len(draft.Exposed) > 32768 {
		return errors.New("invalid session network draft")
	}
	if _, err := egress.ParseMode(string(draft.Mode)); err != nil {
		return err
	}
	for _, path := range append([]string{draft.Project}, draft.Exposed...) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 16384 || !utf8.ValidString(path) || strings.ContainsRune(path, '\x00') {
			return errors.New("invalid session network draft exposure")
		}
	}
	seen := make(map[string]bool, len(draft.Members))
	for _, member := range draft.Members {
		if member.Key == "" || len(member.Key) > 256 || !utf8.ValidString(member.Key) ||
			strings.ContainsAny(member.Key, "\x00\r\n") || seen[member.Key] || !lowerHex(member.InputsID, 64) ||
			len(member.Plan) == 0 || len(member.Plan) > 1<<20 || !utf8.Valid(member.Plan) || !json.Valid(member.Plan) {
			return errors.New("invalid session network draft member")
		}
		seen[member.Key] = true
	}
	return nil
}

func (s *Store) sessionNetworkDraftID(draft SessionNetworkDraft) string {
	draft.ID = ""
	data, _ := json.Marshal(draft)
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("session-network-draft-v1\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) validateSessionNetworkDraft(draft SessionNetworkDraft) error {
	if err := validateSessionNetworkDraftEnvelope(draft); err != nil {
		return err
	}
	if err := s.CheckExposure(draft.Exposed); err != nil {
		return err
	}
	policy, err := s.LoadSnapshot(draft.Project, draft.PolicyFingerprint)
	if err != nil {
		return err
	}
	if policy.Mode != draft.Mode || policy.ExportDestinations != draft.ExportDestinations {
		return errors.New("session network draft differs from its captured policy")
	}
	for _, member := range draft.Members {
		if _, err := s.Inputs(member.InputsID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordSessionNetworkDraft(draft SessionNetworkDraft) (string, error) {
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	if draft.ID != "" {
		return "", errors.New("new session network draft cannot supply its own identity")
	}
	if err := s.validateSessionNetworkDraft(draft); err != nil {
		return "", err
	}
	draft.ID = s.sessionNetworkDraftID(draft)
	data, err := json.Marshal(draft)
	if err != nil || len(data) > maxSessionNetworkDraftBytes {
		return "", errors.New("session network draft exceeds its byte limit")
	}
	name := "session-draft-" + draft.ID + ".json"
	if err := s.publishBounded(name, data, false, 0o600, maxSessionNetworkDraftBytes); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		previous, err := s.read(name, maxSessionNetworkDraftBytes)
		if err != nil || !bytes.Equal(previous, data) {
			return "", errors.New("invalid stored session network draft")
		}
	}
	if err := s.confirmPublication(); err != nil {
		return "", err
	}
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	return draft.ID, nil
}

// SessionNetworkDraftEnvelope authenticates metadata without opening provider
// inputs. The lifecycle owner checks exact intent and full CURRENT exposure
// before LoadSessionNetworkDraft; old path strings alone cannot prove safety.
func (s *Store) SessionNetworkDraftEnvelope(id string) (SessionNetworkDraft, error) {
	if !lowerHex(id, 64) {
		return SessionNetworkDraft{}, errors.New("invalid session network draft reference")
	}
	if err := s.intactAuthority(); err != nil {
		return SessionNetworkDraft{}, err
	}
	data, err := s.read("session-draft-"+id+".json", maxSessionNetworkDraftBytes)
	if err != nil {
		return SessionNetworkDraft{}, err
	}
	var draft SessionNetworkDraft
	if err := strictJSON(data, &draft); err != nil {
		return SessionNetworkDraft{}, err
	}
	canonical, _ := json.Marshal(draft)
	if draft.ID != id || !bytes.Equal(data, canonical) || !hmac.Equal([]byte(s.sessionNetworkDraftID(draft)), []byte(id)) {
		return SessionNetworkDraft{}, errors.New("session network draft failed owner-bound integrity validation")
	}
	if err := validateSessionNetworkDraftEnvelope(draft); err != nil {
		return SessionNetworkDraft{}, err
	}
	return draft, s.intactAuthority()
}

func (s *Store) LoadSessionNetworkDraft(id string) (SessionNetworkDraft, error) {
	draft, err := s.SessionNetworkDraftEnvelope(id)
	if err != nil {
		return SessionNetworkDraft{}, err
	}
	if err := s.validateSessionNetworkDraft(draft); err != nil {
		return SessionNetworkDraft{}, err
	}
	return draft, s.intactAuthority()
}
