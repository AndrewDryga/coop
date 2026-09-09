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

	"github.com/AndrewDryga/coop/internal/processidentity"
)

const maxCatalogHandoffBytes = 4 << 20

// CatalogHandoff is private reexec state, never a request DTO. Its random
// locator exists before the child starts; its authenticated identity also binds
// the exact child observed after Start. Lifecycle owners validate that binding.
type CatalogHandoff struct {
	Version       int                     `json:"version"`
	Locator       string                  `json:"locator"`
	ID            string                  `json:"id"`
	CatalogID     string                  `json:"catalog_id"`
	Project       string                  `json:"project"`
	Workspace     string                  `json:"workspace"`
	Runtime       string                  `json:"runtime"`
	Exposed       []string                `json:"exposed"`
	Owner         json.RawMessage         `json:"owner"`
	Kind          string                  `json:"kind"`
	WorkspaceFork *HandoffForkIdentity    `json:"workspace_fork,omitempty"`
	ForkWorker    *ForkWorkerHandoff      `json:"fork_worker,omitempty"`
	ACPChild      *ACPChildHandoff        `json:"acp_child,omitempty"`
	ACPReload     *ACPReloadHandoff       `json:"acp_reload,omitempty"`
	SessionChild  *SessionACPChildHandoff `json:"session_child,omitempty"`
}

const (
	HandoffForkWorker = "fork-worker"
	HandoffACPChild   = "acp-child"
	HandoffACPReload  = "acp-reload"
	HandoffSessionACP = "session-acp-child"
)

type HandoffForkIdentity struct{ Name, Generation string }
type ForkWorkerHandoff struct {
	ReservationDigest string
	Child             Supervisor
}
type ACPChildHandoff struct {
	SupervisorID, MemberKey  string
	Supervisor, Child, Group Supervisor
	ContainerID              *ContainerIDCapture
}

// The process gate proves who is executing; this binds what session and
// immutable policy that child may execute. The run ID is activity identity,
// separate from the random network execution ID registered before runtime work.
type SessionACPChildHandoff struct {
	Owner             SessionCatalogOwner
	PolicyFingerprint string
	RunID             string
}

// ContainerIDCapture binds a private per-child output, not a runtime argument
// supplied by a request. Only an authenticated ACP handoff may restore it.
type ContainerIDCapture struct {
	Directory     string
	Device, Inode uint64
}
type ACPReloadHandoff struct {
	SupervisorID, ResumeID string
	Supervisor             Supervisor
}

func validHandoffProcess(p Supervisor) bool {
	return p.PID > 1 && len(p.StartToken) <= 256 && processidentity.Stable(p.StartToken) &&
		utf8.ValidString(p.StartToken) && !strings.ContainsAny(p.StartToken, "\x00\r\n")
}

func validateCatalogHandoff(h CatalogHandoff) error {
	if (h.Kind == HandoffSessionACP) != (h.SessionChild != nil) {
		return errors.New("network handoff session custody differs from its kind")
	}
	if h.SessionChild != nil {
		c := h.SessionChild
		if validateSessionCatalogOwner(c.Owner) != nil || h.WorkspaceFork == nil ||
			h.WorkspaceFork.Name != c.Owner.ForkName || h.WorkspaceFork.Generation != c.Owner.ForkGeneration ||
			!lowerHex(c.PolicyFingerprint, 64) || !strings.HasPrefix(c.RunID, "session-") || !lowerHex(strings.TrimPrefix(c.RunID, "session-"), 24) {
			return errors.New("invalid network session child custody")
		}
	}
	if h.ACPChild != nil && h.ACPChild.ContainerID != nil {
		cid := h.ACPChild.ContainerID
		if !filepath.IsAbs(cid.Directory) || filepath.Clean(cid.Directory) != cid.Directory || len(cid.Directory) > 4096 || !utf8.ValidString(cid.Directory) ||
			strings.ContainsAny(cid.Directory, ":\x00\r\n") || !strings.HasPrefix(filepath.Base(cid.Directory), "coop-acp-cid-") || cid.Inode == 0 {
			return errors.New("invalid ACP container identity output")
		}
	}
	if h.Version != 2 || !lowerHex(h.Locator, 32) || !lowerHex(h.CatalogID, 64) ||
		len(h.Owner) == 0 || len(h.Owner) > 1<<20 || !utf8.Valid(h.Owner) || !json.Valid(h.Owner) ||
		len(h.Exposed) == 0 || len(h.Exposed) > 32768 {
		return errors.New("invalid network catalog handoff")
	}
	if f := h.WorkspaceFork; f != nil && (f.Name == "" || len(f.Name) > 256 || filepath.Base(f.Name) != f.Name ||
		f.Name == "." || f.Name == ".." || !utf8.ValidString(f.Name) || strings.ContainsAny(f.Name, "\x00\r\n") || !lowerHex(f.Generation, 32)) {
		return errors.New("invalid network catalog handoff fork")
	}
	switch h.Kind {
	case HandoffForkWorker:
		if h.WorkspaceFork == nil || h.ForkWorker == nil || h.ACPChild != nil || h.ACPReload != nil ||
			!lowerHex(h.ForkWorker.ReservationDigest, 64) || !validHandoffProcess(h.ForkWorker.Child) {
			return errors.New("invalid network fork worker handoff")
		}
	case HandoffACPChild, HandoffSessionACP:
		if h.ForkWorker != nil || h.ACPReload != nil || h.ACPChild == nil {
			return errors.New("invalid network ACP child handoff")
		}
		c := h.ACPChild
		if !lowerHex(c.SupervisorID, 16) || c.MemberKey == "" || len(c.MemberKey) > 256 || !utf8.ValidString(c.MemberKey) ||
			strings.ContainsAny(c.MemberKey, "\x00\r\n") || !validHandoffProcess(c.Supervisor) || !validHandoffProcess(c.Child) || !validHandoffProcess(c.Group) {
			return errors.New("invalid network ACP child identity")
		}
	case HandoffACPReload:
		if h.ForkWorker != nil || h.ACPChild != nil || h.ACPReload == nil || !lowerHex(h.ACPReload.SupervisorID, 16) ||
			!lowerHex(h.ACPReload.ResumeID, 64) || !validHandoffProcess(h.ACPReload.Supervisor) {
			return errors.New("invalid network ACP reload handoff")
		}
	default:
		return errors.New("invalid network catalog handoff kind")
	}
	for _, path := range append([]string{h.Project, h.Workspace, h.Runtime}, h.Exposed...) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 16384 || !utf8.ValidString(path) || strings.ContainsRune(path, '\x00') {
			return errors.New("invalid network catalog handoff path")
		}
	}
	return nil
}

