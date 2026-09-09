package networkstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"unicode/utf8"
)

// Resume carries editor request/session metadata, not the owner's small launch
// configuration frame. Bound this single payload separately from stream volume.
const maxACPResumeBytes = 64 << 20

func (s *Store) acpResumeID(data []byte) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("network-acp-resume-v1\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) RecordACPResume(data []byte) (string, error) {
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > maxACPResumeBytes || !utf8.Valid(data) || !json.Valid(data) {
		return "", errors.New("invalid or oversized private ACP resume frame")
	}
	id := s.acpResumeID(data)
	name := "acp-resume-" + id + ".json"
	if err := s.publishBounded(name, data, false, 0o600, maxACPResumeBytes); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		previous, err := s.read(name, maxACPResumeBytes)
		if err != nil || !bytes.Equal(previous, data) {
			return "", errors.New("private ACP resume frame changed")
		}
	}
	if err := s.confirmPublication(); err != nil {
		return "", err
	}
	return id, s.intactAuthority()
}

func (s *Store) ACPResume(id string) ([]byte, error) {
	if !lowerHex(id, 64) {
		return nil, errors.New("invalid private ACP resume reference")
	}
	if err := s.intactAuthority(); err != nil {
		return nil, err
	}
	data, err := s.read("acp-resume-"+id+".json", maxACPResumeBytes)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(id), []byte(s.acpResumeID(data))) || !utf8.Valid(data) || !json.Valid(data) {
		return nil, errors.New("private ACP resume frame failed integrity validation")
	}
	return data, s.intactAuthority()
}

// ConsumeACPReload creates a durable one-use marker. Same-PID self-exec alone
// cannot distinguish an old snapshot replay. An ambiguous publication refuses;
// it must never be treated as permission to apply the snapshot again.
func (s *Store) ConsumeACPReload(locator, id string) error {
	h, err := s.CatalogHandoff(locator)
	if err != nil {
		return err
	}
	if h.Kind != HandoffACPReload || h.ID != id {
		return errors.New("ACP reload handoff identity changed")
	}
	if _, err := s.ACPResume(h.ACPReload.ResumeID); err != nil {
		return err
	}
	if err := s.publishBounded("consumed-acp-reload-"+locator, []byte(id), false, 0o600, 64); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("ACP reload handoff was already consumed")
		}
		return err
	}
	return s.intactAuthority()
}
