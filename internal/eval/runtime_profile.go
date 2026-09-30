package eval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ProfileProtocol is deliberately fixed: this is an adapted offline-build workload,
// not an importer for arbitrary benchmark runtime requirements.
const ProfileProtocol = "coop-adapted-offline-profile-v1"

// CaseRuntime names host-owned build authority, never a candidate input. Only these four
// fields are authored; staging and the CLI supply the measured identity separately.
type CaseRuntime struct {
	Profile         string   `yaml:"profile"`
	Workdir         string   `yaml:"workdir"`
	AgentTimeout    Duration `yaml:"agent_timeout"`
	VerifierTimeout Duration `yaml:"verifier_timeout"`

	RetainedProfile string      `yaml:"-"`
	ProfileDigest   Fingerprint `yaml:"-"`
	ImageID         string      `yaml:"-"`
	Platform        string      `yaml:"-"`
	sourceIdentity  os.FileInfo
}

func (r *CaseRuntime) validate(s *Suite, c *Case) error {
	if s.IsLoop() {
		return errors.New("profiles belong to agent cases, not loop cases")
	}
	if !filepath.IsAbs(r.Profile) || filepath.Clean(r.Profile) == string(filepath.Separator) {
		return errors.New("profile must name an absolute external directory")
	}
	if r.Workdir != "/app" {
		return errors.New("workdir must be /app for this runtime protocol")
	}
	a, v, outer := time.Duration(r.AgentTimeout), time.Duration(r.VerifierTimeout), time.Duration(c.Timeout)
	if a <= 0 || v <= 0 || a > outer || v > outer-a {
		return errors.New("positive agent_timeout and verifier_timeout must fit within the case timeout")
	}
	profile, err := filepath.EvalSymlinks(r.Profile)
	if err != nil {
		return fmt.Errorf("resolve profile: %w", err)
	}
	suiteDir, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return err
	}
	if overlaps(profile, suiteDir) {
		return errors.New("profile overlaps the suite; use a dedicated external profile export")
	}
	return nil
}
