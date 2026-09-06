package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ServiceApprovalRootEnv overrides where approvals are stored — honored only by test binaries, so a
// test never reads or writes the developer's real approvals.
const ServiceApprovalRootEnv = "COOP_SERVICE_APPROVAL_ROOT"

// ServiceApproval records a human's decision that the sibling services defined by one exact
// compose file may read the secret-looking files it binds — a generated dev TLS key for Keycloak,
// say — instead of the empty decoys serviceShadowOverride would hand them.
//
// The record is keyed by the compose file's CONTENT digest, not its path or workspace: the human
// vouched for that text, and an agent editing the file (the one way a box can reach these binds)
// voids the approval by construction. It lives on the host under ~/.local/state, where no box can
// write, which is what makes it an approval rather than a hint.
type ServiceApproval struct {
	File       string    `json:"file"`      // repo-relative compose path at approval time
	Workspace  string    `json:"workspace"` // where it was approved (display only)
	Paths      []string  `json:"paths"`     // the secret-looking bind sources the approver saw
	ApprovedAt time.Time `json:"approved_at"`
	ApprovedBy string    `json:"approved_by"`
}

func serviceApprovalRoot() (string, error) {
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		if root := os.Getenv(ServiceApprovalRootEnv); root != "" {
			return root, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "coop", "service-approvals"), nil
}

func composeDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ApprovedServiceSecrets reports whether this exact compose content was approved on this machine.
// Any read failure reads as "not approved": the decoys stay, which is the safe direction.
func ApprovedServiceSecrets(data []byte) (ServiceApproval, bool) {
	root, err := serviceApprovalRoot()
	if err != nil {
		return ServiceApproval{}, false
	}
	raw, err := os.ReadFile(filepath.Join(root, composeDigest(data)+".json"))
	if err != nil {
		return ServiceApproval{}, false
	}
	var approval ServiceApproval
	if err := json.Unmarshal(raw, &approval); err != nil {
		return ServiceApproval{}, false
	}
	return approval, true
}

// ServiceSecretReview is what `coop up` puts in front of a human: the secret-looking files one
// compose file binds into its services, which stay decoys until Approve is called.
type ServiceSecretReview struct {
	File   string   // repo-relative compose path
	Hidden []string // repo-relative bind sources that look like secrets, sorted

	workspace string
	data      []byte
}

// ReviewServiceSecrets returns the review a human must see before workspace's compose file (an
// absolute path inside it) hands services any secret-looking file, or nil when nothing is hidden
// or every hidden file is already approved for this exact content. A compose file the host would
// refuse to run is an error here too, so `coop up` reports the violation before asking anything.
func ReviewServiceSecrets(workspace, file string) (*ServiceSecretReview, error) {
	data, err := readValidatedCompose(file, workspace, false)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	_, hidden, err := serviceShadowPlan(workspace, abs, data)
	if err != nil {
		return nil, err
	}
	if len(hidden) == 0 {
		return nil, nil
	}
	if approval, ok := ApprovedServiceSecrets(data); ok {
		remaining := make([]string, 0, len(hidden))
		approved := make(map[string]bool, len(approval.Paths))
		for _, p := range approval.Paths {
			approved[p] = true
		}
		for _, p := range hidden {
			if !approved[p] {
				remaining = append(remaining, p)
			}
		}
		hidden = remaining
		if len(hidden) == 0 {
			return nil, nil
		}
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil {
		rel = abs
	}
	return &ServiceSecretReview{File: filepath.ToSlash(rel), Hidden: hidden, workspace: workspace, data: data}, nil
}

// Approve records the human's decision for the reviewed content. It is written atomically, so a
// reader never sees a half-written approval.
func (r *ServiceSecretReview) Approve() error {
	if r == nil {
		return errors.New("nothing to approve")
	}
	root, err := serviceApprovalRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	// Everything hidden for this content is approved together: Hidden already excludes anything an
	// earlier approval of the same content covered, so re-approving adds the new files to it.
	paths := r.Hidden
	if prior, ok := ApprovedServiceSecrets(r.data); ok {
		paths = append(append([]string(nil), prior.Paths...), paths...)
		sort.Strings(paths)
	}
	approval := ServiceApproval{File: r.File, Workspace: r.workspace, Paths: paths, ApprovedAt: time.Now().UTC(), ApprovedBy: approverName()}
	raw, err := json.MarshalIndent(approval, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(root, composeDigest(r.data)+".json")
	tmp, err := os.CreateTemp(root, "approval-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), final)
}

func approverName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}