func (s *Store) catalogHandoffID(h CatalogHandoff) string {
	h.ID = ""
	data, _ := json.Marshal(h)
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("network-catalog-handoff-v2\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) RecordCatalogHandoff(h CatalogHandoff) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	if h.ID != "" {
		return errors.New("new network handoff cannot supply its own identity")
	}
	if err := validateCatalogHandoff(h); err != nil {
		return err
	}
	envelope, err := s.CatalogEnvelope(h.CatalogID)
	if err != nil {
		return err
	}
	if (envelope.Session != nil) != (h.SessionChild != nil) || h.SessionChild != nil &&
		(*envelope.Session != h.SessionChild.Owner || envelope.Fingerprint != h.SessionChild.PolicyFingerprint) {
		return errors.New("network handoff differs from its session catalog owner")
	}
	if h.ACPReload != nil {
		if _, err := s.ACPResume(h.ACPReload.ResumeID); err != nil {
			return err
		}
	}
	if err := s.CheckExposure(h.Exposed); err != nil {
		return err
	}
	h.ID = s.catalogHandoffID(h)
	data, err := json.Marshal(h)
	if err != nil || len(data) > maxCatalogHandoffBytes {
		return errors.New("network catalog handoff exceeds its byte limit")
	}
	name := "handoff-" + h.Locator + ".json"
	if err := s.publishBounded(name, data, false, 0o600, maxCatalogHandoffBytes); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		previous, err := s.read(name, maxCatalogHandoffBytes)
		if err != nil || !bytes.Equal(previous, data) {
			return errors.New("network catalog handoff locator was already used")
		}
	}
	if err := s.confirmPublication(); err != nil {
		return err
	}
	return s.intactAuthority()
}

// CatalogHandoff authenticates metadata only. A caller must validate child
// custody and current exposure before loading the catalog's captured inputs.
func (s *Store) CatalogHandoff(locator string) (CatalogHandoff, error) {
	if err := s.intactAuthority(); err != nil {
		return CatalogHandoff{}, err
	}
	h, err := s.readCatalogHandoff(locator)
	if err != nil {
		return CatalogHandoff{}, err
	}
	if !hmac.Equal([]byte(h.ID), []byte(s.catalogHandoffID(h))) {
		return CatalogHandoff{}, errors.New("network catalog handoff failed owner-bound integrity validation")
	}
	if err := s.intactAuthority(); err != nil {
		return CatalogHandoff{}, err
	}
	return h, nil
}

func (s *Store) readCatalogHandoff(locator string) (CatalogHandoff, error) {
	if !lowerHex(locator, 32) {
		return CatalogHandoff{}, errors.New("invalid network catalog handoff locator")
	}
	data, err := s.read("handoff-"+locator+".json", maxCatalogHandoffBytes)
	if err != nil {
		return CatalogHandoff{}, err
	}
	var h CatalogHandoff
	if err := strictJSON(data, &h); err != nil {
		return CatalogHandoff{}, err
	}
	canonical, _ := json.Marshal(h)
	if h.Locator != locator || !lowerHex(h.ID, 64) || !bytes.Equal(data, canonical) {
		return CatalogHandoff{}, errors.New("network catalog handoff failed owner-bound integrity validation")
	}
	if err := validateCatalogHandoff(h); err != nil {
		return CatalogHandoff{}, err
	}
	return h, nil
}

// HandoffForCleanup reads only the exact locator retained by a lifecycle owner.
// Compare CatalogHandoffDigest against that owner's independent durable binding
// before this metadata authorizes any process signal or runtime mutation.
func (e *Evidence) HandoffForCleanup(locator string) (CatalogHandoff, error) {
	return e.files.readCatalogHandoff(locator)
}

func CatalogHandoffDigest(h CatalogHandoff) (string, error) {
	if err := validateCatalogHandoff(h); err != nil {
		return "", err
	}
	h.ID = "" // publication adds its HMAC; cleanup binds the complete body
	data, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("network-cleanup-handoff-v1\x00"), data...))
	return hex.EncodeToString(digest[:]), nil
}
