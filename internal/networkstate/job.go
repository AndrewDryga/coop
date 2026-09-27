package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/AndrewDryga/coop/internal/egress"
)

// JobSnapshotRef identifies controller authority after the worker has authenticated and
// journaled the immutable job. Neither field comes from repository content or a box.
type JobSnapshotRef struct {
	JobDigest string
	SessionID string
}

func (r JobSnapshotRef) Validate() error {
	if !lowerHex(r.JobDigest, 64) || !safeRecordToken(r.SessionID, 128) {
		return errors.New("invalid controller job network identity")
	}
	return nil
}

func (s *Store) jobScope(ref JobSnapshotRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	if err := s.authorityAvailable(); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("controller-job-v1\x00" + ref.JobDigest + "\x00" + ref.SessionID))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// CaptureJob publishes only controller-authored filtered rules plus release-owned provider
// dependencies. It intentionally does not read project approval, remembered posture or requests.
func (s *Store) CaptureJob(ref JobSnapshotRef, rules []egress.Rule, bundles []egress.Bundle, exportDestinations bool) (egress.Snapshot, error) {
	scope, err := s.jobScope(ref)
	if err != nil {
		return egress.Snapshot{}, err
	}
	if err := s.checkBundles(bundles); err != nil {
		return egress.Snapshot{}, err
	}
	snapshot, err := egress.Compile(scope, egress.Filtered,
		[]egress.Input{{Rules: rules, Origin: egress.Origin{Kind: "controller", Name: ref.JobDigest}}},
		bundles, exportDestinations, s.key)
	if err != nil {
		return egress.Snapshot{}, err
	}
	if err := s.saveSnapshot(snapshot); err != nil {
		return egress.Snapshot{}, err
	}
	return snapshot, nil
}

// LoadJobSnapshot proves that an exact session is reusing only its own captured authority.
func (s *Store) LoadJobSnapshot(ref JobSnapshotRef, fingerprint string) (egress.Snapshot, error) {
	scope, err := s.jobScope(ref)
	if err != nil {
		return egress.Snapshot{}, err
	}
	return s.loadSnapshotScope(scope, fingerprint)
}
