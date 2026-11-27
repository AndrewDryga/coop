package workerproto

import (
	"errors"
	"time"
)

var ErrModelGrantRefused = errors.New("controller refused model access")

// ModelGrantRequest identifies a host-side run. The controller resolves the
// account and allowed models from the immutable job, never from a prompt.
type ModelGrantRequest struct {
	JobDigest string `json:"job_digest"`
	RunID     string `json:"run_id"`
	Provider  string `json:"provider"`
	Account   string `json:"account"`
}

func (r ModelGrantRequest) Validate() error {
	if !digestPattern.MatchString(r.JobDigest) || reference(r.RunID, 128, "run") != nil ||
		reference(r.Provider, 32, "provider") != nil || reference(r.Account, 128, "account") != nil {
		return errors.New("invalid model grant binding")
	}
	return nil
}

// ModelGrant carries an expiring proxy capability. Selection is the adapter's
// public native authentication variant. A provider's own credentials stay at
// the controller, including provider-issued access tokens.
type ModelGrant struct {
	ModelGrantRequest
	Token     string    `json:"token"`
	Selection string    `json:"selection"`
	AccountID string    `json:"account_id,omitempty"`
	Epoch     uint64    `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (g ModelGrant) Validate(now time.Time) error {
	if g.ModelGrantRequest.Validate() != nil || len(g.Token) < 32 || reference(g.Token, 512, "model token") != nil ||
		reference(g.Selection, 256, "selection") != nil || g.Epoch == 0 ||
		!g.ExpiresAt.After(now) || g.ExpiresAt.After(now.Add(10*time.Minute)) {
		return errors.New("invalid temporary model grant")
	}
	return nil
}
